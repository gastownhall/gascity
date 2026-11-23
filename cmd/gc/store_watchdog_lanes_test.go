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
func storeWatchdogTestRuntime(t *testing.T, store beads.Store, od orderDispatcher) *CityRuntime {
	t.Helper()
	cr := ordersLaneTestRuntime(t, od, "1h", nil)
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
	cr := storeWatchdogTestRuntime(t, store, od)
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
	cr := storeWatchdogTestRuntime(t, store, &recordingOrderDispatcher{})
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

// Lane passes and reload publications are concurrent by design: a reload
// publishes its config under serviceStateMu while every lane pass reads its
// own snapshot, so a pass never sees a torn config and a reload never waits
// on a watchdog. Passes on every lane and publications flipping the city's
// suspension hammering at once must finish (and stay race-free), and every
// lane's last status must stay one of the defined values.
func TestStoreWatchdogLanePassesSurviveConcurrentConfigReloads(t *testing.T) {
	now := time.Now()
	store := beads.NewMemStoreFrom(100, storeWatchdogSeed(now), nil)
	cr := storeWatchdogTestRuntime(t, store, &recordingOrderDispatcher{})

	const publications = 16
	const passesPerLane = 16
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range publications {
			cfg := &config.City{Workspace: config.Workspace{Name: "test-city", Suspended: i%2 == 1}}
			if !cr.publishRuntimeConfig(cfg, cr.sp, cr.dops, fmt.Sprintf("rev-%d", i)) {
				t.Errorf("publication %d was rejected", i)
				return
			}
		}
	}()
	for _, lane := range cr.storeWatchdogLanesOf() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range passesPerLane {
				cr.runStoreWatchdogLanePass(context.Background(), lane)
			}
		}()
	}
	wg.Wait()

	valid := map[storeWatchdogPassStatus]bool{
		storeWatchdogPassNotDue:          true,
		storeWatchdogPassSuspended:       true,
		storeWatchdogPassPressureSkipped: true,
		storeWatchdogPassCanceled:        true,
		storeWatchdogPassAborted:         true,
		storeWatchdogPassCompleted:       true,
	}
	for _, lane := range cr.storeWatchdogLanesOf() {
		if got := lane.status().last; !valid[got] {
			t.Fatalf("lane %q status after concurrent passes = %q, want one of the defined statuses", lane.spec.key, got)
		}
	}
}

// A suspended city's lanes reach no store, even with every watchdog due, and
// leave every clock where it was; once the city resumes, the next pass runs.
func TestStoreWatchdogLanesSkipSuspendedCity(t *testing.T) {
	now := time.Now()
	store := &watchdogOpCountingStore{Store: beads.NewMemStoreFrom(100, storeWatchdogSeed(now), nil)}
	cr := storeWatchdogTestRuntime(t, store, &recordingOrderDispatcher{})
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

// Stopping the lanes does not wait on one blocked in a store call: stop
// cancels every lane first, so the others exit while the retention lane is
// still parked in its closed-history read, and stop returns only once that
// call finishes and its lane exits too.
func TestStoreWatchdogLaneSetStopDoesNotWaitOnABlockedLane(t *testing.T) {
	now := time.Now()
	store := newClosedHistoryBlockingStore(beads.NewMemStoreFrom(100, storeWatchdogSeed(now), nil))
	cr := storeWatchdogTestRuntime(t, store, &recordingOrderDispatcher{})
	cr.cfg.Daemon.PatrolInterval = "10ms"
	set := cr.startStoreWatchdogLanes(context.Background())
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(store.release) }) }
	t.Cleanup(func() {
		release()
		set.stop()
	})
	awaitClose(t, store.entered, "the retention lane reaching its closed-history read")

	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		set.stop()
	}()
	for i, lane := range cr.storeWatchdogLanesOf() {
		if lane.spec.key == storeWatchdogOrderTrackingRetention {
			continue
		}
		awaitClose(t, set.done[i], "the "+lane.spec.work+" lane exiting while retention is blocked")
	}
	select {
	case <-stopDone:
		t.Fatal("stop returned while the retention lane was still in its store call")
	default:
	}
	release()
	awaitClose(t, stopDone, "stop returning once the retention call finished")
}

