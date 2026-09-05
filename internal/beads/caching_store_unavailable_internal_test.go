package beads

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// callCountingStore counts backing List/Get calls so tests can assert the
// breaker-open path performs zero backing operations.
type callCountingStore struct {
	Store
	mu    sync.Mutex
	lists int
	gets  int
}

func (s *callCountingStore) List(query ListQuery) ([]Bead, error) {
	s.mu.Lock()
	s.lists++
	s.mu.Unlock()
	return s.Store.List(query)
}

func (s *callCountingStore) Get(id string) (Bead, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	return s.Store.Get(id)
}

func (s *callCountingStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists, s.gets
}

// counterCountingStore is a callCountingStore that also satisfies Counter, so
// tests can separate "the gate refuses to dial" from "the backing could never
// count in the first place".
type counterCountingStore struct {
	*callCountingStore
}

func (s *counterCountingStore) Count(_ context.Context, query ListQuery, excludeTypes ...string) (int, error) {
	items, err := s.List(query)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, b := range items {
		if !slices.Contains(excludeTypes, b.Type) {
			n++
		}
	}
	return n, nil
}

func newCountedGatedCache(t *testing.T, beadsIn ...Bead) (*CachingStore, *callCountingStore, *fakeAvailabilityGate) {
	t.Helper()
	counting := &callCountingStore{Store: NewMemStore()}
	return newGatedCacheOver(t, counting, counting, beadsIn...)
}

func newCounterGatedCache(t *testing.T, beadsIn ...Bead) (*CachingStore, *callCountingStore, *fakeAvailabilityGate) {
	t.Helper()
	counting := &callCountingStore{Store: NewMemStore()}
	return newGatedCacheOver(t, &counterCountingStore{callCountingStore: counting}, counting, beadsIn...)
}

func newGatedCacheOver(t *testing.T, backing Store, counting *callCountingStore, beadsIn ...Bead) (*CachingStore, *callCountingStore, *fakeAvailabilityGate) {
	t.Helper()
	for _, b := range beadsIn {
		if _, err := backing.Create(b); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	gate := &fakeAvailabilityGate{available: true}
	cache.SetAvailabilityGate(gate)
	return cache, counting, gate
}

func TestCachingStoreListUnavailableServesLastGoodCache(t *testing.T) {
	t.Parallel()
	cache, backing, gate := newCountedGatedCache(t, Bead{Title: "task-1"}, Bead{Title: "task-2"})

	gate.set(false, false)
	listsBefore, _ := backing.counts()

	got, err := cache.List(ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List under open breaker: %v, want last-good cache", err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d beads, want 2 from last-good cache", len(got))
	}
	listsAfter, _ := backing.counts()
	if listsAfter != listsBefore {
		t.Fatalf("backing List called %d times under open breaker, want 0", listsAfter-listsBefore)
	}
	if !cache.Degraded() {
		t.Fatal("Degraded() = false while serving under an open breaker, want true")
	}
	if got := cache.Stats().DegradedReads; got == 0 {
		t.Fatal("Stats().DegradedReads = 0 after a degraded read, want > 0")
	}
}

// TestCachingStoreListUnavailableLiveQueryRefusesStaleAnswer pins the shape
// the snapshot must NOT answer. ListQuery.Live's documented contract is "must
// observe external mutations immediately", and a lifecycle gate that treats
// absence as authoritative would release a running session's pool assignment
// on a stale short list. A non-Live query in the same state proves the
// refusal is scoped, not a retreat from serving last-good.
func TestCachingStoreListUnavailableLiveQueryRefusesStaleAnswer(t *testing.T) {
	t.Parallel()
	cache, backing, gate := newCountedGatedCache(t, Bead{Title: "task-1"})

	gate.set(false, false)
	listsBefore, _ := backing.counts()
	got, err := cache.List(ListQuery{AllowScan: true, Live: true})
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("List(Live) under open breaker = (%d beads, %v), want ErrStoreUnavailable", len(got), err)
	}
	if len(got) != 0 {
		t.Fatalf("List(Live) under open breaker returned %d beads alongside the refusal, want 0", len(got))
	}
	if listsAfter, _ := backing.counts(); listsAfter != listsBefore {
		t.Fatal("Live query reached the backing store under an open breaker")
	}

	nonLive, err := cache.List(ListQuery{AllowScan: true})
	if err != nil || len(nonLive) != 1 {
		t.Fatalf("non-Live List under open breaker = (%d beads, %v), want the last-good bead — the refusal must be scoped to Live", len(nonLive), err)
	}
}

