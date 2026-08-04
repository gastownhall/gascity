package beads

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNextReconcileDelay verifies exponential backoff in nextReconcileDelay:
// delay starts at failure 1 (not 5), doubles per increment, and caps at 10 min.
func TestNextReconcileDelay(t *testing.T) {
	t.Parallel()

	now := time.Unix(10000, 0)

	makeCache := func(syncFails int, problemAt time.Time) *CachingStore {
		c := NewCachingStoreForTest(NewMemStore(), nil)
		c.state = cacheLive
		c.lastFreshAt = time.Unix(1, 0) // stale — normal path returns 0
		c.syncFailures = syncFails
		c.stats.LastProblemAt = problemAt
		return c
	}

	t.Run("backoff applies at failure 1", func(t *testing.T) {
		t.Parallel()
		// problemAt == now so delay == backoff exactly; normal cadence path returns 0 here.
		c := makeCache(1, now)
		if delay := c.nextReconcileDelay(now); delay <= 0 {
			t.Fatalf("syncFailures=1: got delay %v, want > 0 (exponential backoff must apply from failure 1)", delay)
		}
	})

	t.Run("delay doubles per failure", func(t *testing.T) {
		t.Parallel()
		// problemAt == now so delay == backoff; each step must be exactly 2× prior.
		var prev time.Duration
		for n := 1; n <= 6; n++ {
			c := makeCache(n, now)
			delay := c.nextReconcileDelay(now)
			if delay <= 0 {
				t.Fatalf("syncFailures=%d: got delay %v, want > 0", n, delay)
			}
			if n > 1 && delay != prev*2 {
				t.Fatalf("syncFailures=%d: got %v, want %v (2× previous %v)", n, delay, prev*2, prev)
			}
			prev = delay
		}
	})

	t.Run("caps at 10 minutes", func(t *testing.T) {
		t.Parallel()
		maxBackoff := 10 * time.Minute
		// syncFailures=20 → 2s*2^20 far exceeds cap; delay must equal maxBackoff.
		c := makeCache(20, now)
		delay := c.nextReconcileDelay(now)
		if delay != maxBackoff {
			t.Fatalf("syncFailures=20: got %v, want %v (cap)", delay, maxBackoff)
		}
	})
}

type reconcileRaceStore struct {
	Store
	started chan struct{}
	release chan struct{}
	stale   []Bead

	mu    sync.Mutex
	block bool
	once  sync.Once

	afterStaleDepListID string
	afterStaleDepList   func()
	depOnce             sync.Once
}

func (s *reconcileRaceStore) List(query ListQuery) ([]Bead, error) {
	if !query.AllowScan {
		return s.Store.List(query)
	}

	s.mu.Lock()
	block := s.block
	s.mu.Unlock()
	if !block {
		return s.Store.List(query)
	}

	s.once.Do(func() {
		close(s.started)
	})
	<-s.release
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Bead(nil), s.stale...), nil
}

func (s *reconcileRaceStore) DepList(id, direction string) ([]Dep, error) {
	deps, err := s.Store.DepList(id, direction)
	if err == nil && id == s.afterStaleDepListID && s.afterStaleDepList != nil {
		s.depOnce.Do(s.afterStaleDepList)
	}
	return deps, err
}

