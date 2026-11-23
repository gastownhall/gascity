package main

import (
	"context"
	"errors"
	"fmt"
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

// storeWatchdogSeed is order-tracking history every store watchdog has work
// in: one stale open tracking bead (the tracking sweep closes it) and more
// closed tracking beads than the retention floor keeps, all past the TTL (the
// retention watchdog prunes them).
func storeWatchdogSeed(now time.Time) []beads.Bead {
	seed := []beads.Bead{{
		ID:        "stale-open",
		Title:     "order:stale",
		Status:    "open",
		Type:      "task",
		CreatedAt: now.Add(-time.Hour),
		Labels:    []string{"order-run:stale", labelOrderTracking},
	}}
	for i := range minClosedOrderTrackingRetained + 2 {
		seed = append(seed, beads.Bead{
			ID:        fmt.Sprintf("prune-%02d", i),
			Title:     "order:prune",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-8*24*time.Hour + time.Duration(i)*time.Minute),
			Labels:    []string{"order-run:prune", labelOrderTracking},
			Ephemeral: true,
		})
	}
	return seed
}

// assertStoreWatchdogSeedUntouched fails if any watchdog mutated the seed.
func assertStoreWatchdogSeedUntouched(t *testing.T, store beads.Store) {
	t.Helper()
	stale, err := store.Get("stale-open")
	if err != nil {
		t.Fatalf("get stale-open: %v", err)
	}
	if stale.Status != "open" {
		t.Fatalf("stale-open status = %q, want open: the tracking sweep ran", stale.Status)
	}
	closed, err := store.List(beads.ListQuery{Status: "closed", Label: labelOrderTracking, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("list closed tracking: %v", err)
	}
	if got, want := len(closed), minClosedOrderTrackingRetained+2; got != want {
		t.Fatalf("closed tracking beads = %d, want %d: the retention watchdog pruned", got, want)
	}
}

// assertStoreWatchdogClocksZero fails if any watchdog clock advanced.
func assertStoreWatchdogClocksZero(t *testing.T, cr *CityRuntime) {
	t.Helper()
	if !cr.orderSweepWatchdogLast.IsZero() {
		t.Fatalf("orderSweepWatchdogLast = %v, want zero", cr.orderSweepWatchdogLast)
	}
	if !cr.orderSweepWatchdogNotClosedDue.IsZero() {
		t.Fatalf("orderSweepWatchdogNotClosedDue = %v, want zero", cr.orderSweepWatchdogNotClosedDue)
	}
	if !cr.orderTrackingRetentionWatchdogLast.IsZero() {
		t.Fatalf("orderTrackingRetentionWatchdogLast = %v, want zero", cr.orderTrackingRetentionWatchdogLast)
	}
	if !cr.nudgeMailSweepWatchdogLast.IsZero() {
		t.Fatalf("nudgeMailSweepWatchdogLast = %v, want zero", cr.nudgeMailSweepWatchdogLast)
	}
}

// Order dispatch runs no store watchdog: with every watchdog due and work for
// each in the store, the startup dispatch and an orders-lane pass both reach
// the dispatcher without invoking a watchdog, advancing its clock or touching
// the records it would mutate. The watchdogs run on their own lanes, which
// this test never starts.
func TestOrdersPassRunsNoStoreWatchdogs(t *testing.T) {
	now := time.Now()
	store := beads.NewMemStoreFrom(100, storeWatchdogSeed(now), nil)
	od := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, od, "1h", nil)
	cr.standaloneCityStore = store
	cr.wispIndexMigrationApplied = true
	cr.orderRescanLast = now
	cr.managedDoltOwned = func(string) (bool, error) { return false, nil }

	cr.dispatchOrders(context.Background(), cr.cityPath)
	cr.runOrdersLanePass(context.Background(), cr.cityPath, ordersLaneReasonCadence)

	if got := od.calls.Load(); got != 2 {
		t.Fatalf("dispatcher calls = %d, want 2 (startup dispatch and one lane pass)", got)
	}
	assertStoreWatchdogClocksZero(t, cr)
	assertStoreWatchdogSeedUntouched(t, store)
}

// closedHistoryBlockingStore parks every closed-status List (the retention
// prune's history read) until release is closed.
type closedHistoryBlockingStore struct {
	beads.Store
	entered     chan struct{}
	enteredOnce sync.Once
	release     chan struct{}
}

func newClosedHistoryBlockingStore(inner beads.Store) *closedHistoryBlockingStore {
	return &closedHistoryBlockingStore{Store: inner, entered: make(chan struct{}), release: make(chan struct{})}
}

func (s *closedHistoryBlockingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Status == "closed" {
		s.enteredOnce.Do(func() { close(s.entered) })
		<-s.release
	}
	return s.Store.List(q)
}

