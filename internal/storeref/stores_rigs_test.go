package storeref

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// rigTopologies are the topology shapes the rig scope must answer over, each
// with two rigs: single store, whole split, refused, per-class split with
// graph relocated, and per-class split with graph left on the work stores.
func rigTopologies() []topoFixture {
	rigs := map[string]string{"alpha": alphaPrefix, "bravo": bravoPrefix}
	refused := wholeSplit()
	refused.refusing = true
	return []topoFixture{
		buildTopology("single", rigs, nil, nil),
		newT2(),
		buildTopology("refused", rigs, []bindingSpec{refused}, newRefusal()),
		buildTopology("graph-only", rigs, []bindingSpec{{classes: []coordclass.Class{coordclass.ClassGraph}, prefixes: []string{"gcg"}, relics: true}}, nil),
		buildTopology("sessions-only", rigs, []bindingSpec{{classes: []coordclass.Class{coordclass.ClassSessions}, prefixes: []string{"gcs"}, relics: true}}, nil),
	}
}

// wantRigStore is the oracle for a rig scope, stated without Place: the work
// store is the rig's own, through the projection's work view; the graph store
// is the plane's graph refusal, else the binding serving graph (the refusing
// store on a refused city), else the rig's own store through the class view.
func wantRigStore(f topoFixture, opts StoresOptions, rig string, c coordclass.Class) beads.Store {
	own := beads.Store(f.rigs[rig])
	if c == coordclass.ClassWork {
		if opts.Projection != nil {
			return opts.Projection.WorkView(own)
		}
		return own
	}
	if refusal := opts.ClassRefusals[c]; refusal != nil {
		return refusal
	}
	if b, ok := f.topo.BindingFor(c); ok {
		return b.Leg.Store
	}
	if opts.Projection != nil {
		return opts.Projection.ClassView(own)
	}
	return own
}

// TestStoresRigScopes is the rig-scope table (typed-stores T6a, OD-7): for
// every topology shape, option set (none, projection, a per-class graph
// refusal), scope (city, each rig, an unknown rig, a binding ref) and class
// the rig scope has (work, graph), the handle wraps the store the placement
// rule names, Rigs lists every rig in the topology's order, and an unknown
// scope answers false.
func TestStoresRigScopes(t *testing.T) {
	refusal := refusingStore{beads.NewMemStore()}
	optSets := map[string]StoresOptions{
		"plain":          {},
		"projection":     {Projection: taggedProjection{}},
		"graph refused":  {ClassRefusals: map[coordclass.Class]RefusingClassStore{coordclass.ClassGraph: refusal}},
		"orders refused": {ClassRefusals: map[coordclass.Class]RefusingClassStore{coordclass.ClassOrders: refusal}},
	}
	for _, f := range rigTopologies() {
		for optName, opts := range optSets {
			s := NewStores(f.topo, opts)
			name := f.name + "/" + optName

			city, ok := s.GraphFor(WorkRef)
			if !ok || city != s.Graph() {
				t.Errorf("%s: GraphFor(city) = %v, %v; want the city graph handle", name, city.Store, ok)
			}

			var listed []string
			for _, r := range s.Rigs() {
				listed = append(listed, r.Name)
				if work, _ := s.Rig(r.Name); r.Work != work {
					t.Errorf("%s: Rigs()[%s] and Rig(%s) disagree", name, r.Name, r.Name)
				}
			}
			if want := []string{"alpha", "bravo"}; !slices.Equal(listed, want) {
				t.Errorf("%s: Rigs() = %v, want %v", name, listed, want)
			}

			for _, rig := range []string{"alpha", "bravo"} {
				work, ok := s.Rig(rig)
				if want := wantRigStore(f, opts, rig, coordclass.ClassWork); !ok || work.Store != want {
					t.Errorf("%s: Rig(%s) = %v, %v; want %v", name, rig, work.Store, ok, want)
				}
				graph, ok := s.GraphFor(RigRef(rig))
				if want := wantRigStore(f, opts, rig, coordclass.ClassGraph); !ok || graph.Store != want {
					t.Errorf("%s: GraphFor(rig:%s) = %v, %v; want %v", name, rig, graph.Store, ok, want)
				}
				if want := Place(Topology{Work: Leg{Ref: RigRef(rig), Store: f.rigs[rig]}, Bindings: f.topo.Bindings, Refused: f.topo.Refused}, opts, coordclass.ClassGraph); graph.Store != want {
					t.Errorf("%s: GraphFor(rig:%s) wraps %v, Place over the rig frame answers %v", name, rig, graph.Store, want)
				}
			}

			if _, ok := s.Rig("nope"); ok {
				t.Errorf("%s: Rig(an unknown rig) answered", name)
			}
			if _, ok := s.GraphFor(RigRef("nope")); ok {
				t.Errorf("%s: GraphFor(an unknown rig) answered", name)
			}
			for _, b := range f.topo.Bindings {
				if _, ok := s.GraphFor(b.Leg.Ref); ok {
					t.Errorf("%s: GraphFor(a binding ref %s) answered; a binding is no scope", name, b.Leg.Ref)
				}
			}
		}
	}
}

// TestStoresRigScopesSkipNoRig pins the two degenerate inputs: a topology with
// no rig legs has no rig scope, and a leg whose ref names no rig is skipped
// rather than minted under an empty name.
func TestStoresRigScopesSkipNoRig(t *testing.T) {
	f := newT0()
	if got := NewStores(f.topo, StoresOptions{}).Rigs(); len(got) != 0 {
		t.Errorf("a city with no rigs lists %v", got)
	}
	f.topo.Rigs = []Leg{{Ref: WorkRef, Store: beads.NewMemStore()}, {Ref: "rig:", Store: beads.NewMemStore()}}
	if got := NewStores(f.topo, StoresOptions{}).Rigs(); len(got) != 0 {
		t.Errorf("legs naming no rig were minted as %v", got)
	}
}
