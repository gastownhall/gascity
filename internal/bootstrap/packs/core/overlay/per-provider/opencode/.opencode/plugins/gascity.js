// Gas City hooks for OpenCode.
// Installed by gc into {workDir}/.opencode/plugins/gascity.js
//
// OpenCode's plugin API is ESM and hook-oriented:
//   - event() is side-effect-only (no prompt injection)
//   - experimental.chat.system.transform mutates output.system
//   - experimental.session.compacting → inject context before compaction
//
// Gas City uses:
//   - session.created / session.compacted → gc prime --hook (side effects such
//     as session-id persistence and poller bootstrap)
//   - experimental.session.compacting → gc handoff --auto "context cycle"
//     and inject the handoff confirmation into the compaction context
//   - experimental.chat.system.transform → inject gc prime --hook, queued
//     nudges, and unread mail into the system prompt for each turn

import { execFile } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);
const GC_OPENCODE_HOOK_VERSION = 6;
const GC_BIN = process.env.GC_BIN || "gc";
// Optional per-turn injection (queued nudges, unread mail) is best effort, so
// it gets its own fail-open budget rather than the 30s default used for
// lifecycle work such as prime and handoff. A stalled optional command drops
// its contribution for that turn instead of stalling the turn; the items are
// leased rather than consumed, so a later pass redelivers them.
//
// Sized from measurement, not from a latency target. Against a live city the
// floor for one gc invocation (startup, config load, store connect, target
// resolve) is ~800ms and `gc prime --hook` runs ~1.5s, so the budget has to
// clear a couple of seconds of legitimate work plus acknowledgement
// round-trips. 5s is roughly 6x the observed floor while staying 6x tighter
// than the lifecycle default.
//
// Override with GC_OPENCODE_INJECTION_TIMEOUT_MS for a slower store.
const INJECTION_TIMEOUT_MS = (() => {
  const override = Number(process.env.GC_OPENCODE_INJECTION_TIMEOUT_MS);
  return Number.isFinite(override) && override > 0 ? override : 5000;
})();
// GC_BIN is the explicit override. The fallback order matches Pi hooks so
// sibling providers resolve the same installed gc before developer-local bins.
const PATH_PREFIX =
  `/opt/homebrew/bin:/usr/local/bin:${process.env.HOME}/go/bin:${process.env.HOME}/.local/bin:`;

async function runCommand(
  directory,
  args,
  warnOnFailure,
  extraEnv = {},
  timeout = 30000,
) {
  const startedAt = Date.now();
  try {
    const { stdout, stderr } = await execFileAsync(GC_BIN, args, {
      cwd: directory,
      encoding: "utf-8",
      timeout,
      env: {
        ...process.env,
        ...extraEnv,
        PATH: PATH_PREFIX + (process.env.PATH || ""),
      },
    });
    logRunStderr(stderr);
    return stdout.trim();
  } catch (err) {
    if (warnOnFailure) {
      logRunFailure(args, directory, err, Date.now() - startedAt, timeout);
    }
    return "";
  }
}

// Optional injection: short budget, fails open, but still reports why. The
// failure used to be swallowed entirely, which left no way to tell which
// command stalled a turn.
async function runOptional(directory, ...args) {
  return runCommand(directory, args, true, {}, INJECTION_TIMEOUT_MS);
}

async function runWithWarning(directory, ...args) {
  return runCommand(directory, args, true);
}

// Node reports killed=true and signal=SIGTERM both when our own timeout fires
// and when something else terminates the child (a supervisor restart, say), so
// the error alone cannot tell them apart. Report the elapsed time against the
// budget that was in force: a failure at ~3000ms of a 3000ms budget is our
// timeout, one at 200ms is not.
function logRunFailure(args, directory, err, elapsedMs, timeoutMs) {
  try {
    const detail =
      (err && (err.code || err.signal || err.message)) || "unknown error";
    const timing =
      Number.isFinite(elapsedMs) && Number.isFinite(timeoutMs)
        ? ` after ${elapsedMs}ms (budget ${timeoutMs}ms)`
        : "";
    console.warn(
      "gascity opencode plugin:",
      `${GC_BIN} ${args.join(" ")}`,
      "cwd",
      directory,
      `failed${timing}:`,
      detail,
    );
  } catch {
    return;
  }
}

