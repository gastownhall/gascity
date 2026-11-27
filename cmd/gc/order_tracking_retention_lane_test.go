package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
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

// retentionLaneRuntime is a directly-constructed runtime for driving the
// retention lane: a scratch city whose last backup is backupAge old, city as
// its standalone city store and, when binding is non-nil, a split storage
// binding that holds the orders class. FS pressure is pinned off.
func retentionLaneRuntime(t *testing.T, now time.Time, backupAge time.Duration, city, binding beads.Store) (*CityRuntime, *syncWriter) {
	t.Helper()
	t.Setenv(fsPressureThresholdEnv, "100")
	cityPath := t.TempDir()
	writeFreshRetentionBackup(t, cityPath, now.Add(time.Hour-backupAge))
	stderr := &syncWriter{}
	cr := &CityRuntime{
		cityName:            "test-city",
		cityPath:            cityPath,
		cfg:                 &config.City{Workspace: config.Workspace{Name: "test-city"}},
		standaloneCityStore: city,
		standaloneRigStores: map[string]beads.Store{},
		rec:                 events.Discard,
		stdout:              io.Discard,
		stderr:              stderr,
		logPrefix:           "gc test",
	}
	if binding != nil {
		cr.storageRoutes = messagingSplitRoutes(binding)
	}
	return cr, stderr
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// runRetentionLaneUntil starts the retention lane, waits until stderr carries
// want, then stops the lane and waits for it, so the caller reads the lane's
// state after its goroutine has exited.
func runRetentionLaneUntil(t *testing.T, cr *CityRuntime, stderr *syncWriter, want string) {
	t.Helper()
	stop := cr.startOrderTrackingRetentionLane(context.Background())
	awaitCond(t, func() bool { return strings.Contains(stderr.String(), want) },
		fmt.Sprintf("the retention lane's first pass logging %q", want))
	stop()
}

// assertBeadsGone and assertBeadsKept check a store's surviving set.
func assertBeadsGone(t *testing.T, store beads.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := store.Get(id); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("Get(%s) err = %v, want ErrNotFound (pruned)", id, err)
		}
	}
}

func assertBeadsKept(t *testing.T, store beads.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := store.Get(id); err != nil {
			t.Fatalf("Get(%s) = %v, want it retained", id, err)
		}
	}
}

func seedIDs(seed []beads.Bead) []string {
	ids := make([]string, 0, len(seed))
	for _, b := range seed {
		ids = append(ids, b.ID)
	}
	return ids
}

// The retention lane's first pass runs as soon as the lane starts, with no
// interval wait, and prunes what the watchdog prunes: past the TTL, beyond
// the keep-10 floor of each order, in the city store and the orders binding.
func TestOrderTrackingRetentionLaneFirstPassPrunesCityAndOrdersBinding(t *testing.T) {
	now := time.Now()
	citySeed := retentionEligibleSeed("city", minClosedOrderTrackingRetained+2, now)
	bindSeed := retentionEligibleSeed("bind", minClosedOrderTrackingRetained+2, now)
	city := beads.NewMemStoreFrom(100, citySeed, nil)
	binding := beads.NewMemStoreFrom(100, bindSeed, nil)
	cr, stderr := retentionLaneRuntime(t, now, time.Hour, city, binding)

	runRetentionLaneUntil(t, cr, stderr, "pruned 4 closed bead(s)")

	assertBeadsGone(t, city, "city-00", "city-01")
	assertBeadsKept(t, city, seedIDs(citySeed[2:])...)
	assertBeadsGone(t, binding, "bind-00", "bind-01")
	assertBeadsKept(t, binding, seedIDs(bindSeed[2:])...)
}

