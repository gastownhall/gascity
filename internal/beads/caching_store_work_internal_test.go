package beads

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

type storeWorkTally struct {
	active atomic.Int64
	begins atomic.Int64
	ends   atomic.Int64
}

func (w *storeWorkTally) begin() func() {
	w.begins.Add(1)
	w.active.Add(1)
	return func() {
		w.active.Add(-1)
		w.ends.Add(1)
	}
}

func (w *storeWorkTally) assertReleased(t *testing.T, want int64) {
	t.Helper()
	if got := w.active.Load(); got != 0 {
		t.Errorf("active store work after return = %d, want 0", got)
	}
	if begins, ends := w.begins.Load(), w.ends.Load(); begins != want || ends != want {
		t.Errorf("begin/end calls = %d/%d, want %d/%d", begins, ends, want, want)
	}
}

// Store embedding deliberately leaves dependency reads observable instead of
// promoting MemStore's private dependency-snapshot shortcut.
type storeWorkReadSpy struct {
	Store
	onRead  func(string)
	listErr error
}

func (s *storeWorkReadSpy) Get(id string) (Bead, error) {
	s.onRead("get")
	return s.Store.Get(id)
}

func (s *storeWorkReadSpy) List(q ListQuery) ([]Bead, error) {
	s.onRead("list:" + q.Status)
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.Store.List(q)
}

func (s *storeWorkReadSpy) DepList(id, direction string) ([]Dep, error) {
	s.onRead("deps")
	return s.Store.DepList(id, direction)
}

func (s *storeWorkReadSpy) enrichReadyProjectionForCache(rows []Bead) ([]Bead, error) {
	s.onRead("projection")
	if backing, ok := s.Store.(readyProjectionEnrichmentStore); ok {
		return backing.enrichReadyProjectionForCache(rows)
	}
	return rows, nil
}

func TestStoreWorkBeginsBeforeReconcileGateAndReleasesSkippedChecks(t *testing.T) {
	for _, closed := range []bool{true, false} {
		name := "not_due"
		if closed {
			name = "closed_gate"
		}
		t.Run(name, func(t *testing.T) {
			backing := &listCountingStore{MemStore: NewMemStore()}
			now := time.Unix(1_000_000, 0)
			cs := NewCachingStoreForTest(backing, nil, WithClock(func() time.Time { return now }))
			if !closed {
				if err := cs.Prime(context.Background()); err != nil {
					t.Fatal(err)
				}
				if cs.nextReconcileDelay(now) <= 0 {
					t.Fatal("fixture: freshly primed cache is due")
				}
			}
			var work storeWorkTally
			var gates int
			WithStoreWork(work.begin)(cs)
			WithReconcileGate(func() bool {
				gates++
				if got := work.active.Load(); got != 1 {
					t.Errorf("active store work at reconcile gate = %d, want 1", got)
				}
				return !closed
			})(cs)
			before := backing.lists.Load()
			cs.reconcileIfDue(now)
			if gates != 1 {
				t.Errorf("gate calls = %d, want 1", gates)
			}
			if got := backing.lists.Load() - before; got != 0 {
				t.Errorf("skipped check made %d backing reads, want 0", got)
			}
			work.assertReleased(t, 1)
		})
	}
}

func runStoreWorkRead(cs *CachingStore, mode string) error {
	switch mode {
	case "reconcile":
		cs.RequestReconcile()
		cs.ReconcileIfDueForTest()
		return nil
	case "prime_active":
		return cs.PrimeActive()
	default:
		return cs.Prime(context.Background())
	}
}

func TestStoreWorkCoversCacheReadsUntilCompletion(t *testing.T) {
	for _, mode := range []string{"reconcile", "prime_active", "prime"} {
		for _, populated := range []bool{false, true} {
			name := mode + "/empty"
			if populated {
				name = mode + "/populated"
			}
			t.Run(name, func(t *testing.T) {
				mem := NewMemStore()
				var seeded []Bead
				if populated {
					for _, status := range []string{"open", "in_progress"} {
						row, err := mem.Create(Bead{Title: "counted read", Status: status})
						if err != nil {
							t.Fatal(err)
						}
						seeded = append(seeded, row)
					}
					if err := mem.DepAdd(seeded[0].ID, seeded[1].ID, "blocks"); err != nil {
						t.Fatal(err)
					}
				}
				var work storeWorkTally
				var reads []string
				backing := &storeWorkReadSpy{Store: mem, onRead: func(kind string) {
					reads = append(reads, kind)
					if got := work.active.Load(); got != 1 {
						t.Errorf("active store work during %s = %d, want 1", kind, got)
					}
				}}
				cs := NewCachingStoreForTest(backing, nil, WithStoreWork(work.begin))
				for cycle := int64(1); cycle <= 2; cycle++ {
					reads = nil
					if err := runStoreWorkRead(cs, mode); err != nil {
						t.Fatal(err)
					}
					work.assertReleased(t, cycle)
					wantLists := []string{"list:"}
					if mode == "prime_active" {
						wantLists = []string{"list:open", "list:in_progress"}
					}
					for _, kind := range wantLists {
						if !slices.Contains(reads, kind) {
							t.Errorf("backing reads = %v, missing %s", reads, kind)
						}
					}
					if !slices.Contains(reads, "projection") || (populated && !slices.Contains(reads, "deps")) {
						t.Errorf("backing reads = %v, missing projection or populated dependency read", reads)
					}
					for _, row := range seeded {
						if _, err := cs.Get(row.ID); err != nil {
							t.Errorf("seeded row %s not cached: %v", row.ID, err)
						}
					}
					// Partial caches deliberately serve public dependency queries live;
					// their background dependency read was checked above.
					if populated && mode != "prime_active" {
						deps, err := cs.DepList(seeded[0].ID, "down")
						if err != nil || len(deps) != 1 || deps[0].DependsOnID != seeded[1].ID {
							t.Errorf("cached dependency = %v, %v; want the seeded edge", deps, err)
						}
					}
				}
			})
		}
	}
}

