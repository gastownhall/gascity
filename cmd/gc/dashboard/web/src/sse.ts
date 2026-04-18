import type {
  CityEventStreamEnvelope,
  HeartbeatEvent,
  SupervisorEventStreamEnvelope,
} from "./api";
import { reportUIError } from "./ui";

function supervisorBaseURL(): string {
  const meta = document.querySelector<HTMLMetaElement>('meta[name="supervisor-url"]');
  return (meta?.content ?? "").replace(/\/+$/, "");
}

export interface SSEHandle {
  close(): void;
}

export type HeartbeatMessage = {
  event: "heartbeat";
  id?: string;
  data: HeartbeatEvent;
};

export type CityEventMessage = {
  event: "event";
  id?: string;
  data: CityEventStreamEnvelope;
};

export type SupervisorEventMessage = {
  event: "tagged_event";
  id?: string;
  data: SupervisorEventStreamEnvelope;
};

export type DashboardEventMessage =
  | HeartbeatMessage
  | CityEventMessage
  | SupervisorEventMessage;

export interface AgentOutputMessage {
  id?: string;
  type: string;
  data: unknown;
}

export function connectEvents(onEvent: (msg: DashboardEventMessage) => void): SSEHandle {
  return connectTypedStream(
    `${supervisorBaseURL()}/v0/events/stream`,
    decodeSupervisorMessage,
    onEvent,
  );
}

export function connectCityEvents(city: string, onEvent: (msg: DashboardEventMessage) => void): SSEHandle {
  return connectTypedStream(
    `${supervisorBaseURL()}/v0/city/${encodeURIComponent(city)}/events/stream`,
    decodeCityMessage,
    onEvent,
  );
}

function connectTypedStream<T>(
  url: string,
  decode: (eventName: string, raw: string, lastEventID: string) => T,
  onEvent: (msg: T) => void,
): SSEHandle {
  const source = new EventSource(url, { withCredentials: false });
  for (const eventName of ["heartbeat", "event", "tagged_event"]) {
    source.addEventListener(eventName, (event) => {
      try {
        onEvent(decode(eventName, (event as MessageEvent).data, (event as MessageEvent).lastEventId));
      } catch (error) {
        reportUIError("Event stream decode failed", error);
      }
    });
  }
  source.onerror = () => {
    // EventSource handles reconnecting; this path is still surfaced for observability.
    // eslint-disable-next-line no-console
    console.debug("sse: reconnecting events stream");
  };
  return { close: () => source.close() };
}

function decodeSupervisorMessage(eventName: string, raw: string, lastEventID: string): DashboardEventMessage {
  if (eventName === "heartbeat") {
    return {
      event: "heartbeat",
      id: lastEventID || undefined,
      data: parseHeartbeat(raw),
    };
  }
  if (eventName !== "tagged_event") {
    throw new Error(`unexpected supervisor SSE event: ${eventName}`);
  }
  return {
    event: "tagged_event",
    id: lastEventID || undefined,
    data: parseSupervisorEnvelope(raw),
  };
}

function decodeCityMessage(eventName: string, raw: string, lastEventID: string): DashboardEventMessage {
  if (eventName === "heartbeat") {
    return {
      event: "heartbeat",
      id: lastEventID || undefined,
      data: parseHeartbeat(raw),
    };
  }
  if (eventName !== "event") {
    throw new Error(`unexpected city SSE event: ${eventName}`);
  }
  return {
    event: "event",
    id: lastEventID || undefined,
    data: parseCityEnvelope(raw),
  };
}

// connectAgentOutput opens the per-session agent-output stream for
// one session. Returns a handle so the caller can close it when the
// session panel is dismissed.
export function connectAgentOutput(
  city: string,
  sessionID: string,
  onEvent: (msg: AgentOutputMessage) => void,
): SSEHandle {
  const url = `${supervisorBaseURL()}/v0/city/${encodeURIComponent(city)}/session/${encodeURIComponent(sessionID)}/stream`;
  const source = new EventSource(url, { withCredentials: false });
  source.onmessage = (e) => {
    onEvent({ id: e.lastEventId || undefined, type: "message", data: parseUnknownJSON(e.data) });
  };
  for (const eventName of ["turn", "message", "activity", "pending", "heartbeat"]) {
    source.addEventListener(eventName, (event) => {
      onEvent({
        id: (event as MessageEvent).lastEventId || undefined,
        type: eventName,
        data: parseUnknownJSON((event as MessageEvent).data),
      });
    });
  }
  return { close: () => source.close() };
}

export function semanticEventType(msg: DashboardEventMessage): string {
  if (msg.event === "heartbeat") return "heartbeat";
  return msg.data.type;
}

function parseHeartbeat(raw: string): HeartbeatEvent {
  const value = parseJSONObject(raw);
  if (!isRecord(value) || typeof value.timestamp !== "string") {
    throw new Error("invalid heartbeat payload");
  }
  return { timestamp: value.timestamp };
}

function parseCityEnvelope(raw: string): CityEventStreamEnvelope {
  const value = parseJSONObject(raw);
  if (!isEventEnvelope(value)) {
    throw new Error("invalid city event payload");
  }
  return value;
}

function parseSupervisorEnvelope(raw: string): SupervisorEventStreamEnvelope {
  const value = parseJSONObject(raw);
  if (!isTaggedEventEnvelope(value)) {
    throw new Error("invalid supervisor event payload");
  }
  return value;
}

function parseUnknownJSON(raw: string): unknown {
  try {
    return JSON.parse(raw);
  } catch {
    return raw;
  }
}

function parseJSONObject(raw: string): unknown {
  return JSON.parse(raw);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function isEventEnvelope(value: unknown): value is CityEventStreamEnvelope {
  return isBaseEventEnvelope(value);
}

function isTaggedEventEnvelope(value: unknown): value is SupervisorEventStreamEnvelope {
  if (!isRecord(value) || !isBaseEventEnvelope(value)) return false;
  const record = value as Record<string, unknown>;
  return typeof record.city === "string";
}

function isBaseEventEnvelope(value: unknown): value is { actor: string; seq: number; ts: string; type: string } {
  return isRecord(value)
    && typeof value.actor === "string"
    && typeof value.seq === "number"
    && typeof value.ts === "string"
    && typeof value.type === "string";
}
