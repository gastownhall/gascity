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
// read produces: two rigs sharing a path are each listed as their own scope,
// and suspended or blank-path rigs are not read.
func TestHoldLabelRoutedToSweepsScopesConcurrently(t *testing.T) {
	cityDir, rigA, rigB, rigC := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "a", Path: rigA},
		{Name: "b", Path: rigB},
		{Name: "c", Path: rigC},
		{Name: "mirror", Path: rigA},
		{Name: "asleep", Path: "/rigs/asleep", Suspended: true},
		{Name: "nowhere", Path: "  "},
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
		`rig mirror bead A-1 has hold:qa but gc.routed_to=""; use "qa"`,
	}
	if res.Status != doctor.StatusWarning || !slices.Equal(res.Details, want) {
		t.Fatalf("Run = %v %q\ndetails %q\nwant    %q", res.Status, res.Message, res.Details, want)
	}
	if want := "4 bead(s) carry a hold:<value> label without matching gc.routed_to"; res.Message != want {
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

// countingFactory is a store factory over per-path stores that records every
// open and List, and runs onList (when set) inside each List.
type countingFactory struct {
	mu     sync.Mutex
	stores map[string]beads.Store
	opened []string
	lists  int
	onList func()
}

type sweepCountingStore struct {
	beads.Store
	f *countingFactory
}

func (s sweepCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.f.mu.Lock()
	s.f.lists++
	onList := s.f.onList
	s.f.mu.Unlock()
	if onList != nil {
		onList()
	}
	return s.Store.List(q)
}

func (f *countingFactory) open(path string) (beads.Store, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, path)
	store, ok := f.stores[path]
	if !ok {
		return nil, errors.New("no store here")
	}
	return sweepCountingStore{Store: store, f: f}, nil
}

func (f *countingFactory) calls() (opened []string, lists int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.opened), f.lists
}

// abandonedCheck is a store-backed doctor check under test for AC-4.
type abandonedCheck interface {
	Run(*doctor.CheckContext) *doctor.CheckResult
	Fix(*doctor.CheckContext) error
}

// assertNoStoreCallsWhenAbandoned runs check's Run and Fix with Done already
// closed and fails if either issued a store open or List.
func assertNoStoreCallsWhenAbandoned(t *testing.T, f *countingFactory, check abandonedCheck) {
	t.Helper()
	done := make(chan struct{})
	close(done)
	ctx := &doctor.CheckContext{Done: done}
	check.Run(ctx)
	if opened, lists := f.calls(); len(opened) != 0 || lists != 0 {
		t.Fatalf("abandoned Run opened %v and listed %d time(s), want no store calls", opened, lists)
	}
	_ = check.Fix(ctx) // the abandoned scopes are reported as skipped; only the store calls matter here
	if opened, lists := f.calls(); len(opened) != 0 || lists != 0 {
		t.Fatalf("abandoned Fix opened %v and listed %d time(s), want no store calls", opened, lists)
	}
}

// TestHoldLabelRoutedToStopsWhenAbandoned (AC-4): once the runner abandons
// the check, neither Run nor Fix opens or lists a store.
func TestHoldLabelRoutedToStopsWhenAbandoned(t *testing.T) {
	cityDir, rigDir := t.TempDir(), t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}
	f := &countingFactory{stores: map[string]beads.Store{cityDir: beads.NewMemStore(), rigDir: beads.NewMemStore()}}
	assertNoStoreCallsWhenAbandoned(t, f, newHoldLabelRoutedToCheck(cfg, cityDir, f.open))
}

// TestRunTargetBackfillStopsWhenAbandoned (AC-4): once the runner abandons
// the check, neither Run nor Fix opens or lists a store.
func TestRunTargetBackfillStopsWhenAbandoned(t *testing.T) {
	cityDir, rigDir := t.TempDir(), t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}
	f := &countingFactory{stores: map[string]beads.Store{cityDir: beads.NewMemStore(), rigDir: beads.NewMemStore()}}
	assertNoStoreCallsWhenAbandoned(t, f, newRunTargetRoutedToBackfillCheck(cfg, cityDir, f.open))
}

