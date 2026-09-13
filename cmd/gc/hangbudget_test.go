package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// hangBudget is the single wall-clock ceiling shared by every test-side wait in
// this package.
//
// It is a HANG DETECTOR, not a latency assertion. No test in this package waits
// on it to prove the system is fast: the real assertions always come after the
// wait returns. Nothing waits the budget out on a passing run either, because
// awaitClose and awaitCond return the instant their condition is met — so
// raising this number does not make the suite slower, and lowering it does not
// make the suite stricter. It only changes how long a genuinely wedged test
// takes to report.
//
// DO NOT tune hangBudget to make a failing test pass. A test that needs to
// assert a latency bound must keep its own explicit deadline plus a comment
// saying which bound it asserts, or be written as a benchmark. A test that
// needs to assert a negative ("nothing arrives within X") must likewise keep
// its own short deadline — hangBudget is the wrong tool there and would add a
// full minute of dead wait.
//
// RELATIONSHIP TO testutil.GoroutineRaceTimeout — these are not competing
// constants, which is why hangBudget is derived from it rather than declared
// alongside it. TESTING.md's "Test deadline rule" makes GoroutineRaceTimeout
// the MINIMUM safe deadline for a timer racing a goroutine ("must be >= 10s").
// hangBudget is the point at which this package declares a wedge, which is a
// ceiling, not a floor. Use GoroutineRaceTimeout directly when a test needs a
// deadline that satisfies the rule; use awaitClose/awaitCond when the wait is
// purely a hang detector and no assertion depends on how long it took.
//
// Why the floor alone is not enough here: measured under single-core CPU
// starvation, the guarded regions in this package took 18.0-19.8s to complete
// (ga-h51wa1). A 10s deadline would have failed all eight of those runs even
// though nothing was wedged. The multiplier gives roughly 3x headroom over the
// worst run actually observed; Go's package -timeout remains the real backstop.
// This constant exists only so a wedged wait fails at the line that wedged,
// with a useful message, instead of taking the whole package down with an
// unattributed timeout.
const hangBudget = 6 * testutil.GoroutineRaceTimeout

// hangPollInterval is how often awaitCond re-evaluates its condition. It is a
// responsiveness knob only; it has no bearing on whether a test passes.
const hangPollInterval = 5 * time.Millisecond

// awaitClose waits for ch to be closed (or to yield a value) and fails the test
// if that does not happen within hangBudget. what names the thing being waited
// on and is used to build the failure message.
func awaitClose(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(hangBudget):
		t.Fatalf("%s did not complete within the hang budget (%s)", what, hangBudget)
	}
}

// awaitCond polls cond until it reports true and fails the test if that does not
// happen within hangBudget. cond is evaluated once before any sleep, so a
// condition that is already true returns immediately. what names the condition
// being waited on and is used to build the failure message.
func awaitCond(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.After(hangBudget)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("%s did not happen within the hang budget (%s)", what, hangBudget)
		case <-time.After(hangPollInterval):
		}
	}
}
