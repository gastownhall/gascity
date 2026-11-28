package main

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// inFlightProbe counts the calls that are inside a blocking section at once.
// Every call waits until target calls are in flight together, or until the
// safety timer fires, so a one-at-a-time caller finishes (slowly) with a peak
// of 1 instead of hanging.
type inFlightProbe struct {
	target   int32
	inFlight atomic.Int32
	peak     atomic.Int32
	release  chan struct{}
	once     sync.Once
}

func newInFlightProbe(t *testing.T, target int, safety time.Duration) *inFlightProbe {
	t.Helper()
	p := &inFlightProbe{target: int32(target), release: make(chan struct{})}
	timer := time.AfterFunc(safety, p.open)
	t.Cleanup(func() { timer.Stop() })
	return p
}

func (p *inFlightProbe) open() { p.once.Do(func() { close(p.release) }) }

// enter blocks the caller as one in-flight call and returns once released.
func (p *inFlightProbe) enter() {
	n := p.inFlight.Add(1)
	for {
		peak := p.peak.Load()
		if n <= peak || p.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	if n >= p.target {
		p.open()
	}
	<-p.release
	p.inFlight.Add(-1)
}

// inFlightStore is a store whose List runs inside probe's blocking section.
type inFlightStore struct {
	beads.Store
	probe *inFlightProbe
}

func (s inFlightStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.probe.enter()
	return s.Store.List(q)
}

// TestHoldLabelRoutedToSweepsScopesConcurrently (AC-1): the check reads its
// scopes concurrently, and still reports exactly the lines a one-at-a-time
// read produces.
func TestHoldLabelRoutedToSweepsScopesConcurrently(t *testing.T) {
	cityDir, rigA, rigB, rigC := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "a", Path: rigA},
		{Name: "b", Path: rigB},
		{Name: "c", Path: rigC},
	}}
	probe := newInFlightProbe(t, 2, 5*time.Second)
	held := func(id string) []beads.Bead {
		return []beads.Bead{{ID: id, Title: "held", Type: "task", Status: "open", Labels: []string{"hold:qa"}}}
	}
	stores := map[string]beads.Store{
		cityDir: inFlightStore{Store: beads.NewMemStoreFrom(0, held("C-1"), nil), probe: probe},
		rigA:    inFlightStore{Store: beads.NewMemStoreFrom(0, held("A-1"), nil), probe: probe},
		rigB:    inFlightStore{Store: beads.NewMemStoreFrom(0, held("B-1"), nil), probe: probe},
	}
	factory := func(path string) (beads.Store, error) {
		if store, ok := stores[path]; ok {
			return store, nil
		}
		return nil, errors.New("no store here")
	}

	res := newHoldLabelRoutedToCheck(cfg, cityDir, factory).Run(&doctor.CheckContext{})

	if got := probe.peak.Load(); got < 2 {
		t.Fatalf("max in-flight scope reads = %d, want >= 2", got)
	}
	want := []string{
		`city bead C-1 has hold:qa but gc.routed_to=""; use "qa"`,
		`rig a bead A-1 has hold:qa but gc.routed_to=""; use "qa"`,
		`rig b bead B-1 has hold:qa but gc.routed_to=""; use "qa"`,
		"rig c skipped: opening bead store: no store here",
	}
	if res.Status != doctor.StatusWarning || !slices.Equal(res.Details, want) {
		t.Fatalf("Run = %v %q\ndetails %q\nwant    %q", res.Status, res.Message, res.Details, want)
	}
	if want := "3 bead(s) carry a hold:<value> label without matching gc.routed_to"; res.Message != want {
		t.Fatalf("message = %q, want %q", res.Message, want)
	}
}