// Cancellation reaches the nudge/mail watchdog's queue maintenance, and no
// mail sweep follows it: a lane stopped mid-maintenance closes no mail and
// logs no cancellation as a failure.
func TestNudgeMailSweepLaneSweepsNothingAfterCanceledMaintenance(t *testing.T) {
	now := time.Now()
	seed := []beads.Bead{mailSeed("mail-stale", now.Add(-nudgeMailSweepDefaultMailTTL-time.Minute))}
	store := beads.NewMemStoreFrom(100, seed, nil)
	cr := storeWatchdogTestRuntime(t, store, &recordingOrderDispatcher{})
	var stderr bytes.Buffer
	cr.stderr = &stderr
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	maintenanceSawCancel := false
	cr.maintainNudgeQueue = func(mctx context.Context, _ beads.NudgesStore, _ time.Time) error {
		cancel()
		select {
		case <-mctx.Done():
			maintenanceSawCancel = true
			return mctx.Err()
		case <-time.After(hangBudget):
			return errors.New("the lane's cancellation never reached queue maintenance")
		}
	}

	cr.runStoreWatchdogLanePass(ctx, cr.storeWatchdogLaneOf(storeWatchdogNudgeMailSweep))
	if !maintenanceSawCancel {
		t.Fatalf("queue maintenance did not observe the lane's cancellation; stderr:\n%s", stderr.String())
	}
	mail, err := store.Get("mail-stale")
	if err != nil {
		t.Fatalf("get mail-stale: %v", err)
	}
	if mail.Status != "open" {
		t.Fatalf("mail-stale status = %q, want open: a mail sweep followed the cancellation", mail.Status)
	}
	if out := stderr.String(); strings.Contains(out, "context canceled") {
		t.Fatalf("stderr logs the cancellation as a failure:\n%s", out)
	}
}

// The shutdown story end to end: a nudge/mail lane pass parked inside queue
// maintenance when run()'s context is canceled finishes its parked
// maintenance, sweeps nothing after it, and run() returns only once the
// join has waited for that pass.
func TestRunShutdownAbortsNudgeMailSweepMidMaintenance(t *testing.T) {
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
	store := beads.NewMemStoreFrom(100, []beads.Bead{mailSeed("mail-stale", now.Add(-nudgeMailSweepDefaultMailTTL-time.Minute))}, nil)
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = store
	cr.setControllerState(cs)

	maintainEntered := make(chan struct{})
	releaseMaintain := make(chan struct{})
	var maintainOnce, releaseMaintainOnce sync.Once
	unparkMaintain := func() { releaseMaintainOnce.Do(func() { close(releaseMaintain) }) }
	var maintainCalls atomic.Int64
	cr.maintainNudgeQueue = func(context.Context, beads.NudgesStore, time.Time) error {
		maintainCalls.Add(1)
		maintainOnce.Do(func() { close(maintainEntered) })
		// The parked call ignores its context, like a synchronous store
		// call: it returns only when released.
		<-releaseMaintain
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		cr.run(ctx)
	}()
	t.Cleanup(func() {
		unparkMaintain()
		releaseStartup()
		cancel()
		<-runDone
	})
	awaitClose(t, reconcileEntered, "the cold-start reconcile reaching its desired-state build")
	awaitClose(t, maintainEntered, "the nudge/mail lane reaching its queue maintenance")

	// Shutdown lands while the pass is parked inside the maintenance.
	cancel()
	releaseStartup()
	select {
	case <-runDone:
		t.Fatal("run() returned while the nudge/mail lane pass was still parked in queue maintenance")
	case <-time.After(50 * time.Millisecond):
	}
	unparkMaintain()
	awaitClose(t, runDone, "run() returning after shutdown (joining the parked nudge/mail lane pass)")

	if got := maintainCalls.Load(); got != 1 {
		t.Fatalf("queue maintenance calls = %d, want 1: the parked pass must not be followed by another", got)
	}
	if got := statusOf(t, store, "mail-stale"); got != "open" {
		t.Fatalf("stale mail bead status after shutdown = %q, want open: the canceled pass must not sweep", got)
	}
}

// slowClosedHistoryStore makes every closed-status List (the retention
// prune's history read) take at least delay.
type slowClosedHistoryStore struct {
	beads.Store
	delay time.Duration
}

func (s *slowClosedHistoryStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Status == "closed" {
		time.Sleep(s.delay)
	}
	return s.Store.List(q)
}