// One pass deletes at most orderTrackingRetentionWatchdogDeleteBudget beads,
// and never one inside an order's keep-10 floor.
func TestOrderTrackingRetentionLaneFirstPassHonorsDeleteBudget(t *testing.T) {
	now := time.Now()
	n := orderTrackingRetentionWatchdogDeleteBudget + minClosedOrderTrackingRetained + 5
	seed := retentionEligibleSeed("budget", n, now)
	city := beads.NewMemStoreFrom(1000, seed, nil)
	cr, stderr := retentionLaneRuntime(t, now, time.Hour, city, nil)

	runRetentionLaneUntil(t, cr, stderr, fmt.Sprintf("pruned %d closed bead(s)", orderTrackingRetentionWatchdogDeleteBudget))

	assertBeadsKept(t, city, seedIDs(seed[n-minClosedOrderTrackingRetained:])...)
	remaining := 0
	for _, id := range seedIDs(seed) {
		if _, err := city.Get(id); err == nil {
			remaining++
		}
	}
	if want := n - orderTrackingRetentionWatchdogDeleteBudget; remaining != want {
		t.Fatalf("closed tracking beads remaining = %d, want %d after one budgeted pass", remaining, want)
	}
}

// A stale backup still blocks the lane's bulk delete, and the pass still
// consumes the interval stamp, as the watchdog does.
func TestOrderTrackingRetentionLaneFirstPassSkipsOnStaleBackup(t *testing.T) {
	now := time.Now()
	seed := retentionEligibleSeed("stale", minClosedOrderTrackingRetained+2, now)
	city := beads.NewMemStoreFrom(100, seed, nil)
	cr, stderr := retentionLaneRuntime(t, now, 48*time.Hour, city, nil)

	runRetentionLaneUntil(t, cr, stderr, "skipping bulk delete")

	assertBeadsKept(t, city, seedIDs(seed)...)
	if cr.orderTrackingRetentionWatchdogLast.IsZero() {
		t.Fatal("orderTrackingRetentionWatchdogLast is zero, want the skipped pass to consume the interval")
	}
}

// untouchableStore fails the test on any bead-store call: its embedded Store
// is nil, so a method it does not override panics, and the overrides name the
// calls a retention sweep makes first.
type untouchableStore struct {
	beads.Store
	t *testing.T
}

func (s untouchableStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.t.Errorf("store List(%+v) on a suspended city", q)
	return nil, errors.New("untouchable store")
}

func (s untouchableStore) Get(id string) (beads.Bead, error) {
	s.t.Errorf("store Get(%q) on a suspended city", id)
	return beads.Bead{}, errors.New("untouchable store")
}

// A suspended city's retention pass makes no bead-store call: a read would
// restart the proxies suspension retired, as the order pass already avoids.
func TestOrderTrackingRetentionLanePassSkipsSuspendedCity(t *testing.T) {
	now := time.Now()
	cr, _ := retentionLaneRuntime(t, now, time.Hour, untouchableStore{t: t}, nil)
	t.Setenv("GC_SUSPENDED", "1")

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the retention pass called the store on a suspended city: %v", r)
			}
		}()
		cr.runOrderTrackingRetentionLanePass(context.Background())
	}()
	if !cr.orderTrackingRetentionWatchdogLast.IsZero() {
		t.Fatalf("orderTrackingRetentionWatchdogLast = %v, want zero: a suspended pass is not a sweep", cr.orderTrackingRetentionWatchdogLast)
	}
}

// Stopping the lane mid-sweep cancels the sweep at its next store-call
// boundary: stop waits for the in-flight listing and the lane goroutine, no
// store call starts after the listing returns, nothing is deleted, and the
// watchdog logs the cancel instead of an error.
func TestOrderTrackingRetentionLaneStopCancelsSweepAfterInFlightCall(t *testing.T) {
	now := time.Now()
	seed := retentionEligibleSeed("cancel", minClosedOrderTrackingRetained+2, now)
	inner := beads.NewMemStoreFrom(100, seed, nil)
	gate := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	t.Cleanup(release)
	store := newRetentionListingStore(inner, gate)
	cr, stderr := retentionLaneRuntime(t, now, time.Hour, store, nil)

	ctx, cancel := context.WithCancel(context.Background())
	stop := cr.startOrderTrackingRetentionLane(ctx)
	awaitClose(t, store.entered, "the retention lane reaching its listing")
	callsAtListing := store.calls.Load()

	// Cancel the lane's parent first, so the sweep is canceled before the
	// listing returns, then stop the lane as run()'s deferred cleanup does.
	cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stop()
	}()
	// A short window: stop cannot return while the lane goroutine is parked.
	select {
	case <-stopped:
		t.Fatal("stop returned while the lane goroutine was parked in the listing")
	case <-time.After(100 * time.Millisecond):
	}

	release()
	awaitClose(t, stopped, "stop returning once the in-flight listing returned")
	if got := store.calls.Load(); got != callsAtListing {
		t.Fatalf("store calls = %d after the canceled listing returned, want %d: the sweep kept calling the store", got, callsAtListing)
	}
	assertBeadsKept(t, inner, seedIDs(seed)...)
	out := stderr.String()
	if !strings.Contains(out, "canceled after 0 delete(s)") {
		t.Fatalf("stderr = %q, want the canceled line", out)
	}
	if strings.Contains(out, context.Canceled.Error()) {
		t.Fatalf("stderr = %q, want the cancel reported as a cancel, not an error", out)
	}
}

