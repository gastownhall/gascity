import type { GcEventConnState } from '../hooks/useGcEvents';
import { StatusBadge, type StatusTone } from './StatusBadge';

// Live-connection indicator for views that subscribe to the supervisor city
// event stream via useGcEventRefresh. Renders a small StatusBadge surfacing whether
// the SSE stream is healthy. Operator-readable wording in the badge
// (live / connecting / offline); the raw machine state goes in the
// hover title for parity with the rest of the StatusBadge usage.
//
// Originally inlined in /agents (gascity-dashboard-iew); extracted as
// /runs became the second SSE consumer (gascity-dashboard-bqn).
// State is sourced directly from useGcEventRefresh — keep them linked so
// a new connection state added to the hook produces a compile error here.

export type SseState = GcEventConnState;

export function SseIndicator({ state }: { state: GcEventConnState }) {
  const tone: StatusTone =
    state === 'open' ? 'ok' : state === 'connecting' || state === 'degraded' ? 'warn' : 'stuck';
  const label =
    state === 'open'
      ? 'live'
      : state === 'connecting'
        ? 'connecting'
        : state === 'degraded'
          ? 'degraded'
          : 'offline';
  return <StatusBadge tone={tone} label={label} title={`SSE stream: ${state}`} className="w-28" />;
}
