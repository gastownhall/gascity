// Gas City hooks for Pi Coding Agent.
// Installed by gc into {workDir}/.pi/extensions/gc-hooks.js
// Managed by `gc hooks install`; put custom Pi hooks in separate extension
// files so upgrades can replace this file safely.
//
// Pi 0.70+ extension API uses a factory function and pi.on(...)
// subscriptions. Keep this file as .js for existing Gas City provider args
// and auto-discovery paths.
//
// Events:
//   session_start    → gc prime --hook (load context side effects)
//   session_compact  → gc prime --hook + gc handoff --auto "context cycle"
//   before_agent_start → gc hook --inject + queued nudges + unread mail
//   agent_end        → idle re-drive: drain queued nudges into a new turn

const { execFileSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");

const GC_PI_HOOK_VERSION = 10;
const PATH_PREFIX =
  `/opt/homebrew/bin:/usr/local/bin:${process.env.HOME}/go/bin:${process.env.HOME}/.local/bin:`;
let mirrorTempCounter = 0;
// `gc prime --hook` writes the session's startup context to stdout. pi has no
// session_start hook surface that can inject into the model's context, so the
// SessionStart output is parked here and drained into the system prompt by the
// next before_agent_start. Dropping it would consume auto-handoff mail (gc
// archives it once the write succeeds) without ever delivering it.
let pendingPrimeContext = "";

function run(args, cwd, extraEnv = {}) {
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

function logRunFailure(args, cwd, err) {
  try {
    const detail =
      (err && (err.code || err.signal || err.message)) || "unknown error";
    console.error(
      "gc-hooks run:",
      `gc ${args.join(" ")}`,
      "cwd",
      cwd || process.cwd(),
      "failed:",
      detail,
    );
  } catch {
    // Keep Pi hooks non-fatal even if stderr is unavailable.
  }
}

function safeSessionID(sessionID) {
  // Keep this filename contract in sync with safePiSessionID in
  // internal/sessionlog/pi_reader.go.
  return String(sessionID || "").replace(/[^A-Za-z0-9_.-]/g, "_");
}

function mirrorTempPath(dst) {
  mirrorTempCounter += 1;
  return `${dst}.tmp.${process.pid}.${Date.now()}.${mirrorTempCounter}`;
}

function sessionManagerHeader(manager, cwd) {
  try {
    const header = manager.getHeader && manager.getHeader();
    if (header && typeof header === "object") {
      return { ...header, cwd: header.cwd || cwd };
    }
  } catch {
    // Continue to the fallback header below.
  }
  return {
    type: "session",
    version: 3,
    id: manager.getSessionId && manager.getSessionId(),
    timestamp: new Date().toISOString(),
    cwd,
  };
}

function providerSessionEnv(ctx) {
  const sessionID = ctx?.sessionManager?.getSessionId?.() || "";
  const env = { GC_PROVIDER_SESSION_ID_REQUIRED: "pi" };
  if (!sessionID) {
    return env;
  }
  env.GC_PROVIDER_SESSION_ID = String(sessionID);
  return env;
}

// gc identifies a managed hook invocation by GC_MANAGED_SESSION_HOOK and
// routes on GC_HOOK_EVENT_NAME, exactly as the claude settings.json hook does.
// Without both, `gc prime --hook` cannot tell that gc already delivered the
// startup prompt inline and re-emits the entire prompt on top of it.
function hookEnv(ctx, eventName) {
  return {
    ...providerSessionEnv(ctx),
    GC_MANAGED_SESSION_HOOK: "1",
    GC_HOOK_EVENT_NAME: eventName,
  };
}

function mirrorTranscript(ctx) {
  const exportDir = process.env.GC_PI_TRANSCRIPT_DIR || "";
  const manager = ctx && ctx.sessionManager;
  if (!exportDir || !manager) {
    return;
  }
  let tmp = "";
  try {
    const cwd = (manager.getCwd && manager.getCwd()) || ctx.cwd || process.cwd();
    const sessionID = safeSessionID(manager.getSessionId && manager.getSessionId());
    if (!sessionID) {
      return;
    }
    fs.mkdirSync(exportDir, { recursive: true });
    const dst = path.join(exportDir, `${sessionID}.jsonl`);
    tmp = mirrorTempPath(dst);
    const sessionFile = manager.getSessionFile && manager.getSessionFile();
    if (sessionFile && fs.existsSync(sessionFile)) {
      fs.copyFileSync(sessionFile, tmp);
      fs.renameSync(tmp, dst);
      return;
    }
    const header = sessionManagerHeader(manager, cwd);
    const entries = manager.getEntries ? manager.getEntries() : [];
    const lines = [header, ...entries].map((entry) => JSON.stringify(entry));
    fs.writeFileSync(tmp, `${lines.join("\n")}\n`, "utf8");
    fs.renameSync(tmp, dst);
  } catch (err) {
    if (tmp) {
      try {
        fs.rmSync(tmp, { force: true });
      } catch {
        // Keep the original mirror error as the useful diagnostic.
      }
    }
    try {
      console.error(
        "gc-hooks mirrorTranscript:",
        err && err.message ? err.message : String(err),
      );
    } catch {
      // Keep Pi hooks non-fatal even if stderr is unavailable.
    }
  }
}

// Idle re-drive. A managed pi session whose model yields its turn without
// finishing (no tool call, a question to nobody) sits at an idle TUI prompt.
// Queued nudges normally reach pi only in before_agent_start, which needs a
// turn, which needs input — circular while idle. The tmux quiescence poller
// cannot bridge the gap either: an idle pi TUI repaints continuously, so the
// pane's window_activity never ages past the 3 s quiescence window (measured
// on pi 0.84: three samples over 80 s, every one under 1 s old). So the hook,
// which knows exactly when the agent is idle, drains the queue itself: once
// at agent_end and then every IDLE_DRAIN_INTERVAL_MS until the next
// agent_start. Drained nudges arrive as a user message, which starts a turn.
//
// Bounded by construction: `gc nudge drain` claims what it prints, so a
// delivered reminder cannot be re-delivered, and an empty queue prints
// nothing. Only armed when gc exported a session identity, so a human running
// pi in a managed worktree does not get an idle gc subprocess loop.
const IDLE_DRAIN_INTERVAL_MS = 15000;
let idleDrainTimer = null;

function managedSessionIdentity() {
  return process.env.GC_ALIAS || process.env.GC_SESSION_ID || "";
}

// Like run(), but an exit status of 1 is the documented "nothing queued"
// answer from `gc nudge drain`, not a failure, so it is not logged.
function runQuiet(args, cwd) {
  try {
    return execFileSync("gc", args, {
      cwd: cwd || process.cwd(),
      encoding: "utf-8",
      timeout: 30000,
      stdio: ["ignore", "pipe", "inherit"],
      env: { ...process.env, PATH: PATH_PREFIX + (process.env.PATH || "") },
    }).trim();
  } catch (err) {
    if (!err || err.status !== 1) {
      logRunFailure(args, cwd, err);
    }
    return "";
  }
}

function stopIdleDrain() {
  if (idleDrainTimer) {
    clearInterval(idleDrainTimer);
    idleDrainTimer = null;
  }
}

function drainIdleNudges(pi, ctx) {
  const nudges = runQuiet(["nudge", "drain"], ctx.cwd);
  if (!nudges) {
    return false;
  }
  stopIdleDrain();
  pi.sendUserMessage(nudges);
  return true;
}

function startIdleDrain(pi, ctx) {
  stopIdleDrain();
  if (!managedSessionIdentity()) {
    return;
  }
  if (drainIdleNudges(pi, ctx)) {
    return;
  }
  idleDrainTimer = setInterval(() => {
    drainIdleNudges(pi, ctx);
  }, IDLE_DRAIN_INTERVAL_MS);
  if (typeof idleDrainTimer.unref === "function") {
    idleDrainTimer.unref();
  }
}

function appendSystemPrompt(systemPrompt, additions) {
  const extras = additions.filter(Boolean);
  if (extras.length === 0) {
    return systemPrompt;
  }
  return [systemPrompt, ...extras].filter(Boolean).join("\n\n");
}

module.exports = function gascityPiExtension(pi) {
  pi.on("session_start", (_event, ctx) => {
    pendingPrimeContext = run(["prime", "--hook"], ctx.cwd, hookEnv(ctx, "SessionStart"));
    mirrorTranscript(ctx);
  });

  pi.on("session_compact", (_event, ctx) => {
    run(["prime", "--hook"], ctx.cwd, hookEnv(ctx, "PreCompact"));
    run(["handoff", "--auto", "context cycle"], ctx.cwd);
    mirrorTranscript(ctx);
  });

  pi.on("before_agent_start", (event, ctx) => {
    const prime = pendingPrimeContext;
    pendingPrimeContext = "";
    const work = run(["hook", "--inject"], ctx.cwd);
    const nudges = run(["nudge", "drain", "--inject"], ctx.cwd);
    const mail = run(["mail", "check", "--inject"], ctx.cwd);
    const systemPrompt = appendSystemPrompt(event.systemPrompt, [prime, work, nudges, mail]);
    if (systemPrompt !== event.systemPrompt) {
      return { systemPrompt };
    }
  });

  pi.on("message_end", (_event, ctx) => {
    mirrorTranscript(ctx);
  });

  pi.on("agent_start", () => {
    stopIdleDrain();
  });

  pi.on("agent_end", (_event, ctx) => {
    mirrorTranscript(ctx);
    startIdleDrain(pi, ctx);
  });

  pi.on("session_shutdown", (_event, ctx) => {
    stopIdleDrain();
    mirrorTranscript(ctx);
  });
};
