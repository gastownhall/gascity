// Smoke coverage for the openclaw connector loader (lib/openclaw.mjs). The other
// suites import only lib/inbound.mjs + lib/gc-client.mjs, so nothing exercised
// the part of the bridge that actually reaches into the installed openclaw
// packages — including resolveChunkExport, which scans @openclaw/imessage's
// hash-named dist chunks for createIMessageRpcClient and sendMessageIMessage and
// is the most fragile seam across openclaw releases. This test loads both connector surfaces after
// `npm ci` and asserts every function the bridges call resolves, then drives the
// resolved RPC client to pin the close contract bridge.mjs relies on
// (watchDaemonClose): its own stop() during shutdown is not an unexpected close,
// and an imsg daemon that dies under it is, with the close reason.
//
// Run: npm test  (or: node --test test/)

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  loadIMessageConnector,
  loadTelegramConnector,
  resolveChunkExport,
  watchDaemonClose,
} from '../lib/openclaw.mjs'

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

test('resolveChunkExport finds a mangled-alias export in an .mjs chunk, past chunks that only import it', async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), 'openclaw-chunks-'))
  t.after(() => rm(dir, { recursive: true, force: true }))
  await mkdir(path.join(dir, 'nested'))
  // The 2026.8.35 shape: no chunk matches the hint, and chunks that sort
  // before the defining one import the symbol without exporting it.
  await writeFile(
    path.join(dir, 'actions-AbC123.mjs'),
    'import { n as createWidgetClient } from "./sanitize-XyZ789.mjs"\nconst use = () => createWidgetClient()\nexport { use as t }\n',
  )
  await writeFile(
    path.join(dir, 'sanitize-XyZ789.mjs'),
    'function createWidgetClient() { return "ok" }\nexport { createWidgetClient as n }\n',
  )
  const fn = await resolveChunkExport(dir, 'createWidgetClient', 'client-')
  assert.equal(fn(), 'ok')
  await assert.rejects(resolveChunkExport(dir, 'missingSymbol', 'client-'), /could not resolve export missingSymbol/)
})

test('loadIMessageConnector resolves every function the iMessage bridge calls', async () => {
  const oc = await loadIMessageConnector()
  assertFns(oc, [
    'sendMessageIMessage', // resolved via dist chunk scan since 2026.10
    'probeIMessage',
    'createIMessageRpcClient', // resolved via dist chunk scan — the fragile seam
    'resolveIMessageInboundConversationId',
    'normalizeIMessageHandle',
    'formatIMessageChatTarget',
  ])
})

test('watchDaemonClose: either waitForClose outcome means "daemon gone", unexpected only outside shutdown', async () => {
  const closeErr = new Error('imsg rpc closed')
  const rejects = { waitForClose: () => Promise.reject(closeErr) } // openclaw >= 2026.8
  const resolves = { waitForClose: () => Promise.resolve() } // older openclaw
  for (const [client, shuttingDown, want] of [
    [rejects, false, [closeErr]],
    [resolves, false, [undefined]],
    [rejects, true, []],
    [resolves, true, []],
  ]) {
    const got = []
    // Awaiting proves the watch itself never rejects: bridge.mjs does not
    // handle its result, so a rejection there would crash the bridge.
    await watchDaemonClose(client, {
      isShuttingDown: () => shuttingDown,
      onUnexpectedClose: (err) => got.push(err),
    })
    const outcome = client === rejects ? 'rejecting' : 'resolving'
    assert.deepEqual(got, want, `${outcome} waitForClose with shuttingDown=${shuttingDown}`)
  }
})

test('the resolved RPC client round-trips against fake imsg, and its stop() in shutdown is not an unexpected close', async (t) => {
  const fakeDir = await mkdtemp(path.join(os.tmpdir(), 'fake-imsg-'))
  const saved = { PATH: process.env.PATH, FAKE_IMSG_DIR: process.env.FAKE_IMSG_DIR }
  let client
  t.after(async () => {
    // Stop first: a failed assertion must not leave the fake daemon holding
    // the test process open until the runner times out.
    await client?.stop()
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
  client = await oc.createIMessageRpcClient({
    cliPath: path.join(root, 'fake-imsg', 'imsg'),
    onNotification: () => {},
  })
  if (typeof client.start === 'function') await client.start()
  const subscribed = await client.request('watch.subscribe', { attachments: false }, { timeoutMs: 10000 })
  assert.ok(subscribed && typeof subscribed === 'object', 'watch.subscribe returns a result object')

  // The bridge.mjs sequence: watch from startup, flag shutdown, stop the client.
  let shuttingDown = false
  const unexpected = []
  const watched = watchDaemonClose(client, {
    isShuttingDown: () => shuttingDown,
    onUnexpectedClose: (err) => unexpected.push(err),
  })
  shuttingDown = true
  await client.stop()
  await watched
  assert.deepEqual(unexpected, [])
})

test('watchDaemonClose reports an imsg rpc daemon that dies under the running bridge, with the close reason', async (t) => {
  const oc = await loadIMessageConnector()
  // `false` stands in for an imsg daemon that exits on its own (status 1).
  const client = await oc.createIMessageRpcClient({ cliPath: 'false', onNotification: () => {} })
  t.after(() => client.stop())
  const unexpected = []
  await watchDaemonClose(client, {
    isShuttingDown: () => false,
    onUnexpectedClose: (err) => unexpected.push(err),
  })
  assert.equal(unexpected.length, 1, 'the daemon exit is reported exactly once')
  assert.ok(unexpected[0] instanceof Error, 'waitForClose() passes the close reason through')
  assert.match(unexpected[0].message, /code 1/)
})

test('loadTelegramConnector resolves every function the Telegram bridge calls', async () => {
  const oc = await loadTelegramConnector()
  assertFns(oc, ['sendMessageTelegram', 'probeTelegram', 'createForumTopicTelegram'])
})
