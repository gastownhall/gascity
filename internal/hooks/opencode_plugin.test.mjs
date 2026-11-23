// Behavioral tests for the embedded OpenCode and MiMo Code plugins.
//
// Run directly with `node --test internal/hooks/opencode_plugin.test.mjs`;
// TestProviderPluginNodeSuite (plugin_node_test.go) runs the same file under
// `go test ./internal/hooks` whenever node is on PATH.
//
// The plugins resolve gc through GC_BIN at module load, so each suite points
// GC_BIN at a fake gc script before importing the plugin. The fake answers
// `prime --hook` with a fixed role prompt and `nudge drain --inject` with a
// clock line that changes on every call, mirroring the real command, which
// reads its hook input to EOF and then emits `Current time: ...` even when
// the queue is empty.

import { after, before, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const ROLE = "ROLE PROMPT: you are the configured agent.";
const AGENT_PROMPT = "PROVIDER SYSTEM PROMPT";
const ENVIRONMENT = "AGENTS.md instructions\n\nskills";
const HEADER = `${AGENT_PROMPT}\n\n${ENVIRONMENT}`;
const MAIL = "You have unread mail.";
const CLOCK_PREFIX = "Current time: tick-";
// What the fake gc prints for the one SessionStart-marked prime: beacon and
// handoff text with no persona, as gc does once the startup prompt is flagged
// delivered.
const SESSION_START = "SESSION START: beacon, step reminder, handoff.";
const SESSION_START_LABEL = "SessionStart hook additional context:\n";

// The suite deadline. The fake drain reads its stdin to EOF, so a plugin that
// leaves the child's stdin open stalls every drain until the plugin's 30s
// exec timeout; this budget fails the suite well before that adds up.
const SUITE_TIMEOUT_MS = 20000;

// Resolved from import.meta.url rather than import.meta.dirname (Node 20.11+):
// the PATH Bazel pins for tests can resolve an older system node.
const overlayDir = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
  "bootstrap",
  "packs",
  "core",
  "overlay",
  "per-provider",
);

const plugins = [
  {
    name: "opencode",
    source: path.join(overlayDir, "opencode", ".opencode", "plugins", "gascity.js"),
  },
  {
    name: "mimocode",
    source: path.join(overlayDir, "mimocode", ".mimocode", "plugin", "gascity.js"),
  },
];

// Fake gc: state lives in files under the temp dir so successive drains
// observe each other and the tests can reset between cases. Every
// `nudge drain` call appends a line to the drain log, which drainCount
// counts; the clock counter only numbers the clock lines, because two
// overlapping drains can both read it before either writes it back. Every
// `prime --hook` call appends the GC_PROVIDER_SESSION_ID it was given to the
// prime log, and GC_FAKE_PRIME_STAMP marks the output the way the real
// command bakes the current time into its beacon, so a test can tell a
// refreshed prime from the cached one.
//
// Two more logs serve the SessionStart tests. Every gc invocation of any kind
// appends its argv to the spawn log, and every `prime --hook` call appends
// `session|GC_MANAGED_SESSION_HOOK|GC_HOOK_EVENT_NAME` to the prime-call log.
// The fake honors gc's gate: a call carrying both markers prints
// SESSION_START with no persona (GC_FAKE_SESSIONSTART overrides the payload,
// an empty value prints nothing, GC_FAKE_SESSIONSTART_FAIL makes it exit 3),
// and a marker-free call prints the persona.
function writeFakeGc(dir) {
  const counter = path.join(dir, "drain-count");
  const drainLog = path.join(dir, "drain-log");
  const primeLog = path.join(dir, "prime-log");
  const primeCallLog = path.join(dir, "prime-call-log");
  const spawnLog = path.join(dir, "spawn-log");
  const script = `#!/bin/sh
printf '%s\\n' "$*" >> "${spawnLog}"
case "$1 $2" in
  "prime --hook")
    printf '%s\\n' "\${GC_PROVIDER_SESSION_ID:-<unset>}" >> "${primeLog}"
    printf '%s|%s|%s\\n' "\${GC_PROVIDER_SESSION_ID:-}" "\${GC_MANAGED_SESSION_HOOK:-}" "\${GC_HOOK_EVENT_NAME:-}" >> "${primeCallLog}"
    if [ "\${GC_MANAGED_SESSION_HOOK:-}" = "1" ] && [ "\${GC_HOOK_EVENT_NAME:-}" = "SessionStart" ]; then
      if [ -n "\${GC_FAKE_SESSIONSTART_FAIL:-}" ]; then exit 3; fi
      printf '%s' "\${GC_FAKE_SESSIONSTART-${SESSION_START}}"
      exit 0
    fi
    printf '%s' '${ROLE}'
    if [ -n "\${GC_FAKE_PRIME_STAMP:-}" ]; then printf ' (stamp %s)' "\${GC_FAKE_PRIME_STAMP}"; fi
    ;;
  "nudge drain")
    # Read the hook input to EOF first, as the real command does: a plugin
    # that leaves the child's stdin open blocks here (#5562).
    while IFS= read -r line; do :; done
    printf 'drain\\n' >> "${drainLog}"
    if [ "\${GC_INJECT_CLOCK:-}" = "0" ]; then exit 0; fi
    n=0
    if [ -f "${counter}" ]; then n=$(cat "${counter}"); fi
    n=$((n + 1))
    printf '%s' "$n" > "${counter}"
    printf '%s%s (epoch %s)' '${CLOCK_PREFIX}' "$n" "$n"
    ;;
  "mail check")
    if [ -n "\${GC_FAKE_MAIL:-}" ]; then printf '%s' "\${GC_FAKE_MAIL}"; fi
    ;;
  *)
    ;;
esac
`;
  const bin = path.join(dir, "gc");
  fs.writeFileSync(bin, script, { mode: 0o755 });
  return { bin, counter, drainLog, primeLog, primeCallLog, spawnLog };
}

function readLines(file) {
  if (!fs.existsSync(file)) {
    return [];
  }
  return fs.readFileSync(file, "utf-8").split("\n").filter(Boolean);
}

function readPrimeLog(fake) {
  return readLines(fake.primeLog);
}

function drainCount(fake) {
  return readLines(fake.drainLog).length;
}

function resetDrains(fake) {
  fs.rmSync(fake.counter, { force: true });
  fs.rmSync(fake.drainLog, { force: true });
}

