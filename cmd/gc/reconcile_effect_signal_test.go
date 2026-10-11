package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The drain signal's tests (CONTRACT v5 D1, D2, F4; C6b1).

// intentBegunAt is a controller half of reason at generation 3, begun ago
// before gatherNow.
func intentBegunAt(reason string, ago time.Duration) []string {
	return []string{session.DrainIntentReasonKey, reason, session.DrainIntentAtKey, rowAt(-ago), session.DrainIntentIncarnationKey, "3"}
}

// withPatrol sets the city's patrol interval, as gather reads it into the
// World, and its drift drain timeout.
func withPatrol(patrol, drift string) func(*World) {
	return func(w *World) {
		w.Env = &reconcileEnv{Cfg: &config.City{Daemon: config.DaemonConfig{PatrolInterval: patrol, DriftDrainTimeout: drift}}}
		w.Patrol = w.Env.patrol()
	}
}

// TestSignalNeedsGraceAndANewerPass (R30). Kills an immediate signal: a
// drain begun inside its patrol grace holds, with a pass due when the grace
// ends; past it, the signal writes legacy's stop-pending projection.
func TestSignalNeedsGraceAndANewerPass(t *testing.T) {
	it, due := decideDrain(t, intentBegunAt(drainOrphaned, 10*time.Second), undesired(drainOrphaned), withPatrol("30s", ""))
	if it.Kind != "" || it.Reason != decideStopActive || !due.Equal(gatherNow.Add(20*time.Second)) {
		t.Fatalf("inside the grace: %+v due %v, want held until %v", it, due, gatherNow.Add(20*time.Second))
	}
	it, _ = decideDrain(t, intentBegunAt(drainOrphaned, 30*time.Second), undesired(drainOrphaned), withPatrol("30s", ""))
	if it.Kind != intentSignal || it.Reason != decideDrainSignal+drainOrphaned || it.Patch["state_reason"] != session.DrainAckStopPendingReason {
		t.Fatalf("past the grace: %+v, want the stop-pending signal", it)
	}
}

// TestSignalPerReasonAuthorization (D2). Kills a drift, execution-stalled or
// intent drain that never reaches stop-pending, and one signaled without its
// authorization or under Keep.
func TestSignalPerReasonAuthorization(t *testing.T) {
	sleep := func(e *selectionEntry) { e.Desired, e.Reason = desireSleep, reasonIdleSleep }
	keep := func(e *selectionEntry) { e.Desired, e.Reason = desireKeep, reasonPartialRetain }
	for _, c := range []struct {
		name   string
		meta   []string
		entry  func(*selectionEntry)
		signal string
	}{
		{"execution-stalled", intentBegunAt(executionStalledDrainReason, time.Minute), nil, intentSignal},
		{"config-drift, the drift unresolvable", intentBegunAt(drainConfigDrift, time.Minute), nil, intentSignal},
		{"an intent that stands", append(intentBegunAt("rate-limit", time.Minute), "sleep_intent", "rate-limit"), nil, intentSignal},
		{"orphaned, still undesired", intentBegunAt(drainOrphaned, time.Minute), undesired(drainOrphaned), intentSignal},
		{"idle, still asleep", intentBegunAt(drainIdle, time.Minute), sleep, intentSignalFresh},
		{"idle under Keep", intentBegunAt(drainIdle, time.Minute), keep, ""},
		{"orphaned under Keep", intentBegunAt(drainOrphaned, time.Minute), keep, ""},
		{"idle-respawn, C6d's row", intentBegunAt(idleRespawnDrainReason, time.Minute), sleep, ""},
	} {
		it, _ := decideDrain(t, c.meta, c.entry)
		if it.Kind != c.signal || (c.signal == "" && it.Reason != decideStopActive) {
			t.Errorf("%s: %+v, want kind %q", c.name, it, c.signal)
		}
	}
}

