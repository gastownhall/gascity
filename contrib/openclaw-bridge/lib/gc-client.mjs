// Shared gc-side glue for the openclaw extmsg bridges. Each bridge hosts a
// different openclaw connector, but they all talk to gc the same way: the same
// /v0/city/{city}/extmsg wire, the same callback-server shape, and the same
// register / re-register / shutdown lifecycle. Keeping that glue here is what
// stops the two bridges from drifting on controller-facing behavior — the
// gcFetch timeout, the X-GC-Request CSRF header, the register-retry budget, and
// the shutdown ordering all live in one place.

import http from 'node:http'

// env(k, d): process.env[k] when it is set and non-empty, otherwise the default.
export const env = (k, d) => (process.env[k] !== undefined && process.env[k] !== '' ? process.env[k] : d)

// makeGcClient binds a gcFetch to one city. gcFetch throws on any non-2xx with
// the HTTP status attached as err.status, and lets transport errors / timeouts
// propagate with no status, so callers can classify transient (retry) vs
// permanent (drop) failures without parsing error strings.
export function makeGcClient({ baseUrl, city, timeoutMs = 15000 }) {
  const cityBase = `${baseUrl}/v0/city/${encodeURIComponent(city)}`
  async function gcFetch(method, p, body) {
    const res = await fetch(`${cityBase}${p}`, {
      method,
      headers: { 'Content-Type': 'application/json', 'X-GC-Request': '1' },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(timeoutMs), // a wedged gc must not wedge the bridge (esp. shutdown)
    })
    const text = await res.text()
    if (!res.ok) {
      const err = new Error(`${method} ${p}: HTTP ${res.status}: ${text.slice(0, 300)}`)
      err.status = res.status
      throw err
    }
    return text ? JSON.parse(text) : null
  }
  return { gcFetch }
}

// startCallbackServer creates the gc -> bridge callback HTTP server and resolves
// once it is listening on 127.0.0.1:port. It answers the transport-level
// GET /healthz liveness probe itself (identical for every bridge), and hands
// every other request to handleRequest(req, rawBody), which returns
// { status, body }; a thrown handler becomes a 500. Only handleRequest's routing
// is provider-specific.
export function startCallbackServer({ handleRequest, port }) {
  const server = http.createServer((req, res) => {
    const send = (status, body) => {
      res.writeHead(status, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify(body))
    }
    if (req.method === 'GET' && req.url === '/healthz') {
      req.resume() // drain any body before responding
      send(200, { ok: true })
      return
    }
    const chunks = []
    req.on('data', (c) => chunks.push(c))
    req.on('end', () => {
      handleRequest(req, Buffer.concat(chunks).toString('utf8'))
        .then(({ status, body }) => send(status, body))
        .catch((err) => send(500, { error: String(err?.message ?? err) }))
    })
  })
  return new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(port, '127.0.0.1', () => resolve(server))
  })
}

// makeAdapterRegistrar owns the gc adapter registration lifecycle. The gc
// registry is in-memory, so callers register once at startup (retrying while gc
// is still coming up), re-register on an interval to survive controller
// restarts, and unregister on shutdown.
export function makeAdapterRegistrar({ gcFetch, baseUrl, provider, account, name, callbackUrl, capabilities, log }) {
  const register = () =>
    gcFetch('POST', '/extmsg/adapters', {
      provider,
      account_id: account,
      name,
      callback_url: callbackUrl,
      capabilities,
    })

  // registerWithRetry blocks until gc accepts the registration (gc may still be
  // starting), giving up only after ~60s so a misconfigured base url still fails
  // loudly instead of hanging forever.
  async function registerWithRetry() {
    let attempts = 0
    for (;;) {
      try {
        await register()
        return
      } catch (err) {
        attempts += 1
        if (attempts >= 60) throw err
        if (attempts === 1) log(`waiting for gc at ${baseUrl} (${err.message})`)
        await new Promise((r) => setTimeout(r, 1000))
      }
    }
  }

  // startReregister keeps the in-memory gc registration alive; returns the timer
  // so shutdown can clear it.
  const startReregister = () =>
    setInterval(() => register().catch((err) => log('re-register failed:', err.message)), 30000)

  const unregister = () => gcFetch('DELETE', '/extmsg/adapters', { provider, account_id: account })

  return { register, registerWithRetry, startReregister, unregister }
}

// makeShutdown returns an idempotent SIGINT/SIGTERM handler that tears the
// bridge down in a fixed order: stop re-registering, run the provider-specific
// teardown (stop inbound, abort polls), unregister from gc (best-effort — gc may
// already be gone), close the callback server, and exit. Wiring it identically
// in both bridges keeps unregister/teardown ordering from drifting. exitCode is
// evaluated after teardown so a connector that lost an un-acked inbound message
// on shutdown (a source with no redelivery cursor) can refuse a clean exit.
export function makeShutdown({ log, reregister, unregister, server, teardown, exitCode }) {
  let done = false
  return async () => {
    if (done) return
    done = true
    log('shutting down')
    if (reregister) clearInterval(reregister)
    try {
      await teardown?.()
    } catch {
      // provider teardown is best-effort; the daemon/poll may already be gone
    }
    try {
      await unregister()
    } catch {
      // gc may already be gone
    }
    server.close()
    process.exit(exitCode ? exitCode() : 0)
  }
}