// TestDoctorScopeSweepStopsWhenDoneClosesMidSweep (AC-4): when Done closes
// during the first scope's List, no scope not yet dispatched is opened or
// listed, and each is reported as abandoned.
func TestDoctorScopeSweepStopsWhenDoneClosesMidSweep(t *testing.T) {
	scopes := []doctorWorkScope{{"city", "c"}, {"rig a", "a"}, {"rig b", "b"}, {"rig c", "x"}}
	done := make(chan struct{})
	var closeOnce sync.Once
	f := &countingFactory{
		stores: map[string]beads.Store{"c": beads.NewMemStore(), "a": beads.NewMemStore(), "b": beads.NewMemStore()},
		onList: func() { closeOnce.Do(func() { close(done) }) },
	}

	got := sweepDoctorScopeListsLimit(&doctor.CheckContext{Done: done}, scopes, f.open, beads.ListQuery{AllowScan: true}, 1)

	if opened, lists := f.calls(); !slices.Equal(opened, []string{"c"}) || lists != 1 {
		t.Fatalf("opened %v and listed %d time(s), want only the first scope [c] listed once", opened, lists)
	}
	if !got[0].read || got[0].skipped != "" {
		t.Fatalf("first scope = %+v, want read", got[0])
	}
	for _, sc := range got[1:] {
		if want := sc.label + " skipped: check abandoned at its timeout"; sc.read || sc.skipped != want {
			t.Fatalf("scope %s = read %v skipped %q, want skipped %q", sc.label, sc.read, sc.skipped, want)
		}
	}
}

// failingScopeFixture is four scopes: the city and rig d readable with one
// target each, rig b's store failing to open and rig c's failing its List.
type failingScopeFixture struct {
	cfg          *config.City
	cityDir      string
	city, rigD   beads.Store
	factory      func(string) (beads.Store, error)
	wantSkipped  []string
	wantFixError string
}

func newFailingScopeFixture(t *testing.T, bead func(id string) beads.Bead, checkName string) failingScopeFixture {
	t.Helper()
	cityDir, rigB, rigC, rigD := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	fx := failingScopeFixture{
		cfg: &config.City{Rigs: []config.Rig{
			{Name: "b", Path: rigB},
			{Name: "c", Path: rigC},
			{Name: "d", Path: rigD},
		}},
		cityDir: cityDir,
		city:    beads.NewMemStoreFrom(0, []beads.Bead{bead("C-1")}, nil),
		rigD:    beads.NewMemStoreFrom(0, []beads.Bead{bead("D-1")}, nil),
		wantSkipped: []string{
			"rig b skipped: opening bead store: permission denied",
			"rig c skipped: listing beads: listing failed",
		},
	}
	fx.wantFixError = checkName + " skipped 2 scope(s): " + fx.wantSkipped[0] + "; " + fx.wantSkipped[1]
	stores := map[string]beads.Store{cityDir: fx.city, rigC: holdLabelListErrorStore{Store: beads.NewMemStore()}, rigD: fx.rigD}
	fx.factory = func(path string) (beads.Store, error) {
		if path == rigB {
			return nil, errors.New("permission denied")
		}
		return holdLabelStores(stores)(path)
	}
	return fx
}

