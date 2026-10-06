package storeref

// The shared binding builder, at the level it is now shared.
//
// The plane tests (cmd/gc, internal/api) pin what each plane ASKS FOR. These
// pin the answer, and they cover the two properties neither plane test could
// reach while the body was written out twice: a city with more than one binding
// (both plane fixtures carry exactly one), and the standing refusal, which the
// API plane's copy collected but never asserted.

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// bindingTestRefusal is a store that declares itself a standing refusal.
type bindingTestRefusal struct {
	beads.Store
	err error
}

func (s bindingTestRefusal) StorageRefusal() error { return s.err }

// bindingTestMinter names the namespace it mints into, which beads.MemStore
// does not.
type bindingTestMinter struct {
	beads.Store
	prefix string
}

func (s bindingTestMinter) IDPrefix() string { return s.prefix }

// TestBuildBindingsDefaultsPessimisticallyOnBothHalves is the invariant that
// made sharing the body worth doing.
//
// Neither half of the retirement condition may be optimistic when it was not
// observed, and neither failure is visible at the call site: a wrong TRUE
// retires the residence probe, and a retired probe answers "no such bead" for
// every id the split preserved rather than raising anything.
func TestBuildBindingsDefaultsPessimisticallyOnBothHalves(t *testing.T) {
	store := beads.NewMemStore()
	bindings, refused := BuildBindings(
		[]beads.Store{store},
		map[beads.Store][]coordclass.Class{store: {coordclass.ClassGraph}},
		BindingOptions{},
	)
	if refused != nil {
		t.Fatalf("unexpected refusal: %v", refused)
	}
	if len(bindings) != 1 {
		t.Fatalf("got %d bindings, want 1", len(bindings))
	}
	if bindings[0].MintsReserved {
		t.Error("MintsReserved = true for a store that declares no mint prefix; nothing observed it minting inside the namespace")
	}
	if !bindings[0].HasLegacyResidents {
		t.Error("HasLegacyResidents = false with no census supplied; an unasked question is not a clean answer")
	}
	if bindings[0].probeRetired() {
		t.Error("the residence probe retired on a binding nothing was verified about")
	}
}

// TestBuildBindingsObservesAMintingStore is the control for the row above: the
// pessimistic default has to be a DEFAULT, not a constant.
func TestBuildBindingsObservesAMintingStore(t *testing.T) {
	graph, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatal("graph has no reserved mint prefix")
	}
	store := bindingTestMinter{Store: beads.NewMemStore(), prefix: graph}
	bindings, _ := BuildBindings(
		[]beads.Store{store},
		map[beads.Store][]coordclass.Class{store: {coordclass.ClassGraph}},
		BindingOptions{Relics: func(beads.Store) bool { return false }},
	)
	if len(bindings) != 1 {
		t.Fatalf("got %d bindings, want 1", len(bindings))
	}
	if !bindings[0].probeRetired() {
		t.Errorf("a binding that mints truthfully (%v) and censused clean (%v) did not retire its probe; the defaults above are unfalsifiable",
			bindings[0].MintsReserved, bindings[0].HasLegacyResidents)
	}
}

// TestBuildBindingsRaisesThePessimisticBitOnProof pins the one clause that
// makes the two relic bits impossible to disagree.
//
// A durable record naming this binding is PROOF of what the pessimistic bit only
// guesses at, so a live census that answered "clean" against it is a census that
// could not read the store — never grounds to lower the bit. Without the raise,
// a build whose Relics predicate answers clean (the ga-dx4ho namespace predicate
// is the one on the way) would produce MintsReserved+clean and retire the probe
// on a binding proven to hold relics, and probeRefusalPolicy would never be
// consulted at all.
func TestBuildBindingsRaisesThePessimisticBitOnProof(t *testing.T) {
	graph, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatal("graph has no reserved mint prefix")
	}
	store := bindingTestMinter{Store: beads.NewMemStore(), prefix: graph}
	bindings, _ := BuildBindings(
		[]beads.Store{store},
		map[beads.Store][]coordclass.Class{store: {coordclass.ClassGraph}},
		BindingOptions{
			Relics:      func(beads.Store) bool { return false },
			KnownRelics: func(StoreRef) bool { return true },
		},
	)
	if len(bindings) != 1 {
		t.Fatalf("got %d bindings, want 1", len(bindings))
	}
	if !bindings[0].HasLegacyResidents {
		t.Error("a binding a durable record PROVES holds relics reports HasLegacyResidents = false; the proof did not raise the pessimistic bit, so a clean live census can still retire its probe")
	}
	if bindings[0].probeRetired() {
		t.Error("the residence probe retired on a binding proven to hold relics; every id the migration preserved there becomes unreachable")
	}
}

