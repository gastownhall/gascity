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
	"github.com/gastownhall/gascity/internal/rollout/gate"
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
// (mc-zndi7.55): a Live list absorbs an out-of-process close, the cache
// announces it once (gastownhall/gascity#6860) and the scan evicts the closed
// row, but the bead.closed never reaches the event path: the fixture records
// it without delivering it, as when the event log drops it
// (CACHE-LAYERING-REVIEW F2). The sweep sees the row leave the census and runs
// autoclose, after one pass of grace.
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
	if len(notes.ids) != 1 || notes.ids[0] != member.ID {
		t.Fatalf("precondition: the cache notified bead.closed %v, want [%s]; the probe needs one announced close the event path never receives", notes.ids, member.ID)
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
// autoclose, and is not checked again.
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
	if cs.autocloseSweepOf().isPending(step.ID) {
		t.Fatal("a refuted id was queued for another check")
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

// syncAutoclose runs close-triggered autoclose inline for the test.
func syncAutoclose(t *testing.T) {
	t.Helper()
	prev := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prev })
}

// TestAutocloseSweepClosesMoleculeForALostStepClose: a step close whose
// notification was lost is swept for a wisp molecule too, whose rows are all
// ephemeral (the review's tier probe).
func TestAutocloseSweepClosesMoleculeForALostStepClose(t *testing.T) {
	for _, ephemeral := range []bool{false, true} {
		t.Run(fmt.Sprintf("ephemeral=%v", ephemeral), func(t *testing.T) {
			syncAutoclose(t)
			backing := beads.NewMemStore()
			root, err := backing.Create(beads.Bead{Title: "mol", Type: "molecule", Ephemeral: ephemeral})
			if err != nil {
				t.Fatal(err)
			}
			step, err := backing.Create(beads.Bead{
				Title: "step", Type: "step", ParentID: root.ID, Ephemeral: ephemeral,
				Metadata: map[string]string{"gc.root_bead_id": root.ID},
			})
			if err != nil {
				t.Fatal(err)
			}
			// nil onChange: the local close's notification is lost (F2).
			cached := beads.NewCachingStoreForTest(backing, nil)
			if err := cached.Prime(context.Background()); err != nil {
				t.Fatal(err)
			}
			cs := &controllerState{beadStores: map[string]beads.Store{"r": cached}, eventProv: events.NewFake()}
			cs.runAutocloseSweepPass(sweepTestClock(0))
			if err := cached.Close(step.ID); err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 2; i++ {
				cs.runAutocloseSweepPass(sweepTestClock(i))
			}
			if got := statusOf(t, backing, root.ID); got != "closed" {
				t.Fatalf("molecule root %s after the sweep, want closed", got)
			}
		})
	}
}

// TestAutocloseSweepCensusCoversInProgressRows: the census is every active
// row, so a claimed member closed silently is swept like an open one.
func TestAutocloseSweepCensusCoversInProgressRows(t *testing.T) {
	syncAutoclose(t)
	backing := beads.NewMemStore()
	convoy, _ := backing.Create(beads.Bead{Title: "batch", Type: "convoy"})
	member, _ := backing.Create(beads.Bead{Title: "task", ParentID: convoy.ID})
	inProgress := "in_progress"
	if err := backing.Update(member.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}
	cached := beads.NewCachingStoreForTest(backing, nil)
	if err := cached.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	cs := &controllerState{beadStores: map[string]beads.Store{"r": cached}, eventProv: events.NewFake()}
	cs.runAutocloseSweepPass(sweepTestClock(0))
	if err := cached.Close(member.ID); err != nil {
		t.Fatal(err)
	}
	cs.runAutocloseSweepPass(sweepTestClock(1))
	cs.runAutocloseSweepPass(sweepTestClock(2))
	if got := statusOf(t, backing, convoy.ID); got != "closed" {
		t.Fatalf("convoy %s after an in-progress member closed silently, want closed", got)
	}
}

