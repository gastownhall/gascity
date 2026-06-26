// Entrypoint smoke coverage for the two executable bridges. bridge.mjs and
// telegram-bridge.mjs have top-level side effects (env checks, daemon probes,
// callback servers), so they cannot be imported in-process without starting
// real I/O. `node --check` parses each file without executing it, which catches
// syntax errors and other parse-time breakage in the executable entrypoints
// that the lib-only suites would otherwise let merge. The openclaw loader and
// shared-lib imports the entrypoints depend on are covered separately by
// openclaw-loader.test.mjs, gc-client.test.mjs, and inbound.test.mjs.
//
// Run: npm test  (or: node --test test/)

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { execFile } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { promisify } from 'node:util'

const execFileAsync = promisify(execFile)
const entrypoint = (f) => fileURLToPath(new URL(`../${f}`, import.meta.url))

for (const file of ['bridge.mjs', 'telegram-bridge.mjs']) {
  test(`node --check passes for ${file} (executable entrypoint parses)`, async () => {
    try {
      await execFileAsync(process.execPath, ['--check', entrypoint(file)])
    } catch (err) {
      assert.fail(`node --check ${file} failed:\n${err.stderr || err.message}`)
    }
  })
}
