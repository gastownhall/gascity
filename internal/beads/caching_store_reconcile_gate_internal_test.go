package beads

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type listCountingStore struct {
	*MemStore
	lists atomic.Int64
}

func (s *listCountingStore) List(q ListQuery) ([]Bead, error) {
	s.lists.Add(1)
	return s.MemStore.List(q)
}

// A closed reconcile gate skips the periodic full scan without stopping the
// loop; reopening it lets the next due cycle run.
func TestReconcileGateSkipsTheFullScanWhileClosed(t *testing.T) {
	backing := &listCountingStore{MemStore: NewMemStore()}
	var allowed atomic.Bool
	cs := NewCachingStoreForTest(backing, nil)
	WithReconcileGate(allowed.Load)(cs)

	// Never primed, so a reconcile is due.
	cs.reconcileIfDue(time.Now())
	if got := backing.lists.Load(); got != 0 {
		t.Fatalf("closed gate: backing List called %d time(s), want 0", got)
	}

	allowed.Store(true)
	cs.reconcileIfDue(time.Now())
	if got := backing.lists.Load(); got == 0 {
		t.Fatal("open gate: the due reconcile did not scan the backing store")
	}
}

// hookedListStore runs onList inside each List, after the backing read, and
// fails the List while fail is set.
type hookedListStore struct {
	*MemStore
	lists  atomic.Int64
	fail   atomic.Bool
	onList func()
}

func (s *hookedListStore) List(q ListQuery) ([]Bead, error) {
	s.lists.Add(1)
	if s.fail.Load() {
		return nil, errors.New("list failed")
	}
	out, err := s.MemStore.List(q)
	if s.onList != nil {
		s.onList()
	}
	return out, err
}

func primedRequestCache(t *testing.T, backing Store, opts ...CachingStoreOption) *CachingStore {
	t.Helper()
	cs := NewCachingStoreForTest(backing, nil, opts...)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return cs
}

// A request makes a freshly scanned cache due once: the run it causes clears
// it, and the cache is not due again until its cadence says so.
func TestRequestReconcileMakesAFreshCacheDueOnce(t *testing.T) {
	backing := &hookedListStore{MemStore: NewMemStore()}
	cs := primedRequestCache(t, backing)
	now := time.Now()
	if d := cs.nextReconcileDelay(now); d <= 0 {
		t.Fatalf("fresh cache delay = %v, want > 0", d)
	}
	cs.RequestReconcile()
	if d := cs.nextReconcileDelay(now); d != 0 {
		t.Fatalf("requested cache delay = %v, want 0", d)
	}
	before := backing.lists.Load()
	cs.reconcileIfDue(now)
	if got := backing.lists.Load() - before; got != 1 {
		t.Fatalf("requested reconcile scanned %d time(s), want 1", got)
	}
	if d := cs.nextReconcileDelay(time.Now()); d <= 0 {
		t.Fatalf("delay after the requested run = %v, want > 0", d)
	}
	cs.reconcileIfDue(time.Now())
	if got := backing.lists.Load() - before; got != 1 {
		t.Fatalf("a second due check scanned again (%d scans), want the request honored once", got)
	}
	if got := cs.Stats().ReconcileRequests; got != 1 {
		t.Fatalf("ReconcileRequests = %d, want 1", got)
	}
}

// A closed gate holds a request pending until it opens.
func TestRequestReconcileWaitsForTheGate(t *testing.T) {
	backing := &hookedListStore{MemStore: NewMemStore()}
	var allowed atomic.Bool
	cs := primedRequestCache(t, backing, WithReconcileGate(allowed.Load))
	before := backing.lists.Load()
	cs.RequestReconcile()
	cs.reconcileIfDue(time.Now())
	if got := backing.lists.Load() - before; got != 0 {
		t.Fatalf("closed gate: %d scan(s), want 0", got)
	}
	allowed.Store(true)
	cs.reconcileIfDue(time.Now())
	if got := backing.lists.Load() - before; got != 1 {
		t.Fatalf("opened gate: %d scan(s), want the pending request honored once", got)
	}
}

// A request made while a run is listing gets a run of its own, whose list
// read comes after the request.
func TestRequestReconcileDuringARunGetsAnotherRun(t *testing.T) {
	backing := &hookedListStore{MemStore: NewMemStore()}
	cs := primedRequestCache(t, backing)
	var once sync.Once
	backing.onList = func() { once.Do(cs.RequestReconcile) }
	cs.RequestReconcile()
	cs.reconcileIfDue(time.Now())
	if d := cs.nextReconcileDelay(time.Now()); d != 0 {
		t.Fatalf("delay after a run that took a request mid-list = %v, want 0", d)
	}
}

// fullScanHookStore runs onList inside each full-scan List, after its read.
type fullScanHookStore struct {
	*MemStore
	onList func()
}

func (s *fullScanHookStore) List(q ListQuery) ([]Bead, error) {
	out, err := s.MemStore.List(q)
	if q.AllowScan && q.SkipLabels && !q.IncludeClosed && q.Status == "" && s.onList != nil {
		s.onList()
	}
	return out, err
}

