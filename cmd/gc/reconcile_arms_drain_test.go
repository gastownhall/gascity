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
	"github.com/gastownhall/gascity/internal/suspensionstate"
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

func wokeAt(d time.Duration) []string { return []string{"last_woke_at", rowAt(d)} }

// TestDecideRow_INC003_UndesiredWokeUnder5mDefers. Kills a grace for one
// undesired reason only, or a deadline other than the grace's end.
func TestDecideRow_INC003_UndesiredWokeUnder5mDefers(t *testing.T) {
	for _, reason := range []string{drainOrphaned, drainSuspended} {
		it, next := decideDrain(t, wokeAt(-time.Minute), undesired(reason))
		if it.Kind != "" || it.Reason != decideWakeGrace || !next.Equal(gatherNow.Add(4*time.Minute)) {
			t.Fatalf("%s woken 1m ago: %+v next %v, want the grace until +4m", reason, it, next)
		}
	}
}

// TestDecideRow_INC003_DrainsAfterGrace. Kills a grace that never ends, or
// one that an empty or unparseable last_woke_at earns.
func TestDecideRow_INC003_DrainsAfterGrace(t *testing.T) {
	for _, reason := range []string{drainOrphaned, drainSuspended} {
		for _, woke := range [][]string{wokeAt(-5 * time.Minute), wokeAt(-time.Hour), nil, {"last_woke_at", "yesterday"}} {
			it, _ := decideDrain(t, woke, undesired(reason))
			if it.Kind != intentDrainBegin || it.Reason != decideDrainBegin+reason || it.Patch[drainIntentReasonKey] != reason {
				t.Fatalf("%s woke %v: %+v, want the begin", reason, woke, it)
			}
		}
	}
}

// TestINC003SkewGuardBothDirections. Kills a future last_woke_at that pins
// a row forever: up to 5m ahead defers, 5m or more drains.
func TestINC003SkewGuardBothDirections(t *testing.T) {
	for _, c := range []struct {
		ahead    time.Duration
		deferred bool
	}{{time.Minute, true}, {4*time.Minute + 59*time.Second, true}, {5 * time.Minute, false}, {time.Hour, false}} {
		it, next := decideDrain(t, wokeAt(c.ahead), undesired(drainOrphaned))
		switch {
		case c.deferred && (it.Reason != decideWakeGrace || !next.Equal(gatherNow.Add(c.ahead+wakeUndesiredGrace))):
			t.Fatalf("woken %v ahead: %+v next %v, want the grace", c.ahead, it, next)
		case !c.deferred && it.Kind != intentDrainBegin:
			t.Fatalf("woken %v ahead: %+v, want the begin", c.ahead, it)
		}
	}
}

// TestOperatorSuspendSkipsINC003Grace (scenario R16b, v5.4). A rig, an agent
// and a city suspend of a row woken a minute ago each drain on the first
// pass through E2b's operatorSuspendCause; an unsuspended orphan still
// waits. Kills a suspend that cannot drain a just-woken row.
func TestOperatorSuspendSkipsINC003Grace(t *testing.T) {
	on := true
	const cityPath = "/city"
	rig := config.Rig{Name: "myrig", Path: "/city/myrig"}
	for _, c := range []struct {
		name, template string
		cfg            config.City
		st             suspensionstate.State
		grace          bool
	}{
		{
			name: "rig", template: "myrig/polecat", cfg: config.City{Rigs: []config.Rig{rig}, Agents: []config.Agent{{Name: "polecat", Dir: "myrig"}}},
			st: suspensionstate.State{Rigs: map[string]suspensionstate.Override{"myrig": {Suspended: &on}}},
		},
		{name: "agent", template: "worker", cfg: config.City{Agents: []config.Agent{{Name: "worker", Suspended: true}}}},
		{name: "city", template: "worker", cfg: config.City{Agents: []config.Agent{{Name: "worker"}}}, st: suspensionstate.State{City: suspensionstate.Override{Suspended: &on}}},
		{name: "unsuspended orphan", template: "worker", cfg: config.City{Agents: []config.Agent{{Name: "worker"}}}, grace: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			it, _ := decideDrain(t, append(wokeAt(-time.Minute), "template", c.template), undesired(drainOrphaned), func(w *World) {
				w.OperatorSuspend = operatorSuspendCauses(&c.cfg, cityPath, w.Census.Canonical(), c.st)
			})
			if got := it.Reason == decideWakeGrace; got != c.grace || (!c.grace && it.Kind != intentDrainBegin) {
				t.Fatalf("%+v, want grace=%v", it, c.grace)
			}
		})
	}
}

