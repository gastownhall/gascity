import assert from "node:assert/strict";
import childProcess from "node:child_process";
import { readFile } from "node:fs/promises";
import { syncBuiltinESMExports } from "node:module";
import test from "node:test";

// Unmanaged factory calls must remain silent no-ops, even when repeatedly loaded.
test("OpenCode factory is silent without identity and registers managed hooks", async () => {
  const source = await readFile(new URL(
    "../bootstrap/packs/core/overlay/per-provider/opencode/.opencode/plugins/gascity.js",
    import.meta.url,
  ), "utf8");
  const savedEnv = process.env;
  const savedStdout = process.stdout.write;
  const savedStderr = process.stderr.write;
  const savedCommands = {};
  const calls = [];
  const stdout = [];
  const stderr = [];
  try {
    for (const name of ["exec", "execSync", "execFile", "execFileSync", "spawn", "spawnSync", "fork"]) {
      savedCommands[name] = childProcess[name];
      childProcess[name] = (...args) => {
        calls.push({ name, args });
        throw new Error("unexpected subprocess call");
      };
    }
    syncBuiltinESMExports();
    process.env = {};
    const { default: factory } = await import(
      `data:text/javascript;base64,${Buffer.from(source).toString("base64")}`
    );
    process.stdout.write = (chunk) => { stdout.push(String(chunk)); return true; };
    process.stderr.write = (chunk) => { stderr.push(String(chunk)); return true; };
    const identityKeys = ["GC_SESSION_ID", "GC_SESSION_NAME", "GC_ALIAS", "GC_AGENT", "GC_TEMPLATE"];
    for (const env of [{}, Object.fromEntries(identityKeys.map((key) => [key, ""])), Object.fromEntries(identityKeys.map((key) => [key, " \t\n"]))]) {
      process.env = env;
      for (let i = 0; i < 3; i++) {
        assert.deepEqual(await factory({ directory: "/unused", client: {} }), {});
      }
    }
    assert.deepEqual(calls, [], "unmanaged factories must not launch subprocesses");
    assert.deepEqual(stdout, [], "unmanaged factories must not write stdout");
    assert.deepEqual(stderr, [], "unmanaged factories must not write stderr");

    for (const key of identityKeys) {
      process.env = { [key]: "managed-test-session" };
      const hooks = await factory({ directory: "/unused", client: {} });
      assert.deepEqual(Object.keys(hooks).sort(), [
        "chat.message", "event", "experimental.chat.system.transform", "experimental.session.compacting",
      ]);
      for (const hook of Object.values(hooks)) assert.equal(typeof hook, "function");
    }
  } finally {
    process.env = savedEnv;
    process.stdout.write = savedStdout;
    process.stderr.write = savedStderr;
    Object.assign(childProcess, savedCommands);
    syncBuiltinESMExports();
  }
});
