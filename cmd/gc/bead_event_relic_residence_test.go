package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// Bead events for migrated ("relic") ids on a split city (R4).
//
// `gc storage migrate` copies infrastructure beads into the binding with their
// ORIGINAL ids, so a migrated city's binding holds HQ- and rig-prefixed ids
// (maintainer-city holds ~790). The controller routed every bead event by id
// prefix, so a relic's events reached the WORK cache only: the binding cache
// never applied them, and a close read the work store, missed, was refuted,
// and the relic molecule's root never autoclosed.
//
// The binding is a real SQLite store fenced to the infrastructure namespaces;
// the relics are minted through the foreign-id door the migration uses.

type relicEventFixture struct {
	cs      *controllerState
	binding beads.Store
	cache   *beads.CachingStore
	rootID  string
	stepID  string
	openID  string
}

func newRelicEventFixture(t *testing.T, cached bool) relicEventFixture {
	t.Helper()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "alpha", Path: t.TempDir(), Prefix: "ra"}},
	}
	hq := config.EffectiveHQPrefix(cfg)
	leaf := newSplitEnvClassLeaf(t)
	creator, ok := leaf.(beads.ForeignIDCreator)
	if !ok {
		t.Fatalf("class leaf %T has no foreign-id door", leaf)
	}
	// A migrated root is graph class (the migration copies only
	// infrastructure classes), so it carries its wisp marker: the fixture now
	// matches what the production migration puts in the binding.
	root, err := creator.CreateWithForeignID(beads.Bead{ID: hq + "-901", Title: "Formula: mol-pre-cutover", Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}})
	if err != nil {
		t.Fatal(err)
	}
	step, err := creator.CreateWithForeignID(beads.Bead{
		ID: hq + "-902", Title: "Step 1", Type: "step", ParentID: root.ID,
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	open, err := creator.CreateWithForeignID(beads.Bead{ID: hq + "-903", Title: "pre-cutover session", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	// Any residence violation the foreign-id door recorded is the migration's
	// own, deliberate shape.
	_ = splittest.TakeResidenceViolations(leaf)

	routes := splitRoutes(leaf)
	f := relicEventFixture{binding: leaf, rootID: root.ID, stepID: step.ID, openID: open.ID}
	if cached {
		routes = routes.withControllerCache(context.Background(), events.NewFake())
		store, _ := routes.storeFor(coordclassFor(config.BeadClassGraph))
		cache, ok := store.(*beads.CachingStore)
		if !ok {
			t.Fatalf("controller routes hold %T, want a *beads.CachingStore", store)
		}
		f.cache = cache
		f.binding = cache
	}
	f.cs = &controllerState{
		cfg:           cfg,
		cityPath:      t.TempDir(),
		cityBeadStore: splittest.NewWorkStore(t, hq),
		beadStores:    map[string]beads.Store{"alpha": splittest.NewWorkStore(t, "ra")},
		storageRoutes: routes,
		pokeCh:        make(chan struct{}, 1),
	}
	return f
}

func TestRelicCloseEventAutoclosesItsMoleculeRoot(t *testing.T) {
	prev := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prev })
	for _, actor := range []string{"agent", cacheLocalActor} {
		t.Run(actor, func(t *testing.T) {
			f := newRelicEventFixture(t, false)
			if err := f.binding.Close(f.stepID); err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(beads.Bead{ID: f.stepID, Title: "Step 1", Type: "step", Status: "closed"})
			if err != nil {
				t.Fatal(err)
			}
			stores := f.cs.beadEventStores(f.stepID)
			if !storesInclude(stores, f.binding) || !storesInclude(stores, f.cs.cityBeadStore) {
				t.Errorf("event stores for relic %s = %d store(s), want the prefix route's work store AND the binding that holds it", f.stepID, len(stores))
			}
			f.cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Actor: actor, Subject: f.stepID, Payload: payload})
			got, err := f.binding.Get(f.rootID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "closed" {
				t.Errorf("relic molecule root %s status=%q after its only step closed (actor %s), want closed", f.rootID, got.Status, actor)
			}
		})
	}
}

