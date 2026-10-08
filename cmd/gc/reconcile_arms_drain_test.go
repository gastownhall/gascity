package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The drain arms' tests (CONTRACT v5 D2; arms A19 and A20).

// decideDrain decides gc-1 (drainRow with meta) with its entry alive and
// set by entry, under w's extra setup.
func decideDrain(t *testing.T, meta []string, entry func(*selectionEntry), world ...func(*World)) (intent, time.Time) {
	t.Helper()
	w, a := rowWorld(t, drainRow(meta...))
	if entry != nil {
		entry(a.Snapshot.Entries[rowKeyOf("gc-1")])
	}
	for _, f := range world {
		f(w)
	}
	return decideRow(w, a, rowKeyOf("gc-1"))
}

// undesired is a Drain entry for reason.
func undesired(reason string) func(*selectionEntry) {
	return func(e *selectionEntry) { e.Desired, e.DrainReason = desireDrain, reason }
}

// TestResumeVoidsSuspendedDrain (scenario R16, owner ruling 4). Kills a
// suspended drain that outlives the resume, or one voided while the row is
// still suspended; the void clears the controller half alone.
func TestResumeVoidsSuspendedDrain(t *testing.T) {
	meta := intentAt(drainSuspended, "3")
	if it, _ := decideDrain(t, meta, undesired(drainSuspended)); it.Kind != "" {
		t.Fatalf("still suspended: %+v, want the request kept", it)
	}
	store, _ := stampedMem(t, gate.Require)
	b, err := store.Create(drainRow(meta...))
	if err != nil {
		t.Fatal(err)
	}
	k := rowKey{Leg: rowLeg, ID: b.ID}
	w := &World{Now: gatherNow, Census: readCensus(t, gatherNow, censusLegs(rowLeg, store)), LegStores: map[string]beads.Store{rowLeg: store}}
	a := &allocDecision{Snapshot: &selectionSnapshot{Entries: map[rowKey]*selectionEntry{k: {Key: k, Liveness: livenessAlive, Desired: desireWake}}}}
	it, _ := decideRow(w, a, k)
	if it.Kind != intentDrainVoid || it.Reason != decideDrainVoid+drainSuspended {
		t.Fatalf("resumed: %+v, want the void", it)
	}
	if s := effectRegistry[it.Kind](newEffectPass(w, a), it)(context.Background()); s.Outcome != settledLanded {
		t.Fatalf("void settlement %+v, want landed", s)
	}
	got, _ := store.Get(b.ID)
	if got.Metadata[session.DrainIntentReasonKey] != "" || got.Metadata[session.DrainIntentIncarnationKey] != "" || got.Metadata["state"] != "active" {
		t.Fatalf("row after the void %v, want the request cleared and the state kept", got.Metadata)
	}
}

// TestConfigDriftResolvedCancels (legacy cancelSessionConfigDriftDrainInfo).
// Kills a drift drain that survives its drift's resolution or an attached
// deferral, one canceled while the drift persists, and a lost pending lens.
func TestConfigDriftResolvedCancels(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	tp := TemplateParams{TemplateName: "worker", Command: "run"}
	info := censusRowOf(t, drainRow()).Info
	current := runtime.CoreFingerprint(sessionCoreConfigForHashInfo(tp, info))
	resolved := func(w *World) {
		w.Env = &reconcileEnv{Cfg: cfg}
		w.Templates = &templateMemo{entries: map[templateMemoKey]templateResolution{templateMemoKeyOf(info): {TP: tp}}}
	}
	attached := func(w *World) {
		w.Observed = map[rowKey]rowObservation{rowKeyOf("gc-1"): {Liveness: livenessAlive, Attached: true}}
	}
	pending := func(e *selectionEntry) { e.WakeReasons = []WakeReason{WakePending} }
	deferred := []string{"attached_config_drift_deferred_key", "stale:" + current, "attached_config_drift_deferred_at", rowAt(-time.Minute)}
	for _, c := range []struct {
		name   string
		hash   string
		extra  []string
		entry  func(*selectionEntry)
		world  []func(*World)
		cancel bool
	}{
		{name: "drift persists", hash: "stale", world: []func(*World){resolved}},
		{name: "template unresolved", hash: current},
		{name: "drift resolved", hash: current, world: []func(*World){resolved}, cancel: true},
		{name: "attached", hash: "stale", world: []func(*World){resolved, attached}, cancel: true},
		{name: "recently attached-deferred", hash: "stale", extra: deferred, world: []func(*World){resolved}, cancel: true},
		{name: "pending interaction", hash: "stale", entry: pending, world: []func(*World){resolved}, cancel: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			meta := slices.Concat(intentAt(drainConfigDrift, "3"), []string{"started_config_hash", c.hash}, c.extra)
			it, _ := decideDrain(t, meta, c.entry, c.world...)
			if got := it.Kind == intentDrainCancel; got != c.cancel || (!c.cancel && it.Kind != "") {
				t.Fatalf("%+v, want cancel=%v", it, c.cancel)
			}
		})
	}
}