func TestCachingStoreListUnavailableClosedHistoryRefuses(t *testing.T) {
	t.Parallel()
	cache, _, gate := newCountedGatedCache(t, Bead{Title: "task-1"})
	gate.set(false, false)
	got, err := cache.List(ListQuery{AllowScan: true, Status: "closed"})
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("List(closed) under open breaker = (%d beads, %v), want ErrStoreUnavailable — the snapshot holds active beads only, so empty would read as \"none exist\"", len(got), err)
	}
}

func TestCachingStoreListUnavailableUnprimedReturnsTypedError(t *testing.T) {
	t.Parallel()
	backing := &callCountingStore{Store: NewMemStore()}
	cache := NewCachingStoreForTest(backing, nil)
	gate := &fakeAvailabilityGate{}
	cache.SetAvailabilityGate(gate)

	_, err := cache.List(ListQuery{AllowScan: true})
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("List on unprimed cache under open breaker: err = %v, want ErrStoreUnavailable (unavailable must never read as empty)", err)
	}
	if lists, _ := backing.counts(); lists != 0 {
		t.Fatalf("backing List called %d times, want 0", lists)
	}
}

func TestCachingStoreGetUnavailableServesCachedBead(t *testing.T) {
	t.Parallel()
	cache, backing, gate := newCountedGatedCache(t, Bead{Title: "task-1"})
	all, err := cache.List(ListQuery{AllowScan: true})
	if err != nil || len(all) != 1 {
		t.Fatalf("List: %v (len %d)", err, len(all))
	}

	gate.set(false, false)
	_, getsBefore := backing.counts()
	got, err := cache.Get(all[0].ID)
	if err != nil {
		t.Fatalf("Get under open breaker: %v", err)
	}
	if got.Title != "task-1" {
		t.Fatalf("Get Title = %q, want task-1", got.Title)
	}
	if _, getsAfter := backing.counts(); getsAfter != getsBefore {
		t.Fatal("Get reached the backing store under an open breaker")
	}
}

func TestCachingStoreGetUnavailableUncachedReturnsTypedError(t *testing.T) {
	t.Parallel()
	cache, _, gate := newCountedGatedCache(t, Bead{Title: "task-1"})
	gate.set(false, false)
	_, err := cache.Get("missing-id")
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Get(missing) under open breaker: err = %v, want ErrStoreUnavailable (cannot distinguish missing from unreachable)", err)
	}
}

func TestCachingStoreCountUnavailableServesSnapshotCount(t *testing.T) {
	t.Parallel()
	cache, backing, gate := newCounterGatedCache(t, Bead{Title: "task-1"}, Bead{Title: "task-2"})
	gate.set(false, false)
	listsBefore, _ := backing.counts()

	n, err := cache.Count(context.Background(), ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("Count under open breaker: %v, want the snapshot count", err)
	}
	if n != 2 {
		t.Fatalf("Count = %d, want 2 from the last-good snapshot", n)
	}
	if listsAfter, _ := backing.counts(); listsAfter != listsBefore {
		t.Fatal("Count reached the backing store under an open breaker")
	}
}

// TestCachingStoreCountUnavailableRefusesShapesTheSnapshotCannotAnswer pins
// the honesty boundary: a count carries no partial tag, so a closed-history
// shape must refuse rather than return a number that silently omits history.
func TestCachingStoreCountUnavailableRefusesShapesTheSnapshotCannotAnswer(t *testing.T) {
	t.Parallel()
	cache, _, gate := newCounterGatedCache(t, Bead{Title: "task-1"})
	gate.set(false, false)
	_, err := cache.Count(context.Background(), ListQuery{AllowScan: true, Status: "closed"})
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("Count(closed) under open breaker: err = %v, want ErrStoreUnavailable", err)
	}
}

// TestCachingStoreCountUnavailableKeepsErrCountUnsupported pins that the gate
// does not change the answer for a backing that could never count: callers
// fall back to List on ErrCountUnsupported, and turning that into
// ErrStoreUnavailable would break the fallback.
func TestCachingStoreCountUnavailableKeepsErrCountUnsupported(t *testing.T) {
	t.Parallel()
	cache, _, gate := newCountedGatedCache(t, Bead{Title: "task-1"})
	gate.set(false, false)
	_, err := cache.Count(context.Background(), ListQuery{AllowScan: true})
	if !errors.Is(err, ErrCountUnsupported) {
		t.Fatalf("Count under open breaker on a Counter-less backing: err = %v, want ErrCountUnsupported", err)
	}
}

