package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// closeCountingStore is a beads.Store that counts CloseStore calls and poisons
// its read methods after close, so a test can prove (a) how many times a handle
// was closed and (b) that CachedReady never touches the backing once the
// snapshot is in memory. It embeds *beads.MemStore so PrimeActive's list/dep
// scan behaves like a real in-memory store during priming.
type closeCountingStore struct {
	*beads.MemStore
	closeCount  atomic.Int64
	closed      atomic.Bool
	getNotFound bool // when true, Get always answers ErrNotFound (dispatch error path)
}

func newCloseCountingStore(t *testing.T, seedReady bool) *closeCountingStore {
	t.Helper()
	mem := beads.NewMemStore()
	if seedReady {
		if _, err := mem.Create(beads.Bead{Type: "task", Assignee: "control-dispatcher"}); err != nil {
			t.Fatalf("seed ready bead: %v", err)
		}
	}
	return &closeCountingStore{MemStore: mem}
}

func (s *closeCountingStore) CloseStore() error { //nolint:unparam // must satisfy the CloseStore() error store interface closeBeadStoreHandle asserts
	s.closeCount.Add(1)
	s.closed.Store(true)
	return nil
}

func (s *closeCountingStore) closes() int64 { return s.closeCount.Load() }

// List poisons after close so a stray read from a closed handle surfaces loudly.
func (s *closeCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.closed.Load() {
		return nil, fmt.Errorf("closeCountingStore.List called after CloseStore (use-after-close)")
	}
	return s.MemStore.List(q)
}

// Get either forces the not-found dispatch path or poisons after close.
func (s *closeCountingStore) Get(id string) (beads.Bead, error) {
	if s.getNotFound {
		return beads.Bead{}, beads.ErrNotFound
	}
	if s.closed.Load() {
		return beads.Bead{}, fmt.Errorf("closeCountingStore.Get called after CloseStore (use-after-close)")
	}
	return s.MemStore.Get(id)
}

// forceControlReadyCacheStale rewinds a scope's primed timestamp past the TTL so
// the next controlReadyCachesFor call re-primes instead of reusing the entry --
// no clock seam needed.
func forceControlReadyCacheStale(t *testing.T, dir string) {
	t.Helper()
	controlReadyCacheRegistry.mu.Lock()
	defer controlReadyCacheRegistry.mu.Unlock()
	entry, ok := controlReadyCacheRegistry.byDir[dir]
	if !ok {
		t.Fatalf("forceControlReadyCacheStale: no cache entry for %q", dir)
	}
	entry.primedAt = entry.primedAt.Add(-2 * controlReadyCacheTTL)
}

// installControlReadyCacheSourcesFn swaps the source seam for the duration of a
// test and drops the scope's registry entry on cleanup so the global registry
// never leaks fixtures across tests.
func installControlReadyCacheSourcesFn(t *testing.T, dir string, fn func(dir, cityPath string, cfg *config.City) (sources, owned []beads.Store, err error)) {
	t.Helper()
	prev := controlReadyCacheSourcesFn
	controlReadyCacheSourcesFn = fn
	t.Cleanup(func() {
		controlReadyCacheSourcesFn = prev
		controlReadyCacheRegistry.mu.Lock()
		delete(controlReadyCacheRegistry.byDir, dir)
		controlReadyCacheRegistry.mu.Unlock()
	})
}

