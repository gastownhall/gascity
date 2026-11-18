// Gas City hooks for OpenCode.
// Installed by gc into {workDir}/.opencode/plugins/gascity.js
//
// OpenCode's plugin API is ESM and hook-oriented:
//   - event() is side-effect-only (no prompt injection)
//   - chat.message mutates a new user message's parts before it is saved
//   - experimental.chat.system.transform mutates output.system
//   - experimental.session.compacting → inject context before compaction
//
// Gas City uses:
//   - session.created / session.compacted → gc prime --hook (side effects such
//     as session-id persistence and poller bootstrap)
//   - experimental.session.compacting → gc handoff --auto "context cycle"
//     and inject the handoff confirmation into the compaction context
//   - experimental.chat.system.transform → prepend the cached gc prime --hook
//     to system[0], so the role stays at the head of a system prompt that is
//     identical on every generation
//   - chat.message → add the per-turn text (queued nudges with their clock
//     line, unread mail) to the user message that opens the turn, so it
//     becomes saved conversation history and the provider's prompt-cache
//     prefix survives across turns
//
// OpenCode instantiates one plugin per directory, not per session. The
// agent's root session, every subagent child session the task tool opens and
// any other session in the directory share this module's closure.

import { execFile } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);
const GC_OPENCODE_HOOK_VERSION = 9;
const GC_BIN = process.env.GC_BIN || "gc";
// GC_BIN is the explicit override. The fallback order matches Pi hooks so
// sibling providers resolve the same installed gc before developer-local bins.
const PATH_PREFIX =
  `/opt/homebrew/bin:/usr/local/bin:${process.env.HOME}/go/bin:${process.env.HOME}/.local/bin:`;

async function runCommand(directory, args, warnOnFailure, extraEnv = {}) {
  try {
    // execFile always gives the child a stdin pipe and never closes it, so a
    // gc subcommand that reads hook stdin waits for an EOF that never arrives
    // and is killed when the timeout expires. Close it immediately: these
    // calls send nothing on stdin. (`stdio` is not an execFile option — it is
    // honored by spawn and execFileSync, which is why the pi hook can pass
    // stdio: ["ignore", ...] instead.)
    const pending = execFileAsync(GC_BIN, args, {
      cwd: directory,
      encoding: "utf-8",
      timeout: 30000,
      env: {
        ...process.env,
        ...extraEnv,
        PATH: PATH_PREFIX + (process.env.PATH || ""),
      },
    });
    pending.child.stdin?.end();
    const { stdout, stderr } = await pending;
    logRunStderr(stderr);
    return stdout.trim();
  } catch (err) {
    if (warnOnFailure) {
      logRunFailure(args, directory, err);
    }
    return "";
  }
}

async function run(directory, ...args) {
  return runCommand(directory, args, false);
}

async function runWithWarning(directory, ...args) {
  return runCommand(directory, args, true);
}

function logRunFailure(args, directory, err) {
  try {
    const detail =
      (err && (err.code || err.signal || err.message)) || "unknown error";
    console.warn(
      "gascity opencode plugin:",
      `${GC_BIN} ${args.join(" ")}`,
      "cwd",
      directory,
      "failed:",
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

  // childSessions records sessions created with a parent (subagents). Their
  // lifecycle events must not refresh the shared prime or hand their id to
  // `gc prime --hook` as the provider resume key.
  const childSessions = new Set();

  async function readPrime(force = false, extraEnv = {}) {
    if (force || cachedPrime === null) {
      cachedPrime = await runCommand(directory, ["prime", "--hook"], false, extraEnv);
    }
    return cachedPrime;
  }

  function prependText(existing, prefix) {
    return existing ? prefix + "\n\n" + existing : prefix;
  }

  // readVolatile returns the per-turn text: `gc nudge drain --inject` emits a
  // `Current time: ...` line on every call even with an empty queue, and
  // unread mail changes as it arrives. `gc nudge drain` is consumptive.
  async function readVolatile() {
    const nudges = await run(directory, "nudge", "drain", "--inject");
    const mail = await run(directory, "mail", "check", "--inject");
    return [nudges, mail].filter(Boolean).join("\n\n");
  }

  // isChildSession reports whether a session was opened by another session.
  // session.created carries the session info; session.compacted carries only
  // the id, so it falls back to the sessions already recorded and then to
  // the client. A session that cannot be resolved is treated as a root
  // session so the prime refresh is never withheld from the agent's own.
  async function isChildSession(sessionID, event) {
    if (!sessionID) {
      return false;
    }
    if (childSessions.has(sessionID)) {
      return true;
    }
    let info = event?.properties?.info;
    if (!info && client?.session?.get) {
      try {
        info = unwrapData(await client.session.get({ path: { id: sessionID } }));
      } catch {
        info = null;
      }
    }
    if (info?.parentID) {
      childSessions.add(sessionID);
      return true;
    }
    return false;
  }

  // prependStableSystem keeps the cached prime at the head of system[0] so the
  // role opens the prompt and the bytes before OpenCode's own system text are
  // identical from one generation to the next.
  function prependStableSystem(system, prime) {
    if (!prime) {
      return;
    }
    if (system[0]) {
      system[0] = prependText(system[0], prime);
    } else {
      system.unshift(prime);
    }
  }

  // appendVolatilePart drains the per-turn text into the user message that
  // opens a turn, as a synthetic text part after the user's own parts.
  // OpenCode sends the system entries ahead of the conversation, so text that
  // changes every turn in the system prompt would sit ahead of the whole
  // history, and each turn's request would share only the system prompt with
  // the last one: a provider's prompt cache would miss on the entire
  // conversation once per turn. As a saved part it is history instead, and
  // every later request repeats it unchanged. OpenCode saves output.parts,
  // ordered by id, right after chat.message returns and sends synthetic
  // text parts to the model, so the part takes the last part's id plus a
  // suffix (ids only need a "prt" prefix) to sort right after it.
  async function appendVolatilePart(output) {
    const message = output?.message;
    const parts = output?.parts;
    const last = Array.isArray(parts) ? parts.at(-1) : undefined;
    // The drain is consumptive: with nowhere to put the text, leave the
    // nudges queued for the next turn.
    if (!message?.id || !last?.id) {
      return;
    }
    const volatile = await readVolatile();
    if (!volatile) {
      return;
    }
    parts.push({
      id: `${last.id}gc`,
      sessionID: message.sessionID,
      messageID: message.id,
      type: "text",
      text: volatile,
      synthetic: true,
    });
  }

  return {
    event: async ({ event }) => {
      switch (event.type) {
        case "session.created":
        case "session.compacted":
          {
            const sessionID = sessionIDFromEvent(event);
            if (!(await isChildSession(sessionID, event))) {
              await readPrime(true, providerSessionEnv(sessionID));
            }
            await mirrorTranscript(directory, client, sessionID);
          }
          return;
        case "message.updated":
        case "session.idle":
          await mirrorTranscript(directory, client, sessionIDFromEvent(event));
          return;
        default:
          return;
      }
    },

    // chat.message fires once per user prompt, before OpenCode saves it. It
    // must not write output.message.system: OpenCode joins that field into
    // system[0] on every generation of the turn.
    "chat.message": async (_input, output) => {
      await appendVolatilePart(output);
    },

    // experimental.chat.system.transform fires on every model generation
    // (each tool call in a turn), so it adds only the stable prime.
    "experimental.chat.system.transform": async (_input, output) => {
      prependStableSystem(output.system, await readPrime());
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
