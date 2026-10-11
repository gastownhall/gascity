package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// The row-write effect's tests (CONTRACT v5 R2, C0.7, D-9).

// rowWriteBackends are the fenced stores the row write must hold on: the
// real SQLite revision-layout store and MemStore, both stamped require.
var rowWriteBackends = []struct {
	name string
	open func(t *testing.T) beads.Store
}{
	{"sqlite", func(t *testing.T) beads.Store { return stampedSQLite(t, gate.Require) }},
	{"memstore", func(t *testing.T) beads.Store { m, _ := stampedMem(t, gate.Require); return m }},
}

// admittedHeal seeds a row whose hold expired on census, and returns the
// pass that admitted its timer heal, writing through writer, and the heal.
func admittedHeal(t *testing.T, census, writer beads.Store) (*effectPass, intent) {
	t.Helper()
	b, err := census.Create(sessionRow("heal", "template", "worker", "session_name", "s-heal", "state", "asleep", "generation", "3", "held_until", rowAt(-time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	w := &World{Now: gatherNow, Census: readCensus(t, gatherNow, censusLegs(rowLeg, census)), Mislabelled: map[rowKey]bool{}}
	a := &allocDecision{Snapshot: &selectionSnapshot{Entries: map[rowKey]*selectionEntry{}}}
	it, _ := decideRow(w, a, rowKey{Leg: rowLeg, ID: b.ID})
	if it.Kind != intentRowHeal || len(it.Patch) == 0 {
		t.Fatalf("the fixture admits %+v, want a timer heal with its patch", it)
	}
	w.LegStores = map[string]beads.Store{rowLeg: writer}
	return newEffectPass(w, a), it
}

// runRowWrite runs the row heal's transaction for it.
func runRowWrite(ctx context.Context, p *effectPass, it intent) settlement {
	return runTx(ctx, p, it, effectSpecs[intentRowHeal], nil)
}

// withArms replaces rowArms for the test, which must not run in parallel.
func withArms(t *testing.T, edit func([]rowArm) []rowArm) {
	t.Helper()
	saved := rowArms
	rowArms = edit(slices.Clone(saved))
	t.Cleanup(func() { rowArms = saved })
}

func heldUntil(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return b.Metadata["held_until"]
}

// Kills a row write that does not land the re-decided patch, and a landed
// write whose event is dropped.
func TestRowWriteLandsTheRedecidedPatchAndEvent(t *testing.T) {
	for _, backend := range rowWriteBackends {
		t.Run(backend.name, func(t *testing.T) {
			store := backend.open(t)
			p, it := admittedHeal(t, store, store)
			ev := events.Event{Type: "session.test", Subject: it.Key.ID}
			withArms(t, mutateHeal(func(r *rowFacts) (intent, bool) {
				fresh, ok := armTimerHeals(r)
				fresh.Event = &ev
				return fresh, ok
			}))
			s := runRowWrite(context.Background(), p, it)
			if s.Outcome != settledLanded || len(s.Facts.Events) != 1 || s.Facts.Events[0].Type != ev.Type {
				t.Fatalf("settlement %+v, want landed with the re-decided intent's event", s)
			}
			if got := heldUntil(t, store, it.Key.ID); got != "" {
				t.Fatalf("held_until %q after the heal, want it cleared", got)
			}
		})
	}
}

// Kills a blind fallback (C0.7): with no conditional writer (none
// configured, auto degraded, or require on a store that cannot fence) the
// row write is refused and writes nothing.
func TestRowWriteRefusesWithoutConditionalWriter(t *testing.T) {
	for _, c := range []struct {
		name string
		mode gate.Mode
	}{{"unstamped", gate.ModeUnset}, {"auto-degraded", gate.Auto}, {"require-incapable", gate.Require}} {
		t.Run(c.name, func(t *testing.T) {
			m := beads.NewMemStore()
			if c.mode != gate.ModeUnset {
				if err := beads.StampOpenedStore(m, "MemStore", c.mode, nil, nil); err != nil {
					t.Fatal(err)
				}
				m.DisableConditionalWrites = true
			}
			p, it := admittedHeal(t, m, m)
			s := runRowWrite(context.Background(), p, it)
			if s.Outcome != settledRefused || s.Cause != causeNoWriter {
				t.Fatalf("settlement %+v, want refused with cause %q", s, causeNoWriter)
			}
			if heldUntil(t, m, it.Key.ID) == "" {
				t.Fatal("the row was written without a conditional writer")
			}
		})
	}
}

// Kills last-writer-wins and a CAS decided once: an operator's write that
// lands between the row write's read and its CAS survives, and the row write
// reads the row again and decides again on it, refusing with cause
// redecided, writing nothing.
func TestRowWriteExternalWriteBetweenReadAndWrite(t *testing.T) {
	for _, backend := range rowWriteBackends {
		t.Run(backend.name, func(t *testing.T) {
			store := backend.open(t)
			adversary := &interleavedStore{Store: store}
			p, it := admittedHeal(t, store, adversary)
			rehold := rowAt(time.Hour)
			adversary.id = it.Key.ID
			adversary.between = func() {
				if err := store.SetMetadataBatch(it.Key.ID, map[string]string{"held_until": rehold}); err != nil {
					t.Errorf("external write: %v", err)
				}
			}
			s := runRowWrite(context.Background(), p, it)
			if s.Outcome != settledRefused || s.Cause != causePremise {
				t.Fatalf("settlement %+v, want refused with cause %q", s, causePremise)
			}
			if got := heldUntil(t, store, it.Key.ID); got != rehold {
				t.Fatalf("held_until %q, want the external write's %q", got, rehold)
			}
		})
	}
}

// Kills writing an action decided against a stale read or a stale
// allocation (D-9): when the fresh row decides nothing (a `gc session kill`
// fenced it after the pass) or another kind, the write refuses with cause
// redecided and writes nothing; a fresh row at another incarnation than the
// pass saw (legacy's authorized) refuses with cause premise. An effect whose
// context ended writes nothing either.
func TestRowWriteRedecidesAndRefusesDifferentIntent(t *testing.T) {
	store, _ := stampedMem(t, gate.Require)
	p, it := admittedHeal(t, store, store)
	fence := make(map[string]string)
	for i := 0; i+1 < len(killPending); i += 2 {
		fence[killPending[i]] = killPending[i+1]
	}
	if err := store.SetMetadataBatch(it.Key.ID, fence); err != nil {
		t.Fatal(err)
	}
	if s := runRowWrite(context.Background(), p, it); s.Outcome != settledRefused || s.Cause != causePremise {
		t.Fatalf("kill-fenced row: settlement %+v, want refused with cause %q", s, causePremise)
	}

	store, _ = stampedMem(t, gate.Require)
	p, it = admittedHeal(t, store, store)
	stale := it
	stale.Basis.Incarnation++ // the pass saw another incarnation than the fresh row holds
	if s := runRowWrite(context.Background(), p, stale); s.Outcome != settledRefused || s.Cause != causePremise {
		t.Fatalf("another basis: settlement %+v, want refused with cause %q", s, causePremise)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s := runRowWrite(ctx, p, it); s.Outcome != settledFailed {
		t.Fatalf("abandoned: settlement %+v, want failed", s)
	}
	withArms(t, mutateHeal(func(r *rowFacts) (intent, bool) {
		fresh, ok := armTimerHeals(r)
		fresh.Kind = intentRowMetadata // what a newer allocation decides for the row
		return fresh, ok
	}))
	if s := runRowWrite(context.Background(), p, it); s.Outcome != settledRefused || s.Cause != causeRedecided {
		t.Fatalf("another kind: settlement %+v, want refused with cause %q", s, causeRedecided)
	}
	if heldUntil(t, store, it.Key.ID) == "" {
		t.Fatal("a refused row write wrote")
	}
}

// Kills a row write that reaches a store other than its row's writer, and
// one the pass holds no writer for: a sessions-leg row lands through the
// sessions leg's writer, the one gather builds
// (TestGatherBuildsOnlyTheSessionsLegsWriter), and without it is refused.
func TestRowWriteUsesItsLegsWriter(t *testing.T) {
	sessions, _ := stampedMem(t, gate.Require)
	b, err := sessions.Create(sessionRow("heal", "template", "worker", "session_name", "s-heal", "state", "asleep", "generation", "3", "held_until", rowAt(-time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	w := &World{Now: gatherNow, Census: readCensus(t, gatherNow, censusLegs(rowLeg, sessions)), Mislabelled: map[rowKey]bool{}}
	w.LegStores = map[string]beads.Store{rowLeg: sessions}
	a := &allocDecision{Snapshot: &selectionSnapshot{Entries: map[rowKey]*selectionEntry{}}}
	it, _ := decideRow(w, a, rowKey{Leg: rowLeg, ID: b.ID})
	if s := runRowWrite(context.Background(), newEffectPass(w, a), it); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want the heal landed on the sessions store", s)
	}
	if got := heldUntil(t, sessions, b.ID); got != "" {
		t.Fatalf("sessions store held_until %q, want it cleared", got)
	}
	w.LegStores = nil
	if s := runRowWrite(context.Background(), newEffectPass(w, a), it); s.Outcome != settledRefused || s.Cause != causeNoWriter {
		t.Fatalf("no writer for the leg: settlement %+v, want refused with cause %q", s, causeNoWriter)
	}
}

// Kills raw handles reaching an effect (v5 R3): the pass's World copy holds
// no leg store, census store, assigned-work store or provider, and the
// pass's own World keeps them.
func TestEffectPassStripsRawHandles(t *testing.T) {
	store := beads.NewMemStore()
	w := &World{
		Env:           &reconcileEnv{Gen: 1, SP: &sleepCountingProvider{}},
		Demand:        demandView{AssignedStores: []beads.Store{store}},
		LegStores:     map[string]beads.Store{rowLeg: store},
		SessionsStore: store, RigStores: map[string]beads.Store{"rig": store},
		WorkLegs: workLegsFromCensus("", nil, cityWorkLeg{store: store}, nil),
	}
	p := newEffectPass(w, &allocDecision{})
	if p.World.LegStores != nil || p.World.Demand.AssignedStores != nil || p.World.Env.SP != nil {
		t.Fatalf("the effects' World holds raw handles: %+v", p.World)
	}
	if _, ok := p.Writers[rowLeg]; !ok || w.Env.SP == nil || w.LegStores == nil || w.Demand.AssignedStores == nil {
		t.Fatal("want a writer for the leg and the pass's World untouched")
	}
	if p.Runtime != w.Env.SP {
		t.Fatal("the pass's Runtime is not the composite provider")
	}
	if p.World.SessionsStore != nil || p.World.RigStores != nil {
		t.Fatal("the effects' World holds the census stores")
	}
	if p.World.WorkLegs.unusable() == nil {
		t.Fatal("the effects' World holds the work legs; effects reach them only as effectReads")
	}
}

// Kills a row write that commits a decision its deadline overtook, and a
// deadline read as a cancel: a context ended while decideRow runs on the
// fresh row writes nothing and settles failed, with cause deadline when it
// ended at its deadline (context.Cause, as under the fake clock) and
// canceled otherwise.
func TestRowWriteCanceledDuringRedecideWritesNothing(t *testing.T) {
	for _, c := range []struct {
		err   error
		cause string
	}{{context.DeadlineExceeded, causeDeadline}, {context.Canceled, causeShutdown}} {
		store, _ := stampedMem(t, gate.Require)
		p, it := admittedHeal(t, store, store)
		ctx, cancel := context.WithCancelCause(context.Background())
		withArms(t, mutateHeal(func(r *rowFacts) (intent, bool) {
			cancel(c.err) // the context ends mid re-decide
			return armTimerHeals(r)
		}))
		if s := runRowWrite(ctx, p, it); s.Outcome != settledFailed || s.Cause != c.cause {
			t.Fatalf("settlement %+v, want failed with cause %q", s, c.cause)
		}
		if heldUntil(t, store, it.Key.ID) == "" {
			t.Fatal("the row write landed after its context ended")
		}
	}
}

// Kills a row write that writes once its context has ended before the
// write began.
func TestRowWriteCanceledBeforeTheWriteWritesNothing(t *testing.T) {
	store, _ := stampedMem(t, gate.Require)
	p, it := admittedHeal(t, store, store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s := runRowWrite(ctx, p, it); s.Outcome != settledFailed || s.Cause != causeShutdown {
		t.Fatalf("settlement %+v, want failed with cause %q", s, causeShutdown)
	}
	if heldUntil(t, store, it.Key.ID) == "" {
		t.Fatal("the row write landed after its context ended")
	}
}
