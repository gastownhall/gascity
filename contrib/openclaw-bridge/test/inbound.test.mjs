// Regression coverage for the Telegram inbound at-least-once durability model
// (lib/inbound.mjs). This is the proof the review loop asked for: force the
// gc forward to fail for one update and assert the poll offset is NOT advanced
// past it (so Telegram redelivers it), while a permanently-rejected poison
// update is dropped instead of pinning the offset forever.
//
// Run: npm test  (or: node --test test/)

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { forwardWithRetry, drainBatch, isPermanentInboundRejection } from '../lib/inbound.mjs'

const noSleep = () => Promise.resolve()
const never = () => false
const update = (id) => ({ update_id: id, message: { message_id: id, text: 'hi' } })
const httpError = (status) => Object.assign(new Error(`gc HTTP ${status}`), { status })

test('isPermanentInboundRejection: only a non-429 4xx is permanent', () => {
  assert.equal(isPermanentInboundRejection(httpError(400)), true) // malformed/unroutable input (gc handler)
  assert.equal(isPermanentInboundRejection(httpError(422)), true) // schema-invalid body (gc/Huma)
  assert.equal(isPermanentInboundRejection(httpError(404)), true)
  assert.equal(isPermanentInboundRejection(httpError(429)), false) // rate limit -> retry
  assert.equal(isPermanentInboundRejection(httpError(500)), false) // gc outage or transient store fault -> retry
  assert.equal(isPermanentInboundRejection(httpError(503)), false)
  assert.equal(isPermanentInboundRejection(new Error('ECONNREFUSED')), false) // no status -> transient
  assert.equal(isPermanentInboundRejection(undefined), false)
})

test('transient failure holds the offset, then the same update is redelivered after gc recovers', async () => {
  // gc fails update 101 once with a 5xx (transient outage or storage fault),
  // then accepts all. A transient normalized-inbound fault is a 5xx, not a 4xx.
  const delivered = []
  let failsLeft = 1
  const deliver = async (u) => {
    if (u.update_id === 101 && failsLeft > 0) {
      failsLeft -= 1
      throw httpError(503)
    }
    delivered.push(u.update_id)
  }
  const forward = (u) => forwardWithRetry({ update: u, deliver, maxAttempts: 1, isShuttingDown: never, sleep: noSleep })

  // First poll cycle: getUpdates(offset=101) returns [101, 102] (offset is the
  // first update_id the bridge still wants).
  let offset = await drainBatch({ batch: [update(101), update(102)], offset: 101, forward, isShuttingDown: never })
  // 101 failed transiently -> offset held at 101 (not advanced past it), and 102
  // is NOT delivered ahead of 101 (ordering preserved).
  assert.equal(offset, 101)
  assert.deepEqual(delivered, [])

  // Telegram redelivers from the held offset: getUpdates(offset=101) -> [101, 102].
  offset = await drainBatch({ batch: [update(101), update(102)], offset, forward, isShuttingDown: never })
  assert.equal(offset, 103)
  assert.deepEqual(delivered, [101, 102]) // the previously-failed update was redelivered and delivered
})

test('a poison update (non-429 4xx) is dropped past the offset instead of wedging the stream', async () => {
  const delivered = []
  const attempts = new Map()
  const deliver = async (u) => {
    attempts.set(u.update_id, (attempts.get(u.update_id) ?? 0) + 1)
    if (u.update_id === 201) throw httpError(422) // schema-invalid body: gc rejects it identically forever
    delivered.push(u.update_id)
  }
  const forward = (u) => forwardWithRetry({ update: u, deliver, maxAttempts: 5, isShuttingDown: never, sleep: noSleep })

  const offset = await drainBatch({ batch: [update(201), update(202)], offset: 200, forward, isShuttingDown: never })
  // 201 is permanently rejected -> dropped past; 202 still delivered; offset
  // advances past both so the poison update can never pin the poll offset.
  assert.equal(offset, 203)
  assert.deepEqual(delivered, [202])
  assert.equal(attempts.get(201), 1) // dropped immediately, not retried maxAttempts times
})

test('429 rate limiting is retried (transient), never dropped', async () => {
  const delivered = []
  let failsLeft = 2
  const deliver = async (u) => {
    if (failsLeft > 0) {
      failsLeft -= 1
      throw httpError(429)
    }
    delivered.push(u.update_id)
  }
  const ok = await forwardWithRetry({ update: update(301), deliver, maxAttempts: 5, isShuttingDown: never, sleep: noSleep })
  assert.equal(ok, true)
  assert.deepEqual(delivered, [301]) // delivered after retries, never dropped
})

