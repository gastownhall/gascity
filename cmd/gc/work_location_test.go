package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// A sessions store cannot be a work leg (mc-3ixn3.16, NEW2-1): no store
// type, class wrapper or the Store interface assigns or converts to the city
// work store a WorkLegs is minted from, and a WorkLegs has no field a caller
// can set.
func TestWorkLegsSessionStoreIsNotAWorkLeg(t *testing.T) {
	src := reflect.TypeOf(cityWorkStore{})
	for _, from := range []reflect.Type{
		reflect.TypeOf(beads.SessionStore{}),
		reflect.TypeOf(beads.WorkStore{}),
		reflect.TypeOf((*beads.Store)(nil)).Elem(),
		reflect.TypeOf(beads.NewMemStore()),
	} {
		if from.AssignableTo(src) || from.ConvertibleTo(src) {
			t.Errorf("%v reaches cityWorkStore without a mint site", from)
		}
	}
	legs := reflect.TypeOf(WorkLegs{})
	for i := 0; i < legs.NumField(); i++ {
		if legs.Field(i).IsExported() {
			t.Errorf("WorkLegs.%s is exported: a caller could set a leg", legs.Field(i).Name)
		}
	}
	if reflect.TypeOf(beads.SessionStore{}).ConvertibleTo(legs) {
		t.Error("a sessions store converts to WorkLegs")
	}
}

// workLegsMintSites are the only functions that may mint a city work store
// or a WorkLegs, with why the store they hold is the city's work store.
var workLegsMintSites = map[string]string{
	"work_location.go:cityWorkStoreOf":                            "the one-shot mint, its callers pinned below",
	"work_location.go:workLegs":                                   "the controller's: cr.cityBeadStore, the store it registers as the work leg",
	"api_state_wake_refusal.go:WakeStartRefusal":                  "the controller's CityBeadStore",
	"cmd_session_wake.go:doSessionWake":                           "openCityStore in cmdSessionWake",
	"cmd_session.go:cmdSessionClose":                              "openCityStore",
	"cmd_runtime_drain.go:releaseUnexecutedClaimsForSessionStore": "openCityStoreAtWithConfig in releaseUnexecutedClaimsForSession",
	"cmd_start.go:doStartStandalone":                              "oneShotStore, the store the one-shot pass registers as the work leg",
	"reconcile_gather.go:gather":                                  "gatherEnv.WorkStore: cr.cityBeadStore (newPlannerHost)",
	"api_state_wake_refusal.go:wakeWillNotStart":                  "wakeVerdictDeps.work, minted by its two callers above",
}

// TestWorkLegsSingleConstructor pins the mint sites: cityWorkStore literals,
// cityWorkStoreOf calls and workLegsFromCensus calls appear only in
// workLegsMintSites; WorkLegs literals are the zero value only; and only the
// two polarity scopes implement workScope.
func TestWorkLegsSingleConstructor(t *testing.T) {
	root := repoRootForLint(t)
	paths, err := filepath.Glob(filepath.Join(root, "cmd/gc", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	used := map[string]bool{}
	var scopes []string
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Base(p)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if fn.Name.Name == "scopeIDs" && fn.Recv != nil {
				scopes = append(scopes, typeName(fn.Recv.List[0].Type))
			}
			site := file + ":" + fn.Name.Name
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				var what string
				switch v := n.(type) {
				case *ast.CompositeLit:
					switch typeName(v.Type) {
					case "cityWorkStore":
						what = "a cityWorkStore literal"
					case "WorkLegs":
						if len(v.Elts) > 0 && site != "work_location.go:workLegsFromCensus" {
							t.Errorf("%s: a WorkLegs literal with fields; mint it with workLegsFromCensus", site)
						}
					}
				case *ast.CallExpr:
					switch calleeName(v.Fun) {
					case "cityWorkStoreOf", "workLegsFromCensus":
						what = "a " + calleeName(v.Fun) + " call"
					}
				}
				if what != "" {
					if _, ok := workLegsMintSites[site]; !ok {
						t.Errorf("%s: %s outside workLegsMintSites: a work leg is minted only from the city's work store", site, what)
					}
					used[site] = true
				}
				return true
			})
		}
	}
	for site := range workLegsMintSites {
		if !used[site] {
			t.Errorf("workLegsMintSites lists %s, which mints nothing: drop it", site)
		}
	}
	slices.Sort(scopes)
	if !slices.Equal(scopes, []string{"RefuseScope", "ReleaseScope"}) {
		t.Errorf("workScope implementers = %v, want only the two polarity scopes", scopes)
	}
}

func typeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		return typeName(v.X)
	case *ast.SelectorExpr:
		return v.Sel.Name
	}
	return ""
}

// The zero WorkLegs, one minted without a work store, and a nil SeatWork
// answer unknown: no gate may read "no work" from a leg set nothing minted.
func TestWorkLegsUnmintedFailsClosed(t *testing.T) {
	info := session.Info{ID: "gc-1"}
	for name, legs := range map[string]WorkLegs{
		"zero":          {},
		"no work store": workLegsFromCensus(t.TempDir(), nil, cityWorkStore{}, nil),
	} {
		has, err := sessionHasOpenAssignedWorkForReachableStore(newSeatWork(legs), info)
		if !errors.Is(err, errNoWorkStore) {
			t.Errorf("%s: gate = %v, %v; want unknown (errNoWorkStore)", name, has, err)
		}
	}
	if has, err := sessionHasOpenAssignedWorkForReachableStore(nil, info); !errors.Is(err, errNoWorkStore) {
		t.Errorf("nil SeatWork: gate = %v, %v; want unknown (errNoWorkStore)", has, err)
	}
}

// NEW2-1: v2's live work read plans over the city work store gather mints
// from gatherEnv.WorkStore, never the sessions store the census leads with.
// A claim on the work store holds the row; with no work store the read is
// unknown.
func TestGatherWorkLegsReadTheCityWorkStore(t *testing.T) {
	f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "active"))
	work := beads.NewMemStore()
	if _, err := work.Create(beads.Bead{Title: "claim", Type: "task", Status: "open", Assignee: "gc-1"}); err != nil {
		t.Fatal(err)
	}
	f.env.WorkStore = func() beads.Store { return work }
	w := f.gather(t)
	if !w.WorkLegs.isWork(work) || w.WorkLegs.isWork(f.cache) {
		t.Fatal("gather's WorkLegs do not take the city work store as the work leg")
	}
	row := w.Census.Canonical()[0].Info
	if got := newEffectPass(&w, &allocDecision{}).readWork(row); got.Free || got.Err != nil {
		t.Fatalf("L5 = %+v; want the work store's claim to hold the row", got)
	}

	f.env.WorkStore = nil
	w = f.gather(t)
	if got := newEffectPass(&w, &allocDecision{}).readWork(row); got.Free || !errors.Is(got.Err, errNoReadStore) {
		t.Fatalf("L5 without a work store = %+v; want unknown", got)
	}
}

// blockedLegStore's List waits on release, closed when the test ends.
type blockedLegStore struct {
	beads.Store
	release <-chan struct{}
}

func (s blockedLegStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	<-s.release
	return s.Store.List(q)
}

// releasedAtCleanup is a channel closed when t ends.
func releasedAtCleanup(t *testing.T) <-chan struct{} {
	c := make(chan struct{})
	t.Cleanup(func() { close(c) })
	return c
}

// rendezvousStore's lists each wait until all want of them have started: a
// reader that lists its legs one after another never gets past the first.
type rendezvousStore struct {
	beads.Store
	arrived *atomic.Int32
	want    int32
	all     chan struct{}
	done    <-chan struct{}
}

func (s rendezvousStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.arrived.Add(1) == s.want {
		close(s.all)
	}
	select {
	case <-s.all:
	case <-s.done:
	}
	return s.Store.List(q)
}

