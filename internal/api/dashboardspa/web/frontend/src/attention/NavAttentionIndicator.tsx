import type { AttentionDomainSummary, BadgeSeverity } from './compose';

export function NavAttentionIndicator({
  label,
  summary,
}: {
  label: string;
  summary: AttentionDomainSummary;
}) {
  const total = summary.attention + summary.watch;
  if (total === 0 || summary.severity === null) return null;
  const itemWord = total === 1 ? 'item' : 'items';

  return (
    <span
      aria-label={`${label}: ${total} ${summary.severity} ${itemWord}`}
      className={`ml-1 align-super text-[0.65rem] leading-none tnum ${severityClass(summary.severity)}`}
    >
      {total}
    </span>
  );
}

function severityClass(severity: BadgeSeverity): string {
  return severity === 'attention' ? 'text-accent' : 'text-warn';
}
