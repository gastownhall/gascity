package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// listCountingStore counts the List calls that reach the store under the
// controller's CachingStore, so a test can bound what a poked tick costs.
type listCountingStore struct {
	beads.Store
	mu       sync.Mutex
	lists    int
	failList error
}

func (s *listCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	s.lists++
	failList := s.failList
	s.mu.Unlock()
	if failList != nil {
		return nil, failList
	}
	return s.Store.List(query)
}

func (s *listCountingStore) takeLists() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.lists
	s.lists = 0
	return n
}

func (s *listCountingStore) setFailList(err error) {
	s.mu.Lock()
	s.failList = err
	s.mu.Unlock()
}

// newPokeSnapshotRuntime returns a runtime whose city store is a primed
// CachingStore over backing, the shape the controller runs with.
func newPokeSnapshotRuntime(t *testing.T, backing beads.Store) *CityRuntime {
	t.Helper()
	cache := beads.NewCachingStore(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	return &CityRuntime{
		cityName:            "test-city",
		cfg:                 &config.City{},
		standaloneCityStore: cache,
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              io.Discard,
	}
}

// createSessionBeadOutOfProcess writes a session bead straight to the backing
// store, the way `gc session new` does from its own process: the controller's
// cache sees no write and, with the bd event hooks gone, gets no event for it.
func createSessionBeadOutOfProcess(t *testing.T, backing beads.Store) string {
	t.Helper()
	created, err := backing.Create(beads.Bead{
		Title:  "adhoc",
		Type:   sessionpkg.BeadType,
		Labels: []string{sessionpkg.LabelSession},
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "worker-adhoc",
			"state":        "creating",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	return created.ID
}

func snapshotHasSession(snap *sessionBeadSnapshot, id string) bool {
	if snap == nil {
		return false
	}
	for _, info := range snap.OpenInfos() {
		if info.ID == id {
			return true
		}
	}
	return false
}

// `gc session new` creates the session bead in its own process and then pokes
// the controller. The tick that services the poke must see that bead, or the
// deferred start waits for the cache's reconcile pass, which emits a snapshot
// event that does not poke. The start then sits until the next patrol tick.
func TestPokedTickSessionSnapshotSeesOutOfProcessCreate(t *testing.T) {
	backing := &listCountingStore{Store: beads.NewMemStore()}
	cr := newPokeSnapshotRuntime(t, backing)
	id := createSessionBeadOutOfProcess(t, backing)
	backing.takeLists()

	// A patrol tick stays on the cache. That is the cost bound: only a poke
	// pays for a store read.
	if snapshotHasSession(cr.loadTickSessionBeadSnapshot("patrol"), id) {
		t.Fatalf("patrol snapshot saw %s; the cache was expected to be stale here", id)
	}
	if n := backing.takeLists(); n != 0 {
		t.Fatalf("patrol snapshot issued %d store lists, want 0 (served from cache)", n)
	}

	if !snapshotHasSession(cr.loadTickSessionBeadSnapshot("poke"), id) {
		t.Fatalf("poked tick snapshot is missing session %s written by another process", id)
	}
	// One live union: the type leg and the label leg.
	if n := backing.takeLists(); n != 2 {
		t.Fatalf("poked snapshot issued %d store lists, want 2 (type and label legs)", n)
	}

	// The live read put the row in the cache, so the tick's later snapshot
	// loads see it without another store round trip.
	if !snapshotHasSession(cr.loadSessionBeadSnapshot(), id) {
		t.Fatalf("later cached snapshot is missing session %s after the live read", id)
	}
	if n := backing.takeLists(); n != 0 {
		t.Fatalf("later snapshot issued %d store lists, want 0 (served from cache)", n)
	}
}

func TestStartupPokeTickSessionSnapshotReadsLive(t *testing.T) {
	backing := beads.NewMemStore()
	cr := newPokeSnapshotRuntime(t, backing)
	id := createSessionBeadOutOfProcess(t, backing)

	if !snapshotHasSession(cr.loadTickSessionBeadSnapshot("startup-poke"), id) {
		t.Fatalf("startup-poke snapshot is missing session %s written by another process", id)
	}
}

// A failed live read must not leave the tick worse off than the cached read
// it replaces.
func TestPokedTickSessionSnapshotFallsBackToCacheWhenLiveReadFails(t *testing.T) {
	backing := &listCountingStore{Store: beads.NewMemStore()}
	known := createSessionBeadOutOfProcess(t, backing)
	cr := newPokeSnapshotRuntime(t, backing)

	backing.setFailList(errors.New("store unavailable"))
	snap := cr.loadTickSessionBeadSnapshot("poke")
	if snap == nil {
		t.Fatal("poked snapshot is nil after a live-read failure; want the cached snapshot")
	}
	if !snapshotHasSession(snap, known) {
		t.Fatalf("fallback snapshot is missing cached session %s", known)
	}
}

// The same guarantee through the real tick: the desired-state build of a poked
// tick is fed a session snapshot that includes the out-of-process create.
func TestPokedTickFeedsOutOfProcessSessionToDesiredStateBuild(t *testing.T) {
	backing := &listCountingStore{Store: beads.NewMemStore()}
	cr := newPokeSnapshotRuntime(t, backing)
	cr.cityPath = t.TempDir()
	cr.sp = runtime.NewFake()
	var (
		mu    sync.Mutex
		built []*sessionBeadSnapshot
	)
	cr.buildFnWithSessionBeads = func(_ *config.City, _ runtime.Provider, _ beads.Store, _ map[string]beads.Store, snap *sessionBeadSnapshot, _ *sessionReconcilerTraceCycle) DesiredStateResult {
		mu.Lock()
		built = append(built, snap)
		mu.Unlock()
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}
	id := createSessionBeadOutOfProcess(t, backing)
	sawSession := func() (bool, int) {
		mu.Lock()
		defer mu.Unlock()
		n := len(built)
		saw := false
		for _, snap := range built {
			saw = saw || snapshotHasSession(snap, id)
		}
		built = nil
		return saw, n
	}

	var dirty atomic.Bool
	var lastProviderName string
	var prevPoolRunning map[string]bool
	ctx := context.Background()

	cr.tick(ctx, &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "patrol")
	if saw, n := sawSession(); n == 0 || saw {
		t.Fatalf("patrol tick: builds=%d sawSession=%v, want a build that does not see %s yet (stale cache)", n, saw, id)
	}

	cr.tick(ctx, &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "poke")
	if saw, n := sawSession(); n == 0 || !saw {
		t.Fatalf("poked tick: builds=%d sawSession=%v, want the build to see session %s written by another process", n, saw, id)
	}
}