// Each leg has its own budget, from the same start: legs whose lists can
// finish only once every list has started (a serial reader would spend the
// budget on the first) all answer; a leg past the budget is unknown while
// the others still answer.
func TestSeatWorkBudgetIsPerLeg(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "a", Path: filepath.Join(cityPath, "a")}, {Name: "b", Path: filepath.Join(cityPath, "b")}}}
	info := session.Info{ID: "gc-1"}
	scope := releaseScope(info, cfg)
	q := seatWorkQuery{scope: scope, statuses: seatWorkStatuses}
	const budget = 2 * time.Second

	t.Run("legs read at once", func(t *testing.T) {
		// Three legs, one identity, two statuses: six lists.
		meet := rendezvousStore{arrived: new(atomic.Int32), want: 6, all: make(chan struct{}), done: releasedAtCleanup(t)}
		leg := func() beads.Store { m := meet; m.Store = beads.NewMemStore(); return m }
		legs := workLegsFromCensus(cityPath, cfg, cityWorkStore{store: leg()}, map[string]beads.Store{"a": leg(), "b": leg()})
		if has, err := seatWorkFor(legs, scope, budget).has(q); has || err != nil {
			t.Fatalf("gate = %v, %v; want no work, every leg read within its budget", has, err)
		}
	})
	t.Run("a leg past budget", func(t *testing.T) {
		rigs := map[string]beads.Store{"a": beads.NewMemStore(), "b": blockedLegStore{Store: beads.NewMemStore(), release: releasedAtCleanup(t)}}
		legs := workLegsFromCensus(cityPath, cfg, cityWorkStore{store: beads.NewMemStore()}, rigs)
		if has, err := seatWorkFor(legs, scope, 50*time.Millisecond).has(q); has || !errors.Is(err, errSeatWorkBudget) {
			t.Fatalf("gate = %v, %v; want the blocked leg unknown", has, err)
		}
	})
}

// A seat's read lists only its scope's identities, so a question about any
// other identity is unknown, not "no work".
func TestSeatWorkSeatReadRefusesAnUnreadIdentity(t *testing.T) {
	work := beads.NewMemStore()
	if _, err := work.Create(beads.Bead{Title: "claim", Type: "task", Status: "open", Assignee: "other"}); err != nil {
		t.Fatal(err)
	}
	legs := workLegsFromCensus(t.TempDir(), nil, cityWorkStore{store: work}, nil)
	sw := seatWorkFor(legs, ReleaseScope{ids: []string{"gc-1"}}, 0)
	if has, err := sw.has(seatWorkQuery{scope: ReleaseScope{ids: []string{"gc-1"}}, statuses: seatWorkStatuses}); has || err != nil {
		t.Fatalf("read identity = %v, %v; want no work", has, err)
	}
	if has, err := sw.has(seatWorkQuery{scope: RefuseScope{ids: []string{"gc-1", "other"}}, statuses: seatWorkStatuses}); has || err == nil {
		t.Fatalf("unread identity = %v, %v; want unknown", has, err)
	}
}

// One read serves concurrent questions: the starts' claimed-work probes ask
// from parallel goroutines.
func TestSeatWorkConcurrentQuestionsShareOneRead(t *testing.T) {
	work := &workCallCounter{Store: beads.NewMemStore()}
	legs := workLegsFromCensus(t.TempDir(), nil, cityWorkStore{store: work}, nil)
	sw := newSeatWork(legs)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := sw.has(seatWorkQuery{scope: ReleaseScope{ids: []string{"gc-1"}}, statuses: seatWorkStatuses}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := work.n(); got != len(seatWorkStatuses) {
		t.Fatalf("8 questions listed the work store %d times, want %d (one read)", got, len(seatWorkStatuses))
	}
}

// A v2 effect's sections hold the city work legs read-only: every leg, the
// rigs and bindings included, refuses a write (capReadStores).
func TestEffectReadsLegsAreReadOnly(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "a", Path: filepath.Join(cityPath, "a")}}}
	w := &World{Env: &reconcileEnv{}, WorkLegs: workLegsFromCensus(cityPath, cfg, cityWorkStore{store: beads.NewMemStore()}, map[string]beads.Store{"a": beads.NewMemStore()})}
	legs := newEffectPass(w, &allocDecision{}).reads.legs
	n := 0
	legs.each(func(s beads.Store) {
		n++
		if _, err := s.Create(beads.Bead{Title: "write"}); !errors.Is(err, errBlindWriteRefused) {
			t.Errorf("leg %d accepted a write: %v", n, err)
		}
	})
	if n != 2 {
		t.Fatalf("effect reads hold %d legs, want the work store and the rig", n)
	}
}
