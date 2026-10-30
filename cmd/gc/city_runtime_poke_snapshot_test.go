package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// mc-03yxh: `gc session new`'s poke lands while a tick runs, and the patrol
// ticker fires during the same tick. The loop then takes the poke (arming its
// debouncer) and the ticker in either order. When the ticker wins, the patrol
// must not drop the poke's live read: the tick it runs is the poke's, so its
// desired-state build sees the session another process wrote. Before the fix
// the patrol dropped the poke and read the stale cache, and the session sat
// start-pending until some later, unrelated poke (97-129s on stage-dolt).
func TestPatrolOverPendingPokeTickSeesOutOfProcessCreate(t *testing.T) {
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

	pokeDB, ctrlDB := newTickDebouncer(), newTickDebouncer()
	pokeDB.arm(0) // the select took the poke first
	ctrlDB.arm(0)
	trigger := patrolTickTrigger(pokeDB, ctrlDB) // then the ticker

	var dirty atomic.Bool
	var lastProviderName string
	var prevPoolRunning map[string]bool
	cr.tick(context.Background(), &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, trigger)
	mu.Lock()
	saw := false
	for _, snap := range built {
		saw = saw || snapshotHasSession(snap, id)
	}
	n := len(built)
	mu.Unlock()
	if n == 0 || !saw {
		t.Fatalf("patrol over a pending poke (trigger %q): builds=%d sawSession=%v, want the build to see session %s written by another process", trigger, n, saw, id)
	}
	// The patrol's tick was the poke's: no second tick is queued for it, and
	// the control-dispatcher fire is still superseded.
	if got := drainFiredCount(pokeDB, 5*time.Millisecond); got != 0 {
		t.Fatalf("poke fires left after the patrol = %d, want 0", got)
	}
	if got := drainFiredCount(ctrlDB, 5*time.Millisecond); got != 0 {
		t.Fatalf("control-dispatcher fires left after the patrol = %d, want 0", got)
	}
}

func TestPatrolTickTrigger(t *testing.T) {
	tests := []struct {
		name string
		arm  func(pokeDB, ctrlDB *tickDebouncer)
		want string
	}{
		{name: "nothing pending", arm: func(_, _ *tickDebouncer) {}, want: "patrol"},
		{name: "queued poke fire", arm: func(p, _ *tickDebouncer) { p.arm(0) }, want: "poke"},
		{name: "debouncing poke", arm: func(p, _ *tickDebouncer) { p.arm(time.Hour) }, want: "poke"},
		{name: "control dispatch only", arm: func(_, c *tickDebouncer) { c.arm(0) }, want: "patrol"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pokeDB, ctrlDB := newTickDebouncer(), newTickDebouncer()
			tt.arm(pokeDB, ctrlDB)
			if got := patrolTickTrigger(pokeDB, ctrlDB); got != tt.want {
				t.Fatalf("patrolTickTrigger = %q, want %q", got, tt.want)
			}
			if pokeDB.cancelPending() || ctrlDB.cancelPending() {
				t.Fatal("patrolTickTrigger left a fire pending")
			}
		})
	}
}

// The run loop's patrol case itself (mc-03yxh). With a 1h
// tick_debounce a poke stays pending (debouncing) when the 50ms patrol fires,
// so only a patrol that runs the poke's tick reads sessions live and sees the
// session another process wrote. Real time, no fake clock: the debounce dwarfs
// the patrol, so the interleaving is forced.
func TestRunLoopPatrolOverDebouncingPokeSeesOutOfProcessCreate(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	appendToFile(t, tomlPath, "\n[daemon]\npatrol_interval = \"50ms\"\ntick_debounce = \"1h\"\n")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	backing := beads.NewMemStore()
	sp := runtime.NewFake()
	var (
		mu    sync.Mutex
		want  string
		seen  bool
		ticks int
	)
	ready := make(chan struct{})
	var readyOnce sync.Once
	pokeCh := make(chan struct{}, 1)
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath, CityName: "test-city", TomlPath: tomlPath, Cfg: cfg, SP: sp, PokeCh: pokeCh,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		BuildFnWithSessionBeads: func(_ *config.City, _ runtime.Provider, _ beads.Store, _ map[string]beads.Store, snap *sessionBeadSnapshot, _ *sessionReconcilerTraceCycle) DesiredStateResult {
			mu.Lock()
			defer mu.Unlock()
			ticks++
			if want != "" && snapshotHasSession(snap, want) {
				seen = true
			}
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:      newDrainOps(sp),
		Rec:       events.Discard,
		OnStarted: func() { readyOnce.Do(func() { close(ready) }) },
		Stdout:    io.Discard,
		Stderr:    io.Discard,
	})
	cr.od = &recordingOrderDispatcher{}
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = primeTestCache(t, backing)
	cr.setControllerState(cs)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cr.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	awaitClose(t, ready, "readiness")

	id := createSessionBeadOutOfProcess(t, backing) // gc session new, in its own process
	mu.Lock()
	want = id
	mu.Unlock()
	pokeCh <- struct{}{} // then its poke
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		s, n := seen, ticks
		mu.Unlock()
		if s {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("after 5s (%d builds, patrol every 50ms) no tick saw session %s: the patrol dropped the debouncing poke", n, id)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
