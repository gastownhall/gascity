// At-least-once inbound bookkeeping for a getUpdates-style long poll, factored
// out of telegram-bridge.mjs so the redelivery invariant is unit testable
// without a live gc or Bot API (see test/inbound.test.mjs).
//
// The model: Telegram acknowledges updates server-side as soon as the next
// getUpdates sends a higher offset, so an update must be durably handled BEFORE
// the offset advances past it, or a gc outage silently drops it. forward(update)
// returns whether the poll offset may advance past that update:
//   - true  => handled (a real forward, an intentional skip, or a permanent
//              drop) — advance the offset.
//   - false => transient failure — leave the offset un-advanced so Telegram
//              redelivers the same update on the next poll.
//
// This module has no openclaw, gc-server, or network dependency: deliver, sleep,
// and isShuttingDown are injected so the offset/retry logic can be exercised in
// isolation.

// isPermanentInboundRejection: gc returned a definitive non-429 4xx. The
// /extmsg/inbound contract reserves 4xx for input gc can never accept — a
// schema-invalid body (Huma 422) or a malformed/unroutable conversation (400,
// from ErrInvalidConversation/ErrInvalidInput) — while transient server-side
// faults (binding / group-route / transcript store errors) surface as 5xx. A
// 4xx message is rejected identically every time, so retrying — or worse,
// holding the poll offset for redelivery — just wedges the entire inbound
// stream behind one poison update. Everything else (5xx, 429 rate-limit, and
// transport errors / timeouts, which carry no status) is a transient gc-side
// problem worth retrying. gc's split lives in
// internal/api/huma_handlers_extmsg.go (4xx for ErrInvalidConversation/
// ErrInvalidInput, 5xx for storage faults).
export const isPermanentInboundRejection = (err) =>
  typeof err?.status === 'number' && err.status >= 400 && err.status < 500 && err.status !== 429

// forwardWithRetry forwards one update via deliver(update), retrying transient
// failures with linear backoff (capped at maxBackoffMs). Returns true when the
// caller may advance past the update (success, intentional skip, or a permanent
// drop) and false when it must be left un-acked. A permanent rejection is
// dropped immediately — not retried — so it cannot pin a poll offset.
//
// maxAttempts bounds the transient retries. A source WITH a redelivery cursor
// (Telegram's poll offset) uses the default 5 and, on exhaustion, returns false
// so the source resends. A source with NO redelivery cursor (iMessage's watch
// stream) passes maxAttempts: Infinity so a transient gc outage is retried until
// gc recovers, rather than dropping an already-consumed notification; there,
// false is returned only when shutdown interrupts an in-flight retry, which the
// caller must treat as genuine loss.
export async function forwardWithRetry({
  update,
  deliver,
  maxAttempts = 5,
  maxBackoffMs = 30000,
  isShuttingDown,
  sleep,
  log,
}) {
  for (let attempt = 1; !isShuttingDown?.(); attempt++) {
    try {
      await deliver(update)
      return true
    } catch (err) {
      if (isShuttingDown?.()) return false
      if (isPermanentInboundRejection(err)) {
        log?.(
          `inbound permanently rejected (HTTP ${err.status}); dropping update ${update?.update_id} ` +
            `so it cannot pin the poll offset: ${err.message}`,
        )
        return true
      }
      if (attempt >= maxAttempts) {
        log?.(`inbound forward failed after retries; leaving update unacked for redelivery: ${err.message}`)
        return false
      }
      // Linear backoff, capped so an unbounded (iMessage) retry does not grow to
      // multi-minute sleeps and miss gc coming back.
      await sleep(Math.min(attempt * 2000, maxBackoffMs))
    }
  }
  return false
}

// drainBatch forwards a getUpdates batch in order, advancing the offset past
// each handled update and stopping at the first transient failure so the next
// getUpdates redelivers from exactly there. Returns the offset to resume from.
export async function drainBatch({ batch, offset, forward, isShuttingDown }) {
  for (const u of batch) {
    if (isShuttingDown?.()) break
    if (!(await forward(u))) break
    offset = Math.max(offset, u.update_id + 1)
  }
  return offset
}
