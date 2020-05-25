package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v31/github"
	"github.com/inconshreveable/log15"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/schollz/progressbar/v3"
	"golang.org/x/oauth2"
	"golang.org/x/time/rate"
)

func newGHEClient(ctx context.Context, baseURL, uploadURL, token string) (*github.Client, error) {
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(ctx, ts)

	return github.NewEnterpriseClient(baseURL, uploadURL, tc)
}

func init() {
	rand.Seed(time.Now().UnixNano())
}

func randomOrgNameAndSize() (string, int) {
	size := rand.Intn(500)
	if size < 5 {
		size = 5
	}
	name := fmt.Sprintf("%s-%d", getRandomName(0), size)
	return name, size
}

type feederError struct {
	errType string
	err     error
}

func (e *feederError) Error() string {
	return fmt.Sprintf("%v: %v", e.errType, e.err)
}

func (e *feederError) Unwrap() error {
	return e.err
}

type worker struct {
	name               string
	client             *github.Client
	index              int
	scratchDir         string
	work               <-chan string
	wg                 *sync.WaitGroup
	bar                *progressbar.ProgressBar
	numFailed          int64
	numSucceeded       int64
	fdr                *feederDB
	currentOrg         string
	currentNumRepos    int
	currentMaxRepos    int
	logger             log15.Logger
	rateLimiter        *rate.Limiter
	admin              string
	token              string
	host               string
	pushSem            chan struct{}
	cloneSem           chan struct{}
	cloneRepoTimeout   time.Duration
	numCloningAttempts int
}

func (wkr *worker) run(ctx context.Context) {
	defer wkr.wg.Done()

	wkr.currentOrg, wkr.currentMaxRepos = randomOrgNameAndSize()

	wkr.logger.Debug("switching to org", "org", wkr.currentOrg)

	err := wkr.addGHEOrg(ctx)
	if err != nil {
		wkr.logger.Error("failed to create org", "org", wkr.currentOrg, "error", err)
		// add it to default org then
		wkr.currentOrg = ""
	} else {
		err = wkr.fdr.declareOrg(wkr.currentOrg)
		if err != nil {
			wkr.logger.Error("failed to declare org", "org", wkr.currentOrg, "error", err)
		}
	}

	for line := range wkr.work {
		_ = wkr.bar.Add(1)

		if ctx.Err() != nil {
			return
		}

		xs := strings.Split(line, "/")
		if len(xs) != 2 {
			wkr.logger.Error("failed tos split line", "line", line)
			continue
		}
		owner, repo := xs[0], xs[1]

		err := wkr.process(ctx, owner, repo)
		reposProcessedCounter.With(prometheus.Labels{"worker": wkr.name}).Inc()
		remainingWorkGauge.Add(-1.0)
		if err != nil {
			wkr.numFailed++
			errType := "unknown"
			var ferr *feederError
			if errors.As(err, &ferr) {
				errType = ferr.errType
			}
			reposFailedCounter.With(prometheus.Labels{"worker": wkr.name, "err_type": errType}).Inc()
			_ = wkr.fdr.failed(line, errType)
		} else {
			reposSucceededCounter.Inc()
			wkr.numSucceeded++
			wkr.currentNumRepos++

			err = wkr.fdr.succeeded(line, wkr.currentOrg)
			if err != nil {
				wkr.logger.Error("failed to mark succeeded repo", "ownerRepo", line, "error", err)
			}

			if wkr.currentNumRepos >= wkr.currentMaxRepos {
				wkr.currentOrg, wkr.currentMaxRepos = randomOrgNameAndSize()
				wkr.currentNumRepos = 0
				wkr.logger.Debug("switching to org", "org", wkr.currentOrg)
				err := wkr.addGHEOrg(ctx)
				if err != nil {
					wkr.logger.Error("failed to create org", "org", wkr.currentOrg, "error", err)
					// add it to default org then
					wkr.currentOrg = ""
				} else {
					err = wkr.fdr.declareOrg(wkr.currentOrg)
					if err != nil {
						wkr.logger.Error("failed to declare org", "org", wkr.currentOrg, "error", err)
					}
				}
			}
		}
		ownerDir := filepath.Join(wkr.scratchDir, owner)

		err = os.RemoveAll(ownerDir)
		if err != nil {
			wkr.logger.Error("failed to clean up cloned repo", "ownerRepo", line, "error", err, "ownerDir", ownerDir)
		}
	}
}

