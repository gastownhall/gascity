// Smoke coverage for the shared gc transport glue (lib/gc-client.mjs) that both
// bridges now depend on. The important contract here is gcFetch's error shape:
// inbound failure classification (lib/inbound.mjs) keys off err.status, so a
// non-2xx must surface the HTTP status and a transport error must not.
//
// Run: npm test  (or: node --test test/)

import { test } from 'node:test'
import assert from 'node:assert/strict'
import http from 'node:http'
import { env, makeGcClient, startCallbackServer, makeAdapterRegistrar } from '../lib/gc-client.mjs'

// listen starts a one-off server on an ephemeral port and resolves { server, port }.
function listen(handler) {
  return new Promise((resolve) => {
    const server = http.createServer(handler)
    server.listen(0, '127.0.0.1', () => resolve({ server, port: server.address().port }))
  })
}
const close = (server) => new Promise((resolve) => server.close(resolve))

test('env returns the value when set and non-empty, else the default', () => {
  process.env.__GC_TEST_A = 'x'
  process.env.__GC_TEST_B = ''
  try {
    assert.equal(env('__GC_TEST_A', 'd'), 'x')
    assert.equal(env('__GC_TEST_B', 'd'), 'd') // empty string falls back to the default
    assert.equal(env('__GC_TEST_MISSING', 'd'), 'd')
  } finally {
    delete process.env.__GC_TEST_A
    delete process.env.__GC_TEST_B
  }
})

test('gcFetch posts to the URL-encoded city path with the CSRF header and parses JSON', async () => {
  const seen = []
  const { server, port } = await listen((req, res) => {
    const chunks = []
    req.on('data', (c) => chunks.push(c))
    req.on('end', () => {
      seen.push({
        method: req.method,
        url: req.url,
        csrf: req.headers['x-gc-request'],
        body: Buffer.concat(chunks).toString('utf8'),
      })
      res.writeHead(200, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify({ ok: true }))
    })
  })
  try {
    const { gcFetch } = makeGcClient({ baseUrl: `http://127.0.0.1:${port}`, city: 'my city' })
    const out = await gcFetch('POST', '/extmsg/inbound', { hello: 'world' })
    assert.deepEqual(out, { ok: true })
    assert.equal(seen.length, 1)
    assert.equal(seen[0].method, 'POST')
    assert.equal(seen[0].url, '/v0/city/my%20city/extmsg/inbound') // city is URL-encoded
    assert.equal(seen[0].csrf, '1')
    assert.deepEqual(JSON.parse(seen[0].body), { hello: 'world' })
  } finally {
    await close(server)
  }
})

test('gcFetch throws with the HTTP status attached so callers can classify failures', async () => {
  const { server, port } = await listen((req, res) => {
    res.writeHead(422, { 'Content-Type': 'application/json' })
    res.end(JSON.stringify({ error: 'normalized inbound rejected' }))
  })
  try {
    const { gcFetch } = makeGcClient({ baseUrl: `http://127.0.0.1:${port}`, city: 'c' })
    await assert.rejects(
      () => gcFetch('POST', '/extmsg/inbound', {}),
      (err) => {
        assert.equal(err.status, 422)
        assert.match(err.message, /HTTP 422/)
        return true
      },
    )
  } finally {
    await close(server)
  }
})

test('gcFetch surfaces transport errors with no numeric status (treated as transient)', async () => {
  // Bind a server, take its port, then close it so the connect is refused.
  const { server, port } = await listen(() => {})
  await close(server)
  const { gcFetch } = makeGcClient({ baseUrl: `http://127.0.0.1:${port}`, city: 'c' })
  await assert.rejects(
    () => gcFetch('POST', '/extmsg/inbound', {}),
    (err) => {
      assert.equal(typeof err.status, 'undefined') // no HTTP status -> inbound classifier holds for redelivery
      return true
    },
  )
})

test('startCallbackServer serves /healthz, routes to handleRequest, 404s unknown paths, 500s a thrown handler', async () => {
  const handleRequest = async (req, rawBody) => {
    if (req.method === 'POST' && req.url === '/publish') {
      return { status: 200, body: { delivered: true, echo: JSON.parse(rawBody) } }
    }
    if (req.method === 'POST' && req.url === '/boom') throw new Error('handler exploded')
    return { status: 404, body: { error: 'not found' } }
  }
  const server = await startCallbackServer({ handleRequest, port: 0 })
  const base = `http://127.0.0.1:${server.address().port}`
  try {
    const health = await fetch(`${base}/healthz`)
    assert.equal(health.status, 200)
    assert.deepEqual(await health.json(), { ok: true })

    const pub = await fetch(`${base}/publish`, { method: 'POST', body: JSON.stringify({ text: 'hi' }) })
    assert.equal(pub.status, 200)
    assert.deepEqual(await pub.json(), { delivered: true, echo: { text: 'hi' } })

    const missing = await fetch(`${base}/nope`, { method: 'POST', body: '{}' })
    assert.equal(missing.status, 404)

    const boom = await fetch(`${base}/boom`, { method: 'POST', body: '{}' })
    assert.equal(boom.status, 500)
    assert.match((await boom.json()).error, /exploded/)
  } finally {
    await close(server)
  }
})

test('makeAdapterRegistrar registers and unregisters with the gc-facing body shape', async () => {
  const calls = []
  const { server, port } = await listen((req, res) => {
    const chunks = []
    req.on('data', (c) => chunks.push(c))
    req.on('end', () => {
      calls.push({ method: req.method, url: req.url, body: Buffer.concat(chunks).toString('utf8') })
      res.writeHead(200, { 'Content-Type': 'application/json' })
      res.end('{}')
    })
  })
  try {
    const base = `http://127.0.0.1:${port}`
    const { gcFetch } = makeGcClient({ baseUrl: base, city: 'c' })
    const registrar = makeAdapterRegistrar({
      gcFetch,
      baseUrl: base,
      provider: 'telegram',
      account: 'default',
      name: 'openclaw-telegram-bridge',
      callbackUrl: 'http://127.0.0.1:8931',
      capabilities: { SupportsChildConversations: true, SupportsAttachments: false, MaxMessageLength: 0 },
      log: () => {},
    })
    await registrar.registerWithRetry()
    await registrar.unregister()

    const reg = calls.find((c) => c.method === 'POST' && c.url === '/v0/city/c/extmsg/adapters')
    assert.ok(reg, 'posted a registration')
    assert.deepEqual(JSON.parse(reg.body), {
      provider: 'telegram',
      account_id: 'default',
      name: 'openclaw-telegram-bridge',
      callback_url: 'http://127.0.0.1:8931',
      capabilities: { SupportsChildConversations: true, SupportsAttachments: false, MaxMessageLength: 0 },
    })

    const del = calls.find((c) => c.method === 'DELETE' && c.url === '/v0/city/c/extmsg/adapters')
    assert.ok(del, 'deleted the registration on unregister')
    assert.deepEqual(JSON.parse(del.body), { provider: 'telegram', account_id: 'default' })
  } finally {
    await close(server)
  }
})
