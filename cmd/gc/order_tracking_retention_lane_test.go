package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
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

// awaitRetentionPruned waits until the pruning predicate holds.
func awaitRetentionPruned(t *testing.T, pruned func() bool, what string) {
	t.Helper()
	awaitCond(t, pruned, what)
}

// The lane's first pass prunes exactly what the watchdog prunes: the beads
// past the TTL and beyond the retain floor, with a fresh backup, and logs the
// pruned count (AC-4).
func TestOrderTrackingRetentionLanePrunesOnItsFirstPass(t *testing.T) {
	// Real time: the lane stamps its passes with time.Now(), so the backup
	// age and the bead ages must be measured against it, not a fake date.
	now := time.Now()
	cr, store, stderrBuf := seedRetentionWatchdogCity(t, now, time.Hour)
	startRetentionLaneForTest(t, cr)

	awaitRetentionPruned(t, func() bool {
		_, err := store.Get("guard-00")
		return err != nil
	}, "the lane's first pass pruning the eligible beads")

	for _, id := range []string{"guard-00", "guard-01"} {
		if _, err := store.Get(id); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("Get(%s) err = %v, want ErrNotFound (should be pruned)", id, err)
		}
	}
	for i := 2; i < minClosedOrderTrackingRetained+2; i++ {
		id := fmt.Sprintf("guard-%02d", i)
		if _, err := store.Get(id); err != nil {
			t.Fatalf("%s should be preserved at the retain floor: %v", id, err)
		}
	}
	if got := stderrBuf.String(); !strings.Contains(got, "pruned 2 closed bead(s)") {
		t.Fatalf("stderr = %q, want 'pruned 2 closed bead(s)'", got)
	}
	if cr.orderTrackingRetentionWatchdogLast.IsZero() {
		t.Fatal("orderTrackingRetentionWatchdogLast is zero, want the first pass's stamp")
	}
}

// The sweep reaches the orders binding: on a split city every tracking bead
// written since cutover lives there, so a binding-blind sweep prunes nothing
// (AC-4, orders-binding leg).
func TestOrderTrackingRetentionLaneSweepsTheOrdersBinding(t *testing.T) {
	now := time.Now()
	binding := beads.NewMemStoreFrom(700, seedRetentionWatchdogBeads(now, "bind"), nil)
	var stderrBuf bytes.Buffer
	cr := &CityRuntime{
		cityName:            "test-city",
		cityPath:            t.TempDir(),
		cfg:                 &config.City{Workspace: config.Workspace{Name: "test-city"}},
		standaloneCityStore: beads.NewMemStore(),
		storageRoutes:       messagingSplitRoutes(binding),
		stdout:              io.Discard,
		stderr:              &stderrBuf,
		logPrefix:           "gc test",
	}
	startRetentionLaneForTest(t, cr)

	awaitRetentionPruned(t, func() bool {
		return trackingBeadCount(t, binding, "bind") < minClosedOrderTrackingRetained+2
	}, "the lane pruning the orders binding")

	if got := trackingBeadCount(t, binding, "bind"); got != minClosedOrderTrackingRetained {
		t.Fatalf("binding holds %d tracking beads, want the %d retained at the floor", got, minClosedOrderTrackingRetained)
	}
	if got := stderrBuf.String(); !strings.Contains(got, "pruned 2 closed bead(s)") {
		t.Fatalf("stderr = %q, want 'pruned 2 closed bead(s)'", got)
	}
}

// A stale backup still skips the delete while consuming the interval stamp
// (AC-4, backup-age guard leg).
func TestOrderTrackingRetentionLaneSkipsWhenBackupStale(t *testing.T) {
	now := time.Now()
	cr, store, stderrBuf := seedRetentionWatchdogCity(t, now, 48*time.Hour)
	startRetentionLaneForTest(t, cr)

	awaitRetentionPruned(t, func() bool {
		return !cr.orderTrackingRetentionWatchdogLast.IsZero()
	}, "the lane's first pass consuming the interval stamp")

	for i := range minClosedOrderTrackingRetained + 2 {
		id := fmt.Sprintf("guard-%02d", i)
		if _, err := store.Get(id); err != nil {
			t.Fatalf("%s should be preserved when the backup is stale: %v", id, err)
		}
	}
	if got := stderrBuf.String(); !strings.Contains(got, "skipping bulk delete") {
		t.Fatalf("stderr = %q, want the stale-backup skip notice", got)
	}
}

