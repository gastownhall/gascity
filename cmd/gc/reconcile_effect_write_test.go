package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/session"
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
			decide := func(w *World, a *allocDecision, k rowKey) (intent, time.Time) {
				fresh, next := decideRow(w, a, k)
				fresh.Event = &ev
				return fresh, next
			}
			s := rowWrite{pass: p, it: it, decide: decide}.run(context.Background())
			if s.Outcome != settledLanded || len(s.Events) != 1 || s.Events[0].Type != ev.Type {
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
			s := rowWriteEffect(p, it)(context.Background())
			if s.Outcome != settledRefused || s.Cause != causeNoWriter {
				t.Fatalf("settlement %+v, want refused with cause %q", s, causeNoWriter)
			}
			if heldUntil(t, m, it.Key.ID) == "" {
				t.Fatal("the row was written without a conditional writer")
			}
		})
	}
}

// Kills last-writer-wins: an operator's write that lands between the row
// write's read and its CAS survives, and the row write refuses with cause
// cas, writing nothing.
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
			s := rowWriteEffect(p, it)(context.Background())
			if s.Outcome != settledRefused || s.Cause != causeCAS {
				t.Fatalf("settlement %+v, want refused with cause %q", s, causeCAS)
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
// redecided and writes nothing; so does a fresh decision of the admitted
// kind at another incarnation (legacy's authorized). An effect abandoned at
// its deadline writes nothing either.
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
	if s := rowWriteEffect(p, it)(context.Background()); s.Outcome != settledRefused || s.Cause != causeRedecided {
		t.Fatalf("kill-fenced row: settlement %+v, want refused with cause %q", s, causeRedecided)
	}

	store, _ = stampedMem(t, gate.Require)
	p, it = admittedHeal(t, store, store)
	other := func(w *World, a *allocDecision, k rowKey) (intent, time.Time) {
		fresh, next := decideRow(w, a, k)
		fresh.Kind = intentRowMetadata // what a newer allocation decides for the row
		return fresh, next
	}
	if s := (rowWrite{pass: p, it: it, decide: other}).run(context.Background()); s.Outcome != settledRefused || s.Cause != causeRedecided {
		t.Fatalf("another kind: settlement %+v, want refused with cause %q", s, causeRedecided)
	}
	stale := it
	stale.Basis.Incarnation++ // the pass saw another incarnation than the fresh row holds
	if s := rowWriteEffect(p, stale)(context.Background()); s.Outcome != settledRefused || s.Cause != causeRedecided {
		t.Fatalf("another basis: settlement %+v, want refused with cause %q", s, causeRedecided)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s := rowWriteEffect(p, it)(ctx); s.Outcome != settledFailed {
		t.Fatalf("abandoned: settlement %+v, want failed", s)
	}
	if heldUntil(t, store, it.Key.ID) == "" {
		t.Fatal("a refused row write wrote")
	}
}

// Kills a row write that goes to one store whatever the row's leg (session
// rows live in work stores too, #5187), and one that reaches a store the
// pass holds no writer for: a row on a work-store leg is written through
// that leg's writer, leaving the sessions store alone, and a leg without a
// writer is refused.
func TestRowWriteUsesItsLegsWriter(t *testing.T) {
	sessions, _ := stampedMem(t, gate.Require)
	work, _ := stampedMem(t, gate.Require)
	b, err := work.Create(sessionRow("heal", "template", "worker", "session_name", "s-heal", "state", "asleep", "generation", "3", "held_until", rowAt(-time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	const workLeg = "rig:work"
	w := &World{Now: gatherNow, Census: readCensus(t, gatherNow, censusLegs(rowLeg, sessions, workLeg, work)), Mislabelled: map[rowKey]bool{}}
	w.LegStores = map[string]beads.Store{rowLeg: sessions, workLeg: work}
	a := &allocDecision{Snapshot: &selectionSnapshot{Entries: map[rowKey]*selectionEntry{}}}
	it, _ := decideRow(w, a, rowKey{Leg: workLeg, ID: b.ID})
	if s := rowWriteEffect(newEffectPass(w, a), it)(context.Background()); s.Outcome != settledLanded {
		t.Fatalf("settlement %+v, want the heal landed on the work store", s)
	}
	if got := heldUntil(t, work, b.ID); got != "" {
		t.Fatalf("work store held_until %q, want it cleared", got)
	}
	delete(w.LegStores, workLeg)
	if s := rowWriteEffect(newEffectPass(w, a), it)(context.Background()); s.Outcome != settledRefused || s.Cause != causeNoWriter {
		t.Fatalf("no writer for the leg: settlement %+v, want refused with cause %q", s, causeNoWriter)
	}
}

// Kills raw handles reaching an effect (v5 R3): the pass's World copy holds
// no leg, city, rig or assigned-work store or provider, the city and rig
// stores reach the effect only read-only, and the pass's own World keeps
// them all.
func TestEffectPassStripsRawHandles(t *testing.T) {
	store := beads.NewMemStore()
	w := &World{
		Env:           &reconcileEnv{Gen: 1, SP: &sleepCountingProvider{}},
		Demand:        demandView{AssignedStores: []beads.Store{store}},
		LegStores:     map[string]beads.Store{rowLeg: store},
		SessionsStore: store, RigStores: map[string]beads.Store{"rig": store},
	}
	p := newEffectPass(w, &allocDecision{})
	if p.World.LegStores != nil || p.World.Demand.AssignedStores != nil || p.World.Env.SP != nil ||
		p.World.SessionsStore != nil || p.World.RigStores != nil {
		t.Fatalf("the effects' World holds raw handles: %+v", p.World)
	}
	for name, read := range map[string]beads.Store{"city": p.Reads.City, "rig": p.Reads.Rigs["rig"]} {
		if _, err := read.Create(beads.Bead{Title: "x"}); !errors.Is(err, errBlindWriteRefused) {
			t.Fatalf("the %s read store wrote: %v", name, err)
		}
	}
	if p.Runtime != w.Env.SP || p.Releasers.Legs[rowLeg] != store || len(p.Releasers.Assigned) != 1 {
		t.Fatalf("want the composite provider and the release-capable stores: %+v", p)
	}
	if _, ok := p.Writers[rowLeg]; !ok || w.Env.SP == nil || w.LegStores == nil || w.Demand.AssignedStores == nil {
		t.Fatal("want a writer for the leg and the pass's World untouched")
	}
	if p.Runtime != w.Env.SP {
		t.Fatal("the pass's Runtime is not the composite provider")
	}
}

// Kills a row write that commits a decision its deadline overtook: a
// context canceled while decideRow runs on the fresh row, after the check
// before the lock, writes nothing and settles failed.
func TestRowWriteCanceledDuringRedecideWritesNothing(t *testing.T) {
	store, _ := stampedMem(t, gate.Require)
	p, it := admittedHeal(t, store, store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	canceling := func(w *World, a *allocDecision, k rowKey) (intent, time.Time) {
		cancel() // the deadline lands mid re-decide
		return decideRow(w, a, k)
	}
	if s := (rowWrite{pass: p, it: it, decide: canceling}).run(ctx); s.Outcome != settledFailed || s.Cause != causeDeadline {
		t.Fatalf("settlement %+v, want failed with cause %q", s, causeDeadline)
	}
	if heldUntil(t, store, it.Key.ID) == "" {
		t.Fatal("the row write landed after its context ended")
	}
}

// Kills a runLocked that takes the row's session mutation lock (an effect
// that holds it would deadlock, the lock not being reentrant), and a run
// that does not take it.
func TestRowWriteRunLockedTakesNoMutationLock(t *testing.T) {
	store, _ := stampedMem(t, gate.Require)
	p, it := admittedHeal(t, store, store)
	done := make(chan settlement, 1)
	_ = session.WithSessionMutationLock(it.Key.ID, func() error {
		go func() { done <- rowWriteEffect(p, it)(context.Background()) }()
		select {
		case s := <-done:
			t.Errorf("run settled %+v while another section held the row's lock", s)
		case <-time.After(50 * time.Millisecond):
		}
		go func() { done <- (rowWrite{pass: p, it: it, decide: decideRow}).runLocked(context.Background()) }()
		select {
		case s := <-done:
			if s.Outcome != settledLanded {
				t.Errorf("runLocked settled %+v, want landed", s)
			}
		case <-time.After(10 * time.Second):
			t.Error("runLocked waited on the row's session mutation lock")
		}
		return nil
	})
	select {
	case s := <-done:
		if s.Outcome != settledRefused || s.Cause != causeRedecided {
			t.Fatalf("run after runLocked landed: settlement %+v, want refused %q", s, causeRedecided)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run never settled")
	}
}

// Kills a row write that writes once its context has ended before the
// write began.
func TestRowWriteCanceledBeforeTheWriteWritesNothing(t *testing.T) {
	store, _ := stampedMem(t, gate.Require)
	p, it := admittedHeal(t, store, store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s := rowWriteEffect(p, it)(ctx); s.Outcome != settledFailed || s.Cause != causeDeadline {
		t.Fatalf("settlement %+v, want failed with cause %q", s, causeDeadline)
	}
	if heldUntil(t, store, it.Key.ID) == "" {
		t.Fatal("the row write landed after its context ended")
	}
}
