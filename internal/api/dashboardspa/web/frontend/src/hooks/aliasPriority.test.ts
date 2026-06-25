import { describe, it, expect } from 'vitest';
import { displayLabel, prioritizeAliases, type AliasTier } from './aliasPriority';

// gascity-dashboard-e85 — Mail identity-switcher prioritization rules.
//
// Tiers (top → bottom):
//   1. you      — the operator alias (always present, always top, always single)
//   2. mayor    — the literal alias 'mayor', if seen in sessions or mail
//   3. active   — aliases that appear as from OR to in the mail corpus
//                 (excluding the operator and mayor, which are handled above)
//   4. other    — every other known alias, sorted alphabetically
//
// The operator alias appears exactly once, in tier 1. Mayor cannot also
// appear in 'active'. The unique-aliases invariant lets the UI assume a
// flat dropdown.

const OPERATOR = 'stephanie';

function tierOf(
  buckets: ReadonlyArray<{ tier: AliasTier; aliases: ReadonlyArray<string> }>,
  alias: string,
): AliasTier | null {
  for (const b of buckets) if (b.aliases.includes(alias)) return b.tier;
  return null;
}

describe('prioritizeAliases', () => {
  it('always places the operator first as the sole "you" tier member', () => {
    const result = prioritizeAliases({
      operator: OPERATOR,
      sessionAliases: ['mayor', OPERATOR, 'mechanic'],
      mailFromOrTo: [],
    });
    expect(result[0]?.tier).toBe('you');
    expect(result[0]?.aliases).toEqual([OPERATOR]);
  });

  it('excludes the operator from active even when they appear in mail corpus', () => {
    // Structural guard: the operator should always be in 'you' and never
    // surface in any other tier. Pinned because removing the operator
    // 'continue' in prioritizeAliases.ts is a silent regression.
    const result = prioritizeAliases({
      operator: OPERATOR,
      sessionAliases: [],
      mailFromOrTo: [OPERATOR, 'mechanic'],
    });
    expect(tierOf(result, OPERATOR)).toBe('you');
    expect(tierOf(result, 'mechanic')).toBe('active');
    const flat = result.flatMap((b) => b.aliases);
    expect(flat.filter((a) => a === OPERATOR).length).toBe(1);
  });

  it('places mayor in its own tier when present', () => {
    const result = prioritizeAliases({
      operator: OPERATOR,
      sessionAliases: ['mayor', 'mechanic'],
      mailFromOrTo: [],
    });
    expect(tierOf(result, 'mayor')).toBe('mayor');
    expect(tierOf(result, 'mechanic')).toBe('other');
  });

  it('omits the mayor tier entirely when mayor is unknown', () => {
    const result = prioritizeAliases({
      operator: OPERATOR,
      sessionAliases: ['mechanic'],
      mailFromOrTo: [],
    });
    expect(result.some((b) => b.tier === 'mayor')).toBe(false);
  });

  it('places aliases with mail activity in the active tier', () => {
    const result = prioritizeAliases({
      operator: OPERATOR,
      sessionAliases: ['mechanic', 'scix-worker', 'historian'],
      mailFromOrTo: ['mechanic', 'scix-worker'],
    });
    expect(tierOf(result, 'mechanic')).toBe('active');
    expect(tierOf(result, 'scix-worker')).toBe('active');
    expect(tierOf(result, 'historian')).toBe('other');
  });

  it('admits aliases that have mail activity but no session', () => {
    // An alias might be retired (no live session) but still appear in
    // historical mail. We want to be able to read its inbox.
    const result = prioritizeAliases({
      operator: OPERATOR,
      sessionAliases: [],
      mailFromOrTo: ['retired-agent'],
    });
    expect(tierOf(result, 'retired-agent')).toBe('active');
  });

  it('never double-lists an alias: operator/mayor are excluded from active and other', () => {
    const result = prioritizeAliases({
      operator: OPERATOR,
      sessionAliases: ['mayor', OPERATOR],
      mailFromOrTo: ['mayor', OPERATOR, 'mechanic'],
    });
    const flat = result.flatMap((b) => b.aliases);
    const counts = new Map<string, number>();
    for (const a of flat) counts.set(a, (counts.get(a) ?? 0) + 1);
    for (const [, count] of counts) expect(count).toBe(1);
    expect(tierOf(result, OPERATOR)).toBe('you');
    expect(tierOf(result, 'mayor')).toBe('mayor');
    expect(tierOf(result, 'mechanic')).toBe('active');
  });

  it('sorts active and other tiers alphabetically (case-insensitive)', () => {
    const result = prioritizeAliases({
      operator: OPERATOR,
      sessionAliases: ['zeta', 'alpha', 'Mu'],
      mailFromOrTo: ['gamma', 'beta'],
    });
    const active = result.find((b) => b.tier === 'active');
    const other = result.find((b) => b.tier === 'other');
    expect(active?.aliases).toEqual(['beta', 'gamma']);
    expect(other?.aliases).toEqual(['alpha', 'Mu', 'zeta']);
  });

  it('treats alias casing in mail vs sessions as the same identity', () => {
    // Mail corpus emits 'Mayor' sometimes, while supervisor mail filtering is
    // case-insensitive on the wire. The session alias is 'mayor'. They
    // are the same identity; do not produce two rows.
    const result = prioritizeAliases({
      operator: OPERATOR,
      sessionAliases: ['mayor'],
      mailFromOrTo: ['Mayor'],
    });
    expect(tierOf(result, 'mayor')).toBe('mayor');
    const flat = result.flatMap((b) => b.aliases);
    expect(flat.filter((a) => a.toLowerCase() === 'mayor').length).toBe(1);
  });

  it('omits an empty tier so the consumer can render only the tiers it has', () => {
    const result = prioritizeAliases({
      operator: OPERATOR,
      sessionAliases: [],
      mailFromOrTo: [],
    });
    // Only 'you' should be present.
    expect(result).toHaveLength(1);
    expect(result[0]?.tier).toBe('you');
  });
});

describe('displayLabel', () => {
  it('renders the operator as "user" regardless of internal alias', () => {
    expect(displayLabel('stephanie', 'stephanie')).toBe('user');
  });

  it('passes non-operator aliases through unchanged', () => {
    expect(displayLabel('mayor', 'stephanie')).toBe('mayor');
    expect(displayLabel('scix-worker', 'stephanie')).toBe('scix-worker');
  });

  it('handles a hypothetical operator rename without breaking the rule', () => {
    // If OPERATOR_ALIAS were ever changed to 'human' upstream, the function
    // should still render the operator as 'user'. The check is identity, not
    // string equality with 'stephanie'.
    expect(displayLabel('human', 'human')).toBe('user');
  });
});
