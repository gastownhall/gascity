import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { AgentPanel } from '../components/AgentPanel';
import type { AliasBucket } from '../hooks/aliasPriority';

// gascity-dashboard-5gg: Mail agent panel degraded-state footnote.
//
// Three footnote branches exercised here:
//
//   1. No footnote when sessionsUnavailable=false.
//   2. Single-line "Agent list unavailable; …" when sessions fetch failed
//      but the mail-derived alias list still has non-operator entries.
//   3. Broader "Agent list and mail history both unavailable." when the
//      visible buckets collapse to the operator-only entry (both fetches
//      failed, or the mail corpus is genuinely empty).
//
// The bead description called the existing copy misleading in branch 3:
// "showing mail-derived aliases only" reads as if mail-derived aliases
// were present, when in fact they aren't — only the operator was.

afterEach(() => {
  cleanup();
});

function renderPanel(props: Partial<React.ComponentProps<typeof AgentPanel>> = {}) {
  return render(
    <AgentPanel
      buckets={props.buckets ?? [{ tier: 'you', aliases: ['stephanie'] }]}
      loading={props.loading ?? false}
      sessionsUnavailable={props.sessionsUnavailable ?? false}
      value={props.value ?? 'stephanie'}
      onChange={props.onChange ?? (() => {})}
      onReset={props.onReset ?? (() => {})}
      isOperator={props.isOperator ?? true}
    />,
  );
}

describe('AgentPanel — degraded-state footnote', () => {
  it('renders no footnote when sessionsUnavailable=false', () => {
    renderPanel({
      buckets: [
        { tier: 'you', aliases: ['stephanie'] },
        { tier: 'active', aliases: ['mechanic'] },
      ],
      sessionsUnavailable: false,
    });
    // Expand the panel to see the footnote region.
    fireEvent.click(screen.getByRole('button', { name: /agents/i }));
    expect(screen.queryByText(/agent list.*unavailable/i)).toBeNull();
  });

  it('renders the "mail-derived aliases only" copy when sessionsUnavailable AND non-operator aliases are visible', () => {
    const buckets: AliasBucket[] = [
      { tier: 'you', aliases: ['stephanie'] },
      { tier: 'active', aliases: ['mechanic', 'scix-worker'] },
    ];
    renderPanel({ buckets, sessionsUnavailable: true });
    fireEvent.click(screen.getByRole('button', { name: /agents/i }));
    expect(
      screen.getByText(/agent list unavailable; showing mail-derived aliases only/i),
    ).toBeTruthy();
    // The broader copy must NOT appear simultaneously.
    expect(screen.queryByText(/agent list and mail history both unavailable/i)).toBeNull();
  });

  it('renders the broader "agent list and mail history both unavailable" copy when only the operator entry is visible', () => {
    // Both fetches failed (or mail corpus genuinely empty): the only
    // bucket is the operator-only 'you' tier.
    const buckets: AliasBucket[] = [{ tier: 'you', aliases: ['stephanie'] }];
    renderPanel({ buckets, sessionsUnavailable: true });
    fireEvent.click(screen.getByRole('button', { name: /agents/i }));
    expect(screen.getByText(/agent list and mail history both unavailable/i)).toBeTruthy();
    // The narrower copy must NOT appear simultaneously — that's the
    // misleading-copy regression the bead flagged.
    expect(screen.queryByText(/showing mail-derived aliases only/i)).toBeNull();
  });

  it('does not render any degraded-state footnote while loading=true', () => {
    // While the initial fetch is in flight, the panel shows "Loading
    // aliases" / "Loading more agents" instead of the terminal copy.
    const buckets: AliasBucket[] = [{ tier: 'you', aliases: ['stephanie'] }];
    renderPanel({ buckets, sessionsUnavailable: true, loading: true });
    fireEvent.click(screen.getByRole('button', { name: /agents/i }));
    expect(screen.queryByText(/agent list unavailable/i)).toBeNull();
    expect(screen.queryByText(/agent list and mail history both unavailable/i)).toBeNull();
  });
});
