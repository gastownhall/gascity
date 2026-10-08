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
				w.OperatorSuspend = operatorSuspendCauses(&c.cfg, cityPath, w.Census.Canonical(), c.st, gatherNow)
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
		if it.Kind != intentDrainVoid || it.Reason != decideDrainVoid+c.reason || it.Patch[drainIntentReasonKey] != "" {
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

// TestNoDrainOnKeepOrUnknownLiveness (P-3, GUAR-053). Kills a begin under
// Keep, on a runtime not read alive, on a row with open assigned work
// (SESS-074), on a row that no longer claims its runtime, on a Sleep row
// under a heartbeat hold (SESS-617), or on an operator's user-hold row,
// which C6b2's direct stop owns.
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
		{name: "open work", entry: func(e *selectionEntry) { undesired(drainOrphaned)(e); e.OpenWork = &assignedWorkView{BeadID: "w"} }, want: decideDrainWorkKept},
		{name: "asleep", meta: []string{"state", "asleep"}, entry: undesired(drainOrphaned), want: decideNoAction},
		{name: "heartbeat hold (SESS-617)", meta: []string{"held_until", rowAt(time.Minute)}, entry: func(e *selectionEntry) { e.Desired = desireSleep }, want: decideNoAction},
		{name: "user-hold intent (C6b2's)", meta: []string{"sleep_intent", "user-hold"}, entry: undesired(drainOrphaned), want: decideNoAction},
	} {
		it, _ := decideDrain(t, c.meta, c.entry)
		if it.Kind != "" || it.Reason != c.want {
			t.Errorf("%s: %+v, want hold %q", c.name, it, c.want)
		}
	}
}

// TestINC003IsDrainOnly. Kills the grace applied to a Sleep entry: legacy's
// grace guards only the undesired (orphaned, suspended) begin.
func TestINC003IsDrainOnly(t *testing.T) {
	sleep := func(e *selectionEntry) { e.Desired = desireSleep }
	if it, _ := decideDrain(t, wokeAt(-time.Minute), sleep); it.Kind != intentDrainBeginFresh || it.Reason != decideDrainBegin+reasonNoWake {
		t.Fatalf("Sleep woken 1m ago: %+v, want the no-wake-reason begin", it)
	}
}

// TestWokenRowClearsIdleStopPending (SESS-616). Kills a leftover legacy
// idle-stop-pending mark surviving a re-wake, where a later idle decision
// would bypass its probe on it.
func TestWokenRowClearsIdleStopPending(t *testing.T) {
	wake := func(e *selectionEntry) { e.Desired = desireWake }
	it, _ := decideDrain(t, []string{"sleep_intent", sleepIntentIdle}, wake)
	if it.Kind != intentRowHeal || it.Reason != decideIdleStopClear || len(it.Patch) != 1 || it.Patch["sleep_intent"] != "" {
		t.Fatalf("%+v, want the sleep_intent clear alone", it)
	}
}

// TestEmptyGenerationDrainsAtZero. Kills a row with no generation pinned
// undrained: legacy drains it at generation 0, and so does the request.
func TestEmptyGenerationDrainsAtZero(t *testing.T) {
	it, _ := decideDrain(t, []string{"generation", ""}, undesired(drainOrphaned))
	if it.Kind != intentDrainBegin || it.Patch[drainIntentIncarnationKey] != "0" {
		t.Fatalf("%+v, want a begin at incarnation 0", it)
	}
	if req, ok := activeStop(censusRowOf(t, drainRow(append([]string{"generation", ""}, intentAt(drainOrphaned, "0")...)...))); !ok || req.Phase != stopRequested {
		t.Fatalf("activeStop = %+v, %v; want the request at generation 0", req, ok)
	}
}

// TestIdleBeginGates (SESS-621, decide side). Kills an idle begin for a
// session whose idleness cannot be proven: a non-interactive one begins, an
// interactive one only with Full sleep capability, and a row with no
// resolved policy never.
func TestIdleBeginGates(t *testing.T) {
	idle := func(e *selectionEntry) { e.Desired, e.Reason = desireSleep, reasonIdleSleep }
	for _, c := range []struct {
		name   string
		policy *resolvedSessionSleepPolicy
		begins bool
	}{
		{"no policy", nil, false},
		{"non-interactive", &resolvedSessionSleepPolicy{Class: config.SessionSleepNonInteractive}, true},
		{"interactive, not Full", &resolvedSessionSleepPolicy{Class: config.SessionSleepInteractiveResume, Capability: runtime.SessionSleepCapabilityDisabled}, false},
		{"interactive, Full", &resolvedSessionSleepPolicy{Class: config.SessionSleepInteractiveResume, Capability: runtime.SessionSleepCapabilityFull}, true},
	} {
		it, _ := decideDrain(t, nil, idle, func(w *World) {
			if c.policy != nil {
				w.SleepPolicies = map[string]resolvedSessionSleepPolicy{"gc-1": *c.policy}
			}
		})
		if begins := it.Kind == intentDrainBeginFresh; begins != c.begins || (!begins && it.Reason != decideIdleUnprovable) {
			t.Errorf("%s: %+v, want begin=%v", c.name, it, c.begins)
		}
	}
}

