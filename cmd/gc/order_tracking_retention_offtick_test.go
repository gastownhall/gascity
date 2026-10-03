package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
)

// blockingDeleteStore is a MemStore whose Delete waits for release, standing
// in for a hub store where each graph delete takes seconds.
type blockingDeleteStore struct {
	*beads.MemStore
	deleteStarted chan string
	release       chan struct{}
}

func (s *blockingDeleteStore) Delete(id string) error {
	select {
	case s.deleteStarted <- id:
	default:
	}
	<-s.release
	return s.MemStore.Delete(id)
}

// newRetentionBacklogRuntime returns a runtime over a store holding
// minClosedOrderTrackingRetained+extra closed tracking beads for one order,
// all past the default 7d TTL, so extra of them are eligible for pruning.
func newRetentionBacklogRuntime(now time.Time, extra int) (*CityRuntime, *blockingDeleteStore) {
	total := minClosedOrderTrackingRetained + extra
	store := &blockingDeleteStore{
		MemStore:      beads.NewMemStoreFrom(total, retentionBacklogSeed(now, total), nil),
		deleteStarted: make(chan string, 1),
		release:       make(chan struct{}),
	}
	cr := &CityRuntime{
		cityName:                  "test-city",
		cfg:                       &config.City{Workspace: config.Workspace{Name: "test-city"}},
		standaloneCityStore:       store,
		stdout:                    io.Discard,
		stderr:                    io.Discard,
		logPrefix:                 "gc test",
		wispIndexMigrationApplied: true,
	}
	return cr, store
}

// retentionBacklogSeed returns total closed tracking beads for one order, all
// past the default 7d TTL; backlog-0000 is the oldest.
func retentionBacklogSeed(now time.Time, total int) []beads.Bead {
	seed := make([]beads.Bead, 0, total)
	for i := range total {
		seed = append(seed, beads.Bead{
			ID:        fmt.Sprintf("backlog-%04d", i),
			Title:     "order:backlog",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-30*24*time.Hour + time.Duration(i)*time.Minute),
			Labels:    []string{"order-run:backlog", labelOrderTracking},
			Ephemeral: true,
		})
	}
	return seed
}

// The startup order pass (and every orders-lane pass) must not wait on the
// retention prune: with a large closed backlog and slow deletes it would hold
// the pass — and the cold start behind it — for budget × per-delete latency.
func TestOrderTrackingRetentionWatchdog_DoesNotBlockOrderPass(t *testing.T) {
	now := time.Now()
	cr, store := newRetentionBacklogRuntime(now, 500)
	releaseOnce := func() {
		select {
		case <-store.release:
		default:
			close(store.release)
		}
	}
	defer releaseOnce()

	passDone := make(chan struct{})
	go func() {
		defer close(passDone)
		cr.dispatchOrders(context.Background(), "")
	}()

	// Nothing releases the Delete until the pass has returned, so a pass that
	// waits on the prune never returns: the hang budget is a wedge detector, not
	// a latency bound.
	awaitClose(t, passDone, "the order pass while the retention sweep's Delete is held")
	if !cr.orderTrackingRetentionSweepInFlight() {
		t.Fatalf("the order pass returned with no retention sweep in flight: the prune did not run off the pass")
	}

	select {
	case <-store.deleteStarted:
	case <-time.After(hangBudget):
		t.Fatalf("retention sweep never reached Delete within the hang budget (%s): the prune did not run at all", hangBudget)
	}
	releaseOnce()
	cr.waitOrderTrackingRetentionSweep()
}

// runRetentionWatchdogAndWait runs one watchdog check and waits for any sweep
// it started, for tests that assert on the sweep's effects.
func runRetentionWatchdogAndWait(cr *CityRuntime, now time.Time) {
	cr.runOrderTrackingRetentionWatchdog(context.Background(), cr.cfg, now)
	cr.waitOrderTrackingRetentionSweep()
}

func countDeletedBacklog(t *testing.T, store *blockingDeleteStore, total int) int {
	t.Helper()
	gone := 0
	for i := range total {
		if _, err := store.Get(fmt.Sprintf("backlog-%04d", i)); err != nil {
			gone++
		}
	}
	return gone
}