// TestProbeRetirementIgnoresNeitherRelicBit is the same invariant enforced where
// it is READ, for a ClassBinding that never went through BuildBindings.
//
// The builder's raise keeps the FIELD honest; this keeps the DECISION safe.
// storeref exports ClassBinding, so a plane assembling one by hand — the CLI
// already does, for its refused-city topology — can spell the contradictory
// state the builder cannot produce, and the failure would be silent: a retired
// probe answers "no such bead" for exactly the ids the proof is about.
func TestProbeRetirementIgnoresNeitherRelicBit(t *testing.T) {
	if (ClassBinding{MintsReserved: true, KnownLegacyResidents: true}).probeRetired() {
		t.Error("a hand-built binding with proof but no pessimistic bit retired its probe; proof must retire nothing")
	}
	if (ClassBinding{MintsReserved: true, HasLegacyResidents: true}).probeRetired() {
		t.Error("a binding carrying the pessimistic bit retired its probe")
	}
	if !(ClassBinding{MintsReserved: true}).probeRetired() {
		t.Error("a mint-truthful binding with neither relic bit did not retire; the two checks above are unfalsifiable")
	}
}

// TestBuildBindingsOrdersByRefRegardlessOfInput covers the difference the two
// copies had drifted into: the CLI plane sorted, the API plane returned map
// order fixed by whatever sequence its accessors happened to be listed in.
//
// A plan is built by walking these in order, so an arrangement that reported
// its bindings in a different sequence on two planes would probe the same id in
// a different sequence — and on a city where two bindings both claim a
// namespace, resolve it to a different store.
func TestBuildBindingsOrdersByRefRegardlessOfInput(t *testing.T) {
	sessions := beads.NewMemStore()
	graph := beads.NewMemStore()
	byStore := map[beads.Store][]coordclass.Class{
		sessions: {coordclass.ClassSessions},
		graph:    {coordclass.ClassGraph},
	}
	wantRefs := []StoreRef{ClassRef([]coordclass.Class{coordclass.ClassGraph}), ClassRef([]coordclass.Class{coordclass.ClassSessions})}

	for _, order := range [][]beads.Store{{sessions, graph}, {graph, sessions}} {
		bindings, _ := BuildBindings(order, byStore, BindingOptions{})
		if len(bindings) != 2 {
			t.Fatalf("got %d bindings, want 2", len(bindings))
		}
		for i, want := range wantRefs {
			if bindings[i].Leg.Ref != want {
				t.Errorf("binding %d is %q, want %q — the order depends on the caller's input order", i, bindings[i].Leg.Ref, want)
			}
		}
	}
}

// TestBuildBindingsReportsAStandingRefusal pins that a refused city is reported
// as refused rather than as a servable binding whose reads happen to fail.
func TestBuildBindingsReportsAStandingRefusal(t *testing.T) {
	boom := errors.New("storage is configured for a split this build cannot serve")
	store := bindingTestRefusal{Store: beads.NewMemStore(), err: boom}
	bindings, refused := BuildBindings(
		[]beads.Store{store},
		map[beads.Store][]coordclass.Class{store: {coordclass.ClassGraph}},
		BindingOptions{},
	)
	if len(bindings) != 1 {
		t.Fatalf("got %d bindings, want 1", len(bindings))
	}
	if !errors.Is(refused, boom) {
		t.Errorf("refused = %v, want the store's own refusal; a caller that cannot see it degrades to a work-only answer", refused)
	}
}

// TestBuildBindingsOnNoRelocatedClassesIsSingleStore pins the empty case as the
// identity fast path rather than as a topology carrying an empty binding list
// that IsSingleStore would have to special-case.
func TestBuildBindingsOnNoRelocatedClassesIsSingleStore(t *testing.T) {
	bindings, refused := BuildBindings(nil, nil, BindingOptions{})
	if bindings != nil || refused != nil {
		t.Fatalf("BuildBindings(nil) = %v, %v; want nil, nil", bindings, refused)
	}
	if !(Topology{Bindings: bindings}).IsSingleStore() {
		t.Error("a city relocating nothing did not read as single-store")
	}
}

// TestBuildBindingsCompletesClassesBeforeDerivingTheRef pins the ordering the
// API plane depends on: the blind-spot correction has to run BEFORE the ref and
// the namespace union are derived from the class set, or the plane names a
// different binding than the others for the same store.
func TestBuildBindingsCompletesClassesBeforeDerivingTheRef(t *testing.T) {
	store := beads.NewMemStore()
	observed := []coordclass.Class{coordclass.ClassGraph}
	complete := []coordclass.Class{coordclass.ClassGraph, coordclass.ClassNudges}

	bindings, _ := BuildBindings(
		[]beads.Store{store},
		map[beads.Store][]coordclass.Class{store: observed},
		BindingOptions{CompleteClasses: func([]coordclass.Class) []coordclass.Class { return complete }},
	)
	if len(bindings) != 1 {
		t.Fatalf("got %d bindings, want 1", len(bindings))
	}
	if got, want := bindings[0].Leg.Ref, ClassRef(complete); got != want {
		t.Errorf("ref = %q, want %q — derived from the observed set, not the completed one", got, want)
	}
	for _, want := range config.ReservedClassPrefixesFor(config.BeadClassNudges) {
		if !idInAnyNamespace(want+"-x", bindings[0].Prefixes) {
			t.Errorf("binding declares %v, missing the %q namespace a completed class brought with it", bindings[0].Prefixes, want)
		}
	}
}
