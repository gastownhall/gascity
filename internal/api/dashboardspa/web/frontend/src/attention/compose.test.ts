import { describe, expect, it } from 'vitest';
import {
  ATTENTION_DOMAINS,
  composeAttention,
  type AttentionContributor,
  type AttentionItem,
} from './compose';

describe('composeAttention', () => {
  it('derives per-domain counts and highest severity from item-level facts', () => {
    const model = composeAttention([
      contributor('runs', [
        item('run-attention', 'runs', 'attention'),
        item('run-watch', 'runs', 'watch'),
      ]),
      contributor('mail', [item('mail-watch', 'mail', 'watch')]),
    ]);

    expect(model.byDomain.runs.attention).toBe(1);
    expect(model.byDomain.runs.watch).toBe(1);
    expect(model.byDomain.runs.severity).toBe('attention');
    expect(model.byDomain.mail.attention).toBe(0);
    expect(model.byDomain.mail.watch).toBe(1);
    expect(model.byDomain.mail.severity).toBe('watch');
    expect(model.byDomain.agents.severity).toBeNull();
  });

  it('ranks attention before watch, current actionable items before historical items, then nav domain order', () => {
    const model = composeAttention([
      contributor('mail', [item('mail-watch', 'mail', 'watch', { actionable: true })]),
      contributor('activity', [
        item('historical-activity', 'activity', 'attention', { current: false, actionable: true }),
      ]),
      contributor('beads', [item('bead-attention', 'beads', 'attention')]),
      contributor('agents', [item('agent-attention', 'agents', 'attention', { actionable: true })]),
    ]);

    expect(model.items.map((attention) => attention.id)).toEqual([
      'agent-attention',
      'bead-attention',
      'historical-activity',
      'mail-watch',
    ]);
  });

  it('splits Home top items from grouped overflow with stable domain ordering', () => {
    const model = composeAttention(
      [
        contributor('runs', [
          item('run-1', 'runs', 'attention', { actionable: true }),
          item('run-2', 'runs', 'watch'),
        ]),
        contributor('mail', [item('mail-1', 'mail', 'watch'), item('mail-2', 'mail', 'watch')]),
      ],
      { topLimit: 2 },
    );

    expect(model.topItems.map((attention) => attention.id)).toEqual(['run-1', 'run-2']);
    expect(model.overflowByDomain).toEqual([
      { domain: 'mail', attention: 0, watch: 2, unavailable: 0, total: 2 },
    ]);
  });

  it('counts unavailable items in their own tier without inflating the badge or recoloring it', () => {
    const model = composeAttention([
      contributor('runs', [
        item('run-watch', 'runs', 'watch'),
        item('run-degraded', 'runs', 'unavailable', { provenance: 'stale' }),
        item('run-down', 'runs', 'unavailable', { provenance: 'error' }),
      ]),
    ]);

    // Badge count is attention + watch only — the two unavailable items are excluded.
    expect(model.byDomain.runs.attention).toBe(0);
    expect(model.byDomain.runs.watch).toBe(1);
    expect(model.byDomain.runs.unavailable).toBe(2);
    // The badge tier stays at watch; degraded reads never escalate the color.
    expect(model.byDomain.runs.severity).toBe('watch');
  });

  it('never sets the badge severity from unavailable items alone', () => {
    const model = composeAttention([
      contributor('runs', [
        item('feed-partial', 'runs', 'unavailable', { provenance: 'fresh' }),
        item('detail-down', 'runs', 'unavailable', { provenance: 'error' }),
      ]),
    ]);

    expect(model.byDomain.runs.attention).toBe(0);
    expect(model.byDomain.runs.watch).toBe(0);
    expect(model.byDomain.runs.unavailable).toBe(2);
    // No attention/watch items ⇒ no badge, even though the domain has items.
    expect(model.byDomain.runs.severity).toBeNull();
  });

  it('ranks unavailable items last and groups them as their own overflow count', () => {
    const model = composeAttention(
      [
        contributor('runs', [
          item('run-attention', 'runs', 'attention', { actionable: true }),
          item('run-unavailable', 'runs', 'unavailable'),
          item('run-watch', 'runs', 'watch'),
        ]),
      ],
      { topLimit: 1 },
    );

    // attention < watch < unavailable in rank order.
    expect(model.items.map((entry) => entry.id)).toEqual([
      'run-attention',
      'run-watch',
      'run-unavailable',
    ]);
    expect(model.overflowByDomain).toEqual([
      { domain: 'runs', attention: 0, watch: 1, unavailable: 1, total: 2 },
    ]);
  });

  it('keeps the domain set explicit so nav consumers cannot drift', () => {
    expect(ATTENTION_DOMAINS).toEqual([
      'agents',
      'beads',
      'runs',
      'mail',
      'activity',
      'health',
    ]);
  });
});

function contributor(
  domain: AttentionItem['domain'],
  items: readonly AttentionItem[],
): AttentionContributor {
  return {
    id: `${domain}-test`,
    domain,
    getItems: () => items,
  };
}

function item(
  id: string,
  domain: AttentionItem['domain'],
  severity: AttentionItem['severity'],
  overrides: Partial<AttentionItem> = {},
): AttentionItem {
  return {
    id,
    domain,
    severity,
    title: id,
    href: `/${domain}`,
    current: true,
    actionable: false,
    ...overrides,
  };
}
