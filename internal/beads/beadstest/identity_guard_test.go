package beadstest

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// identityFixture is TestScanStoreIdentity's input package: every shape the
// store-identity guard must flag, and the ones it must not. It is written to a
// temporary directory rather than kept as a source file, so no build compiles
// (or lints) its deliberate raw stores.
const identityFixture = `package fixture

import (
	"slices"

	b "github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

type holder struct {
	store  b.Store
	graph  b.GraphStore
	legs   map[string]b.Store
	name   string
	Work   string // a field of this package named like a class accessor: it must not hide RigWork.Work
}

type state interface {
	Mail() string // a same-named method of this package that is no store
	CityBeadStore() b.Store
	GraphBeadStore() b.GraphStore
	BeadStores() map[string]b.Store
}

func cityOf(st state) b.Store { return st.CityBeadStore() }

// flagged holds 7 identity comparisons and 1 store-keyed map.
func flagged(st state, h holder, w b.WorkStore, m b.MoleculeTarget, n b.MoleculeTarget) {
	graph := st.GraphBeadStore().Store
	_ = graph == st.CityBeadStore()  // 1: unwrap vs accessor
	_ = h.store != cityOf(st)        // 2: field vs package function
	_ = w == b.WorkStore{Store: nil} // 3: a non-empty handle literal
	for _, leg := range st.BeadStores() {
		_ = leg == h.graph // 4: range value vs handle field
	}
	_ = h.legs["x"] == h.store // 5: map index vs field
	_ = m != n                 // 6: molecule targets
	other, ok := h.legs["y"]
	_ = ok && other == w.Store // 7: comma-ok index vs handle's store
	_ = map[b.Store]bool{}     // map-key
}

// crossPackage holds 5 identity comparisons of handles another package hands
// out (storeref.Stores is not declared here): a class accessor's .Store, two
// class accessors' handles, a RigWork's Work field, and the first results of
// Rig and GraphFor.
func crossPackage(rs, other storeref.Stores, w b.WorkStore) {
	_ = rs.Graph().Store == rs.Work().Store
	_ = rs.Graph() != other.Graph()
	// Each operand below is store-typed only by the rule it pins.
	for _, r := range rs.Rigs() {
		for _, o := range other.Rigs() {
			_ = r.Work == o.Work // storeref.RigWork's handle
		}
	}
	rig, _ := rs.Rig("x")
	otherRig, _ := other.Rig("x")
	_ = rig == otherRig
	g, _ := rs.GraphFor("rig:x")
	otherG, _ := other.GraphFor("rig:x")
	_ = g == otherG
	_ = w
}

// switched holds 1 switch on a store.
func switched(h holder, w b.WorkStore) {
	switch h.store {
	case w.Store:
	}
}

// slicesHelpers holds 3 slices calls that compare stores with ==.
func slicesHelpers(h holder, list, other []b.Store) {
	_ = slices.Contains(list, h.store)
	_ = slices.Index(list, h.store)
	_ = slices.Equal(list, other)
}

// clean compares nothing by store identity.
func clean(st state, h holder, w b.WorkStore) {
	_ = h.store == nil
	_ = w == (b.WorkStore{})
	_ = h.name == "x"
	_ = b.SameStore(h.store, st.CityBeadStore())
	_ = map[string]b.Store{}
	_ = slices.Contains([]string{h.name}, "x")
	_ = slices.ContainsFunc([]b.Store{h.store}, func(s b.Store) bool { return b.SameStore(s, w) })
	_ = st.Mail() == "x"
	switch h.name {
	case "x":
	}
}
`

// TestScanStoreIdentity pins what the store-identity guard counts, over a
// fixture holding each shape: == and != whose operand is a store, a class
// handle or a molecule target, typed from parameters, fields, range values,
// map indexes, the package's functions and methods, and another package's
// class accessors and .Store; a switch on a store; slices.Contains, Index and
// Equal over stores; and a map keyed by a store. Nil and zero-handle checks,
// SameStore, ContainsFunc, non-store slices and switches, and maps whose values
// are stores are not identity.
func TestScanStoreIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(identityFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ScanStoreIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for k, lines := range got {
		counts[k] = len(lines)
	}
	want := map[string]int{
		"fixture.go flagged eq":           7,
		"fixture.go flagged map-key":      1,
		"fixture.go crossPackage eq":      5,
		"fixture.go switched switch":      1,
		"fixture.go slicesHelpers slices": 3,
	}
	if !reflect.DeepEqual(counts, want) {
		t.Errorf("ScanStoreIdentity = %v (lines %v), want %v", counts, got, want)
	}
}
