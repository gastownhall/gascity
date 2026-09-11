import { useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import type { SupervisorBead } from '../supervisor/beadReads';
import type { SupervisorSession } from '../supervisor/sessionReads';
import { useNow } from '../contexts/NowContext';
import { formatRelative } from '../hooks/time';
import {
  deriveActiveWorkers,
  summarizeActiveWorkers,
  type ActiveWorker,
} from '../hooks/activeWorkers';
import { Button } from './Button';
import { Modal } from './Modal';
import { LiveSessionPeek } from './LiveSessionPeek';
import { StatusBadge, stateTone } from './StatusBadge';

// "Workers active" — the operator's calm at-a-glance answer to "what is
// actually working right now".
//
// SESSION-DRIVEN, not bead-driven. The work-beads churn to zero within seconds
// (focus-reviews finish fast) and live in rig stores the dashboard's bead fetch
// doesn't reliably aggregate, while the worker SESSIONS stay active across that
// churn. So the primary signal is the live worker sessions, grouped by rig:
//
//     8 workers active across gascity (3), scix-experiments (3), gascity-packs (2).
//
// Per-worker rows below the summary, most-recently-active first:
//
//     gascity · polecat · 2m              [→ gc-5rarj: fix the thing]
//
// The bead is best-effort secondary context — attached only when an in-progress
// bead's assignee embeds this session's id. The common case is no bead; the
// worker being active IS the signal, so there is never an "unassigned" row.

interface WorkInFlightProps {
  beads: readonly SupervisorBead[];
  sessions: readonly SupervisorSession[];
  // The "Workers active" count is SESSION-driven, so the section's honesty
  // depends on the sessions fetch having actually delivered. Thread its
  // loading/error state through: when sessions data is absent (initial load or
  // a fetch that never succeeded) the section must render an explicit
  // unavailable/loading state — never the calm "No workers active" all-clear,
  // which would mask a dead aggregator (the fail-safe invariant: failure must
  // read as explicit unavailable, never silent all-clear). Beads are
  // best-effort secondary context and do not gate this signal.
  sessionsLoading: boolean;
  sessionsError: string | null;
}

function WorkerRow({
  worker,
  accent,
  onPeek,
}: {
  worker: ActiveWorker;
  accent: boolean;
  onPeek: (sessionId: string) => void;
}) {
  const now = useNow();
  const { session, rig, bead } = worker;
  // Workers being "active" is normal, not an alert. Per the One Mark Rule, only
  // the FIRST stuck/failed worker (accent === true) renders its state badge in
  // tone; every other worker reads as neutral state text so at most one maroon
  // mark appears per viewport.
  const tone = accent ? stateTone(session.state) : 'neutral';
  return (
    <li className="px-2 py-2 -mx-2 rounded-sm transition-colors duration-150 ease-out-quart hover:bg-surface-tint/60">
      <div className="flex items-baseline justify-between gap-4">
        <div className="min-w-0 text-body text-fg">
          {/* Consistency with the Available-agents roster (Agents.tsx), whose
              row label is a clickable Link into the agent's detail: the worker
              label opens this session's transcript in the same LiveSessionPeek
              the Peek button uses, so the whole worker is one click from its
              transcript. A button (not a Link) because the peek is a modal, not
              a route. focus-mark + group-hover accent match the roster's
              interactive affordance; the bold-rig / muted-worker hierarchy is
              preserved at rest. */}
          <button
            type="button"
            onClick={() => onPeek(session.id)}
            className="group text-left cursor-pointer focus-mark"
            title={`Open ${rig} · ${worker.worker} transcript`}
          >
            <span className="font-medium group-hover:text-accent">{rig}</span>
            <span className="text-fg-faint" aria-hidden="true">
              {' '}
              ·{' '}
            </span>
            <span className="text-fg-muted group-hover:text-accent">{worker.worker}</span>
          </button>
          {bead && (
            <Link
              to={`/beads?bead=${encodeURIComponent(bead.id)}`}
              className="hover:text-accent focus-mark"
              title={`Open ${bead.id}`}
            >
              <span className="text-fg-faint" aria-hidden="true">
                {' '}
                →{' '}
              </span>
              <span className="tnum text-fg-muted">{bead.id}</span>
              <span className="text-fg-muted">: {bead.title}</span>
            </Link>
          )}
        </div>
        <div className="flex items-baseline gap-3 shrink-0">
          <StatusBadge tone={tone} label={session.state} />
          <span className="tnum text-fg-muted w-10 text-right">
            {formatRelative(session.last_active, now)}
          </span>
          {/* The worker row IS a live session, so its session.id is directly
              available — no name→id remap (unlike the Agents roster, where
              SessionInfo carries only the name). Peek opens that session's
              transcript in the shared LiveSessionPeek modal. */}
          <Button size="sm" tone="quiet" onClick={() => onPeek(session.id)}>
            Peek
          </Button>
        </div>
      </div>
    </li>
  );
}

// A worker session is worth a live stream when it carries the running signal
// (mirrors isSessionStreamable / isRunningAgent: process `running`, or gc state
// `active`/`running`). Workers in this section are active by construction, but
// a freshly-stuck worker may not be — gate the stream so a dead session shows a
// snapshot instead of a perpetual "connecting" badge.
function isWorkerStreamable(session: SupervisorSession): boolean {
  return session.running === true || session.state === 'active' || session.state === 'running';
}

export function WorkInFlight({
  beads,
  sessions,
  sessionsLoading,
  sessionsError,
}: WorkInFlightProps) {
  // Memoized on the fetched inputs: the parent AgentsPage re-renders every
  // second on the useNow tick, and the derivation walks the full bead +
  // session lists — only recompute when a fetch/SSE refresh changes them.
  const active = useMemo(() => deriveActiveWorkers(sessions, beads), [sessions, beads]);
  const summary = useMemo(() => summarizeActiveWorkers(active), [active]);

  // Peek key is the worker's live session id (directly available on the row).
  const [peekSessionId, setPeekSessionId] = useState<string | null>(null);
  const peekWorker = useMemo(
    () =>
      peekSessionId ? (active.workers.find((w) => w.session.id === peekSessionId) ?? null) : null,
    [active.workers, peekSessionId],
  );

  // One Mark Rule: render at most one accent state badge — the first worker
  // whose state is stuck/failed (an actual anomaly). Every other worker's state
  // is neutral, since an active worker is the expected, calm case.
  const accentIndex = useMemo(
    () => active.workers.findIndex((w) => stateTone(w.session.state) === 'stuck'),
    [active.workers],
  );

  // Fail-safe: the all-clear ("No workers active") is only honest once the
  // sessions fetch has delivered data. With no data yet, distinguish a failed
  // fetch (explicit "unavailable") from an in-flight one (loading) — the mirror
  // of the Agents roster's rosterUnavailable pattern. useCachedData retains
  // stale data across a re-fetch error, so this window is only the initial-load
  // / never-succeeded case; a prior success keeps rendering its workers.
  const sessionsAbsent = sessions.length === 0;
  const sessionsUnavailable = sessionsError !== null && sessionsAbsent;
  const sessionsPending = sessionsLoading && sessionsAbsent;
  const countLabel = sessionsUnavailable || sessionsPending ? '—' : active.total;

  return (
    <section className="mb-10" aria-label="Workers active">
      <header className="flex items-baseline justify-between border-b border-rule pb-2 mb-4">
        <h2 className="text-headline text-fg">Workers active</h2>
        <span className="text-label tnum text-fg-muted">{countLabel}</span>
      </header>
      {sessionsUnavailable ? (
        <p className="text-body text-fg-muted" role="status">
          Worker status unavailable.
        </p>
      ) : sessionsPending ? (
        <p className="text-body text-fg-muted" role="status">
          Checking worker status…
        </p>
      ) : active.total === 0 ? (
        <p className="text-body text-fg-muted">No workers active right now.</p>
      ) : (
        <>
          <p className="text-body text-fg-muted mb-4">{summary}</p>
          <ul className="space-y-1">
            {active.workers.map((worker, i) => (
              <WorkerRow
                key={worker.session.id}
                worker={worker}
                accent={i === accentIndex}
                onPeek={setPeekSessionId}
              />
            ))}
          </ul>
        </>
      )}

      <Modal
        open={peekWorker !== null}
        onClose={() => setPeekSessionId(null)}
        title={peekWorker ? `${peekWorker.rig} · ${peekWorker.worker}` : 'Transcript'}
        caption={
          peekWorker?.bead ? (
            // Surface the worker's captured bead beside the peek so its work
            // is one click away from the transcript.
            <Link
              to={`/beads?bead=${encodeURIComponent(peekWorker.bead.id)}`}
              className="text-fg-muted hover:text-accent focus-mark"
              title={`Open ${peekWorker.bead.id}`}
            >
              <span className="tnum">{peekWorker.bead.id}</span>
              <span>: {peekWorker.bead.title}</span>
            </Link>
          ) : (
            "Live transcript from the supervisor's session stream."
          )
        }
        widthClass="max-w-5xl"
      >
        <LiveSessionPeek
          sessionId={peekSessionId}
          stream={peekWorker ? isWorkerStreamable(peekWorker.session) : false}
          showBadge
          showCaption
        />
      </Modal>
    </section>
  );
}