// guardFailingStore cancels the sweep's context and fails on the first call of
// the per-candidate strand guard: a real store error that precedes a cancel.
type guardFailingStore struct {
	beads.Store
	cancel context.CancelFunc
}

func (s guardFailingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if isRetentionListing(q) {
		return s.Store.List(q)
	}
	s.cancel()
	return nil, errors.New("guard read failed")
}

// A cross-store sweep canceled part-way lists no further store and keeps the
// real error that preceded the cancel, without reporting the cancel itself.
func TestSweepClosedOrderTrackingRetentionAcrossStoresBoundedStopsOnCancel(t *testing.T) {
	now := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := guardFailingStore{
		Store:  beads.NewMemStoreFrom(100, retentionEligibleSeed("first", minClosedOrderTrackingRetained+2, now), nil),
		cancel: cancel,
	}
	secondSeed := retentionEligibleSeed("second", minClosedOrderTrackingRetained+2, now)
	secondInner := beads.NewMemStoreFrom(100, secondSeed, nil)
	second := newRetentionListingStore(secondInner, nil)

	deleted, err := sweepClosedOrderTrackingRetentionAcrossStoresBounded(ctx, []beads.Store{first, second}, now,
		orderTrackingRetentionPolicyForConfig(&config.City{}), nil, orderTrackingRetentionWatchdogDeleteBudget)

	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0", deleted)
	}
	if err == nil || !strings.Contains(err.Error(), "guard read failed") {
		t.Fatalf("err = %v, want the first store's guard error", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancel itself not reported as an error", err)
	}
	if n := second.calls.Load(); n != 0 {
		t.Fatalf("second store calls = %d, want 0 after the cancel", n)
	}
	assertBeadsKept(t, secondInner, seedIDs(secondSeed)...)
}

// retentionCancelStore cancels a sweep's context inside one store call, the
// one cancelIn names, and logs every store call the sweep makes, so a test
// reads what ran after the cancel. The cancel lands while that call is in
// flight; the call itself completes. Embedding the beads.Store interface hides
// DeleteBatch, so a sweep deletes bead by bead, as it does through the
// watchdog's scoped stores.
type retentionCancelStore struct {
	beads.Store
	cancel   context.CancelFunc
	cancelIn string
	calls    []string
	// canceledAt indexes calls at the call the cancel landed in; -1 before.
	canceledAt int
}

func newRetentionCancelStore(inner beads.Store, cancel context.CancelFunc, cancelIn string) *retentionCancelStore {
	return &retentionCancelStore{Store: inner, cancel: cancel, cancelIn: cancelIn, canceledAt: -1}
}

func (s *retentionCancelStore) record(call string) {
	s.calls = append(s.calls, call)
	if s.canceledAt < 0 && call == s.cancelIn {
		s.canceledAt = len(s.calls) - 1
		s.cancel()
	}
}

// afterCancel returns the store calls that started after the cancel, failing
// the test when the sweep never made the call to cancel in.
func (s *retentionCancelStore) afterCancel(t *testing.T) []string {
	t.Helper()
	if s.canceledAt < 0 {
		t.Fatalf("the sweep never made the call %q to cancel in; it made %q", s.cancelIn, s.calls)
	}
	return s.calls[s.canceledAt+1:]
}

// describeRetentionList names a List call by the sweep step that makes it:
// the retention listing, a candidate's strand guard (its membership read
// includes closed members), a set-level strand guard read, or the candidate
// guard walk's child read.
func describeRetentionList(q beads.ListQuery) string {
	root := q.Metadata[beadmeta.RootBeadIDMetadataKey]
	switch {
	case isRetentionListing(q):
		return "listing"
	case root != "" && q.IncludeClosed:
		return "candidate guard " + root
	case root != "":
		return "set guard " + root
	case q.ParentID != "":
		return "children " + q.ParentID
	default:
		return fmt.Sprintf("List %+v", q)
	}
}

