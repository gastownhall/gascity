package storeref

// The relic census, and the direction it is allowed to be wrong in.
//
// HasLegacyResidents is the half of the retirement condition that a
// point-in-time read answers, so every row here is really about one question:
// when the census cannot see clearly, which way does it fall? True keeps the
// probe and costs one Get per out-of-namespace by-id read. False retires the
// probe and strands every bead the migration carried across under its original
// id — the ga-axin6 shape. The error rows are therefore not edge cases; they
// are the point.

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// censusStore is a binding store whose List can be made to fail, so the
// fail-safe direction is testable rather than asserted.
type censusStore struct {
	*beads.MemStore
	listErr error
}

func newCensusStore() *censusStore {
	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	return &censusStore{MemStore: mem}
}

func (s *censusStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.MemStore.List(q)
}

func (s *censusStore) seedBead(t *testing.T, id string) {
	t.Helper()
	if _, err := s.Create(beads.Bead{ID: id, Title: id, Type: "task"}); err != nil {
		t.Fatalf("seeding %q: %v", id, err)
	}
}

func censusBinding(store beads.Store) ClassBinding {
	return ClassBinding{
		Classes:  infraClasses,
		Prefixes: infraPrefixes,
		Leg:      Leg{Ref: ClassRef(infraClasses), Store: store},
	}
}

func TestOpenLegacyResidentsFindsAMigratedID(t *testing.T) {
	store := newCensusStore()
	store.seedBead(t, "gcg-1")
	store.seedBead(t, "ga-relic")

	relics, err := OpenLegacyResidents(store, infraPrefixes)
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if len(relics) != 1 || relics[0] != "ga-relic" {
		t.Fatalf("census reported %v, want just the work-shaped id the migration carried across", relics)
	}
	if !HasOpenLegacyResidents(censusBinding(store)) {
		t.Error("a binding holding an open relic reported none; its probe would retire and the relic would be unreadable")
	}
}

func TestOpenLegacyResidentsIgnoresEveryNamespaceTheBindingHolds(t *testing.T) {
	// Both halves of "holds": the prefix each class mints under, and the
	// auxiliary the nudge queue pins. A census that knew only the mint
	// prefixes would count every queued nudge as a relic and keep the probe
	// alive forever on a perfectly clean city.
	store := newCensusStore()
	for _, id := range []string{"gcg-1", "gcm-2", "gcs-3", "gco-4", "gcn-5", "gcnq-6"} {
		store.seedBead(t, id)
	}

	relics, err := OpenLegacyResidents(store, infraPrefixes)
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if len(relics) != 0 {
		t.Fatalf("census reported %v as relics; every one of those namespaces is one this binding declares", relics)
	}
	if HasOpenLegacyResidents(censusBinding(store)) {
		t.Error("a binding holding only its own ids reported relics")
	}
}

func TestOpenLegacyResidentsIgnoresAClosedRelic(t *testing.T) {
	// The retirement blocker is a relic that can still be READ BY ID, and the
	// operational story is watching that population drain as the beads close.
	// Counting closed rows would pin the probe to a city's whole history.
	store := newCensusStore()
	store.seedBead(t, "ga-done")
	if err := store.Close("ga-done"); err != nil {
		t.Fatalf("closing the relic: %v", err)
	}

	relics, err := OpenLegacyResidents(store, infraPrefixes)
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if len(relics) != 0 {
		t.Fatalf("census reported %v; a closed relic is not a retirement blocker", relics)
	}
}

func TestOpenLegacyResidentsReportsAnEmptyBindingClean(t *testing.T) {
	if HasOpenLegacyResidents(censusBinding(newCensusStore())) {
		t.Error("an empty binding reported relics; nothing is what a fresh born-split city holds")
	}
}

// The fail-safe control. An unreadable binding has told us nothing, and
// "nothing" must not read as "clean".
func TestUnreadableBindingKeepsItsProbe(t *testing.T) {
	store := newCensusStore()
	store.listErr = errors.New("binding unreachable")

	if _, err := OpenLegacyResidents(store, infraPrefixes); err == nil {
		t.Fatal("a failing List produced no census error; the caller cannot tell a clean binding from an unread one")
	}
	if !HasOpenLegacyResidents(censusBinding(store)) {
		t.Error("an unreadable binding was reported clean; a census that cannot read must not retire a probe")
	}
}

// The refused city takes the same branch, and it matters that it does: its
// binding store answers every read with the standing refusal, which is the
// least informative answer there is.
func TestRefusedBindingKeepsItsProbe(t *testing.T) {
	store := newCensusStore()
	store.listErr = newRefusal()

	if !HasOpenLegacyResidents(censusBinding(store)) {
		t.Error("a refused city's binding was reported clean")
	}
}

func TestCensusWithoutAStoreKeepsItsProbe(t *testing.T) {
	if _, err := OpenLegacyResidents(nil, infraPrefixes); err == nil {
		t.Fatal("censusing a nil store succeeded")
	}
	if !HasOpenLegacyResidents(ClassBinding{Classes: infraClasses, Prefixes: infraPrefixes}) {
		t.Error("a binding with no store was reported clean")
	}
}

// A binding that claims no namespace holds nothing it can recognize, so every
// resident is a relic. That is the honest answer and it pairs with the mint
// bit's own control: such a binding never mints truthfully either, so its
// probe was never retiring anyway.
func TestBindingClaimingNoNamespaceCountsEveryResident(t *testing.T) {
	store := newCensusStore()
	store.seedBead(t, "gcg-1")

	relics, err := OpenLegacyResidents(store, nil)
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if len(relics) != 1 {
		t.Fatalf("census reported %v, want the one resident no declared namespace covers", relics)
	}
}

// The census reads the binding and nothing else. A topology's work ledger is
// full of work-shaped ids by definition, and reading it would report every
// city on earth as relic-bound.
func TestCensusReadsOnlyTheBinding(t *testing.T) {
	binding := newCensusStore()
	binding.seedBead(t, "gcg-1")
	work := newCensusStore()
	work.seedBead(t, "ga-1")

	if HasOpenLegacyResidents(ClassBinding{
		Classes:  []coordclass.Class{coordclass.ClassGraph},
		Prefixes: []string{"gcg"},
		Leg:      Leg{Ref: ClassRef([]coordclass.Class{coordclass.ClassGraph}), Store: binding},
	}) {
		t.Error("the census reported relics for a clean binding; it must read the binding leg alone")
	}
}
