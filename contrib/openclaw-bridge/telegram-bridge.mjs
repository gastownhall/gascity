#!/usr/bin/env node
// Gas City extmsg out-of-process adapter hosting the openclaw Telegram
// connector's outbound pipeline (PoC).
//
// Wire contract (gc side, see internal/extmsg/http_adapter.go):
//   gc -> bridge   POST {callback_url}/publish        PublishRequest (snake_case)
//   bridge -> gc   POST /v0/city/{city}/extmsg/inbound  pre-normalized message
//   bridge -> gc   POST /v0/city/{city}/extmsg/adapters register (in-memory; re-register periodically)
//
// Connector side: openclaw's shipped dist code does the outbound platform
// work — probeTelegram (getMe handshake) and sendMessageTelegram (markdown ->
// Telegram HTML, chunking, retries). Inbound is the bridge's own getUpdates
// long-poll: openclaw's telegram inbound only exists inside
// monitorTelegramProvider, their pairing/dispatch layer, which is exactly the
// layer gc replaces. Telegram's inbound protocol is small enough to own.
// Routing policy stays in gc.

import { loadTelegramConnector } from './lib/openclaw.mjs'
import { env, makeGcClient, startCallbackServer, makeAdapterRegistrar, makeShutdown } from './lib/gc-client.mjs'
import { forwardWithRetry, drainBatch } from './lib/inbound.mjs'

const CITY = process.env.GC_CITY
if (!CITY) {
  console.error('[tg-bridge] GC_CITY is required (gas city name for /v0/city/{name}/... routes)')
  process.exit(2)
}
const TOKEN = process.env.TELEGRAM_BOT_TOKEN
if (!TOKEN) {
  console.error('[tg-bridge] TELEGRAM_BOT_TOKEN is required (BotFather token, or the fake server token)')
  process.exit(2)
}
const GC_BASE = env('GC_BASE_URL', 'http://127.0.0.1:8372') // gc supervisor default port
const SCOPE = env('GC_SCOPE_ID', CITY)
const PROVIDER = env('BRIDGE_PROVIDER', 'telegram')
const ACCOUNT = env('BRIDGE_ACCOUNT_ID', 'default')
const PORT = Number(env('BRIDGE_PORT', '8931'))
const API_ROOT = env('TELEGRAM_API_ROOT', 'https://api.telegram.org').replace(/\/+$/, '')

// Mechanical inbound gating at the bridge edge (the one piece of openclaw's
// dmPolicy worth keeping, as config): ALLOW_FROM is a comma-separated list
// of telegram user ids and/or usernames (with or without @). Non-matching
// senders are dropped with a log line and never reach gc. Unset/empty
// preserves allow-all for demos.
const ALLOW_FROM = new Set(
  env('ALLOW_FROM', '')
    .split(',')
    .map((s) => s.trim().toLowerCase().replace(/^@/, ''))
    .filter(Boolean),
)
const senderAllowed = (from) =>
  ALLOW_FROM.size === 0 ||
  ALLOW_FROM.has(String(from?.id ?? '').toLowerCase()) ||
  ALLOW_FROM.has(String(from?.username ?? '').toLowerCase())

// Telegram's API design puts the token in every URL (/bot<token>/...), so
// transport errors can embed it in their messages. Strip it from anything
// that leaves the process: logs AND the error metadata reported back to gc
// (receipts land in durable transcripts).
const redact = (v) => String(v).split(TOKEN).join('<token>')
const log = (...args) => console.log('[tg-bridge]', ...args.map(redact))
const logError = (...args) => console.error('[tg-bridge]', ...args.map(redact))

// Minimal OpenClawConfig literal — the only config the connector code needs.
// apiRoot flows into the grammY client, so the same bridge runs against the
// real Bot API or the local fake.
const ocConfig = {
  channels: { telegram: { enabled: true, botToken: TOKEN, apiRoot: API_ROOT } },
}