func TestOrderTrackingRetentionWatchdog_SingleFlight(t *testing.T) {
	now := time.Now()
	const extra = 500
	cr, store := newRetentionBacklogRuntime(now, extra)

	cr.runOrderTrackingRetentionWatchdog(context.Background(), cr.cfg, now)
	select {
	case <-store.deleteStarted:
	case <-time.After(5 * time.Second):
		t.Fatalf("first sweep never reached Delete")
	}

	// A pass that is due while the first sweep is still deleting must neither
	// start a second sweep nor consume the interval.
	later := now.Add(orderTrackingRetentionWatchdogInterval + time.Minute)
	cr.runOrderTrackingRetentionWatchdog(context.Background(), cr.cfg, later)
	if !cr.orderTrackingRetentionWatchdogLast.Equal(now) {
		t.Fatalf("orderTrackingRetentionWatchdogLast = %v, want %v (no second sweep while one is in flight)", cr.orderTrackingRetentionWatchdogLast, now)
	}

	close(store.release)
	cr.waitOrderTrackingRetentionSweep()
	if got := countDeletedBacklog(t, store, minClosedOrderTrackingRetained+extra); got != orderTrackingRetentionWatchdogDeleteBudget {
		t.Fatalf("deleted %d beads, want exactly one budget (%d): a second sweep ran", got, orderTrackingRetentionWatchdogDeleteBudget)
	}

	// Once the first sweep has finished, the next due pass starts a new one.
	cr.runOrderTrackingRetentionWatchdog(context.Background(), cr.cfg, later)
	cr.waitOrderTrackingRetentionSweep()
	if !cr.orderTrackingRetentionWatchdogLast.Equal(later) {
		t.Fatalf("orderTrackingRetentionWatchdogLast = %v, want %v after the first sweep finished", cr.orderTrackingRetentionWatchdogLast, later)
	}
}

func TestOrderTrackingRetentionWatchdog_StopCancelsAndJoinsInFlightSweep(t *testing.T) {
	now := time.Now()
	const extra = 500
	cr, store := newRetentionBacklogRuntime(now, extra)

	// The runtime's context stays live: only stop may cancel the sweep.
	cr.runOrderTrackingRetentionWatchdog(context.Background(), cr.cfg, now)
	select {
	case <-store.deleteStarted:
	case <-time.After(5 * time.Second):
		t.Fatalf("sweep never reached Delete")
	}

	canceled := observeRetentionSweepCancel(cr)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		cr.stopOrderTrackingRetentionSweep()
	}()
	// The Delete stays held until stop is provably parked in its join. A stop
	// that does not join returns instead, and nothing else ends this wait.
	var returnedEarly bool
	awaitCond(t, func() bool {
		select {
		case <-stopped:
			returnedEarly = true
			return true
		default:
		}
		return retentionSweepJoinParkedIn("(*CityRuntime).stopOrderTrackingRetentionSweep")
	}, "stopOrderTrackingRetentionSweep parking in the sweep's join")
	if returnedEarly {
		close(store.release)
		t.Fatalf("stopOrderTrackingRetentionSweep returned while the sweep still held a Delete")
	}
	// stop cancels before it joins, so parked in the join means canceled.
	select {
	case <-canceled:
	default:
		close(store.release)
		t.Fatalf("stopOrderTrackingRetentionSweep joined the sweep without canceling it")
	}

	close(store.release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatalf("stopOrderTrackingRetentionSweep did not return after the held Delete finished")
	}
	// The sweep saw the cancellation: it stopped after the delete in progress
	// instead of spending its budget.
	if got := countDeletedBacklog(t, store, minClosedOrderTrackingRetained+extra); got != 1 {
		t.Fatalf("deleted %d beads, want 1 (only the delete in progress when stop canceled)", got)
	}
	if cr.orderTrackingRetentionSweepInFlight() {
		t.Fatalf("sweep still in flight after stop")
	}
}