// TestControlReadyCachesForClosesOwnedSourcesPerPrime is the regression pin for
// the WAL-starvation leak: every TTL-stale re-prime must release the scoped
// backing it opened, and the primed snapshot must keep answering afterward.
func TestControlReadyCachesForClosesOwnedSourcesPerPrime(t *testing.T) {
	dir := t.TempDir()

	var mu sync.Mutex
	var minted []*closeCountingStore
	installControlReadyCacheSourcesFn(t, dir, func(_, _ string, _ *config.City) ([]beads.Store, []beads.Store, error) {
		f := newCloseCountingStore(t, true)
		mu.Lock()
		minted = append(minted, f)
		mu.Unlock()
		return []beads.Store{f}, []beads.Store{f}, nil
	})

	const primes = 3
	var caches []*beads.CachingStore
	for i := 0; i < primes; i++ {
		caches = controlReadyCachesFor(dir, dir, nil)
		if len(caches) != 1 {
			t.Fatalf("prime %d: controlReadyCachesFor returned %d caches, want 1", i, len(caches))
		}
		forceControlReadyCacheStale(t, dir)
	}

	mu.Lock()
	opens := int64(len(minted))
	var closes int64
	for _, f := range minted {
		closes += f.closes()
	}
	mu.Unlock()

	if opens != primes {
		t.Fatalf("opens = %d, want %d (each stale re-prime opens a fresh scoped store)", opens, primes)
	}
	// The leak fix must bound live handles: opens minus closes is the count of
	// still-open backings, and the fix closes each per prime, so it is 0 here and
	// must never exceed 1. On the pre-fix base closes == 0, so opens-closes == 3.
	if opens-closes > 1 {
		t.Fatalf("opens-closes = %d (opens=%d closes=%d), want <= 1: scoped backings are leaking", opens-closes, opens, closes)
	}

	// The last prime's snapshot must still answer from memory even though its
	// backing was closed -- the poisoned fake proves CachedReady never touched it.
	if _, ok := cachedControlReadyUnion(caches); !ok {
		t.Fatal("cachedControlReadyUnion reported unavailable after the backing was closed; CachedReady must serve from the in-memory snapshot")
	}
}

// TestControlReadyCachesForNeverClosesSharedBindingLeg pins the ownership
// boundary: the process-shared graph binding must never be closed by the cache
// prime, only the scoped leg this call opened.
func TestControlReadyCachesForNeverClosesSharedBindingLeg(t *testing.T) {
	dir := t.TempDir()

	shared := newCloseCountingStore(t, false)
	var mu sync.Mutex
	var ownedMinted []*closeCountingStore
	installControlReadyCacheSourcesFn(t, dir, func(_, _ string, _ *config.City) ([]beads.Store, []beads.Store, error) {
		owned := newCloseCountingStore(t, true)
		mu.Lock()
		ownedMinted = append(ownedMinted, owned)
		mu.Unlock()
		// sources = {owned scoped leg, shared binding leg}; owned = {scoped leg}.
		return []beads.Store{owned, shared}, []beads.Store{owned}, nil
	})

	const primes = 3
	for i := 0; i < primes; i++ {
		if got := controlReadyCachesFor(dir, dir, nil); len(got) != 2 {
			t.Fatalf("prime %d: got %d caches, want 2 (scoped + binding legs)", i, len(got))
		}
		forceControlReadyCacheStale(t, dir)
	}

	if got := shared.closes(); got != 0 {
		t.Fatalf("shared binding leg closed %d times, want 0: closing the process-shared binding poisons every later graph-class op", got)
	}
	mu.Lock()
	var ownedCloses int64
	for _, f := range ownedMinted {
		ownedCloses += f.closes()
	}
	ownedOpens := int64(len(ownedMinted))
	mu.Unlock()
	if ownedCloses != ownedOpens {
		t.Fatalf("owned scoped legs: closes = %d, want %d (one per prime)", ownedCloses, ownedOpens)
	}
}

// TestControlReadyCachesForClosesOwnedSourcesOnPrimeFailure covers the
// prime-failure early return: a source that cannot prime must still have its
// opened backing closed rather than leaked.
func TestControlReadyCachesForClosesOwnedSourcesOnPrimeFailure(t *testing.T) {
	dir := t.TempDir()

	failing := &primeFailingStore{closeCountingStore: newCloseCountingStore(t, false)}
	installControlReadyCacheSourcesFn(t, dir, func(_, _ string, _ *config.City) ([]beads.Store, []beads.Store, error) {
		return []beads.Store{failing}, []beads.Store{failing}, nil
	})

	if got := controlReadyCachesFor(dir, dir, nil); got != nil {
		t.Fatalf("controlReadyCachesFor = %v, want nil when a leg fails to prime", got)
	}
	if got := failing.closes(); got != 1 {
		t.Fatalf("failing scoped leg closed %d times, want 1: the prime-failure early return must not leak the opened handle", got)
	}
}

