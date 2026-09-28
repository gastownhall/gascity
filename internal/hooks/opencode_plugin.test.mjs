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
// observe each other and the tests can reset between cases.
function writeFakeGc(dir) {
  const counter = path.join(dir, "drain-count");
  const script = `#!/bin/sh
case "$1 $2" in
  "prime --hook")
    printf '%s' '${ROLE}'
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
  return { bin, counter };
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

    async function transform(system) {
      const output = { system };
      await hooks["experimental.chat.system.transform"](
        { sessionID: "ses_test", model: {} },
        output,
      );
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
    async function generate(message) {
      const system = [[AGENT_PROMPT, ENVIRONMENT, message.system].filter(Boolean).join("\n")];
      const header = system[0];
      await hooks["experimental.chat.system.transform"]({ sessionID: "ses_test", model: {} }, { system });
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
  });
}