// TestAutocloseSweepKeepsTheCensusOfAnUnlistableCache: a cache that cannot
// serve its census this pass (a dirty row) is skipped, not read as empty,
// which would queue every active row as a departure.
func TestAutocloseSweepKeepsTheCensusOfAnUnlistableCache(t *testing.T) {
	backing := beads.NewMemStore()
	row, _ := backing.Create(beads.Bead{Title: "task"})
	store := &downGetStore{Store: backing}
	cached := beads.NewCachingStoreForTest(store, nil)
	if err := cached.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	cs := &controllerState{beadStores: map[string]beads.Store{"r": cached}, eventProv: events.NewFake()}
	cs.runAutocloseSweepPass(sweepTestClock(0))

	// The update's refetch fails, leaving the row dirty.
	store.down.Store(true)
	title := "renamed"
	if err := cached.Update(row.ID, beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cached.CachedList(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth}); ok {
		t.Fatal("precondition: the cache still serves its census")
	}
	cs.runAutocloseSweepPass(sweepTestClock(1))
	if cs.autocloseSweepOf().isPending(row.ID) {
		t.Fatal("an unlistable cache's rows were queued as departures")
	}
}

// TestAutocloseSweepCachesFindsEveryFedCache: the sweep censuses a cache
// behind the bead-policy wrapper and a relocated class's cache, not just the
// bare work stores.
func TestAutocloseSweepCachesFindsEveryFedCache(t *testing.T) {
	work := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
	relocated := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
	cs := &controllerState{
		beadStores:    map[string]beads.Store{"r": &beadPolicyStore{Store: work}},
		storageRoutes: splitRoutes(relocated),
	}
	got := map[*beads.CachingStore]bool{}
	for _, cache := range cs.sweepCaches() {
		if got[cache] {
			t.Fatal("sweepCaches listed a cache twice")
		}
		got[cache] = true
	}
	if !got[work] || !got[relocated] || len(got) != 2 {
		t.Fatalf("sweepCaches = %v; want the policy-wrapped work cache and the relocated cache", got)
	}
}

// touchingStore writes to row id right after each live read of it while
// touches remain: a writer racing every fenced close.
type touchingStore struct {
	beads.Store
	inner   beads.Store
	id      string
	touches atomic.Int32
}

func (s *touchingStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if id == s.id && s.touches.Add(-1) >= 0 {
		_ = s.inner.SetMetadata(id, "progress", fmt.Sprint(s.touches.Load()))
	}
	return b, err
}

func (s *touchingStore) ConditionalWritesResolveTarget() beads.Store { return s.inner }

// refusalFixture is a conditional-writes convoy, under a cache, whose last
// open member the caller closes; the backing touches the convoy after each
// live read while touches remain.
func refusalFixture(t *testing.T) (cs *controllerState, mem *beads.MemStore, backing *touchingStore, cached *beads.CachingStore, convoy, member beads.Bead) {
	t.Helper()
	mem = beads.NewMemStore()
	if err := beads.StampOpenedStore(mem, "MemStore", gate.Auto, nil, nil); err != nil {
		t.Fatal(err)
	}
	convoy, _ = mem.Create(beads.Bead{Title: "batch", Type: "convoy"})
	member, _ = mem.Create(beads.Bead{Title: "task", ParentID: convoy.ID})
	backing = &touchingStore{Store: mem, inner: mem, id: convoy.ID}
	cached = beads.NewCachingStoreForTest(backing, nil)
	if err := cached.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	cs = &controllerState{beadStores: map[string]beads.Store{"r": cached}, eventProv: events.NewFake()}
	return cs, mem, backing, cached, convoy, member
}

func closeAndNotify(t *testing.T, cs *controllerState, mem beads.Store, cached *beads.CachingStore, id string) {
	t.Helper()
	if err := cached.Close(id); err != nil {
		t.Fatal(err)
	}
	closed, _ := mem.Get(id)
	payload, _ := beads.EncodeBeadEventPayload(closed)
	cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Actor: cacheLocalActor, Subject: id, Payload: payload})
}

// TestAutocloseRedecidesARefusedClose is the review's CAS-refusal probe: an
// unrelated write lands between the fence's re-read and its close. The run
// re-reads, re-checks and closes on the next attempt.
func TestAutocloseRedecidesARefusedClose(t *testing.T) {
	syncAutoclose(t)
	cs, mem, backing, cached, convoy, member := refusalFixture(t)
	backing.touches.Store(1)
	closeAndNotify(t, cs, mem, cached, member.ID)
	if got := statusOf(t, mem, convoy.ID); got != "closed" {
		t.Fatalf("complete convoy is %s after one refused close", got)
	}
	if !cs.autocloseSweepOf().hasRan(member.ID) || cs.autocloseSweepOf().isPending(member.ID) {
		t.Fatal("a run that closed on retry was not marked handled")
	}
}