// TestDoctorScopeSweepReportsFailuresPerScope (AC-5): a scope that fails to
// open or to list is reported in the existing skip-line format with its
// label, and the other scopes' targets are still reported and repaired.
func TestDoctorScopeSweepReportsFailuresPerScope(t *testing.T) {
	t.Run("sweep", func(t *testing.T) {
		fx := newFailingScopeFixture(t, func(id string) beads.Bead { return beads.Bead{ID: id, Title: "x", Type: "task", Status: "open"} }, "")
		got := sweepDoctorScopeLists(&doctor.CheckContext{}, doctorWorkScopes(fx.cfg, fx.cityDir), fx.factory, beads.ListQuery{AllowScan: true})
		var labels, skipped []string
		for _, sc := range got {
			labels = append(labels, sc.label)
			if sc.skipped != "" {
				skipped = append(skipped, sc.skipped)
			}
		}
		if !slices.Equal(labels, []string{"city", "rig b", "rig c", "rig d"}) || !slices.Equal(skipped, fx.wantSkipped) {
			t.Fatalf("labels %q skipped %q, want scope order and %q", labels, skipped, fx.wantSkipped)
		}
		if !got[0].read || len(got[0].items) != 1 || !got[3].read || len(got[3].items) != 1 || got[1].read || got[2].read {
			t.Fatalf("read flags/items = %+v", got)
		}
	})

	t.Run("hold-label-routed-to", func(t *testing.T) {
		fx := newFailingScopeFixture(t, func(id string) beads.Bead {
			return beads.Bead{ID: id, Title: "held", Type: "task", Status: "open", Labels: []string{"hold:qa"}}
		}, "hold-label-routed-to")
		check := newHoldLabelRoutedToCheck(fx.cfg, fx.cityDir, fx.factory)
		want := append([]string{
			`city bead C-1 has hold:qa but gc.routed_to=""; use "qa"`,
			`rig d bead D-1 has hold:qa but gc.routed_to=""; use "qa"`,
		}, fx.wantSkipped...)
		slices.Sort(want)
		if res := check.Run(&doctor.CheckContext{}); !slices.Equal(res.Details, want) {
			t.Fatalf("details %q\nwant    %q", res.Details, want)
		}
		if err := check.Fix(&doctor.CheckContext{}); err == nil || err.Error() != fx.wantFixError {
			t.Fatalf("Fix error = %v, want %q", err, fx.wantFixError)
		}
		if got := holdLabelRoute(t, fx.city, "C-1") + "," + holdLabelRoute(t, fx.rigD, "D-1"); got != "qa,qa" {
			t.Fatalf("repaired routes = %q, want qa,qa", got)
		}
	})

	t.Run("run-target-routed-to-backfill", func(t *testing.T) {
		fx := newFailingScopeFixture(t, func(id string) beads.Bead {
			return beads.Bead{ID: id, Title: "root", Type: "task", Status: "open", Metadata: map[string]string{
				"gc.kind": "workflow", "gc.run_target": "worker",
			}}
		}, "run-target-routed-to-backfill")
		check := newRunTargetRoutedToBackfillCheck(fx.cfg, fx.cityDir, fx.factory)
		want := append([]string{
			`city bead C-1 has gc.run_target="worker" with empty gc.routed_to`,
			`rig d bead D-1 has gc.run_target="worker" with empty gc.routed_to`,
		}, fx.wantSkipped...)
		slices.Sort(want)
		if res := check.Run(&doctor.CheckContext{}); !slices.Equal(res.Details, want) {
			t.Fatalf("details %q\nwant    %q", res.Details, want)
		}
		if err := check.Fix(&doctor.CheckContext{}); err == nil || err.Error() != fx.wantFixError {
			t.Fatalf("Fix error = %v, want %q", err, fx.wantFixError)
		}
		if got := holdLabelRoute(t, fx.city, "C-1") + "," + holdLabelRoute(t, fx.rigD, "D-1"); got != "worker,worker" {
			t.Fatalf("repaired routes = %q, want worker,worker", got)
		}
	})
}