// TestDriftSignalGraceCappedByDriftDrainTimeout (S-16). Kills a drift drain
// that waits past drift_drain_timeout: its grace is min(patrol,
// drift_drain_timeout), and every other reason waits out the patrol.
func TestDriftSignalGraceCappedByDriftDrainTimeout(t *testing.T) {
	long := withPatrol("10m", "2m")
	if it, _ := decideDrain(t, intentBegunAt(drainConfigDrift, 3*time.Minute), nil, long); it.Kind != intentSignal {
		t.Fatalf("drift past drift_drain_timeout: %+v, want the signal", it)
	}
	if it, due := decideDrain(t, intentBegunAt(drainConfigDrift, time.Minute), nil, long); it.Kind != "" || !due.Equal(gatherNow.Add(time.Minute)) {
		t.Fatalf("drift inside drift_drain_timeout: %+v due %v, want held a minute", it, due)
	}
	if it, _ := decideDrain(t, intentBegunAt(drainOrphaned, 3*time.Minute), undesired(drainOrphaned), long); it.Kind != "" {
		t.Fatalf("orphaned inside its patrol: %+v, want held", it)
	}
}

// TestAckSignalsAtOnce (D2's ack row, grace 0). Kills an acked drain that
// waits out a grace, and a bare ack (no controller half) never signaled.
func TestAckSignalsAtOnce(t *testing.T) {
	ack := []string{session.DrainAckIncarnationKey, "3", session.DrainAckAtKey, rowAt(-time.Second)}
	for _, c := range []struct {
		name string
		meta []string
	}{
		{"acked inside the grace", append(intentBegunAt(drainIdle, time.Second), ack...)},
		{"a bare ack", ack},
	} {
		it, _ := decideDrain(t, c.meta, func(e *selectionEntry) { e.Desired = desireWake })
		if it.Kind != intentSignal || it.Patch["state"] != string(session.StateDraining) {
			t.Errorf("%s: %+v, want the plain signal at once", c.name, it)
		}
	}
}

// runSignal decides gc row with meta and entry, checks the kind, and runs
// the signal's effect after outside writes.
func runSignal(t *testing.T, meta []string, entry func(*selectionEntry), kind string, outside func(beads.Store, intent)) (settlement, beads.Bead) {
	t.Helper()
	p, it, store, _ := admittedBeginOn(t, meta, entry)
	if it.Kind != kind {
		t.Fatalf("decided %+v, want %s", it, kind)
	}
	if outside != nil {
		outside(store, it)
	}
	s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
	got, err := store.Get(it.Key.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, got
}

// TestSignalFreshAttachDefers (R26, C8.9). Kills an idle or no-wake-reason
// signal that skips its fresh L3 or L4: attached or pending refuses with its
// cause and writes nothing; detached with nothing pending signals.
func TestSignalFreshAttachDefers(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(*fenceLeaf)
		cause string
	}{
		{"attached", func(l *fenceLeaf) { l.SetAttached("s-gc-1", true) }, fenceAttached},
		{"pending", func(l *fenceLeaf) {
			l.SetPendingInteraction("s-gc-1", &runtime.PendingInteraction{RequestID: "r", Kind: "approval"})
		}, fencePending},
		{"detached, nothing pending", func(*fenceLeaf) {}, ""},
	} {
		p, it, store, leaf := admittedBeginOn(t, intentBegunAt(drainIdle, time.Minute), func(e *selectionEntry) { e.Desired, e.Reason = desireSleep, reasonIdleSleep })
		if it.Kind != intentSignalFresh {
			t.Fatalf("%s: %+v, want the fresh signal", c.name, it)
		}
		c.setup(leaf)
		s := runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
		got, _ := store.Get(it.Key.ID)
		pending := isDrainAckStopPendingInfo(censusRowOf(t, got).Info)
		switch {
		case c.cause != "" && (s.Outcome != settledRefused || s.Cause != c.cause || pending):
			t.Fatalf("%s: %+v (stop-pending %v), want refused %q and no signal", c.name, s, pending, c.cause)
		case c.cause == "" && (s.Outcome != settledLanded || !pending):
			t.Fatalf("%s: %+v, want the signal landed", c.name, s)
		}
	}
}

// TestSignalPatchIsLegacyReadable (I7, D1). Kills a signal legacy cannot
// read after a rollback, and one that drops the request: the landed row is
// legacy's stop-pending state, with drain_at for D6's alert, and the next
// pass reads it as the signaled request, so A20 leaves it to A4.
func TestSignalPatchIsLegacyReadable(t *testing.T) {
	s, got := runSignal(t, intentBegunAt(drainOrphaned, time.Minute), undesired(drainOrphaned), intentSignal, nil)
	if s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	row := censusRowOf(t, got)
	if !isDrainAckStopPendingInfo(row.Info) || got.Metadata["drain_at"] != gatherNow.Format(time.RFC3339) {
		t.Fatalf("row %v, want legacy's stop-pending state with drain_at", got.Metadata)
	}
	if req, ok := activeStop(row); !ok || req.Phase != stopSignaled || req.Reason != drainOrphaned || req.Synthesized {
		t.Fatalf("activeStop = %+v %v, want the orphaned request signaled", req, ok)
	}
}

