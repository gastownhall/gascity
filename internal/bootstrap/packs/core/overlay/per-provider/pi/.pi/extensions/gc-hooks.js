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

const { execFileSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");

const GC_PI_HOOK_VERSION = 5;
const PATH_PREFIX =
  `/opt/homebrew/bin:/usr/local/bin:${process.env.HOME}/go/bin:${process.env.HOME}/.local/bin:`;
let mirrorTempCounter = 0;

function run(args, cwd, extraEnv = {}) {
  try {
    return execFileSync("gc", args, {
      cwd: cwd || process.cwd(),
      encoding: "utf-8",
      timeout: 30000,
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
  if (!sessionID) {
    return {};
  }
  return { GC_PROVIDER_SESSION_ID: String(sessionID) };
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

function appendSystemPrompt(systemPrompt, additions) {
  const extras = additions.filter(Boolean);
  if (extras.length === 0) {
    return systemPrompt;
  }
  return [systemPrompt, ...extras].filter(Boolean).join("\n\n");
}

module.exports = function gascityPiExtension(pi) {
  pi.on("session_start", (_event, ctx) => {
    run(["prime", "--hook"], ctx.cwd, providerSessionEnv(ctx));
    mirrorTranscript(ctx);
  });

  pi.on("session_compact", (_event, ctx) => {
    run(["prime", "--hook"], ctx.cwd, providerSessionEnv(ctx));
    run(["handoff", "--auto", "context cycle"], ctx.cwd);
    mirrorTranscript(ctx);
  });

  pi.on("before_agent_start", (event, ctx) => {
    const work = run(["hook", "--inject"], ctx.cwd);
    const nudges = run(["nudge", "drain", "--inject"], ctx.cwd);
    const mail = run(["mail", "check", "--inject"], ctx.cwd);
    const systemPrompt = appendSystemPrompt(event.systemPrompt, [work, nudges, mail]);
    if (systemPrompt !== event.systemPrompt) {
      return { systemPrompt };
    }
  });

  pi.on("message_end", (_event, ctx) => {
    mirrorTranscript(ctx);
  });

  pi.on("agent_end", (_event, ctx) => {
    mirrorTranscript(ctx);
  });

  pi.on("session_shutdown", (_event, ctx) => {
    mirrorTranscript(ctx);
  });
};