// On a split city the closed tracking beads live in the orders binding; the
// sweep must prune there, through the binding handle routes own.
func TestOrderTrackingRetentionWatchdog_PrunesTheOrdersBinding(t *testing.T) {
	now := time.Now()
	const extra = 3
	_, binding := newRetentionBacklogRuntime(now, extra)
	close(binding.release)
	cr := &CityRuntime{
		cityName:                  "test-city",
		cfg:                       &config.City{Workspace: config.Workspace{Name: "test-city"}},
		storageRoutes:             messagingSplitRoutes(binding),
		standaloneCityStore:       beads.NewMemStore(),
		standaloneRigStores:       map[string]beads.Store{},
		stdout:                    io.Discard,
		stderr:                    io.Discard,
		logPrefix:                 "gc test",
		wispIndexMigrationApplied: true,
	}
	if got := cr.relocatedOrdersStore(cr.cfg); got != beads.Store(binding) {
		t.Fatalf("relocatedOrdersStore = %T, want the binding", got)
	}

	runRetentionWatchdogAndWait(cr, now)

	if got := countDeletedBacklog(t, binding, minClosedOrderTrackingRetained+extra); got != extra {
		t.Fatalf("deleted %d beads from the orders binding, want %d", got, extra)
	}
}

// run() can return straight after the startup order pass started a sweep —
// here the pass itself cancels the city context while a delete is held. The
// exit must reach shutdown()'s join, and neither return nor close the storage
// binding the sweep may be deleting through, until the held Delete finishes.
func TestCityRuntimeRun_StartupExitClosesBindingOnlyAfterSweepJoin(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	// A fresh recovery point, so the backup-age guard lets the sweep run.
	backupDir := filepath.Join(cityPath, ".beads", "backup")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", backupDir, err)
	}
	stateJSON := fmt.Sprintf(`{"timestamp":%q}`, time.Now().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(backupDir, "backup_state.json"), []byte(stateJSON), 0o644); err != nil {
		t.Fatalf("write backup_state.json: %v", err)
	}

	_, store := newRetentionBacklogRuntime(time.Now(), 50)
	sp := runtime.NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	closer := &observedCloser{cr: cr}
	if cr.storageRoutes == nil {
		cr.storageRoutes = &storageRoutes{}
	}
	cr.storageRoutes.closers = append(cr.storageRoutes.closers, closer)
	held := make(chan struct{})
	var canceled <-chan struct{}
	cr.od = &recordingOrderDispatcher{onDispatch: func(context.Context, string, time.Time) {
		<-store.deleteStarted
		// The watchdog ran before this dispatch on run()'s own goroutine, so the
		// sweep's cancel is recorded and no join can have read it yet.
		canceled = observeRetentionSweepCancel(cr)
		close(held)
		cancel()
	}}
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = store
	cr.setControllerState(cs)

	done := make(chan struct{})
	go func() {
		defer close(done)
		cr.run(ctx)
	}()
	awaitClose(t, held, "the startup pass starting a sweep that reached Delete")
	// The Delete stays held until run()'s exit is provably parked in
	// shutdown()'s join. An exit that skips the join returns or closes the
	// binding instead, and nothing else ends this wait. Parked is read before
	// returned and closed, so a join that moved after either still reads them.
	awaitCond(t, func() bool {
		parked := retentionSweepJoinParkedIn("(*CityRuntime).shutdown")
		select {
		case <-done:
			return true
		default:
		}
		return closer.closed.Load() || parked
	}, "run()'s exit parking in shutdown()'s sweep join, returning, or closing the storage binding")
	select {
	case <-done:
		close(store.release)
		t.Fatalf("run() returned while the retention sweep still held a Delete")
	default:
	}
	if closer.closed.Load() {
		close(store.release)
		t.Fatalf("run()'s exit closed the storage binding while the retention sweep still held a Delete")
	}
	select {
	case <-canceled:
	default:
		close(store.release)
		t.Fatalf("shutdown() joined the retention sweep without canceling it")
	}

	close(store.release)
	awaitClose(t, done, "run() after the held Delete finished")
	if !closer.closed.Load() {
		t.Fatalf("run()'s exit never closed the storage binding")
	}
	if closer.sweepInFlightAt.Load() {
		t.Fatalf("run()'s exit closed the storage binding before the retention sweep exited")
	}
}

