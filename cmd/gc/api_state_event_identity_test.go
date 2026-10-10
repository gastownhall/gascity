package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// countBeadCloseAutocloseDispatches replaces the autoclose dispatcher with one
// that only counts launches, so a test can prove a bead.closed event did or did
// not start convoy/wisp/molecule autoclose.
func countBeadCloseAutocloseDispatches(t *testing.T) *int {
	t.Helper()
	prev := beadCloseAutocloseDispatch
	var launched int
	beadCloseAutocloseDispatch = func(func()) { launched++ }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prev })
	return &launched
}

func primedCache(t *testing.T, seed ...beads.Bead) (*beads.CachingStore, *beads.MemStore) {
	t.Helper()
	backing := beads.NewMemStore()
	backing.HonorExplicitIDs = true
	for _, b := range seed {
		if _, err := backing.Create(b); err != nil {
			t.Fatalf("seed %s: %v", b.ID, err)
		}
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return cache, backing
}

// closedSnapshotInBacking closes id in the backing store only, as an
// out-of-process bd close would, and returns the bead.closed payload its hook
// would carry.
func closedSnapshotInBacking(t *testing.T, backing *beads.MemStore, id string) json.RawMessage {
	t.Helper()
	if err := backing.Close(id); err != nil {
		t.Fatalf("close %s in backing: %v", id, err)
	}
	closed, err := backing.Get(id)
	if err != nil {
		t.Fatalf("get closed %s: %v", id, err)
	}
	payload, err := beads.EncodeBeadEventPayload(closed)
	if err != nil {
		t.Fatalf("encode %s snapshot: %v", id, err)
	}
	return payload
}

func identityTestController(cityStore, rigStore beads.Store) *controllerState {
	return &controllerState{
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city", Prefix: "ct"},
			Rigs:      []config.Rig{{Name: "rig1", Prefix: "rw"}},
		},
		cityName:      "test-city",
		cityBeadStore: cityStore,
		beadStores:    map[string]beads.Store{"rig1": rigStore},
		pokeCh:        make(chan struct{}, 1),
	}
}

// A bead event whose envelope subject names one bead while its payload carries
// another bead's snapshot must be dropped. Routing by the subject delivered the
// rig bead's snapshot to the city cache (inserting a foreign row), started
// autoclose for the subject bead, and poked the controller, while the rig
// cache that owns the payload bead never saw the event.
func TestControllerStateDropsBeadEventWithMismatchedSubject(t *testing.T) {
	launched := countBeadCloseAutocloseDispatches(t)
	cityStore, _ := primedCache(t, beads.Bead{ID: "ct-1", Title: "city bead", Type: "task"})
	rigStore, rigBacking := primedCache(t, beads.Bead{ID: "rw-1", Title: "rig bead", Type: "task"})
	cs := identityTestController(cityStore, rigStore)

	cs.applyBeadEventToStores(events.Event{
		Type:    events.BeadClosed,
		Actor:   "bd-hook",
		Subject: "ct-1",
		Payload: closedSnapshotInBacking(t, rigBacking, "rw-1"),
	})

	if got, err := cityStore.Get("rw-1"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("city cache Get(rw-1) = (%+v, %v), want ErrNotFound: mismatched event leaked a rig snapshot into the city cache", got, err)
	}
	if got, err := cityStore.Get("ct-1"); err != nil || got.Status != "open" {
		t.Fatalf("city cache Get(ct-1) = (%+v, %v), want untouched open bead", got, err)
	}
	if got, err := rigStore.Get("rw-1"); err != nil || got.Status != "open" {
		t.Fatalf("rig cache Get(rw-1) = (%+v, %v), want untouched open bead", got, err)
	}
	if *launched != 0 {
		t.Fatalf("autoclose launched %d times for a mismatched close event, want 0", *launched)
	}
	select {
	case <-cs.pokeCh:
		t.Fatal("mismatched bead event poked the controller")
	default:
	}
}