func TestStoreWorkReleasesFailedCacheReads(t *testing.T) {
	for _, mode := range []string{"reconcile", "prime_active", "prime"} {
		t.Run(mode, func(t *testing.T) {
			var work storeWorkTally
			var reads int
			failure := errors.New("backing unavailable")
			backing := &storeWorkReadSpy{Store: NewMemStore(), listErr: failure, onRead: func(kind string) {
				reads++
				if got := work.active.Load(); got != 1 {
					t.Errorf("active store work during failing %s = %d, want 1", kind, got)
				}
			}}
			cs := NewCachingStoreForTest(backing, nil, WithStoreWork(work.begin))
			cs.SetPrimeRetryDelayForTest(func(int) time.Duration { return 0 })
			err := runStoreWorkRead(cs, mode)
			if mode == "reconcile" {
				if cs.nextReconcileDelay(cs.clockNow()) <= 0 || !strings.Contains(cs.Stats().LastProblem, failure.Error()) {
					t.Error("failed reconcile did not retain its problem and backoff")
				}
			} else if !errors.Is(err, failure) || !strings.Contains(err.Error(), "prime") {
				t.Errorf("prime error = %v, want contextual backing failure", err)
			}
			wantReads := 1
			if mode == "prime" {
				wantReads = 3
			}
			if reads != wantReads {
				t.Errorf("failing reads = %d, want %d", reads, wantReads)
			}
			work.assertReleased(t, 1)
		})
	}
}

func TestStoreWorkReleasesACanceledPrime(t *testing.T) {
	var work storeWorkTally
	backing := &listCountingStore{MemStore: NewMemStore()}
	cs := NewCachingStoreForTest(backing, nil, WithStoreWork(work.begin))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cs.Prime(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled Prime = %v, want context.Canceled", err)
	}
	if got := backing.lists.Load(); got != 0 {
		t.Errorf("canceled Prime made %d backing reads, want 0", got)
	}
	work.assertReleased(t, 1)
}

func TestStoreWorkOverlappingReadsReleaseOnlyTheirOwnCount(t *testing.T) {
	var work storeWorkTally
	var lists atomic.Int64
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	backing := &storeWorkReadSpy{Store: NewMemStore(), onRead: func(kind string) {
		if strings.HasPrefix(kind, "list:") && lists.Add(1) == 1 {
			close(entered)
			<-release
		}
	}}
	var gateCalls atomic.Int64
	cs := NewCachingStoreForTest(backing, nil, WithStoreWork(work.begin), WithReconcileGate(func() bool {
		want := gateCalls.Add(1)
		if got := work.active.Load(); got != want {
			t.Errorf("active store work at gate %d = %d, want %d", want, got, want)
		}
		return true
	}))
	go func() {
		defer close(done)
		cs.ReconcileIfDueForTest()
	}()
	defer func() {
		close(release)
		select {
		case <-done:
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Error("held reconcile did not finish")
		}
		work.assertReleased(t, 3)
	}()
	select {
	case <-entered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("reconcile did not enter its backing read")
	}
	cs.ReconcileIfDueForTest() // Competing check must not start a second scan.
	if got := lists.Load(); got != 1 {
		t.Errorf("competing reconcile made %d scans, want only the held scan", got)
	}
	if err := cs.PrimeActive(); err != nil {
		t.Fatal(err)
	}
	if got := work.active.Load(); got != 1 {
		t.Errorf("active store work after competing reads finish = %d, want the held reconcile's 1", got)
	}
}

func TestJoinedCacheOptionsSkipNilAndKeepTheirOrder(t *testing.T) {
	var work storeWorkTally
	var order []int
	mark := func(n int) CachingStoreOption {
		return func(*CachingStore) { order = append(order, n) }
	}
	cs := NewCachingStoreForTest(NewMemStore(), nil, JoinCachingStoreOptions(
		nil, mark(1), WithStoreWork(work.begin), nil, mark(2), nil,
	))
	if !slices.Equal(order, []int{1, 2}) {
		t.Errorf("option order = %v, want [1 2]", order)
	}
	cs.ReconcileIfDueForTest()
	work.assertReleased(t, 1)

	failure := errors.New("joined backing unavailable")
	backing := &storeWorkReadSpy{Store: NewMemStore(), listErr: failure, onRead: func(string) {}}
	cs = NewCachingStoreForTest(backing, nil, JoinCachingStoreOptions(nil, WithStoreWork(work.begin), nil))
	if err := cs.PrimeActive(); !errors.Is(err, failure) {
		t.Errorf("joined prime error = %v, want backing failure", err)
	}
	work.assertReleased(t, 2)
}

func TestEmptyCacheOptionsAllowUncountedReads(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  CachingStoreOption
	}{
		{"empty_join", JoinCachingStoreOptions()},
		{"all_nil_join", JoinCachingStoreOptions(nil, nil)},
		{"nil_counter", WithStoreWork(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing := &listCountingStore{MemStore: NewMemStore()}
			cs := NewCachingStoreForTest(backing, nil, tc.opt)
			cs.ReconcileIfDueForTest()
			if got := backing.lists.Load(); got != 1 {
				t.Errorf("ordinary cycle made %d backing reads, want 1", got)
			}
		})
	}
}
