package main

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The drain-begin effect's tests (CONTRACT v5 D1, D2, F4, C8.9).

// admittedBegin seeds gc row (drainRow) on a require-stamped store, which
// is also the city's work store, with a live runtime on a fence leaf,
// decides it with entry under world, and returns the pass that admitted the
// begin, the begin, the store and the leaf.
func admittedBegin(t *testing.T, entry func(*selectionEntry), world ...func(*World)) (*effectPass, intent, beads.Store, *fenceLeaf) {
	t.Helper()
	store, _ := stampedMem(t, gate.Require)
	b, err := store.Create(drainRow("last_woke_at", rowAt(-time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	leaf := newFenceLeaf(runtime.ProfileTmux)
	startRuntime(t, leaf.Fake, "s-gc-1", map[string]string{"GC_SESSION_ID": b.ID, "GC_INSTANCE_TOKEN": "tok-3"})
	k := rowKey{Leg: rowLeg, ID: b.ID}
	w := &World{
		Now: gatherNow, CityPath: t.TempDir(), Env: &reconcileEnv{SP: leaf.P, Cfg: &config.City{Agents: []config.Agent{{Name: "worker"}}}},
		Census: readCensus(t, gatherNow, censusLegs(rowLeg, store)), LegStores: map[string]beads.Store{rowLeg: store}, SessionsStore: store,
		SleepPolicies: map[string]resolvedSessionSleepPolicy{b.ID: {Class: config.SessionSleepNonInteractive}},
	}
	w.Obs, w.ObsMaxAge = observedOwn(w, b.ID), observeMaxAge
	for _, f := range world {
		f(w)
	}
	e := &selectionEntry{Key: k, Liveness: livenessAlive}
	entry(e)
	a := &allocDecision{Snapshot: &selectionSnapshot{Entries: map[rowKey]*selectionEntry{k: e}}}
	it, _ := decideRow(w, a, k)
	return newEffectPass(w, a), it, store, leaf
}

func runBegin(p *effectPass, it intent) settlement {
	return runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
}

// TestDrainBeginIdleNeedsFreshDetachAndNoPending (C8.9, R26). Kills an idle
// or no-wake-reason begin that skips the fresh attach or pending read, or
// writes while either holds.
func TestDrainBeginIdleNeedsFreshDetachAndNoPending(t *testing.T) {
	for _, reason := range []string{reasonIdleSleep, ""} {
		sleep := func(e *selectionEntry) { e.Desired, e.Reason = desireSleep, reason }
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
			p, it, store, leaf := admittedBegin(t, sleep)
			if it.Kind != intentDrainBeginFresh {
				t.Fatalf("%q: %+v, want the fresh begin", reason, it)
			}
			c.setup(leaf)
			s := runBegin(p, it)
			got, _ := store.Get(it.Key.ID)
			switch {
			case c.cause != "" && (s.Outcome != settledRefused || s.Cause != c.cause || got.Metadata[session.DrainIntentReasonKey] != ""):
				t.Fatalf("%q %s: %+v %v, want refused %q and no request", reason, c.name, s, got.Metadata, c.cause)
			case c.cause == "" && (s.Outcome != settledLanded || got.Metadata[session.DrainIntentReasonKey] == ""):
				t.Fatalf("%q %s: %+v %v, want the request landed", reason, c.name, s, got.Metadata)
			}
		}
	}
}

// TestDrainBeginOrphanedProceedsWhileAttached (D2). Kills a fresh-leg probe
// on the undesired reasons: an orphaned begin is a plain row write and lands
// on an attached runtime (the signal and the stop verb fence it later).
func TestDrainBeginOrphanedProceedsWhileAttached(t *testing.T) {
	p, it, store, leaf := admittedBegin(t, undesired(drainOrphaned))
	leaf.SetAttached("s-gc-1", true)
	if it.Kind != intentDrainBegin {
		t.Fatalf("%+v, want the plain begin", it)
	}
	if s := runBegin(p, it); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	if got, _ := store.Get(it.Key.ID); got.Metadata[session.DrainIntentReasonKey] != drainOrphaned {
		t.Fatalf("row %v, want the orphaned request", got.Metadata)
	}
}

// TestDrainRequestWritesNewKeysOnly (v5 R1, I7). Kills BeginDrainPatch's
// state=draining, which legacy skips as an unknown state after a rollback:
// the begin changes the three controller keys and nothing else, and the
// next pass reads it back as the requested drain.
func TestDrainRequestWritesNewKeysOnly(t *testing.T) {
	p, it, store, _ := admittedBegin(t, undesired(drainSuspended))
	before, _ := store.Get(it.Key.ID)
	if s := runBegin(p, it); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want landed", s)
	}
	after, _ := store.Get(it.Key.ID)
	changed := maps.Clone(after.Metadata)
	maps.DeleteFunc(changed, func(k, v string) bool { return before.Metadata[k] == v })
	want := map[string]string{session.DrainIntentReasonKey: drainSuspended, session.DrainIntentAtKey: gatherNow.Format(time.RFC3339), session.DrainIntentIncarnationKey: "3"}
	if !maps.Equal(changed, want) {
		t.Fatalf("the begin changed %v, want only %v", changed, want)
	}
	row := readCensus(t, gatherNow, censusLegs(rowLeg, store)).Rows[it.Key]
	if req, ok := activeStop(row); !ok || req.Phase != stopRequested || req.Reason != drainSuspended {
		t.Fatalf("activeStop after the begin = %+v, %v; want the requested drain", req, ok)
	}
}

// failingReads is a work store whose every read fails.
type failingReads struct{ beads.Store }

var errReadFailed = errors.New("read failed")

func (failingReads) List(beads.ListQuery) ([]beads.Bead, error) { return nil, errReadFailed }
func (failingReads) ListByAssignee(string, string, int) ([]beads.Bead, error) {
	return nil, errReadFailed
}

// blockingReads is a work store whose reads wait for release.
type blockingReads struct {
	beads.Store
	release chan struct{}
}

func (b blockingReads) List(q beads.ListQuery) ([]beads.Bead, error) {
	<-b.release
	return b.Store.List(q)
}

func (b blockingReads) ListByAssignee(a, s string, n int) ([]beads.Bead, error) {
	<-b.release
	return b.Store.ListByAssignee(a, s, n)
}

// TestDrainBeginReReadsWorkLive (SESS-074, L5). Kills an undesired begin
// that trusts the pass's work view, or waits on it unbounded: work assigned
// after the pass, a work read still running when the effect's context ends,
// or one that fails, refuses with has-work and writes nothing.
func TestDrainBeginReReadsWorkLive(t *testing.T) {
	p, it, store, _ := admittedBegin(t, undesired(drainOrphaned))
	if _, err := store.Create(beads.Bead{Title: "w", Type: "task", Status: "open", Assignee: it.Key.ID}); err != nil {
		t.Fatal(err)
	}
	if s := runBegin(p, it); s.Outcome != settledRefused || s.Cause != causeHasWork {
		t.Fatalf("work assigned after the pass: %+v, want refused %q", s, causeHasWork)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	p, it, store, _ = admittedBegin(t, undesired(drainOrphaned))
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	p.reads.city = blockingReads{store, stuck}
	done := make(chan settlement, 1)
	go func() { done <- runTx(ctx, p, it, effectSpecs[it.Kind], nil) }()
	select {
	case s := <-done:
		if s.Outcome == settledLanded {
			t.Fatalf("work read past its context: %+v, want no landing", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the begin waited on a work read past its context")
	}
	if got, _ := store.Get(it.Key.ID); got.Metadata[session.DrainIntentReasonKey] != "" {
		t.Fatalf("a begin whose work read outlived its context wrote %v", got.Metadata)
	}
	p, it, store, _ = admittedBegin(t, undesired(drainSuspended))
	p.reads.city = failingReads{store}
	if s := runBegin(p, it); s.Outcome != settledRefused || s.Cause != causeHasWork {
		t.Fatalf("work read failed: %+v, want refused %q", s, causeHasWork)
	}
	if got, _ := store.Get(it.Key.ID); got.Metadata[session.DrainIntentReasonKey] != "" {
		t.Fatalf("a refused begin wrote %v", got.Metadata)
	}
}

// TestIdleBeginProvesIdleInTheEffect (SESS-621, effect side). Kills an
// interactive idle begin without the idle probe, or one that ignores
// activity since: a failed WaitForIdle or activity after the pass began
// refuses not-idle; a proven idle session lands; a non-interactive one is
// never probed.
func TestIdleBeginProvesIdleInTheEffect(t *testing.T) {
	idle := func(e *selectionEntry) { e.Desired, e.Reason = desireSleep, reasonIdleSleep }
	full := func(w *World) {
		for id := range w.SleepPolicies {
			w.SleepPolicies[id] = resolvedSessionSleepPolicy{Class: config.SessionSleepInteractiveResume, Capability: runtime.SessionSleepCapabilityFull}
		}
	}
	for _, c := range []struct {
		name     string
		waitErr  error
		activity time.Duration
		want     string
	}{
		{"not idle", errors.New("busy"), -time.Hour, causeNotIdle},
		{"active since the pass", nil, time.Second, causeNotIdle},
		{"proven idle", nil, -time.Hour, ""},
	} {
		p, it, _, leaf := admittedBegin(t, idle, full)
		leaf.WaitForIdleErrors["s-gc-1"] = c.waitErr
		leaf.Activity = map[string]time.Time{"s-gc-1": gatherNow.Add(c.activity)}
		s := runBegin(p, it)
		if s.Cause != c.want || (c.want == "") != (s.Outcome == settledLanded) {
			t.Errorf("%s: %+v, want cause %q", c.name, s, c.want)
		}
		// Not idle is a no-op, never a refusal that backs the row off.
		if c.want != "" && s.Outcome != settledNoop {
			t.Errorf("%s: outcome %v, want a no-op (no row backoff)", c.name, s.Outcome)
		}
	}
	p, it, _, leaf := admittedBegin(t, idle)
	if s := runBegin(p, it); s.Outcome != settledLanded {
		t.Fatalf("non-interactive: %+v, want landed", s)
	}
	for _, call := range leaf.Calls {
		if call.Method == "WaitForIdle" {
			t.Fatal("a non-interactive session was probed for idleness")
		}
	}
}

// TestDrainBeginTakesTheRuntimeNameLock (PLUMBING: a fresh effect reads and
// writes under the name lock). Kills a begin that probes a runtime another
// effect holds: a busy name refuses name-busy and writes nothing.
func TestDrainBeginTakesTheRuntimeNameLock(t *testing.T) {
	p, it, store, _ := admittedBegin(t, func(e *selectionEntry) { e.Desired = desireSleep })
	_, held, cause, err := lockRuntimeName(p.World, p.World.Census.Rows[it.Key].Info, nil, 0)
	if cause != "" {
		t.Fatalf("fixture: the name lock: %s %v", cause, err)
	}
	defer held.Release()
	if s := runBegin(p, it); s.Outcome != settledRefused || s.Cause != causeNameBusy {
		t.Fatalf("%+v, want refused %q", s, causeNameBusy)
	}
	if got, _ := store.Get(it.Key.ID); got.Metadata[session.DrainIntentReasonKey] != "" {
		t.Fatalf("a refused begin wrote %v", got.Metadata)
	}
}

// TestDrainBeginRecordsItsTransition. Kills a begin that lands without
// legacy's drain telemetry, or carries one it did not land: a landed begin's
// settlement carries (name, reason, "begin"), a refused one none.
func TestDrainBeginRecordsItsTransition(t *testing.T) {
	p, it, _, leaf := admittedBegin(t, func(e *selectionEntry) { e.Desired = desireSleep })
	leaf.SetAttached("s-gc-1", true)
	if s := runBegin(p, it); s.Outcome != settledRefused || s.Facts.Transition != nil {
		t.Fatalf("refused begin %+v carries transition %+v, want none", s, s.Facts.Transition)
	}
	p, it, _, _ = admittedBegin(t, func(e *selectionEntry) { e.Desired = desireSleep })
	want := drainTransition{Name: "s-gc-1", Reason: reasonNoWake, Transition: "begin"}
	if s := runBegin(p, it); s.Outcome != settledLanded || s.Facts.Transition == nil || *s.Facts.Transition != want {
		t.Fatalf("landed begin %+v carries transition %+v, want %+v", s, s.Facts.Transition, want)
	}
}

// TestDrainBeginPremiseIsTheDrainBasis (orchestrator ruling at the C6a2
// replay). Kills a begin behind the default premise, which an agent's
// heartbeat starves: a held_until written between the pass and the CAS lets
// the begin land; an operator's intent (sleep_intent) or a wake request
// written then refuses it, writing nothing.
func TestDrainBeginPremiseIsTheDrainBasis(t *testing.T) {
	for _, c := range []struct {
		name  string
		write map[string]string
		lands bool
	}{
		{"a heartbeat", map[string]string{"held_until": gatherNow.Add(time.Minute).Format(time.RFC3339)}, true},
		{"an operator's intent", map[string]string{"sleep_intent": "user-hold"}, false},
		{"a wake request", map[string]string{"wake_request": "explicit"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, it, store, _ := admittedBegin(t, undesired(drainOrphaned))
			if err := store.SetMetadataBatch(it.Key.ID, c.write); err != nil {
				t.Fatal(err)
			}
			s := runBegin(p, it)
			got, _ := store.Get(it.Key.ID)
			switch {
			case c.lands && (s.Outcome != settledLanded || got.Metadata[session.DrainIntentReasonKey] != drainOrphaned):
				t.Fatalf("%+v %v, want the begin landed", s, got.Metadata)
			case !c.lands && (s.Outcome != settledRefused || s.Cause != causePremise || got.Metadata[session.DrainIntentReasonKey] != ""):
				t.Fatalf("%+v %v, want refused %q and no request", s, got.Metadata, causePremise)
			}
		})
	}
}

// TestDrainBeginRedecidesOutsideTheBasis (C6a2 review, mutant M02). Kills a
// begin that writes its pass's patch without deciding again: a change the
// drain basis does not compare, a heartbeat hold landing on a Sleep row
// (SESS-617) or the row turning asleep, passes the premise and refuses
// redecided inside the CAS, writing nothing.
func TestDrainBeginRedecidesOutsideTheBasis(t *testing.T) {
	sleep := func(e *selectionEntry) { e.Desired = desireSleep }
	for _, c := range []struct {
		name  string
		write map[string]string
	}{
		{"a heartbeat hold on a Sleep row", map[string]string{"held_until": gatherNow.Add(time.Hour).Format(time.RFC3339)}},
		{"the row turned asleep", map[string]string{"state": "asleep"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, it, store, _ := admittedBegin(t, sleep)
			if it.Kind != intentDrainBeginFresh {
				t.Fatalf("fixture: %+v, want the fresh begin", it)
			}
			if err := store.SetMetadataBatch(it.Key.ID, c.write); err != nil {
				t.Fatal(err)
			}
			s := runBegin(p, it)
			got, _ := store.Get(it.Key.ID)
			if s.Outcome != settledRefused || s.Cause != causeRedecided || got.Metadata[session.DrainIntentReasonKey] != "" {
				t.Fatalf("%+v %v, want refused %q and no request", s, got.Metadata, causeRedecided)
			}
		})
	}
}
