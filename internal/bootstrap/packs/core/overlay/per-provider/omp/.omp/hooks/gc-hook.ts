// Gas City hooks for Oh My Pi (OMP).
// Installed by gc into {workDir}/.omp/hooks/gc-hook.ts
// Managed by `gc hooks install`; put custom OMP hooks in separate extension
// files so upgrades can replace this file safely.
//
// Events:
//   session_start       → gc prime --hook (load context side effects and capture OMP session id)
//   session_compact     → gc prime --hook (reload after compaction)
//   before_agent_start  → inject queued nudges + unread mail

import { execFileSync } from "node:child_process";
import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";

const GC_OMP_HOOK_VERSION = 2;
const PATH_PREFIX =
  `/opt/homebrew/bin:/usr/local/bin:${process.env.HOME}/go/bin:${process.env.HOME}/.local/bin:`;

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

function appendSystemPrompt(systemPrompt: string[], additions: string[]): string[] {
  const extras = additions.filter(Boolean);
  if (extras.length === 0) {
    return systemPrompt;
  }
  return [...systemPrompt, extras.join("\n\n")];
}

export default function gascityOmpExtension(pi: ExtensionAPI) {
  pi.on("session_start", (_event, ctx) => {
    run(["prime", "--hook"], ctx.cwd, providerSessionEnv(ctx));
  });

  pi.on("session_compact", (_event, ctx) => {
    run(["prime", "--hook"], ctx.cwd, providerSessionEnv(ctx));
  });

  pi.on("before_agent_start", (event, ctx) => {
    const nudges = run(["nudge", "drain", "--inject"], ctx.cwd);
    const mail = run(["mail", "check", "--inject"], ctx.cwd);
    const systemPrompt = appendSystemPrompt(event.systemPrompt, [nudges, mail]);
    if (systemPrompt !== event.systemPrompt) {
      return { systemPrompt };
    }
  });
}
