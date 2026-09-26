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
//     nudges, and unread mail into the system prompt for each turn. The
//     cached prime is prepended to system[0] so the role stays at the head;
//     the per-turn text (nudges with their clock line, unread mail) is
//     appended as a trailing system entry so it lands after every stable
//     entry and the provider's prompt-cache prefix survives across turns.
//
// OpenCode instantiates one plugin per directory, not per session. The
// agent's root session, every subagent child session the task tool opens and
// any other session in the directory share this module's closure, so all
// per-turn state below is keyed by session id.

import { execFile } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);
const GC_OPENCODE_HOOK_VERSION = 8;
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

  // experimental.chat.system.transform fires once per model generation, not
  // once per user turn: OpenCode triggers it from Agent.generate, so every
  // tool call in a turn rebuilds the prefix. `gc nudge drain --inject` is
  // consumptive, so draining on every generation emptied the queue several
  // times per turn and the drained items landed in whichever generation won
  // the race; each run also opened store connections (#5552).
  //
  // turns maps a session id to the turn its consumptive commands last ran
  // for and the text they returned; later generations of the same turn
  // repeat that text so every generation in a turn carries identical system
  // messages. Keying by session keeps a child session's user message from
  // reopening the parent's turn. Entries are never removed: one small record
  // per session for the life of the process is the accepted bound.
  //
  // This scopes the cache, not the queue. Every session in the process runs
  // gc with the same identity, so a drain from a child or human session in
  // this directory still consumes the agent's nudge queue; only the text
  // each session repeats for its own turn is kept apart here.
  const turns = new Map();
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
  // unread mail changes as it arrives. Neither may sit ahead of stable text.
  async function readVolatile() {
    const nudges = await run(directory, "nudge", "drain", "--inject");
    const mail = await run(directory, "mail", "check", "--inject");
    return [nudges, mail].filter(Boolean).join("\n\n");
  }

  function turnState(sessionID) {
    sessionID = String(sessionID || "");
    if (!sessionID) {
      return null;
    }
    let state = turns.get(sessionID);
    if (!state) {
      state = { currentTurnID: "", drainedTurnID: null, volatile: Promise.resolve("") };
      turns.set(sessionID, state);
    }
    return state;
  }

  // openTurn records the user message that starts a session's turn. It is
  // driven by message.updated alone: OpenCode persists the user message, and
  // publishes that event to plugins inline, before it starts the turn's first
  // generation, so the turn is known by the time the transform runs as long
  // as the event handler records it before its first await. No chat.message
  // hook is registered for this (see the transform below).
  //
  // Only a newer message advances the turn. OpenCode also emits
  // message.updated for an older user message when a background fiber
  // finishes that message's diff summary; the fiber is forked and never
  // joined, and a new prompt does not wait for the session to go idle, so
  // the update can land any number of turns later. Treating it as current
  // would rewind the turn and either withhold the new turn's drain or drain
  // a second time and change its system text mid-turn. Message ids are
  // `msg_` + a fixed-width hex encoding of the creation time and a
  // per-millisecond counter, so within a process a plain string comparison
  // follows creation order: an id at or below the current turn is an update
  // to an earlier message, not a new turn. If the clock steps backwards, a
  // genuinely new message can sort below the current turn; that turn then
  // repeats the previous turn's volatile text and its nudges wait for the
  // next turn, which is preferable to consuming them twice.
  function openTurn(sessionID, messageID) {
    const state = turnState(sessionID);
    if (!state || !messageID) {
      return;
    }
    messageID = String(messageID);
    if (state.currentTurnID && messageID <= state.currentTurnID) {
      return;
    }
    state.currentTurnID = messageID;
  }

  // readTurnVolatile runs the consumptive commands once per turn per session.
  // With no session id or no known turn — events not delivered, or a payload
  // without the fields above — it drains every time, so nudges are never
  // silently withheld.
  async function readTurnVolatile(sessionID) {
    const state = turnState(sessionID);
    if (!state || !state.currentTurnID) {
      return readVolatile();
    }
    if (state.drainedTurnID !== state.currentTurnID) {
      // Claim the turn before awaiting so concurrent generations of the same
      // turn share this pending drain instead of each reaching gc.
      state.drainedTurnID = state.currentTurnID;
      state.volatile = readVolatile();
    }
    return state.volatile;
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

  // appendVolatileSystem places the per-turn text after every stable entry.
  // OpenCode folds system[1..] into one message only while system[0] is still
  // its own header, so after prependStableSystem ran this entry stays a
  // separate trailing system message; the prompt-cache prefix ends at the
  // stable text instead of at the prime.
  function appendVolatileSystem(system, volatile) {
    if (volatile) {
      system.push(volatile);
    }
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
          {
            // A new user message opens a turn for its own session.
            const info = event?.properties?.info;
            if (info && info.role === "user" && info.id) {
              openTurn(info.sessionID || sessionIDFromEvent(event), info.id);
            }
          }
          await mirrorTranscript(directory, client, sessionIDFromEvent(event));
          return;
        case "session.idle":
          await mirrorTranscript(directory, client, sessionIDFromEvent(event));
          return;
        default:
          return;
      }
    },

    // No chat.message hook: OpenCode persists output.message.system on the
    // user message and joins it into the tail of system[0] on every
    // generation of that turn, so anything written there re-enters the
    // stable header and undoes the split below. It is not needed to open
    // the turn either; message.updated precedes the first generation.
    "experimental.chat.system.transform": async (input, output) => {
      const prime = await readPrime();
      const volatile = await readTurnVolatile(input?.sessionID);
      prependStableSystem(output.system, prime);
      appendVolatileSystem(output.system, volatile);
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
