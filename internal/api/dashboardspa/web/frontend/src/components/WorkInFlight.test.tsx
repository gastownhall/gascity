import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { WorkInFlight } from './WorkInFlight';
import { NowProvider } from '../contexts/NowContext';
import { setActiveCity } from '../api/cityBase';
import type { SupervisorBead } from '../supervisor/beadReads';
import type { SupervisorSession } from '../supervisor/sessionReads';

// The "Workers active" section is SESSION-driven: it counts the live worker
// sessions, groups them by rig for the calm summary, and best-effort attaches an
// in-progress bead when one is captured. Orchestration sessions are excluded;
// an unassigned in-progress bead is NOT surfaced (the worker is the signal).

function bead(partial: Partial<SupervisorBead> & { id: string }): SupervisorBead {
  return {
    title: `title for ${partial.id}`,
    status: 'in_progress',
    issue_type: 'task',
    created_at: '2026-06-03T00:00:00Z',
    ...partial,
  } as SupervisorBead;
}

function session(partial: Partial<SupervisorSession> & { id: string }): SupervisorSession {
  return {
    template: 'polecat',
    session_name: partial.id,
    title: partial.id,
    state: 'active',
    created_at: '2026-06-03T00:00:00Z',
    attached: false,
    running: true,
    provider: 'claude',
    ...partial,
  } as SupervisorSession;
}

function renderSection(
  beads: SupervisorBead[],
  sessions: SupervisorSession[],
  opts: { loading?: boolean; error?: string | null } = {},
) {
  return render(
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      <NowProvider intervalMs={1_000_000}>
        <WorkInFlight
          beads={beads}
          sessions={sessions}
          sessionsLoading={opts.loading ?? false}
          sessionsError={opts.error ?? null}
        />
      </NowProvider>
    </MemoryRouter>,
  );
}

const transcriptUrls: string[] = [];

function stubTranscriptFetch() {
  transcriptUrls.length = 0;
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = input instanceof Request ? input.url : String(input);
      const path = url.startsWith(window.location.origin)
        ? url.slice(window.location.origin.length)
        : url;
      transcriptUrls.push(path);
      return new Response(
        JSON.stringify({
          id: 'gc-335825',
          template: 'polecat',
          provider: 'claude',
          format: 'conversation',
          turns: [{ role: 'assistant', text: 'worker transcript snapshot' }],
        }),
        { status: 200, headers: { 'content-type': 'application/json' } },
      );
    }),
  );
}

