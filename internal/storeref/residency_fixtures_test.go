package storeref

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// The seven topologies of the conformance corpus. They are values, not
// fixtures with I/O, because the resolver is pure: a row is (intent, topology)
// -> Plan.String(), and nothing in this file opens a store.
//
//	T0  single-store city (no binding, no rig)
//	T1  whole split: one binding carrying all five infrastructure classes
//	T2  T1 plus two rigs
//	T3  standing refusal (the deleted-[storage] trap)
//	T3k T3 whose binding is PROVEN to have held work-prefixed relics
//	T4  T2 with one rig suspended — the constructor excluded it
//	T5  per-class split: graph and sessions on two DIFFERENT bindings
//	T6  T1 with a mint-truthful binding (the section-5 retirement shape)
//	T6r T6 with legacy residents still open — the retirement's other half
const (
	cityPrefix  = "ga"
	alphaPrefix = "ra"
	bravoPrefix = "rb"
)

// infraClasses is the class set a whole-split binding carries, and
// infraPrefixes the reserved id namespaces it holds beads under. They are
// stated here rather than read from internal/config because storeref is a leaf:
// a caller supplies the prefixes, so the corpus supplies them too.
//
// "gcnq" is in the list and is not a fifth-class mint prefix: it is the nudge
// queue's own namespace inside the nudges binding, minted by a subsystem rather
// than by the store's sequence. The resolver draws no distinction — a namespace
// a binding holds is a namespace it has authority over — and that is the
// property these rows pin.
var (
	infraClasses  = []coordclass.Class{coordclass.ClassGraph, coordclass.ClassMessaging, coordclass.ClassSessions, coordclass.ClassOrders, coordclass.ClassNudges}
	infraPrefixes = []string{"gcg", "gcm", "gcs", "gco", "gcn", "gcnq"}
)

// errRefused stands in for the standing storage refusal a refused city's boot
// takes. It implements the marker the resolver keys the tolerated-non-fault
// carve-out on, so T3 exercises the real predicate rather than a stub.
type refusalError struct{ msg string }

func (e refusalError) Error() string         { return e.msg }
func (refusalError) StandingStorageRefusal() {}
func newRefusal() error {
	return refusalError{msg: "storage refused: run `gc storage migrate`"}
}

// namedStore is a beads.Store that reports which leg it is, so an agreement or
// executor assertion can name the store it got back instead of comparing
// opaque pointers.
type namedStore struct {
	*beads.MemStore
	name string
	// gets counts Get calls — the probe counter the identity fast-path row
	// and its control are measured with.
	gets *int
	// getErr, when non-nil, is returned by every Get instead of reading.
	getErr error
	// listErr, when non-nil, is returned by every ListOpen instead of reading.
	listErr error
}

func newNamedStore(name string) *namedStore {
	n := 0
	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	return &namedStore{MemStore: mem, name: name, gets: &n}
}

func (s *namedStore) Get(id string) (beads.Bead, error) {
	*s.gets++
	if s.getErr != nil {
		return beads.Bead{}, s.getErr
	}
	return s.MemStore.Get(id)
}

func (s *namedStore) ListOpen(status ...string) ([]beads.Bead, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.MemStore.ListOpen(status...)
}

func (s *namedStore) seed(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := s.Create(beads.Bead{ID: id, Title: id, Type: "task"}); err != nil {
			t.Fatalf("seed %q into %s: %v", id, s.name, err)
		}
	}
}

// storeNameOf renders a resolved store for a failure message.
func storeNameOf(s beads.Store) string {
	if named, ok := s.(*namedStore); ok {
		return named.name
	}
	if s == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%T", s)
}

// topoFixture is a topology plus the stores behind its legs, so an executor
// test can seed a leg and then name the leg it got back.
type topoFixture struct {
	name     string
	topo     Topology
	work     *namedStore
	rigs     map[string]*namedStore
	bindings map[StoreRef]*namedStore
}

func (f topoFixture) legStore(ref StoreRef) *namedStore {
	if ref == WorkRef {
		return f.work
	}
	if s, ok := f.bindings[ref]; ok {
		return s
	}
	return f.rigs[strings.TrimPrefix(string(ref), "rig:")]
}

// totalGets is the probe count across every leg — the measurement the identity
// fast-path row makes.
func (f topoFixture) totalGets() int {
	n := *f.work.gets
	for _, s := range f.rigs {
		n += *s.gets
	}
	for _, s := range f.bindings {
		n += *s.gets
	}
	return n
}

func (f topoFixture) resetGets() {
	*f.work.gets = 0
	for _, s := range f.rigs {
		*s.gets = 0
	}
	for _, s := range f.bindings {
		*s.gets = 0
	}
}

type bindingSpec struct {
	classes  []coordclass.Class
	prefixes []string
	mints    bool
	relics   bool
	known    bool
	refusing bool
}

