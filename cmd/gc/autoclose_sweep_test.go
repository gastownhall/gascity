package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

var sweepEpoch = time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)

// sweepTestClock is the time of the i-th sweep pass.
func sweepTestClock(i int) time.Time {
	return sweepEpoch.Add(time.Duration(i) * autocloseSweepInterval)
}

// closeNotifications collects the bead.closed notifications a cache emits.
type closeNotifications struct {
	mu  sync.Mutex
	ids []string
}

func (r *closeNotifications) record(eventType, beadID string, _ json.RawMessage) {
	if eventType != events.BeadClosed {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, beadID)
}

// convoySweepFixture is a cached convoy whose only open member is returned.
func convoySweepFixture(t *testing.T) (cs *controllerState, backing *beads.MemStore, cached *beads.CachingStore, notes *closeNotifications, convoy, member beads.Bead) {
	t.Helper()
	backing = beads.NewMemStore()
	convoy, err := backing.Create(beads.Bead{Title: "batch", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	member, err = backing.Create(beads.Bead{Title: "task", ParentID: convoy.ID})
	if err != nil {
		t.Fatal(err)
	}
	notes = &closeNotifications{}
	cached = beads.NewCachingStoreForTest(backing, notes.record)
	if err := cached.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	cs = &controllerState{beadStores: map[string]beads.Store{"r": cached}, eventProv: events.NewFake()}
	return cs, backing, cached, notes, convoy, member
}

// TestAutocloseSweepRunsAutocloseForASilentClose is the missed-close probe
// (mc-zndi7.55): a Live list absorbs an out-of-process close and the scan
// evicts the closed row, so no bead.closed is ever emitted. The sweep sees the
// row leave the census and runs autoclose, after one pass of grace.
func TestAutocloseSweepRunsAutocloseForASilentClose(t *testing.T) {
	cs, backing, cached, notes, convoy, member := convoySweepFixture(t)

	cs.runAutocloseSweepPass(sweepTestClock(0)) // seeds the census

	if err := backing.Close(member.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cached.List(beads.ListQuery{Live: true, Status: "open"}); err != nil {
		t.Fatal(err)
	}
	cached.ReconcileNowForTest()
	cached.ReconcileNowForTest()
	if len(notes.ids) != 0 {
		t.Fatalf("precondition: the cache notified bead.closed %v; the probe needs a silent close", notes.ids)
	}

	if got := cs.runAutocloseSweepPass(sweepTestClock(1)); got.Ran != 0 {
		t.Fatalf("pass 1 ran autoclose %d time(s) inside the grace", got.Ran)
	}
	if got := statusOf(t, backing, convoy.ID); got != "open" {
		t.Fatalf("convoy %s during the grace, want open", got)
	}
	if got := cs.runAutocloseSweepPass(sweepTestClock(2)); got.Ran != 1 {
		t.Fatalf("pass 2 ran autoclose %d time(s), want 1", got.Ran)
	}
	if got := statusOf(t, backing, convoy.ID); got != "closed" {
		t.Fatalf("convoy %s after the sweep, want closed", got)
	}
	if got := cs.runAutocloseSweepPass(sweepTestClock(3)); got.Ran != 0 {
		t.Fatalf("pass 3 ran autoclose %d time(s) again", got.Ran)
	}
}

// TestAutocloseSweepSkipsClosesTheEventPathRan: a close that reached the bus
// already ran autoclose, so its departure costs no read.
func TestAutocloseSweepSkipsClosesTheEventPathRan(t *testing.T) {
	prev := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prev })
	cached, backing := primedCache(t, beads.Bead{ID: "gc-1", Title: "task"})
	cs := &controllerState{beadStores: map[string]beads.Store{"r": cached}, eventProv: events.NewFake()}

	cs.runAutocloseSweepPass(sweepTestClock(0))
	payload := closedSnapshotInBacking(t, backing, "gc-1")
	cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Actor: "bd-close", Subject: "gc-1", Payload: payload})
	if !cs.autocloseSweepOf().hasRan("gc-1") {
		t.Fatal("precondition: the event path did not run autoclose")
	}

	for i := 1; i <= 3; i++ {
		if got := cs.runAutocloseSweepPass(sweepTestClock(i)); got != (autocloseSweepResult{}) {
			t.Fatalf("pass %d = %+v, want nothing to do", i, got)
		}
	}
	if cs.autocloseSweepOf().hasRan("gc-1") || cs.autocloseSweepOf().isPending("gc-1") {
		t.Fatal("the swept departure left state behind")
	}
}