func TestCachingStoreReconciliationPreservesConcurrentMutation(t *testing.T) {
	mem := NewMemStore()
	original, err := mem.Create(Bead{Title: "before reconcile"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	backing := &reconcileRaceStore{
		Store:   mem,
		started: make(chan struct{}),
		release: make(chan struct{}),
		stale:   []Bead{original},
	}
	cs := NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.mu.Lock()
	backing.block = true
	backing.mu.Unlock()

	done := make(chan struct{})
	go func() {
		cs.runReconciliation()
		close(done)
	}()

	<-backing.started
	title := "after concurrent update"
	if err := cs.Update(original.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	close(backing.release)
	<-done

	items, err := cs.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(items) != 1 || items[0].Title != title {
		t.Fatalf("ListOpen = %#v, want updated title %q", items, title)
	}
}

func TestCachingStoreReconciliationPreservesConcurrentEvent(t *testing.T) {
	mem := NewMemStore()
	original, err := mem.Create(Bead{Title: "before reconcile"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	backing := &reconcileRaceStore{
		Store:   mem,
		started: make(chan struct{}),
		release: make(chan struct{}),
		stale:   []Bead{original},
	}
	cs := NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.mu.Lock()
	backing.block = true
	backing.mu.Unlock()

	done := make(chan struct{})
	go func() {
		cs.runReconciliation()
		close(done)
	}()

	<-backing.started
	eventBead := cloneBead(original)
	eventBead.Title = "after concurrent event"
	payload, err := json.Marshal(eventBead)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	cs.ApplyEvent("bead.updated", payload)
	close(backing.release)
	<-done

	items, err := cs.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(items) != 1 || items[0].Title != eventBead.Title {
		t.Fatalf("ListOpen = %#v, want event title %q", items, eventBead.Title)
	}
}

func TestCachingStoreReconciliationPreservesConcurrentDependencyInvalidation(t *testing.T) {
	mem := NewMemStore()
	blocker, err := mem.Create(Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create(blocker): %v", err)
	}
	target, err := mem.Create(Bead{Title: "target"})
	if err != nil {
		t.Fatalf("Create(target): %v", err)
	}

	backing := &reconcileRaceStore{Store: mem}
	cs := NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.afterStaleDepListID = target.ID
	backing.afterStaleDepList = func() {
		if err := mem.DepAdd(target.ID, blocker.ID, "blocks"); err != nil {
			t.Errorf("DepAdd: %v", err)
			return
		}
		payload, err := json.Marshal(target)
		if err != nil {
			t.Errorf("Marshal: %v", err)
			return
		}
		cs.ApplyEvent("bead.updated", payload)
	}

	cs.runReconciliation()

	if ready, ok := cs.CachedReady(); ok {
		t.Fatalf("CachedReady answered from stale dependency cache after concurrent invalidation: %v", ready)
	}
	ready, err := cs.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	for _, bead := range ready {
		if bead.ID == target.ID {
			t.Fatalf("Ready includes %s after backing dependency add; ready=%v", target.ID, ready)
		}
	}
}

func TestCachingStoreReconciliationSkipsReemitForAlreadyClosedBead(t *testing.T) {
	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "to be closed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var events []string
	cs := NewCachingStoreForTest(mem, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cs.Close(bead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wantClose := "bead.closed:" + bead.ID
	closeSeen := false
	for _, e := range events {
		if e == wantClose {
			closeSeen = true
			break
		}
	}
	if !closeSeen {
		t.Fatalf("events after Close = %v, want to include %q", events, wantClose)
	}
	events = nil

	cs.runReconciliation()

	for _, e := range events {
		if strings.HasPrefix(e, "bead.closed:") {
			t.Fatalf("reconciliation re-emitted close event: %v", events)
		}
	}

	cs.mu.RLock()
	_, stillCached := cs.beads[bead.ID]
	cs.mu.RUnlock()
	if stillCached {
		t.Fatalf("closed bead %s should be evicted from cache after reconcile", bead.ID)
	}
}

func TestCachingStoreReconciliationSkipsReemitForAlreadyClosedBeadWithConcurrentMutation(t *testing.T) {
	mem := NewMemStore()
	closedBead, err := mem.Create(Bead{Title: "closed before reconcile"})
	if err != nil {
		t.Fatalf("Create(closed): %v", err)
	}
	other, err := mem.Create(Bead{Title: "concurrent target"})
	if err != nil {
		t.Fatalf("Create(other): %v", err)
	}

	backing := &reconcileRaceStore{
		Store:   mem,
		started: make(chan struct{}),
		release: make(chan struct{}),
		stale:   []Bead{other},
	}

	var events []string
	var eventsMu sync.Mutex
	cs := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, eventType+":"+beadID)
	})
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cs.Close(closedBead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	eventsMu.Lock()
	events = nil
	eventsMu.Unlock()

	backing.mu.Lock()
	backing.block = true
	backing.mu.Unlock()

	done := make(chan struct{})
	go func() {
		cs.runReconciliation()
		close(done)
	}()

	<-backing.started
	title := "after concurrent update"
	if err := cs.Update(other.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update(other): %v", err)
	}
	close(backing.release)
	<-done

	eventsMu.Lock()
	defer eventsMu.Unlock()
	for _, e := range events {
		if strings.HasPrefix(e, "bead.closed:") {
			t.Fatalf("reconciliation re-emitted close event in race path: %v", events)
		}
	}

	cs.mu.RLock()
	_, stillCached := cs.beads[closedBead.ID]
	cs.mu.RUnlock()
	if stillCached {
		t.Fatalf("closed bead %s should be evicted from cache after reconcile", closedBead.ID)
	}
}

func TestCachingStoreReconciliationMergesFreshDataWithConcurrentMutation(t *testing.T) {
	mem := NewMemStore()
	mutated, err := mem.Create(Bead{Title: "before mutate"})
	if err != nil {
		t.Fatalf("Create(mutated): %v", err)
	}
	refreshed, err := mem.Create(Bead{Title: "before refresh"})
	if err != nil {
		t.Fatalf("Create(refreshed): %v", err)
	}

	backing := &reconcileRaceStore{
		Store:   mem,
		started: make(chan struct{}),
		release: make(chan struct{}),
		stale:   []Bead{mutated, refreshed},
	}
	cs := NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.mu.Lock()
	backing.block = true
	backing.mu.Unlock()

	done := make(chan struct{})
	go func() {
		cs.runReconciliation()
		close(done)
	}()

	<-backing.started
	title := "after concurrent update"
	if err := cs.Update(mutated.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update(mutated): %v", err)
	}
	refreshedTitle := "after reconcile refresh"
	if err := mem.Update(refreshed.ID, UpdateOpts{Title: &refreshedTitle}); err != nil {
		t.Fatalf("Update(refreshed backing): %v", err)
	}
	refreshedBead, err := mem.Get(refreshed.ID)
	if err != nil {
		t.Fatalf("Get(refreshed backing): %v", err)
	}
	backing.mu.Lock()
	backing.stale = []Bead{
		cloneBead(mutated),
		cloneBead(refreshedBead),
	}
	backing.mu.Unlock()
	close(backing.release)
	<-done

	items, err := cs.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	gotTitles := map[string]string{}
	for _, item := range items {
		gotTitles[item.ID] = item.Title
	}
	if gotTitles[mutated.ID] != title {
		t.Fatalf("mutated title = %q, want %q", gotTitles[mutated.ID], title)
	}
	if gotTitles[refreshed.ID] != refreshedTitle {
		t.Fatalf("refreshed title = %q, want %q", gotTitles[refreshed.ID], refreshedTitle)
	}
}

// TestRunReconciliationLogsSuccess asserts the per-reconcile success log
// line surfaces a heartbeat after the cache refreshes. Before this line
// existed, a reconciler running silently on stale data produced no
// operator-visible signal — the T7920 incident 2026-05-26 went undetected
// for 2h 31m.
func TestRunReconciliationLogsSuccess(t *testing.T) {
	logBuf := captureLog(t)

	mem := NewMemStore()
	if _, err := mem.Create(Bead{Title: "heartbeat target"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cs := NewCachingStoreForTestWithPrefix(mem, "test-rig", nil)
	if err := cs.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	cs.runReconciliation()

	out := logBuf.String()
	if !strings.Contains(out, "beads cache: reconciled") {
		t.Fatalf("expected reconcile success line, got:\n%s", out)
	}
	if !strings.Contains(out, "rig=test-rig") {
		t.Errorf("missing rig identity in log; out=%q", out)
	}
	for _, want := range []string{"beads=", "adds=", "updates=", "removes=", "took=", "cadence="} {
		if !strings.Contains(out, want) {
			t.Errorf("missing field %q in log; out=%q", want, out)
		}
	}
}

// TestRunReconciliationLogRateLimited asserts the success log line is
// rate-limited to cacheReconcileSuccessLogWindow (one minute). Two
// back-to-back reconciles emit exactly one line.
func TestRunReconciliationLogRateLimited(t *testing.T) {
	logBuf := captureLog(t)

	mem := NewMemStore()
	cs := NewCachingStoreForTestWithPrefix(mem, "test-rig", nil)
	if err := cs.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	cs.runReconciliation()
	cs.runReconciliation()
	cs.runReconciliation()

	out := logBuf.String()
	count := strings.Count(out, "beads cache: reconciled")
	if count != 1 {
		t.Errorf("expected 1 reconciled line within rate-limit window, got %d:\n%s", count, out)
	}
}

// TestRunReconciliationLogEmitsAgainAfterWindow asserts the success log
// line is re-emitted once the rate-limit window has elapsed. The test
// reaches into lastReconcileLogAt to advance the simulated clock without
// sleeping a real minute.
func TestRunReconciliationLogEmitsAgainAfterWindow(t *testing.T) {
	logBuf := captureLog(t)

	mem := NewMemStore()
	cs := NewCachingStoreForTestWithPrefix(mem, "test-rig", nil)
	if err := cs.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	cs.runReconciliation()

	// Backdate the rate-limit gate beyond the window so the next emit fires.
	cs.mu.Lock()
	cs.lastReconcileLogAt = cs.lastReconcileLogAt.Add(-2 * cacheReconcileSuccessLogWindow)
	cs.mu.Unlock()

	cs.runReconciliation()

	out := logBuf.String()
	count := strings.Count(out, "beads cache: reconciled")
	if count != 2 {
		t.Errorf("expected 2 reconciled lines after window elapsed, got %d:\n%s", count, out)
	}
}

// failingScanStore fails full-scan List calls (the Prime path) while
// letting status-filtered List calls (the PrimeActive path) through, so
// tests can model a store whose initial full prime fails.
type failingScanStore struct {
	Store

	mu       sync.Mutex
	failScan bool
}

func (s *failingScanStore) setFailScan(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failScan = fail
}

func (s *failingScanStore) List(query ListQuery) ([]Bead, error) {
	if query.AllowScan {
		s.mu.Lock()
		fail := s.failScan
		s.mu.Unlock()
		if fail {
			return nil, errors.New("full scan unavailable")
		}
	}
	return s.Store.List(query)
}

// TestRunReconciliation_CircuitTripLogs_OnLiveToDegraded guards that the
// first live→cacheDegraded transition emits exactly one "circuit-breaker
// tripped" message, and that subsequent reconciliations in the degraded
// window do not re-emit it.
func TestRunReconciliation_CircuitTripLogs_OnLiveToDegraded(t *testing.T) {
	backing := &failingScanStore{Store: NewMemStore()}
	backing.setFailScan(true)
	cs := NewCachingStoreForTest(backing, nil)
	cs.state = cacheLive

	var logMu sync.Mutex
	var logLines []string
	cs.problemf = func(msg string) {
		logMu.Lock()
		logLines = append(logLines, msg)
		logMu.Unlock()
	}

	// Drive syncFailures to maxCacheSyncFailures to trigger the live→degraded transition.
	for i := 0; i < maxCacheSyncFailures; i++ {
		cs.runReconciliation()
	}

	if cs.state != cacheDegraded {
		t.Fatalf("state = %v, want cacheDegraded after %d failures", cs.state, maxCacheSyncFailures)
	}

	logMu.Lock()
	lines := append([]string(nil), logLines...)
	logMu.Unlock()

	tripCount := 0
	for _, l := range lines {
		if strings.Contains(l, "circuit-breaker tripped") {
			tripCount++
		}
	}
	if tripCount != 1 {
		t.Fatalf("expected exactly one 'circuit-breaker tripped' log on the live→degraded transition, got %d", tripCount)
	}

	// Subsequent reconciliations in the degraded window must NOT re-emit the trip.
	logMu.Lock()
	logLines = logLines[:0]
	logMu.Unlock()

	cs.runReconciliation()

	logMu.Lock()
	lines = append([]string(nil), logLines...)
	logMu.Unlock()

	for _, l := range lines {
		if strings.Contains(l, "circuit-breaker tripped") {
			t.Fatalf("circuit-breaker trip re-emitted on second degraded reconcile; want exactly once per live→degraded transition")
		}
	}
}

// TestRunReconciliation_CircuitTripReArmsAfterReconcileRecovery guards that the
// one-shot breaker signal re-arms when a degraded store recovers via the
// reconcile path (not just prime): trip → reconcile-recover → re-degrade must
// fire the trip log a SECOND time. Without the circuitTripped reset in
// promoteLiveLocked, a flapping store emits the signal at most once per process.
func TestRunReconciliation_CircuitTripReArmsAfterReconcileRecovery(t *testing.T) {
	backing := &failingScanStore{Store: NewMemStore()}
	backing.setFailScan(true)
	cs := NewCachingStoreForTest(backing, nil)
	cs.state = cacheLive

	var logMu sync.Mutex
	var logLines []string
	cs.problemf = func(msg string) {
		logMu.Lock()
		logLines = append(logLines, msg)
		logMu.Unlock()
	}
	tripCount := func() int {
		logMu.Lock()
		defer logMu.Unlock()
		n := 0
		for _, l := range logLines {
			if strings.Contains(l, "circuit-breaker tripped") {
				n++
			}
		}
		return n
	}

	// 1. Trip: drive live→degraded; the breaker fires once.
	for i := 0; i < maxCacheSyncFailures; i++ {
		cs.runReconciliation()
	}
	if cs.state != cacheDegraded {
		t.Fatalf("state = %v, want cacheDegraded after the first failure run", cs.state)
	}
	if got := tripCount(); got != 1 {
		t.Fatalf("trip count after first degrade = %d, want 1", got)
	}

	// 2. Recover via reconcile: a clean scan promotes degraded→live through
	//    promoteLiveLocked, which must re-arm the breaker.
	backing.setFailScan(false)
	cs.runReconciliation()
	if cs.state != cacheLive {
		t.Fatalf("state = %v, want cacheLive after the recovery reconcile", cs.state)
	}

	// 3. Re-degrade: the breaker must fire AGAIN, proving it re-armed on the
	//    reconcile recovery rather than staying latched from the first trip.
	backing.setFailScan(true)
	for i := 0; i < maxCacheSyncFailures; i++ {
		cs.runReconciliation()
	}
	if cs.state != cacheDegraded {
		t.Fatalf("state = %v, want cacheDegraded after the re-degrade run", cs.state)
	}
	if got := tripCount(); got != 2 {
		t.Fatalf("trip count after recover→re-trip = %d, want 2 (breaker must re-arm on reconcile recovery)", got)
	}
}

// TestRunReconciliationPromotesPartialCacheToLive asserts that a clean
// full-scan reconciliation promotes a PrimeActive-only (cachePartial)
// cache to live. A reconcile loads the same complete active snapshot a
// successful Prime would, so a store whose initial full prime failed must
// converge to live through reconciliation instead of serving its
// PrimeActive-era snapshot indefinitely.
func TestRunReconciliationPromotesPartialCacheToLive(t *testing.T) {
	mem := NewMemStore()
	primed, err := mem.Create(Bead{Title: "present at prime-active"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cs := NewCachingStoreForTest(mem, nil)
	if err := cs.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	if cs.IsLive() {
		t.Fatal("cache live after PrimeActive alone, want partial")
	}

	// A bead created behind the cache's back (no event delivered) models
	// storage-level state the partial snapshot missed.
	missed, err := mem.Create(Bead{Title: "missed by prime-active"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cs.runReconciliation()

	if !cs.IsLive() {
		t.Fatal("cache not live after clean reconcile, want promoted to live")
	}
	got, ok := cs.CachedReady()
	if !ok {
		t.Fatal("CachedReady not servable after reconcile promotion")
	}
	ids := make(map[string]bool, len(got))
	for _, b := range got {
		ids[b.ID] = true
	}
	if !ids[primed.ID] || !ids[missed.ID] {
		t.Fatalf("CachedReady = %v, want both %s and %s", ids, primed.ID, missed.ID)
	}
}

// TestRunReconciliationPromotesUnprimedCacheToLive asserts reconciliation
// also converges a cache whose PrimeActive never succeeded
// (cacheUninitialized), mirroring Prime's unconditional promotion.
func TestRunReconciliationPromotesUnprimedCacheToLive(t *testing.T) {
	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "storage-level work"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cs := NewCachingStoreForTest(mem, nil)

	cs.runReconciliation()

	if !cs.IsLive() {
		t.Fatal("cache not live after clean reconcile from uninitialized state")
	}
	got, ok := cs.CachedReady()
	if !ok {
		t.Fatal("CachedReady not servable after reconcile promotion")
	}
	if len(got) != 1 || got[0].ID != bead.ID {
		t.Fatalf("CachedReady = %#v, want only %s", got, bead.ID)
	}
}

// TestRunReconciliationDoesNotPromoteOnFailure asserts a failed reconcile
// leaves a partial cache partial — promotion requires a clean full scan.
func TestRunReconciliationDoesNotPromoteOnFailure(t *testing.T) {
	mem := NewMemStore()
	if _, err := mem.Create(Bead{Title: "present at prime-active"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &failingScanStore{Store: mem}
	cs := NewCachingStoreForTest(backing, nil)
	if err := cs.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	backing.setFailScan(true)

	cs.runReconciliation()

	if cs.IsLive() {
		t.Fatal("cache promoted to live by a FAILED reconcile")
	}
}

// TestPrimeFailureThenReconcileConverges is the end-to-end shape of the
// recovery path: PrimeActive succeeds, the full Prime fails, and a later
// clean reconciliation converges the cache to storage and promotes it
// live so cached readers stop falling back.
func TestPrimeFailureThenReconcileConverges(t *testing.T) {
	mem := NewMemStore()
	if _, err := mem.Create(Bead{Title: "present at prime-active"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &failingScanStore{Store: mem, failScan: true}
	cs := NewCachingStoreForTest(backing, nil)
	cs.primeRetryDelay = func(int) time.Duration { return 0 }
	if err := cs.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	if err := cs.Prime(context.Background()); err == nil {
		t.Fatal("Prime succeeded against failing scan store, want error")
	}

	missed, err := mem.Create(Bead{Title: "created while prime was failing"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	backing.setFailScan(false)
	cs.runReconciliation()

	if !cs.IsLive() {
		t.Fatal("cache not live after reconcile recovered from failed prime")
	}
	got, err := cs.cachedReadyOnly(ReadyQuery{TierMode: TierBoth})
	if err != nil {
		t.Fatalf("cachedReadyOnly: %v", err)
	}
	found := false
	for _, b := range got {
		if b.ID == missed.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("cachedReadyOnly = %#v, want to include %s", got, missed.ID)
	}
}
