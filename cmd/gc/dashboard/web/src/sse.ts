// EventSource wrappers with automatic Last-Event-ID resume.
//
// Two streams are consumed by the SPA today:
//
//   - `/v0/events/stream` — global supervisor event bus. Drives the
//     activity panel and invalidates other panels on state-changing
//     events (sessions, beads, mail).
//   - `/v0/city/{cityName}/session/{id}/stream` — per-session agent
//     output. Only opened when a session panel is focused.
//
// `EventSource` natively handles reconnection; it re-sends the last
// received `id` in the `Last-Event-ID` header, and the supervisor
// replays from that cursor. The Go proxy's hand-written reconnect
// loop becomes unnecessary.

function supervisorBaseURL(): string {
  const meta = document.querySelector<HTMLMetaElement>('meta[name="supervisor-url"]');
  return (meta?.content ?? "").replace(/\/+$/, "");
}

export interface SSEHandle {
  close(): void;
}

export interface EventMessage {
  id?: string;
  type: string;
  data: unknown;
}

const namedTypes = [
  "session.started", "session.ended", "session.crashed", "session.woke", "session.suspended",
  "agent.message", "agent.tool_call", "agent.tool_result", "agent.thinking", "agent.output",
  "agent.idle", "agent.error", "agent.completed",
  "bead.created", "bead.updated", "bead.closed",
  "mail.delivered", "mail.read",
  "convoy.created", "convoy.closed",
  "event", "heartbeat",
];

export function connectEvents(onEvent: (msg: EventMessage) => void): SSEHandle {
  return connectStream(`${supervisorBaseURL()}/v0/events/stream`, onEvent);
}

export function connectCityEvents(city: string, onEvent: (msg: EventMessage) => void): SSEHandle {
  return connectStream(`${supervisorBaseURL()}/v0/city/${encodeURIComponent(city)}/events/stream`, onEvent);
}

function connectStream(url: string, onEvent: (msg: EventMessage) => void): SSEHandle {
  const source = new EventSource(url, { withCredentials: false });

  // Supervisor emits named event types (e.g. `agent.message`,
  // `session.crashed`). A generic `message` handler catches unnamed
  // events; the typed events come through `addEventListener(type, …)`.
  source.onmessage = (e) => {
    onEvent({ id: e.lastEventId || undefined, type: "message", data: safeParse(e.data) });
  };
  for (const t of namedTypes) {
    source.addEventListener(t, (e: MessageEvent) => {
      onEvent({ id: e.lastEventId || undefined, type: t, data: safeParse(e.data) });
    });
  }

  source.onerror = () => {
    // EventSource auto-reconnects; only log transient errors. The
    // browser will reopen with Last-Event-ID and the supervisor
    // replays from the cursor.
    // eslint-disable-next-line no-console
    console.debug("sse: reconnecting events stream");
  };

  return { close: () => source.close() };
}

// connectAgentOutput opens the per-session agent-output stream for
// one session. Returns a handle so the caller can close it when the
// session panel is dismissed.
export function connectAgentOutput(
  city: string,
  sessionID: string,
  onEvent: (msg: EventMessage) => void,
): SSEHandle {
  const url = `${supervisorBaseURL()}/v0/city/${encodeURIComponent(city)}/session/${encodeURIComponent(sessionID)}/stream`;
  const source = new EventSource(url, { withCredentials: false });
  source.onmessage = (e) => {
    onEvent({ id: e.lastEventId || undefined, type: "message", data: safeParse(e.data) });
  };
  return { close: () => source.close() };
}

function safeParse(raw: string): unknown {
  try {
    return JSON.parse(raw);
  } catch {
    return raw;
  }
}