// A requested run that discards its merge, because a full Prime that listed
// before the request merged first, re-arms the request: the run whose list
// read comes after the request is still owed.
func TestRequestReconcileSurvivesARunThatDiscardsItsMerge(t *testing.T) {
	backing := &fullScanHookStore{MemStore: NewMemStore()}
	cs := NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	var calls atomic.Int32
	primeListed := make(chan struct{})
	reconcileListed := make(chan struct{})
	primeDone := make(chan struct{})
	backing.onList = func() {
		switch calls.Add(1) {
		case 1: // the full Prime's list
			close(primeListed)
			<-reconcileListed
		case 2: // the requested reconcile's list
			close(reconcileListed)
			<-primeDone
		}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := cs.Prime(context.Background()); err != nil {
			t.Errorf("second Prime: %v", err)
		}
	}()
	<-primeListed
	// A write the Prime's list missed, committed before the request.
	row, err := backing.Create(Bead{Title: "committed before the request"})
	if err != nil {
		t.Fatal(err)
	}
	cs.RequestReconcile()
	done := make(chan struct{})
	go func() {
		cs.reconcileIfDue(time.Now())
		close(done)
	}()
	<-reconcileListed
	wg.Wait() // the Prime merged first
	close(primeDone)
	<-done
	backing.onList = nil
	if d := cs.nextReconcileDelay(time.Now()); d != 0 {
		t.Fatalf("delay after a requested run discarded its merge = %v, want the request still due", d)
	}
	cs.reconcileIfDue(time.Now())
	if _, err := cs.Get(row.ID); err != nil {
		t.Fatalf("the re-armed run did not cache the row: %v", err)
	}
}

// Before the first reconcile, the backstop is due an interval after the
// Prime's (or PrimeActive's, as the controller boots) scan: refreshes that
// change rows (a journal feed's steady out-of-band activity) mark the cache
// fresh but do not postpone it.
func TestFirstReconcileIsDueAnIntervalAfterThePrime(t *testing.T) {
	for _, tc := range []struct {
		name  string
		prime func(cs *CachingStore) error
	}{
		{name: "prime", prime: func(cs *CachingStore) error { return cs.Prime(context.Background()) }},
		{name: "prime_active", prime: func(cs *CachingStore) error { return cs.PrimeActive() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			now := time.Unix(1_000_000, 0)
			clock := func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return now
			}
			advance := func(d time.Duration) {
				mu.Lock()
				defer mu.Unlock()
				now = now.Add(d)
			}
			backing := NewMemStore()
			cs := NewCachingStoreForTest(backing, nil, WithClock(clock))
			if err := tc.prime(cs); err != nil {
				t.Fatalf("prime: %v", err)
			}
			primedAt := clock()
			cs.mu.RLock()
			interval := cs.adaptiveIntervalLocked()
			cs.mu.RUnlock()
			for elapsed := time.Duration(0); elapsed < 2*interval; elapsed += interval / 4 {
				advance(interval / 4)
				row, err := backing.Create(Bead{Title: "out of band"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := cs.RefreshRow(row.ID); err != nil {
					t.Fatalf("RefreshRow: %v", err)
				}
			}
			if d := cs.nextReconcileDelay(clock()); d != 0 {
				t.Fatalf("delay %v after the prime at %v with refreshes since = %v, want due", clock().Sub(primedAt), primedAt, d)
			}
		})
	}
}

// A request never bypasses the failure backoff of a failing backing.
func TestRequestReconcileKeepsTheFailureBackoff(t *testing.T) {
	backing := &hookedListStore{MemStore: NewMemStore()}
	cs := primedRequestCache(t, backing)
	backing.fail.Store(true)
	cs.ReconcileNowForTest()
	cs.RequestReconcile()
	if d := cs.nextReconcileDelay(time.Now()); d <= 0 {
		t.Fatalf("delay after a failed scan and a request = %v, want the backoff", d)
	}
}

// A running loop honors a request at once, well inside its 5s poll.
func TestRequestReconcileWakesTheRunningLoop(t *testing.T) {
	backing := &hookedListStore{MemStore: NewMemStore()}
	cs := primedRequestCache(t, backing)
	cs.StartReconciler(context.Background(), WithStaggerOff(), "test")
	defer cs.StopReconciler()
	before := backing.lists.Load()
	cs.RequestReconcile()
	deadline := time.After(time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for backing.lists.Load() == before {
		select {
		case <-deadline:
			t.Fatal("the loop did not scan within 1s of a request")
		case <-tick.C:
		}
	}
}

// Stopped closes when the cache's background work is stopped.
func TestCachingStoreStoppedClosesOnStopReconciler(t *testing.T) {
	cs := NewCachingStoreForTest(NewMemStore(), nil)
	select {
	case <-cs.Stopped():
		t.Fatal("Stopped closed before StopReconciler")
	default:
	}
	cs.StopReconciler()
	select {
	case <-cs.Stopped():
	case <-time.After(time.Second):
		t.Fatal("Stopped did not close on StopReconciler")
	}
}

// OwnsBeadID is the filter ApplyEvent applies.
func TestCachingStoreOwnsBeadIDMatchesTheEventFilter(t *testing.T) {
	plain := NewCachingStoreForTest(NewMemStore(), nil)
	prefixed := NewCachingStoreForTestWithPrefix(NewMemStore(), "gc", nil)
	multi := NewCachingStoreForTest(NewMemStore(), nil, WithEventIDPrefixes("gc", "nq"))
	for _, cs := range []*CachingStore{plain, prefixed, multi} {
		for _, id := range []string{"gc-1", " GC-2 ", "nq-3", "other-4", ""} {
			if got, want := cs.OwnsBeadID(id), cs.ownsBeadID(id); got != want {
				t.Fatalf("OwnsBeadID(%q) = %v, the event filter says %v", id, got, want)
			}
		}
	}
	if !prefixed.OwnsBeadID("gc-1") || prefixed.OwnsBeadID("other-4") {
		t.Fatal("a prefixed cache must own exactly its namespace")
	}
	if !multi.OwnsBeadID("nq-3") || multi.OwnsBeadID("other-4") {
		t.Fatal("an event-prefixed cache must own exactly its namespaces")
	}
}
