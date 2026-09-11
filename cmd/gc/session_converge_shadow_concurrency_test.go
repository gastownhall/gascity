package main

import (
	"fmt"
	"sync"
	"testing"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestConvergeShadowRecorderOwnershipIsolatesConcurrentTicks proves the
// ownership-token fix for the multi-city recorder race: the supervisor
// reconciles each city on its own goroutine, so two enabled ticks can overlap.
// Only the tick that installs the process-global recorder owns it; a concurrent
// second tick is a no-owner. It records nothing of its own, marks its sessions
// with the typed recorder_contended skip (an honest denominator, never a false
// divergence), and — critically — the owner's compared-key reads never pick up
// the contended tick's writes, because writes are keyed by globally-unique
// session bead IDs. This is the "writes cannot cross between recorders" guard.
func TestConvergeShadowRecorderOwnershipIsolatesConcurrentTicks(t *testing.T) {
	t.Setenv("GC_CONVERGE_SHADOW", "1")
	t.Cleanup(func() { convergeGlobalRecorder.Store(nil) })

	countersOwner := newConvergeShadowCounters()
	countersContended := newConvergeShadowCounters()

	// Owner attaches first and wins the CAS; the contended tick overlaps it.
	owner := newConvergeShadowTick("city-owner", 1, fixtureNow, true, countersOwner)
	contended := newConvergeShadowTick("city-contended", 2, fixtureNow, true, countersContended)
	if owner == nil || contended == nil {
		t.Fatal("newConvergeShadowTick returned nil with harness enabled")
	}
	if !owner.owned {
		t.Fatal("first tick must own the process-global recorder")
	}
	if contended.owned {
		t.Fatal("second concurrent tick must NOT own the recorder (ownership token failed)")
	}

	const sidOwner = "sess-owner"
	const sidContended = "sess-contended"

	// Owner: clean steady state — canonical present at both ends, derivation empty.
	owner.captureDurable(sidOwner, "tok-o", "dir/agent-owner",
		durableFacts{canonicalIdentity: "dir/agent-owner", now: fixtureNow},
		map[string]string{sessionpkg.CanonicalInstanceNameMetadata: "dir/agent-owner"},
		convergePredictedValues{})
	owner.captureRuntime(sidOwner, "desired", "dir/agent-owner", convergeTriTrue, convergeTriTrue)

	// Contended: an owned-key delta with no recorder entry it can see. If it were
	// wrongly scored it would flag foreign_write; instead it must be skipped.
	contended.captureDurable(sidContended, "tok-c", "dir/agent-contended",
		durableFacts{canonicalIdentity: "", now: fixtureNow},
		map[string]string{},
		convergePredictedValues{canonicalInstanceName: "dir/agent-contended"})
	contended.captureRuntime(sidContended, "desired", "dir/agent-contended", convergeTriTrue, convergeTriTrue)

	// The contended tick's write site goes through the SAME global wrapper the real
	// reconciler uses; it lands in the owner's recorder (the only one attached),
	// tagged by the contended session's unique id. It must never surface in the
	// owner's evaluation.
	recordLegacyCompareWrites(sidContended, "syncSessionBeads", map[string]string{
		sessionpkg.CanonicalInstanceNameMetadata: "dir/agent-contended",
	})

	// Finish the contended tick first: it detaches without clearing the owner's
	// live recorder, and its session is a typed recorder_contended skip.
	contended.finish(map[string]map[string]string{
		sidContended: {sessionpkg.CanonicalInstanceNameMetadata: "dir/agent-contended"},
	})
	snapC := countersContended.snapshot()
	if snapC.SessionsSkipped[skipRecorderContended] != 1 {
		t.Fatalf("contended tick: recorder_contended skip = %d, want 1 (skips=%v)", snapC.SessionsSkipped[skipRecorderContended], snapC.SessionsSkipped)
	}
	if snapC.SessionsEvaluated != 0 {
		t.Fatalf("contended tick must not evaluate anything, got evaluated=%d", snapC.SessionsEvaluated)
	}
	if got := snapC.survivingDivergences(); got != 0 {
		t.Fatalf("contended tick must not manufacture divergences, got %d (classes: %v)", got, snapC.DivergenceTotal)
	}

	// Finish the owner: it evaluates its own clean session and clears the recorder.
	owner.finish(map[string]map[string]string{
		sidOwner: {sessionpkg.CanonicalInstanceNameMetadata: "dir/agent-owner"},
	})
	snapO := countersOwner.snapshot()
	if snapO.SessionsEvaluated != 1 {
		t.Fatalf("owner tick: evaluated = %d, want 1", snapO.SessionsEvaluated)
	}
	if got := snapO.survivingDivergences(); got != 0 {
		t.Fatalf("owner tick's clean session must not diverge because of the contended write, got %d (classes: %v)", got, snapO.DivergenceTotal)
	}

	if convergeGlobalRecorder.Load() != nil {
		t.Fatal("recorder must be cleared once every tick has detached")
	}
}

// TestConvergeShadowRecorderConcurrentTicksNoRace runs two full tick lifecycles
// concurrently (as the supervisor's per-city goroutines do) and proves the
// attach/record/detach path is race-free and always leaves the global recorder
// cleared, regardless of which tick wins the ownership CAS. Run under -race.
func TestConvergeShadowRecorderConcurrentTicksNoRace(t *testing.T) {
	t.Setenv("GC_CONVERGE_SHADOW", "1")
	t.Cleanup(func() { convergeGlobalRecorder.Store(nil) })

	var wg sync.WaitGroup
	for i, city := range []string{"city-a", "city-b"} {
		wg.Add(1)
		go func(seq int, city string) {
			defer wg.Done()
			counters := newConvergeShadowCounters()
			tick := newConvergeShadowTick(city, int64(seq+1), fixtureNow, true, counters)
			if tick == nil {
				return
			}
			sid := fmt.Sprintf("sess-%s", city)
			tick.captureDurable(sid, "tok", "dir/agent",
				durableFacts{canonicalIdentity: "dir/agent", now: fixtureNow},
				map[string]string{sessionpkg.CanonicalInstanceNameMetadata: "dir/agent"},
				convergePredictedValues{})
			tick.captureRuntime(sid, "desired", "dir/agent", convergeTriTrue, convergeTriTrue)
			recordLegacyCompareWrites(sid, "syncSessionBeads", map[string]string{
				sessionpkg.CanonicalInstanceNameMetadata: "dir/agent",
			})
			tick.finish(map[string]map[string]string{sid: {sessionpkg.CanonicalInstanceNameMetadata: "dir/agent"}})
		}(i, city)
	}
	wg.Wait()

	if convergeGlobalRecorder.Load() != nil {
		t.Fatal("global recorder must be nil after all concurrent ticks detach")
	}
}
