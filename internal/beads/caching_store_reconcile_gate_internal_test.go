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
