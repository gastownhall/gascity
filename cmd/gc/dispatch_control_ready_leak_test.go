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

// closeCountingStore is a beads.Store that counts CloseStore calls and Ready
// reads and poisons its read methods after close, so a test can prove how many
// times a handle was closed, how many reads a scan made against it, and that
// no read reached it after it was closed.
type closeCountingStore struct {
	*beads.MemStore
	closeCount  atomic.Int64
	readyCount  atomic.Int64
	closed      atomic.Bool
	getNotFound bool // when true, Get always answers ErrNotFound (dispatch error path)
	readyErr    error
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

func (s *closeCountingStore) readies() int64 { return s.readyCount.Load() }

// List poisons after close so a stray read from a closed handle surfaces loudly.
func (s *closeCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.closed.Load() {
		return nil, fmt.Errorf("closeCountingStore.List called after CloseStore (use-after-close)")
	}
	return s.MemStore.List(q)
}

// Ready counts reads and poisons after close.
func (s *closeCountingStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	if s.closed.Load() {
		return nil, fmt.Errorf("closeCountingStore.Ready called after CloseStore (use-after-close)")
	}
	s.readyCount.Add(1)
	if s.readyErr != nil {
		return nil, s.readyErr
	}
	return s.MemStore.Ready(q...)
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

// installControlReadyLegSourcesFn swaps the source seam for the duration of a
// test.
func installControlReadyLegSourcesFn(t *testing.T, fn func(dir, cityPath string, cfg *config.City) (sources, owned []beads.Store, err error)) {
	t.Helper()
	prev := controlReadyLegSourcesFn
	controlReadyLegSourcesFn = fn
	t.Cleanup(func() { controlReadyLegSourcesFn = prev })
}

// TestControlReadyLegsReadyClosesOwnedSourcesPerScan is the regression pin for
// the WAL-starvation leak (#6255) and the scan's cost (ga-vnycm2.19): every
// scan reads each leg exactly once, answers from that read, and releases the
// scoped backing it opened before returning.
func TestControlReadyLegsReadyClosesOwnedSourcesPerScan(t *testing.T) {
	dir := t.TempDir()

	var mu sync.Mutex
	var minted []*closeCountingStore
	installControlReadyLegSourcesFn(t, func(_, _ string, _ *config.City) ([]beads.Store, []beads.Store, error) {
		f := newCloseCountingStore(t, true)
		mu.Lock()
		minted = append(minted, f)
		mu.Unlock()
		return []beads.Store{f}, []beads.Store{f}, nil
	})

	const scans = 3
	for i := 0; i < scans; i++ {
		ready, opened, err := controlReadyLegsReady(dir, dir, nil)
		if err != nil || !opened {
			t.Fatalf("scan %d: controlReadyLegsReady = opened %t, err %v; want opened, nil", i, opened, err)
		}
		if len(ready) != 1 {
			t.Fatalf("scan %d: ready = %d beads, want the 1 seeded ready bead", i, len(ready))
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(minted) != scans {
		t.Fatalf("opens = %d, want %d (each scan reads a freshly opened scoped store)", len(minted), scans)
	}
	for i, f := range minted {
		if got := f.closes(); got != 1 {
			t.Fatalf("scan %d's scoped leg closed %d times, want 1: scoped backings are leaking", i, got)
		}
		if got := f.readies(); got != 1 {
			t.Fatalf("scan %d's scoped leg read %d times, want exactly one Ready per leg per scan", i, got)
		}
	}
}

// TestControlReadyLegsReadyNeverClosesSharedBindingLeg pins the ownership
// boundary: the process-shared graph binding must never be closed by a scan,
// only the scoped leg this call opened, and each leg is read once per scan.
func TestControlReadyLegsReadyNeverClosesSharedBindingLeg(t *testing.T) {
	dir := t.TempDir()

	shared := newCloseCountingStore(t, true)
	var mu sync.Mutex
	var ownedMinted []*closeCountingStore
	installControlReadyLegSourcesFn(t, func(_, _ string, _ *config.City) ([]beads.Store, []beads.Store, error) {
		owned := newCloseCountingStore(t, true)
		mu.Lock()
		ownedMinted = append(ownedMinted, owned)
		mu.Unlock()
		// sources = {owned scoped leg, shared binding leg}; owned = {scoped leg}.
		return []beads.Store{owned, shared}, []beads.Store{owned}, nil
	})

	const scans = 3
	for i := 0; i < scans; i++ {
		if _, opened, err := controlReadyLegsReady(dir, dir, nil); err != nil || !opened {
			t.Fatalf("scan %d: controlReadyLegsReady = opened %t, err %v; want opened, nil", i, opened, err)
		}
	}

	if got := shared.closes(); got != 0 {
		t.Fatalf("shared binding leg closed %d times, want 0: closing the process-shared binding poisons every later graph-class op", got)
	}
	if got := shared.readies(); got != scans {
		t.Fatalf("shared binding leg read %d times over %d scans, want one Ready per scan", got, scans)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, f := range ownedMinted {
		if f.closes() != 1 || f.readies() != 1 {
			t.Fatalf("scan %d's scoped leg: closes = %d, readies = %d; want 1 and 1", i, f.closes(), f.readies())
		}
	}
}

// TestControlReadyLegsReadyClosesOwnedSourcesOnReadFailure covers the read
// failure return: a leg whose Ready fails fails the scan loudly, and the
// opened backing is still closed rather than leaked.
func TestControlReadyLegsReadyClosesOwnedSourcesOnReadFailure(t *testing.T) {
	dir := t.TempDir()

	failing := newCloseCountingStore(t, false)
	failing.readyErr = fmt.Errorf("ready unavailable")
	installControlReadyLegSourcesFn(t, func(_, _ string, _ *config.City) ([]beads.Store, []beads.Store, error) {
		return []beads.Store{failing}, []beads.Store{failing}, nil
	})

	ready, opened, err := controlReadyLegsReady(dir, dir, nil)
	if err == nil || !opened || ready != nil {
		t.Fatalf("controlReadyLegsReady = (%v, %t, %v), want (nil, true, error) when a leg's read fails", ready, opened, err)
	}
	if got := failing.closes(); got != 1 {
		t.Fatalf("failing scoped leg closed %d times, want 1: the read-failure return must not leak the opened handle", got)
	}
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