// storeWatchdogTraceRecords closes cr's tracer and returns the store watchdog
// cycles' operation and result records, by the watchdog each cycle's trigger
// detail names. It fails on any orders cycle: no test that calls it runs an
// orders pass.
func storeWatchdogTraceRecords(t *testing.T, cr *CityRuntime, traceRoot string) (ops, results map[storeWatchdogKey][]SessionReconcilerTraceRecord) {
	t.Helper()
	if err := cr.trace.Close(); err != nil {
		t.Fatalf("closing the tracer: %v", err)
	}
	records, err := ReadTraceRecords(traceCityRuntimeDir(traceRoot), TraceFilter{})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	// Only a cycle's start record carries its trigger; the rest share its
	// tick ID.
	cycles := map[string]storeWatchdogKey{}
	for _, r := range records {
		if r.RecordType != TraceRecordCycleStart {
			continue
		}
		if r.TickTrigger == ordersLaneTraceTrigger {
			t.Fatalf("an orders cycle in a watchdog-only run: %+v", r)
		}
		if r.TickTrigger == storeWatchdogLaneTraceTrigger {
			cycles[r.TickID] = storeWatchdogKey(r.TriggerDetail)
		}
	}
	ops = map[storeWatchdogKey][]SessionReconcilerTraceRecord{}
	results = map[storeWatchdogKey][]SessionReconcilerTraceRecord{}
	for _, r := range records {
		key, ok := cycles[r.TickID]
		if !ok {
			continue
		}
		switch r.RecordType {
		case TraceRecordOperation:
			ops[key] = append(ops[key], r)
		case TraceRecordCycleResult:
			results[key] = append(results[key], r)
		}
	}
	return ops, results
}

// Each due pass is a trace cycle of its own, triggered store_watchdog and
// detailed with its watchdog: its operation record names the watchdog and
// carries the work's duration, and the cycle ends completed. A pass that is
// not due writes no trace at all.
func TestStoreWatchdogLanePassesTraceTheirOwnCycles(t *testing.T) {
	now := time.Now()
	const slow = 30 * time.Millisecond
	store := &slowClosedHistoryStore{Store: beads.NewMemStoreFrom(100, storeWatchdogSeed(now), nil), delay: slow}
	cr := storeWatchdogTestRuntime(t, store, &recordingOrderDispatcher{})
	traceRoot := t.TempDir()
	cr.trace = newSessionReconcilerTraceManager(traceRoot, "test-city", io.Discard)
	lanes := cr.storeWatchdogLanesOf()
	for range 2 {
		for _, lane := range lanes {
			cr.runStoreWatchdogLanePass(context.Background(), lane)
		}
	}

	ops, results := storeWatchdogTraceRecords(t, cr, traceRoot)
	for _, lane := range lanes {
		key := lane.spec.key
		if len(ops[key]) != 1 || len(results[key]) != 1 {
			t.Fatalf("%s: %d operation and %d result records, want one of each for its one due pass", key, len(ops[key]), len(results[key]))
		}
		op := ops[key][0]
		if op.Fields["operation_name"] != string(key) || op.Fields["watchdog"] != string(key) || op.Fields["status"] != string(storeWatchdogPassCompleted) {
			t.Fatalf("%s operation record fields = %v, want the watchdog named and status completed", key, op.Fields)
		}
		res := results[key][0]
		if res.CompletionStatus != TraceCompletionCompleted || res.Fields["watchdog"] != string(key) || res.Fields["status"] != string(storeWatchdogPassCompleted) {
			t.Fatalf("%s cycle result = %s %v, want completed with the watchdog and its status", key, res.CompletionStatus, res.Fields)
		}
	}
	if d := ops[storeWatchdogOrderTrackingRetention][0].DurationMS; d < slow.Milliseconds() {
		t.Fatalf("retention operation duration = %dms, want at least the %dms its history read took", d, slow.Milliseconds())
	}
}

