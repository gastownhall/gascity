package main

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// T8: bookkeeping on a held claim retries a busy queue lock, so a lock held
// past one wait budget does not turn a delivered nudge into a redelivery.
func TestNudgeAckAfterDeliveryRetriesABusyLock(t *testing.T) {
	h := newNQCharHarness(t)
	enqueueAndClaimForTest(t, h, "n-1")
	restore := nudgequeue.SetLockWaitForTest(300 * time.Millisecond)
	defer restore()
	release := holdNudgeQueueLockForTest(t, h.city)
	h.lockHeldByTest = true
	// The other holder lets go while the ack backs off after its first busy
	// attempt, so only a retry can succeed.
	prev := nudgeBookkeepingBackoff
	retries := 0
	nudgeBookkeepingBackoff = func(int) time.Duration {
		retries++
		release()
		return time.Millisecond
	}
	t.Cleanup(func() { nudgeBookkeepingBackoff = prev })
	if err := ackQueuedNudgesWithOutcome(h.city, []string{"n-1"}, "injected", "", "provider-nudge-return"); err != nil {
		t.Fatalf("ack under a lock held past one budget: %v, want success on a retry", err)
	}
	if retries != 1 {
		t.Fatalf("retries = %d, want the ack to succeed on its first retry", retries)
	}
	state := loadNudgeStateForTest(t, h.city)
	if len(state.InFlight)+len(state.Pending) != 0 {
		t.Fatalf("after the ack pending=%v in-flight=%v, want the delivered nudge retired", nqCharItems(state.Pending), nqCharItems(state.InFlight))
	}
}

// A producer does not retry: an enqueue under a lock held past its budget
// fails with ErrQueueBusy after one budget.
func TestNudgeEnqueueDoesNotRetryABusyLock(t *testing.T) {
	h := newNQCharHarness(t)
	restore := nudgequeue.SetLockWaitForTest(300 * time.Millisecond)
	defer restore()
	holdNudgeQueueLockForTest(t, h.city)
	h.lockHeldByTest = true
	start := time.Now()
	err := enqueueQueuedNudgeWithStore(h.city, h.shadow(), h.item("n-1", "alpha", 0, queuedNudgeOptions{}))
	if !errors.Is(err, nudgequeue.ErrQueueBusy) {
		t.Fatalf("enqueue under a held lock: err = %v, want ErrQueueBusy", err)
	}
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Fatalf("enqueue took %s, want one 300ms budget and no retry", elapsed)
	}
}

// Bookkeeping retries only a busy lock, at most nudgeBookkeepingRetries times.
func TestRetryNudgeBookkeepingRetriesOnlyBusy(t *testing.T) {
	prev := nudgeBookkeepingBackoff
	nudgeBookkeepingBackoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { nudgeBookkeepingBackoff = prev })

	calls := 0
	err := retryNudgeBookkeeping(func() error { calls++; return nudgequeue.ErrQueueBusy })
	if !errors.Is(err, nudgequeue.ErrQueueBusy) || calls != nudgeBookkeepingRetries+1 {
		t.Fatalf("always busy: err = %v after %d calls, want ErrQueueBusy after %d", err, calls, nudgeBookkeepingRetries+1)
	}
	calls = 0
	other := errors.New("parse nudge queue")
	if err := retryNudgeBookkeeping(func() error { calls++; return other }); !errors.Is(err, other) || calls != 1 {
		t.Fatalf("a non-busy error: err = %v after %d calls, want it after one", err, calls)
	}
	for retry := 0; retry < nudgeBookkeepingRetries; retry++ {
		if d := prev(retry); d < 250*time.Millisecond || d >= 2*time.Second {
			t.Fatalf("backoff(%d) = %s, want within [250ms, 2s)", retry, d)
		}
	}
}

// The lock observer names the verb of a slow hold under GC_DEBUG.
func TestNudgeQueueLockObserverNamesASlowHold(t *testing.T) {
	t.Setenv("GC_DEBUG", "1")
	var buf bytes.Buffer
	var mu sync.Mutex
	prev := nudgeQueueLockDebugWriter
	nudgeQueueLockDebugWriter = nqLockDebugWriter{w: &buf, mu: &mu}
	t.Cleanup(func() { nudgeQueueLockDebugWriter = prev })

	observeNudgeQueueLock(nudgequeue.LockSample{Verb: "ack", Outcome: nudgequeue.LockOutcomeOK, Wait: time.Millisecond, Hold: 150 * time.Millisecond})
	observeNudgeQueueLock(nudgequeue.LockSample{Verb: "claim", Outcome: nudgequeue.LockOutcomeOK, Hold: time.Millisecond})
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "held 150ms by ack") || strings.Contains(out, "claim") {
		t.Fatalf("debug output = %q, want one line naming the slow ack", out)
	}
}

type nqLockDebugWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l nqLockDebugWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
