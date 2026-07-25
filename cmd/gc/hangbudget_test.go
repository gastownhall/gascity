package main

import (
	"testing"
	"time"
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
// Sized as a genuine hang budget rather than fitted to any observed run: the
// slowest guarded region measured under fleet load was ~3.9s (ga-h51wa1), and
// Go's package -timeout remains the real backstop. This exists only so a wedged
// wait fails at the line that wedged, with a useful message, instead of taking
// the whole package down with an unattributed timeout.
const hangBudget = 60 * time.Second

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
