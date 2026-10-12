// Package workqueue is a small keyed work queue for the reconciler: one
// reconcile per key at a time, with every trigger that lands meanwhile
// folded into the next one.
//
// A key is at any instant exactly one of absent, queued (hot or resync),
// processing, or processing and dirty. Separately it may have one pending
// timer and a failure record. The queue promises:
//
//   - Dedupe. Adding a queued key merges its reason into the queued item
//     (deduped by Kind, at most MaxReasons, overflow counted). Adding a
//     processing key marks it dirty; Done puts it back at the tail of the
//     strongest lane it was added on.
//   - Per-key serialization. Get never returns a key that is processing.
//     Every successful Get needs exactly one Done, deferred by the caller.
//   - Two lanes. Get takes the hot head, except that every AgingEvery-th
//     dequeue takes the resync head, so resync work cannot starve. A hot Add
//     of a key queued on the resync lane promotes it to the hot tail.
//   - One timer per key, the earliest. AddAfter with a later deadline only
//     merges its reason; an immediate Add never cancels the timer.
//   - Gated backoff. AddRateLimited counts a failure and closes the key's
//     backoff gate until its jittered retry time. While the gate is closed a
//     non-urgent Add of an idle key, and a non-urgent dirty requeue at Done,
//     are deferred to the retry time instead of running at event rate.
//     Urgent reasons bypass the gate. Forget clears the failure record and
//     releases what the gate held, or discards it if the key is processing,
//     since the running reconcile started after all of it. Callers report
//     the outcome (AddRateLimited or Forget) before Done.
//   - Holds. Get blocks while any named hold is active; WaitIdle waits for
//     in-flight items to finish.
//   - Shutdown. ShutDown stops admission and timers. Get returns false from
//     then on, even if items remain: queued work is never started. Done keeps
//     working so in-flight items can finish.
//   - Sequence numbers. Every accepted Add returns a queue-wide monotonic
//     seq; seqs are not dense. An item carries the seq of its first and
//     latest merged Add, which Coverage uses to tell exactly when every
//     tracked Add has been reconciled: NewCoverage for a key set known up
//     front (boot), Track and Seal for one built while workers run (resync).
//
// No ordering between different keys is promised beyond FIFO per lane.
// There is no global rate limit, no priority beyond the two lanes and no
// persistence.
//
// The package imports only the standard library. Blocking waits use a
// broadcast channel, so goroutines blocked in Get or WaitIdle are durably
// blocked under testing/synctest.
package workqueue
