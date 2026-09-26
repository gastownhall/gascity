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
// emits `Current time: ...` even when the queue is empty.

import { after, before, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";

const ROLE = "ROLE PROMPT: you are the configured agent.";
const AGENT_PROMPT = "PROVIDER SYSTEM PROMPT";
const ENVIRONMENT = "AGENTS.md instructions\n\nskills";
const HEADER = `${AGENT_PROMPT}\n\n${ENVIRONMENT}`;
const MAIL = "You have unread mail.";
const CLOCK_PREFIX = "Current time: tick-";

const overlayDir = path.resolve(
  import.meta.dirname,
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
// `prime --hook` call appends the GC_PROVIDER_SESSION_ID it was given to the
// prime log, and GC_FAKE_PRIME_STAMP marks the output the way the real
// command bakes the current time into its beacon, so a test can tell a
// refreshed prime from the cached one.
function writeFakeGc(dir) {
  const counter = path.join(dir, "drain-count");
  const primeLog = path.join(dir, "prime-log");
  const script = `#!/bin/sh
case "$1 $2" in
  "prime --hook")
    printf '%s\\n' "\${GC_PROVIDER_SESSION_ID:-<unset>}" >> "${primeLog}"
    printf '%s' '${ROLE}'
    if [ -n "\${GC_FAKE_PRIME_STAMP:-}" ]; then printf ' (stamp %s)' "\${GC_FAKE_PRIME_STAMP}"; fi
    ;;
  "nudge drain")
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
  return { bin, counter, primeLog };
}

function readPrimeLog(fake) {
  if (!fs.existsSync(fake.primeLog)) {
    return [];
  }
  return fs.readFileSync(fake.primeLog, "utf-8").split("\n").filter(Boolean);
}

function drainCount(fake) {
  if (!fs.existsSync(fake.counter)) {
    return 0;
  }
  return Number(fs.readFileSync(fake.counter, "utf-8"));
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
  fs.copyFileSync(plugin.source, path.join(stage, "gascity.js"));
  // OpenCode loads workdir plugins as ES modules; the module-type package.json
  // gives the staged copy the same semantics outside a package.
  fs.writeFileSync(path.join(stage, "package.json"), '{"type":"module"}');
  return path.join(stage, "gascity.js");
}

function countOccurrences(haystack, needle) {
  return haystack.split(needle).length - 1;
}

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "gc-plugin-test-"));
const fake = writeFakeGc(tmp);
process.env.GC_BIN = fake.bin;

after(() => {
  fs.rmSync(tmp, { recursive: true, force: true });
});

for (const plugin of plugins) {
  describe(`${plugin.name} plugin experimental.chat.system.transform`, () => {
    let hooks;

    before(async () => {
      const staged = stagePlugin(tmp, plugin);
      const { default: gascityPlugin } = await import(pathToFileURL(staged).href);
      hooks = await gascityPlugin({ directory: tmp, client: {} });
    });

    beforeEach(() => {
      fs.rmSync(fake.counter, { force: true });
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

    it("registers no chat.message hook and opens the turn from message.updated alone", async () => {
      // OpenCode persists the user message (publishing message.updated to
      // plugins inline, before the handler's first await can yield) and only
      // then starts the turn's first generation, so message.updated is
      // sufficient to open the turn. chat.message would arrive earlier still,
      // but any hook registered there risks writing output.message.system,
      // which OpenCode folds back into system[0] every generation.
      assert.equal(hooks["chat.message"], undefined);

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
      // No session id on the transform input (older payloads) and a session
      // that never reported a user message both fall back to draining each
      // time so nudges are never silently withheld.
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

  describe(`${plugin.name} plugin session lifecycle events`, () => {
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
      fs.rmSync(fake.counter, { force: true });
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
}