// TestDrainCancelLensesMatchLegacy (DRAIN-045, 046, 047, 501). On a row
// woken again, or held under Keep, every reason cancels exactly when
// legacy's lenses do; otherwise it holds where still authorized or under
// Keep, cancels a drift drain, and voids the rest (D2 as amended).
func TestDrainCancelLensesMatchLegacy(t *testing.T) {
	lenses := map[string][]WakeReason{"pending": {WakePending}, "work": {WakeWork}, "config": {WakeConfig}}
	for _, reason := range []string{drainIdle, reasonNoWake, drainOrphaned, drainConfigDrift, executionStalledDrainReason, "user-hold"} {
		for name, wake := range lenses {
			for _, keep := range []bool{false, true} {
				entry := func(e *selectionEntry) {
					e.Desired, e.Reason, e.WakeReasons = desireWake, reasonAssignedWork, wake
					if keep {
						e.Desired, e.Reason = desireKeep, reasonPartialRetain
					}
				}
				lens := (containsWakeReason(wake, WakePending) && pendingDrainReasonCancelable(reason)) ||
					(!keep && containsWakeReason(wake, WakeWork) && assignedWorkDrainReasonCancelable(reason)) ||
					drainReasonCancelable(reason)
				want := intentDrainVoid
				switch {
				case keep && drainRank[reason] > 0 && reason != drainSuspended:
					want = "" // hold under Keep
				case lens:
					want = intentDrainCancel
				case reason == drainConfigDrift || reason == executionStalledDrainReason:
					want = "" // still authorized
				}
				if it, _ := decideDrain(t, intentAt(reason, "3"), entry); it.Kind != want {
					t.Errorf("%s, %s lens, keep=%v: %+v, want kind %q", reason, name, keep, it, want)
				}
			}
		}
	}
	// SESS-617: a heartbeat hold cancels a cancelable drain.
	hb := func(e *selectionEntry) { e.Desired = desireSleep }
	if it, _ := decideDrain(t, append(intentAt(reasonNoWake, "3"), "held_until", rowAt(time.Hour)), hb); it.Kind != intentDrainCancel {
		t.Fatalf("heartbeat hold: %+v, want the cancel", it)
	}
}

// TestLostAuthorizationVoids (D2 as amended: "lens; otherwise void"). Kills
// a request left standing once nothing authorizes it and no lens fires: an
// orphaned drain on a row selected again, a no-wake-reason drain the
// allocation now sleeps for a weaker reason, and an intent drain whose
// intent was cleared are each voided; the next pass begins the current
// reason (A20).
func TestLostAuthorizationVoids(t *testing.T) {
	sleep := func(reason string) func(*selectionEntry) {
		return func(e *selectionEntry) { e.Desired, e.Reason = desireSleep, reason }
	}
	for _, c := range []struct {
		name, reason string
		entry        func(*selectionEntry)
	}{
		{"orphaned row selected again", drainOrphaned, sleep("")},
		{"no-wake-reason now idle", reasonNoWake, sleep(reasonIdleSleep)},
		{"wait-hold cleared", "wait-hold", sleep("")},
	} {
		it, _ := decideDrain(t, intentAt(c.reason, "3"), c.entry)
		if it.Kind != intentDrainVoid || it.Reason != decideDrainVoid+c.reason || it.Patch[session.DrainIntentReasonKey] != "" {
			t.Errorf("%s: %+v, want the void", c.name, it)
		}
	}
}

// TestLensCancelsAnAuthorizedDrain (DRAIN-047, SESS-615). Kills a lens
// checked only once authorization is lost: a wait-hold drain whose intent
// still stands, and an idle drain the allocation still sleeps, each cancel
// once a wake reason appears.
func TestLensCancelsAnAuthorizedDrain(t *testing.T) {
	waitHold := func(e *selectionEntry) { e.Desired, e.WakeReasons = desireWake, []WakeReason{WakeWork} }
	if it, _ := decideDrain(t, append(intentAt("wait-hold", "3"), "sleep_intent", "wait-hold"), waitHold); it.Kind != intentDrainCancel {
		t.Errorf("wait-hold with a wake reason: %+v, want the cancel", it)
	}
	idle := func(e *selectionEntry) {
		e.Desired, e.Reason, e.WakeReasons = desireSleep, reasonIdleSleep, []WakeReason{WakeConfig}
	}
	if it, _ := decideDrain(t, intentAt(drainIdle, "3"), idle); it.Kind != intentDrainCancel {
		t.Errorf("idle drain with a wake reason: %+v, want the cancel", it)
	}
}