func (wkr *worker) process(ctx context.Context, owner, repo string) error {
	err := wkr.cloneRepo(ctx, owner, repo)
	if err != nil {
		wkr.logger.Error("failed to clone repo", "owner", owner, "repo", repo, "error", err)
		return &feederError{"clone", err}
	}

	gheRepo, err := wkr.addGHERepo(ctx, owner, repo)
	if err != nil {
		wkr.logger.Error("failed to create GHE repo", "owner", owner, "repo", repo, "error", err)
		return &feederError{"api", err}
	}

	err = wkr.addRemote(ctx, gheRepo, owner, repo)
	if err != nil {
		wkr.logger.Error("failed to add GHE as a remote in cloned repo", "owner", owner, "repo", repo, "error", err)
		return &feederError{"api", err}
	}

	for attempt := 0; attempt < wkr.numCloningAttempts && ctx.Err() == nil; attempt++ {
		err = wkr.pushToGHE(ctx, owner, repo)
		if err == nil {
			return nil
		}
		wkr.logger.Error("failed to push cloned repo to GHE", "attempt", attempt+1, "owner", owner, "repo", repo, "error", err)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}
	return &feederError{"push", err}
}

func (wkr *worker) cloneRepo(ctx context.Context, owner, repo string) error {
	select {
	case wkr.cloneSem <- struct{}{}:
		defer func() {
			<-wkr.cloneSem
		}()

		ownerDir := filepath.Join(wkr.scratchDir, owner)
		err := os.MkdirAll(ownerDir, 0777)
		if err != nil {
			wkr.logger.Error("failed to create owner dir", "ownerDir", ownerDir, "error", err)
			return err
		}

		ctx, cancel := context.WithTimeout(ctx, wkr.cloneRepoTimeout)
		defer cancel()

		cmd := exec.CommandContext(ctx, "git", "clone",
			fmt.Sprintf("https://github.com/%s/%s", owner, repo))
		cmd.Dir = ownerDir
		cmd.Env = append(cmd.Env, "GIT_ASKPASS=/bin/echo")

		return cmd.Run()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (wkr *worker) addRemote(ctx context.Context, gheRepo *github.Repository, owner, repo string) error {
	repoDir := filepath.Join(wkr.scratchDir, owner, repo)

	remoteURL := fmt.Sprintf("https://%s@%s/%s.git", wkr.token, wkr.host, *gheRepo.FullName)
	cmd := exec.CommandContext(ctx, "git", "remote", "add", "ghe", remoteURL)
	cmd.Dir = repoDir

	return cmd.Run()
}

func (wkr *worker) pushToGHE(ctx context.Context, owner, repo string) error {
	select {
	case wkr.pushSem <- struct{}{}:
		defer func() {
			<-wkr.pushSem
		}()
		repoDir := filepath.Join(wkr.scratchDir, owner, repo)

		ctx, cancel := context.WithTimeout(ctx, wkr.cloneRepoTimeout)
		defer cancel()

		cmd := exec.CommandContext(ctx, "git", "push", "ghe", "master")
		cmd.Dir = repoDir

		return cmd.Run()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (wkr *worker) addGHEOrg(ctx context.Context) error {
	err := wkr.rateLimiter.Wait(ctx)
	if err != nil {
		wkr.logger.Error("failed to get a request spot from rate limiter", "error", err)
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, time.Second*30)
	defer cancel()

	gheOrg := &github.Organization{
		Login: github.String(wkr.currentOrg),
	}

	_, _, err = wkr.client.Admin.CreateOrg(ctx, gheOrg, wkr.admin)
	return err
}

func (wkr *worker) addGHERepo(ctx context.Context, owner, repo string) (*github.Repository, error) {
	err := wkr.rateLimiter.Wait(ctx)
	if err != nil {
		wkr.logger.Error("failed to get a request spot from rate limiter", "error", err)
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, time.Second*30)
	defer cancel()

	gheRepo := &github.Repository{
		Name: github.String(fmt.Sprintf("%s-%s", owner, repo)),
	}

	gheReturnedRepo, _, err := wkr.client.Repositories.Create(ctx, wkr.currentOrg, gheRepo)
	return gheReturnedRepo, err
}