function userMessageEvent(sessionID, messageID) {
  return {
    event: {
      type: "message.updated",
      properties: {
        sessionID,
        info: { id: messageID, sessionID, role: "user", time: { created: 1 } },
      },
    },
  };
}

function sessionEvent(type, sessionID, info) {
  const properties = { sessionID };
  if (info) {
    properties.info = { id: sessionID, ...info };
  }
  return { event: { type, properties } };
}

function stagePlugin(dir, plugin) {
  const stage = path.join(dir, plugin.name);
  fs.mkdirSync(stage, { recursive: true });
  // Write the bytes rather than copyFileSync: that carries over the source's
  // mode, and Bazel's remote runfiles are read-only, so staging the same
  // plugin a second time could not overwrite the first copy.
  fs.writeFileSync(path.join(stage, "gascity.js"), fs.readFileSync(plugin.source));
  // OpenCode loads workdir plugins as ES modules; the module-type package.json
  // gives the staged copy the same semantics outside a package.
  fs.writeFileSync(path.join(stage, "package.json"), '{"type":"module"}');
  return path.join(stage, "gascity.js");
}

function countOccurrences(haystack, needle) {
  return haystack.split(needle).length - 1;
}

function spawnLines() {
  return readLines(fake.spawnLog);
}

// primeCalls lists every `prime --hook` the fake saw as
// { session, managed, event }, in call order. Tests assert on it as a multiset
// (primeCallKeys): the persona refresh and the SessionStart prime are
// independent, so no ordering between them is part of the contract.
function primeCalls() {
  return readLines(fake.primeCallLog).map((line) => {
    const [session, managed, event] = line.split("|");
    return { session, managed, event };
  });
}

function primeCallKeys(calls) {
  return calls.map((c) => `${c.session}|${c.managed}|${c.event}`).sort();
}

function sessionStartPrimes() {
  return primeCalls().filter((c) => c.managed === "1" && c.event === "SessionStart");
}

function resetSessionStartLogs() {
  fs.rmSync(fake.primeCallLog, { force: true });
  fs.rmSync(fake.spawnLog, { force: true });
  fs.rmSync(fake.primeLog, { force: true });
}

// captureWarnings records console.warn calls made while fn runs: the plugin
// reports every failure path through console.warn.
async function captureWarnings(fn) {
  const original = console.warn;
  const calls = [];
  console.warn = (...args) => {
    calls.push(args);
  };
  try {
    await fn();
  } finally {
    console.warn = original;
  }
  return calls;
}

// The host's user-message parts carry ascending ids. OpenCode v1 ids are hex
// after the prefix and v2 ids lead with g; both must sort after the plugin's.
const HOST_PART_IDS = ["prt_0000000001aaaaaaaaaaaaaaaa", "prt_g0000000002bbbbbbbbbbbbbbb"];
const PART_KEYS = new Set(["id", "sessionID", "messageID", "type", "text", "synthetic", "metadata"]);

// assertHostAcceptsPart is the strict shape check both hosts effectively make:
// a prt-prefixed id, matching session and message ids, a text part with
// non-empty text, and no top-level key the part schema does not know. The host
// only logs an invalid part and still saves it, so the test must pin the shape.
function assertHostAcceptsPart(part, sessionID, messageID) {
  assert.equal(typeof part.id, "string");
  assert.ok(part.id.startsWith("prt"), `part id ${part.id} lacks the prt prefix`);
  assert.equal(part.sessionID, sessionID);
  assert.equal(part.messageID, messageID);
  assert.equal(part.type, "text");
  assert.ok(typeof part.text === "string" && part.text.length > 0, "part text is empty");
  for (const key of Object.keys(part)) {
    assert.ok(PART_KEYS.has(key), `unknown top-level part key ${key}`);
  }
}

// createUserMessage is the host double for one user message. It assigns the
// host's ids to the user's parts first, awaits the plugin's chat.message hook
// with { message, parts } the way both hosts do, then iterates the SAME array
// it passed: an in-place push persists and a reassignment of output.parts does
// not. The model view orders by id with plain string comparison, and the TUI
// view drops synthetic parts.
async function createUserMessage(hooks, sessionID, messageID, text = "do the work") {
  const parts = HOST_PART_IDS.map((id, i) => ({
    id,
    sessionID,
    messageID,
    type: "text",
    text: `${text} ${i}`,
  }));
  const message = { id: messageID, sessionID, role: "user", time: { created: 1 }, agent: "build" };
  const output = { message, parts };
  const hook = hooks["chat.message"];
  if (hook) {
    await hook({ sessionID, agent: "build", messageID }, output);
  }
  const persisted = [...parts];
  assert.equal(new Set(persisted.map((p) => p.id)).size, persisted.length, "part ids are not unique");
  for (const part of persisted) {
    assertHostAcceptsPart(part, sessionID, messageID);
  }
  const model = [...persisted].sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
  const tui = persisted.filter((p) => !p.synthetic);
  return { parts, output, message, persisted, model, tui, injected: persisted.filter((p) => p.synthetic) };
}

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "gc-plugin-test-"));
const fake = writeFakeGc(tmp);
process.env.GC_BIN = fake.bin;
// Both plugins mirror session transcripts through the client, and MiMo Code
// mirrors by default into the user's Gas City data directory, so keep every
// mirror under the suite's temp dir.
const transcriptDir = path.join(tmp, "transcripts");
process.env.GC_OPENCODE_TRANSCRIPT_DIR = transcriptDir;
process.env.GC_MIMOCODE_TRANSCRIPT_DIR = transcriptDir;

after(() => {
  fs.rmSync(tmp, { recursive: true, force: true });
});

