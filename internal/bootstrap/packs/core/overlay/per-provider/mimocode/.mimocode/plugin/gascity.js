// Gas City hooks for MiMo Code.
// Installed by gc into {workDir}/.mimocode/plugin/gascity.js
//
// MiMo Code is an OpenCode fork and exposes the same ESM, hook-oriented
// plugin API:
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
//   - chat.message → on the first message of a root session, run one
//     gc prime --hook that carries the SessionStart markers and push its
//     output onto that message as one synthetic text part, so the
//     session-start context (beacon, step reminder, auto-handoff, unread-mail
//     snapshot) reaches the model once and stays until compaction. The role
//     prompt stays in system[0].
//
// The plugin is instantiated once per directory, not per session. The
// agent's root session, every subagent child session the task tool opens and
// any other session in the directory share this module's closure, so all
// per-turn state below is keyed by session id.

import { execFile } from "node:child_process";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);
const GC_MIMOCODE_HOOK_VERSION = 5;
const GC_BIN = process.env.GC_BIN || "gc";
// GC_BIN is the explicit override. The fallback order matches Pi hooks so
// sibling providers resolve the same installed gc before developer-local bins.
const PATH_PREFIX =
  `/opt/homebrew/bin:/usr/local/bin:${process.env.HOME}/go/bin:${process.env.HOME}/.local/bin:`;
// RECENT_USER_MESSAGES is how many user-message ids each session remembers
// to tell a new turn from a late update to an earlier message (see openTurn).
const RECENT_USER_MESSAGES = 64;

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
      "gascity mimocode plugin:",
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
      console.warn("gascity mimocode plugin:", detail);
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
  const env = { GC_PROVIDER_SESSION_ID_REQUIRED: "mimocode" };
  if (!sessionID) {
    return env;
  }
  env.GC_PROVIDER_SESSION_ID = sessionID;
  return env;
}

// sessionStartEnv is the environment of the one `gc prime --hook` that
// reports a session start. gc reads the marker pair as "this is the session's
// start": it adds the auto-handoff and the unread-mail snapshot, archives the
// handoff as it writes stdout, and leaves out the role prompt once the startup
// prompt was delivered. The pair therefore belongs to this one call and never
// to providerSessionEnv(), the compaction refresh or the persona refresh.
function sessionStartEnv(sessionID) {
  return {
    ...providerSessionEnv(sessionID),
    GC_MANAGED_SESSION_HOOK: "1",
    GC_HOOK_EVENT_NAME: "SessionStart",
  };
}

// Gas City's transcript discovery (sessionlog.DefaultMimoCodeSearchPaths)
// reads ~/.local/share/gascity/mimocode-transcripts, so mirroring defaults
// to that path; GC_MIMOCODE_TRANSCRIPT_DIR overrides it (test harnesses).
function defaultTranscriptDir() {
  const home = os.homedir() || "";
  if (!home) {
    return "";
  }
  return path.join(home, ".local", "share", "gascity", "mimocode-transcripts");
}