// A sweep cut short by its context traces how far it got, once; a sweep that
// runs to completion reports what it pruned and no cancel.
func TestOrderTrackingRetentionWatchdog_TracesASweepCutShort(t *testing.T) {
	now := time.Now()

	cr, store := newRetentionBacklogRuntime(now, 50)
	stderr := newSyncBuffer()
	cr.stderr = stderr
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cr.runOrderTrackingRetentionWatchdog(ctx, cr.cfg, now)
	select {
	case <-store.deleteStarted:
	case <-time.After(hangBudget):
		t.Fatalf("sweep never reached Delete within the hang budget (%s)", hangBudget)
	}
	cancel()
	close(store.release)
	cr.waitOrderTrackingRetentionSweep()
	got := stderr.String()
	if !strings.Contains(got, "order-tracking retention watchdog: canceled after 1 delete(s)") {
		t.Fatalf("stderr = %q, want the canceled trace for a sweep cut short after one delete", got)
	}
	if strings.Contains(got, "pruned") {
		t.Fatalf("stderr = %q, want no pruned line for a canceled sweep", got)
	}

	const extra = 3
	cr, store = newRetentionBacklogRuntime(now, extra)
	close(store.release)
	stderr = newSyncBuffer()
	cr.stderr = stderr
	runRetentionWatchdogAndWait(cr, now)
	got = stderr.String()
	if !strings.Contains(got, fmt.Sprintf("order-tracking retention watchdog: pruned %d closed bead(s)", extra)) {
		t.Fatalf("stderr = %q, want the pruned line for a completed sweep", got)
	}
	if strings.Contains(got, "canceled") {
		t.Fatalf("stderr = %q, want no canceled trace for a sweep that ran to completion", got)
	}
}

// closeTrackingStore is a controller store handle whose graph-delete calls
// refuse once the handle is closed, recording that a call reached it after the
// close. Its first DepRemove, once it has landed, holds until release, so a
// test can act in the middle of a graph delete.
type closeTrackingStore struct {
	*beads.MemStore
	closed     atomic.Bool
	usedClosed atomic.Bool
	depRemoved chan struct{}
	release    chan struct{}
	holdOnce   sync.Once
}

func newCloseTrackingBacklogStore(now time.Time, extra int, deps []beads.Dep) *closeTrackingStore {
	total := minClosedOrderTrackingRetained + extra
	return &closeTrackingStore{
		MemStore:   beads.NewMemStoreFrom(total, retentionBacklogSeed(now, total), deps),
		depRemoved: make(chan struct{}),
		release:    make(chan struct{}),
	}
}

func (s *closeTrackingStore) CloseStore() error { //nolint:unparam // implements the CloseStore interface closeBeadStoreHandle calls
	s.closed.Store(true)
	return nil
}

func (s *closeTrackingStore) refuseClosed() error {
	if !s.closed.Load() {
		return nil
	}
	s.usedClosed.Store(true)
	return fmt.Errorf("closeTrackingStore: %w", beads.ErrStoreClosed)
}

func (s *closeTrackingStore) DepList(id, direction string) ([]beads.Dep, error) {
	if err := s.refuseClosed(); err != nil {
		return nil, err
	}
	return s.MemStore.DepList(id, direction)
}

func (s *closeTrackingStore) DepAdd(issueID, dependsOnID, depType string) error {
	if err := s.refuseClosed(); err != nil {
		return err
	}
	return s.MemStore.DepAdd(issueID, dependsOnID, depType)
}

func (s *closeTrackingStore) DepRemove(issueID, dependsOnID string) error {
	if err := s.refuseClosed(); err != nil {
		return err
	}
	err := s.MemStore.DepRemove(issueID, dependsOnID)
	s.holdOnce.Do(func() {
		close(s.depRemoved)
		<-s.release
	})
	return err
}

func (s *closeTrackingStore) Delete(id string) error {
	if err := s.refuseClosed(); err != nil {
		return err
	}
	return s.MemStore.Delete(id)
}

// newRetentionControllerRuntime returns a runtime whose controller state serves
// store as the city handle. A reload (cs.update) replaces it with a fresh
// MemStore; the close delay is zero, so a close update schedules runs before
// the scheduling call returns.
func newRetentionControllerRuntime(t *testing.T, store beads.Store, cfg *config.City) (*CityRuntime, *controllerState) {
	t.Helper()
	prevOpen := newControllerStateOpenCityStore
	t.Cleanup(func() { newControllerStateOpenCityStore = prevOpen })
	newControllerStateOpenCityStore = func(string, gate.Mode) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: beads.NewMemStore()}, nil
	}
	setControllerStateStoreCloseDelayForTest(t, 0)
	cs := &controllerState{
		cfg:           &config.City{},
		cityPath:      t.TempDir(),
		cityBeadStore: store,
		beadStores:    map[string]beads.Store{},
	}
	cr := &CityRuntime{
		cityName:                  "test-city",
		cfg:                       cfg,
		stdout:                    io.Discard,
		stderr:                    io.Discard,
		logPrefix:                 "gc test",
		wispIndexMigrationApplied: true,
	}
	cr.setControllerState(cs)
	return cr, cs
}

