package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// The nudge queue lock's process-wide policy (DES-NQL §5.6, §5.8, ga-fsp5w9):
// its telemetry, and the retry of bookkeeping on a held claim.

// nudgeQueueLockSlowHold is the hold past which GC_DEBUG names the holder.
// Every queue callback is pure, so a hold is a JSON round trip of the queue;
// one this long means a holder is doing work it should not.
const nudgeQueueLockSlowHold = 100 * time.Millisecond

// nudgeQueueLockDebugWriter receives the GC_DEBUG slow-hold lines.
var nudgeQueueLockDebugWriter io.Writer = os.Stderr

func init() { nudgequeue.SetLockObserver(observeNudgeQueueLock) }

// observeNudgeQueueLock records one acquisition of the queue lock: the wait
// and hold histograms by verb and outcome, and a GC_DEBUG line for a slow
// hold.
func observeNudgeQueueLock(sample nudgequeue.LockSample) {
	verb := sample.Verb
	if verb == "" {
		verb = "unnamed"
	}
	telemetry.RecordNudgeQueueLock(context.Background(), verb, sample.Outcome, durationMs(sample.Wait), durationMs(sample.Hold))
	if sample.Hold > nudgeQueueLockSlowHold && gcDebugEnabled() && nudgeQueueLockDebugWriter != nil {
		fmt.Fprintf(nudgeQueueLockDebugWriter, "gc nudge: queue lock held %s by %s (waited %s, outcome %s)\n", //nolint:errcheck // best-effort diagnostic
			sample.Hold.Round(time.Millisecond), verb, sample.Wait.Round(time.Millisecond), sample.Outcome)
	}
}

func durationMs(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// nudgeQueueLockWait is how long a queue verb of this process waits for the
// queue lock: the queue's budget, or, inside a hook, no longer than the time
// its deadline leaves (hookWorkEnd). A verb that took the lock as the deadline
// fell would commit a transition, an ack, whose delivery gc hook run then
// discards with the output. With no time left the verb tries the lock once.
func nudgeQueueLockWait() time.Duration {
	wait := nudgequeue.LockWait()
	if end, ok := hookWorkEnd(); ok {
		wait = min(wait, max(0, time.Until(end)))
	}
	return wait
}

// nudgeBookkeepingRetries bounds how often bookkeeping on a held claim retries
// a busy queue lock.
const nudgeBookkeepingRetries = 3

// nudgeBookkeepingBackoff is the jittered wait before retry number retry
// (0-based): [250ms, 500ms), then [500ms, 1s), then [1s, 2s). It is a seam so
// a test can make the retries immediate. Tests that replace it must stay
// serial.
var nudgeBookkeepingBackoff = func(retry int) time.Duration {
	base := 250 * time.Millisecond << retry
	if base > time.Second {
		base = time.Second
	}
	return base + rand.N(base)
}

// retryNudgeBookkeeping runs fn, the bookkeeping of a delivery whose items
// the caller holds in flight under its own claim (an ack, a release, a
// failure, a repin), and retries it up to nudgeBookkeepingRetries times while
// it fails with nudgequeue.ErrQueueBusy. Giving up there after a delivery
// leaves the items in flight until their lease expires and redelivers them.
// An attempt reads busy only after a whole lock wait (nudgeQueueLockWait), so
// a lock held throughout costs four waits and the backoffs between them, about
// 35 s outside a hook: inside the two-minute lease, though the caller waits
// that long. Inside a hook, it stops retrying when the backoff would run into
// the time the hook's deadline leaves, and each attempt's lock wait ends there
// too (nudgeQueueLockWait). A producer (Enqueue) does not retry: nothing
// happened, and its op says so.
func retryNudgeBookkeeping(fn func() error) error {
	err := fn()
	for retry := 0; retry < nudgeBookkeepingRetries && errors.Is(err, nudgequeue.ErrQueueBusy); retry++ {
		delay := nudgeBookkeepingBackoff(retry)
		if ok, deadlineErr := hookDeadlineLeavesArchiveTime(time.Now().Add(delay), hookProcessStart); deadlineErr != nil || !ok {
			return err
		}
		time.Sleep(delay)
		err = fn()
	}
	return err
}