// gc HTTP client (see lib/gc-client.mjs): gcFetch attaches err.status on non-2xx
// so inbound forwarding can tell a transient gc outage from a permanent reject.
const { gcFetch } = makeGcClient({ baseUrl: GC_BASE, city: CITY })

const oc = await loadTelegramConnector()

// 1. Handshake against the Bot API exactly like openclaw's gateway would.
const probe = await oc.probeTelegram(TOKEN, 15000, { apiRoot: API_ROOT })
if (!probe || probe.ok !== true) {
  logError('telegram probe failed:', JSON.stringify(probe))
  process.exit(1)
}
log(`telegram probe ok: @${probe.bot?.username ?? '?'} via ${API_ROOT}`)

// 2. Inbound: getUpdates long-poll. Each update is forwarded to gc (with a few
//    retries) BEFORE the poll offset advances past it. Telegram acknowledges
//    updates server-side as soon as the next getUpdates sends a higher offset,
//    so advancing before a successful forward is exactly what would let a gc
//    outage silently drop an inbound message. Forwarding inline also keeps
//    transcript order matching platform order; gc dedupes on
//    (conversation, provider_message_id), so a redelivered update after a crash
//    is harmless.
let shuttingDown = false
let pollAbort = null
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// Forward one update to gc before the poll offset advances past it. Transient
// failures (gc outage, 5xx, 429, network/timeout) are retried and then left
// un-acked for Telegram to redeliver; a permanent non-429 4xx is dropped past
// the offset so one poison update cannot wedge the whole inbound stream.
// Intentional skips (bot/unallowed sender/non-text) resolve without a POST and
// advance the offset normally. (See lib/inbound.mjs for the durability model.)
const forwardInbound = (update) =>
  forwardWithRetry({ update, deliver: onInbound, isShuttingDown: () => shuttingDown, sleep, log })

async function pollUpdates() {
  let offset = 0
  let failures = 0
  while (!shuttingDown) {
    try {
      pollAbort = new AbortController()
      const timer = setTimeout(() => pollAbort.abort(), 40000) // poll timeout 25s + slack
      const res = await fetch(`${API_ROOT}/bot${TOKEN}/getUpdates`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ offset, timeout: 25, allowed_updates: ['message'] }),
        signal: pollAbort.signal,
      }).finally(() => clearTimeout(timer))
      const data = await res.json()
      if (data.ok !== true) throw new Error(`getUpdates: ${JSON.stringify(data).slice(0, 200)}`)
      failures = 0
      // Forward each update before advancing the offset. drainBatch stops at the
      // first transient failure and holds the offset there, so Telegram
      // redelivers from that update on the next poll instead of losing it.
      offset = await drainBatch({
        batch: data.result ?? [],
        offset,
        forward: forwardInbound,
        isShuttingDown: () => shuttingDown,
      })
    } catch (err) {
      if (shuttingDown) return
      failures += 1
      // The Bot API daemon being unreachable is fatal after sustained failure;
      // exit loudly instead of staying registered with gc as a zombie adapter.
      if (failures >= 10) {
        logError('getUpdates failing persistently:', err.message)
        process.exit(1)
      }
      log(`getUpdates failed (${failures}/10):`, err.message)
      await new Promise((r) => setTimeout(r, Math.min(failures * 2000, 10000)))
    }
  }
}

// Child conversations are Telegram forum topics. The child conversation_id
// encodes both halves of the platform address — "<chat_id>:topic:<thread_id>"
// — so publish and inbound can recover (chat, message_thread_id) without any
// bridge-side state surviving restarts.
const childConversationId = (chatId, topicId) => `${chatId}:topic:${topicId}`
const parseChildConversationId = (conversationId) => {
  const m = /^(-?\d+):topic:(\d+)$/.exec(String(conversationId ?? ''))
  return m ? { chatId: m[1], topicId: Number(m[2]) } : null
}