// servesCityStore reports whether cs currently serves store as its city handle.
func servesCityStore(cs *controllerState, store beads.Store) bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.cityBeadStore == store
}

// A store update that lands in the middle of a graph delete (deps unwound,
// Delete not yet run) must return without waiting for it — API mutations run
// update on HTTP goroutines under a client timeout — and must not close the
// replaced handle under it: the delete and any rollback run on the handle the
// sweep resolved, the bead ends whole or fully deleted, and the handle closes
// once the sweep has exited.
func TestOrderTrackingRetentionWatchdog_UpdateDuringGraphDeleteReturnsAndClosesAfterSweep(t *testing.T) {
	now := time.Now()
	store := newCloseTrackingBacklogStore(now, 1, []beads.Dep{
		{IssueID: "backlog-0000", DependsOnID: "anchor-a", Type: "blocks"},
		{IssueID: "backlog-0000", DependsOnID: "anchor-b", Type: "blocks"},
	})
	cr, cs := newRetentionControllerRuntime(t, store, &config.City{Workspace: config.Workspace{Name: "test-city"}})

	cr.runOrderTrackingRetentionWatchdog(context.Background(), cr.cfg, now)
	awaitClose(t, store.depRemoved, "the sweep's first DepRemove")

	updated := make(chan struct{})
	go func() {
		defer close(updated)
		cs.update(&config.City{}, runtime.NewFake())
	}()
	awaitClose(t, updated, "the store update while the sweep holds a graph delete")
	if servesCityStore(cs, store) {
		t.Fatalf("the update returned without replacing the city handle")
	}
	if !cr.orderTrackingRetentionSweepInFlight() {
		t.Fatalf("the sweep exited while its graph delete was still held")
	}
	if store.closed.Load() {
		t.Fatalf("the update closed the replaced handle while the sweep was still in its graph delete")
	}

	close(store.release)
	cr.waitOrderTrackingRetentionSweep()
	awaitCond(t, store.closed.Load, "the replaced handle's close after the sweep exited")

	if store.usedClosed.Load() {
		t.Fatalf("the graph delete reached the replaced handle after it was closed")
	}
	deps, err := store.MemStore.DepList("backlog-0000", "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	_, getErr := store.Get("backlog-0000")
	switch {
	case getErr != nil && len(deps) == 0: // fully deleted
	case getErr == nil && len(deps) == 2: // kept whole
	default:
		t.Fatalf("backlog-0000 present=%v with %d of 2 deps: a surviving bead lost edges", getErr == nil, len(deps))
	}
}

// Resolving the sweep's handles and starting it is one snapshot with respect
// to a store update: no update can swap the handles in between, so a sweep
// never starts on handles an update has already replaced.
func TestOrderTrackingRetentionWatchdog_UpdateCannotSwapWhileSweepResolves(t *testing.T) {
	now := time.Now()
	store := newCloseTrackingBacklogStore(now, 5, nil)
	close(store.release)
	rigDir := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "frontend", Path: rigDir}},
	}
	cr, cs := newRetentionControllerRuntime(t, store, cfg)

	// The controller serves no frontend handle, so resolution opens one after
	// it has resolved the city handle. An update swaps only under updateMu:
	// it must be held here.
	opened := false
	swappable := false
	prevOpenSweep := newCityRuntimeOpenSweepStore
	t.Cleanup(func() { newCityRuntimeOpenSweepStore = prevOpenSweep })
	newCityRuntimeOpenSweepStore = func(string, string) (beads.Store, error) {
		opened = true
		if cs.updateMu.TryLock() {
			swappable = true
			cs.updateMu.Unlock()
		}
		return beads.NewMemStore(), nil
	}

	cr.runOrderTrackingRetentionWatchdog(context.Background(), cr.cfg, now)
	if !opened {
		t.Fatalf("resolution never opened the frontend handle")
	}
	if swappable {
		t.Fatalf("a store update could swap the handles between the sweep resolving them and starting")
	}

	cs.update(&config.City{}, runtime.NewFake())
	cr.waitOrderTrackingRetentionSweep()
	awaitCond(t, store.closed.Load, "the replaced handle's close")
	if store.usedClosed.Load() {
		t.Fatalf("the sweep deleted through the handle the update replaced and closed")
	}
}