test('drainBatch stops at the first transient failure so ordering is preserved', async () => {
  const delivered = []
  const deliver = async (u) => {
    if (u.update_id === 402) throw httpError(500)
    delivered.push(u.update_id)
  }
  const forward = (u) => forwardWithRetry({ update: u, deliver, maxAttempts: 1, isShuttingDown: never, sleep: noSleep })
  const offset = await drainBatch({ batch: [update(401), update(402), update(403)], offset: 400, forward, isShuttingDown: never })
  // 401 delivered, 402 fails -> stop; 403 must NOT be delivered ahead of 402.
  assert.equal(offset, 402)
  assert.deepEqual(delivered, [401])
})

test('shutdown mid-retry leaves the update un-acked for redelivery', async () => {
  let shuttingDown = false
  const deliver = async () => {
    shuttingDown = true // gc starts failing right as the bridge begins shutting down
    throw httpError(503)
  }
  const ok = await forwardWithRetry({
    update: update(501),
    deliver,
    maxAttempts: 5,
    isShuttingDown: () => shuttingDown,
    sleep: noSleep,
  })
  assert.equal(ok, false) // not advanced -> redelivered on the next run
})

// The iMessage watch stream has no redelivery cursor, so the bridge passes
// maxAttempts: Infinity: a transient gc outage must be retried until gc recovers
// rather than dropping an already-consumed notification after a bounded budget.
test('unbounded retry (maxAttempts: Infinity) keeps retrying a transient outage past the default budget, never dropping', async () => {
  const delivered = []
  let failsLeft = 8 // longer than the default maxAttempts of 5
  const deliver = async (u) => {
    if (failsLeft > 0) {
      failsLeft -= 1
      throw httpError(503)
    }
    delivered.push(u.update_id)
  }
  const ok = await forwardWithRetry({
    update: update(601),
    deliver,
    maxAttempts: Infinity,
    isShuttingDown: never,
    sleep: noSleep,
  })
  assert.equal(ok, true) // delivered after recovery
  assert.deepEqual(delivered, [601]) // exactly once, never dropped despite >5 failures
})

test('unbounded retry caps the linear backoff at maxBackoffMs', async () => {
  const slept = []
  const sleep = (ms) => {
    slept.push(ms)
    return Promise.resolve()
  }
  let failsLeft = 20
  const deliver = async () => {
    if (failsLeft > 0) {
      failsLeft -= 1
      throw httpError(503)
    }
  }
  await forwardWithRetry({
    update: update(602),
    deliver,
    maxAttempts: Infinity,
    maxBackoffMs: 5000,
    isShuttingDown: never,
    sleep,
  })
  assert.ok(slept.length >= 10, `expected many backoff sleeps, got ${slept.length}`)
  assert.ok(Math.max(...slept) <= 5000, `backoff must be capped at 5000ms, saw ${Math.max(...slept)}`)
  assert.ok(slept.includes(2000) && slept.includes(4000), 'early backoff still grows linearly before the cap')
})

test('unbounded retry still drops a poison (non-429 4xx) immediately, not retried forever', async () => {
  let attempts = 0
  const deliver = async () => {
    attempts += 1
    throw httpError(400) // permanent: gc rejects it identically every time
  }
  const ok = await forwardWithRetry({
    update: update(603),
    deliver,
    maxAttempts: Infinity,
    isShuttingDown: never,
    sleep: noSleep,
  })
  assert.equal(ok, true) // dropped (handled), so the caller advances past it
  assert.equal(attempts, 1) // not retried — a poison message must not pin the stream
})

test('unbounded retry returns false when shutdown interrupts an in-flight outage (iMessage loss signal)', async () => {
  let shuttingDown = false
  let calls = 0
  const deliver = async () => {
    calls += 1
    if (calls === 2) shuttingDown = true // shutdown arrives mid-outage
    throw httpError(503)
  }
  const ok = await forwardWithRetry({
    update: update(604),
    deliver,
    maxAttempts: Infinity,
    isShuttingDown: () => shuttingDown,
    sleep: noSleep,
  })
  // false => the bridge could not deliver before shutdown; with no source
  // redelivery the caller must treat this as genuine loss (and exit non-zero).
  assert.equal(ok, false)
})