// Within a single store the subject still drives close-side work: a mismatched
// close would run autoclose for the subject bead even though the payload says a
// different bead closed.
func TestControllerStateDropsSameStoreBeadEventWithMismatchedSubject(t *testing.T) {
	launched := countBeadCloseAutocloseDispatches(t)
	cityStore, cityBacking := primedCache(t,
		beads.Bead{ID: "ct-1", Title: "subject bead", Type: "task"},
		beads.Bead{ID: "ct-2", Title: "payload bead", Type: "task"},
	)
	cs := identityTestController(cityStore, nil)

	cs.applyBeadEventToStores(events.Event{
		Type:    events.BeadClosed,
		Subject: "ct-1",
		Payload: closedSnapshotInBacking(t, cityBacking, "ct-2"),
	})

	for _, id := range []string{"ct-1", "ct-2"} {
		if got, err := cityStore.Get(id); err != nil || got.Status != "open" {
			t.Fatalf("cache Get(%s) = (%+v, %v), want untouched open bead", id, got, err)
		}
	}
	if *launched != 0 {
		t.Fatalf("autoclose launched %d times for a mismatched close event, want 0", *launched)
	}
}

// Normal events are unaffected: a matching subject reaches only the owning
// cache, starts autoclose, and pokes the controller.
func TestControllerStateAppliesBeadEventWithMatchingSubject(t *testing.T) {
	launched := countBeadCloseAutocloseDispatches(t)
	cityStore, _ := primedCache(t)
	rigStore, rigBacking := primedCache(t, beads.Bead{ID: "rw-1", Title: "rig bead", Type: "task"})
	cs := identityTestController(cityStore, rigStore)

	cs.applyBeadEventToStores(events.Event{
		Type:    events.BeadClosed,
		Subject: " rw-1 ",
		Payload: closedSnapshotInBacking(t, rigBacking, "rw-1"),
	})

	if got, err := rigStore.Get("rw-1"); err != nil || got.Status != "closed" {
		t.Fatalf("rig cache Get(rw-1) = (%+v, %v), want closed", got, err)
	}
	if _, err := cityStore.Get("rw-1"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("city cache Get(rw-1) error = %v, want ErrNotFound", err)
	}
	if *launched != 1 {
		t.Fatalf("autoclose launched %d times, want 1", *launched)
	}
	select {
	case <-cs.pokeCh:
	default:
		t.Fatal("matching bead event did not poke the controller")
	}
}

// A subjectless event takes its identity from the payload snapshot, so the
// close-side work that keys on the bead ID runs for the payload bead: closing a
// convoy's last open member through a subjectless close closes the convoy.
func TestControllerStateSubjectlessCloseEventUsesPayloadIdentity(t *testing.T) {
	prev := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prev })

	backing := beads.NewMemStore()
	convoy, err := backing.Create(beads.Bead{Title: "batch", Type: "convoy"})
	if err != nil {
		t.Fatalf("Create convoy: %v", err)
	}
	child, err := backing.Create(beads.Bead{Title: "task", ParentID: convoy.ID})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	cached := beads.NewCachingStoreForTest(backing, nil)
	if err := cached.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := backing.Close(child.ID); err != nil {
		t.Fatalf("Close child: %v", err)
	}
	closed, err := backing.Get(child.ID)
	if err != nil {
		t.Fatalf("Get child: %v", err)
	}
	payload, err := json.Marshal(closed)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	cs := &controllerState{
		beadStores: map[string]beads.Store{"test": cached},
		pokeCh:     make(chan struct{}, 1),
	}

	cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Payload: payload})

	if got, err := cached.Get(child.ID); err != nil || got.Status != "closed" {
		t.Fatalf("cache Get(child) = (%+v, %v), want closed", got, err)
	}
	got, err := backing.Get(convoy.ID)
	if err != nil {
		t.Fatalf("Get convoy: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("convoy status = %q after its last member closed via a subjectless event, want closed", got.Status)
	}
}

