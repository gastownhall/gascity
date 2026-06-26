package beads

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// queryRecordingStore wraps a Store and records every ListQuery it serves so
// tests can assert the exact scan scope the cache requests from the backing
// store.
type queryRecordingStore struct {
	Store

	mu      sync.Mutex
	queries []ListQuery
}

func (s *queryRecordingStore) List(query ListQuery) ([]Bead, error) {
	s.mu.Lock()
	s.queries = append(s.queries, query)
	s.mu.Unlock()
	return s.Store.List(query)
}

func (s *queryRecordingStore) recorded() []ListQuery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ListQuery(nil), s.queries...)
}

func (s *queryRecordingStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = nil
}

// TestCacheFullScanQueryShape pins the exact shape of the full-scan query the
// reconciler and Prime issue. The reconcile diff treats the result as the
// complete active universe, so Limit must stay unset and closed beads must
// stay excluded; see cacheFullScanQuery for the full rationale.
func TestCacheFullScanQueryShape(t *testing.T) {
	q := cacheFullScanQuery()
	if !q.AllowScan {
		t.Error("AllowScan = false, want true (full scan is intentional)")
	}
	if !q.SkipLabels {
		t.Error("SkipLabels = false, want true (reconciler is label-blind)")
	}
	if q.TierMode != TierBoth {
		t.Errorf("TierMode = %v, want TierBoth", q.TierMode)
	}
	if q.IncludeClosed {
		t.Error("IncludeClosed = true, want false (scan is O(active beads) by design)")
	}
	if q.Status != "" {
		t.Errorf("Status = %q, want empty (status filters would shrink the authoritative set)", q.Status)
	}
	if q.IncludesClosed() {
		t.Error("IncludesClosed() = true, want false")
	}
	if q.Limit != 0 {
		t.Errorf("Limit = %d, want 0 (a partial list would cause false evictions)", q.Limit)
	}
}

// TestReconcileScanNeverRequestsClosedBeads asserts the reconcile cycle never
// asks the backing store for closed beads. The reconcile full scan is bounded
// at O(active beads); on a store carrying as much closed history as active
// work, an IncludeClosed scan would multiply the per-cycle bd payload without
// changing the diff result.
func TestReconcileScanNeverRequestsClosedBeads(t *testing.T) {
	mem := NewMemStore()
	open, err := mem.Create(Bead{Title: "active"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	closed, err := mem.Create(Bead{Title: "history"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mem.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	backing := &queryRecordingStore{Store: mem}
	cs := NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	backing.reset()

	cs.runReconciliation()

	queries := backing.recorded()
	scans := 0
	for _, q := range queries {
		if q.IncludesClosed() {
			t.Errorf("reconcile issued a query including closed beads: %+v", q)
		}
		if q.AllowScan {
			scans++
			if q.Limit != 0 {
				t.Errorf("reconcile full scan set Limit = %d, want 0 (result is the authoritative active set)", q.Limit)
			}
		}
	}
	if scans == 0 {
		t.Fatalf("reconcile issued no full scan; queries = %+v", queries)
	}

	// The active-only scan still converges the cache: the open bead is
	// cached, the closed one is not.
	cs.mu.RLock()
	_, openCached := cs.beads[open.ID]
	_, closedCached := cs.beads[closed.ID]
	cs.mu.RUnlock()
	if !openCached {
		t.Errorf("open bead %s missing from cache after reconcile", open.ID)
	}
	if closedCached {
		t.Errorf("closed bead %s cached after reconcile, want excluded", closed.ID)
	}
}

// TestPrimeScanNeverRequestsClosedBeads asserts the full Prime path shares
// the same pinned active-only scan scope as the reconciler.
func TestPrimeScanNeverRequestsClosedBeads(t *testing.T) {
	mem := NewMemStore()
	closed, err := mem.Create(Bead{Title: "history"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mem.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	backing := &queryRecordingStore{Store: mem}
	cs := NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	queries := backing.recorded()
	scans := 0
	for _, q := range queries {
		if q.IncludesClosed() {
			t.Errorf("prime issued a query including closed beads: %+v", q)
		}
		if q.AllowScan {
			scans++
			if q.Limit != 0 {
				t.Errorf("prime full scan set Limit = %d, want 0", q.Limit)
			}
		}
	}
	if scans == 0 {
		t.Fatalf("prime issued no full scan; queries = %+v", queries)
	}
}

// captureCacheScanTelemetry intercepts the reconcile scan-size telemetry seam
// for the duration of the test and returns the recorded calls.
type cacheScanTelemetryCall struct {
	rig       string
	beadCount int
	threshold int
	elapsed   time.Duration
}

func captureCacheScanTelemetry(t *testing.T) *[]cacheScanTelemetryCall {
	t.Helper()
	var calls []cacheScanTelemetryCall
	prev := recordCacheScanLarge
	recordCacheScanLarge = func(_ context.Context, rig string, beadCount, threshold int, elapsed time.Duration) {
		calls = append(calls, cacheScanTelemetryCall{rig: rig, beadCount: beadCount, threshold: threshold, elapsed: elapsed})
	}
	t.Cleanup(func() { recordCacheScanLarge = prev })
	return &calls
}

// TestReconcileEmitsScanSizeTelemetryOverThreshold asserts a reconcile scan
// at or above cacheReconcileScanWarnThreshold emits the scan-size telemetry,
// making O(active beads) growth visible instead of silent (ga-698fl2: a dev
// store reached 3,272 active beads / ~11MB of JSON per scan unnoticed).
func TestReconcileEmitsScanSizeTelemetryOverThreshold(t *testing.T) {
	calls := captureCacheScanTelemetry(t)

	mem := NewMemStore()
	for i := 0; i < cacheReconcileScanWarnThreshold; i++ {
		if _, err := mem.Create(Bead{Title: fmt.Sprintf("bead %d", i)}); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	cs := NewCachingStoreForTestWithPrefix(mem, "test-rig", nil)

	cs.runReconciliation()

	if len(*calls) != 1 {
		t.Fatalf("scan telemetry calls = %d, want 1", len(*calls))
	}
	got := (*calls)[0]
	if got.beadCount != cacheReconcileScanWarnThreshold {
		t.Errorf("beadCount = %d, want %d", got.beadCount, cacheReconcileScanWarnThreshold)
	}
	if got.threshold != cacheReconcileScanWarnThreshold {
		t.Errorf("threshold = %d, want %d", got.threshold, cacheReconcileScanWarnThreshold)
	}
	if got.rig != "test-rig" {
		t.Errorf("rig = %q, want %q", got.rig, "test-rig")
	}
	if got.elapsed < 0 {
		t.Errorf("elapsed = %v, want >= 0", got.elapsed)
	}
}

// TestReconcileSkipsScanSizeTelemetryUnderThreshold asserts a small scan emits
// no scan-size telemetry, so healthy stores stay quiet.
func TestReconcileSkipsScanSizeTelemetryUnderThreshold(t *testing.T) {
	calls := captureCacheScanTelemetry(t)

	mem := NewMemStore()
	if _, err := mem.Create(Bead{Title: "small store"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cs := NewCachingStoreForTest(mem, nil)

	cs.runReconciliation()

	if len(*calls) != 0 {
		t.Fatalf("scan telemetry calls = %d, want 0; calls = %+v", len(*calls), *calls)
	}
}