// TestAutocloseLeavesAPersistentlyRefusedCloseToTheSweep: every attempt is
// refused, so the trigger is not marked handled; the sweep re-decides it once
// the writer stops.
func TestAutocloseLeavesAPersistentlyRefusedCloseToTheSweep(t *testing.T) {
	syncAutoclose(t)
	cs, mem, backing, cached, convoy, member := refusalFixture(t)
	backing.touches.Store(1 << 20)
	closeAndNotify(t, cs, mem, cached, member.ID)
	if got := statusOf(t, mem, convoy.ID); got != "open" {
		t.Fatalf("convoy %s; every close was refused", got)
	}
	if cs.autocloseSweepOf().hasRan(member.ID) || !cs.autocloseSweepOf().isPending(member.ID) {
		t.Fatal("a refused run was marked handled instead of left to the sweep")
	}
	backing.touches.Store(0)
	if got := cs.runAutocloseSweepPass(time.Now().Add(time.Second)); got.Ran != 1 {
		t.Fatalf("sweep = %+v, want one finished autoclose", got)
	}
	if got := statusOf(t, mem, convoy.ID); got != "closed" {
		t.Fatalf("convoy %s after the sweep re-decided, want closed", got)
	}
}

// listDownStore fails List while down is set: autoclose can read the closed
// bead but not the members its decision rests on.
type listDownStore struct {
	beads.Store
	down atomic.Bool
}

func (s *listDownStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if s.down.Load() {
		return nil, errors.New("backing down")
	}
	return s.Store.List(query)
}

// TestAutocloseLeavesARunWithAFailedReadToTheSweep: a bead is marked handled
// only once autoclose has finished its reads. A read error leaves it pending;
// the sweep retries while the store is down and closes the convoy once it is
// back.
func TestAutocloseLeavesARunWithAFailedReadToTheSweep(t *testing.T) {
	syncAutoclose(t)
	mem := beads.NewMemStore()
	convoy, _ := mem.Create(beads.Bead{Title: "batch", Type: "convoy"})
	member, _ := mem.Create(beads.Bead{Title: "task", ParentID: convoy.ID})
	if err := mem.Close(member.ID); err != nil {
		t.Fatal(err)
	}
	store := &listDownStore{Store: mem}
	store.down.Store(true)
	cs := &controllerState{cityBeadStore: store, eventProv: events.NewFake()}
	closed, _ := mem.Get(member.ID)
	payload, _ := beads.EncodeBeadEventPayload(closed)

	cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Actor: "bd-close", Subject: member.ID, Payload: payload})
	if cs.autocloseSweepOf().hasRan(member.ID) || !cs.autocloseSweepOf().isPending(member.ID) {
		t.Fatal("a run whose reads failed was marked handled")
	}
	now := time.Now().Add(time.Second)
	if got := cs.runAutocloseSweepPass(now); got.Retried != 1 || got.Ran != 0 {
		t.Fatalf("sweep while down = %+v, want one retry", got)
	}
	store.down.Store(false)
	if got := cs.runAutocloseSweepPass(now.Add(autocloseSweepInterval)); got.Ran != 1 {
		t.Fatalf("sweep after recovery = %+v, want one autoclose", got)
	}
	if got := statusOf(t, mem, convoy.ID); got != "closed" {
		t.Fatalf("convoy %s, want closed", got)
	}
}

