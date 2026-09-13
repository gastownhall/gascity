import type { SourceStatus } from 'gas-city-dashboard-shared';

export const ATTENTION_DOMAINS = [
  'agents',
  'beads',
  'runs',
  'mail',
  'activity',
  'health',
] as const;

/**
 * Severity tiers. `attention` and `watch` are operator-actionable signals that
 * count toward a domain's nav badge. `unavailable` is the data-degradation tier
 * (gascity-dashboard issue-88 follow-up): a slice of a source could not be read,
 * so the item must surface the degradation WITHOUT inflating the badge count or
 * recoloring it. Only attention/watch drive `AttentionDomainSummary.severity`.
 */
export type AttentionSeverity = 'attention' | 'watch' | 'unavailable';

/** The badge-driving tiers — the subset of severities that color/count a badge. */
export type BadgeSeverity = Exclude<AttentionSeverity, 'unavailable'>;
export type AttentionDomain = (typeof ATTENTION_DOMAINS)[number];

export interface AttentionItem {
  id: string;
  domain: AttentionDomain;
  severity: AttentionSeverity;
  title: string;
  href?: string;
  summary?: string;
  current?: boolean;
  actionable?: boolean;
  updatedAt?: string;
  /**
   * Freshness of the source this item was derived from, mirroring
   * AlertItem.provenance. Carried on `unavailable` items so a badge fed from a
   * stale cache read can be marked stale rather than rendered as live truth.
   */
  provenance?: SourceStatus;
  /**
   * ISO timestamp of the cache read this item was derived from (the
   * useCachedData/cache fetch primitive). Pairs with `provenance` so a consumer
   * can age a degradation signal — "as of <fetchedAt>, this slice was
   * unavailable" — instead of treating a long-stale read as current.
   */
  fetchedAt?: string;
}

export interface AttentionContributor {
  id: string;
  domain: AttentionDomain;
  getItems(): readonly AttentionItem[];
}

export interface AttentionDomainSummary {
  domain: AttentionDomain;
  attention: number;
  watch: number;
  /** Count of `unavailable`-tier items; never folded into the badge count. */
  unavailable: number;
  /** Badge tier — only ever attention/watch/null; `unavailable` never sets it. */
  severity: BadgeSeverity | null;
  items: readonly AttentionItem[];
}

export interface AttentionOverflowGroup {
  domain: AttentionDomain;
  attention: number;
  watch: number;
  unavailable: number;
  total: number;
}

export interface AttentionModel {
  items: readonly AttentionItem[];
  topItems: readonly AttentionItem[];
  overflowByDomain: readonly AttentionOverflowGroup[];
  byDomain: Record<AttentionDomain, AttentionDomainSummary>;
}

export interface ComposeAttentionOptions {
  topLimit?: number;
}

const DEFAULT_TOP_LIMIT = 5;

const DOMAIN_ORDER = new Map<AttentionDomain, number>(
  ATTENTION_DOMAINS.map((domain, index) => [domain, index]),
);

export function composeAttention(
  contributors: readonly AttentionContributor[],
  options: ComposeAttentionOptions = {},
): AttentionModel {
  const byDomain = emptyDomainSummaries();
  const indexedItems: Array<{ item: AttentionItem; index: number }> = [];
  let index = 0;

  for (const contributor of contributors) {
    for (const item of contributor.getItems()) {
      indexedItems.push({ item, index });
      const summary = byDomain[item.domain];
      const nextItems = [...summary.items, item];
      byDomain[item.domain] = {
        domain: item.domain,
        attention: summary.attention + (item.severity === 'attention' ? 1 : 0),
        watch: summary.watch + (item.severity === 'watch' ? 1 : 0),
        unavailable: summary.unavailable + (item.severity === 'unavailable' ? 1 : 0),
        // Unavailable items report degradation; they must never color the badge.
        severity:
          item.severity === 'unavailable'
            ? summary.severity
            : highestSeverity(summary.severity, item.severity),
        items: nextItems,
      };
      index += 1;
    }
  }

  const items = indexedItems
    .sort((a, b) => compareAttentionItems(a.item, b.item) || a.index - b.index)
    .map(({ item }) => item);
  const topLimit = options.topLimit ?? DEFAULT_TOP_LIMIT;
  const topItems = items.slice(0, topLimit);
  const overflowByDomain = groupOverflow(items.slice(topLimit));

  return { items, topItems, overflowByDomain, byDomain };
}

function emptyDomainSummaries(): Record<AttentionDomain, AttentionDomainSummary> {
  const summaries = {} as Record<AttentionDomain, AttentionDomainSummary>;
  for (const domain of ATTENTION_DOMAINS) {
    summaries[domain] = {
      domain,
      attention: 0,
      watch: 0,
      unavailable: 0,
      severity: null,
      items: [],
    };
  }
  return summaries;
}

function highestSeverity(current: BadgeSeverity | null, next: BadgeSeverity): BadgeSeverity {
  if (current === 'attention' || next === 'attention') return 'attention';
  return 'watch';
}

function compareAttentionItems(a: AttentionItem, b: AttentionItem): number {
  return (
    severityRank(a.severity) - severityRank(b.severity) ||
    booleanRank(b.current ?? true) - booleanRank(a.current ?? true) ||
    booleanRank(b.actionable ?? false) - booleanRank(a.actionable ?? false) ||
    recencyRank(b.updatedAt) - recencyRank(a.updatedAt) ||
    domainRank(a.domain) - domainRank(b.domain)
  );
}

function severityRank(severity: AttentionSeverity): number {
  switch (severity) {
    case 'attention':
      return 0;
    case 'watch':
      return 1;
    case 'unavailable':
      return 2;
  }
}

function booleanRank(value: boolean): number {
  return value ? 1 : 0;
}

function recencyRank(value: string | undefined): number {
  if (value === undefined) return 0;
  const parsed = Date.parse(value);
  return Number.isFinite(parsed) ? parsed : 0;
}

function domainRank(domain: AttentionDomain): number {
  return DOMAIN_ORDER.get(domain) ?? ATTENTION_DOMAINS.length;
}

function groupOverflow(items: readonly AttentionItem[]): AttentionOverflowGroup[] {
  const groups: AttentionOverflowGroup[] = [];
  for (const domain of ATTENTION_DOMAINS) {
    let attention = 0;
    let watch = 0;
    let unavailable = 0;
    for (const item of items) {
      if (item.domain !== domain) continue;
      if (item.severity === 'attention') attention += 1;
      else if (item.severity === 'watch') watch += 1;
      else unavailable += 1;
    }
    const total = attention + watch + unavailable;
    if (total > 0) groups.push({ domain, attention, watch, unavailable, total });
  }
  return groups;
}