// TestRunTargetBackfillSweepsScopesConcurrently (AC-2): the backfill check
// reads its rig scopes concurrently and reports every rig's unrouted root.
func TestRunTargetBackfillSweepsScopesConcurrently(t *testing.T) {
	cityDir, rigA, rigB := t.TempDir(), t.TempDir(), t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "a", Path: rigA}, {Name: "b", Path: rigB}}}
	probe := newInFlightProbe(t, 2, 5*time.Second)
	root := func(id string) []beads.Bead {
		return []beads.Bead{{ID: id, Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.run_target": "worker",
		}}}
	}
	stores := map[string]beads.Store{
		cityDir: inFlightStore{Store: beads.NewMemStore(), probe: probe},
		rigA:    inFlightStore{Store: beads.NewMemStoreFrom(0, root("A-1"), nil), probe: probe},
		rigB:    inFlightStore{Store: beads.NewMemStoreFrom(0, root("B-1"), nil), probe: probe},
	}

	res := newRunTargetRoutedToBackfillCheck(cfg, cityDir, holdLabelStores(stores)).Run(&doctor.CheckContext{})

	if got := probe.peak.Load(); got < 2 {
		t.Fatalf("max in-flight scope reads = %d, want >= 2", got)
	}
	want := []string{
		`rig a bead A-1 has gc.run_target="worker" with empty gc.routed_to`,
		`rig b bead B-1 has gc.run_target="worker" with empty gc.routed_to`,
	}
	if res.Status != doctor.StatusWarning || !slices.Equal(res.Details, want) {
		t.Fatalf("Run = %v %q\ndetails %q\nwant    %q", res.Status, res.Message, res.Details, want)
	}
}

// TestPerRunStoreFactoryOpensDistinctScopesConcurrently (AC-3): distinct
// scopes open in parallel, while concurrent requests for one scope share a
// single open, its result, and a single observer call.
func TestPerRunStoreFactoryOpensDistinctScopesConcurrently(t *testing.T) {
	t.Run("distinct paths", func(t *testing.T) {
		probe := newInFlightProbe(t, 2, 5*time.Second)
		var seen []string // unguarded: the factory serializes its observers
		open := perRunStoreFactory(func(string) (beads.Store, error) {
			probe.enter()
			return beads.NewMemStore(), nil
		}, func(dir string) { seen = append(seen, dir) })
		var wg sync.WaitGroup
		for _, dir := range []string{"a", "b"} {
			wg.Go(func() {
				if _, err := open(dir); err != nil {
					t.Errorf("open %s: %v", dir, err)
				}
			})
		}
		wg.Wait()
		if got := probe.peak.Load(); got < 2 {
			t.Fatalf("max in-flight opens = %d, want >= 2", got)
		}
		slices.Sort(seen)
		if !slices.Equal(seen, []string{"a", "b"}) {
			t.Fatalf("observed %v, want [a b]", seen)
		}
	})

	for _, tc := range []struct {
		name    string
		openErr error
	}{
		{name: "one path opens", openErr: nil},
		{name: "one path fails", openErr: errors.New("no store")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := beads.NewMemStore()
			var calls, observed atomic.Int32
			entered, release := make(chan struct{}), make(chan struct{})
			open := perRunStoreFactory(func(string) (beads.Store, error) {
				if calls.Add(1) == 1 {
					close(entered)
				}
				<-release
				if tc.openErr != nil {
					return nil, tc.openErr
				}
				return want, nil
			}, func(string) { observed.Add(1) })

			const callers = 8
			stores := make([]beads.Store, callers)
			errs := make([]error, callers)
			var wg sync.WaitGroup
			for i := range callers {
				wg.Go(func() { stores[i], errs[i] = open("same") })
			}
			<-entered
			time.Sleep(50 * time.Millisecond) // let the other callers queue behind the open
			close(release)
			wg.Wait()

			if got := calls.Load(); got != 1 {
				t.Fatalf("open ran %d times, want 1", got)
			}
			for i := range callers {
				if !errors.Is(errs[i], tc.openErr) || (tc.openErr == nil && stores[i] != want) {
					t.Fatalf("caller %d got (%v, %v), want (%v, %v)", i, stores[i], errs[i], want, tc.openErr)
				}
			}
			wantObserved := int32(1)
			if tc.openErr != nil {
				wantObserved = 0
			}
			if got := observed.Load(); got != wantObserved {
				t.Fatalf("observer called %d times, want %d", got, wantObserved)
			}
		})
	}
}