// TestRefutedCloseNeverRunsAutoclose: autoclose must not run for a close a
// live read refutes, on the event path or the sweep. A stepless graph.v2
// root closes on its source bead's close alone, with no other premise, so
// it is the autoclose a false close would wrongly fire.
func TestRefutedCloseNeverRunsAutoclose(t *testing.T) {
	setup := func(t *testing.T) (*controllerState, *beads.MemStore, beads.Bead, beads.Bead, json.RawMessage) {
		syncAutoclose(t)
		mem := beads.NewMemStore()
		work, _ := mem.Create(beads.Bead{Title: "fix the bug", Type: "task"})
		root, _ := mem.Create(beads.Bead{Title: "mol-focus-review", Type: "task", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.source_bead_id": work.ID,
		}})
		falselyClosed := work
		falselyClosed.Status = "closed"
		payload, _ := beads.EncodeBeadEventPayload(falselyClosed)
		return &controllerState{cityBeadStore: mem, eventProv: events.NewFake()}, mem, work, root, payload
	}
	t.Run("event path", func(t *testing.T) {
		cs, mem, work, root, payload := setup(t)
		cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Actor: cacheReconcileActor, Subject: work.ID, Payload: payload})
		if got := statusOf(t, mem, root.ID); got != "open" {
			t.Fatalf("workflow root %s after its open source bead was falsely closed", got)
		}
	})
	t.Run("sweep", func(t *testing.T) {
		cs, mem, work, root, _ := setup(t)
		cs.autocloseSweepOf().deferID(work.ID, sweepTestClock(0))
		if got := cs.runAutocloseSweepPass(sweepTestClock(0)); got.Refuted != 1 {
			t.Fatalf("sweep = %+v, want one refuted check", got)
		}
		if got := statusOf(t, mem, root.ID); got != "open" {
			t.Fatalf("workflow root %s after the sweep refuted its source close", got)
		}
	})
}

// panicGetStore panics on Get.
type panicGetStore struct{ beads.Store }

func (panicGetStore) Get(string) (beads.Bead, error) { panic("bad row") }

// TestSafeAutocloseSweepPassRecoversAPanic: a panicking pass is logged and
// survived, and leaves the controller lock free.
func TestSafeAutocloseSweepPassRecoversAPanic(t *testing.T) {
	cs := &controllerState{cityBeadStore: panicGetStore{beads.NewMemStore()}, eventProv: events.NewFake()}
	cs.autocloseSweepOf().deferID("gc-1", sweepTestClock(0))
	if !cs.safeAutocloseSweepPass(sweepTestClock(0)) {
		t.Fatal("the panic was not reported")
	}
	cs.mu.Lock()
	cs.mu.Unlock() //nolint:staticcheck // asserting the lock is free
	if cs.safeAutocloseSweepPass(sweepTestClock(1)) {
		t.Fatal("a clean pass reported a panic")
	}
}

// TestLiveReadOwnerReadsTheStoreThatHoldsTheRow: on the unconfigured
// broadcast fallback, the confirming read and autoclose use the store that
// holds the row, not whichever store came first.
func TestLiveReadOwnerReadsTheStoreThatHoldsTheRow(t *testing.T) {
	other, owner := beads.NewMemStore(), beads.NewMemStore()
	row, _ := owner.Create(beads.Bead{Title: "task"})
	store, got, err := liveReadOwner([]beads.Store{other, owner}, row.ID)
	if err != nil || store != owner || got.ID != row.ID {
		t.Fatalf("liveReadOwner = (%p, %q, %v), want the owner %p", store, got.ID, err, owner)
	}
	if _, _, err := liveReadOwner([]beads.Store{other}, row.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("liveReadOwner of a row nowhere: err = %v, want ErrNotFound", err)
	}
}

// TestAutocloseSweepSettle: a finished run marks the id handled; an
// unfinished one, even after an earlier run finished, unmarks it and owes it
// a check, so the sweep does not skip it as handled.
func TestAutocloseSweepSettle(t *testing.T) {
	s := newAutocloseSweep()
	s.settle("gc-1", true, sweepTestClock(0))
	if !s.hasRan("gc-1") || s.isPending("gc-1") {
		t.Fatal("a finished run was not marked handled")
	}
	s.settle("gc-1", false, sweepTestClock(0))
	if s.hasRan("gc-1") || !s.isPending("gc-1") {
		t.Fatal("an unfinished re-run left the id marked handled")
	}
	if got := s.due(sweepTestClock(0)); len(got) != 1 || got[0] != "gc-1" {
		t.Fatalf("due = %v, want [gc-1]", got)
	}
}