// storeWatchdogTestRuntime is ordersLaneTestRuntime over store, with every
// watchdog due and the order rescan, index migration and managed-Dolt
// preflight out of the way. Its empty city path skips the retention
// watchdog's backup-age check, as on every directly-built test runtime.
func storeWatchdogTestRuntime(t *testing.T, store beads.Store, od orderDispatcher, patrol string) *CityRuntime {
	t.Helper()
	cr := ordersLaneTestRuntime(t, od, patrol, nil)
	cr.cityPath = ""
	cr.standaloneCityStore = store
	cr.wispIndexMigrationApplied = true
	cr.orderRescanLast = time.Now()
	cr.managedDoltOwned = func(string) (bool, error) { return false, nil }
	return cr
}

// A retention prune blocked in its closed-history read holds nothing else:
// the startup dispatch, an orders-lane pass and the tracking and nudge/mail
// lanes each finish their work while it stays parked.
func TestStoreWatchdogLanesRunWhileRetentionIsBlocked(t *testing.T) {
	now := time.Now()
	seed := append(storeWatchdogSeed(now), mailSeed("mail-stale", now.Add(-nudgeMailSweepDefaultMailTTL-time.Minute)))
	store := newClosedHistoryBlockingStore(beads.NewMemStoreFrom(100, seed, nil))
	od := &recordingOrderDispatcher{}
	cr := storeWatchdogTestRuntime(t, store, od, "1h")
	ctx := context.Background()

	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)
		cr.runStoreWatchdogLanePass(ctx, cr.storeWatchdogLaneOf(storeWatchdogOrderTrackingRetention))
	}()
	t.Cleanup(func() {
		close(store.release)
		awaitClose(t, retentionDone, "the retention pass finishing after release")
	})
	awaitClose(t, store.entered, "the retention watchdog reaching its closed-history read")

	othersDone := make(chan struct{})
	go func() {
		defer close(othersDone)
		cr.dispatchOrders(ctx, cr.cityPath)
		cr.runOrdersLanePass(ctx, cr.cityPath, ordersLaneReasonCadence)
		cr.runStoreWatchdogLanePass(ctx, cr.storeWatchdogLaneOf(storeWatchdogOrderTrackingSweep))
		cr.runStoreWatchdogLanePass(ctx, cr.storeWatchdogLaneOf(storeWatchdogNudgeMailSweep))
	}()
	awaitClose(t, othersDone, "dispatch and the tracking and nudge/mail lanes while retention is blocked")

	select {
	case <-retentionDone:
		t.Fatal("the retention pass finished before release: the test did not hold it")
	default:
	}
	if got := od.calls.Load(); got != 2 {
		t.Fatalf("dispatcher calls = %d, want 2 (startup dispatch and one lane pass)", got)
	}
	for id, want := range map[string]string{"stale-open": "closed", "mail-stale": "closed"} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != want {
			t.Fatalf("%s status = %q, want %q while retention is blocked", id, got.Status, want)
		}
	}
}

// watchdogOpCountingStore counts every read and write a watchdog reaches it with.
type watchdogOpCountingStore struct {
	beads.Store
	ops atomic.Int64
}

func (s *watchdogOpCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.ops.Add(1)
	return s.Store.List(q)
}

func (s *watchdogOpCountingStore) Close(id string) error {
	s.ops.Add(1)
	return s.Store.Close(id)
}

// The store watchdog lanes start right after the orders lane, ahead of the
// cold-start reconcile, and run without waiting for it: with the reconcile
// parked in its desired-state build, every watchdog's work lands within a few
// patrol intervals of startup.
func TestStoreWatchdogLanesRunDuringColdStartReconcile(t *testing.T) {
	t.Setenv(fsPressureThresholdEnv, "100")
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Daemon.PatrolInterval = "10ms"
	sp := runtime.NewFake()
	reconcileEntered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	releaseStartup := func() { releaseOnce.Do(func() { close(release) }) }
	cr, runtimeErr := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			enterOnce.Do(func() { close(reconcileEntered) })
			<-release
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if runtimeErr != nil {
		t.Fatalf("building the city runtime: %v", runtimeErr)
	}
	cr.od = &recordingOrderDispatcher{}
	now := time.Now()
	seed := append(storeWatchdogSeed(now), mailSeed("mail-stale", now.Add(-nudgeMailSweepDefaultMailTTL-time.Minute)))
	store := beads.NewMemStoreFrom(100, seed, nil)
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = store
	cr.setControllerState(cs)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		cr.run(ctx)
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		releaseStartup()
		cancel()
		awaitClose(t, runDone, "run() returning after cancellation")
	}
	t.Cleanup(stop)
	awaitClose(t, reconcileEntered, "the cold-start reconcile reaching its desired-state build")

	deadline := time.Now().Add(hangBudget)
	for {
		mail, mailErr := store.Get("mail-stale")
		closed, listErr := store.List(beads.ListQuery{Status: "closed", Label: "order-run:prune", TierMode: beads.TierBoth})
		if mailErr != nil || listErr != nil {
			t.Fatalf("reading the store: %v", errors.Join(mailErr, listErr))
		}
		if mail.Status == "closed" && len(closed) <= minClosedOrderTrackingRetained {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("with the cold-start reconcile parked: mail-stale %q, %d closed prunable tracking beads; want the nudge/mail and retention lanes to have run", mail.Status, len(closed))
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-release:
		t.Fatal("the cold-start reconcile was released before the lanes ran")
	default:
	}

	// run() joins the lanes before it returns, so their clocks are readable.
	stop()
	if cr.orderSweepWatchdogLast.IsZero() || cr.orderTrackingRetentionWatchdogLast.IsZero() || cr.nudgeMailSweepWatchdogLast.IsZero() {
		t.Fatalf("watchdog clocks after run: sweep=%v retention=%v nudge/mail=%v, want every lane to have run",
			cr.orderSweepWatchdogLast, cr.orderTrackingRetentionWatchdogLast, cr.nudgeMailSweepWatchdogLast)
	}
}