// Map a createForumTopicTelegram failure to the status gc should see. Structural
// rejections (parent is not a forum supergroup, etc.) come back from the Bot API
// as a 4xx and are permanent — gc must not retry them. Only a positive transient
// signal upgrades to 503: a Bot API 5xx, or a transport abort/timeout. Anything
// else (including an error whose shape we cannot read) stays 400, so a permanent
// structural rejection is never mislabeled retryable.
const childConversationFailureStatus = (err) => {
  const code = Number(err?.error_code ?? err?.status ?? err?.statusCode)
  if (Number.isInteger(code) && code >= 500) return 503
  const name = String(err?.name ?? '')
  if (name === 'AbortError' || name === 'TimeoutError') return 503
  return 400
}

async function onInbound(update) {
  const m = update?.message
  if (!m || typeof m !== 'object') return
  if (m.from?.is_bot === true) return // never loop on bot traffic (incl. our own)
  if (!senderAllowed(m.from)) {
    log(`dropping inbound from unallowed sender ${m.from?.id ?? '?'} (@${m.from?.username ?? '?'})`)
    return
  }
  const text = typeof m.text === 'string' ? m.text : ''
  if (!text) {
    log(`skipping non-text update ${update.update_id} (media has no gc representation yet)`)
    return
  }
  const chat = m.chat ?? {}
  if (chat.id == null) return
  const isDm = chat.type === 'private'
  const topicId = m.is_topic_message === true && m.message_thread_id != null ? Number(m.message_thread_id) : null

  const conversation = {
    scope_id: SCOPE,
    provider: PROVIDER,
    account_id: ACCOUNT,
    // Telegram chat ids are the canonical conversation key; forum-topic
    // messages address the child conversation instead, parented on the chat.
    ...(topicId != null
      ? {
          conversation_id: childConversationId(chat.id, topicId),
          parent_conversation_id: String(chat.id),
          kind: 'thread',
        }
      : {
          conversation_id: String(chat.id),
          kind: isDm ? 'dm' : 'room',
        }),
  }
  const from = m.from ?? {}
  const message = {
    // message_id is chat-scoped in Telegram; keep it raw so replies map back. gc
    // dedupes inbound on (conversation, provider_message_id), and the
    // conversation_id is itself chat- (or chat:topic-) scoped, so the pair is
    // globally unique without a separate dedup key.
    provider_message_id: String(m.message_id),
    conversation,
    actor: {
      id: String(from.id ?? ''),
      display_name: from.username || from.first_name || String(from.id ?? ''),
      is_bot: false,
    },
    text,
    received_at: m.date ? new Date(m.date * 1000).toISOString() : new Date().toISOString(),
    ...(m.reply_to_message?.message_id != null
      ? { reply_to_message_id: String(m.reply_to_message.message_id) }
      : {}),
  }
  const result = await gcFetch('POST', '/extmsg/inbound', { message })
  log(`inbound ${conversation.conversation_id}: ${JSON.stringify(text)} -> session ${result?.TargetSessionID || '(unbound)'}`)
}

// 3. HTTP callback server for gc -> bridge publishes (started below, before
//    registration, so gc never publishes before the bridge can receive).
//    GET /healthz is handled by the shared callback server (lib/gc-client.mjs).
async function handleRequest(req, rawBody) {
  if (req.method === 'POST' && req.url === '/publish') {
    return { status: 200, body: await handlePublish(JSON.parse(rawBody)) }
  }
  if (req.method === 'POST' && req.url === '/child-conversation') {
    return handleChildConversation(JSON.parse(rawBody))
  }
  return { status: 404, body: { error: 'not found' } }
}