// A relic written out of process (a one-shot `gc bd update` served from the
// binding) must refresh the controller's binding cache from its event, not
// wait for the next rescan.
func TestRelicUpdateEventRefreshesTheBindingCache(t *testing.T) {
	f := newRelicEventFixture(t, true)
	before, err := f.cache.Get(f.openID)
	if err != nil {
		t.Fatalf("binding cache does not hold relic %s: %v", f.openID, err)
	}
	if before.Title != "pre-cutover session" {
		t.Fatalf("cached title = %q", before.Title)
	}
	newTitle := "renamed out of process"
	if err := f.cache.Backing().Update(f.openID, beads.UpdateOpts{Title: &newTitle}); err != nil {
		t.Fatal(err)
	}
	fresh, err := f.cache.Backing().Get(f.openID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(fresh)
	if err != nil {
		t.Fatal(err)
	}
	f.cs.applyBeadEventToStores(events.Event{Type: events.BeadUpdated, Actor: "gc", Subject: f.openID, Payload: payload})
	got, err := f.cache.Get(f.openID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != newTitle {
		t.Errorf("binding cache serves relic %s with title %q after its update event, want %q", f.openID, got.Title, newTitle)
	}
}

// Work ids keep their prefix route: an id the binding does not hold goes to
// the work store its prefix names, on the same fixture.
func TestWorkIDEventKeepsItsPrefixRouteOnAMigratedCity(t *testing.T) {
	f := newRelicEventFixture(t, false)
	work, err := f.cs.cityBeadStore.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	stores := f.cs.beadEventStores(work.ID)
	if len(stores) != 1 || stores[0] != f.cs.cityBeadStore {
		t.Fatalf("event stores for work bead %s = %d store(s), want exactly the city work store", work.ID, len(stores))
	}
}

// ga-s1ms9: an event whose id no configured prefix owns reaches the binding
// when the binding holds it. The only such id a binding holds is a migrated
// relic with a foreign prefix, and the residence probe routes it there; the
// broadcast itself does not include the binding, because the binding cache's
// namespace filter drops an unowned id and every cache drops an id-less one.
func TestUnownedBeadEventReachesTheBindingThatHoldsIt(t *testing.T) {
	f := newRelicEventFixture(t, false)
	creator := f.binding.(beads.ForeignIDCreator)
	if _, err := creator.CreateWithForeignID(beads.Bead{ID: "zz-404", Title: "foreign relic", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	_ = splittest.TakeResidenceViolations(f.binding)
	if stores := f.cs.beadEventStores("zz-404"); !storesInclude(stores, f.binding) {
		t.Fatalf("event stores for unowned id zz-404 held by the binding omit the binding (%d stores)", len(stores))
	}
	if stores := f.cs.beadEventStores("zz-405"); storesInclude(stores, f.binding) {
		t.Fatalf("event stores for unowned id zz-405 the binding does not hold include the binding")
	}
	if stores := f.cs.beadEventStores(""); storesInclude(stores, f.binding) {
		t.Fatalf("the id-less broadcast includes the binding, whose cache drops an id-less payload")
	}
}

// Review 7: a work bead that reuses a relic's id keeps its own route. Its
// events reach the work store as well as the binding, and a committed close is
// autoclosed in the store whose row reads closed.
func TestRelicIDCollisionKeepsTheWorkRoute(t *testing.T) {
	f := newRelicEventFixture(t, false)
	work := f.cs.cityBeadStore
	creator, ok := work.(beads.ForeignIDCreator)
	if !ok {
		t.Skipf("work leaf %T cannot pin an id", work)
	}
	if _, err := creator.CreateWithForeignID(beads.Bead{ID: f.openID, Title: "work bead reusing the relic id", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	stores := f.cs.beadEventStores(f.openID)
	if !storesInclude(stores, work) || !storesInclude(stores, f.binding) {
		t.Fatalf("event stores for colliding id %s = %d store(s), want the work store and the binding", f.openID, len(stores))
	}
	if err := work.Close(f.openID); err != nil {
		t.Fatal(err)
	}
	if owner := committedCloseOwner(stores, f.openID); owner != work {
		t.Fatalf("committed close owner for %s is not the work store whose row closed", f.openID)
	}
}

// Review 8: an inferred close of a relic is confirmed in the store whose row
// reads closed, on the event path and on the sweep, as a committed close is. A
// work bead reusing the relic's id holds the first row a read finds; while that
// row is open it must not refute the relic's close, or the relic molecule
// never autocloses. The autoclose runs in the binding, in the ID-only store-ref
// mode relic closes take.
func TestRelicIDCollisionInferredCloseAutoclosesTheRelic(t *testing.T) {
	syncAutoclose(t)
	for _, path := range []string{"event", "sweep"} {
		t.Run(path, func(t *testing.T) {
			f := newRelicEventFixture(t, false)
			work := f.cs.cityBeadStore
			creator, ok := work.(beads.ForeignIDCreator)
			if !ok {
				t.Fatalf("work leaf %T cannot pin an id", work)
			}
			if _, err := creator.CreateWithForeignID(beads.Bead{ID: f.stepID, Title: "work bead reusing the relic id", Type: "task"}); err != nil {
				t.Fatal(err)
			}
			if err := f.binding.Close(f.stepID); err != nil {
				t.Fatal(err)
			}
			stores := f.cs.beadEventStores(f.stepID)
			if len(stores) != 2 || stores[0] != work || stores[1] != f.binding {
				t.Fatalf("event stores for colliding id %s = %d store(s), want the work store, then the binding", f.stepID, len(stores))
			}
			owner, live, err := liveCloseOwner(stores, f.stepID, f.binding)
			if err != nil || owner != f.binding || live.Status != "closed" {
				t.Fatalf("close owner for %s = (%T, %q, %v), want the binding's closed row", f.stepID, owner, live.Status, err)
			}
			f.cs.mu.RLock()
			prefixRef := f.cs.autocloseStoreRefLocked(f.stepID)
			f.cs.mu.RUnlock()
			if prefixRef == "" {
				t.Fatalf("precondition: the prefix route of %s has no store ref, so the ID-only mode is indistinguishable", f.stepID)
			}
			if ref := relicStoreRef(owner, f.binding, prefixRef); ref != "" {
				t.Fatalf("store ref for the relic's close = %q, want the ID-only mode", ref)
			}
			switch path {
			case "event":
				payload, err := json.Marshal(beads.Bead{ID: f.stepID, Title: "Step 1", Type: "step", Status: "closed"})
				if err != nil {
					t.Fatal(err)
				}
				f.cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Actor: "agent", Subject: f.stepID, Payload: payload})
			case "sweep":
				f.cs.autocloseSweepOf().deferID(f.stepID, sweepTestClock(0))
				if got := f.cs.runAutocloseSweepPass(sweepTestClock(0)); got.Ran != 1 || got.Refuted != 0 {
					t.Fatalf("sweep pass = %+v, want the relic's close confirmed and its autoclose run", got)
				}
			}
			root, err := f.binding.Get(f.rootID)
			if err != nil {
				t.Fatal(err)
			}
			if root.Status != "closed" {
				t.Errorf("relic molecule root %s status=%q after its only step closed beside an open work bead of the same id, want closed", f.rootID, root.Status)
			}
			if got, err := work.Get(f.stepID); err != nil || got.Status != "open" {
				t.Errorf("colliding work bead %s = (%q, %v) after the relic's close, want it open and untouched", f.stepID, got.Status, err)
			}
		})
	}
}

func storesInclude(stores []beads.Store, want beads.Store) bool {
	for _, s := range stores {
		if s == want {
			return true
		}
	}
	return false
}

// A city that relocates nothing broadcasts to its work stores exactly as
// before.
func TestUnownedBeadEventBroadcastIsUnchangedWithoutRoutes(t *testing.T) {
	work := beads.NewMemStore()
	rig := beads.NewMemStore()
	cs := &controllerState{
		cfg:           &config.City{Workspace: config.Workspace{Name: "test-city"}, Rigs: []config.Rig{{Name: "alpha", Path: t.TempDir(), Prefix: "ra"}}},
		cityBeadStore: work,
		beadStores:    map[string]beads.Store{"alpha": rig},
	}
	if got := cs.beadEventStores(""); len(got) != 2 {
		t.Fatalf("broadcast stores = %d, want the two work stores", len(got))
	}
}