// TestSignalRefusesWhenAccessorSaysNoRequest (D1 rule 2, R16). Kills a
// signal over an operator's suspend, and one sent after the drain was voided
// or canceled: the effect decides again on the fresh row and writes nothing.
func TestSignalRefusesWhenAccessorSaysNoRequest(t *testing.T) {
	for _, c := range []struct {
		name  string
		write map[string]string
	}{
		{"an operator's suspend", map[string]string{"state": string(session.StateSuspended), "sleep_intent": "user-hold"}},
		{"the drain voided", stopCancelPatch()},
	} {
		s, got := runSignal(t, intentBegunAt(drainOrphaned, time.Minute), undesired(drainOrphaned), intentSignal, func(store beads.Store, it intent) {
			if err := store.SetMetadataBatch(it.Key.ID, c.write); err != nil {
				t.Fatal(err)
			}
		})
		if s.Outcome == settledLanded || got.Metadata["state_reason"] == session.DrainAckStopPendingReason {
			t.Errorf("%s: %+v row %v, want no signal", c.name, s, got.Metadata)
		}
	}
}

// TestSignalWaitNeverPinnedBySkew (#7607 review; INC-003's rule that skew
// never pins a row). Kills a requested drain held for the whole skew of a
// drain_intent_at a clock wrote ahead of now, and one an unparseable stamp
// holds: either is due at once, and no wait exceeds one grace.
func TestSignalWaitNeverPinnedBySkew(t *testing.T) {
	ahead := []string{session.DrainIntentReasonKey, drainOrphaned, session.DrainIntentAtKey, rowAt(time.Hour), session.DrainIntentIncarnationKey, "3"}
	garbled := []string{session.DrainIntentReasonKey, drainOrphaned, session.DrainIntentAtKey, "yesterday-ish", session.DrainIntentIncarnationKey, "3"}
	for name, meta := range map[string][]string{"an hour ahead": ahead, "unparseable": garbled} {
		if it, _ := decideDrain(t, meta, undesired(drainOrphaned), withPatrol("30s", "")); it.Kind != intentSignal {
			t.Errorf("%s: %+v, want the signal at once", name, it)
		}
	}
	if it, due := decideDrain(t, intentBegunAt(drainOrphaned, time.Second), undesired(drainOrphaned), withPatrol("30s", "")); it.Kind != "" || due.After(gatherNow.Add(30*time.Second)) {
		t.Fatalf("just begun: %+v due %v, want held at most one grace", it, due)
	}
}

// TestSignalPremiseIsTheDrainBasis (#7607 review, mutant G9). Kills a signal
// behind the default premise, which an agent's heartbeat starves: a
// heartbeat between the pass and the CAS lands the signal; an operator's
// intent or a wake request refuses it.
func TestSignalPremiseIsTheDrainBasis(t *testing.T) {
	for _, c := range []struct {
		name  string
		write map[string]string
		lands bool
	}{
		{"a heartbeat", map[string]string{"held_until": gatherNow.Add(time.Minute).Format(time.RFC3339)}, true},
		{"an operator's intent", map[string]string{"sleep_intent": "user-hold"}, false},
		{"a wake request", map[string]string{"wake_request": "explicit"}, false},
	} {
		s, got := runSignal(t, intentBegunAt(drainOrphaned, time.Minute), undesired(drainOrphaned), intentSignal, func(store beads.Store, it intent) {
			if err := store.SetMetadataBatch(it.Key.ID, c.write); err != nil {
				t.Fatal(err)
			}
		})
		signaled := isDrainAckStopPendingInfo(censusRowOf(t, got).Info)
		if landed := s.Outcome == settledLanded; landed != c.lands || signaled != c.lands {
			t.Errorf("%s: %+v (signaled %v), want landed=%v", c.name, s, signaled, c.lands)
		}
	}
}
