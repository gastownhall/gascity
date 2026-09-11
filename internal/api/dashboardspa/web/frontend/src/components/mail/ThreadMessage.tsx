import { StatusBadge } from '../StatusBadge';
import { PROMPT_INJECTION_NOTICE } from '../../lib/constants';
import { formatMailSender } from '../../lib/mailSender';
import type { AttentionSeverity } from '../../attention/compose';
import type { SupervisorMailItem } from '../../supervisor/mailReads';

export function ThreadMessage({
  message,
  attentionSeverity = null,
}: {
  message: SupervisorMailItem;
  attentionSeverity?: AttentionSeverity | null;
}) {
  return (
    <article
      {...attentionAttrs(attentionSeverity)}
      className="space-y-3 pb-4 border-b border-rule last:border-0"
    >
      <header className="flex items-baseline justify-between gap-3">
        <div className="text-label uppercase tracking-wider text-fg-muted truncate">
          <span className="text-fg font-medium">{formatMailSender(message.from)}</span>
          <span className="mx-1.5 text-fg-faint">→</span>
          <span>{formatMailSender(message.to)}</span>
        </div>
        <span className="text-label uppercase tracking-wider text-fg-faint tnum">
          {formatAbsolute(message.created_at)}
        </span>
      </header>
      <p className="text-title font-semibold text-fg">{message.subject}</p>
      <StatusBadge tone="warn" label={PROMPT_INJECTION_NOTICE} />
      <pre className="text-body whitespace-pre-wrap leading-relaxed text-fg overflow-x-auto">
        {message.body}
      </pre>
    </article>
  );
}

function attentionAttrs(severity: AttentionSeverity | null): {
  'data-attention-severity'?: AttentionSeverity;
} {
  return severity === null ? {} : { 'data-attention-severity': severity };
}

function formatAbsolute(iso: string): string {
  const ms = Date.parse(iso);
  if (!Number.isFinite(ms)) return '·';
  return new Date(ms).toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
  });
}
