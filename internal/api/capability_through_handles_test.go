package api

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/storeref"
)

// capabilityStore has every optional capability the API reaches for on a
// store it is handed, counting each call: the count that bounds /v0/beads and
// /status, the context-aware ready read /status prefers, the row witness
// store health trusts, the parent-projection wait a reparent awaits, and the
// cache-only list /agents and the order history read first.
type capabilityStore struct {
	*beads.MemStore
	counts, readyCtx, waits, cachedLists atomic.Int64
}

func (s *capabilityStore) Count(context.Context, beads.ListQuery, ...string) (int, error) {
	s.counts.Add(1)
	return 7, nil
}

func (s *capabilityStore) ReadyContext(_ context.Context, q ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.readyCtx.Add(1)
	return s.Ready(q...)
}

func (s *capabilityStore) SawRows() bool { return true }

func (s *capabilityStore) WaitForParentProjection(context.Context, string, string, string) error {
	s.waits.Add(1)
	return nil
}

func (s *capabilityStore) CachedList(q beads.ListQuery) ([]beads.Bead, bool) {
	s.cachedLists.Add(1)
	rows, err := s.List(q)
	return rows, err == nil
}

// asHandedState hands out its rig stores as the test put them in, with no
// split-strict work view over them: a handle reaches a handler as the mint
// will hand it one, with any gate view inside the handle rather than over it.
type asHandedState struct{ *fakeState }

func (s asHandedState) BeadStore(rig string) beads.Store { return s.stores[rig] }

func (s asHandedState) BeadStores() map[string]beads.Store { return s.stores }

// Stores mints the handles over the stores as the test put them in, with no
// projection, as BeadStore and BeadStores hand them out.
func (s asHandedState) Stores() storeref.Stores {
	return mintFakeStores(s.cityBeadStore, s.stores, s.relocatedClasses(), storeref.StoresOptions{}, s.cfg, s.ClassBindingHasLegacyResidents)
}

// apiHandleWraps are the shapes T6 hands the API's store helpers: the raw
// store (the control) and a class handle over it of each class a handler
// serves through Stores().
var apiHandleWraps = map[string]func(beads.Store) beads.Store{
	"the store":        func(s beads.Store) beads.Store { return s },
	"a work handle":    func(s beads.Store) beads.Store { return beadstest.Work(s) },
	"a graph handle":   func(s beads.Store) beads.Store { return beadstest.Graph(s) },
	"an orders handle": func(s beads.Store) beads.Store { return beadstest.Orders(s) },
}

// TestAPICapabilitiesReachTheStoreThroughAHandle (typed-stores T6a) pins each
// of the API's ten capability lookups to answer through a class handle exactly
// as through the raw store. A direct type assertion answers "no" on a handle,
// with no compile error: the /v0/beads count bounding disengages (an unbounded
// list over a binding), /status falls to List and to ScopedStoreLike, store
// health stops trusting its witness, a reparent stops waiting for its
// projection, /agents and the order history bypass the cache, and the cache
// gate and age header go silent.
func TestAPICapabilitiesReachTheStoreThroughAHandle(t *testing.T) {
	ctx := context.Background()
	for name, wrap := range apiHandleWraps {
		t.Run(name, func(t *testing.T) {
			state := newFakeState(t)
			store := &capabilityStore{MemStore: beads.NewMemStore()}

			// handler_status.go statusStoredWorkCounts: Counter.
			if _, err := statusStoredWorkCounts(ctx, state, wrap(store)); err != nil || store.counts.Load() == 0 {
				t.Errorf("statusStoredWorkCounts made %d counts (%v), want the Counter path", store.counts.Load(), err)
			}
			// store_health.go countBeadStoreRows: Counter.
			store.counts.Store(0)
			if n, err := countBeadStoreRows(ctx, state, wrap(store)); err != nil || n != 7 || store.counts.Load() != 1 {
				t.Errorf("countBeadStoreRows = %d after %d counts (%v), want the Counter's 7", n, store.counts.Load(), err)
			}
			// huma_handlers_beads.go beadListLegCount: Counter.
			if n, ok := beadListLegCount(ctx, wrap(store), "", &BeadListInput{}); !ok || n != 7 {
				t.Errorf("beadListLegCount = %d, %v; want the Counter's 7", n, ok)
			}
			// handler_status.go statusReadyStoreWithTimeout: ContextReadyReader.
			if _, err := statusReadyStoreWithTimeout(ctx, state, wrap(store)); err != nil || store.readyCtx.Load() != 1 {
				t.Errorf("statusReadyStoreWithTimeout made %d context-aware ready reads (%v), want 1", store.readyCtx.Load(), err)
			}
			// store_health.go measuredStoreRows: RowWitness.
			if _, err := measuredStoreRows(wrap(store), 0, countBranchCounter); !errors.Is(err, errRowCountContradicted) {
				t.Errorf("measuredStoreRows over a witness that saw rows = %v, want errRowCountContradicted", err)
			}
			// huma_handlers_orders.go orderHistoryBeadsAcrossStoreInfosCachedFirst: CachedLister.
			if _, err := orderHistoryBeadsAcrossStoreInfosCachedFirst([]workflowStoreInfo{{ref: "city", store: wrap(store)}}, "o", 10, time.Time{}); err != nil || store.cachedLists.Load() != 1 {
				t.Errorf("order history made %d cache-only lists (%v), want 1", store.cachedLists.Load(), err)
			}
			// handler_agents.go findActiveBeadForAssigneesWithFreshness: CachedLister.
			state.stores = map[string]beads.Store{"myrig": wrap(store)}
			routed := asHandedState{state}
			store.cachedLists.Store(0)
			New(routed).findActiveBeadForAssigneesWithFreshness("myrig", false, "worker")
			if store.cachedLists.Load() != 1 {
				t.Errorf("the active-bead lookup made %d cache-only lists, want 1", store.cachedLists.Load())
			}
			// huma_handlers_beads.go humaHandleBeadUpdate: ParentProjectionWaiter.
			parent, err := store.Create(beads.Bead{Title: "parent"})
			if err != nil {
				t.Fatal(err)
			}
			child, err := store.Create(beads.Bead{Title: "child"})
			if err != nil {
				t.Fatal(err)
			}
			h := newTestCityHandler(t, routed)
			rec := beadRequest(t, h, http.MethodPost, cityURL(routed, "/bead/")+child.ID+"/update", `{"parent":"`+parent.ID+`"}`)
			if rec.Code != http.StatusOK || store.waits.Load() != 1 {
				t.Errorf("reparent through %s: status %d after %d projection waits, want 200 after 1; body: %s", name, rec.Code, store.waits.Load(), rec.Body.String())
			}
		})
	}
}

