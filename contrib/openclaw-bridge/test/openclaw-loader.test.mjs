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
import { loadIMessageConnector, loadTelegramConnector } from '../lib/openclaw.mjs'

const assertFns = (surface, names) => {
  for (const name of names) {
    assert.equal(typeof surface[name], 'function', `expected openclaw connector to export ${name}() as a function`)
  }
}

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
