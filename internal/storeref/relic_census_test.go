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
	"slices"
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
	if !HasLegacyResidents(censusBinding(store)) {
		t.Error("a binding holding a relic reported none; its probe would retire and the relic would be unreadable")
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
	if HasLegacyResidents(censusBinding(store)) {
		t.Error("a binding holding only its own ids reported relics")
	}
}

// The DRAIN report stays open-only, and this is the row that keeps it that way.
//
// OpenLegacyResidents is now only `gc storage status`'s count — the number an
// operator watches fall as the carried-across work closes. Retirement moved off
// it (see TestAClosedRelicKeepsTheProbe). Widening this one to match the
// verdict would be a natural-looking tidy-up that silently replaces a draining
// count with one that can never fall, and nothing else in the tree would fail.
func TestOpenLegacyResidentsIgnoresAClosedRelic(t *testing.T) {
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
		t.Fatalf("the drain count reported %v; a closed relic is drained, and an operator watching this number must see it reach zero", relics)
	}
}

// The soundness row for ga-qdt5y.19, and the one that fails against the rule
// this replaced.
//
// Closing a relic does not make it unreachable. `gc storage migrate` never
// deletes the work store's pre-migration copy, so if closing the last relic
// retired the probe, that id's next read would fall to the work axis and be
// served from a frozen copy that reads OPEN with pre-migration fields — a
// lifecycle regression that never heals, on `gc bd show`, `gc beads show`,
// `gc convoy` and the API State surface alike.
//
// Mutation that must make this fire: legacyResidents' IncludeClosed back to
// false, or the verdict pointed at OpenLegacyResidents.
func TestAClosedRelicKeepsTheProbe(t *testing.T) {
	store := newCensusStore()
	store.seedBead(t, "ga-done")
	if err := store.Close("ga-done"); err != nil {
		t.Fatalf("closing the relic: %v", err)
	}

	relics, err := LegacyResidents(store, infraPrefixes)
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if len(relics) != 1 {
		t.Fatalf("the census reported %v, want the closed relic; it is still read, reopened and claimed by id, and only this binding holds the live copy", relics)
	}
	if !HasLegacyResidents(censusBinding(store)) {
		t.Error("a binding whose only relic has closed was certified clean; the probe retires and every read of that id is answered by the migration's frozen copy")
	}
}

// The must-be-silent counterpart. Widening the census to closed beads must
// widen only the CLOSED half — the namespace filter still decides what counts.
//
// Without this row, a census that dropped the namespace check on closed rows
// would pass everything above: no other fixture holds a closed IN-namespace
// bead at census time, so the binding's own retired infrastructure would read
// as a relic and pin the probe on every converged city forever.
func TestAClosedInNamespaceBeadIsNotARelic(t *testing.T) {
	store := newCensusStore()
	store.seedBead(t, "gcg-1")
	if err := store.Close("gcg-1"); err != nil {
		t.Fatalf("closing the binding's own bead: %v", err)
	}

	relics, err := LegacyResidents(store, infraPrefixes)
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if len(relics) != 0 {
		t.Fatalf("the census reported %v; a closed bead inside the binding's own namespace is ordinary retired infrastructure, findable by prefix, and blocks nothing", relics)
	}
	if HasLegacyResidents(censusBinding(store)) {
		t.Error("a binding holding only its own closed beads kept its probe; retirement would never fire on any city that has ever closed an infrastructure bead")
	}
}

