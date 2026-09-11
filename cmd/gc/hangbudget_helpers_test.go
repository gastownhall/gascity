package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// The hang-budget helpers exist to remove a class of load-sensitive flake in
// which a fixed wall-clock deadline, sized for an idle box, fires on a
// contended one (ga-h51wa1). These tests pin the two properties that make the
// replacement safe: waits still return the instant their condition is met, and
// the budget cannot be quietly tuned back down to flake territory.

func TestAwaitCloseReturnsAsSoonAsTheChannelCloses(t *testing.T) {
	t.Parallel()

	ch := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(ch)
	}()

	start := time.Now()
	awaitClose(t, ch, "test channel")
	elapsed := time.Since(start)

	// The point of the budget is that nothing ever waits it out on a passing
	// run. The bound is keyed off hangBudget rather than a fixed number of
	// seconds so this guard stays a hang check and never becomes the kind of
	// load-sensitive latency assertion this package is removing.
	if elapsed > hangBudget/2 {
		t.Fatalf("awaitClose took %s; want it to return when the channel closes, not to wait out the budget", elapsed)
	}
}

func TestAwaitCloseReturnsImmediatelyForAnAlreadyClosedChannel(t *testing.T) {
	t.Parallel()

	ch := make(chan struct{})
	close(ch)

	start := time.Now()
	awaitClose(t, ch, "already-closed channel")
	if elapsed := time.Since(start); elapsed > hangBudget/2 {
		t.Fatalf("awaitClose took %s on an already-closed channel; want an immediate return", elapsed)
	}
}

func TestAwaitCondReturnsAsSoonAsTheConditionHolds(t *testing.T) {
	t.Parallel()

	var ready atomic.Bool
	go func() {
		time.Sleep(20 * time.Millisecond)
		ready.Store(true)
	}()

	start := time.Now()
	awaitCond(t, ready.Load, "test condition")
	if elapsed := time.Since(start); elapsed > hangBudget/2 {
		t.Fatalf("awaitCond took %s; want it to return when the condition holds, not to wait out the budget", elapsed)
	}
}

func TestAwaitCondEvaluatesTheConditionBeforeSleeping(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	awaitCond(t, func() bool {
		calls.Add(1)
		return true
	}, "already-true condition")

	// The call count is the whole property: an already-true condition must be
	// observed on the first evaluation, before any poll sleep. Deliberately NOT
	// asserted as "elapsed < hangPollInterval" — a few-microseconds-of-work
	// assertion with a 5ms bound is exactly the load-sensitive latency
	// assertion this package's hang budget exists to remove, and a thread on a
	// 2.6x-oversubscribed box can be descheduled for longer than that.
	if got := calls.Load(); got != 1 {
		t.Fatalf("cond evaluated %d time(s), want exactly 1 for an already-true condition", got)
	}
}

// TestHangBudgetStaysAHangDetector guards the constant against exactly the
// change that would re-introduce the flake: someone shrinking it to "make the
// suite faster". Shrinking it cannot make a passing suite faster — the waits
// return on their condition — it can only convert load spikes back into red
// builds.
//
// Two independent floors are asserted. The first is TESTING.md's "Test deadline
// rule", which this package must not fall below. The second is this migration's
// own evidence: the largest fixed deadline it replaced was 15s
// (cmd_stop_test.go's controller-availability poll), and guarded regions were
// measured at 18.0-19.8s under single-core starvation, so anything at or below
// 2x the replaced deadline is known-insufficient rather than merely aggressive.
func TestHangBudgetStaysAHangDetector(t *testing.T) {
	t.Parallel()

	if hangBudget < testutil.GoroutineRaceTimeout {
		t.Fatalf("hangBudget = %s, want >= testutil.GoroutineRaceTimeout (%s); "+
			"TESTING.md's test deadline rule is a floor this package may not go below",
			hangBudget, testutil.GoroutineRaceTimeout)
	}

	const largestReplacedDeadline = 15 * time.Second
	if hangBudget < 2*largestReplacedDeadline {
		t.Fatalf("hangBudget = %s, want >= %s (twice the largest fixed deadline it replaced); "+
			"it is a hang detector, not a latency assertion — do not tune it to make a test pass",
			hangBudget, 2*largestReplacedDeadline)
	}
}
