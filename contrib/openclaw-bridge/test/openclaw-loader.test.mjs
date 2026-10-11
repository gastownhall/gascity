// Smoke coverage for the openclaw connector loader (lib/openclaw.mjs). The other
// suites import only lib/inbound.mjs + lib/gc-client.mjs, so nothing exercised
// the part of the bridge that actually reaches into the installed `openclaw`
// package — including resolveChunkExport, which scans openclaw's hash-named dist
// chunks for createIMessageRpcClient and is the most fragile seam across openclaw
// releases. This test loads both connector surfaces after `npm ci` and asserts
// every function the bridges call resolves, WITHOUT starting any network daemon
// (loadIMessage/TelegramConnector only import modules and resolve references).
//
// Run: npm test  (or: node --test test/)

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises'
import os from 'node:os'
import path from 'node:path'
import { loadIMessageConnector, loadTelegramConnector, resolveChunkExport } from '../lib/openclaw.mjs'

const assertFns = (surface, names) => {
  for (const name of names) {
    assert.equal(typeof surface[name], 'function', `expected openclaw connector to export ${name}() as a function`)
  }
}

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

test('loadTelegramConnector resolves every function the Telegram bridge calls', async () => {
  const oc = await loadTelegramConnector()
  assertFns(oc, ['sendMessageTelegram', 'probeTelegram', 'createForumTopicTelegram'])
})