func TestOpenLegacyResidentsReportsAnEmptyBindingClean(t *testing.T) {
	if HasLegacyResidents(censusBinding(newCensusStore())) {
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
	if !HasLegacyResidents(censusBinding(store)) {
		t.Error("an unreadable binding was reported clean; a census that cannot read must not retire a probe")
	}
}

// The refused city takes the same branch, and it matters that it does: its
// binding store answers every read with the standing refusal, which is the
// least informative answer there is.
func TestRefusedBindingKeepsItsProbe(t *testing.T) {
	store := newCensusStore()
	store.listErr = newRefusal()

	if !HasLegacyResidents(censusBinding(store)) {
		t.Error("a refused city's binding was reported clean")
	}
}

func TestCensusWithoutAStoreKeepsItsProbe(t *testing.T) {
	if _, err := OpenLegacyResidents(nil, infraPrefixes); err == nil {
		t.Fatal("censusing a nil store succeeded")
	}
	if !HasLegacyResidents(ClassBinding{Classes: infraClasses, Prefixes: infraPrefixes}) {
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

// censusCapabilityStore is a binding store that can answer the verdict itself,
// instrumented so a test can tell WHICH path produced an answer rather than
// inferring it from the answer's value.
type censusCapabilityStore struct {
	*beads.MemStore
	outside      bool
	censusErr    error
	censusCalls  int
	listCalls    int
	prefixesSeen []string
}

func newCensusCapabilityStore() *censusCapabilityStore {
	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	return &censusCapabilityStore{MemStore: mem}
}

func (s *censusCapabilityStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls++
	return s.MemStore.List(q)
}

func (s *censusCapabilityStore) HasResidentOutside(prefixes []string) (bool, error) {
	s.censusCalls++
	s.prefixesSeen = append([]string(nil), prefixes...)
	if s.censusErr != nil {
		return false, s.censusErr
	}
	return s.outside, nil
}

func (s *censusCapabilityStore) seedBead(t *testing.T, id string) {
	t.Helper()
	if _, err := s.Create(beads.Bead{ID: id, Title: id, Type: "task"}); err != nil {
		t.Fatalf("seeding %q: %v", id, err)
	}
}

// The verdict takes the store's own answer when the store has one, and it must
// be the store's answer rather than a scan that happens to agree.
//
// The fixture is deliberately contradictory — an EMPTY store that reports a
// resident outside — because that is the only way to prove which path spoke.
// The zero List calls are the other half: the whole point of the capability is
// that a clean born-split city stops hydrating its entire binding history on
// every one-shot process, and only the TRUE verdict is memoized, so that city
// pays this on every invocation forever.
func TestLegacyResidentsVerdictUsesTheCensusCapability(t *testing.T) {
	store := newCensusCapabilityStore()
	store.outside = true

	if !HasLegacyResidents(censusBinding(store)) {
		t.Fatal("the verdict ignored the store's own census; an empty store scanned clean and the capability's answer was discarded")
	}
	if store.censusCalls != 1 {
		t.Errorf("the store's census was asked %d times, want exactly 1", store.censusCalls)
	}
	if store.listCalls != 0 {
		t.Errorf("the verdict issued %d List calls on a store that answers the question directly; the scan is what this replaces", store.listCalls)
	}
	if !slices.Equal(store.prefixesSeen, infraPrefixes) {
		t.Fatalf("the store was handed %v, want the binding's declared namespaces %v", store.prefixesSeen, infraPrefixes)
	}
}

// The clean answer has to come from the capability too, or the fallback would
// mask a predicate that never answers false.
func TestLegacyResidentsVerdictTakesTheCensusCapabilitysCleanAnswer(t *testing.T) {
	store := newCensusCapabilityStore()
	store.seedBead(t, "ga-relic")

	if HasLegacyResidents(censusBinding(store)) {
		t.Fatal("the verdict scanned past the store's own clean answer; the capability is the verdict, not a hint")
	}
	if store.listCalls != 0 {
		t.Errorf("the verdict issued %d List calls; a store that answered must not be scanned as well", store.listCalls)
	}
}

// Most stores cannot answer the question — a class binding served by the native
// Dolt engine reaches its rows through beadslib.Storage and has no SQL of its
// own — so the scan is not a legacy path, it is the general one.
func TestLegacyResidentsVerdictFallsBackToTheScan(t *testing.T) {
	store := newCensusStore()
	if _, ok := beads.Store(store).(beads.NamespaceCensus); ok {
		t.Fatal("the fallback fixture answers the census itself; this row proves nothing")
	}
	store.seedBead(t, "ga-relic")

	if !HasLegacyResidents(censusBinding(store)) {
		t.Error("a store with no census capability was reported clean while holding a relic; the scan must still run for it")
	}
}

// A capability that failed is an unread binding, and the fail-safe direction is
// the same one a failed scan takes: keep the probe. Falling back to the scan
// here would be worse than useless — it would double the cost of a store that
// is already failing, and a store whose SQL cannot run will not serve a List
// either.
func TestACensusCapabilityErrorKeepsItsProbe(t *testing.T) {
	store := newCensusCapabilityStore()
	store.censusErr = errors.New("binding unreachable")

	if !HasLegacyResidents(censusBinding(store)) {
		t.Error("a binding whose census failed was reported clean; a census that cannot read must not retire a probe")
	}
}

// The drain report is a LIST of ids, not a verdict, so it cannot be served by a
// predicate that only answers yes or no. This row keeps a natural-looking tidy-up
// — routing both census entry points through the fast path — from silently
// emptying `gc storage status`'s count.
func TestTheDrainReportIgnoresTheCensusCapability(t *testing.T) {
	store := newCensusCapabilityStore()
	store.seedBead(t, "ga-relic")

	relics, err := OpenLegacyResidents(store, infraPrefixes)
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if len(relics) != 1 || relics[0] != "ga-relic" {
		t.Fatalf("the drain report returned %v, want the ids themselves; an operator watches these fall", relics)
	}
	if store.censusCalls != 0 {
		t.Errorf("the drain report asked the store for a verdict %d times; a verdict cannot name the ids", store.censusCalls)
	}
}

// The equivalence row: the predicate and the scan are two implementations of one
// question, and a disagreement silently narrows the census on exactly the cities
// that have relics.
//
// Both run against the SAME seeded store, so this is not two fixtures that
// happen to agree — it is one population read two ways. The predicate is asked
// directly rather than through HasLegacyResidents, because a verdict that had
// quietly stopped routing through the capability would otherwise compare the
// scan with itself and agree forever; the verdict is then checked against the
// same answer, which is what pins the routing.
func TestTheCensusPredicateAndTheScanAgree(t *testing.T) {
	for _, tc := range []struct {
		name     string
		seed     []beads.Bead
		close    []string
		prefixes []string
	}{
		{name: "clean binding", seed: []beads.Bead{{ID: "gcg-1"}, {ID: "gcnq-2"}}, prefixes: infraPrefixes},
		{name: "open relic", seed: []beads.Bead{{ID: "gcg-1"}, {ID: "ga-relic"}}, prefixes: infraPrefixes},
		{name: "closed relic only", seed: []beads.Bead{{ID: "gcg-1"}, {ID: "ga-done"}}, close: []string{"ga-done"}, prefixes: infraPrefixes},
		{name: "closed in-namespace only", seed: []beads.Bead{{ID: "gcg-1"}}, close: []string{"gcg-1"}, prefixes: infraPrefixes},
		{name: "wisp relic", seed: []beads.Bead{{ID: "gcg-1"}, {ID: "ga-wisp", Ephemeral: true}}, prefixes: infraPrefixes},
		{name: "lookalike prefix", seed: []beads.Bead{{ID: "gcgx-1"}}, prefixes: infraPrefixes},
		{name: "bare prefix id", seed: []beads.Bead{{ID: "gcg"}}, prefixes: infraPrefixes},
		{name: "empty binding", prefixes: infraPrefixes},
		{name: "binding claiming no namespace", seed: []beads.Bead{{ID: "gcg-1"}}, prefixes: nil},
		{name: "blank namespace", seed: []beads.Bead{{ID: "gcg-1"}}, prefixes: []string{" "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
			if err != nil {
				t.Fatalf("OpenSQLiteStore: %v", err)
			}
			census, ok := store.(beads.NamespaceCensus)
			if !ok {
				t.Fatalf("%T cannot answer the census itself; this row would compare the scan with itself", store)
			}
			for _, b := range tc.seed {
				b.Title = b.ID
				b.Type = "task"
				if _, err := store.Create(b); err != nil {
					t.Fatalf("seeding %q: %v", b.ID, err)
				}
			}
			for _, id := range tc.close {
				if err := store.Close(id); err != nil {
					t.Fatalf("closing %q: %v", id, err)
				}
			}

			relics, err := LegacyResidents(store, tc.prefixes)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			scanned := len(relics) > 0
			predicate, err := census.HasResidentOutside(tc.prefixes)
			if err != nil {
				t.Fatalf("predicate: %v", err)
			}
			if predicate != scanned {
				t.Fatalf("the predicate answered %v and the scan answered %v (relics %v); one of the two is narrowing the census", predicate, scanned, relics)
			}

			binding := ClassBinding{Classes: infraClasses, Prefixes: tc.prefixes, Leg: Leg{Ref: ClassRef(infraClasses), Store: store}}
			if got := HasLegacyResidents(binding); got != scanned {
				t.Fatalf("the boot verdict answered %v while both the predicate and the scan answered %v", got, scanned)
			}
		})
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

	if HasLegacyResidents(ClassBinding{
		Classes:  []coordclass.Class{coordclass.ClassGraph},
		Prefixes: []string{"gcg"},
		Leg:      Leg{Ref: ClassRef([]coordclass.Class{coordclass.ClassGraph}), Store: binding},
	}) {
		t.Error("the census reported relics for a clean binding; it must read the binding leg alone")
	}
}

// censusWrapperStore is the shape of a production decorator over a binding —
// cmd/gc's emitting class store — reproduced here so the verdict's DISCOVERY is
// testable in this package.
//
// It carries HasResidentOutside structurally because a reflective guard over
// there forces it to, and it declares beads.NamespaceCensusHandleProvider
// because carrying the method is not the same as being able to answer. The two
// counters are what let a row say WHICH of the three paths ran: the backing's
// census, the wrapper's own method, or the scan.
type censusWrapperStore struct {
	beads.Store
	directCalls int
	handleCalls int
}

func (s *censusWrapperStore) HasResidentOutside(prefixes []string) (bool, error) {
	s.directCalls++
	census, ok := beads.NamespaceCensusFor(s.Store)
	if !ok {
		return false, beads.ErrNamespaceCensusUnsupported
	}
	return census.HasResidentOutside(prefixes)
}

func (s *censusWrapperStore) NamespaceCensusHandle() (beads.NamespaceCensus, bool) {
	s.handleCalls++
	return beads.NamespaceCensusFor(s.Store)
}

// A wrapped binding whose backing CAN answer still reaches the predicate. The
// wrapper is not a wall: losing the capability here would put every one-shot
// command on a converged city back on the full scan, which is the cost this
// whole capability exists to remove.
func TestTheVerdictReachesThePredicateThroughAWrapper(t *testing.T) {
	backing := newCensusCapabilityStore()
	backing.outside = true
	wrapper := &censusWrapperStore{Store: backing}

	if !HasLegacyResidents(censusBinding(wrapper)) {
		t.Fatal("the verdict discarded the wrapped backing's census; an empty store scanned clean and the capability's answer was thrown away")
	}
	if backing.censusCalls != 1 {
		t.Errorf("the backing's census was asked %d times, want exactly 1", backing.censusCalls)
	}
	if backing.listCalls != 0 {
		t.Errorf("the verdict issued %d List calls through a wrapper whose backing answers directly", backing.listCalls)
	}
	if wrapper.handleCalls != 1 {
		t.Errorf("the wrapper's census handle was consulted %d times, want exactly 1; discovery went somewhere else", wrapper.handleCalls)
	}
}

// The row that makes the seam worth having: a wrapped binding whose backing
// CANNOT answer must fall back to the scan and reach the same verdict the scan
// reaches unwrapped.
//
// This is the native-Dolt-served binding. Its wrapper carries
// HasResidentOutside all the same, so a bare `store.(beads.NamespaceCensus)`
// would match, the scan would never run, and the binding's whole population
// would be answered for by a method with nothing behind it. Both directions run
// against one fixture read three ways — wrapped verdict, unwrapped verdict,
// scan — because a fallback that answered TRUE unconditionally would pass the
// relic row and strand nothing only by luck.
func TestTheVerdictScansThroughAWrapperWhoseBackingCannotAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed []string
		want bool
	}{
		{name: "binding holding a relic", seed: []string{"gcg-1", "ga-relic"}, want: true},
		{name: "clean binding", seed: []string{"gcg-1", "gcnq-2"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing := newCensusStore()
			for _, id := range tc.seed {
				backing.seedBead(t, id)
			}
			if _, ok := beads.Store(backing).(beads.NamespaceCensus); ok {
				t.Fatal("the fixture backing answers the census itself; this row proves nothing")
			}
			wrapper := &censusWrapperStore{Store: backing}
			if _, ok := beads.Store(wrapper).(beads.NamespaceCensus); !ok {
				t.Fatal("the wrapper fixture does not carry HasResidentOutside, so it does not model the wrapper the guard forces; this row proves nothing")
			}

			relics, err := LegacyResidents(backing, infraPrefixes)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if scanned := len(relics) > 0; scanned != tc.want {
				t.Fatalf("the unwrapped scan answered %v, want %v (relics %v)", scanned, tc.want, relics)
			}
			if got := HasLegacyResidents(censusBinding(wrapper)); got != tc.want {
				t.Fatalf("the verdict over the wrapped binding answered %v, want the scan's %v", got, tc.want)
			}
			if wrapper.directCalls != 0 {
				t.Errorf("the wrapper's own HasResidentOutside answered for a backing that has no census (%d call(s)); the scan is the only thing that can speak for this binding", wrapper.directCalls)
			}
		})
	}
}
