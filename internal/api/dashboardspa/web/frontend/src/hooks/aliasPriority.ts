// Identity-switcher prioritization rules + display-label mapping
// (gascity-dashboard-e85).
//
// The Mail page's "Reading as" dropdown groups aliases into four tiers so
// the operator can find the inboxes they care about without scanning a
// long alphabetical list:
//
//   you      — the operator alias, always top, always single
//   mayor    — the 'mayor' identity, when present
//   active   — aliases with mail activity (appear as from OR to)
//   other    — every other known alias, alphabetical
//
// The functions here are pure so the ViewingAs provider can compute the
// list once on data refresh and hand it down via context, and so the unit
// tests can pin the ordering rules without React.

export type AliasTier = 'you' | 'mayor' | 'active' | 'other';

export interface AliasBucket {
  tier: AliasTier;
  aliases: string[];
}

export interface PrioritizeAliasesInput {
  operator: string;
  /** Aliases observed in supervisor sessions (alphabet/casing as supervisor emits). */
  sessionAliases: ReadonlyArray<string>;
  /** Aliases observed as `from` OR `to` in the recent mail corpus. */
  mailFromOrTo: ReadonlyArray<string>;
}

const MAYOR_ALIAS = 'mayor';

/**
 * Bucket known aliases into the four UX tiers. Returns only the tiers
 * that have members so the consumer doesn't render empty optgroups.
 *
 * Casing rule: aliases are case-insensitively unique. The first form
 * seen for a given lowercase key wins as the display form. Sessions
 * win over mail because supervisor session metadata is the canonical
 * shape.
 */
export function prioritizeAliases(input: PrioritizeAliasesInput): AliasBucket[] {
  const { operator, sessionAliases, mailFromOrTo } = input;

  // Canonicalize: collect the display form of each unique alias.
  // Sessions seed first so their casing wins when mail differs.
  const canonical = new Map<string, string>(); // lowercase → display form
  for (const a of sessionAliases) {
    const key = a.toLowerCase();
    if (!canonical.has(key)) canonical.set(key, a);
  }
  for (const a of mailFromOrTo) {
    const key = a.toLowerCase();
    if (!canonical.has(key)) canonical.set(key, a);
  }

  const operatorKey = operator.toLowerCase();
  const mailSet = new Set(mailFromOrTo.map((a) => a.toLowerCase()));

  const youAliases: string[] = [operator];
  const mayorAliases: string[] = [];
  const active: string[] = [];
  const other: string[] = [];

  for (const [key, display] of canonical) {
    if (key === operatorKey) continue; // already in 'you'
    if (key === MAYOR_ALIAS) {
      mayorAliases.push(display);
      continue;
    }
    if (mailSet.has(key)) {
      active.push(display);
    } else {
      other.push(display);
    }
  }

  const sortAlpha = (a: string, b: string): number =>
    a.toLowerCase().localeCompare(b.toLowerCase());
  active.sort(sortAlpha);
  other.sort(sortAlpha);

  const buckets: AliasBucket[] = [{ tier: 'you', aliases: youAliases }];
  if (mayorAliases.length > 0) {
    buckets.push({ tier: 'mayor', aliases: mayorAliases });
  }
  if (active.length > 0) {
    buckets.push({ tier: 'active', aliases: active });
  }
  if (other.length > 0) {
    buckets.push({ tier: 'other', aliases: other });
  }
  return buckets;
}

/**
 * Map an alias to its UI label. The operator is rendered as 'user'
 * (display-only — the underlying alias stays load-bearing for the audit
 * log, gc supervisor identity routing, and the bd assignee). Every other
 * alias renders verbatim.
 *
 * Threading the operator alias through as a parameter keeps this pure
 * and resilient to a future operator rename.
 */
export function displayLabel(alias: string, operator: string): string {
  return alias === operator ? 'user' : alias;
}

/**
 * Render a human-readable label for each tier (used as `<optgroup>` label).
 */
export function tierLabel(tier: AliasTier): string {
  switch (tier) {
    case 'you':
      return 'You';
    case 'mayor':
      return 'Mayor';
    case 'active':
      return 'Active';
    case 'other':
      return 'Other';
  }
}