// seedRetentionWatchdogBeads seeds n closed order-tracking beads named
// <prefix>-NN, 8 days old: past the 7d default TTL, so the two oldest of 12
// exceed the retain-10 floor.
func seedRetentionWatchdogBeads(now time.Time, prefix string) []beads.Bead {
	seed := make([]beads.Bead, 0, minClosedOrderTrackingRetained+2)
	for i := range minClosedOrderTrackingRetained + 2 {
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

// trackingBeadCount counts the closed order-tracking beads a store holds.
func trackingBeadCount(t *testing.T, store beads.Store, prefix string) int {
	t.Helper()
	runs, err := orders.NewStore(beads.OrdersStore{Store: store}).ClosedRunsForRetention()
	if err != nil {
		t.Fatalf("listing the binding's closed tracking beads: %v", err)
	}
	n := 0
	for _, run := range runs {
		if strings.Contains(run.ID, prefix+"-") {
			n++
		}
	}
	return n
}

// The retention lane is joined before shutdown() begins: with the lane parked
// in a store call, canceling the city context must not let run()'s deferred
// shutdown start — observed through the order dispatcher drain, shutdown's
// first side effect — until the parked call returns (AC-8). The supervisor's
// forced shutdown is outside this guarantee; see the lane's known limits.
func TestCityRuntimeRunJoinsRetentionSweepBeforeShutdown(t *testing.T) {
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
		awaitClose(t, runDone, "run() returning after the parked sweep is released")
	})

	awaitClose(t, parking.entered, "the retention lane parking in the closed-tracking listing")

	// The lane is parked. Canceling the city context must not start shutdown:
	// run() is joined on the lane inside its deferred cleanup, ahead of the
	// deferred shutdown().
	cancel()
	select {
	case <-runDone:
		t.Fatal("run() returned while the retention sweep was parked: shutdown is not joined behind the lane")
	case <-time.After(2 * time.Second):
	}
	if od.drainCalls != 0 {
		t.Fatalf("order dispatcher drains = %d, want 0 while the sweep is parked: shutdown has begun", od.drainCalls)
	}

	parking.releaseSweep()
	awaitClose(t, runDone, "run() returning once the parked sweep is released")
	if od.drainCalls == 0 {
		t.Fatal("shutdown never drained the order dispatcher after the lane exited")
	}
}

// failingRetentionStore counts Lists and fails them; the assertion reads the
// counter from the test goroutine.
type failingRetentionStore struct {
	beads.Store
	calls atomic.Int32
}

func (s *failingRetentionStore) List(_ beads.ListQuery) ([]beads.Bead, error) {
	s.calls.Add(1)
	return nil, fmt.Errorf("suspended pass must not read the store")
}

// A suspended city gets no store reads from the retention lane: the order
// pass applies the same predicate, because a read on a suspended city
// restarts the proxies suspension retired (AC-5).
func TestOrderTrackingRetentionLanePassMakesNoStoreCallWhenSuspended(t *testing.T) {
	t.Setenv("GC_SUSPENDED", "1")
	failing := &failingRetentionStore{}
	cr := &CityRuntime{
		cityName:            "test-city",
		cityPath:            t.TempDir(),
		cfg:                 &config.City{Workspace: config.Workspace{Name: "test-city"}},
		standaloneCityStore: failing,
		stdout:              io.Discard,
		stderr:              io.Discard,
		logPrefix:           "gc test",
	}
	cr.runOrderTrackingRetentionLanePass(context.Background(), &retentionLaneFSEpisode{})
	if n := failing.calls.Load(); n != 0 {
		t.Fatalf("the suspended pass made %d store call(s), want 0", n)
	}
}