// With no sweep in flight — none started yet, or the last one finished — an
// update schedules the replaced handle's close exactly as it always has.
func TestOrderTrackingRetentionWatchdog_UpdateWithNoSweepInFlightClosesReplacedHandle(t *testing.T) {
	for _, tc := range []struct {
		name     string
		runSweep bool
	}{
		{name: "no sweep yet"},
		{name: "sweep finished", runSweep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			store := newCloseTrackingBacklogStore(now, 1, nil)
			close(store.release)
			cr, cs := newRetentionControllerRuntime(t, store, &config.City{Workspace: config.Workspace{Name: "test-city"}})
			if tc.runSweep {
				runRetentionWatchdogAndWait(cr, now)
			}

			// The close delay is zero, so the close runs inside update.
			cs.update(&config.City{}, runtime.NewFake())
			if servesCityStore(cs, store) {
				t.Fatalf("the update did not replace the city handle")
			}
			if !store.closed.Load() {
				t.Fatalf("the update did not close the replaced handle with no sweep in flight")
			}
		})
	}
}

// A cancel that arrives while the sweep is still listing ends it with no
// deletes at all.
func TestOrderTrackingRetentionWatchdog_CancelDuringListingDeletesNothing(t *testing.T) {
	now := time.Now()
	const extra = 5
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &cancelOnListStore{
		blockingDeleteStore: &blockingDeleteStore{
			MemStore:      beads.NewMemStoreFrom(minClosedOrderTrackingRetained+extra, retentionBacklogSeed(now, minClosedOrderTrackingRetained+extra), nil),
			deleteStarted: make(chan string, 1),
			release:       make(chan struct{}),
		},
		cancel: cancel,
	}
	close(store.release)

	deleted, err := sweepClosedOrderTrackingRetentionAcrossStoresBounded(
		ctx, []beads.Store{store}, now, orderTrackingRetentionPolicyForConfig(&config.City{}), nil, orderTrackingRetentionWatchdogDeleteBudget)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted %d beads after a cancel during the listing, want 0", deleted)
	}
	if got := countDeletedBacklog(t, store.blockingDeleteStore, minClosedOrderTrackingRetained+extra); got != 0 {
		t.Fatalf("%d beads gone from the store after a cancel during the listing, want 0", got)
	}
}

// cancelOnListStore cancels the sweep's context while serving its listing.
type cancelOnListStore struct {
	*blockingDeleteStore
	cancel context.CancelFunc
}

func (s *cancelOnListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.cancel()
	return s.blockingDeleteStore.List(query)
}

// observeRetentionSweepCancel wraps the in-flight sweep's cancel so the test
// can observe that it was called. The returned channel closes on the first
// call.
func observeRetentionSweepCancel(cr *CityRuntime) <-chan struct{} {
	canceled := make(chan struct{})
	var once sync.Once
	cr.retentionSweepMu.Lock()
	defer cr.retentionSweepMu.Unlock()
	sweepCancel := cr.retentionSweepCancel
	cr.retentionSweepCancel = func() {
		sweepCancel()
		once.Do(func() { close(canceled) })
	}
	return canceled
}

// retentionSweepJoinParkedIn reports whether some goroutine whose stack
// includes caller is blocked in waitOrderTrackingRetentionSweep's receive on
// the sweep's done channel. It is the positive evidence that a stop or
// shutdown has reached the join itself — not merely its cancel — read from
// the goroutine dump, so production code carries no test seam.
func retentionSweepJoinParkedIn(caller string) bool {
	buf := make([]byte, 1<<20)
	for {
		n := goruntime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		header, frames, _ := strings.Cut(g, "\n")
		if !strings.Contains(header, "[chan receive") || !strings.Contains(frames, caller) {
			continue
		}
		// The innermost non-runtime frame is where the goroutine blocked.
		for _, line := range strings.Split(frames, "\n") {
			if line == "" || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "runtime.") {
				continue
			}
			if strings.Contains(line, "(*CityRuntime).waitOrderTrackingRetentionSweep(") {
				return true
			}
			break
		}
	}
	return false
}

