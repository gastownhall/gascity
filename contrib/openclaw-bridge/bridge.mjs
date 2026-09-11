#!/usr/bin/env node
// Gas City extmsg out-of-process adapter hosting the openclaw iMessage
// connector (PoC).
//
// Wire contract (gc side, see internal/extmsg/http_adapter.go):
//   gc -> bridge   POST {callback_url}/publish        PublishRequest (snake_case)
//   bridge -> gc   POST /v0/city/{city}/extmsg/inbound  pre-normalized message
//   bridge -> gc   POST /v0/city/{city}/extmsg/adapters register (in-memory; re-register periodically)
//
// Connector side: openclaw's shipped dist code does the platform work —
// probeIMessage (handshake), createIMessageRpcClient (JSON-RPC daemon +
// watch notifications), sendMessageIMessage (outbound pipeline), and the
// iMessage conversation-id model. Routing policy stays in gc.

import { fileURLToPath } from 'node:url'
import { loadIMessageConnector } from './lib/openclaw.mjs'
import { env, makeGcClient, startCallbackServer, makeAdapterRegistrar, makeShutdown } from './lib/gc-client.mjs'
import { forwardWithRetry } from './lib/inbound.mjs'

const CITY = process.env.GC_CITY
if (!CITY) {
  console.error('[bridge] GC_CITY is required (gas city name for /v0/city/{name}/... routes)')
  process.exit(2)
}
const GC_BASE = env('GC_BASE_URL', 'http://127.0.0.1:8372') // gc supervisor default port
const SCOPE = env('GC_SCOPE_ID', CITY)
const PROVIDER = env('BRIDGE_PROVIDER', 'imessage')
const ACCOUNT = env('BRIDGE_ACCOUNT_ID', 'default')
const PORT = Number(env('BRIDGE_PORT', '8930'))
const CLI = env('IMSG_CLI_PATH', fileURLToPath(new URL('./fake-imsg/imsg', import.meta.url)))

const log = (...args) => console.log('[bridge]', ...args)

// Minimal OpenClawConfig literal — the only config the connector code needs.
const ocConfig = {
  channels: { imessage: { enabled: true, cliPath: CLI, service: 'auto', region: 'US' } },
}

// gc HTTP client (see lib/gc-client.mjs).
const { gcFetch } = makeGcClient({ baseUrl: GC_BASE, city: CITY })

const oc = await loadIMessageConnector()

