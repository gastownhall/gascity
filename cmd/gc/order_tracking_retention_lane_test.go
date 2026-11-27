package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

// retentionListingStore parks the closed order-tracking listing the retention
// sweep reads (orders.Store.ClosedRunsForRetention) until release closes,
// signaling entered on the first one, and counts the store calls the sweep
// makes. A nil release never parks. Embedding the beads.Store interface (not
// *MemStore) hides DeleteBatch, so a sweep deletes bead by bead.
type retentionListingStore struct {
	beads.Store
	release   <-chan struct{}
	entered   chan struct{}
	enterOnce sync.Once
	// listings counts retention listings started; listed counts those that
	// returned.
	listings atomic.Int32
	listed   atomic.Int32
	// calls counts every List, Get, Delete and DepList the store serves.
	calls atomic.Int32
}

func newRetentionListingStore(inner beads.Store, release <-chan struct{}) *retentionListingStore {
	return &retentionListingStore{Store: inner, release: release, entered: make(chan struct{})}
}

// isRetentionListing reports whether q is the retention sweep's listing: every
// closed order-tracking bead, newest first, unbounded. The startup backlog
// advisory's listing carries a limit and no sort, so it never matches.
func isRetentionListing(q beads.ListQuery) bool {
	return q.Status == "closed" && q.Label == labelOrderTracking &&
		q.Sort == beads.SortCreatedDesc && q.Limit == 0
}

func (s *retentionListingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.calls.Add(1)
	if !isRetentionListing(q) {
		return s.Store.List(q)
	}
	s.listings.Add(1)
	s.enterOnce.Do(func() { close(s.entered) })
	if s.release != nil {
		<-s.release
	}
	defer s.listed.Add(1)
	return s.Store.List(q)
}

func (s *retentionListingStore) Get(id string) (beads.Bead, error) {
	s.calls.Add(1)
	return s.Store.Get(id)
}

func (s *retentionListingStore) Delete(id string) error {
	s.calls.Add(1)
	return s.Store.Delete(id)
}

func (s *retentionListingStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.calls.Add(1)
	return s.Store.DepList(id, direction)
}

// retentionEligibleSeed is n closed order-tracking runs of one order, all
// 8 days old (past the 7d default TTL), oldest first: with n above
// minClosedOrderTrackingRetained, the n-10 oldest are prunable.
func retentionEligibleSeed(prefix string, n int, now time.Time) []beads.Bead {
	seed := make([]beads.Bead, 0, n)
	for i := range n {
		seed = append(seed, beads.Bead{
			ID:        fmt.Sprintf("%s-%02d", prefix, i),
			Title:     "order:" + prefix,
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-8*24*time.Hour + time.Duration(i)*time.Minute),
			Labels:    []string{"order-run:" + prefix, labelOrderTracking},
			Ephemeral: true,
		})
	}
	return seed
}

// writeFreshRetentionBackup stamps a backup taken an hour ago under cityPath,
// so the retention watchdog's backup-age guard lets the sweep delete.
func writeFreshRetentionBackup(t *testing.T, cityPath string, now time.Time) {
	t.Helper()
	backupDir := filepath.Join(cityPath, ".beads", "backup")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", backupDir, err)
	}
	stateJSON := fmt.Sprintf(`{"timestamp":%q}`, now.Add(-time.Hour).Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(backupDir, "backup_state.json"), []byte(stateJSON), 0o644); err != nil {
		t.Fatalf("write backup_state.json: %v", err)
	}
}

// The startup order pass runs before the cold-start reconcile and readiness,
// so it must not run the retention sweep: with prunable beads, a fresh backup
// and a zero stamp, it neither lists nor deletes closed tracking runs and
// leaves the stamp for the retention lane (#6429).
func TestStartupOrderPassDoesNotRunRetentionSweep(t *testing.T) {
	now := time.Now()
	cr, store, _ := seedRetentionWatchdogCity(t, now, time.Hour)
	listing := newRetentionListingStore(store, nil)
	cr.standaloneCityStore = listing
	od := &recordingOrderDispatcher{}
	cr.od = od

	cr.dispatchOrders(context.Background(), cr.cityPath)

	if !od.called.Load() {
		t.Fatal("the startup order pass did not reach the order dispatcher")
	}
	if n := listing.listings.Load(); n != 0 {
		t.Fatalf("closed order-tracking retention listings = %d, want 0 in the order pass", n)
	}
	for _, id := range []string{"guard-00", "guard-01"} {
		if _, err := store.Get(id); err != nil {
			t.Fatalf("Get(%s) = %v: the order pass pruned a closed tracking bead", id, err)
		}
	}
	if !cr.orderTrackingRetentionWatchdogLast.IsZero() {
		t.Fatalf("orderTrackingRetentionWatchdogLast = %v, want zero after the order pass", cr.orderTrackingRetentionWatchdogLast)
	}
}