// TestResumeVoidsSuspendedDrain (scenario R16, owner ruling 4). Kills a
// suspended drain that outlives the resume, or one voided while the row is
// still suspended; the void clears the controller half alone.
func TestResumeVoidsSuspendedDrain(t *testing.T) {
	meta := intentAt(drainSuspended, "3")
	if it, _ := decideDrain(t, meta, undesired(drainSuspended)); it.Kind != "" || it.Reason != decideStopActive {
		t.Fatalf("still suspended: %+v, want the request held", it)
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
	if s := rowWriteEffect(newEffectPass(w, a), it)(context.Background()); s.Outcome != settledLanded {
		t.Fatalf("void settlement %+v, want landed", s)
	}
	got, _ := store.Get(b.ID)
	if got.Metadata[drainIntentReasonKey] != "" || got.Metadata[drainIntentIncarnationKey] != "" || got.Metadata["state"] != "active" {
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
			if got := it.Kind == intentDrainCancel; got != c.cancel || (!c.cancel && it.Reason != decideStopActive) {
				t.Fatalf("%+v, want cancel=%v", it, c.cancel)
			}
		})
	}
}

// TestDrainCancelLensesMatchLegacy (DRAIN-045, 046, 047, 501). On a row
// woken again, every reason cancels exactly when legacy's lenses do, in
// legacy's order; under Keep the lens reasons hold.
func TestDrainCancelLensesMatchLegacy(t *testing.T) {
	lenses := map[string][]WakeReason{"pending": {WakePending}, "work": {WakeWork}, "config": {WakeConfig}}
	for _, reason := range []string{drainIdle, reasonNoWake, drainOrphaned, drainConfigDrift, executionStalledDrainReason, "user-hold"} {
		for name, wake := range lenses {
			for _, keep := range []bool{false, true} {
				entry := func(e *selectionEntry) {
					e.Desired, e.Reason, e.WakeReasons = desireWake, wakeAssignedWork, wake
					if keep {
						e.Desired, e.Reason = desireKeep, reasonPartialRetain
					}
				}
				it, _ := decideDrain(t, intentAt(reason, "3"), entry)
				want := (containsWakeReason(wake, WakePending) && pendingDrainReasonCancelable(reason)) ||
					(!keep && containsWakeReason(wake, WakeWork) && assignedWorkDrainReasonCancelable(reason)) ||
					drainReasonCancelable(reason)
				if keep && reason != "user-hold" && reason != drainConfigDrift {
					want = false // hold under Keep (D2)
				}
				if got := it.Kind == intentDrainCancel; got != want {
					t.Errorf("%s, %s lens, keep=%v: %+v, want cancel=%v", reason, name, keep, it, want)
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

// TestNoDrainOnKeepOrUnknownLiveness (P-3, GUAR-053). Kills a begin under
// Keep, on a runtime not read alive, on a row with open assigned work
// (SESS-074), or on a row that no longer claims its runtime.
func TestNoDrainOnKeepOrUnknownLiveness(t *testing.T) {
	for _, c := range []struct {
		name  string
		meta  []string
		entry func(*selectionEntry)
		want  string
	}{
		{name: "keep", entry: func(e *selectionEntry) { e.Desired = desireKeep }, want: decideNoAction},
		{name: "unknown", entry: func(e *selectionEntry) { e.Desired, e.Liveness = desireDrain, livenessUnknown }, want: decideLivenessUnknown},
		{name: "gone", entry: func(e *selectionEntry) { e.Desired, e.Liveness = desireDrain, livenessGone }, want: decideNoAction},
		{name: "assigned work", entry: func(e *selectionEntry) { undesired(drainOrphaned)(e); e.AssignedWork = &assignedWorkView{BeadID: "w"} }, want: decideDrainWorkKept},
		{name: "asleep", meta: []string{"state", "asleep"}, entry: undesired(drainOrphaned), want: decideNoAction},
	} {
		it, _ := decideDrain(t, c.meta, c.entry)
		if it.Kind != "" || it.Reason != c.want {
			t.Errorf("%s: %+v, want hold %q", c.name, it, c.want)
		}
	}
	// The reason selection (SESS-618): idle drains are the fresh kind.
	idle := func(e *selectionEntry) { e.Desired, e.Reason = desireSleep, reasonIdleSleep }
	if it, _ := decideDrain(t, nil, idle); it.Kind != intentDrainBeginFresh || it.Patch[drainIntentReasonKey] != drainIdle {
		t.Fatalf("idle sleep: %+v, want a fresh idle begin", it)
	}
}
