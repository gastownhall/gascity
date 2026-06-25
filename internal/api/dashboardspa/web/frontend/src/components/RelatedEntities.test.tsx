import { cleanup, render, screen, fireEvent, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { EntityLinkView } from 'gas-city-dashboard-shared';
import { RelatedEntities, isStale } from './RelatedEntities';
import { assertExactlyOneMark } from '../test/assertions/oneMarkRule';

afterEach(() => cleanup());

const NOW = Date.parse('2026-05-26T12:00:00Z');

function view(over: Partial<EntityLinkView> = {}): EntityLinkView {
  return {
    focus: { key: 'bead:c:focus', type: 'bead', ref: 'focus' },
    nodes: [
      {
        key: 'bead:c:focus',
        type: 'bead',
        ref: 'focus',
        title: 'Focus',
        status: 'open',
        url: null,
        fetchedAt: '2026-05-26T11:59:30Z',
        unresolved: false,
      },
      {
        key: 'bead:c:child',
        type: 'bead',
        ref: 'child',
        title: 'Child bead',
        status: 'open',
        url: null,
        fetchedAt: '2026-05-26T11:59:30Z',
        unresolved: false,
      },
      {
        key: 'session:session:s1',
        type: 'session',
        ref: 's1',
        title: 'session one',
        status: 'active',
        url: null,
        fetchedAt: '2026-05-26T11:59:30Z',
        unresolved: false,
      },
      {
        key: 'github_pr:github:42',
        type: 'github_pr',
        ref: 'pr/42',
        title: null,
        status: null,
        url: 'https://github.com/o/r/pull/42',
        fetchedAt: '2026-05-25T11:00:00Z',
        unresolved: true,
      },
    ],
    edges: [
      {
        from: 'bead:c:focus',
        to: 'bead:c:child',
        relation: 'child',
        provenance: 'supervisor',
        resolved: true,
      },
      {
        from: 'bead:c:focus',
        to: 'session:session:s1',
        relation: 'session',
        provenance: 'supervisor',
        resolved: true,
      },
      {
        from: 'bead:c:focus',
        to: 'github_pr:github:42',
        relation: 'pr',
        provenance: 'supervisor',
        resolved: false,
      },
    ],
    stats: [],
    partial: false,
    generatedAt: '2026-05-26T12:00:00Z',
    asOf: '2026-05-25T11:00:00Z',
    ...over,
  };
}

function renderRelated(v: EntityLinkView, onOpenBead = vi.fn()) {
  return {
    onOpenBead,
    ...render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <RelatedEntities view={v} loading={false} error={null} now={NOW} onOpenBead={onOpenBead} />
      </MemoryRouter>,
    ),
  };
}

describe('RelatedEntities (R5/R6/RK3)', () => {
  it('renders groups under tracked labels with no card container', () => {
    const { container } = renderRelated(view());
    fireEvent.click(screen.getByRole('button', { name: /show detail/i }));
    expect(screen.getByRole('heading', { name: /related/i })).toBeTruthy();
    expect(screen.getByText(/^Beads$/)).toBeTruthy();
    expect(screen.getByText(/^Sessions$/)).toBeTruthy();
    expect(screen.getByText(/^Pull requests$/)).toBeTruthy();
    // No card / chip / left-stripe class anywhere in the section (DESIGN.md).
    const html = container.innerHTML;
    expect(html).not.toMatch(/\bcard\b/);
    expect(html).not.toMatch(/border-l-/); // side-stripe
    expect(html).not.toMatch(/rounded-(?:xl|2xl|full)/);
  });

  it('shows at most one maroon (text-accent) mark in the section', () => {
    // Three unresolved links cross the threshold → one aggregate maroon.
    const v = view({
      nodes: [
        ...view().nodes,
        {
          key: 'github_issue:github:7',
          type: 'github_issue',
          ref: 'issue/7',
          title: null,
          status: null,
          url: null,
          fetchedAt: null,
          unresolved: true,
        },
        {
          key: 'github_issue:github:8',
          type: 'github_issue',
          ref: 'issue/8',
          title: null,
          status: null,
          url: null,
          fetchedAt: null,
          unresolved: true,
        },
      ],
      edges: [
        ...view().edges,
        {
          from: 'bead:c:focus',
          to: 'github_issue:github:7',
          relation: 'issue',
          provenance: 'supervisor',
          resolved: false,
        },
        {
          from: 'bead:c:focus',
          to: 'github_issue:github:8',
          relation: 'issue',
          provenance: 'supervisor',
          resolved: false,
        },
      ],
    });
    const { container } = renderRelated(v);
    fireEvent.click(screen.getByRole('button', { name: /show detail/i }));
    // One Mark Rule (DESIGN.md): three unresolved links cross the
    // threshold → exactly one aggregate maroon. The exact-1 claim is
    // load-bearing — a broken aggregation that paints zero maroon must
    // not pass silently.
    assertExactlyOneMark(container);
  });

  it('clicking a resolved bead row invokes the open handler', () => {
    const { onOpenBead } = renderRelated(view());
    fireEvent.click(screen.getByRole('button', { name: /show detail/i }));
    fireEvent.click(screen.getByRole('button', { name: /child bead/i }));
    expect(onOpenBead).toHaveBeenCalledWith('child');
  });

  it('R6: an unresolved PR row shows "unresolved" and an outbound link', () => {
    renderRelated(view());
    fireEvent.click(screen.getByRole('button', { name: /show detail/i }));
    const prGroup = screen.getByText(/^Pull requests$/).closest('div')!;
    expect(within(prGroup).getByText(/unresolved/i)).toBeTruthy();
    const link = within(prGroup).getByRole('link');
    expect(link.getAttribute('href')).toBe('https://github.com/o/r/pull/42');
  });

  it('renders a calm empty state', () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <RelatedEntities
          view={{ ...view(), nodes: [view().nodes[0]!], edges: [] }}
          loading={false}
          error={null}
          now={NOW}
        />
      </MemoryRouter>,
    );
    expect(screen.getByText(/no related entities/i)).toBeTruthy();
  });
});

describe('isStale (R7/RK2)', () => {
  it('flags a node older than the staleness band', () => {
    expect(isStale('2026-05-25T11:00:00Z', NOW)).toBe(true); // ~25h old
    expect(isStale('2026-05-26T11:59:30Z', NOW)).toBe(false); // 30s old
    expect(isStale(null, NOW)).toBe(false);
  });
});