// TestDoctorScopeSweepEdgeCases (AC-5): the sweep reads exactly the scopes
// the checks read one at a time: suspended and pathless rigs are left out, a
// nil factory reads nothing, and two rigs sharing a path are each listed.
func TestDoctorScopeSweepEdgeCases(t *testing.T) {
	cityDir, shared := t.TempDir(), t.TempDir()
	if got := doctorWorkScopes(nil, cityDir); !slices.Equal(got, []doctorWorkScope{{"city", cityDir}}) {
		t.Fatalf("city-only scopes = %+v", got)
	}
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "a", Path: shared},
		{Name: "idle", Path: t.TempDir(), Suspended: true},
		{Name: "nopath", Path: "  "},
		{Name: "b", Path: shared},
	}}
	scopes := doctorWorkScopes(cfg, cityDir)
	if want := []doctorWorkScope{{"city", cityDir}, {"rig a", shared}, {"rig b", shared}}; !slices.Equal(scopes, want) {
		t.Fatalf("scopes = %+v, want %+v", scopes, want)
	}

	if got := sweepDoctorScopeLists[beads.Store](&doctor.CheckContext{}, scopes, nil, beads.ListQuery{AllowScan: true}); len(got) != 3 || got[0].read || got[0].skipped != "" {
		t.Fatalf("nil factory sweep = %+v, want three unread scopes", got)
	}

	f := &countingFactory{stores: map[string]beads.Store{
		cityDir: beads.NewMemStore(),
		shared:  beads.NewMemStoreFrom(0, []beads.Bead{{ID: "S-1", Title: "x", Type: "task", Status: "open"}}, nil),
	}}
	got := sweepDoctorScopeLists(&doctor.CheckContext{}, scopes, perRunStoreFactory(f.open), beads.ListQuery{AllowScan: true})
	opened, lists := f.calls()
	slices.Sort(opened)
	wantOpened := []string{cityDir, shared}
	slices.Sort(wantOpened)
	if !slices.Equal(opened, wantOpened) || lists != 3 {
		t.Fatalf("opened %v and listed %d time(s), want each path opened once and every scope listed", opened, lists)
	}
	if len(got[1].items) != 1 || len(got[2].items) != 1 {
		t.Fatalf("shared-path scopes = %+v, want both rigs to list S-1", got[1:])
	}
}

// TestDoctorScopeSweepBoundsConcurrency (AC-6): the sweep keeps exactly
// doctorScopeSweepConcurrency reads in flight, never more, and a sweep at a
// limit of 1 returns the same results as one at the default limit.
func TestDoctorScopeSweepBoundsConcurrency(t *testing.T) {
	// newScopes builds 3x the limit readable scopes; with a probe, every List
	// runs inside the probe's blocking section.
	newScopes := func(probe *inFlightProbe) ([]doctorWorkScope, func(string) (beads.Store, error)) {
		stores := map[string]beads.Store{}
		var scopes []doctorWorkScope
		for range 3 * doctorScopeSweepConcurrency {
			path := t.TempDir()
			var store beads.Store = beads.NewMemStoreFrom(0, []beads.Bead{{ID: path, Title: "x", Type: "task", Status: "open"}}, nil)
			if probe != nil {
				store = inFlightStore{Store: store, probe: probe}
			}
			stores[path] = store
			scopes = append(scopes, doctorWorkScope{label: "rig " + path, path: path})
		}
		return scopes, holdLabelStores(stores)
	}

	// A one-past-the-limit target only releases early if the bound leaks;
	// otherwise the safety timer releases the first full batch.
	probe := newInFlightProbe(t, doctorScopeSweepConcurrency+1, time.Second)
	scopes, factory := newScopes(probe)
	got := sweepDoctorScopeLists(&doctor.CheckContext{}, scopes, factory, beads.ListQuery{AllowScan: true})
	if peak := probe.peak.Load(); peak != doctorScopeSweepConcurrency {
		t.Fatalf("peak concurrent List calls = %d, want exactly %d", peak, doctorScopeSweepConcurrency)
	}
	for i, sc := range got {
		if !sc.read || len(sc.items) != 1 {
			t.Fatalf("scope %d = %+v, want read with one item", i, sc)
		}
	}

	scopes, factory = newScopes(nil)
	query := beads.ListQuery{AllowScan: true}
	serial := sweepDoctorScopeListsLimit(&doctor.CheckContext{}, scopes, factory, query, 1)
	parallel := sweepDoctorScopeLists(&doctor.CheckContext{}, scopes, factory, query)
	for i := range scopes {
		if serial[i].label != parallel[i].label || serial[i].skipped != parallel[i].skipped || serial[i].read != parallel[i].read ||
			!slices.EqualFunc(serial[i].items, parallel[i].items, func(a, b beads.Bead) bool { return a.ID == b.ID }) {
			t.Fatalf("scope %d: limit 1 = %+v, default = %+v", i, serial[i], parallel[i])
		}
	}
}