// buildTopology assembles a fixture. rigs is name->prefix.
func buildTopology(name string, rigs map[string]string, specs []bindingSpec, refused error) topoFixture {
	f := topoFixture{
		name:     name,
		work:     newNamedStore("work"),
		rigs:     map[string]*namedStore{},
		bindings: map[StoreRef]*namedStore{},
	}
	f.topo.Work = Leg{Ref: WorkRef, Store: f.work, Prefix: cityPrefix}
	f.topo.Refused = refused

	names := make([]string, 0, len(rigs))
	for n := range rigs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		s := newNamedStore("rig:" + n)
		f.rigs[n] = s
		f.topo.Rigs = append(f.topo.Rigs, Leg{Ref: RigRef(n), Store: s, Prefix: rigs[n]})
	}

	for _, spec := range specs {
		ref := ClassRef(spec.classes)
		s := newNamedStore(string(ref))
		if spec.refusing {
			s.getErr = refused
			s.listErr = refused
		}
		f.bindings[ref] = s
		f.topo.Bindings = append(f.topo.Bindings, ClassBinding{
			Classes:              append([]coordclass.Class(nil), spec.classes...),
			Prefixes:             append([]string(nil), spec.prefixes...),
			Leg:                  Leg{Ref: ref, Store: s},
			MintsReserved:        spec.mints,
			HasLegacyResidents:   spec.relics,
			KnownLegacyResidents: spec.known,
		})
	}
	return f
}

func wholeSplit() bindingSpec {
	return bindingSpec{classes: infraClasses, prefixes: infraPrefixes}
}

func newT0() topoFixture { return buildTopology("T0", nil, nil, nil) }
func newT1() topoFixture { return buildTopology("T1", nil, []bindingSpec{wholeSplit()}, nil) }

func newT2() topoFixture {
	return buildTopology("T2", map[string]string{"alpha": alphaPrefix, "bravo": bravoPrefix}, []bindingSpec{wholeSplit()}, nil)
}

func newT3() topoFixture {
	refusal := newRefusal()
	spec := wholeSplit()
	spec.refusing = true
	return buildTopology("T3", nil, []bindingSpec{spec}, refusal)
}

// newT3Known is T3 whose binding a durable census PROVED holds ids outside its
// reserved namespaces. The refusal is the same; what changes is that the
// tolerated-refusal rationale — "this leg was only ever a residence probe for an
// id no relocated class could own" — is known to be false here.
func newT3Known() topoFixture {
	refusal := newRefusal()
	spec := wholeSplit()
	spec.refusing = true
	spec.relics = true
	spec.known = true
	return buildTopology("T3k", nil, []bindingSpec{spec}, refusal)
}

// newT4 is T2 with the bravo rig suspended. The constructor is TOLD which rigs
// to include; it does not decide. So a suspended rig is simply absent, and the
// row proves the resolver never re-invents it.
func newT4() topoFixture {
	return buildTopology("T4", map[string]string{"alpha": alphaPrefix}, []bindingSpec{wholeSplit()}, nil)
}

// newT5 is the per-class split the runtime cannot SERVE yet but the resolver
// must already answer for. It is asserted live rather than skipped: a
// skip-until row rots, and the tripwire this replaces lived in two files.
func newT5() topoFixture {
	return buildTopology("T5", nil, []bindingSpec{
		{classes: []coordclass.Class{coordclass.ClassGraph}, prefixes: []string{"gcg"}},
		{classes: []coordclass.Class{coordclass.ClassSessions}, prefixes: []string{"gcs"}},
	}, nil)
}

func newT6() topoFixture {
	spec := wholeSplit()
	spec.mints = true
	return buildTopology("T6", nil, []bindingSpec{spec}, nil)
}

func newT6Relics() topoFixture {
	spec := wholeSplit()
	spec.mints = true
	spec.relics = true
	return buildTopology("T6r", nil, []bindingSpec{spec}, nil)
}

func allTopologies() []topoFixture {
	return []topoFixture{newT0(), newT1(), newT2(), newT3(), newT3Known(), newT4(), newT5(), newT6(), newT6Relics()}
}

// planString renders a row: the plan, or "error: <msg>" when the intent cannot
// be planned over the topology. Both are pinned, because a refusal that stopped
// naming its remedy is exactly the regression the T3 rows exist for.
func planString(t *testing.T, i Intent, topo Topology) string {
	t.Helper()
	p, err := Plan(i, topo)
	if err != nil {
		return "error: " + err.Error()
	}
	return p.String()
}

func mustPlan(t *testing.T, i Intent, topo Topology) ResolvedPlan {
	t.Helper()
	p, err := Plan(i, topo)
	if err != nil {
		t.Fatalf("Plan(%T) over topology: unexpected error: %v", i, err)
	}
	return p
}
