package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestConvergeShadowOperatorSummaryReportsSoakSignal proves the counters have an
// operator-visible read path: a clean fixture tick renders a bounded summary line
// carrying the proven denominator (evaluated) and the surviving-divergence count
// that gates a soak. Without this, enabling GC_CONVERGE_SHADOW increments counters
// nothing can read.
func TestConvergeShadowOperatorSummaryReportsSoakSignal(t *testing.T) {
	// converged_steady_state_noop: one evaluated session, zero surviving divergences.
	snap := runFixture(t, convergeCleanCorpus()[0])
	line := snap.operatorSummary()
	if !strings.Contains(line, "evaluated=1") {
		t.Fatalf("operator summary must report the proven denominator, got %q", line)
	}
	if !strings.Contains(line, "surviving_divergences=0") {
		t.Fatalf("operator summary must report zero surviving divergences for a clean tick, got %q", line)
	}
}

// TestConvergeShadowReconcilerEmitsOperatorSummary proves the read path is wired
// end to end: a live enabled reconcile tick writes the soak summary to the
// reconciler's stderr operator channel, reporting a nonzero denominator and zero
// surviving divergences (a live soak can be observed, not just unit-tested).
func TestConvergeShadowReconcilerEmitsOperatorSummary(t *testing.T) {
	t.Setenv("GC_CONVERGE_SHADOW", "1")
	prev := convergeShadowMetrics
	convergeShadowMetrics = newConvergeShadowCounters()
	t.Cleanup(func() { convergeShadowMetrics = prev })

	env := newReconcilerTestEnv()
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		sessionpkg.CanonicalInstanceNameMetadata: "worker",
	})

	env.reconcile([]beads.Bead{session})

	out := env.stderr.String()
	if !strings.Contains(out, "converge-shadow soak:") {
		t.Fatalf("reconciler did not emit the shadow soak summary to stderr; stderr=%q", out)
	}
	if !strings.Contains(out, "surviving_divergences=0") {
		t.Fatalf("live tick summary must report zero surviving divergences; stderr=%q", out)
	}
	if snap := convergeShadowMetrics.snapshot(); snap.SessionsEvaluated == 0 {
		t.Fatalf("live tick must move the denominator; evaluated=0 (skips=%v)", snap.SessionsSkipped)
	}
}