// Mechanical inbound gating at the bridge edge (the one piece of openclaw's
// dmPolicy worth keeping, as config): ALLOW_FROM is a comma-separated list
// of iMessage handles (phone numbers / emails). Non-matching senders are
// dropped with a log line and never reach gc. Unset/empty preserves
// allow-all for demos. Entries and senders are compared in openclaw's
// normalized handle form, so "+1 (555) 010-0001" matches "+15550100001".
const ALLOW_FROM = new Set(
  env('ALLOW_FROM', '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
    .map((h) => (oc.normalizeIMessageHandle(h) || h).toLowerCase()),
)
const senderAllowed = (sender) =>
  ALLOW_FROM.size === 0 || ALLOW_FROM.has((oc.normalizeIMessageHandle(sender) || sender).toLowerCase())

// 1. Handshake with the imsg CLI exactly like openclaw's gateway would.
const probe = await oc.probeIMessage(15000, { cliPath: CLI })
if (!probe || probe.ok !== true) {
  console.error('[bridge] imsg probe failed:', JSON.stringify(probe))
  process.exit(1)
}
log(`imsg probe ok via ${CLI}`)

// 2. Persistent JSON-RPC client; watch notifications become gc inbound posts.
//    Forwards are serialized through one promise chain so transcript order
//    matches platform order. Each forward runs through the shared
//    forwardWithRetry policy (lib/inbound.mjs) — the same retry/drop classifier
//    Telegram uses — so a permanently-rejected (non-429 4xx) message is dropped
//    fast instead of stalling the chain. Unlike Telegram, the iMessage watch
//    stream has NO redelivery cursor: once the daemon hands us a notification it
//    will not resend it, so the bridge is the sole owner until gc accepts it. We
//    therefore retry transient gc failures unboundedly while the process is live
//    (maxAttempts: Infinity); the serialized chain holds later notifications
//    behind a stuck forward instead of dropping them, preserving order. The only
//    unavoidable loss is shutdown interrupting an in-flight forward — there is no
//    source to redeliver from — so we count those and fail the shutdown loudly
//    (non-zero exit) rather than claiming a clean stop.
let shuttingDown = false
let inboundChain = Promise.resolve()
let undeliveredOnShutdown = 0
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const client = await oc.createIMessageRpcClient({
  cliPath: CLI,
  onNotification: (msg) => {
    if (msg?.method !== 'message') return
    const params = msg.params
    const m = params?.message
    // forwardWithRetry logs update?.update_id; iMessage params carry none, so
    // surface the message guid/id for debuggable drop logs while leaving the
    // body onInbound reads (params.message) intact.
    const update = m && typeof m === 'object' ? { ...params, update_id: m.guid ?? m.id } : params
    inboundChain = inboundChain.then(async () => {
      const handled = await forwardWithRetry({
        update,
        deliver: onInbound,
        maxAttempts: Infinity, // no watch-stream cursor: retry until gc accepts, never drop on a transient outage
        isShuttingDown: () => shuttingDown,
        sleep,
        log,
      })
      // false here means shutdown interrupted the retry before gc accepted the
      // message; with no source redelivery that notification is lost.
      if (!handled) undeliveredOnShutdown += 1
    })
  },
})
if (typeof client.start === 'function') await client.start() // idempotent

// The connector's RPC client fails permanently when the imsg daemon dies;
// exit loudly instead of staying registered with gc as a zombie adapter.
client.waitForClose?.().then(() => {
  if (!shuttingDown) {
    console.error('[bridge] imsg rpc daemon exited unexpectedly')
    process.exit(1)
  }
})

async function onInbound(params) {
  const m = params?.message
  if (!m || typeof m !== 'object') return
  if (m.is_from_me === true) return
  if (m.is_reaction === true || m.is_tapback === true) {
    log(`skipping reaction/tapback ${m.guid ?? ''}`)
    return
  }
  const sender = typeof m.sender === 'string' ? m.sender : ''
  const text = typeof m.text === 'string' ? m.text : ''
  if (!sender || !text) return
  if (!senderAllowed(sender)) {
    log(`dropping inbound from unallowed sender ${sender}`)
    return
  }
  const isGroup = m.is_group === true
  const conversationId = oc.resolveIMessageInboundConversationId({
    isGroup,
    sender,
    chatId: typeof m.chat_id === 'number' ? m.chat_id : undefined,
  })
  if (!conversationId) return

  const conversation = {
    scope_id: SCOPE,
    provider: PROVIDER,
    account_id: ACCOUNT,
    conversation_id: conversationId,
    kind: isGroup ? 'room' : 'dm',
  }
  const message = {
    provider_message_id: typeof m.guid === 'string' && m.guid !== '' ? m.guid : String(m.id ?? ''),
    conversation,
    actor: { id: oc.normalizeIMessageHandle(sender) || sender, display_name: sender, is_bot: false },
    text,
    received_at: m.created_at ? new Date(m.created_at).toISOString() : new Date().toISOString(),
    ...(m.reply_to_id != null ? { reply_to_message_id: String(m.reply_to_id) } : {}),
  }
  const result = await gcFetch('POST', '/extmsg/inbound', { message })
  log(`inbound ${conversationId}: ${JSON.stringify(text)} -> session ${result?.TargetSessionID || '(unbound)'}`)
}

// 3. HTTP callback server for gc -> bridge publishes (started below, before
//    registration, so gc never publishes before the bridge can receive).
//    GET /healthz is handled by the shared callback server (lib/gc-client.mjs).
async function handleRequest(req, rawBody) {
  if (req.method === 'POST' && req.url === '/publish') {
    return { status: 200, body: await handlePublish(JSON.parse(rawBody)) }
  }
  if (req.method === 'POST' && req.url === '/child-conversation') {
    return { status: 404, body: { error: 'child conversations unsupported' } }
  }
  return { status: 404, body: { error: 'not found' } }
}

// Maps a gc PublishRequest onto the connector's send pipeline and returns the
// snake_case wire receipt gc expects (empty/invalid body counts as undelivered).
// Known PoC simplification: every failure is reported as failure_kind
// "transient", including permanently-bad targets.
async function handlePublish(pub) {
  const conv = pub?.conversation ?? {}
  const convID = String(conv.conversation_id ?? '')
  // All-digit ids are chat.db ROWIDs (group chats); anything else (handles,
  // chat_guid:/chat_identifier: forms) is parsed by the connector itself.
  const target = /^\d+$/.test(convID) ? oc.formatIMessageChatTarget(Number(convID)) : convID
  try {
    const result = await oc.sendMessageIMessage(target, pub?.text ?? '', {
      config: ocConfig,
      client, // reuse the persistent RPC client; send.ts leaves caller-owned clients open
      replyToId: pub?.reply_to_message_id || undefined,
      timeoutMs: 20000,
    })
    log(`publish -> ${target}: ${JSON.stringify(pub?.text ?? '')} (message_id=${result.messageId})`)
    // Coerce openclaw's return values to strings at the edge, like
    // provider_message_id elsewhere: gc's wirePublishReceipt decodes
    // message_id and metadata as strings, so a future numeric openclaw return
    // would otherwise flip a delivered message to a malformed (transient) receipt.
    return {
      message_id: String(result.messageId ?? ''),
      conversation: conv,
      delivered: true,
      ...(result.guid ? { metadata: { guid: String(result.guid) } } : {}),
    }
  } catch (err) {
    log(`publish -> ${target} FAILED: ${err?.message ?? err}`)
    return {
      conversation: conv,
      delivered: false,
      failure_kind: 'transient',
      metadata: { error: String(err?.message ?? err) },
    }
  }
}

const server = await startCallbackServer({ handleRequest, port: PORT })
log(`callback server on http://127.0.0.1:${PORT}`)

const subscribed = await client.request(
  'watch.subscribe',
  { attachments: false, include_reactions: true },
  { timeoutMs: 10000 },
)
const subscriptionID = subscribed?.subscription ?? 1
log('subscribed to imsg watch notifications')

// 4. Register with gc last, so gc never publishes before we can send. The
//    registry is in-memory on the gc side, so re-register on an interval to
//    survive controller restarts. PascalCase capabilities are correct here:
//    extmsg.AdapterCapabilities is intentionally untagged on the gc side
//    (internal/extmsg/types.go) while the rest of this request body is snake_case.
const registrar = makeAdapterRegistrar({
  gcFetch,
  baseUrl: GC_BASE,
  provider: PROVIDER,
  account: ACCOUNT,
  name: 'openclaw-imessage-bridge',
  callbackUrl: `http://127.0.0.1:${PORT}`,
  capabilities: { SupportsChildConversations: false, SupportsAttachments: false, MaxMessageLength: 0 },
  log,
})
await registrar.registerWithRetry()
log(`registered adapter provider=${PROVIDER} account=${ACCOUNT} city=${CITY}`)
const reregister = registrar.startReregister()

const shutdown = makeShutdown({
  log,
  reregister,
  unregister: registrar.unregister,
  server,
  teardown: async () => {
    shuttingDown = true
    // Let the in-flight inbound forward settle before we exit, mirroring the
    // Telegram bridge's `await pollDone`. forwardWithRetry breaks its retry loop
    // once shuttingDown is true, so this only waits out the current gcFetch
    // (bounded by its 15s timeout), not a fresh round of retries.
    await inboundChain.catch(() => {})
    if (undeliveredOnShutdown > 0) {
      console.error(
        `[bridge] ${undeliveredOnShutdown} inbound message(s) were not delivered to gc before shutdown; ` +
          'the iMessage watch stream has no redelivery cursor, so they are lost',
      )
    }
    try {
      await client.request('watch.unsubscribe', { subscription: subscriptionID }, { timeoutMs: 2000 })
    } catch {
      // daemon may already be gone
    }
    await client.stop().catch(() => {})
  },
  // Refuse a clean exit if an already-consumed notification was lost on shutdown.
  exitCode: () => (undeliveredOnShutdown > 0 ? 1 : 0),
})
process.on('SIGINT', shutdown)
process.on('SIGTERM', shutdown)
log('ready')