func (s *retentionCancelStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.record(describeRetentionList(q))
	return s.Store.List(q)
}

func (s *retentionCancelStore) Get(id string) (beads.Bead, error) {
	s.record("Get " + id)
	return s.Store.Get(id)
}

func (s *retentionCancelStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.record("DepList " + id + " " + direction)
	return s.Store.DepList(id, direction)
}

func (s *retentionCancelStore) DepRemove(issueID, dependsOnID string) error {
	s.record("DepRemove " + issueID + " " + dependsOnID)
	return s.Store.DepRemove(issueID, dependsOnID)
}

func (s *retentionCancelStore) Delete(id string) error {
	s.record("Delete " + id)
	return s.Store.Delete(id)
}

// retentionCancelBatchStore is a retentionCancelStore whose deletes take the
// batched path.
type retentionCancelBatchStore struct {
	*retentionCancelStore
}

var _ beads.BatchDeleter = retentionCancelBatchStore{}

func (s retentionCancelBatchStore) DeleteBatch(ids []string) error {
	s.record("DeleteBatch " + strings.Join(ids, ","))
	for _, id := range ids {
		if err := s.Store.Delete(id); err != nil {
			return err
		}
	}
	return nil
}

// retentionThreeCandidateSeed is 13 closed runs of one order, all past the
// TTL: the bounded sweep visits the three beyond the keep-10 floor in the
// order run-02, run-01, run-00.
func retentionThreeCandidateSeed(now time.Time) []beads.Bead {
	return retentionEligibleSeed("run", minClosedOrderTrackingRetained+3, now)
}

// sweepRetentionBoundedWithDefaults runs the bounded per-store sweep with the
// watchdog's default policy and delete budget.
func sweepRetentionBoundedWithDefaults(ctx context.Context, store beads.Store, now time.Time) (int, error) {
	return sweepClosedOrderTrackingRetentionBounded(ctx, store, now,
		orderTrackingRetentionPolicyForConfig(&config.City{}), nil, orderTrackingRetentionWatchdogDeleteBudget)
}

// A cancel that lands inside a candidate's strand guard, one the candidate
// passes, stops the sweep once that guard returns: no later candidate is
// guarded, no set-level guard read or delete starts, and every run survives.
func TestSweepClosedOrderTrackingRetentionBoundedCancelInCandidateGuard(t *testing.T) {
	now := time.Now()
	seed := retentionThreeCandidateSeed(now)
	inner := beads.NewMemStoreFrom(100, seed, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newRetentionCancelStore(inner, cancel, "candidate guard run-02")

	deleted, err := sweepRetentionBoundedWithDefaults(ctx, store, now)

	if deleted != 0 || err != nil {
		t.Fatalf("sweep = (%d, %v), want (0, nil)", deleted, err)
	}
	// The guard in flight finishes its own walk of run-02, and nothing else
	// runs.
	walk := map[string]bool{"children run-02": true, "Get run-02": true, "DepList run-02 up": true}
	after := store.afterCancel(t)
	for _, call := range after {
		if !walk[call] {
			t.Fatalf("store calls after the cancel = %q, want only the in-flight guard's walk of run-02", after)
		}
	}
	assertBeadsKept(t, inner, seedIDs(seed)...)
}

// A cancel that lands inside a set-level strand guard read stops the sweep
// before the next guard read and before any delete, batched or per bead: the
// stop waits for that one read.
func TestSweepClosedOrderTrackingRetentionBoundedCancelInSetGuard(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		cancelIn string
		batched  bool
	}{
		{name: "first read, per bead", cancelIn: "set guard run-02"},
		{name: "first read, batched", cancelIn: "set guard run-02", batched: true},
		{name: "last read, per bead", cancelIn: "set guard run-00"},
		{name: "last read, batched", cancelIn: "set guard run-00", batched: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed := retentionThreeCandidateSeed(now)
			inner := beads.NewMemStoreFrom(100, seed, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := newRetentionCancelStore(inner, cancel, tc.cancelIn)
			var swept beads.Store = store
			if tc.batched {
				swept = retentionCancelBatchStore{store}
			}

			deleted, err := sweepRetentionBoundedWithDefaults(ctx, swept, now)

			if deleted != 0 || err != nil {
				t.Fatalf("sweep = (%d, %v), want (0, nil)", deleted, err)
			}
			if after := store.afterCancel(t); len(after) != 0 {
				t.Fatalf("store calls after the cancel = %q, want none", after)
			}
			assertBeadsKept(t, inner, seedIDs(seed)...)
		})
	}
}

