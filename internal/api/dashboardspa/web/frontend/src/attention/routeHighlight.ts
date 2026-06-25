import type { HTMLAttributes } from 'react';
import type { AttentionDomain, AttentionModel, AttentionSeverity, BadgeSeverity } from './compose';

type AttentionAttributes<T extends HTMLElement> = HTMLAttributes<T> & {
  'data-attention-severity'?: AttentionSeverity;
};

export function resourceAttentionSeverity(
  model: AttentionModel,
  domain: AttentionDomain,
  resourceId: string,
): BadgeSeverity | null {
  const prefix = `${domain}:${resourceId}:`;
  const exact = `${domain}:${resourceId}`;
  let severity: BadgeSeverity | null = null;
  for (const item of model.byDomain[domain].items) {
    if (item.id !== exact && !item.id.startsWith(prefix)) continue;
    if (item.severity === 'attention') return 'attention';
    // `unavailable` reports a degraded read, not a watch-worthy state — it must
    // not tint a row as if the resource itself needs the operator.
    if (item.severity === 'watch') severity = 'watch';
  }
  return severity;
}

export function prefixedAttentionSeverity(
  model: AttentionModel,
  domain: AttentionDomain,
  itemIdPrefixes: readonly string[],
): BadgeSeverity | null {
  let severity: BadgeSeverity | null = null;
  for (const item of model.byDomain[domain].items) {
    if (!itemIdPrefixes.some((prefix) => item.id.startsWith(prefix))) continue;
    if (item.severity === 'attention') return 'attention';
    // `unavailable` reports a degraded read, not a watch-worthy state.
    if (item.severity === 'watch') severity = 'watch';
  }
  return severity;
}

export function attentionRowProps(
  severity: BadgeSeverity | null,
): AttentionAttributes<HTMLTableRowElement> {
  if (severity === null) return {};
  return {
    'data-attention-severity': severity,
    className: attentionHighlightClass(severity),
  };
}

// Row props that expose the attention severity for tooling (home-alerts
// panel, keyboard nav) WITHOUT painting a background tint. Used where a
// domain is not an alert by default — e.g. mail (gascity-dashboard-s464):
// unread mail is normal, so its rows render in the neutral foreground.
export function attentionDataProps(
  severity: AttentionSeverity | null,
): AttentionAttributes<HTMLTableRowElement> {
  if (severity === null) return {};
  return { 'data-attention-severity': severity };
}

export function attentionListItemProps(
  severity: BadgeSeverity | null,
): AttentionAttributes<HTMLLIElement> {
  if (severity === null) return {};
  return {
    'data-attention-severity': severity,
    className: attentionHighlightClass(severity),
  };
}

export function attentionBlockProps(
  severity: BadgeSeverity | null,
): AttentionAttributes<HTMLDivElement> {
  if (severity === null) return {};
  return {
    'data-attention-severity': severity,
    className: attentionHighlightClass(severity),
  };
}

export function attentionSectionProps(
  severity: BadgeSeverity | null,
): AttentionAttributes<HTMLElement> {
  if (severity === null) return {};
  const toneClass =
    severity === 'attention' ? 'border-accent bg-accent/5' : 'border-warn bg-warn/5';
  return {
    'data-attention-severity': severity,
    className: `border-l-2 pl-4 -ml-4 py-1 ${toneClass}`,
  };
}

function attentionHighlightClass(severity: BadgeSeverity): string {
  return severity === 'attention'
    ? 'bg-accent/10 hover:bg-accent/15'
    : 'bg-warn/10 hover:bg-warn/15';
}