// TestDrainBeginKeepsUndesiredRowWithOpenWork (SESS-074), through the real
// allocation. Kills the keep read off the awake decision's anchor instead
// of legacy's predicate: a suspended agent's or a suspended rig's row, and
// an unselected row, with open work that is not ready are kept open.
func TestDrainBeginKeepsUndesiredRowWithOpenWork(t *testing.T) {
	agent := allocPoolAgent("worker", 3)
	agent.Suspended = true
	rigCfg := &config.City{
		Rigs:   []config.Rig{{Name: "r", Path: "/rigs/r"}},
		Agents: []config.Agent{{Name: "worker", Dir: "r", MaxActiveSessions: intPtr(3)}},
	}
	for _, c := range []struct {
		name, template string
		cfg            *config.City
		rigSuspended   bool
	}{
		{"agent suspended", "worker", &config.City{Agents: []config.Agent{agent}}, false},
		{"rig suspended", "r/worker", rigCfg, true},
		{"unselected", "worker", &config.City{Agents: []config.Agent{allocPoolAgent("worker", 3)}}, false},
	} {
		f := newAllocFixture(t, c.cfg).sessions(poolRow("gc-1", c.template, 1, "active")).alive("s-gc-1", InventoryAttrs{AttachedKnown: true})
		if c.rigSuspended {
			f.in.SuspendedRigPaths = map[string]bool{"/rigs/r": true}
		}
		f.in.Demand.AssignedWork = []beads.Bead{{ID: "w-1", Status: "open", Assignee: "gc-1"}} // blocked: not ready
		f.in.Demand.AssignedStoreRefs = []string{""}
		d := f.decideSelecting()
		k := rowKey{Leg: allocSessionsLeg, ID: "gc-1"}
		if e := d.Snapshot.Entries[k]; e.Desired != desireDrain || e.OpenWork == nil {
			t.Fatalf("%s: entry %s %+v, want a Drain with its open work", c.name, e.Desired, e.OpenWork)
		}
		w := &World{Now: allocNow, Census: f.census()}
		if it, _ := decideRow(w, &d, k); it.Kind != "" || it.Reason != decideDrainWorkKept {
			t.Errorf("%s: %+v, want the row kept open", c.name, it)
		}
	}
}

// TestGatherReadsOperatorSuspendOnlyInsideGrace (v5.4; review: 263 ms at
// 1,000 rows). Kills the operator-suspend resolver run for every row: of two
// rows of a suspended agent, only the one inside INC-003's grace is read.
func TestGatherReadsOperatorSuspendOnlyInsideGrace(t *testing.T) {
	f := newGatherFixture(t,
		sessionRow("gc-1", "template", "worker", "session_name", "s-gc-1", "state", "active", "last_woke_at", rowAt(-time.Minute)),
		sessionRow("gc-2", "template", "worker", "session_name", "s-gc-2", "state", "active", "last_woke_at", rowAt(-time.Hour)))
	env := *f.cur.Load()
	cfg := *env.Cfg
	cfg.Agents = slices.Clone(cfg.Agents)
	cfg.Agents[0].Suspended = true
	env.Cfg = &cfg
	f.cur.Store(&env)
	w := f.gather(t)
	if len(w.OperatorSuspend) != 1 || w.OperatorSuspend[rowKeyOf("gc-1")] != "agent" {
		t.Fatalf("OperatorSuspend = %v, want only gc-1's agent cause", w.OperatorSuspend)
	}
}

// TestDrainBeginsUnderHoldOrQuarantineAsLegacy (owner ruling: an unexpired
// hold or quarantine is not operator-dormant, D1 rule 2). Kills a begin
// withheld where legacy drains: a live quarantined row the allocation
// sleeps, and an orphaned row under a hold.
func TestDrainBeginsUnderHoldOrQuarantineAsLegacy(t *testing.T) {
	for _, c := range []struct {
		name, reason string
		meta         []string
		entry        func(*selectionEntry)
	}{
		{"live quarantined row", reasonNoWake, []string{"quarantined_until", rowAt(time.Minute), "sleep_reason", "quarantine"}, func(e *selectionEntry) { e.Desired = desireSleep }},
		{"orphaned row under a hold", drainOrphaned, []string{"held_until", rowAt(time.Minute), "sleep_reason", "user-hold"}, undesired(drainOrphaned)},
	} {
		it, _ := decideDrain(t, c.meta, c.entry)
		if it.Reason != decideDrainBegin+c.reason || it.Patch[drainIntentReasonKey] != c.reason {
			t.Errorf("%s: %+v, want the %s begin", c.name, it, c.reason)
		}
	}
}