// A cancel that lands while a run's per-bead delete is in flight lets that
// delete finish, dep unwind included, so the run is never left half deleted,
// and stops before the next run's delete: the later runs survive and the
// sweep counts the one delete.
func TestSweepClosedOrderTrackingRetentionBoundedCancelInDeleteFinishesOnlyThatRun(t *testing.T) {
	now := time.Now()
	seed := append(retentionThreeCandidateSeed(now), beads.Bead{ID: "anchor", Title: "anchor", Status: "open", Type: "task"})
	inner := beads.NewMemStoreFrom(100, seed, nil)
	if err := inner.DepAdd("run-02", "anchor", "blocks"); err != nil {
		t.Fatalf("DepAdd(run-02, anchor): %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newRetentionCancelStore(inner, cancel, "DepList run-02 down")

	deleted, err := sweepRetentionBoundedWithDefaults(ctx, store, now)

	if deleted != 1 || err != nil {
		t.Fatalf("sweep = (%d, %v), want (1, nil)", deleted, err)
	}
	want := []string{"DepList run-02 up", "DepRemove run-02 anchor", "Delete run-02"}
	if after := store.afterCancel(t); !slices.Equal(after, want) {
		t.Fatalf("store calls after the cancel = %q, want only the in-flight delete of run-02, %q", after, want)
	}
	assertBeadsGone(t, inner, "run-02")
	assertBeadsKept(t, inner, "run-01", "run-00", "anchor")
}

// secondDeleteFailingStore fails the second bead Delete it serves.
type secondDeleteFailingStore struct {
	beads.Store
	deletes int
}

func (s *secondDeleteFailingStore) Delete(id string) error {
	s.deletes++
	if s.deletes == 2 {
		return errors.New("second delete failed")
	}
	return s.Store.Delete(id)
}

// A per-bead delete that fails part-way stops the sweep, and the sweep counts
// the run it deleted before the failure, so the watchdog's delete budget and
// its pruned line see that delete.
func TestSweepClosedOrderTrackingRetentionBoundedCountsDeletesBeforeAFailure(t *testing.T) {
	now := time.Now()
	inner := beads.NewMemStoreFrom(100, retentionThreeCandidateSeed(now), nil)
	store := &secondDeleteFailingStore{Store: inner}

	deleted, err := sweepRetentionBoundedWithDefaults(context.Background(), store, now)

	if deleted != 1 || err == nil || !strings.Contains(err.Error(), "second delete failed") {
		t.Fatalf("sweep = (%d, %v), want (1, the second delete's error)", deleted, err)
	}
	assertBeadsGone(t, inner, "run-02")
	assertBeadsKept(t, inner, "run-01", "run-00")
}

// run() starts the retention lane once the city is ready and joins it on the
// way out: with the lane parked inside its listing, a canceled run() does not
// begin shutdown(), which closes the storage binding the sweep reads, until
// the listing returns and the lane exits.
func TestCityRuntimeRunJoinsRetentionLaneBeforeShutdown(t *testing.T) {
	h := newRetentionRunHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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

	awaitClose(t, h.started, "onStarted")
	awaitClose(t, h.store.entered, "run()'s retention lane reaching its listing")

	cancel()
	// A short window: shutdown cannot begin while the lane is parked.
	select {
	case <-h.od.drained:
		t.Fatal("shutdown began while the retention lane was parked in its listing")
	case <-runDone:
		t.Fatal("run() returned while the retention lane was parked in its listing")
	case <-time.After(100 * time.Millisecond):
	}

	h.release()
	awaitClose(t, runDone, "run() returning once the retention lane exited")
	awaitClose(t, h.od.drained, "shutdown draining the order dispatcher")
}
