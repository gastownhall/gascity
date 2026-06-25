import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it } from 'vitest';
import { isHistoricalLane, LaneCard } from './LaneCard';
import type { RunLane } from 'gas-city-dashboard-shared';

afterEach(() => cleanup());

describe('LaneCard navigation', () => {
  it('links run rows to the run detail route with scope query params', () => {
    const lane: RunLane = {
      id: 'gc-root',
      title: 'Adopt PR #42',
      formula: { status: 'known', name: 'mol-adopt-pr-v2' },
      scope: {
        status: 'available',
        kind: 'city',
        ref: 'racoon-city',
        rootStoreRef: 'city:racoon-city',
      },
      external: { status: 'unavailable', error: 'external unavailable in test' },
      phase: 'review',
      phaseLabel: 'review',
      statusCounts: { in_progress: 1 },
      activeAssignees: ['gc-session-b'],
      updatedAt: {
        status: 'available',
        at: '2026-05-24T12:00:00Z',
      },
      stages: [],
      progress: {
        status: 'unavailable',
        error: 'run progress unavailable in test',
      },
      formulaStageResolved: false,
      health: { status: 'unavailable', error: 'run health has not been derived' },
    };

    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <LaneCard lane={lane} now={Date.parse('2026-05-24T12:01:00Z')} />
      </MemoryRouter>,
    );

    const link = screen.getByRole('link', { name: /adopt pr #42/i });
    expect(link.getAttribute('href')).toBe('/runs/gc-root?scope_kind=city&scope_ref=racoon-city');
  });

  it('omits scope query params when the lane has unavailable scope', () => {
    const lane: RunLane = {
      id: 'gc-root',
      title: 'Adopt PR #42',
      formula: { status: 'known', name: 'mol-adopt-pr-v2' },
      scope: { status: 'unavailable', error: 'scope unavailable in test' },
      external: { status: 'unavailable', error: 'external unavailable in test' },
      phase: 'review',
      phaseLabel: 'review',
      statusCounts: { in_progress: 1 },
      activeAssignees: ['gc-session-b'],
      updatedAt: {
        status: 'available',
        at: '2026-05-24T12:00:00Z',
      },
      stages: [],
      progress: {
        status: 'unavailable',
        error: 'run progress unavailable in test',
      },
      formulaStageResolved: false,
      health: { status: 'unavailable', error: 'run health has not been derived' },
    };

    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <LaneCard lane={lane} now={Date.parse('2026-05-24T12:01:00Z')} />
      </MemoryRouter>,
    );

    const link = screen.getByRole('link', { name: /adopt pr #42/i });
    expect(link.getAttribute('href')).toBe('/runs/gc-root');
  });
});

// gascity-dashboard-f4ps: historical lanes ship from the backend with
// `health: { status: 'unavailable' }` because run health is derived only over
// the active subset. The health concepts (thrashing, stalled-session) are
// meaningless for completed runs, so the lane render must not surface the
// unavailable health string as if it were a degradation signal.
describe('LaneCard historical-lane render', () => {
  function makeHistoricalLane(overrides: Partial<RunLane> = {}): RunLane {
    return {
      id: 'gc-historical',
      title: 'Adopt PR #41 (merged)',
      formula: { status: 'known', name: 'mol-adopt-pr-v2' },
      scope: {
        status: 'available',
        kind: 'city',
        ref: 'racoon-city',
        rootStoreRef: 'city:racoon-city',
      },
      external: { status: 'unavailable', error: 'external unavailable in test' },
      phase: 'complete',
      phaseLabel: 'complete',
      statusCounts: { closed: 4 },
      activeAssignees: [],
      updatedAt: {
        status: 'available',
        at: '2026-04-20T12:00:00Z',
      },
      stages: [],
      progress: {
        status: 'unavailable',
        error: 'workflow progress unavailable in test',
      },
      formulaStageResolved: false,
      health: {
        status: 'unavailable',
        error: 'workflow health has not been derived',
      },
      ...overrides,
    };
  }

  it('isHistoricalLane returns true for phase: complete and false otherwise', () => {
    expect(isHistoricalLane(makeHistoricalLane())).toBe(true);
    expect(isHistoricalLane(makeHistoricalLane({ phase: 'review', phaseLabel: 'review' }))).toBe(
      false,
    );
    expect(isHistoricalLane(makeHistoricalLane({ phase: 'blocked', phaseLabel: 'blocked' }))).toBe(
      false,
    );
  });

  it('renders the run root bead id so look-alike runs are distinguishable (gascity-dashboard-7hek)', () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <LaneCard
          lane={makeHistoricalLane({ id: 'gc-zz9q1', title: 'mol-focus-review' })}
          now={Date.parse('2026-05-29T12:00:00Z')}
        />
      </MemoryRouter>,
    );
    // The root bead id is the only thing distinguishing same-formula runs.
    expect(screen.getByText('gc-zz9q1')).toBeTruthy();
  });

  it('does not render the health "unavailable" string for a historical lane', () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <LaneCard lane={makeHistoricalLane()} now={Date.parse('2026-05-29T12:00:00Z')} />
      </MemoryRouter>,
    );

    // The error text from health-unavailable must NEVER reach the DOM for a
    // historical lane: closed lanes have no honest health state to report.
    expect(screen.queryByText(/workflow health has not been derived/i)).toBeNull();
    expect(screen.queryByText(/unavailable/i)).toBeNull();
  });

  it('renders the phase label in a quieter (muted) tone for a historical lane', () => {
    render(
      <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
        <LaneCard lane={makeHistoricalLane()} now={Date.parse('2026-05-29T12:00:00Z')} />
      </MemoryRouter>,
    );

    // The phase label for a completed lane should not visually equate with
    // the active-lane register; quiet greyscale tone marks it as past-tense.
    const label = screen.getByText('complete');
    expect(label.className).toContain('text-fg-muted');
    expect(label.className).not.toContain('text-accent');
  });
});
