// Smoke coverage for the openclaw connector loader (lib/openclaw.mjs). The other
// suites import only lib/inbound.mjs + lib/gc-client.mjs, so nothing exercised
// the part of the bridge that actually reaches into the installed openclaw
// packages — including resolveChunkExport, which scans @openclaw/imessage's
// hash-named dist chunks for createIMessageRpcClient and is the most fragile
// seam across openclaw releases. This test loads both connector surfaces after
// `npm ci` and asserts every function the bridges call resolves, then drives the
// resolved RPC client against the fake imsg CLI to pin the lifecycle contract
// bridge.mjs relies on (start, request, stop, waitForClose settles).
//
// Run: npm test  (or: node --test test/)

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { loadIMessageConnector, loadTelegramConnector, resolveChunkExport } from '../lib/openclaw.mjs'

const root = fileURLToPath(new URL('..', import.meta.url))

const assertFns = (surface, names) => {
  for (const name of names) {
    assert.equal(typeof surface[name], 'function', `expected openclaw connector to export ${name}() as a function`)
  }
}

test('@openclaw/imessage is pinned to the same release as openclaw', async () => {
  // @openclaw/imessage peers on openclaw >= its own version and imports
  // openclaw/plugin-sdk/* at runtime, so the two must move together.
  const pkg = JSON.parse(await readFile(path.join(root, 'package.json'), 'utf8'))
  assert.ok(pkg.dependencies?.openclaw, 'openclaw must be a direct dependency')
  assert.equal(pkg.dependencies['@openclaw/imessage'], pkg.dependencies.openclaw)
})

test('resolveChunkExport finds a mangled-alias export in an .mjs chunk', async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), 'openclaw-chunks-'))
  t.after(() => rm(dir, { recursive: true, force: true }))
  await mkdir(path.join(dir, 'nested'))
  await writeFile(path.join(dir, 'other-AbC123.mjs'), 'const x = 1\nexport { x as t }\n')
  await writeFile(
    path.join(dir, 'client-XyZ789.mjs'),
    'function createWidgetClient() { return "ok" }\nexport { createWidgetClient as n }\n',
  )
  const fn = await resolveChunkExport(dir, 'createWidgetClient', 'client-')
  assert.equal(fn(), 'ok')
  await assert.rejects(resolveChunkExport(dir, 'missingSymbol', 'client-'), /could not resolve export missingSymbol/)
})

test('loadIMessageConnector resolves every function the iMessage bridge calls', async () => {
  const oc = await loadIMessageConnector()
  assertFns(oc, [
    'sendMessageIMessage',
    'probeIMessage',
    'createIMessageRpcClient', // resolved via dist chunk scan — the fragile seam
    'resolveIMessageInboundConversationId',
    'normalizeIMessageHandle',
    'formatIMessageChatTarget',
  ])
})

test('the resolved iMessage RPC client round-trips against fake imsg and settles waitForClose on stop', async (t) => {
  const fakeDir = await mkdtemp(path.join(os.tmpdir(), 'fake-imsg-'))
  const saved = { PATH: process.env.PATH, FAKE_IMSG_DIR: process.env.FAKE_IMSG_DIR }
  t.after(async () => {
    process.env.PATH = saved.PATH
    if (saved.FAKE_IMSG_DIR === undefined) delete process.env.FAKE_IMSG_DIR
    else process.env.FAKE_IMSG_DIR = saved.FAKE_IMSG_DIR
    await rm(fakeDir, { recursive: true, force: true })
  })
  // fake-imsg is a `#!/usr/bin/env node` script; Bazel's test PATH need not
  // contain node, so put the running interpreter first.
  process.env.PATH = `${path.dirname(process.execPath)}${path.delimiter}${saved.PATH ?? ''}`
  process.env.FAKE_IMSG_DIR = fakeDir

  const oc = await loadIMessageConnector()
  const client = await oc.createIMessageRpcClient({
    cliPath: path.join(root, 'fake-imsg', 'imsg'),
    onNotification: () => {},
  })
  if (typeof client.start === 'function') await client.start()
  const subscribed = await client.request('watch.subscribe', { attachments: false }, { timeoutMs: 10000 })
  assert.ok(subscribed && typeof subscribed === 'object', 'watch.subscribe returns a result object')

  // bridge.mjs treats waitForClose() settling (resolve OR reject — openclaw
  // 2026.8.x rejects with the terminal close error) as "daemon gone".
  const closed = client.waitForClose().then(
    () => 'resolved',
    () => 'rejected',
  )
  await client.stop()
  assert.match(await closed, /^(resolved|rejected)$/)
})

test('loadTelegramConnector resolves every function the Telegram bridge calls', async () => {
  const oc = await loadTelegramConnector()
  assertFns(oc, ['sendMessageTelegram', 'probeTelegram', 'createForumTopicTelegram'])
})
