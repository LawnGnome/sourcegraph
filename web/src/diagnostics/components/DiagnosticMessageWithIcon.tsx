import { Diagnostic } from '@sourcegraph/extension-api-types'
import React from 'react'
import * as sourcegraph from 'sourcegraph'
import { Markdown } from '../../../../shared/src/components/Markdown'
import { renderMarkdown } from '../../../../shared/src/util/markdown'
import { DiagnosticSeverityIcon } from './DiagnosticSeverityIcon'

interface Props {
    diagnostic: Diagnostic | sourcegraph.Diagnostic

    className?: string
}

export const DiagnosticMessageWithIcon: React.FunctionComponent<Props> = ({ diagnostic, className = '' }) => (
    <div className={`diagnostic-message-with-icon d-flex align-items-start ${className}`}>
        <DiagnosticSeverityIcon severity={diagnostic.severity} className="icon-inline mr-2" />
        <div>
            <Markdown dangerousInnerHTML={renderMarkdown(diagnostic.message)} />
            {diagnostic.detail && (
                <Markdown
                    dangerousInnerHTML={renderMarkdown(diagnostic.detail)}
                    className="text-muted small mt-1 mb-2"
                />
            )}
        </div>
    </div>
)