// TestAutocloseSweepRefutesAFalseDeparture: a row that left the census while
// open in the store (a false scan eviction) gets no completion fact and no
// autoclose.
func TestAutocloseSweepRefutesAFalseDeparture(t *testing.T) {
	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	step, convoy, _ := graphStepFixture(t, mem)
	rec := events.NewFake()
	cs := &controllerState{cityBeadStore: mem, eventProv: rec}

	cs.autocloseSweepOf().deferID(step.ID, sweepTestClock(0))
	if got := cs.runAutocloseSweepPass(sweepTestClock(0)); got.Ran != 0 || got.Refuted != 1 {
		t.Fatalf("pass = %+v, want one refuted check and no autoclose", got)
	}
	if got := completedFor(rec, step.ID); got != 0 {
		t.Fatalf("completion facts = %d for an open step", got)
	}
	if got := statusOf(t, mem, convoy.ID); got != "open" {
		t.Fatalf("convoy %s, want open", got)
	}
}

// downGetStore fails Get while down is set.
type downGetStore struct {
	beads.Store
	down atomic.Bool
}

func (s *downGetStore) Get(id string) (beads.Bead, error) {
	if s.down.Load() {
		return beads.Bead{}, errors.New("backing down")
	}
	return s.Store.Get(id)
}

// TestAutocloseSweepRetriesAnUnconfirmedClose: a close deferred because the
// row could not be read is retried until a read answers.
func TestAutocloseSweepRetriesAnUnconfirmedClose(t *testing.T) {
	backing := beads.NewMemStore()
	convoy, err := backing.Create(beads.Bead{Title: "batch", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := backing.Create(beads.Bead{Title: "task", ParentID: convoy.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := backing.Close(member.ID); err != nil {
		t.Fatal(err)
	}
	store := &downGetStore{Store: backing}
	store.down.Store(true)
	cs := &controllerState{cityBeadStore: store, eventProv: events.NewFake()}
	cs.autocloseSweepOf().deferID(member.ID, sweepTestClock(0))

	if got := cs.runAutocloseSweepPass(sweepTestClock(0)); got.Retried != 1 {
		t.Fatalf("pass while down = %+v, want one retry", got)
	}
	store.down.Store(false)
	if got := cs.runAutocloseSweepPass(sweepTestClock(0)); got.Ran != 0 {
		t.Fatalf("the retry ran before it was due: %+v", got)
	}
	if got := cs.runAutocloseSweepPass(sweepTestClock(1)); got.Ran != 1 {
		t.Fatalf("pass after recovery = %+v, want one autoclose", got)
	}
	if got := statusOf(t, backing, convoy.ID); got != "closed" {
		t.Fatalf("convoy %s, want closed", got)
	}
}

func TestAutocloseSweepIsBounded(t *testing.T) {
	s := newAutocloseSweep()
	s.batch, s.pendingCap, s.ranCap = 2, 3, 2
	for i := range 5 {
		s.deferID(fmt.Sprintf("gc-%d", i), sweepTestClock(0))
		s.noteRan(fmt.Sprintf("ran-%d", i))
	}
	if len(s.pending) != 3 || s.dropped != 2 {
		t.Fatalf("pending=%d dropped=%d, want 3 and 2", len(s.pending), s.dropped)
	}
	if len(s.ran) > 2 {
		t.Fatalf("ran holds %d ids, cap 2", len(s.ran))
	}
	if got := s.due(sweepTestClock(0)); len(got) != 2 {
		t.Fatalf("due = %v, want a batch of 2", got)
	}
	if got := s.due(sweepTestClock(0)); len(got) != 1 {
		t.Fatalf("due = %v, want the remaining 1", got)
	}
}

// TestAutocloseSweepCensus pins the diff: the first census seeds, a departure
// is due after the grace, and an arrival forgets the id, so a reopened row's
// next close is checked even though autoclose ran for its last one.
func TestAutocloseSweepCensus(t *testing.T) {
	s := newAutocloseSweep()
	cache := &beads.CachingStore{}
	set := func(ids ...string) map[string]struct{} {
		m := map[string]struct{}{}
		for _, id := range ids {
			m[id] = struct{}{}
		}
		return m
	}
	s.observe(cache, set("a", "b"), sweepTestClock(0))
	if len(s.pending) != 0 {
		t.Fatalf("the seeding census queued %v", s.pending)
	}
	s.noteRan("a")
	s.observe(cache, set("b"), sweepTestClock(1))
	if got := s.due(sweepTestClock(1)); len(got) != 0 {
		t.Fatalf("due inside the grace: %v", got)
	}
	s.observe(cache, set("a", "b"), sweepTestClock(1)) // a reopened
	if s.hasRan("a") || s.isPending("a") {
		t.Fatal("an arrival kept the id's ran or pending state")
	}
	s.observe(cache, set("b"), sweepTestClock(2)) // a closed again, silently
	if got := s.due(sweepTestClock(3)); len(got) != 1 || got[0] != "a" {
		t.Fatalf("due = %v, want [a]", got)
	}
	s.retain(map[*beads.CachingStore]struct{}{})
	if len(s.census) != 0 {
		t.Fatal("retain kept a replaced cache's census")
	}
}