func TestCachingStoreAvailableGateLeavesReadsUntouched(t *testing.T) {
	t.Parallel()
	cache, _, gate := newCountedGatedCache(t, Bead{Title: "task-1"})
	gate.set(true, false)
	got, err := cache.List(ListQuery{AllowScan: true})
	if err != nil || len(got) != 1 {
		t.Fatalf("List with available gate: %v (len %d)", err, len(got))
	}
	if cache.Degraded() {
		t.Fatal("Degraded() = true with an available gate and healthy cache, want false")
	}
	if got := cache.Stats().DegradedReads; got != 0 {
		t.Fatalf("Stats().DegradedReads = %d with an available gate, want 0", got)
	}
}

func TestCachingStoreNilGateLeavesReadsUntouched(t *testing.T) {
	t.Parallel()
	backing := NewMemStore()
	if _, err := backing.Create(Bead{Title: "task-1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	got, err := cache.List(ListQuery{AllowScan: true})
	if err != nil || len(got) != 1 {
		t.Fatalf("List without gate: %v (len %d)", err, len(got))
	}
	if cache.Degraded() {
		t.Fatal("Degraded() = true without a gate and with a healthy cache")
	}
}

func TestCachingStoreReconcileSkipsCycleWhileUnavailable(t *testing.T) {
	t.Parallel()
	cache, backing, gate := newCountedGatedCache(t, Bead{Title: "task-1"})
	gate.set(false, false)

	listsBefore, _ := backing.counts()
	cache.runReconciliation()
	if listsAfter, _ := backing.counts(); listsAfter != listsBefore {
		t.Fatal("runReconciliation reached the backing store under an open breaker with no probe due")
	}
}

func TestCachingStoreReconcileRunsWhenProbeDue(t *testing.T) {
	t.Parallel()
	cache, backing, gate := newCountedGatedCache(t, Bead{Title: "task-1"})
	gate.set(false, true) // open, but a recovery probe is due

	listsBefore, _ := backing.counts()
	cache.runReconciliation()
	if listsAfter, _ := backing.counts(); listsAfter == listsBefore {
		t.Fatal("runReconciliation skipped the cycle although a probe was due — the breaker could never recover")
	}
}

func TestCachingStoreReconcileSkipLogsProblemOncePerEpisode(t *testing.T) {
	t.Parallel()
	cache, _, gate := newCountedGatedCache(t, Bead{Title: "task-1"})
	gate.set(false, false)

	cache.runReconciliation()
	first := cache.Stats().ProblemCount
	if first == 0 {
		t.Fatal("ProblemCount = 0 after an unavailable-skip, want one recorded problem")
	}
	cache.runReconciliation()
	cache.runReconciliation()
	if got := cache.Stats().ProblemCount; got != first {
		t.Fatalf("ProblemCount = %d after repeated skips in one episode, want %d (emit once)", got, first)
	}

	// Recovery closes the episode; the next outage logs again.
	gate.set(true, false)
	cache.runReconciliation()
	gate.set(false, false)
	cache.runReconciliation()
	if got := cache.Stats().ProblemCount; got != first+1 {
		t.Fatalf("ProblemCount = %d after a second episode, want %d", got, first+1)
	}
}

func TestCachingStoreNextReconcileDelaySkipsWhileUnavailable(t *testing.T) {
	t.Parallel()
	cache, _, gate := newCountedGatedCache(t, Bead{Title: "task-1"})
	gate.set(false, false)
	// Force "due now" conditions, then verify the gate overrides them.
	cache.mu.Lock()
	cache.lastFreshAt = time.Time{}
	cache.mu.Unlock()
	if got := cache.nextReconcileDelay(time.Now()); got <= 0 {
		t.Fatalf("nextReconcileDelay = %v under open breaker, want a positive skip delay", got)
	}
	gate.set(false, true)
	if got := cache.nextReconcileDelay(time.Now()); got != 0 {
		t.Fatalf("nextReconcileDelay = %v with probe due, want 0 (run the probing cycle)", got)
	}
}

func TestErrStoreUnavailableIsDistinct(t *testing.T) {
	t.Parallel()
	for _, other := range []error{ErrNotFound, ErrCacheUnavailable, ErrStoreClosed} {
		if errors.Is(ErrStoreUnavailable, other) || errors.Is(other, ErrStoreUnavailable) {
			t.Fatalf("ErrStoreUnavailable must be distinct from %v", other)
		}
	}
}