// Only a pass whose work finished uncanceled refreshes a lane's completed-pass
// time. A suspended pass, a pass canceled mid-work and a pass still blocked
// in its store call leave it where it was; the tick reports each lane's age
// and last status on its own, and a canceled pass's cycle ends aborted.
func TestStoreWatchdogLaneCompletedPassIgnoresUnfinishedWork(t *testing.T) {
	now := time.Now()
	seed := append(storeWatchdogSeed(now), mailSeed("mail-stale", now.Add(-nudgeMailSweepDefaultMailTTL-time.Minute)))
	store := newClosedHistoryBlockingStore(beads.NewMemStoreFrom(100, seed, nil))
	cr := storeWatchdogTestRuntime(t, store, &recordingOrderDispatcher{})
	traceRoot := t.TempDir()
	cr.trace = newSessionReconcilerTraceManager(traceRoot, "test-city", io.Discard)
	sweep := cr.storeWatchdogLaneOf(storeWatchdogOrderTrackingSweep)
	retention := cr.storeWatchdogLaneOf(storeWatchdogOrderTrackingRetention)
	nudge := cr.storeWatchdogLaneOf(storeWatchdogNudgeMailSweep)
	assertStatus := func(lane *storeWatchdogLane, want storeWatchdogPassStatus, wantCompleted bool) {
		t.Helper()
		st := lane.status()
		if st.last != want || st.completed != wantCompleted {
			t.Fatalf("%s status = %q completed=%v, want %q completed=%v", lane.spec.key, st.last, st.completed, want, wantCompleted)
		}
	}

	live := cr.cfg
	suspended := *live
	suspended.Workspace.Suspended = true
	cr.cfg = &suspended
	cr.runStoreWatchdogLanePass(context.Background(), nudge)
	assertStatus(nudge, storeWatchdogPassSuspended, false)
	cr.serviceStateMu.Lock()
	cr.cfg = live
	cr.serviceStateMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cr.maintainNudgeQueue = func(mctx context.Context, _ beads.NudgesStore, _ time.Time) error {
		cancel()
		return mctx.Err()
	}
	cr.runStoreWatchdogLanePass(ctx, nudge)
	assertStatus(nudge, storeWatchdogPassCanceled, false)

	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)
		cr.runStoreWatchdogLanePass(context.Background(), retention)
	}()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(store.release) }) }
	t.Cleanup(func() {
		release()
		awaitClose(t, retentionDone, "the retention pass finishing after release")
	})
	awaitClose(t, store.entered, "the retention watchdog reaching its closed-history read")
	if retention.status().completed {
		t.Fatal("a retention pass still blocked in its store call counts as completed")
	}
	release()
	awaitClose(t, retentionDone, "the retention pass finishing after release")
	assertStatus(retention, storeWatchdogPassCompleted, true)

	cr.runStoreWatchdogLanePass(context.Background(), sweep)
	completedAt := sweep.status().completedAt
	cr.runStoreWatchdogLanePass(context.Background(), sweep)
	assertStatus(sweep, storeWatchdogPassNotDue, true)
	if got := sweep.status().completedAt; !got.Equal(completedAt) {
		t.Fatalf("a not-due pass moved the completed-pass time from %v to %v", completedAt, got)
	}

	fields := cr.storeWatchdogLaneTickFields()
	for k, want := range map[string]any{
		"order_tracking_sweep_backstop_ran":     true,
		"order_tracking_sweep_last_status":      string(storeWatchdogPassNotDue),
		"order_tracking_retention_backstop_ran": true,
		"order_tracking_retention_last_status":  string(storeWatchdogPassCompleted),
		"nudge_mail_sweep_backstop_ran":         false,
		"nudge_mail_sweep_last_status":          string(storeWatchdogPassCanceled),
	} {
		if fields[k] != want {
			t.Fatalf("tick field %s = %v, want %v; fields: %v", k, fields[k], want, fields)
		}
	}
	if _, ok := fields["nudge_mail_sweep_backstop_age_seconds"]; ok {
		t.Fatalf("a lane with no completed pass reports an age: %v", fields)
	}
	if _, ok := fields["order_tracking_retention_backstop_age_seconds"]; !ok {
		t.Fatalf("a lane with a completed pass reports no age: %v", fields)
	}

	_, results := storeWatchdogTraceRecords(t, cr, traceRoot)
	if got := results[storeWatchdogNudgeMailSweep]; len(got) != 1 || got[0].CompletionStatus != TraceCompletionAborted || got[0].Fields["status"] != string(storeWatchdogPassCanceled) {
		t.Fatalf("nudge/mail cycle results = %+v, want one aborted canceled cycle and none for the suspended pass", got)
	}
}
