package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// residueState is the raw store accessors State keeps as T7 residue beside
// Stores (typed-stores T6 plan, OD-2).
type residueState interface {
	Stores() storeref.Stores
	CityBeadStore() beads.Store
	BeadStore(rig string) beads.Store
	BeadStores() map[string]beads.Store
	CityName() string
}

// checkStoresWrapTheResidueAccessors fails for every class or rig whose
// Stores() handle does not wrap exactly the store it must: the work and rig
// handles the stores CityBeadStore and BeadStore return, and each class handle
// the store classes names for it.
func checkStoresWrapTheResidueAccessors(t *testing.T, name string, st residueState, classes map[string]beads.Store) {
	t.Helper()
	s := st.Stores()
	for class, pair := range map[string][2]beads.Store{
		"work":     {s.Work().Store, st.CityBeadStore()},
		"graph":    {s.Graph().Store, classes["graph"]},
		"sessions": {s.Sessions().Store, classes["sessions"]},
		"mail":     {s.Mail().Store, classes["mail"]},
		"orders":   {s.Orders().Store, classes["orders"]},
		"nudges":   {s.Nudges().Store, classes["nudges"]},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s: Stores().%s wraps %v, want %v", name, class, pair[0], pair[1])
		}
	}
	if g, ok := s.GraphFor(storeref.WorkRef); !ok || g != s.Graph() {
		t.Errorf("%s: GraphFor(city) is not the city graph handle", name)
	}
	rigs := map[string]bool{}
	for _, r := range s.Rigs() {
		rigs[r.Name] = true
		if r.Work.Store != st.BeadStore(r.Name) {
			t.Errorf("%s: rig %s wraps %v, BeadStore returns %v", name, r.Name, r.Work.Store, st.BeadStore(r.Name))
		}
	}
	for rig, store := range st.BeadStores() {
		if rig == st.CityName() || store == nil {
			continue
		}
		if !rigs[rig] {
			t.Errorf("%s: BeadStores holds rig %s, Stores().Rigs() does not", name, rig)
		}
	}
}

// fakeClasses is the store each of f's class handles must wrap: the store a
// test relocated the class to, or, for a class left on the city store (unset,
// or set to the city store itself), the city store through the gate's class
// view. Messaging a test left unset follows a relocated sessions store, as the
// routes' whole split relocates both.
func fakeClasses(f *fakeState) map[string]beads.Store {
	class := func(relocated beads.Store) beads.Store {
		if relocated != nil && relocated != f.cityBeadStore {
			return relocated
		}
		return splittest.GateClassView(f.cityBeadStore)
	}
	mail := f.mailBeadStore
	if mail == nil {
		mail = f.sessionsBeadStore
	}
	return map[string]beads.Store{
		"graph":    class(f.graphBeadStore),
		"sessions": class(f.sessionsBeadStore),
		"mail":     class(mail),
		"orders":   class(f.ordersBeadStore),
		"nudges":   class(f.nudgesBeadStore),
	}
}