for (const plugin of plugins) {
  describe(`${plugin.name} plugin experimental.chat.system.transform`, { timeout: SUITE_TIMEOUT_MS }, () => {
    let hooks;

    before(async () => {
      const staged = stagePlugin(tmp, plugin);
      const { default: gascityPlugin } = await import(pathToFileURL(staged).href);
      hooks = await gascityPlugin({ directory: tmp, client: {} });
    });

    beforeEach(() => {
      resetDrains(fake);
      delete process.env.GC_FAKE_MAIL;
    });

    async function transform(system, sessionID = "ses_test") {
      const output = { system };
      const input = { model: {} };
      if (sessionID !== null) {
        input.sessionID = sessionID;
      }
      await hooks["experimental.chat.system.transform"](input, output);
      return output.system;
    }

    it("keeps system[0] byte-identical across turns while the clock line moves", async () => {
      const first = await transform([HEADER]);
      const second = await transform([HEADER]);

      assert.equal(first[0], second[0], "stable system[0] changed between turns");
      assert.ok(first[0].startsWith(ROLE), "role prompt must open system[0]");
      assert.ok(first[0].endsWith(HEADER), "provider system text must follow the role");
      assert.equal(countOccurrences(first[0], CLOCK_PREFIX), 0, "clock line leaked into system[0]");

      assert.equal(first.length, 2, `expected [stable, volatile], got ${JSON.stringify(first)}`);
      assert.equal(first[1], `${CLOCK_PREFIX}1 (epoch 1)`);
      assert.equal(second[1], `${CLOCK_PREFIX}2 (epoch 2)`);
    });

    it("places every volatile line after all stable system text", async () => {
      process.env.GC_FAKE_MAIL = MAIL;
      const system = await transform([HEADER]);

      assert.equal(system.length, 2);
      const stableEnd = system[0].length;
      const joined = system.join("\n");
      assert.ok(joined.indexOf(CLOCK_PREFIX) > stableEnd, "clock line precedes stable text");
      assert.ok(joined.indexOf(MAIL) > stableEnd, "mail precedes stable text");
      assert.equal(system[1], `${CLOCK_PREFIX}1 (epoch 1)\n\n${MAIL}`);
    });

    it("injects the role prompt exactly once", async () => {
      process.env.GC_FAKE_MAIL = MAIL;
      const system = await transform([HEADER]);
      assert.equal(countOccurrences(system.join("\n"), ROLE), 1);
    });

    it("adds no trailing entry when nothing volatile was returned", async () => {
      // GC_INJECT_CLOCK=0 silences the real clock line; the fake honors it the
      // same way, so with an empty queue and no mail the drain prints nothing.
      process.env.GC_INJECT_CLOCK = "0";
      try {
        const system = await transform([HEADER]);
        assert.deepEqual(system, [`${ROLE}\n\n${HEADER}`]);
      } finally {
        delete process.env.GC_INJECT_CLOCK;
      }
    });

    it("seeds an empty system list with the role first and the clock last", async () => {
      const system = await transform([]);
      assert.deepEqual(system, [ROLE, `${CLOCK_PREFIX}1 (epoch 1)`]);
    });

    it("leaves the user message's system field alone in chat.message", async () => {
      // OpenCode persists output.message.system on the user message and joins
      // it into the tail of system[0] on every generation of that turn, so any
      // per-turn text written here re-enters the stable header and defeats the
      // transform's split. The hook may exist, but it must not write the field.
      process.env.GC_FAKE_MAIL = MAIL;
      const output = { message: { system: undefined }, parts: [] };
      await hooks["chat.message"]?.({ sessionID: "ses_test" }, output);
      assert.equal(output.message.system, undefined);
    });

    // Reproduces LLMRequestPrep.prepare (opencode session/llm/request.ts):
    // system[0] is the join of the agent prompt, the environment text and the
    // persisted user.system; the transform runs; then system[1..] is folded
    // into one entry only while system[0] still equals the pre-hook header.
    async function generate(message, sessionID = "ses_test") {
      const system = [[AGENT_PROMPT, ENVIRONMENT, message.system].filter(Boolean).join("\n")];
      const header = system[0];
      await hooks["experimental.chat.system.transform"]({ sessionID, model: {} }, { system });
      if (system.length > 2 && system[0] === header) {
        const rest = system.slice(1);
        system.length = 0;
        system.push(header, rest.join("\n"));
      }
      return system;
    }

    // One user turn: chat.message fires once when the message is created,
    // then every generation in the turn (the first call and each tool-call
    // continuation) rebuilds the system list from the persisted message.
    async function turn() {
      const message = { system: undefined };
      await hooks["chat.message"]?.({ sessionID: "ses_test" }, { message, parts: [] });
      return [await generate(message), await generate(message)];
    }

    it("keeps system[0] identical across turns when chat.message runs first", async () => {
      process.env.GC_FAKE_MAIL = MAIL;
      const [turnOneFirst, turnOneSecond] = await turn();
      const [turnTwoFirst, turnTwoSecond] = await turn();

      for (const system of [turnOneFirst, turnOneSecond, turnTwoFirst, turnTwoSecond]) {
        assert.equal(system[0], turnOneFirst[0], "system[0] drifted between generations");
        assert.equal(system[0], `${ROLE}\n\n${AGENT_PROMPT}\n${ENVIRONMENT}`);
        assert.equal(countOccurrences(system.join("\n"), ROLE), 1, "role prompt repeated");
        assert.equal(countOccurrences(system[0], CLOCK_PREFIX), 0, "clock line inside system[0]");
      }
      assert.equal(turnOneFirst[1], `${CLOCK_PREFIX}1 (epoch 1)\n\n${MAIL}`);
      assert.equal(turnTwoSecond[1], `${CLOCK_PREFIX}4 (epoch 4)\n\n${MAIL}`);
    });

    // The plugin is instantiated once per directory, so the sessions below all
    // share one closure; every assertion about "no cross-talk" is an assertion
    // about state that used to be a single pair of module-level variables.

    it("drains once per turn per session and repeats the turn's volatile text", async () => {
      process.env.GC_FAKE_MAIL = MAIL;
      await hooks.event(userMessageEvent("ses_a", "msg_a1"));
      await hooks.event(userMessageEvent("ses_b", "msg_b1"));

      const a1 = await transform([HEADER], "ses_a");
      const b1 = await transform([HEADER], "ses_b");
      const a2 = await transform([HEADER], "ses_a");

      assert.equal(drainCount(fake), 2, "each session drains its own turn exactly once");
      assert.equal(a1[1], `${CLOCK_PREFIX}1 (epoch 1)\n\n${MAIL}`, "A drained first");
      assert.equal(b1[1], `${CLOCK_PREFIX}2 (epoch 2)\n\n${MAIL}`, "B drained second, not A's text");
      assert.deepEqual(a2, a1, "a later generation of A's turn repeats A's system messages verbatim");

      // A new user message in A opens a new turn there and leaves B alone.
      await hooks.event(userMessageEvent("ses_a", "msg_a2"));
      const a3 = await transform([HEADER], "ses_a");
      const b2 = await transform([HEADER], "ses_b");
      assert.equal(a3[1], `${CLOCK_PREFIX}3 (epoch 3)\n\n${MAIL}`);
      assert.deepEqual(b2, b1, "B's turn is untouched by A's new turn");
      assert.equal(drainCount(fake), 3);
    });

    it("keeps a child session's user message from reopening the parent's turn", async () => {
      await hooks.event(userMessageEvent("ses_parent", "msg_p1"));
      const p1 = await transform([HEADER], "ses_parent");
      assert.equal(drainCount(fake), 1);

      // The task tool creates a child session and submits a user message to
      // it; before per-session state that message rewrote the shared turn id
      // and the parent's next generation drained again.
      await hooks.event(userMessageEvent("ses_child", "msg_c1"));
      const p2 = await transform([HEADER], "ses_parent");
      assert.equal(drainCount(fake), 1, "child user message must not drain for the parent");
      assert.deepEqual(p2, p1);

      const c1 = await transform([HEADER], "ses_child");
      assert.equal(drainCount(fake), 2, "the child drains its own turn once");
      assert.equal(c1[1], `${CLOCK_PREFIX}2 (epoch 2)`);
      const c2 = await transform([HEADER], "ses_child");
      assert.deepEqual(c2, c1);
      assert.equal(drainCount(fake), 2);
    });

    it("registers a chat.message hook that opens no turn", async () => {
      // OpenCode persists the user message (publishing message.updated) and
      // only then starts the turn's first generation, so message.updated alone
      // opens the turn. The chat.message hook exists for the one SessionStart
      // part; it must neither write output.message.system, which OpenCode
      // folds back into system[0] every generation, nor open a turn.
      assert.equal(typeof hooks["chat.message"], "function");
      await hooks["chat.message"]({ sessionID: "ses_order" }, { message: { id: "msg_o0" }, parts: [] });

      // OpenCode invokes the event hook as `void hook.event(...)` and does not
      // wait for it; the turn must be recorded before the first await.
      const pending = hooks.event(userMessageEvent("ses_order", "msg_o1"));
      const first = await transform([HEADER], "ses_order");
      const second = await transform([HEADER], "ses_order");
      await pending;
      assert.equal(drainCount(fake), 1, "turn was not open when the first generation ran");
      assert.deepEqual(second, first);

      // The same user message updated again (the same id) is the same turn.
      await hooks.event(userMessageEvent("ses_order", "msg_o1"));
      const third = await transform([HEADER], "ses_order");
      assert.equal(drainCount(fake), 1);
      assert.deepEqual(third, first);
    });

    it("keeps every generation of a turn on identical system messages under the fold", async () => {
      // The generate() reproduction above, with the user message persisted
      // the way OpenCode does it: message.updated before the first generation.
      process.env.GC_FAKE_MAIL = MAIL;
      async function realTurn(messageID) {
        const message = { system: undefined };
        void hooks.event(userMessageEvent("ses_fold", messageID));
        return [await generate(message, "ses_fold"), await generate(message, "ses_fold")];
      }
      const [turnOneFirst, turnOneSecond] = await realTurn("msg_t1");
      const [turnTwoFirst, turnTwoSecond] = await realTurn("msg_t2");

      for (const system of [turnOneFirst, turnOneSecond, turnTwoFirst, turnTwoSecond]) {
        assert.equal(system[0], `${ROLE}\n\n${AGENT_PROMPT}\n${ENVIRONMENT}`);
        assert.equal(system.length, 2);
      }
      assert.deepEqual(turnOneSecond, turnOneFirst, "second generation of turn one differs");
      assert.deepEqual(turnTwoSecond, turnTwoFirst, "second generation of turn two differs");
      assert.equal(turnOneFirst[1], `${CLOCK_PREFIX}1 (epoch 1)\n\n${MAIL}`);
      assert.equal(turnTwoFirst[1], `${CLOCK_PREFIX}2 (epoch 2)\n\n${MAIL}`);
      assert.equal(drainCount(fake), 2);
    });

    // OpenCode also emits message.updated for an OLDER user message when a
    // background fiber finishes its diff summary (session/summary.ts
    // summarize → updateMessage(target.info), forked from
    // session/processor.ts after the assistant step), which can land after
    // a newer user message opened the next turn. Such an update must not
    // rewind the current turn.

    it("ignores a delayed update to the previous user message before the new turn drained", async () => {
      process.env.GC_FAKE_MAIL = MAIL;
      await hooks.event(userMessageEvent("ses_late", "msg_l1"));
      const first = await transform([HEADER], "ses_late");
      assert.equal(drainCount(fake), 1);

      await hooks.event(userMessageEvent("ses_late", "msg_l2"));
      // The summary update for msg_l1 arrives after msg_l2 opened turn two.
      await hooks.event(userMessageEvent("ses_late", "msg_l1"));
      const second = await transform([HEADER], "ses_late");
      assert.equal(drainCount(fake), 2, "turn two never drained: the late update rewound to turn one");
      assert.equal(second[1], `${CLOCK_PREFIX}2 (epoch 2)\n\n${MAIL}`);
      assert.notDeepEqual(second, first);
    });

    it("ignores a delayed update to the previous user message after the new turn drained", async () => {
      await hooks.event(userMessageEvent("ses_late2", "msg_m1"));
      await transform([HEADER], "ses_late2");
      await hooks.event(userMessageEvent("ses_late2", "msg_m2"));
      const first = await transform([HEADER], "ses_late2");
      assert.equal(drainCount(fake), 2);

      await hooks.event(userMessageEvent("ses_late2", "msg_m1"));
      const second = await transform([HEADER], "ses_late2");
      assert.equal(drainCount(fake), 2, "late update caused another consumptive drain");
      assert.deepEqual(second, first, "system text changed mid-turn");

      // A genuinely new message still advances the turn.
      await hooks.event(userMessageEvent("ses_late2", "msg_m3"));
      const third = await transform([HEADER], "ses_late2");
      assert.equal(drainCount(fake), 3);
      assert.equal(third[1], `${CLOCK_PREFIX}3 (epoch 3)`);
    });

    // The summary fiber is forked and never joined, and promptAsync does not
    // wait for the session to go idle, so the late update can be for a
    // message more than one turn back.

    it("ignores a delayed update to a user message two turns back before the new turn drained", async () => {
      await hooks.event(userMessageEvent("ses_far", "msg_n1"));
      await transform([HEADER], "ses_far");
      await hooks.event(userMessageEvent("ses_far", "msg_n2"));
      await hooks.event(userMessageEvent("ses_far", "msg_n3"));
      await hooks.event(userMessageEvent("ses_far", "msg_n1"));
      const third = await transform([HEADER], "ses_far");
      assert.equal(drainCount(fake), 2, "turn three never drained: the late update rewound to turn one");
      assert.equal(third[1], `${CLOCK_PREFIX}2 (epoch 2)`);
    });

    it("ignores a delayed update to a user message two turns back after the new turn drained", async () => {
      await hooks.event(userMessageEvent("ses_far2", "msg_o1"));
      await transform([HEADER], "ses_far2");
      await hooks.event(userMessageEvent("ses_far2", "msg_o2"));
      await hooks.event(userMessageEvent("ses_far2", "msg_o3"));
      const first = await transform([HEADER], "ses_far2");
      assert.equal(drainCount(fake), 2);

      await hooks.event(userMessageEvent("ses_far2", "msg_o1"));
      const second = await transform([HEADER], "ses_far2");
      assert.equal(drainCount(fake), 2, "late update caused another consumptive drain");
      assert.deepEqual(second, first, "system text changed mid-turn");
    });

    // Real OpenCode ids: the 12 hex digits after `msg_` are the low 48 bits of
    // the creation time in 1/4096 ms, so they wrapped on 2026-08-14 and a
    // message created after that sorts below one created before it.
    // CLOCK_STEP_ID was created after POST_WRAP_ID once the clock stepped back
    // 412 ms; LATER_ID was created 5 s after POST_WRAP_ID and sorts above it.
    const PRE_WRAP_ID = "msg_f4d541459001u0Kb7U0n31y156";
    const POST_WRAP_ID = "msg_10001428c001o7DH2T7BpAk6CR";
    const CLOCK_STEP_ID = "msg_1000140f0001Qm4TzR8vXc2LpA";
    const LATER_ID = "msg_100015614001Vb9KcT3wQe7NhZ";

    it("opens a turn for every new user message whatever order the ids sort in", async () => {
      const ids = [PRE_WRAP_ID, POST_WRAP_ID, CLOCK_STEP_ID];
      for (const [i, id] of ids.entries()) {
        const n = i + 1;
        await hooks.event(userMessageEvent("ses_wrap", id));
        const first = await transform([HEADER], "ses_wrap");
        assert.equal(drainCount(fake), n, `user message ${id} did not open a turn`);
        assert.equal(first[1], `${CLOCK_PREFIX}${n} (epoch ${n})`);
        const second = await transform([HEADER], "ses_wrap");
        assert.equal(drainCount(fake), n, "a later generation of the turn drained again");
        assert.deepEqual(second, first);
      }
    });

    for (const [order, earlier, later] of [
      ["above", PRE_WRAP_ID, POST_WRAP_ID],
      ["below", POST_WRAP_ID, LATER_ID],
    ]) {
      it(`ignores a delayed update to a user message whose id sorts ${order} the current turn's`, async () => {
        const sessionID = `ses_wrap_${order}`;
        await hooks.event(userMessageEvent(sessionID, earlier));
        await transform([HEADER], sessionID);
        await hooks.event(userMessageEvent(sessionID, later));
        // The earlier message's summary update lands before turn two drained.
        await hooks.event(userMessageEvent(sessionID, earlier));
        const first = await transform([HEADER], sessionID);
        assert.equal(drainCount(fake), 2, "turn two never drained");
        assert.equal(first[1], `${CLOCK_PREFIX}2 (epoch 2)`);

        // And again after it drained.
        await hooks.event(userMessageEvent(sessionID, earlier));
        const second = await transform([HEADER], sessionID);
        assert.equal(drainCount(fake), 2, "late update caused another consumptive drain");
        assert.deepEqual(second, first, "system text changed mid-turn");
      });
    }

    it("forgets the oldest user messages so a session's memory stays bounded", async () => {
      const ids = Array.from({ length: 1000 }, (_, i) => `msg_bound${String(i).padStart(4, "0")}`);
      for (const id of ids) {
        await hooks.event(userMessageEvent("ses_bound", id));
      }
      await transform([HEADER], "ses_bound");
      assert.equal(drainCount(fake), 1);

      // A recent message is still known, so its late update is ignored.
      await hooks.event(userMessageEvent("ses_bound", ids.at(-2)));
      await transform([HEADER], "ses_bound");
      assert.equal(drainCount(fake), 1, "a recent message's late update opened a turn");

      // The first message aged out long ago: its late update opens a turn and
      // costs one extra drain rather than the session keeping every id.
      await hooks.event(userMessageEvent("ses_bound", ids[0]));
      await transform([HEADER], "ses_bound");
      assert.equal(drainCount(fake), 2, "the session still remembers its first message");
    });

    it("shares one pending drain between concurrent generations of a turn", async () => {
      await hooks.event(userMessageEvent("ses_race", "msg_r1"));
      const [g1, g2] = await Promise.all([
        transform([HEADER], "ses_race"),
        transform([HEADER], "ses_race"),
      ]);
      assert.equal(drainCount(fake), 1);
      assert.deepEqual(g1, g2);
      assert.equal(g1[1], `${CLOCK_PREFIX}1 (epoch 1)`);
    });

    it("drains every generation when no turn is known", async () => {
      // No session id on the transform input (OpenCode's Agent.generate sends
      // only the model) and a session that never reported a user message
      // both fall back to draining each time so nudges are never silently
      // withheld.
      const noSession1 = await transform([HEADER], null);
      const noSession2 = await transform([HEADER], null);
      assert.equal(noSession1[1], `${CLOCK_PREFIX}1 (epoch 1)`);
      assert.equal(noSession2[1], `${CLOCK_PREFIX}2 (epoch 2)`);

      const noTurn1 = await transform([HEADER], "ses_no_turn");
      const noTurn2 = await transform([HEADER], "ses_no_turn");
      assert.equal(noTurn1[1], `${CLOCK_PREFIX}3 (epoch 3)`);
      assert.equal(noTurn2[1], `${CLOCK_PREFIX}4 (epoch 4)`);
    });
  });

  describe(`${plugin.name} plugin turn order under transcript mirroring`, { timeout: SUITE_TIMEOUT_MS }, () => {
    beforeEach(() => {
      resetDrains(fake);
    });

    it("opens the turn before the event handler awaits the transcript mirror", async () => {
      // message.updated mirrors the transcript after opening the turn, and
      // the mirror waits on client round trips (MiMo Code mirrors in every
      // default session). This client holds them until both generations of
      // the turn have run, so a turn opened after the mirror is seen late.
      let release;
      const released = new Promise((resolve) => {
        release = resolve;
      });
      let mirrorStarted = false;
      const client = {
        session: {
          get: async ({ path: { id } }) => {
            mirrorStarted = true;
            await released;
            return { data: { id } };
          },
          messages: async () => {
            await released;
            return { data: [] };
          },
        },
      };
      const staged = stagePlugin(tmp, plugin);
      const { default: gascityPlugin } = await import(pathToFileURL(staged).href);
      const hooks = await gascityPlugin({ directory: tmp, client });
      async function generate() {
        const output = { system: [HEADER] };
        await hooks["experimental.chat.system.transform"]({ sessionID: "ses_mirror", model: {} }, output);
        return output.system;
      }

      const pending = hooks.event(userMessageEvent("ses_mirror", "msg_x1"));
      const first = await generate();
      const second = await generate();
      assert.ok(mirrorStarted, "the handler never reached the client, so nothing held it");
      release();
      await pending;

      assert.equal(drainCount(fake), 1, "turn was not open when the first generation ran");
      assert.deepEqual(second, first);
      assert.ok(
        fs.existsSync(path.join(transcriptDir, "ses_mirror.json")),
        "the mirror wrote outside the suite's temp dir",
      );
    });
  });

  describe(`${plugin.name} plugin session lifecycle events`, { timeout: SUITE_TIMEOUT_MS }, () => {
    // A fresh plugin instance per test: the prime cache is per instance and
    // these tests stamp it.
    const known = new Map();
    const client = {
      session: {
        get: async ({ path: { id } }) => {
          if (!known.has(id)) {
            throw new Error(`unknown session ${id}`);
          }
          return { data: { id, ...known.get(id) } };
        },
        messages: async () => ({ data: [] }),
      },
    };
    let hooks;

    before(async () => {
      const staged = stagePlugin(tmp, plugin);
      const { default: gascityPlugin } = await import(pathToFileURL(staged).href);
      hooks = await gascityPlugin({ directory: tmp, client });
    });

    beforeEach(() => {
      resetDrains(fake);
      fs.rmSync(fake.primeLog, { force: true });
      known.clear();
      delete process.env.GC_FAKE_MAIL;
      delete process.env.GC_FAKE_PRIME_STAMP;
    });

    async function stableSystem(sessionID) {
      const output = { system: [HEADER] };
      await hooks["experimental.chat.system.transform"]({ sessionID, model: {} }, output);
      return output.system[0];
    }

    it("refreshes the prime with the root session id and never with a child's", async () => {
      process.env.GC_FAKE_PRIME_STAMP = "root";
      await hooks.event(sessionEvent("session.created", "ses_root", { directory: tmp }));
      assert.deepEqual(readPrimeLog(fake), ["ses_root"]);
      const rootPrime = await stableSystem("ses_root");
      assert.ok(rootPrime.startsWith(`${ROLE} (stamp root)`), rootPrime);

      // A subagent session: session.created carries info.parentID. Running
      // `gc prime --hook` here would hand the child id to
      // persistPrimeHookProviderSessionKey as the resume key and re-bake the
      // shared prime, changing the parent's system[0] mid-turn.
      process.env.GC_FAKE_PRIME_STAMP = "child";
      await hooks.event(
        sessionEvent("session.created", "ses_child", { parentID: "ses_root", directory: tmp }),
      );
      assert.deepEqual(readPrimeLog(fake), ["ses_root"], "child session.created must not run prime --hook");
      assert.equal(await stableSystem("ses_root"), rootPrime, "child creation changed the shared prime");

      // session.compacted carries only the id; the child was seen above.
      await hooks.event(sessionEvent("session.compacted", "ses_child"));
      assert.deepEqual(readPrimeLog(fake), ["ses_root"], "child session.compacted must not run prime --hook");
      assert.equal(await stableSystem("ses_root"), rootPrime);

      // A child never seen through session.created (resumed subagent) is
      // resolved through the client.
      known.set("ses_child_resumed", { parentID: "ses_root", directory: tmp });
      await hooks.event(sessionEvent("session.compacted", "ses_child_resumed"));
      assert.deepEqual(readPrimeLog(fake), ["ses_root"], "resolved child must not run prime --hook");
      assert.equal(await stableSystem("ses_root"), rootPrime);

      // The root's own compaction still refreshes.
      process.env.GC_FAKE_PRIME_STAMP = "compacted";
      await hooks.event(sessionEvent("session.compacted", "ses_root"));
      assert.deepEqual(readPrimeLog(fake), ["ses_root", "ses_root"]);
      assert.ok((await stableSystem("ses_root")).startsWith(`${ROLE} (stamp compacted)`));
    });

    it("treats a session the client cannot resolve as a root session", async () => {
      // session.compacted with no info, unknown to the client: the refresh
      // must not be withheld from what may be the agent's own session.
      await hooks.event(sessionEvent("session.compacted", "ses_unknown"));
      assert.deepEqual(readPrimeLog(fake), ["ses_unknown"]);
    });
  });
  describe(`${plugin.name} plugin SessionStart context`, { timeout: SUITE_TIMEOUT_MS }, () => {
    // A fresh plugin instance per test: the arm and claim records live in the
    // plugin's closure. The client has no session API, so session.created
    // neither resolves the session through the client nor mirrors a transcript.
    let factory;
    let hooks;
    const WARN_PREFIX = `gascity ${plugin.name} plugin:`;
    const PART_TEXT = `${SESSION_START_LABEL}${SESSION_START}`;

    before(async () => {
      const staged = stagePlugin(tmp, plugin);
      ({ default: factory } = await import(pathToFileURL(staged).href));
    });

    beforeEach(async () => {
      resetDrains(fake);
      resetSessionStartLogs();
      for (const name of [
        "GC_FAKE_MAIL",
        "GC_FAKE_PRIME_STAMP",
        "GC_FAKE_SESSIONSTART",
        "GC_FAKE_SESSIONSTART_FAIL",
        "GC_MANAGED_SESSION_HOOK",
        "GC_HOOK_EVENT_NAME",
      ]) {
        delete process.env[name];
      }
      hooks = await factory({ directory: tmp, client: {} });
    });

    const created = (sessionID, info = { directory: tmp }) =>
      hooks.event(sessionEvent("session.created", sessionID, info));

    const BOTH_MARKERS = { managed: "1", event: "SessionStart" };

    it("registers a chat.message hook", () => {
      assert.equal(typeof hooks["chat.message"], "function");
    });

    it("arms a root session and delivers one synthetic part on its first message", async () => {
      await created("ses_root");
      const msgID = "msg_b1c2d3e4f5a6AbCdEfGhIjKlMn";
      const host = await createUserMessage(hooks, "ses_root", msgID);

      assert.equal(host.injected.length, 1, "expected exactly one synthetic part");
      assert.deepEqual(host.injected[0], {
        id: "prt_-b1c2d3e4f5a6AbCdEfGhIjKlMn",
        sessionID: "ses_root",
        messageID: msgID,
        type: "text",
        text: PART_TEXT,
        synthetic: true,
        metadata: { gascity: { hook: "SessionStart" } },
      });
      assert.equal(host.persisted.length, HOST_PART_IDS.length + 1);

      // The id sorts ahead of every ascending host id (v1 hex and v2 g-led),
      // so the model reads the context before the prompt.
      assert.equal(host.model[0], host.injected[0], "the part must lead the model's view");
      assert.deepEqual(
        host.tui.map((p) => p.id),
        HOST_PART_IDS,
        "the TUI must show only the user's own parts",
      );

      // The persona refresh and the SessionStart prime are independent.
      assert.deepEqual(
        primeCallKeys(primeCalls()),
        ["ses_root||", `ses_root|${BOTH_MARKERS.managed}|${BOTH_MARKERS.event}`].sort(),
      );
      assert.equal(host.message.system, undefined, "the hook must not write output.message.system");
      assert.ok(!("system" in host.message));
    });

    it("pushes in place: output.parts keeps its identity", async () => {
      await created("ses_inplace");
      const parts = [];
      const output = { message: { id: "msg_inplace1" }, parts };
      await hooks["chat.message"]({ sessionID: "ses_inplace" }, output);
      assert.equal(output.parts, parts, "output.parts was reassigned");
      assert.equal(parts.length, 1);
      assert.equal(parts[0].id, "prt_-inplace1");
    });

    it("derives a part id per message from the message id", async () => {
      await created("ses_ids_a");
      await created("ses_ids_b");
      const a = await createUserMessage(hooks, "ses_ids_a", "msg_aaaaaaaaaaaa1");
      const b = await createUserMessage(hooks, "ses_ids_b", "msg_bbbbbbbbbbbb2");
      assert.equal(a.injected[0].id, "prt_-aaaaaaaaaaaa1");
      assert.equal(b.injected[0].id, "prt_-bbbbbbbbbbbb2");
      assert.notEqual(a.injected[0].id, b.injected[0].id);
    });

    it("arms before the event handler's first await", async () => {
      // The host runs `void hook.event(...)`; the arm must already be recorded
      // when chat.message runs, whatever the handler is still awaiting.
      const pending = created("ses_sync");
      const host = await createUserMessage(hooks, "ses_sync", "msg_sync0001");
      await pending;
      assert.equal(host.injected.length, 1);
    });

    it("never arms a child session", async () => {
      await created("ses_child", { parentID: "ses_parent", directory: tmp });
      const before = spawnLines().length;
      const host = await createUserMessage(hooks, "ses_child", "msg_child0001");
      assert.equal(host.injected.length, 0);
      assert.equal(spawnLines().length, before, "chat.message spawned gc for a child session");
      assert.deepEqual(sessionStartPrimes(), []);
    });

    it("does nothing for a session it never saw created (resume)", async () => {
      const host = await createUserMessage(hooks, "ses_resumed", "msg_resume001");
      assert.equal(host.injected.length, 0);
      assert.equal(spawnLines().length, 0, "chat.message spawned gc for an unarmed session");
      assert.deepEqual(sessionStartPrimes(), []);
    });

    it("does not arm on session.created without info", async () => {
      await created("ses_noinfo", null);
      const before = spawnLines().length;
      const host = await createUserMessage(hooks, "ses_noinfo", "msg_noinfo001");
      assert.equal(host.injected.length, 0);
      assert.equal(spawnLines().length, before);
      assert.deepEqual(sessionStartPrimes(), []);
    });

    it("does not arm on session.created after the first chat.message", async () => {
      // 1.14 delivers events on a separate fiber, so session.created can land
      // after the first message. A late arm must not turn the second message
      // into the claim.
      const first = await createUserMessage(hooks, "ses_late", "msg_late00001");
      assert.equal(first.injected.length, 0);
      await created("ses_late");
      const before = spawnLines().length;
      const second = await createUserMessage(hooks, "ses_late", "msg_late00002");
      assert.equal(second.injected.length, 0);
      assert.equal(spawnLines().length, before);
      assert.deepEqual(sessionStartPrimes(), []);
    });

    it("keeps session.compacted marker-free and never arms from it", async () => {
      await hooks.event(sessionEvent("session.compacted", "ses_compacted"));
      assert.deepEqual(primeCallKeys(primeCalls()), ["ses_compacted||"]);
      const host = await createUserMessage(hooks, "ses_compacted", "msg_compact01");
      assert.equal(host.injected.length, 0);
      assert.deepEqual(sessionStartPrimes(), []);
    });

    it("carries both markers on exactly one prime across a full lifecycle", async () => {
      await created("ses_life");
      const first = await createUserMessage(hooks, "ses_life", "msg_life00001");
      await hooks.event(userMessageEvent("ses_life", "msg_life00001"));
      const output = { system: [HEADER] };
      await hooks["experimental.chat.system.transform"]({ sessionID: "ses_life", model: {} }, output);
      await hooks.event(sessionEvent("session.compacted", "ses_life"));
      const second = await createUserMessage(hooks, "ses_life", "msg_life00002");
      await hooks.event(sessionEvent("session.compacted", "ses_life"));

      assert.equal(first.injected.length, 1);
      assert.equal(second.injected.length, 0);
      const calls = primeCalls();
      assert.equal(sessionStartPrimes().length, 1, JSON.stringify(calls));
      assert.equal(
        calls.filter((c) => c.managed !== "" || c.event !== "").length,
        1,
        "a marker appeared outside the one SessionStart prime",
      );
      // Neither marker is in the persona the transform prepends.
      assert.ok(output.system[0].startsWith(ROLE));
      assert.equal(countOccurrences(output.system.join("\n"), SESSION_START), 0);
    });

    it("gives one prime and one part for sequential and concurrent first messages", async () => {
      await created("ses_seq");
      await created("ses_conc");
      const before = spawnLines().length;

      const one = await createUserMessage(hooks, "ses_seq", "msg_seq000001");
      const two = await createUserMessage(hooks, "ses_seq", "msg_seq000002");
      assert.equal(one.injected.length, 1);
      assert.equal(two.injected.length, 0);
      assert.equal(spawnLines().length - before, 1, "sequential messages spawned gc more than once");

      const mid = spawnLines().length;
      const [a, b] = await Promise.all([
        createUserMessage(hooks, "ses_conc", "msg_conc00001"),
        createUserMessage(hooks, "ses_conc", "msg_conc00002"),
      ]);
      assert.equal(a.injected.length + b.injected.length, 1, "concurrent messages must yield one part");
      assert.equal(spawnLines().length - mid, 1, "concurrent messages spawned gc more than once");
      assert.equal(sessionStartPrimes().length, 2, "one SessionStart prime per session");
    });

    it("pushes nothing and stays silent when the payload is empty", async () => {
      process.env.GC_FAKE_SESSIONSTART = "";
      await created("ses_empty");
      let host;
      const warnings = await captureWarnings(async () => {
        host = await createUserMessage(hooks, "ses_empty", "msg_empty0001");
      });
      assert.equal(host.injected.length, 0);
      assert.deepEqual(warnings, []);
      assert.equal(sessionStartPrimes().length, 1);
    });

    it("warns, pushes nothing and still creates the message when gc fails", async () => {
      process.env.GC_FAKE_SESSIONSTART_FAIL = "1";
      await created("ses_fail");
      let host;
      const warnings = await captureWarnings(async () => {
        host = await createUserMessage(hooks, "ses_fail", "msg_fail00001");
      });
      assert.equal(host.injected.length, 0);
      assert.deepEqual(
        host.persisted.map((p) => p.id),
        HOST_PART_IDS,
        "the user's own parts must persist untouched",
      );
      assert.equal(warnings.length, 1, JSON.stringify(warnings));
      assert.equal(warnings[0][0], WARN_PREFIX);
      assert.equal(sessionStartPrimes().length, 1);
    });

    for (const [label, outputFor] of [
      ["parts is not an array", () => ({ message: { id: "msg_bad000001" }, parts: undefined })],
      ["parts is an object", () => ({ message: { id: "msg_bad000002" }, parts: {} })],
      ["message id is empty", () => ({ message: { id: "" }, parts: [] })],
      ["message id is missing", () => ({ message: {}, parts: [] })],
      ["message id is not a string", () => ({ message: { id: 7 }, parts: [] })],
    ]) {
      it(`warns once and runs no SessionStart prime when ${label}`, async () => {
        const sessionID = `ses_invalid_${label.replace(/\W+/g, "_")}`;
        await created(sessionID);
        const output = outputFor();
        let thrown = null;
        const warnings = await captureWarnings(async () => {
          try {
            await hooks["chat.message"]({ sessionID }, output);
          } catch (err) {
            thrown = err;
          }
        });
        assert.equal(thrown, null, "the hook threw");
        assert.equal(warnings.length, 1, JSON.stringify(warnings));
        assert.equal(warnings[0][0], WARN_PREFIX);
        assert.deepEqual(sessionStartPrimes(), [], "a prime ran without a delivery channel");

        // Nothing was consumed: the per-turn path still delivers.
        await hooks.event(userMessageEvent(sessionID, "msg_turn00001"));
        const system = { system: [HEADER] };
        await hooks["experimental.chat.system.transform"]({ sessionID, model: {} }, system);
        assert.equal(system.system[1], `${CLOCK_PREFIX}1 (epoch 1)`);
      });
    }

    it("never throws whatever input and output it is given", async () => {
      await created("ses_armed_junk");
      const junk = [undefined, null, {}];
      for (const sessionID of ["ses_never_seen", "ses_armed_junk"]) {
        for (const input of junk) {
          for (const output of junk) {
            const call = hooks["chat.message"](input ?? undefined, output ?? undefined);
            await call;
          }
        }
        await hooks["chat.message"]({ sessionID }, undefined);
        await hooks["chat.message"]({ sessionID }, null);
        await hooks["chat.message"]({ sessionID }, {});
      }
    });

    it("stays silent for every unarmed shape", async () => {
      const warnings = await captureWarnings(async () => {
        for (const input of [undefined, null, {}, { sessionID: "ses_quiet" }]) {
          for (const output of [undefined, null, {}, { message: {}, parts: undefined }]) {
            await hooks["chat.message"](input, output);
          }
        }
      });
      assert.deepEqual(warnings, []);
      assert.equal(spawnLines().length, 0);
    });

    it("opens no turn and drains nothing from chat.message", async () => {
      await created("ses_noturn");
      const spawnsBefore = spawnLines().length;
      await createUserMessage(hooks, "ses_noturn", "msg_noturn001");
      const added = spawnLines().slice(spawnsBefore);
      assert.equal(added.length, 1, `chat.message spawned ${JSON.stringify(added)}`);
      assert.ok(added[0].startsWith("prime --hook"), added[0]);
      assert.equal(drainCount(fake), 0, "chat.message drained nudges");

      // With no turn known the transform drains every time, so two
      // generations drain twice: chat.message did not open the turn.
      await hooks["experimental.chat.system.transform"]({ sessionID: "ses_noturn", model: {} }, { system: [HEADER] });
      await hooks["experimental.chat.system.transform"]({ sessionID: "ses_noturn", model: {} }, { system: [HEADER] });
      assert.equal(drainCount(fake), 2, "chat.message opened a turn");
    });

    it("keeps system[0] byte-identical across turns with the part present", async () => {
      process.env.GC_FAKE_MAIL = MAIL;
      await created("ses_stable");
      async function generate(message) {
        const system = [[AGENT_PROMPT, ENVIRONMENT, message.system].filter(Boolean).join("\n")];
        await hooks["experimental.chat.system.transform"]({ sessionID: "ses_stable", model: {} }, { system });
        return system;
      }
      async function turn(messageID) {
        const host = await createUserMessage(hooks, "ses_stable", messageID);
        void hooks.event(userMessageEvent("ses_stable", messageID));
        return [host, await generate(host.message), await generate(host.message)];
      }
      const [hostOne, oneA, oneB] = await turn("msg_stable0001");
      const [hostTwo, twoA, twoB] = await turn("msg_stable0002");

      assert.equal(hostOne.injected.length, 1);
      assert.equal(hostTwo.injected.length, 0);
      for (const system of [oneA, oneB, twoA, twoB]) {
        assert.equal(system[0], `${ROLE}\n\n${AGENT_PROMPT}\n${ENVIRONMENT}`);
        assert.equal(countOccurrences(system.join("\n"), SESSION_START), 0, "the part's text leaked into system[]");
      }
    });
  });
}
