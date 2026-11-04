// Gas City hooks for Oh My Pi (OMP).
// Installed by gc into {workDir}/.omp/hooks/gc-hook.ts
// Managed by `gc hooks install`; put custom OMP hooks in separate extension
// files so upgrades can replace this file safely.
//
// Events:
//   session_start       → gc prime --hook (capture OMP session id); its output is parked
//                         and injected by the next before_agent_start
//   session_compact     → gc prime --hook (reload after compaction)
//   before_agent_start  → inject the parked prime output + queued nudges + unread mail

import { execFileSync } from "node:child_process";
import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";

const GC_OMP_HOOK_VERSION = 3;
const PATH_PREFIX =
  `/opt/homebrew/bin:/usr/local/bin:${process.env.HOME}/go/bin:${process.env.HOME}/.local/bin:`;

// `gc prime --hook` writes the session's startup context to stdout. OMP's
// session_start cannot inject into the model's context, so the SessionStart
// output is parked here and drained into the system prompt by the next
// before_agent_start. Dropping it would consume auto-handoff mail (gc archives
// it once the write succeeds) without ever delivering it.
let pendingPrimeContext = "";

function run(args: string[], cwd?: string, extraEnv: Record<string, string> = {}): string {
  try {
    return execFileSync("gc", args, {
      cwd: cwd || process.cwd(),
      encoding: "utf-8",
      timeout: 30000,
      stdio: ["ignore", "pipe", "inherit"],
      env: {
        ...process.env,
        ...extraEnv,
        PATH: PATH_PREFIX + (process.env.PATH || ""),
      },
    }).trim();
  } catch (err) {
    logRunFailure(args, cwd, err);
    return "";
  }
}

function logRunFailure(args: string[], cwd: string | undefined, err: unknown): void {
  try {
    const maybeError = err as { code?: string; signal?: string; message?: string } | undefined;
    const detail = maybeError?.code || maybeError?.signal || maybeError?.message || "unknown error";
    console.error(
      "gc-hooks run:",
      `gc ${args.join(" ")}`,
      "cwd",
      cwd || process.cwd(),
      "failed:",
      detail,
    );
  } catch {
    // Keep OMP hooks non-fatal even if stderr is unavailable.
  }
}

function providerSessionEnv(ctx: { sessionManager?: { getSessionId?: () => string } }): Record<string, string> {
  const sessionID = ctx.sessionManager?.getSessionId?.() || "";
  const env: Record<string, string> = { GC_PROVIDER_SESSION_ID_REQUIRED: "omp" };
  if (!sessionID) {
    return env;
  }
  env.GC_PROVIDER_SESSION_ID = sessionID;
  return env;
}

// gc identifies a managed hook invocation by GC_MANAGED_SESSION_HOOK and routes
// on GC_HOOK_EVENT_NAME. Without both, `gc prime --hook` cannot tell that gc
// already delivered the startup prompt at launch and re-emits it. Only
// session_start may pass them: the markers must never ride a non-SessionStart
// invocation whose output reaches the model. They go on the child env of one
// run() call and are never assigned onto process.env.
function hookEnv(ctx: { sessionManager?: { getSessionId?: () => string } }, eventName: string): Record<string, string> {
  return {
    ...providerSessionEnv(ctx),
    GC_MANAGED_SESSION_HOOK: "1",
    GC_HOOK_EVENT_NAME: eventName,
  };
}

function appendSystemPrompt(systemPrompt: string[], additions: string[]): string[] {
  const extras = additions.filter(Boolean);
  if (extras.length === 0) {
    return systemPrompt;
  }
  return [...systemPrompt, extras.join("\n\n")];
}

export default function gascityOmpExtension(pi: ExtensionAPI) {
  pi.on("session_start", (_event, ctx) => {
    pendingPrimeContext = run(["prime", "--hook"], ctx.cwd, hookEnv(ctx, "SessionStart"));
  });

  pi.on("session_compact", (_event, ctx) => {
    run(["prime", "--hook"], ctx.cwd, providerSessionEnv(ctx));
  });

  pi.on("before_agent_start", (event, ctx) => {
    const prime = pendingPrimeContext;
    pendingPrimeContext = "";
    const nudges = run(["nudge", "drain", "--inject"], ctx.cwd);
    const mail = run(["mail", "check", "--inject"], ctx.cwd);
    const systemPrompt = appendSystemPrompt(event.systemPrompt, [prime, nudges, mail]);
    if (systemPrompt !== event.systemPrompt) {
      return { systemPrompt };
    }
  });
}