// TestFakeStateStoresWrapTheResidueAccessors is State.Stores's equivalence
// table over the test fakes (typed-stores T6a, T6c): unsplit, with a city
// store, with each class relocated, with a class "relocated" to the city store
// itself, with rigs, and the two fakes that override Stores. It runs in both
// gate modes, so under GC_TEST_SPLIT_STRICT=1 every handle carries the gate's
// view exactly as the residue accessors do.
func TestFakeStateStoresWrapTheResidueAccessors(t *testing.T) {
	cases := map[string]func(*fakeState){
		"default":      func(*fakeState) {},
		"a city store": func(f *fakeState) { f.cityBeadStore = beads.NewMemStore() },
		"two rigs":     func(f *fakeState) { f.stores["other"] = beads.NewMemStore() },
		"a nil rig":    func(f *fakeState) { f.stores["unopened"] = nil },
		"graph split":  func(f *fakeState) { f.cityBeadStore, f.graphBeadStore = beads.NewMemStore(), beads.NewMemStore() },
		"orders split": func(f *fakeState) { f.cityBeadStore, f.ordersBeadStore = beads.NewMemStore(), beads.NewMemStore() },
		"nudges split": func(f *fakeState) { f.cityBeadStore, f.nudgesBeadStore = beads.NewMemStore(), beads.NewMemStore() },
		"mail split":   func(f *fakeState) { f.cityBeadStore, f.mailBeadStore = beads.NewMemStore(), beads.NewMemStore() },
		"sessions split": func(f *fakeState) {
			f.cityBeadStore, f.sessionsBeadStore = beads.NewMemStore(), beads.NewMemStore()
		},
		"whole split": func(f *fakeState) {
			binding := beads.NewMemStore()
			f.cityBeadStore = beads.NewMemStore()
			f.graphBeadStore, f.sessionsBeadStore, f.ordersBeadStore, f.nudgesBeadStore = binding, binding, binding, binding
		},
		"relocated to the city store": func(f *fakeState) {
			f.cityBeadStore = beads.NewMemStore()
			f.graphBeadStore = f.cityBeadStore
		},
	}
	for name, setup := range cases {
		f := newFakeState(t)
		setup(f)
		checkStoresWrapTheResidueAccessors(t, name, f, fakeClasses(f))
	}

	st, _, _ := splitClassState(t)
	unavailable := fakeClasses(st)
	unavailable["sessions"], unavailable["mail"] = nil, nil
	checkStoresWrapTheResidueAccessors(t, "sessions class unavailable", sessionClassUnavailableState{st}, unavailable)

	f := newFakeState(t)
	f.cityBeadStore = beads.NewMemStore()
	f.graphBeadStore = beads.NewMemStore()
	reopened := beads.NewMemStore()
	all := map[string]beads.Store{"graph": reopened, "sessions": reopened, "mail": reopened, "orders": reopened, "nudges": reopened}
	checkStoresWrapTheResidueAccessors(t, "rebuilt over a new store", &rebuiltOverState{fakeState: f, store: reopened}, all)
}

// reviewedResidueCallers are this package's production calls of State's raw
// store accessors, keyed "file func method" (typed-stores T6 plan, OD-2): the
// T7 residue the plan's Appendix A names (federation, by-id, the status
// session snapshot and work counts, and ScopedStoreLike, which must keep
// receiving raw stores, R12) and store health (whose list fallback reaches
// ScopedStoreLike). The table only shrinks: a new caller takes its class
// handle from Stores().
var reviewedResidueCallers = map[string]int{
	// Not the accessor: SeededCityDeps' field of the same name, read by the
	// integration-tag dashport seam (OD-6). The guard matches by name.
	"dashport_support.go newSeededState CityBeadStore":                    1,
	"handler_convoy_dispatch.go workflowStoreByRef BeadStore":             1,
	"handler_convoy_dispatch.go workflowStoreByRef CityBeadStore":         3,
	"handler_convoy_dispatch.go workflowStoresWith BeadStore":             1,
	"handler_convoy_dispatch.go workflowStoresWith BeadStores":            1,
	"handler_convoy_dispatch.go workflowStoresWith CityBeadStore":         1,
	"handler_convoys.go isGraphConvoyID BeadStores":                       1,
	"handler_convoys.go isGraphConvoyID CityBeadStore":                    1,
	"handler_sling.go Server.sourceWorkflowStores BeadStores":             2,
	"handler_sling.go Server.sourceWorkflowStores CityBeadStore":          1,
	"handler_status.go Server.statusSessionSnapshot CityBeadStore":        1,
	"handler_status.go Server.statusSessionSnapshot ScopedStoreLike":      1,
	"handler_status.go Server.statusWorkCounts BeadStores":                1,
	"handler_status.go Server.statusWorkCounts CityBeadStore":             1,
	"handler_status.go statusListStoreWithTimeout ScopedStoreLike":        1,
	"handler_status.go statusReadyStoreWithTimeout ScopedStoreLike":       1,
	"huma_handlers_beads.go Server.humaHandleBeadList BeadStores":         1,
	"huma_handlers_beads.go Server.humaHandleBeadReady BeadStores":        1,
	"huma_handlers_beads.go Server.humaHandleBeadReady CityBeadStore":     1,
	"huma_handlers_orders.go orderStoreInfosForState BeadStore":           1,
	"huma_handlers_orders.go orderStoreInfosForState CityBeadStore":       1,
	"rig_graph_legs.go workflowGraphStoreInfo CityBeadStore":              1,
	"session_resolution.go Server.retiredSessionWorkStores BeadStores":    1,
	"session_resolution.go Server.retiredSessionWorkStores CityBeadStore": 1,
	"store_health.go Server.computeStoreHealth CityBeadStore":             1,
	"work_record_close_gate.go Server.workRecordLegacyRepoDir BeadStore":  1,
}