// cacheHandleProbes are cacheLiveOr503 and cacheAgeSeconds over a handle of
// each class a handler can name: one instantiation per class, so every arm of
// classAs is exercised.
var cacheHandleProbes = map[string]struct {
	live func(beads.Store) error
	age  func(beads.Store) cacheFreshness
}{
	"a work handle": {
		func(s beads.Store) error { return cacheLiveOr503(beadstest.Work(s)) },
		func(s beads.Store) cacheFreshness { return cacheAgeSeconds(beadstest.Work(s)) },
	},
	"a graph handle": {
		func(s beads.Store) error { return cacheLiveOr503(beadstest.Graph(s)) },
		func(s beads.Store) cacheFreshness { return cacheAgeSeconds(beadstest.Graph(s)) },
	},
	"a sessions handle": {
		func(s beads.Store) error { return cacheLiveOr503(beadstest.Sessions(s)) },
		func(s beads.Store) cacheFreshness { return cacheAgeSeconds(beadstest.Sessions(s)) },
	},
	"a mail handle": {
		func(s beads.Store) error { return cacheLiveOr503(beadstest.Mail(s)) },
		func(s beads.Store) cacheFreshness { return cacheAgeSeconds(beadstest.Mail(s)) },
	},
	"an orders handle": {
		func(s beads.Store) error { return cacheLiveOr503(beadstest.Orders(s)) },
		func(s beads.Store) cacheFreshness { return cacheAgeSeconds(beadstest.Orders(s)) },
	},
	"a nudges handle": {
		func(s beads.Store) error { return cacheLiveOr503(beadstest.Nudges(s)) },
		func(s beads.Store) cacheFreshness { return cacheAgeSeconds(beadstest.Nudges(s)) },
	},
}

// TestCacheGateAndAgeReachACacheThroughAHandle pins cache_liveness.go's two
// lookups through a class handle of every class over a real CachingStore: the
// 503 fires for a cache that is not live, and the age header reports the
// cache's freshness.
func TestCacheGateAndAgeReachACacheThroughAHandle(t *testing.T) {
	for name, probe := range cacheHandleProbes {
		cold := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
		if err := probe.live(cold); err == nil {
			t.Errorf("%s over an unprimed cache: cacheLiveOr503 = nil, want the 503", name)
		}
		primed := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
		if err := primed.Prime(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := probe.live(primed); err != nil {
			t.Errorf("%s over a live cache: cacheLiveOr503 = %v, want nil", name, err)
		}
		restore := SetLivenessClockForTest(&clock.Fake{Time: primed.Stats().LastFreshAt.Add(9 * time.Second)})
		if got := probe.age(primed); got.ageS != 9 || got.refresh.State == "" {
			t.Errorf("%s over a primed cache: cacheAgeSeconds = %+v, want age 9 and a refresh state", name, got)
		}
		restore()
	}
}