beforeEach(() => {
  setActiveCity('test-city');
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('WorkInFlight (Workers active)', () => {
  it('renders the calm session-driven summary line grouped by rig', () => {
    const sessions = [
      session({ id: 'gc-1', template: 'polecat', rig: '/home/ds/gascity' }),
      session({ id: 'gc-2', template: 'polecat', rig: '/home/ds/gascity' }),
      session({ id: 'gc-3', template: 'polecat', rig: '/home/ds/gascity' }),
      session({ id: 'gc-4', template: 'scix-worker', rig: 'scix_experiments' }),
      session({ id: 'gc-5', template: 'scix-worker', rig: 'scix_experiments' }),
      session({ id: 'gc-6', template: 'scix-worker', rig: 'scix_experiments' }),
      session({ id: 'gc-7', template: 'polecat', rig: '/home/ds/gascity-packs-main' }),
      session({ id: 'gc-8', template: 'polecat', rig: '/home/ds/gascity-packs-main' }),
      session({ id: 'gc-9', template: 'worker', rig: 'zeldascension' }),
      // Orchestration — excluded from the count.
      session({ id: 'gc-m', template: 'mayor', rig: '' }),
    ];
    renderSection([], sessions);
    expect(screen.getByText('Workers active')).toBeTruthy();
    expect(
      screen.getByText(
        '9 workers active across gascity (3), scix_experiments (3), gascity-packs (2), zeldascension (1).',
      ),
    ).toBeTruthy();
  });

  it('renders a per-worker row "<rig> · <clean-worker>" with relative activity', () => {
    const sessions = [
      session({
        id: 'gc-335825',
        template: 'polecat',
        rig: '/home/ds/gascity-main',
        last_active: '2026-06-03T00:00:00Z',
      }),
    ];
    renderSection([], sessions);
    // -main rig suffix stripped, role cleaned.
    expect(screen.getByText('gascity')).toBeTruthy();
    expect(screen.getByText('polecat')).toBeTruthy();
  });

  it('appends "→ <bead-id>: <title>" when an in-progress bead embeds the session id', () => {
    const sessions = [session({ id: 'gc-335825', template: 'polecat', rig: 'gascity' })];
    const beads = [bead({ id: 'gc-5rarj', title: 'fix the thing', assignee: 'polecat-gc-335825' })];
    renderSection(beads, sessions);
    const link = screen.getByRole('link', { name: /gc-5rarj/ });
    expect(link.getAttribute('href')).toBe('/beads?bead=gc-5rarj');
    expect(link.textContent).toContain('fix the thing');
  });

  it('does NOT surface an unassigned in-progress bead as a row', () => {
    const sessions = [session({ id: 'gc-1', template: 'polecat', rig: 'gascity' })];
    // A stalled, unassigned in-progress bead is not a working worker.
    renderSection([bead({ id: 'gc-stalled' })], sessions);
    expect(screen.queryByText(/gc-stalled/)).toBeNull();
    // No bead link rendered for the worker without a captured bead.
    expect(screen.queryByRole('link')).toBeNull();
  });

  it("opens the peek for the worker's own session id when its Peek control is clicked", async () => {
    stubTranscriptFetch();
    const sessions = [
      session({ id: 'gc-335825', template: 'polecat', rig: 'gascity', state: 'active' }),
    ];
    renderSection([], sessions);

    fireEvent.click(screen.getByRole('button', { name: /peek/i }));

    // The worker row carries session.id directly (no name→id remap), so the
    // peek must hit the transcript route for THAT exact session id.
    await waitFor(() => {
      expect(transcriptUrls).toContain(
        '/gc-supervisor/v0/city/test-city/session/gc-335825/transcript?format=conversation',
      );
    });
  });

  it('opens the peek for the clicked worker (not just any worker), mirroring the Available-agents roster', async () => {
    stubTranscriptFetch();
    // Two distinct workers so the test proves the clicked label maps to ITS OWN
    // session — a single-worker render would still pass if the wiring opened the
    // first/only session regardless of which label was clicked.
    const sessions = [
      session({ id: 'gc-100', template: 'polecat', rig: 'alpha', state: 'active' }),
      session({ id: 'gc-200', template: 'polecat', rig: 'bravo', state: 'active' }),
    ];
    renderSection([], sessions);

    // The worker label is itself a button (mirroring the clickable agent-roster
    // row label), distinct from that row's explicit Peek button — its accessible
    // name is the worker identity ("bravo · polecat"), not "Peek".
    const bravoLabel = screen.getByRole('button', { name: /bravo.*polecat/i });
    expect(screen.getAllByRole('button', { name: /peek/i })).not.toContain(bravoLabel);

    fireEvent.click(bravoLabel);

    // Only the clicked worker's transcript is fetched — the other worker's is not.
    await waitFor(() => {
      expect(transcriptUrls).toContain(
        '/gc-supervisor/v0/city/test-city/session/gc-200/transcript?format=conversation',
      );
    });
    expect(transcriptUrls).not.toContain(
      '/gc-supervisor/v0/city/test-city/session/gc-100/transcript?format=conversation',
    );
  });

  it('surfaces the captured bead as a link beside the peek transcript', async () => {
    stubTranscriptFetch();
    const sessions = [session({ id: 'gc-335825', template: 'polecat', rig: 'gascity' })];
    const beads = [bead({ id: 'gc-5rarj', title: 'fix the thing', assignee: 'polecat-gc-335825' })];
    renderSection(beads, sessions);

    fireEvent.click(screen.getByRole('button', { name: /peek/i }));

    // The peek caption links the worker's captured bead to its detail view.
    const beadLinks = await screen.findAllByRole('link', { name: /gc-5rarj/ });
    expect(beadLinks.some((l) => l.getAttribute('href') === '/beads?bead=gc-5rarj')).toBe(true);
  });

  it('renders no Peek control when there are no active workers', () => {
    renderSection([], [session({ id: 'gc-m', template: 'mayor', rig: '' })]);
    expect(screen.queryByRole('button', { name: /peek/i })).toBeNull();
  });

  it('shows the calm empty state when no workers are active', () => {
    // Only orchestration + an in-progress bead present: still empty.
    renderSection(
      [bead({ id: 'gc-1', assignee: 'polecat-gc-1' })],
      [session({ id: 'gc-m', template: 'mayor', rig: '' })],
    );
    expect(screen.getByText('No workers active right now.')).toBeTruthy();
    expect(screen.queryByRole('link')).toBeNull();
  });

  // Fail-safe: when the sessions fetch has not delivered data, the section must
  // NOT render the calm "No workers active" all-clear — that would mask a dead
  // aggregator on the exact surface the work-in-flight signal is meant to make
  // trustworthy. Distinguish a failed fetch from an in-flight one.
  it('renders explicit unavailable (not the all-clear) when the sessions fetch failed with no data', () => {
    renderSection([], [], { error: 'sessions backend unavailable' });
    expect(screen.getByText('Worker status unavailable.')).toBeTruthy();
    expect(screen.queryByText('No workers active right now.')).toBeNull();
  });

  it('renders a loading state (not the all-clear) while the initial sessions fetch is in flight', () => {
    renderSection([], [], { loading: true });
    expect(screen.getByText('Checking worker status…')).toBeTruthy();
    expect(screen.queryByText('No workers active right now.')).toBeNull();
  });

  it('keeps rendering stale workers when a re-fetch errors but prior data is retained', () => {
    // useCachedData holds the last good data across a re-fetch failure, so the
    // false-all-clear window is only the no-data case — stale data still counts.
    renderSection([], [session({ id: 'gc-1', template: 'polecat', rig: 'gascity' })], {
      error: 're-fetch failed',
    });
    expect(screen.queryByText('Worker status unavailable.')).toBeNull();
    expect(screen.getByText('1 worker active across gascity (1).')).toBeTruthy();
  });
});