// TestStateResidueAccessorCallers holds the production uses of State's raw
// store accessors (BeadStore, BeadStores, CityBeadStore, ScopedStoreLike and
// ClassBindingHasLegacyResidents) to reviewedResidueCallers. It matches every
// selector of those names, calls and method values alike, over every non-test
// file, whatever its build tags.
func TestStateResidueAccessorCallers(t *testing.T) {
	residue := map[string]bool{"BeadStore": true, "BeadStores": true, "CityBeadStore": true, "ScopedStoreLike": true, "ClassBindingHasLegacyResidents": true}
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	fset := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				recv := fn.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if id, ok := recv.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				// Every selector, not only calls: a method value handed
				// on (s.state.ClassBindingHasLegacyResidents) reaches the
				// accessor as surely as a call does.
				if sel, ok := n.(*ast.SelectorExpr); ok && residue[sel.Sel.Name] {
					got[path+" "+name+" "+sel.Sel.Name]++
				}
				return true
			})
		}
	}
	for key, n := range got {
		if want := reviewedResidueCallers[key]; n > want {
			t.Errorf("%s: %d call(s), the table allows %d: take the class handle from Stores()", key, n, want)
		}
	}
	for key, want := range reviewedResidueCallers {
		if n := got[key]; n < want {
			t.Errorf("%s: %d call(s), the table lists %d: shrink the reviewed table", key, n, want)
		}
	}
}

// TestResidueReadsTakeEachClassFromStores pins the T7 residue that unwraps a
// class handle from Stores() to the class it names, on a city that relocates
// graph, sessions, orders and nudges to four distinct stores: the API's
// residency topology (the minted one since T7c) and the retired-session
// sweep's graph leg.
func TestResidueReadsTakeEachClassFromStores(t *testing.T) {
	f := newFakeState(t)
	f.cityBeadStore = beads.NewMemStore()
	graph, sessions, orders, nudges := beads.NewMemStore(), beads.NewMemStore(), beads.NewMemStore(), beads.NewMemStore()
	f.graphBeadStore, f.sessionsBeadStore, f.ordersBeadStore, f.nudgesBeadStore = graph, sessions, orders, nudges
	s := New(f)

	want := map[coordclass.Class]beads.Store{
		coordclass.ClassGraph:    graph,
		coordclass.ClassSessions: sessions,
		coordclass.ClassOrders:   orders,
		coordclass.ClassNudges:   nudges,
	}
	topo := f.Stores().Topology()
	for class, store := range want {
		if binding, ok := topo.BindingFor(class); !ok || binding.Leg.Store != store {
			t.Errorf("Stores().Topology(): %s is served from %+v, want a binding over %v", class, binding.Leg, store)
		}
	}

	swept := s.retiredSessionWorkStores(sessions)
	if !slices.ContainsFunc(swept, func(st beads.Store) bool { return beads.SameStore(st, graph) }) {
		t.Errorf("the retired-session sweep lists %v, want the graph binding among them", swept)
	}
	if slices.ContainsFunc(swept, func(st beads.Store) bool { return beads.SameStore(st, orders) }) {
		t.Errorf("the retired-session sweep lists %v, want no orders binding: orders hold no assigned work", swept)
	}
}