// A bead.closed the autoclose dispatcher gets to after the city went
// quiescent reads nothing: the quiescent city retired its bd pairs, and a
// read would restart them. The close is owed to the sweep, which confirms it
// after resume (acceptance: TestProxiedSuspensionIsQuiescenceSuspendedCity,
// whose window caught a drained session's wisp autoclose restarting the
// city's pair).
func TestBeadCloseAutocloseDefersWhileTheCityIsQuiescent(t *testing.T) {
	prev := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prev })

	backing := beads.NewMemStore()
	convoy, err := backing.Create(beads.Bead{Title: "batch", Type: "convoy"})
	if err != nil {
		t.Fatalf("Create convoy: %v", err)
	}
	child, err := backing.Create(beads.Bead{Title: "task", ParentID: convoy.ID})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	cached := beads.NewCachingStoreForTest(backing, nil)
	if err := cached.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	payload := closedSnapshotInBacking(t, backing, child.ID)
	quiescent := new(atomic.Bool)
	quiescent.Store(true)
	cs := &controllerState{
		beadStores:     map[string]beads.Store{"test": cached},
		pokeCh:         make(chan struct{}, 1),
		beadsQuiescent: quiescent,
	}

	cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Subject: child.ID, Payload: payload})

	got, err := backing.Get(convoy.ID)
	if err != nil {
		t.Fatalf("Get convoy: %v", err)
	}
	if got.Status == "closed" {
		t.Fatal("a quiescent city's autoclose closed the convoy: it read and wrote a store it had retired")
	}
	if !cs.autocloseSweepOf().isPending(child.ID) {
		t.Fatalf("the close of %s is not owed to the sweep; it would never be confirmed after resume", child.ID)
	}

	quiescent.Store(false)
	if res := cs.runAutocloseSweepPass(time.Now().Add(time.Second)); res.Ran != 1 {
		t.Fatalf("after resume the sweep = %+v, want one confirmed close", res)
	}
	if got, err := backing.Get(convoy.ID); err != nil || got.Status != "closed" {
		t.Fatalf("convoy after resume = (%+v, %v), want closed", got, err)
	}
}

// A committed close's autoclose run is store work from dispatch until it
// returns, so the city's pairs are not retired while it is queued or reading;
// a quiescent city's committed close leaves no work counted behind it.
func TestCommittedCloseCountsItsAutocloseAsStoreWork(t *testing.T) {
	var queued []func()
	prev := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { queued = append(queued, fn) }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prev })

	backing := beads.NewMemStore()
	convoy, err := backing.Create(beads.Bead{Title: "batch", Type: "convoy"})
	if err != nil {
		t.Fatalf("Create convoy: %v", err)
	}
	child, err := backing.Create(beads.Bead{Title: "task", ParentID: convoy.ID})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	cached := beads.NewCachingStoreForTest(backing, nil)
	if err := cached.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	payload := closedSnapshotInBacking(t, backing, child.ID)
	quiescent := new(atomic.Bool)
	cs := &controllerState{
		beadStores:     map[string]beads.Store{"test": cached},
		pokeCh:         make(chan struct{}, 1),
		beadsQuiescent: quiescent,
	}

	cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Subject: child.ID, Payload: payload})
	if len(queued) != 1 {
		t.Fatalf("dispatched %d autoclose run(s), want 1", len(queued))
	}
	if !cs.storeWorkInFlight() {
		t.Fatal("a dispatched autoclose run is not counted as store work until it runs")
	}
	queued[0]()
	if cs.storeWorkInFlight() {
		t.Fatal("store work still counted after the autoclose run returned")
	}

	quiescent.Store(true)
	cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Subject: child.ID, Payload: payload})
	if len(queued) != 1 {
		t.Fatalf("a quiescent city dispatched an autoclose run (%d total)", len(queued))
	}
	if cs.storeWorkInFlight() {
		t.Fatal("a quiescent city's deferred committed close left store work counted")
	}
}
