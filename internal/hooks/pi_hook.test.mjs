// Behavioral tests for the embedded Pi extension (gc-hooks.js).
//
// Run directly with `node --test internal/hooks/pi_hook.test.mjs`;
// TestPiHookNodeSuite (plugin_node_test.go) runs the same file under
// `go test ./internal/hooks` whenever node is on PATH.
//
// The extension resolves gc through GC_BIN at module load, so the suite points
// GC_BIN at a fake gc script before it loads the extension. The fake answers
// `nudge drain --inject` with a line that changes on every call, as the real
// command does with its clock line.

import { after, before, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { createRequire } from "node:module";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const PRIME = "PRIME CONTEXT";
const WORK = "WORK CONTEXT";
const NUDGE_PREFIX = "Current time: tick-";
const BASE = "PI SYSTEM PROMPT";

const extensionPath = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
  "bootstrap",
  "packs",
  "core",
  "overlay",
  "per-provider",
  "pi",
  ".pi",
  "extensions",
  "gc-hooks.js",
);

function writeFakeGc(dir) {
  const counter = path.join(dir, "drain-count");
  const script = `#!/bin/sh
case "$1 $2" in
  "prime --hook")
    if [ ! -f "${dir}/no-prime" ]; then printf '%s' '${PRIME}'; fi
    ;;
  "hook --inject") printf '%s' '${WORK}' ;;
  "nudge drain")
    n=0
    if [ -f "${counter}" ]; then n=$(cat "${counter}"); fi
    n=$((n + 1))
    printf '%s' "$n" > "${counter}"
    printf '%s%s' '${NUDGE_PREFIX}' "$n"
    ;;
  "mail check")
    if [ -n "\${GC_FAKE_MAIL:-}" ]; then printf '%s' "\${GC_FAKE_MAIL}"; fi
    ;;
  *) ;;
esac
`;
  const bin = path.join(dir, "gc");
  fs.writeFileSync(bin, script, { mode: 0o755 });
  return { bin, counter };
}

// Loads a fresh copy of the extension and returns its registered handlers.
function loadExtension() {
  const require = createRequire(import.meta.url);
  delete require.cache[extensionPath];
  const factory = require(extensionPath);
  const handlers = {};
  factory({ on: (name, handler) => { handlers[name] = handler; } });
  return handlers;
}

describe("pi gc-hooks.js", () => {
  let dir;
  let fake;
  let ctx;

  before(() => {
    dir = fs.mkdtempSync(path.join(os.tmpdir(), "gc-pi-hook-"));
    fake = writeFakeGc(dir);
    process.env.GC_BIN = fake.bin;
    delete process.env.GC_PI_TRANSCRIPT_DIR;
    ctx = { cwd: dir, sessionManager: { getSessionId: () => "pi-session-1" } };
  });

  after(() => {
    fs.rmSync(dir, { recursive: true, force: true });
  });

  beforeEach(() => {
    fs.rmSync(fake.counter, { force: true });
    delete process.env.GC_FAKE_MAIL;
  });

  it("puts the per-prompt context in sections and leaves the system prompt alone", () => {
    const hooks = loadExtension();
    hooks.session_start({}, ctx);
    const sections = { "other-extension": "kept" };
    const first = { systemPrompt: BASE, systemPromptOptions: { sections } };

    assert.equal(hooks.before_agent_start(first, ctx), undefined);
    assert.equal(first.systemPrompt, BASE);
    assert.equal(sections["gc-prime"], PRIME);
    assert.equal(sections["gc-work"], WORK);
    assert.equal(sections["gc-nudges"], `${NUDGE_PREFIX}1`);
    assert.equal("gc-mail" in sections, false);
    assert.equal(sections["other-extension"], "kept");

    // Pi may rebuild the options for every prompt, so the next prompt gets a
    // fresh sections object.
    process.env.GC_FAKE_MAIL = "You have unread mail.";
    const next = { "other-extension": "kept" };
    const second = { systemPrompt: BASE, systemPromptOptions: { sections: next } };
    assert.equal(hooks.before_agent_start(second, ctx), undefined);
    assert.equal(second.systemPrompt, BASE);
    assert.equal(next["gc-prime"], PRIME, "the startup context stays for the session");
    assert.equal(next["gc-work"], WORK);
    assert.equal(next["gc-nudges"], `${NUDGE_PREFIX}2`);
    assert.equal(next["gc-mail"], "You have unread mail.");
    assert.equal(next["other-extension"], "kept");

    // A section whose command now prints nothing goes away.
    delete process.env.GC_FAKE_MAIL;
    hooks.before_agent_start({ systemPrompt: BASE, systemPromptOptions: { sections: next } }, ctx);
    assert.equal("gc-mail" in next, false);
  });

  it("starts a new session without the previous session's startup context", () => {
    const hooks = loadExtension();
    hooks.session_start({}, ctx);
    hooks.before_agent_start({ systemPrompt: BASE, systemPromptOptions: { sections: {} } }, ctx);
    fs.writeFileSync(path.join(dir, "no-prime"), "");
    hooks.session_start({}, ctx);
    const sections = {};
    hooks.before_agent_start({ systemPrompt: BASE, systemPromptOptions: { sections } }, ctx);
    assert.equal("gc-prime" in sections, false);
    fs.rmSync(path.join(dir, "no-prime"));
  });

  it("falls back to replacing the system prompt when Pi offers no sections", () => {
    const hooks = loadExtension();
    hooks.session_start({}, ctx);
    const result = hooks.before_agent_start({ systemPrompt: BASE }, ctx);
    assert.deepEqual(result, {
      systemPrompt: [BASE, PRIME, WORK, `${NUDGE_PREFIX}1`].join("\n\n"),
    });
  });
});