function logRunStderr(stderr) {
  try {
    const detail = String(stderr || "").trim();
    if (detail) {
      console.warn("gascity opencode plugin:", detail);
    }
  } catch {
    return;
  }
}

function unwrapData(result) {
  if (result && typeof result === "object" && "data" in result) {
    return result.data;
  }
  return result;
}

function safeSessionID(sessionID) {
  return String(sessionID || "").replace(/[^A-Za-z0-9_.-]/g, "_");
}

function sessionIDFromEvent(event) {
  return (
    event?.properties?.sessionID ||
    event?.properties?.info?.sessionID ||
    event?.properties?.message?.info?.sessionID ||
    ""
  );
}

function providerSessionEnv(sessionID) {
  sessionID = String(sessionID || "");
  const env = { GC_PROVIDER_SESSION_ID_REQUIRED: "opencode" };
  if (!sessionID) {
    return env;
  }
  env.GC_PROVIDER_SESSION_ID = sessionID;
  return env;
}

async function mirrorTranscript(directory, client, sessionID) {
  const exportDir = process.env.GC_OPENCODE_TRANSCRIPT_DIR || "";
  const safeID = safeSessionID(sessionID);
  if (!exportDir || !safeID || !client?.session) {
    return;
  }

  try {
    const [infoResult, messagesResult] = await Promise.all([
      client.session.get({ path: { id: sessionID } }),
      client.session.messages({ path: { id: sessionID } }),
    ]);
    const info = unwrapData(infoResult) || {};
    const messages = unwrapData(messagesResult) || [];
    if (!info.directory) {
      info.directory = directory;
    }
    await fs.mkdir(exportDir, { recursive: true });
    const dst = path.join(exportDir, `${safeID}.json`);
    const tmp = `${dst}.tmp`;
    await fs.writeFile(tmp, JSON.stringify({ info, messages }, null, 2));
    await fs.rename(tmp, dst);
  } catch {
    return;
  }
}

export default async function gascityPlugin({ directory, client }) {
  let cachedPrime = null;

  async function readPrime(force = false, extraEnv = {}) {
    if (force || cachedPrime === null) {
      cachedPrime = await runCommand(directory, ["prime", "--hook"], false, extraEnv);
    }
    return cachedPrime;
  }

  function prependText(existing, prefix) {
    return existing ? prefix + "\n\n" + existing : prefix;
  }

  async function buildPrefix() {
    const prime = await readPrime();
    const [nudges, mail] = await Promise.all([
      runOptional(directory, "nudge", "drain", "--inject"),
      runOptional(directory, "mail", "check", "--inject"),
    ]);
    return [prime, nudges, mail].filter(Boolean).join("\n\n");
  }

  return {
    event: async ({ event }) => {
      switch (event.type) {
        case "session.created":
        case "session.compacted":
          {
            const sessionID = sessionIDFromEvent(event);
            await readPrime(true, providerSessionEnv(sessionID));
            await mirrorTranscript(directory, client, sessionID);
          }
          return;
        case "session.idle":
        case "message.updated":
          await mirrorTranscript(directory, client, sessionIDFromEvent(event));
          return;
        default:
          return;
      }
    },

    "chat.message": async (_input, output) => {
      const prefix = await buildPrefix();
      if (prefix) {
        output.message.system = prependText(output.message.system, prefix);
      }
    },

    "experimental.chat.system.transform": async (_input, output) => {
      const prefix = await buildPrefix();
      if (prefix) {
        if (output.system[0]) {
          output.system[0] = prependText(output.system[0], prefix);
        } else {
          output.system.unshift(prefix);
        }
      }
    },

    "experimental.session.compacting": async (_input, output) => {
      const handoff = await runWithWarning(directory, "handoff", "--auto", "context cycle");
      if (!handoff) {
        return;
      }
      if (Array.isArray(output?.context)) {
        output.context.push(handoff);
        return;
      }
      try {
        console.warn(
          "gascity opencode plugin: compacting output.context is not an array; skipped handoff injection",
        );
      } catch {
        return;
      }
    },
  };
}