// A due close in a suspended rig is not read: the read would restart the rig's
// retired proxy. It is confirmed once the rig resumes. A quiescent city runs
// no pass at all.
func TestAutocloseSweepLeavesASuspendedRigCold(t *testing.T) {
	cs, backing, cached, _, convoy, member := convoySweepFixture(t)
	cs.runAutocloseSweepPass(sweepTestClock(0))
	if err := backing.Close(member.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cached.List(beads.ListQuery{Live: true, Status: "open"}); err != nil {
		t.Fatal(err)
	}
	cached.ReconcileNowForTest()
	cached.ReconcileNowForTest()
	cs.runAutocloseSweepPass(sweepTestClock(1))

	cs.setSuspendedRigs(map[string]bool{"r": true})
	if got := cs.runAutocloseSweepPass(sweepTestClock(2)); got.Ran != 0 {
		t.Fatalf("ran autoclose %d time(s) in a suspended rig", got.Ran)
	}
	if got := statusOf(t, backing, convoy.ID); got != "open" {
		t.Fatalf("convoy %s while its rig is suspended, want open", got)
	}
	quiescent := new(atomic.Bool)
	quiescent.Store(true)
	cs.beadsQuiescent = quiescent
	cs.setSuspendedRigs(nil)
	if got := cs.runAutocloseSweepPass(sweepTestClock(3)); got != (autocloseSweepResult{}) {
		t.Fatalf("a quiescent city ran a sweep pass: %+v", got)
	}
	quiescent.Store(false)
	if got := cs.runAutocloseSweepPass(sweepTestClock(4)); got.Ran != 1 {
		t.Fatalf("after resume the sweep ran autoclose %d time(s), want 1", got.Ran)
	}
}

// quiescingGetStore records the ids it is asked for and turns the city
// quiescent on the first Get, as a suspend landing mid-pass would.
type quiescingGetStore struct {
	beads.Store
	quiescent *atomic.Bool
	mu        sync.Mutex
	read      map[string]bool
}

func (s *quiescingGetStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	if s.read == nil {
		s.read = map[string]bool{}
	}
	s.read[id] = true
	s.mu.Unlock()
	s.quiescent.Store(true)
	return s.Store.Get(id)
}

// A sweep pass that began before the city went quiescent reads no further
// store: the rows still due wait for resume instead of restarting the bd
// pair the quiescent city just retired. No timer is involved: the pass is
// driven directly, and the first live read is what makes the city quiescent.
func TestAutocloseSweepStopsReadingWhenTheCityGoesQuiescentMidPass(t *testing.T) {
	backing := beads.NewMemStore()
	var ids []string
	for i := 0; i < 3; i++ {
		b, err := backing.Create(beads.Bead{Title: "task"})
		if err != nil {
			t.Fatal(err)
		}
		if err := backing.Close(b.ID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, b.ID)
	}
	quiescent := new(atomic.Bool)
	store := &quiescingGetStore{Store: backing, quiescent: quiescent}
	cs := &controllerState{cityBeadStore: store, eventProv: events.NewFake(), beadsQuiescent: quiescent}
	for _, id := range ids {
		cs.autocloseSweepOf().deferID(id, sweepTestClock(0))
	}

	got := cs.runAutocloseSweepPass(sweepTestClock(0))
	store.mu.Lock()
	read := 0
	for _, id := range ids {
		if store.read[id] {
			read++
		}
	}
	store.mu.Unlock()
	if read != 1 {
		t.Fatalf("the pass read %d of the %d due rows, want 1: it kept reading after the city went quiescent", read, len(ids))
	}
	if got.Retried != len(ids)-1 {
		t.Fatalf("pass = %+v, want the %d rows after the flip deferred", got, len(ids)-1)
	}
	pending := 0
	for _, id := range ids {
		if cs.autocloseSweepOf().isPending(id) {
			pending++
		}
	}
	if pending != len(ids)-1 {
		t.Fatalf("%d row(s) still owed a check, want %d", pending, len(ids)-1)
	}
}

// A cache-inferred bead.closed that reaches applyInferredClose while the city
// is quiescent makes no live read (which would restart the retired pair) and
// is owed to the sweep instead.
func TestInferredCloseDefersWhileTheCityIsQuiescent(t *testing.T) {
	prev := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prev })

	quiescent := new(atomic.Bool)
	quiescent.Store(true)
	cs := &controllerState{
		cityBeadStore:  panicGetStore{beads.NewMemStore()},
		eventProv:      events.NewFake(),
		pokeCh:         make(chan struct{}, 1),
		beadsQuiescent: quiescent,
	}
	payload, err := beads.EncodeBeadEventPayload(beads.Bead{ID: "gc-1", Title: "task", Status: "closed"})
	if err != nil {
		t.Fatal(err)
	}
	cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Subject: "gc-1", Actor: cacheReconcileActor, Payload: payload})

	if !cs.autocloseSweepOf().isPending("gc-1") {
		t.Fatal("the inferred close of gc-1 is not owed to the sweep; it would never be confirmed after resume")
	}
}
