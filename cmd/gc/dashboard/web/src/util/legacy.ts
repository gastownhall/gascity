import { relativeTime } from "./time";

export const ACTIVE_WINDOW_MS = 5 * 60 * 1000;
export const STALE_WINDOW_MS = 10 * 60 * 1000;
export const STUCK_WINDOW_MS = 30 * 60 * 1000;

export type ActivityColor = "green" | "yellow" | "red" | "unknown";

export interface ActivityInfo {
  display: string;
  colorClass: ActivityColor;
}

export function formatTimestamp(ts: string | undefined | null): string {
  if (!ts) return "—";
  const date = new Date(ts);
  if (Number.isNaN(date.getTime())) return "—";
  const now = new Date();
  const opts: Intl.DateTimeFormatOptions =
    date.getFullYear() === now.getFullYear()
      ? { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" }
      : { month: "short", day: "numeric", year: "numeric", hour: "numeric", minute: "2-digit" };
  return date.toLocaleString(undefined, opts);
}

export function calculateActivity(ts: string | undefined | null): ActivityInfo {
  if (!ts) return { display: "unknown", colorClass: "unknown" };
  const then = new Date(ts);
  if (Number.isNaN(then.getTime())) return { display: "unknown", colorClass: "unknown" };
  const delta = Math.max(0, Date.now() - then.getTime());
  const display = relativeTime(ts).replace(" ago", "");
  if (delta < ACTIVE_WINDOW_MS) return { display, colorClass: "green" };
  if (delta < STALE_WINDOW_MS) return { display, colorClass: "yellow" };
  return { display, colorClass: "red" };
}

// formatAgentAddress turns a structured agent address into a short label
// for display. Addresses are one of:
//   - "name"              → single-segment (bare agent)
//   - "rig/name"          → rig-qualified
//   - "rig/pool/name"     → pool-in-rig
// No role names are hardcoded; the formatting is purely structural.
export function formatAgentAddress(addr: string | undefined | null): string {
  if (!addr) return "—";
  const parts = addr.split("/").filter(Boolean);
  if (parts.length === 0) return "—";
  if (parts.length === 1) return parts[0]!;
  if (parts.length >= 3) return `${parts[parts.length - 1]} (${parts[0]}/${parts[1]})`;
  return `${parts[0]}/${parts[parts.length - 1]}`;
}

export function extractRig(actor: string | undefined | null): string {
  if (!actor || !actor.includes("/")) return "";
  return actor.split("/", 1)[0] ?? "";
}

export function eventCategory(eventType: string): string {
  if (eventType.startsWith("agent.") || eventType.startsWith("session.")) return "agent";
  if (eventType.startsWith("bead.") || eventType.startsWith("convoy.") || eventType.startsWith("order.")) {
    return "work";
  }
  if (eventType.startsWith("mail.")) return "comms";
  if (eventType.startsWith("request.result.") || eventType === "request.failed") return "system";
  return "system";
}

export function eventIcon(eventType: string): string {
  const icons: Record<string, string> = {
    "session.started": "▶",
    "session.ended": "■",
    "session.crashed": "☠",
    "session.suspended": "⏸",
    "session.woke": "▶",
    "agent.message": "💬",
    "agent.output": "📝",
    "agent.tool_call": "🛠",
    "agent.tool_result": "✅",
    "agent.error": "⚠",
    "bead.created": "📿",
    "bead.updated": "📝",
    "bead.closed": "✅",
    "convoy.created": "🚚",
    "convoy.closed": "✅",
    "mail.delivered": "📬",
    "mail.read": "📨",
    "request.failed": "❌",
  };
  if (eventType.startsWith("request.result.")) return "🔔";
  return icons[eventType] ?? "📋";
}

export function eventSummary(
  eventType: string,
  actor: string | undefined | null,
  subject: string | undefined | null,
  message: string | undefined | null,
): string {
  const shortActor = formatAgentAddress(actor);
  switch (eventType) {
    case "session.started":
      return `${formatAgentAddress(subject)} started`;
    case "session.ended":
      return `${formatAgentAddress(subject)} ended`;
    case "session.crashed":
      return `${formatAgentAddress(subject)} crashed`;
    case "session.suspended":
      return `${formatAgentAddress(subject)} suspended`;
    case "session.woke":
      return `${formatAgentAddress(subject)} woke`;
    case "bead.created":
      return `${shortActor} created bead ${subject ?? ""}`.trim();
    case "bead.updated":
      return `${shortActor} updated bead ${subject ?? ""}`.trim();
    case "bead.closed":
      return `${shortActor} closed bead ${subject ?? ""}`.trim();
    case "mail.delivered":
      return `${shortActor} delivered mail`;
    case "mail.read":
      return `${shortActor} read mail`;
    case "convoy.created":
      return `${shortActor} created convoy ${subject ?? ""}`.trim();
    case "convoy.closed":
      return `${shortActor} closed convoy ${subject ?? ""}`.trim();
    case "request.failed":
      return message ?? `${subject ?? "request"} failed`;
    default:
      if (eventType.startsWith("request.result."))
        return message ?? `${subject ?? "request"} succeeded`;
      return message ?? subject ?? eventType;
  }
}

export function truncate(text: string | undefined | null, max: number): string {
  if (!text) return "";
  if (text.length <= max) return text;
  return `${text.slice(0, max - 1)}…`;
}

export function beadPriority(priority: number | undefined | null): number {
  if (typeof priority !== "number" || Number.isNaN(priority) || priority <= 0) return 4;
  return priority;
}

export function priorityBadgeClass(priority: number | undefined | null): string {
  switch (beadPriority(priority)) {
    case 1:
      return "badge-red";
    case 2:
      return "badge-orange";
    case 3:
      return "badge-yellow";
    default:
      return "badge-muted";
  }
}

export function statusBadgeClass(status: string | undefined | null): string {
  switch ((status ?? "").toLowerCase()) {
    case "open":
    case "running":
    case "ready":
    case "working":
      return "badge-green";
    case "in_progress":
    case "pending":
    case "stale":
    case "warning":
      return "badge-yellow";
    case "closed":
    case "stopped":
      return "badge-muted";
    case "error":
    case "failed":
    case "stuck":
      return "badge-red";
    default:
      return "badge-blue";
  }
}