// retentionRunHarness is a city runtime that starts cleanly through run(),
// whose city store parks the retention listing until release closes.
type retentionRunHarness struct {
	cr      *CityRuntime
	store   *retentionListingStore
	od      *drainSignalOrderDispatcher
	started chan struct{}
	release func()
}

// drainSignalOrderDispatcher closes drained on its first drain: shutdown()
// drains the order dispatchers before it stops sessions and closes the
// storage binding, so drained marks shutdown under way.
type drainSignalOrderDispatcher struct {
	drained   chan struct{}
	drainOnce sync.Once
}

func (d *drainSignalOrderDispatcher) dispatch(context.Context, string, time.Time) {}

func (d *drainSignalOrderDispatcher) drain(context.Context) bool {
	d.drainOnce.Do(func() { close(d.drained) })
	return true
}

func newRetentionRunHarness(t *testing.T) *retentionRunHarness {
	t.Helper()
	t.Setenv(fsPressureThresholdEnv, "100")
	now := time.Now()
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	writeFreshRetentionBackup(t, cityPath, now)
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	started := make(chan struct{})
	var startedOnce sync.Once
	cr, runtimeErr := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:      newDrainOps(sp),
		Rec:       events.Discard,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		OnStarted: func() { startedOnce.Do(func() { close(started) }) },
	})
	if runtimeErr != nil {
		t.Fatalf("building the city runtime: %v", runtimeErr)
	}
	od := &drainSignalOrderDispatcher{drained: make(chan struct{})}
	cr.od = od
	gate := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	t.Cleanup(release)
	store := newRetentionListingStore(beads.NewMemStoreFrom(100, retentionEligibleSeed("run", minClosedOrderTrackingRetained+2, now), nil), gate)
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = store
	cr.setControllerState(cs)
	return &retentionRunHarness{cr: cr, store: store, od: od, started: started, release: release}
}

// A restart must not wait for the retention sweep: with the closed
// order-tracking listing parked, run() still fires onStarted and publishes
// readiness (#6429: a 73k-bead backlog kept a city not-ready for minutes).
func TestCityRuntimeRunReachesReadyWhileRetentionListingBlocks(t *testing.T) {
	h := newRetentionRunHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		h.cr.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		h.release()
		awaitClose(t, runDone, "run() returning after cancellation")
	})

	awaitClose(t, h.started, "onStarted while the retention listing is parked")
	if n := h.store.listed.Load(); n != 0 {
		t.Fatalf("retention listings returned = %d, want 0: readiness waited for the listing", n)
	}
}

// parkedRetentionRuntime is an orders-lane test runtime whose standalone
// store holds prunable closed tracking runs behind a fresh backup, and parks
// the retention listing until the returned release runs (also at cleanup).
func parkedRetentionRuntime(t *testing.T, od orderDispatcher, stderr io.Writer) (*CityRuntime, *retentionListingStore, func()) {
	t.Helper()
	cr := ordersLaneTestRuntime(t, od, "1h", stderr)
	now := time.Now()
	writeFreshRetentionBackup(t, cr.cityPath, now)
	gate := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	t.Cleanup(release)
	store := newRetentionListingStore(beads.NewMemStoreFrom(100, retentionEligibleSeed("park", minClosedOrderTrackingRetained+2, now), nil), gate)
	cr.standaloneCityStore = store
	return cr, store, release
}

// An orders-lane pass must not wait for the retention sweep: with the
// retention lane parked inside its listing, a pass completes and dispatches.
func TestOrdersLanePassDoesNotWaitForRetentionSweep(t *testing.T) {
	od := &recordingOrderDispatcher{}
	cr, store, release := parkedRetentionRuntime(t, od, nil)
	cr.managedDoltOwned = func(string) (bool, error) { return false, nil }

	stop := cr.startOrderTrackingRetentionLane(context.Background())
	t.Cleanup(func() {
		release()
		stop()
	})
	awaitClose(t, store.entered, "the retention lane reaching its listing")

	passDone := make(chan struct{})
	go func() {
		defer close(passDone)
		cr.runOrdersLanePass(context.Background(), cr.cityPath, ordersLaneReasonCadence)
	}()
	// The bound asserts the pass does not wait on the parked listing; it is
	// the minimum safe deadline for a timer racing a goroutine.
	select {
	case <-passDone:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("the orders-lane pass waited on the parked retention listing")
	}
	if !od.called.Load() {
		t.Fatal("the orders-lane pass did not reach the order dispatcher")
	}
}