// Implements gc's EnsureChildConversation contract (internal/extmsg
// http_adapter.go): body {conversation, label}, success reply is the bare
// child ConversationRef. The child is a Telegram forum topic, so the parent
// must be a forum-enabled supergroup; anything else is a clean error.
async function handleChildConversation(body) {
  const parent = body?.conversation ?? {}
  const label = typeof body?.label === 'string' && body.label.trim() !== '' ? body.label.trim() : 'gc workstream'
  if (parseChildConversationId(parent.conversation_id)) {
    return { status: 400, body: { error: 'nested child conversations unsupported (parent is already a forum topic)' } }
  }
  if (parent.kind === 'dm') {
    return { status: 400, body: { error: 'child conversations unsupported for DMs (forum topics need a supergroup)' } }
  }
  const chatId = String(parent.conversation_id ?? '')
  if (!chatId) {
    return { status: 400, body: { error: 'conversation.conversation_id required' } }
  }
  try {
    // Topic names are capped at 128 chars by the Bot API.
    const topic = await oc.createForumTopicTelegram(chatId, label.slice(0, 128), { cfg: ocConfig })
    log(`child-conversation ${chatId}: topic ${topic.topicId} (${JSON.stringify(topic.name)})`)
    return {
      status: 200,
      body: {
        scope_id: parent.scope_id ?? SCOPE,
        provider: PROVIDER,
        account_id: ACCOUNT,
        conversation_id: childConversationId(chatId, topic.topicId),
        parent_conversation_id: chatId,
        kind: 'thread',
      },
    }
  } catch (err) {
    // A transient Bot API/transport failure must look retryable to gc, while a
    // structural rejection (not a forum supergroup, etc.) stays a permanent 400.
    const status = childConversationFailureStatus(err)
    log(`child-conversation ${chatId} FAILED (HTTP ${status}): ${err?.message ?? err}`)
    return { status, body: { error: redact(err?.message ?? err) } }
  }
}

// Maps a gc PublishRequest onto the connector's send pipeline and returns the
// snake_case wire receipt gc expects (empty/invalid body counts as undelivered).
// Known PoC simplification: every failure is reported as failure_kind
// "transient", including permanently-bad targets.
async function handlePublish(pub) {
  const conv = pub?.conversation ?? {}
  // Child conversations carry the forum topic in the conversation id;
  // everything else publishes straight to the chat.
  const child = parseChildConversationId(conv.conversation_id)
  const target = child ? child.chatId : String(conv.conversation_id ?? '')
  const replyTo = Number(pub?.reply_to_message_id)
  try {
    const result = await oc.sendMessageTelegram(target, pub?.text ?? '', {
      cfg: ocConfig,
      ...(child ? { messageThreadId: child.topicId } : {}),
      ...(Number.isInteger(replyTo) && replyTo > 0 ? { replyToMessageId: replyTo } : {}),
    })
    log(`publish -> ${target}${child ? `#${child.topicId}` : ''}: ${JSON.stringify(pub?.text ?? '')} (message_id=${result.messageId})`)
    // Coerce openclaw's return values to strings at the edge, like
    // provider_message_id elsewhere: gc's wirePublishReceipt decodes
    // message_id and metadata as strings, so a future numeric openclaw return
    // would otherwise flip a delivered message to a malformed (transient) receipt.
    return {
      message_id: String(result.messageId ?? ''),
      conversation: conv,
      delivered: true,
      metadata: { chat_id: String(result.chatId ?? '') },
    }
  } catch (err) {
    log(`publish -> ${target} FAILED: ${err?.message ?? err}`)
    return {
      conversation: conv,
      delivered: false,
      failure_kind: 'transient',
      metadata: { error: redact(err?.message ?? err) },
    }
  }
}

const server = await startCallbackServer({ handleRequest, port: PORT })
log(`callback server on http://127.0.0.1:${PORT}`)

const pollDone = pollUpdates()
log('polling telegram getUpdates')

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
  name: 'openclaw-telegram-bridge',
  callbackUrl: `http://127.0.0.1:${PORT}`,
  capabilities: { SupportsChildConversations: true, SupportsAttachments: false, MaxMessageLength: 0 },
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
    pollAbort?.abort()
    await pollDone.catch(() => {})
  },
})
process.on('SIGINT', shutdown)
process.on('SIGTERM', shutdown)
log('ready')