// observedCloser records, when the storage binding is closed, whether the
// retention sweep was still in flight.
type observedCloser struct {
	cr              *CityRuntime
	closed          atomic.Bool
	sweepInFlightAt atomic.Bool
}

func (c *observedCloser) Close() error {
	c.sweepInFlightAt.Store(c.cr.orderTrackingRetentionSweepInFlight())
	c.closed.Store(true)
	return nil
}

// A forced shutdown runs shutdown() while run() may still be live. shutdown
// closes the storage binding the sweep may be deleting through, so it must
// cancel and join the sweep first.
func TestOrderTrackingRetentionWatchdog_ShutdownJoinsSweepBeforeClosingBinding(t *testing.T) {
	now := time.Now()
	const extra = 50
	cr, store := newRetentionBacklogRuntime(now, extra)
	cr.preserveSessionsShutdown.Store(true)
	cr.storageRoutes = messagingSplitRoutes(store)
	closer := &observedCloser{cr: cr}
	cr.storageRoutes.closers = append(cr.storageRoutes.closers, closer)

	cr.runOrderTrackingRetentionWatchdog(context.Background(), cr.cfg, now)
	select {
	case <-store.deleteStarted:
	case <-time.After(5 * time.Second):
		t.Fatalf("sweep never reached Delete")
	}

	canceled := observeRetentionSweepCancel(cr)
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		cr.shutdown()
	}()
	// The Delete stays held until shutdown is provably parked in the sweep's
	// join. A shutdown that does not join first reaches the binding close
	// instead — observed by closer — and nothing else ends this wait. Parked is
	// read before closed, so a join moved after the close still reads closed.
	awaitCond(t, func() bool {
		parked := retentionSweepJoinParkedIn("(*CityRuntime).shutdown")
		return closer.closed.Load() || parked
	}, "shutdown() parking in the sweep's join or closing the storage binding")
	if closer.closed.Load() {
		close(store.release)
		t.Fatalf("shutdown() closed the storage binding while the retention sweep still held a Delete")
	}
	// The join cancels before it waits, so parked in the join means canceled:
	// the released Delete is the sweep's last.
	select {
	case <-canceled:
	default:
		close(store.release)
		t.Fatalf("shutdown() joined the retention sweep without canceling it")
	}

	close(store.release)
	awaitClose(t, shutdownDone, "shutdown()")
	if !closer.closed.Load() {
		t.Fatalf("shutdown() never closed the storage binding")
	}
	if closer.sweepInFlightAt.Load() {
		t.Fatalf("shutdown() closed the storage binding before the retention sweep exited")
	}
	if got := countDeletedBacklog(t, store, minClosedOrderTrackingRetained+extra); got != 1 {
		t.Fatalf("deleted %d beads, want 1: shutdown must cancel the sweep, not wait out its budget", got)
	}
}

// failFirstDeleteStore fails its first Delete, then behaves like
// blockingDeleteStore.
type failFirstDeleteStore struct {
	*blockingDeleteStore
	failed atomic.Bool
}

func (s *failFirstDeleteStore) Delete(id string) error {
	if s.failed.CompareAndSwap(false, true) {
		return errors.New("injected delete failure")
	}
	return s.blockingDeleteStore.Delete(id)
}

// A real delete failure is reported even when the sweep is canceled later.
func TestOrderTrackingRetentionWatchdog_ReportsErrorsBeforeCancel(t *testing.T) {
	now := time.Now()
	cr, blocking := newRetentionBacklogRuntime(now, 50)
	store := &failFirstDeleteStore{blockingDeleteStore: blocking}
	cr.standaloneCityStore = store
	stderr := newSyncBuffer()
	cr.stderr = stderr

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cr.runOrderTrackingRetentionWatchdog(ctx, cr.cfg, now)
	select {
	case <-blocking.deleteStarted:
	case <-time.After(5 * time.Second):
		t.Fatalf("sweep never reached the second Delete")
	}
	cancel()
	close(blocking.release)
	cr.waitOrderTrackingRetentionSweep()

	if got := stderr.String(); !strings.Contains(got, "injected delete failure") {
		t.Fatalf("stderr = %q, want the delete failure that preceded the cancel", got)
	}
}
