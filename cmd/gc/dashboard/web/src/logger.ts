type DashboardLogLevel = "debug" | "info" | "warn" | "error";

interface DashboardLogEntry {
  city: string;
  details?: unknown;
  level: DashboardLogLevel;
  message: string;
  scope: string;
  ts: string;
  url: string;
}

type ConsoleMethod = (...args: unknown[]) => void;

const originalConsole: Record<"debug" | "error" | "info" | "log" | "warn", ConsoleMethod> = {
  debug: console.debug.bind(console),
  error: console.error.bind(console),
  info: console.info.bind(console),
  log: console.log.bind(console),
  warn: console.warn.bind(console),
};

let installed = false;

export function installDashboardLogging(): void {
  if (installed || typeof window === "undefined") return;
  installed = true;

  mirrorConsole("debug", "debug");
  mirrorConsole("info", "info");
  mirrorConsole("warn", "warn");
  mirrorConsole("error", "error");
  mirrorConsole("log", "info");

  window.addEventListener("error", (event) => {
    logError("window", "Unhandled error", {
      colno: event.colno,
      error: event.error,
      filename: event.filename,
      lineno: event.lineno,
      message: event.message,
    });
  });

  window.addEventListener("unhandledrejection", (event) => {
    logError("window", "Unhandled promise rejection", { reason: event.reason });
  });
}

export function logDebug(scope: string, message: string, details?: unknown): void {
  emit("debug", scope, message, details);
}

export function logInfo(scope: string, message: string, details?: unknown): void {
  emit("info", scope, message, details);
}

export function logWarn(scope: string, message: string, details?: unknown): void {
  emit("warn", scope, message, details);
}

export function logError(scope: string, message: string, details?: unknown): void {
  emit("error", scope, message, details);
}

function emit(level: DashboardLogLevel, scope: string, message: string, details?: unknown): void {
  const entry = makeEntry(level, scope, message, details);
  originalConsole[level](`[dashboard][${scope}] ${message}`, safeSerialize(details));
  sendToServer(entry);
}

function mirrorConsole(method: keyof typeof originalConsole, level: DashboardLogLevel): void {
  const original = originalConsole[method];
  console[method] = (...args: unknown[]) => {
    original(...args);
    sendToServer(makeEntry(level, "console", extractMessage(args), args.length > 1 ? args.slice(1) : args[0]));
  };
}

function makeEntry(level: DashboardLogLevel, scope: string, message: string, details?: unknown): DashboardLogEntry {
  return {
    city: currentCityScope(),
    details: details === undefined ? undefined : safeSerialize(details),
    level,
    message,
    scope,
    ts: new Date().toISOString(),
    url: typeof window === "undefined" ? "" : window.location.href,
  };
}

function currentCityScope(): string {
  if (typeof window === "undefined") return "";
  return (new URLSearchParams(window.location.search).get("city") ?? "").trim();
}

function extractMessage(args: unknown[]): string {
  if (args.length === 0) return "console event";
  const [first] = args;
  if (typeof first === "string" && first.trim() !== "") return first;
  if (first instanceof Error) return first.message;
  return "console event";
}

function sendToServer(entry: DashboardLogEntry): void {
  const body = JSON.stringify(entry);
  if (typeof navigator !== "undefined" && typeof navigator.sendBeacon === "function") {
    const blob = new Blob([body], { type: "application/json" });
    if (navigator.sendBeacon("/__client-log", blob)) return;
  }
  void fetch("/__client-log", {
    body,
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    keepalive: true,
    method: "POST",
  }).catch(() => {
    // Ignore logging transport failures to avoid recursive error loops.
  });
}

function safeSerialize(value: unknown, depth = 0, seen = new WeakSet<object>()): unknown {
  if (value === undefined || value === null) return value ?? null;
  if (typeof value === "string") return value.length > 2000 ? `${value.slice(0, 1999)}…` : value;
  if (typeof value === "number" || typeof value === "boolean") return value;
  if (value instanceof Error) {
    return {
      message: value.message,
      name: value.name,
      stack: value.stack,
    };
  }
  if (typeof value === "function") {
    return `[function ${(value as Function).name || "anonymous"}]`;
  }
  if (depth >= 4) return "[max-depth]";
  if (Array.isArray(value)) {
    return value.slice(0, 20).map((item) => safeSerialize(item, depth + 1, seen));
  }
  if (typeof value === "object") {
    if (seen.has(value as object)) return "[circular]";
    seen.add(value as object);
    const out: Record<string, unknown> = {};
    for (const [key, child] of Object.entries(value as Record<string, unknown>).slice(0, 40)) {
      out[key] = safeSerialize(child, depth + 1, seen);
    }
    return out;
  }
  return String(value);
}