async function mirrorTranscript(directory, client, sessionID) {
  const exportDir =
    process.env.GC_MIMOCODE_TRANSCRIPT_DIR || defaultTranscriptDir();
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
  // once per user turn, so every tool call in a turn rebuilds the prefix.
  // `gc nudge drain --inject` is consumptive, so draining on every generation
  // emptied the queue several times per turn and the drained items landed in
  // whichever generation won the race; each run also opened store
  // connections (#5552, same shape as the OpenCode plugin).
  //
  // turns maps a session id to the turn its consumptive commands last ran
  // for and the text they returned; later generations of the same turn
  // repeat that text so every generation in a turn carries identical system
  // messages. Keying by session keeps a child session's user message from
  // reopening the parent's turn. Entries are never removed: one small record
  // per session, holding at most RECENT_USER_MESSAGES message ids, for the
  // life of the process is the accepted bound.
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
  // sessionStarts maps a session id to "armed" (a root session.created was
  // seen and no message has claimed it yet) or "spent" (its first message has
  // been seen). A session never armed, such as a child, a resumed session or
  // one whose created event arrived late, has no entry until its first
  // message records it as spent, and keeps today's per-turn delivery only.
  // Entries are never removed: one small record per session for the life of
  // the process, the same accepted bound as turns.
  const sessionStarts = new Map();

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
      state = {
        currentTurnID: "",
        drainedTurnID: null,
        volatile: Promise.resolve(""),
        userMessageIDs: new Set(),
      };
      turns.set(sessionID, state);
    }
    return state;
  }

  // openTurn records the user message that starts a session's turn. It is
  // driven by message.updated alone: OpenCode persists the user message and
  // publishes that event before it starts the turn's first generation.
  // OpenCode 1.18 calls the event hook while publishing; 1.14 delivers it on
  // a separate subscriber fiber, which relies on that delivery running ahead
  // of the first generation (the prompt loop awaits several steps before it
  // generates). Neither awaits the hook, so the event handler must record
  // the turn before its first await. chat.message does not open turns.
  //
  // Only a user message the session has not reported before opens a turn.
  // OpenCode also emits message.updated for an older user message when a
  // background fiber finishes that message's diff summary; the fiber is
  // forked and never joined, and a new prompt does not wait for the session
  // to go idle, so the update can land any number of turns later. Treating
  // it as current would rewind the turn and either withhold the new turn's
  // drain or drain a second time and change its system text mid-turn.
  // Message ids cannot tell the two apart by order: the 12 hex digits after
  // `msg_` are the low 48 bits of the creation time in 1/4096 ms, which wrap
  // every 2^36 ms (last on 2026-08-14), and the clock can step backwards, so
  // a new message can sort below the current turn's. Each session instead
  // remembers its most recent RECENT_USER_MESSAGES user-message ids; any
  // other id opens a turn. An update that lands after its id aged out of
  // that window opens one too and costs one extra drain, never a withheld
  // one.
  function openTurn(sessionID, messageID) {
    const state = turnState(sessionID);
    if (!state || !messageID) {
      return;
    }
    messageID = String(messageID);
    if (state.userMessageIDs.has(messageID)) {
      return;
    }
    state.userMessageIDs.add(messageID);
    if (state.userMessageIDs.size > RECENT_USER_MESSAGES) {
      // A Set iterates in insertion order, so the first id is the oldest.
      state.userMessageIDs.delete(state.userMessageIDs.values().next().value);
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
  // role opens the prompt and the bytes before MiMo Code's own system text are
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
  // The OpenCode request path MiMo Code inherits folds system[1..] into one
  // message only while system[0] is still its own header, so after
  // prependStableSystem ran this entry stays a separate trailing system
  // message; the prompt-cache prefix ends at the stable text instead of at
  // the prime.
  function appendVolatileSystem(system, volatile) {
    if (volatile) {
      system.push(volatile);
    }
  }

  // armSessionStart marks a root session whose first chat.message should carry
  // the SessionStart context. It reads only the created event, never the
  // client, so a payload without session info leaves the session unarmed, and
  // it runs before the event handler's first await because the host does not
  // wait for that handler.
  function armSessionStart(sessionID, event) {
    const info = event?.properties?.info;
    if (!info || typeof info !== "object" || info.parentID) {
      return;
    }
    if (typeof sessionID !== "string" || sessionID === "") {
      return;
    }
    if (!sessionStarts.has(sessionID)) {
      sessionStarts.set(sessionID, "armed");
    }
  }

  // deliverSessionStart runs in chat.message, which the host awaits before it
  // persists the user message, so it spawns gc at most once per session: in
  // the call that claims an armed session. Every other call returns without
  // I/O. The claim is recorded before the first await so a concurrent second
  // message cannot also claim it. Delivery is at-most-once: gc archives the
  // auto-handoff when it writes stdout, so nothing here may retry. Every
  // failure leaves the content to the per-turn path.
  async function deliverSessionStart(sessionID, output) {
    if (typeof sessionID !== "string" || sessionID === "") {
      return;
    }
    const armed = sessionStarts.get(sessionID) === "armed";
    sessionStarts.set(sessionID, "spent");
    if (!armed) {
      return;
    }
    // Check the delivery channel before gc runs, so nothing is consumed
    // without somewhere to put it.
    const messageID = output?.message?.id;
    if (!Array.isArray(output?.parts) || typeof messageID !== "string" || messageID === "") {
      console.warn(
        "gascity mimocode plugin:",
        "chat.message output has no parts array or message id; SessionStart context is left to the per-turn path",
      );
      return;
    }
    const payload = await runCommand(directory, ["prime", "--hook"], true, sessionStartEnv(sessionID));
    if (!payload) {
      return;
    }
    // The host assigned the user's part ids before this hook ran, and a part's
    // id is its storage key, so the part brings its own, derived from the
    // unique message id. "-" sorts below every id the host assigns, so the
    // model reads this context ahead of the prompt. The array is extended in
    // place: the host iterates the array it passed, and a replaced one is
    // never persisted.
    output.parts.push({
      id: "prt_-" + messageID.replace(/^msg_/, ""),
      sessionID,
      messageID,
      type: "text",
      text: "SessionStart hook additional context:\n" + payload,
      synthetic: true,
      metadata: { gascity: { hook: "SessionStart" } },
    });
  }

  return {
    event: async ({ event }) => {
      switch (event.type) {
        case "session.created":
        case "session.compacted":
          {
            const sessionID = sessionIDFromEvent(event);
            if (event.type === "session.created") {
              armSessionStart(sessionID, event);
            }
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

    // chat.message runs before the host persists the user message, so it is
    // where the SessionStart part joins that message's parts. It writes
    // neither output.message.system (the host persists it on the user message
    // and joins it into the tail of system[0] on every generation of that
    // turn, which would undo the stable-header split below) nor a turn: turns
    // open from message.updated. It never throws; the host awaits it.
    "chat.message": async (input, output) => {
      try {
        await deliverSessionStart(input?.sessionID, output);
      } catch (err) {
        try {
          console.warn("gascity mimocode plugin:", "chat.message SessionStart delivery failed:", err?.message || err);
        } catch {
          return;
        }
      }
    },

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
          "gascity mimocode plugin: compacting output.context is not an array; skipped handoff injection",
        );
      } catch {
        return;
      }
    },
  };
}
