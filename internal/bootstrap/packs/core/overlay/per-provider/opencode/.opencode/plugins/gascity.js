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
// Every gc call this plugin makes shares one timeout. Optional per-turn
// injection used to carry a shorter budget of its own, but that was justified
// only while it blocked the send acknowledgement; it no longer does, and the
// stalls it was hedging against were an unclosed child stdin rather than slow
// work.
const COMMAND_TIMEOUT_MS = 30000;
// GC_BIN is the explicit override. The fallback order matches Pi hooks so
// sibling providers resolve the same installed gc before developer-local bins.
const PATH_PREFIX =
  `/opt/homebrew/bin:/usr/local/bin:${process.env.HOME}/go/bin:${process.env.HOME}/.local/bin:`;

async function runCommand(directory, args, warnOnFailure, extraEnv = {}) {
  const startedAt = Date.now();
  try {
    const { stdout, stderr } = await execFileAsync(GC_BIN, args, {
      cwd: directory,
      encoding: "utf-8",
      timeout: COMMAND_TIMEOUT_MS,
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
      logRunFailure(args, directory, err, Date.now() - startedAt, COMMAND_TIMEOUT_MS);
    }
    return "";
  }
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
      runWithWarning(directory, "nudge", "drain", "--inject"),
      runWithWarning(directory, "mail", "check", "--inject"),
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