// primeFailingStore fails every List so CachingStore.PrimeActive returns an
// error, exercising controlReadyCachesFor's early-return path.
type primeFailingStore struct {
	*closeCountingStore
}

func (s *primeFailingStore) List(_ beads.ListQuery) ([]beads.Bead, error) {
	return nil, fmt.Errorf("primeFailingStore: list unavailable")
}

// TestRunControlDispatcherInStoreClosesScopeStoreOnError pins the second leak
// site: runControlDispatcherInStore must close the scope store it opened even
// when the dispatch fails before completing.
func TestRunControlDispatcherInStoreClosesScopeStoreOnError(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	fake := newCloseCountingStore(t, false)
	fake.getNotFound = true // controlBeadLedger.Get -> ErrNotFound -> dispatch returns an error

	prev := openControlStoreForDispatch
	openControlStoreForDispatch = func(_, _ string, _ *config.City) (beads.Store, error) {
		return fake, nil
	}
	t.Cleanup(func() { openControlStoreForDispatch = prev })

	var stdout, stderr bytes.Buffer
	err := runControlDispatcherInStore(cityDir, cityDir, "ga-missing-control", &stdout, &stderr)
	if err == nil {
		t.Fatalf("runControlDispatcherInStore: err = nil, want an error for a missing control bead (stderr=%q)", stderr.String())
	}
	if got := fake.closes(); got != 1 {
		t.Fatalf("scope store closed %d times, want 1: the dispatch error path must not leak the opened store", got)
	}
}

// TestRunControlDispatcherInStoreClosesScopeStoreOnSuccess pins the same leak
// site on the path the serve loop actually takes. Both paths share one
// unconditional defer today, so the error-path sibling above would stay green
// under a restructure that closed only inside the error branch — while every
// successful dispatch leaked a store again, which is the per-bead leak this
// file exists to prevent.
func TestRunControlDispatcherInStoreClosesScopeStoreOnSuccess(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	fake := newCloseCountingStore(t, false)
	// An orphaned scope-check is the cheapest control bead that dispatches all
	// the way to a processed result: gc.root_bead_id names a root the store
	// does not hold, so ProcessControl closes the control bead and reports
	// Processed without an error, and scope-check needs no city-config
	// resolution. Status is forced to "open" by Create, which is what keeps
	// this off ProcessControl's not-open skip.
	control, err := fake.Create(beads.Bead{
		Type: "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindScopeCheck,
			beadmeta.RootBeadIDMetadataKey:   "ga-missing-root",
			beadmeta.RootStoreRefMetadataKey: "city:test-city",
		},
	})
	if err != nil {
		t.Fatalf("seed control bead: %v", err)
	}

	prev := openControlStoreForDispatch
	openControlStoreForDispatch = func(_, _ string, _ *config.City) (beads.Store, error) {
		return fake, nil
	}
	t.Cleanup(func() { openControlStoreForDispatch = prev })

	var stdout, stderr bytes.Buffer
	if err := runControlDispatcherInStore(cityDir, cityDir, control.ID, &stdout, &stderr); err != nil {
		t.Fatalf("runControlDispatcherInStore: %v (stderr=%q)", err, stderr.String())
	}
	// Assert the dispatch actually reached the processed branch. Without this
	// the test would still pass if the bead stopped qualifying and ProcessControl
	// returned a nil error from an early skip, which would silently stop
	// covering the success path it is named for.
	if !strings.Contains(stdout.String(), "action=orphaned-workflow") {
		t.Fatalf("stdout = %q, want a processed control dispatch (stderr=%q)", stdout.String(), stderr.String())
	}
	if got := fake.closes(); got != 1 {
		t.Fatalf("scope store closed %d times, want 1: the dispatch success path must not leak the opened store", got)
	}
}
