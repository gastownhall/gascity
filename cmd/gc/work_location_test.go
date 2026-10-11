package main

import (
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// A sessions store cannot be a work leg (mc-3ixn3.16, NEW2-1) by any
// assignment or conversion: the mint takes the work-class handle, which a
// beads.SessionStore is not assignable to, and no store type converts to the
// work leg itself. What remains, a WorkStore built from a sessions store or a
// field set on a WorkLegs, the worklegs analyzer (tools/nogo) refuses.
func TestWorkLegsSessionStoreIsNotAWorkLeg(t *testing.T) {
	src := reflect.TypeOf(cityWorkLeg{})
	for _, from := range []reflect.Type{
		reflect.TypeOf(beads.SessionStore{}),
		reflect.TypeOf(beads.WorkStore{}),
		reflect.TypeOf((*beads.Store)(nil)).Elem(),
		reflect.TypeOf(beads.NewMemStore()),
	} {
		if from.AssignableTo(src) || from.ConvertibleTo(src) {
			t.Errorf("%v converts to cityWorkLeg", from)
		}
	}
	if reflect.TypeOf(beads.SessionStore{}).ConvertibleTo(reflect.TypeOf(WorkLegs{})) {
		t.Error("a sessions store converts to WorkLegs")
	}
}

// The zero WorkLegs, one minted without a work store, and a nil SeatWork
// answer unknown: no gate may read "no work" from a leg set nothing minted.
func TestWorkLegsUnmintedFailsClosed(t *testing.T) {
	info := session.Info{ID: "gc-1"}
	for name, legs := range map[string]WorkLegs{
		"zero":          {},
		"no work store": workLegsFromCensus(t.TempDir(), nil, cityWorkLeg{}, nil),
	} {
		has, err := sessionHasOpenAssignedWorkForReachableStore(newSeatWork(legs), info)
		if !errors.Is(err, errNoWorkStore) {
			t.Errorf("%s: gate = %v, %v; want unknown (errNoWorkStore)", name, has, err)
		}
	}
	if has, err := sessionHasOpenAssignedWorkForReachableStore(nil, info); !errors.Is(err, errNoWorkStore) {
		t.Errorf("nil SeatWork: gate = %v, %v; want unknown (errNoWorkStore)", has, err)
	}
}

// NEW2-1: v2's live work read plans over the city work store gather mints
// from gatherEnv.WorkStore, never the sessions store the census leads with.
// A claim on the work store holds the row; with no work store the read is
// unknown.
func TestGatherWorkLegsReadTheCityWorkStore(t *testing.T) {
	f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "active"))
	work := beads.NewMemStore()
	if _, err := work.Create(beads.Bead{Title: "claim", Type: "task", Status: "open", Assignee: "gc-1"}); err != nil {
		t.Fatal(err)
	}
	f.env.WorkStore = func() beads.Store { return work }
	w := f.gather(t)
	if !w.WorkLegs.isWork(work) || w.WorkLegs.isWork(f.cache) {
		t.Fatal("gather's WorkLegs do not take the city work store as the work leg")
	}
	row := w.Census.Canonical()[0].Info
	if got := newEffectPass(&w, &allocDecision{}).readWork(row); got.Free || got.Err != nil {
		t.Fatalf("L5 = %+v; want the work store's claim to hold the row", got)
	}

	f.env.WorkStore = nil
	w = f.gather(t)
	if got := newEffectPass(&w, &allocDecision{}).readWork(row); got.Free || !errors.Is(got.Err, errNoReadStore) {
		t.Fatalf("L5 without a work store = %+v; want unknown", got)
	}
}

// A seat's read lists only its scope's identities, so a question about any
// other identity is unknown, not "no work".
func TestSeatWorkSeatReadRefusesAnUnreadIdentity(t *testing.T) {
	work := beads.NewMemStore()
	if _, err := work.Create(beads.Bead{Title: "claim", Type: "task", Status: "open", Assignee: "other"}); err != nil {
		t.Fatal(err)
	}
	legs := workLegsFromCensus(t.TempDir(), nil, cityWorkLeg{store: work}, nil)
	sw := seatWorkFor(legs, ReleaseScope{ids: []string{"gc-1"}})
	if has, err := sw.has(seatWorkQuery{scope: ReleaseScope{ids: []string{"gc-1"}}, statuses: seatWorkStatuses}); has || err != nil {
		t.Fatalf("read identity = %v, %v; want no work", has, err)
	}
	if has, err := sw.has(seatWorkQuery{scope: RefuseScope{ids: []string{"gc-1", "other"}}, statuses: seatWorkStatuses}); has || err == nil {
		t.Fatalf("unread identity = %v, %v; want unknown", has, err)
	}
}

// One read serves concurrent questions: the starts' claimed-work probes ask
// from parallel goroutines.
func TestSeatWorkConcurrentQuestionsShareOneRead(t *testing.T) {
	work := &workCallCounter{Store: beads.NewMemStore()}
	legs := workLegsFromCensus(t.TempDir(), nil, cityWorkLeg{store: work}, nil)
	sw := newSeatWork(legs)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := sw.has(seatWorkQuery{scope: ReleaseScope{ids: []string{"gc-1"}}, statuses: seatWorkStatuses}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := work.n(); got != len(seatWorkStatuses) {
		t.Fatalf("8 questions listed the work store %d times, want %d (one read)", got, len(seatWorkStatuses))
	}
}

// A v2 effect's sections hold the city work legs read-only: every leg, the
// rigs and bindings included, refuses a write (capReadStores).
func TestEffectReadsLegsAreReadOnly(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "a", Path: filepath.Join(cityPath, "a")}}}
	w := &World{Env: &reconcileEnv{}, WorkLegs: workLegsFromCensus(cityPath, cfg, cityWorkLeg{store: beads.NewMemStore()}, map[string]beads.Store{"a": beads.NewMemStore()})}
	legs := newEffectPass(w, &allocDecision{}).reads.legs
	n := 0
	legs.each(func(s beads.Store) {
		n++
		if _, err := s.Create(beads.Bead{Title: "write"}); !errors.Is(err, errBlindWriteRefused) {
			t.Errorf("leg %d accepted a write: %v", n, err)
		}
	})
	if n != 2 {
		t.Fatalf("effect reads hold %d legs, want the work store and the rig", n)
	}
}
