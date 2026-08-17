import { cleanup, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { AttentionItem } from '../../attention/compose';
import { composeAttention } from '../../attention/compose';
import { createAttentionContributors } from '../../attention/registry';
import type { Bead } from 'gas-city-dashboard-shared/gc-supervisor';
import { BeadAttentionPanel, beadIdFromHref } from './BeadAttentionPanel';

afterEach(() => cleanup());

function attentionItem(
  overrides: Partial<AttentionItem> & Pick<AttentionItem, 'id'>,
): AttentionItem {
  return {
    domain: 'beads',
    severity: 'attention',
    title: 'Bead',
    ...overrides,
  };
}

function bead(overrides: Partial<Bead>): Bead {
  return {
    created_at: '2026-06-01T11:00:00.000Z',
    id: 'B-0',
    issue_type: 'task',
    status: 'open',
    title: 'Bead',
    ...overrides,
  };
}

describe('beadIdFromHref', () => {
  it('extracts the bead id from a /beads?bead=… href', () => {
    expect(beadIdFromHref('/beads?bead=B-1')).toBe('B-1');
  });
  it('returns null for an href with no bead param', () => {
    expect(beadIdFromHref('/beads')).toBeNull();
    expect(beadIdFromHref(undefined)).toBeNull();
  });
});

describe('BeadAttentionPanel (gascity-dashboard-2j8e.3)', () => {
  const noop = () => undefined;

  it('opens each item and never offers an operator Claim (gascity-dashboard-2j8e.8)', () => {
    const onOpen = vi.fn();
    render(
      <BeadAttentionPanel
        items={[
          attentionItem({
            id: 'beads:B-ready:ready-unclaimed',
            severity: 'watch',
            title: 'B-ready unclaimed',
            href: '/beads?bead=B-ready',
          }),
          attentionItem({
            id: 'beads:B-esc:escalated',
            title: 'B-esc escalated',
            href: '/beads?bead=B-esc',
          }),
        ]}
        onOpen={onOpen}
      />,
    );

    expect(screen.getByText('Needs you').textContent).toContain('(2)');
    // The operator cannot be a bead assignee, so no row offers Claim — only Open.
    expect(screen.queryByRole('button', { name: 'Claim' })).toBeNull();

    const readyRow = screen.getByText('B-ready unclaimed').closest('li') as HTMLElement;
    within(readyRow).getByRole('button', { name: 'Open' }).click();
    expect(onOpen).toHaveBeenCalledWith('B-ready');

    const escRow = screen.getByText('B-esc escalated').closest('li') as HTMLElement;
    within(escRow).getByRole('button', { name: 'Open' }).click();
    expect(onOpen).toHaveBeenCalledWith('B-esc');
  });

  it('renders nothing when there are no badge-counting items', () => {
    const { container } = render(<BeadAttentionPanel items={[]} onOpen={noop} />);
    expect(container.firstChild).toBeNull();
  });

  it('renders exactly the nav-badge count — page and badge read the same model', () => {
    // Build the real composed model the nav badge reads, then assert the panel
    // renders one row per badge-counted item (attention + watch).
    const model = composeAttention(
      createAttentionContributors({
        beads: {
          decisionLabel: 'needs/stephanie',
          nowMs: Date.parse('2026-06-07T12:00:00.000Z'),
          items: [
            bead({ id: 'B-ready', status: 'open', created_at: '2026-06-04T11:00:00.000Z' }),
            // plain dependency-blocked — excluded from both badge and page.
            bead({ id: 'B-dep', status: 'blocked' }),
          ],
          escalations: [bead({ id: 'B-esc', status: 'blocked', labels: ['gc:escalation'] })],
          // mayor-decision items count toward the badge too — they must also
          // render in the panel, so the parity holds across every source.
          decisions: [bead({ id: 'B-dec', title: 'Decide: X', labels: ['needs/stephanie'] })],
        },
      }),
    );
    const summary = model.byDomain.beads;
    const navTotal = summary.attention + summary.watch;
    expect(navTotal).toBe(3);

    render(<BeadAttentionPanel items={summary.items} onOpen={noop} />);

    expect(screen.getByText('Needs you').textContent).toContain(`(${navTotal})`);
    expect(screen.getAllByRole('button', { name: 'Open' })).toHaveLength(navTotal);
  });
});
