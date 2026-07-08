package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestConvergeShadowReconcilerWiringLive proves the 3a fact capture is actually
// wired into the reconciler's Phase 1 (not silently dead): with the harness
// enabled, reconciling a live desired session moves the denominator
// (sessions_evaluated) and produces zero surviving divergences on this
// steady-state tick. A flatlined denominator here would be a wiring failure, not
// a pass (hardening 2).
func TestConvergeShadowReconcilerWiringLive(t *testing.T) {
	t.Setenv("GC_CONVERGE_SHADOW", "1")
	// Use an isolated counter set so the assertion is deterministic regardless of
	// other tests that may have run the global harness.
	prev := convergeShadowMetrics
	convergeShadowMetrics = newConvergeShadowCounters()
	t.Cleanup(func() { convergeShadowMetrics = prev })

	env := newReconcilerTestEnv()
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	// Stamp a canonical identity so the steady-state tick derives nothing and the
	// comparison is a clean ∅-on-live.
	env.setSessionMetadata(&session, map[string]string{
		sessionpkg.CanonicalInstanceNameMetadata: "worker",
	})

	env.reconcile([]beads.Bead{session})

	snap := convergeShadowMetrics.snapshot()
	if snap.SessionsEvaluated == 0 && snap.Incomparable == 0 {
		t.Fatalf("shadow harness wiring is dead: nothing evaluated (skipped: %v)", snap.SessionsSkipped)
	}
	if got := snap.survivingDivergences(); got != 0 {
		t.Fatalf("steady-state tick produced %d surviving divergences (classes: %v)", got, snap.DivergenceTotal)
	}
}

// TestConvergeShadowReconcilerDisabledInert proves the reconciler is inert when
// the harness is off: the global recorder is never attached and the denominator
// does not move.
func TestConvergeShadowReconcilerDisabledInert(t *testing.T) {
	t.Setenv("GC_CONVERGE_SHADOW", "")
	prev := convergeShadowMetrics
	convergeShadowMetrics = newConvergeShadowCounters()
	t.Cleanup(func() { convergeShadowMetrics = prev })

	env := newReconcilerTestEnv()
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	env.reconcile([]beads.Bead{session})

	if convergeGlobalRecorder.Load() != nil {
		t.Fatal("global recorder attached with harness disabled")
	}
	snap := convergeShadowMetrics.snapshot()
	if snap.SessionsEvaluated != 0 {
		t.Fatalf("denominator moved with harness disabled: %d", snap.SessionsEvaluated)
	}
}