// A pass that has just run leaves its watchdog not due: the patrol interval
// paces attempts, and each watchdog keeps its own interval, so a second pass
// right after the first reaches no store.
func TestStoreWatchdogLanePassKeepsWatchdogIntervals(t *testing.T) {
	now := time.Now()
	store := &watchdogOpCountingStore{Store: beads.NewMemStoreFrom(100, storeWatchdogSeed(now), nil)}
	cr := storeWatchdogTestRuntime(t, store, &recordingOrderDispatcher{}, "1h")
	for _, lane := range cr.storeWatchdogLanesOf() {
		cr.runStoreWatchdogLanePass(context.Background(), lane)
	}
	if store.ops.Load() == 0 {
		t.Fatal("the first passes reached no store: the watchdogs were not due")
	}
	before := store.ops.Load()
	for _, lane := range cr.storeWatchdogLanesOf() {
		cr.runStoreWatchdogLanePass(context.Background(), lane)
	}
	if got := store.ops.Load() - before; got != 0 {
		t.Fatalf("second passes made %d store calls, want 0: a watchdog ran before its interval", got)
	}
	later := time.Now()
	if !cr.orderTrackingSweepWatchdogDue(later.Add(orderTrackingSweepWatchdogInterval)) ||
		!cr.orderTrackingRetentionWatchdogDue(later.Add(orderTrackingRetentionWatchdogInterval)) ||
		!cr.nudgeMailSweepWatchdogDue(later.Add(nudgeMailSweepWatchdogInterval)) {
		t.Fatal("a watchdog is not due one interval after its pass")
	}
	if cr.orderTrackingRetentionWatchdogDue(later.Add(orderTrackingRetentionWatchdogInterval/2)) ||
		cr.nudgeMailSweepWatchdogDue(later.Add(nudgeMailSweepWatchdogInterval/2)) {
		t.Fatal("a watchdog is due half an interval after its pass")
	}
}

// A suspended city's lanes reach no store, even with every watchdog due, and
// leave every clock where it was; once the city resumes, the next pass runs.
func TestStoreWatchdogLanesSkipSuspendedCity(t *testing.T) {
	now := time.Now()
	store := &watchdogOpCountingStore{Store: beads.NewMemStoreFrom(100, storeWatchdogSeed(now), nil)}
	cr := storeWatchdogTestRuntime(t, store, &recordingOrderDispatcher{}, "1h")
	suspended := *cr.cfg
	suspended.Workspace.Suspended = true
	cr.cfg = &suspended

	for _, lane := range cr.storeWatchdogLanesOf() {
		cr.runStoreWatchdogLanePass(context.Background(), lane)
	}
	if got := store.ops.Load(); got != 0 {
		t.Fatalf("suspended passes made %d store calls, want 0", got)
	}
	assertStoreWatchdogClocksZero(t, cr)
	assertStoreWatchdogSeedUntouched(t, store)

	resumed := suspended
	resumed.Workspace.Suspended = false
	cr.serviceStateMu.Lock()
	cr.cfg = &resumed
	cr.serviceStateMu.Unlock()
	for _, lane := range cr.storeWatchdogLanesOf() {
		cr.runStoreWatchdogLanePass(context.Background(), lane)
	}
	if store.ops.Load() == 0 {
		t.Fatal("no pass reached a store after the city resumed")
	}
	if cr.orderSweepWatchdogLast.IsZero() || cr.orderTrackingRetentionWatchdogLast.IsZero() || cr.nudgeMailSweepWatchdogLast.IsZero() {
		t.Fatalf("watchdog clocks after resume: sweep=%v retention=%v nudge/mail=%v, want every lane to have run",
			cr.orderSweepWatchdogLast, cr.orderTrackingRetentionWatchdogLast, cr.nudgeMailSweepWatchdogLast)
	}
}
