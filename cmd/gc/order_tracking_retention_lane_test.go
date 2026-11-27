package main

import (
	"context"
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
)

// retentionParkingStore parks the closed order-tracking listing the retention
// sweep reads (orders.Store.ClosedRunsForRetention's query: closed, the
// order-tracking label, both tiers) until release is closed, signaling entered
// on the first one. Every other read passes through, so a test can hold the
// sweep alone while the rest of the runtime keeps working. closedTrackingLists
// counts the parking queries that returned.
type retentionParkingStore struct {
	beads.Store
	entered             chan struct{}
	enteredOnce         sync.Once
	release             chan struct{}
	releaseOnce         sync.Once
	closedTrackingLists atomic.Int32
}

func (s *retentionParkingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Status == "closed" && q.Label == labelOrderTracking && q.TierMode == beads.TierBoth {
		s.enteredOnce.Do(func() { close(s.entered) })
		<-s.release
		s.closedTrackingLists.Add(1)
	}
	return s.Store.List(q)
}

func (s *retentionParkingStore) releaseSweep() {
	s.releaseOnce.Do(func() { close(s.release) })
}

// startRetentionLaneForTest starts the retention lane and registers a cleanup
// that releases any parked sweep before stopping the lane and waiting for it,
// so no pass outlives the test or deadlocks on it.
func startRetentionLaneForTest(t *testing.T, cr *CityRuntime) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stop := cr.startOrderTrackingRetentionLane(ctx)
	t.Cleanup(func() {
		cancel()
		stop()
	})
}

// The retention lane runs beside the orders lane, not inside it: with the
// sweep parked in a store call, an orders lane pass must still complete and
// dispatch (#6429).
func TestOrdersLanePassDoesNotWaitForRetentionSweep(t *testing.T) {
	od := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, od, "1h", nil)
	store := &retentionParkingStore{
		Store:   beads.NewMemStore(),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	cr.standaloneCityStore = store
	startRetentionLaneForTest(t, cr)
	t.Cleanup(store.releaseSweep)

	awaitClose(t, store.entered, "the retention lane reaching the closed-tracking listing")

	passDone := make(chan struct{})
	go func() {
		defer close(passDone)
		cr.runOrdersLanePass(context.Background(), cr.cityPath, ordersLaneReasonCadence)
	}()
	select {
	case <-passDone:
	case <-time.After(5 * time.Second):
		close(store.release)
		<-passDone
		t.Fatal("the orders lane pass waited on the retention sweep parked in a store call")
	}
	if !od.called.Load() {
		t.Fatal("the orders lane pass did not dispatch orders")
	}
}

// The retention lane's first pass runs at once, not one interval after the
// lane starts: the lane is started after readiness, and a backlog that just
// crossed the TTL should not wait another 15 minutes on top of it.
func TestOrderTrackingRetentionLaneFirstPassRunsAtOnce(t *testing.T) {
	od := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, od, "1h", nil)
	started := time.Now()
	listed := make(chan struct{})
	store := &retentionParkingStore{
		Store:   beads.NewMemStore(),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	// Signal the first listing, then let it through.
	go func() {
		<-store.entered
		close(listed)
		store.releaseSweep()
	}()
	cr.standaloneCityStore = store
	t.Cleanup(store.releaseSweep)

	startRetentionLaneForTest(t, cr)

	select {
	case <-listed:
	case <-time.After(5 * time.Second):
		t.Fatalf("the retention lane's first pass did not run within 5s of start (started %v)", time.Since(started))
	}
}

// The startup order pass no longer sweeps, and the retention lane starts only
// once the city is ready: with the closed-tracking listing parked, run() must
// still fire onStarted and then park the lane's first pass in that listing.
// On main the startup pass parked inside the sweep and the city never became
// ready (#6429).
func TestCityRuntimeRunReachesReadyWhileRetentionListingBlocks(t *testing.T) {
	t.Setenv(fsPressureThresholdEnv, "100")
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	parking := &retentionParkingStore{
		Store:   beads.NewMemStore(),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
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
		OnStarted: func() { startedOnce.Do(func() { close(started) }) },
		Dops:      newDrainOps(sp),
		Rec:       events.Discard,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
	})
	if runtimeErr != nil {
		t.Fatalf("building the city runtime: %v", runtimeErr)
	}
	od := &recordingOrderDispatcher{}
	cr.od = od
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = parking
	cr.setControllerState(cs)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		cr.run(ctx)
	}()
	t.Cleanup(func() {
		parking.releaseSweep()
		cancel()
		awaitClose(t, runDone, "run() returning after cancellation")
	})

	awaitClose(t, started, "onStarted firing while the closed-tracking listing is parked")
	awaitClose(t, parking.entered, "the retention lane parking in the closed-tracking listing after readiness")
}
