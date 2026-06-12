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

import http from 'node:http'
import { fileURLToPath } from 'node:url'
import { loadIMessageConnector } from './lib/openclaw.mjs'

const env = (k, d) => (process.env[k] !== undefined && process.env[k] !== '' ? process.env[k] : d)

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

async function gcFetch(method, p, body) {
  const res = await fetch(`${GC_BASE}/v0/city/${encodeURIComponent(CITY)}${p}`, {
    method,
    headers: { 'Content-Type': 'application/json', 'X-GC-Request': '1' },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(15000), // a wedged gc must not wedge the bridge (esp. shutdown)
  })
  const text = await res.text()
  if (!res.ok) throw new Error(`${method} ${p}: HTTP ${res.status}: ${text.slice(0, 300)}`)
  return text ? JSON.parse(text) : null
}

const oc = await loadIMessageConnector()

// 1. Handshake with the imsg CLI exactly like openclaw's gateway would.
const probe = await oc.probeIMessage(15000, { cliPath: CLI })
if (!probe || probe.ok !== true) {
  console.error('[bridge] imsg probe failed:', JSON.stringify(probe))
  process.exit(1)
}
log(`imsg probe ok via ${CLI}`)

// 2. Persistent JSON-RPC client; watch notifications become gc inbound posts.
//    Forwards are serialized through one promise chain so transcript order
//    matches platform order, with a few retries so a gc restart doesn't drop
//    a message the daemon has already moved past.
let shuttingDown = false
let inboundChain = Promise.resolve()
const client = await oc.createIMessageRpcClient({
  cliPath: CLI,
  onNotification: (msg) => {
    if (msg?.method !== 'message') return
    inboundChain = inboundChain.then(async () => {
      for (let attempt = 1; ; attempt++) {
        try {
          await onInbound(msg.params)
          return
        } catch (err) {
          if (attempt >= 5) {
            log('inbound forward dropped after retries:', err.message)
            return
          }
          await new Promise((r) => setTimeout(r, attempt * 2000))
        }
      }
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
    ...(typeof m.guid === 'string' && m.guid !== '' ? { dedup_key: m.guid } : {}),
  }
  const result = await gcFetch('POST', '/extmsg/inbound', { message })
  log(`inbound ${conversationId}: ${JSON.stringify(text)} -> session ${result?.TargetSessionID || '(unbound)'}`)
}

// 3. HTTP callback server for gc -> bridge publishes.
const server = http.createServer((req, res) => {
  const chunks = []
  req.on('data', (c) => chunks.push(c))
  req.on('end', () => {
    handleRequest(req, Buffer.concat(chunks).toString('utf8'))
      .then(({ status, body }) => {
        res.writeHead(status, { 'Content-Type': 'application/json' })
        res.end(JSON.stringify(body))
      })
      .catch((err) => {
        res.writeHead(500, { 'Content-Type': 'application/json' })
        res.end(JSON.stringify({ error: String(err?.message ?? err) }))
      })
  })
})

async function handleRequest(req, rawBody) {
  if (req.method === 'GET' && req.url === '/healthz') {
    return { status: 200, body: { ok: true } }
  }
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
    return {
      message_id: result.messageId,
      conversation: conv,
      delivered: true,
      ...(result.guid ? { metadata: { guid: result.guid } } : {}),
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

await new Promise((resolve, reject) => {
  server.once('error', reject)
  server.listen(PORT, '127.0.0.1', resolve)
})
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
//    survive controller restarts.
async function register() {
  await gcFetch('POST', '/extmsg/adapters', {
    provider: PROVIDER,
    account_id: ACCOUNT,
    name: 'openclaw-imessage-bridge',
    callback_url: `http://127.0.0.1:${PORT}`,
    // PascalCase is correct here: extmsg.AdapterCapabilities is intentionally
    // untagged on the gc side (internal/extmsg/types.go) while the rest of
    // this request body is snake_case.
    capabilities: { SupportsChildConversations: false, SupportsAttachments: false, MaxMessageLength: 0 },
  })
}

{
  let attempts = 0
  for (;;) {
    try {
      await register()
      break
    } catch (err) {
      attempts += 1
      if (attempts >= 60) throw err
      if (attempts === 1) log(`waiting for gc at ${GC_BASE} (${err.message})`)
      await new Promise((r) => setTimeout(r, 1000))
    }
  }
}
log(`registered adapter provider=${PROVIDER} account=${ACCOUNT} city=${CITY}`)
const reregister = setInterval(() => register().catch((err) => log('re-register failed:', err.message)), 30000)

async function shutdown() {
  shuttingDown = true
  log('shutting down')
  clearInterval(reregister)
  try {
    await client.request('watch.unsubscribe', { subscription: subscriptionID }, { timeoutMs: 2000 })
  } catch {
    // daemon may already be gone
  }
  try {
    await gcFetch('DELETE', '/extmsg/adapters', { provider: PROVIDER, account_id: ACCOUNT })
  } catch {
    // gc may already be gone
  }
  await client.stop().catch(() => {})
  server.close()
  process.exit(0)
}
process.on('SIGINT', shutdown)
process.on('SIGTERM', shutdown)
log('ready')
