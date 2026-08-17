import type { TypedEventStreamEnvelope } from 'gas-city-dashboard-shared/gc-supervisor';

export type SupervisorEventSignal = 'attention' | 'watch' | 'event';

const ATTENTION_EVENT_TYPES = new Set<string>([
  'gc.store.maintenance.failed',
  'order.failed',
  'request.failed',
  'session.crashed',
  'session.stranded',
  'session.work_query_failed',
  'supervisor.shutdown_requested',
]);

const WATCH_EVENT_TYPES = new Set<string>([
  'events.rotated',
  'session.quarantined',
  'session.suspended',
  'supervisor.fs_pressure.skipped_tick',
]);

export function supervisorEventSignal(event: TypedEventStreamEnvelope): SupervisorEventSignal {
  if (ATTENTION_EVENT_TYPES.has(event.type)) return 'attention';
  if (WATCH_EVENT_TYPES.has(event.type)) return 'watch';
  return 'event';
}

export function supervisorEventDetail(event: TypedEventStreamEnvelope): string {
  return event.message ?? event.subject ?? event.type;
}
