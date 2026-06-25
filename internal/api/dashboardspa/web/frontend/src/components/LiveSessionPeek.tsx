import type { DashboardSession } from 'gas-city-dashboard-shared';
import type { AgentResponse } from 'gas-city-dashboard-shared/gc-supervisor';
import { formatRelative } from '../hooks/time';
import {
  useSessionStream,
  type SessionStreamConnState,
  type SessionStreamProgress,
} from '../hooks/useSessionStream';
import { formatHumanSize } from '../lib/format';
import { SessionPeekContent } from './SessionPeek';
import { StatusBadge, type StatusTone } from './StatusBadge';

// Composes the session transcript snapshot with a live SSE tail. Owns the
// "fetch snapshot, then stream turns, show a connection badge" pattern so
// every surface that views a session (agent drilldown, agents-list peek
// modal, formula run node) shares ONE live-peek body instead of each
// re-implementing the hook + badge + render wiring.
//
// Pure composition over useSessionStream — no fetch/cadence decisions of its
// own; the caller decides whether to stream (`stream`) and which chrome to
// show (`showBadge`, `showCaption`).

interface LiveSessionPeekProps {
  /** Session to peek. Null renders nothing (idle). */
  sessionId: string | null;
  /** Open the live SSE tail. False = one-shot snapshot only. */
  stream: boolean;
  /**
   * Show the connection badge (live/connecting/offline/snapshot). Default
   * true. The badge reflects `streamState`, so it reads "snapshot" when
   * `stream` is false — informative, not noise.
   */
  showBadge?: boolean;
  /** Show the turn-count / captured-at caption line. Default false. */
  showCaption?: boolean;
}

export function LiveSessionPeek({
  sessionId,
  stream,
  showBadge = true,
  showCaption = false,
}: LiveSessionPeekProps) {
  const sessionState = useSessionStream(sessionId, stream);
  const result = sessionState.status === 'ready' ? sessionState.result : null;
  const loading = sessionState.status === 'loading';
  const error = sessionState.status === 'failed' ? sessionState.error : null;
  const badge = streamBadge(sessionState.stream);

  const captionParts: string[] = [];
  if (showCaption && result) {
    captionParts.push(`${result.turns.length} turn(s)`);
    captionParts.push(formatHumanSize(result.total_chars, 'chars'));
    captionParts.push(`captured ${formatRelative(result.captured_at, Date.now())}`);
  }

  return (
    <div className="space-y-4">
      {showBadge && (
        <div className="flex justify-end">
          <StatusBadge
            tone={badge.tone}
            label={badge.label}
            title={`Session stream: ${sessionState.stream.status}`}
            className="text-label uppercase tracking-wider"
          />
        </div>
      )}
      <SessionPeekContent
        loading={loading}
        error={error}
        result={result}
        {...(captionParts.length > 0 ? { caption: captionParts.join(' · ') } : {})}
      />
    </div>
  );
}

/** Maps the SSE connection state to a status badge tone + label. */
export function streamBadge(stream: SessionStreamProgress | SessionStreamConnState): {
  tone: StatusTone;
  label: string;
} {
  const status = typeof stream === 'string' ? stream : stream.status;
  switch (status) {
    case 'open':
      return { tone: 'ok', label: 'live' };
    case 'connecting':
      return { tone: 'warn', label: 'connecting' };
    case 'closed':
      return { tone: 'stuck', label: 'offline' };
    case 'degraded':
      return { tone: 'warn', label: 'degraded' };
    case 'idle':
      return { tone: 'neutral', label: 'snapshot' };
  }
}

/**
 * Whether a session is worth opening a live stream for. A streamed dead
 * session would hold a connection that never emits a turn and show a
 * perpetual "connecting" badge, so gate on the running signal. Mirrors the
 * "running" predicate (isRunningAgent) in routes/Agents.tsx so a
 * session the rest of the UI shows as running also streams: process
 * `running`, gc state `active`, OR gc state `running`.
 */
export function isSessionStreamable(session: DashboardSession | null): boolean {
  if (session === null) return false;
  return session.running === true || session.state === 'active' || session.state === 'running';
}

/**
 * Agent-shaped equivalent of `isSessionStreamable` (gascity-dashboard-ay6).
 * An agent is streamable ONLY WHEN it has a bound session AND the running
 * signal is present (process `running` === true, gc state `active`, or gc
 * state `running`). The session check is a hard prerequisite — `running`
 * on an orphan agent does NOT make it streamable because there's nothing
 * to stream from. Mirrors the isRunningAgent `running` predicate on the
 * agent shape so the peek-modal gate stays consistent with what the rest
 * of the Agents view labels as running.
 */
export function isAgentStreamable(agent: AgentResponse | null): boolean {
  if (agent === null) return false;
  if (!agent.session) return false;
  return agent.running === true || agent.state === 'active' || agent.state === 'running';
}
