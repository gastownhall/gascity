package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
)

// byIDRouteCity seeds the one-shot funnel for a city whose graph class is served
// from its own binding, and returns the city path plus the two stores.
//
// The work leaf mints outside the reserved class namespace and the class leaf
// mints inside it, exactly as a real split city does — which is what makes the
// namespace gate and the residence probe distinguishable here at all.
func byIDRouteCity(t *testing.T) (cityPath string, work, class beads.Store) {
	t.Helper()
	cityPath = t.TempDir()
	work, class = splittest.NewSplitStores(t)
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(class))
	return cityPath, work, class
}

// TestClassRoutedStoreForIDIsIdentityOnACityThatRelocatesNothing is the
// single-store compatibility claim, and it is asserted by STORE IDENTITY rather
// than by behavior: the resolver must hand back the exact value it was given,
// so every optional-capability type assertion the caller already made keeps
// holding. Mutate the identity branch to return any other store and this fails.
func TestClassRoutedStoreForIDIsIdentityOnACityThatRelocatesNothing(t *testing.T) {
	cityPath := t.TempDir()
	resetCLIStorageRoutes(t)
	seedCLIStorageRoutes(t, cityPath, nil)
	work := splittest.NewWorkStore(t, "hq")
	bead, err := work.Create(beads.Bead{Title: "work bead", Type: "task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	classPrefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatal("no reserved graph prefix; there is no namespace for by-id routing to run on")
	}
	// Both id shapes, because they take different branches: a work id misses the
	// namespace gate and a class-shaped id clears it, so only the class-shaped
	// row can prove the IDENTITY gate fires first.
	ids := []string{bead.ID, classPrefix + "-1", "hq-does-not-exist"}
	for _, id := range ids {
		got, err := classRoutedStoreForID(cityPath, id, work)
		if err != nil {
			t.Fatalf("classRoutedStoreForID(%q): %v", id, err)
		}
		if got != work {
			t.Errorf("classRoutedStoreForID(%q) returned %p, want the caller's own store %p — a city that relocates nothing must get its exact store value back", id, got, work)
		}
	}

	// The second identity shape: routes that DO name a binding for the graph
	// class, whose store is the work store. storeref.ClassCandidates gates on
	// exactly this (Class != Work) and returns nil, and so must this resolver —
	// otherwise a city with nowhere to route to is routed anyway, and the class
	// leg and the work leg are probed as if they were two ledgers.
	selfRouted := t.TempDir()
	resetCLIStorageRoutes(t)
	seedCLIStorageRoutes(t, selfRouted, messagingSplitRoutes(work))
	for _, id := range ids {
		got, err := classRoutedStoreForID(selfRouted, id, work)
		if err != nil {
			t.Fatalf("classRoutedStoreForID(%q) on a self-routed city: %v", id, err)
		}
		if got != work {
			t.Errorf("classRoutedStoreForID(%q) returned %p on a city whose class binding IS its work store, want %p", id, got, work)
		}
	}
}

// TestClassRoutedStoreForIDCoversTheLegacyIDTheSharedResolverDeclines is the
// correction #5139's council landed, asserted rather than described.
//
// storeref.ClassCandidates gates on the class NAMESPACE, and `gc storage
// migrate` preserves ids — so a bead the migration relocated keeps its HQ/rig-era
// prefix and the shared resolver returns NIL candidates for it, for exactly the
// ids that moved. Both halves are asserted here: that the resolver declines, and
// that the residence probe answers anyway. Delete the fallback and the second
// half fails.
func TestClassRoutedStoreForIDCoversTheLegacyIDTheSharedResolverDeclines(t *testing.T) {
	cityPath, work, class := byIDRouteCity(t)
	// The migration copies the work store's infrastructure slice with ids
	// PRESERVED, so the relocated row carries a work-shaped id.
	migrated, err := class.Create(beads.Bead{ID: "hq-4177", Title: "relocated with its id preserved", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the migrated row: %v", err)
	}
	splittest.TakeResidenceViolations(class)

	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatal("no reserved graph prefix")
	}
	if got := storeref.ClassCandidates(migrated.ID, storeref.ClassRouting{Prefix: prefix, Class: class, Work: work}); got != nil {
		t.Fatalf("storeref.ClassCandidates(%q) returned %d candidates; this test's premise is that the namespace gate declines a migrated legacy id — if the resolver now covers it, the residence fallback can be reconsidered", migrated.ID, len(got))
	}

	got, err := classRoutedStoreForID(cityPath, migrated.ID, work)
	if err != nil {
		t.Fatalf("classRoutedStoreForID(%q): %v", migrated.ID, err)
	}
	if got != class {
		t.Errorf("classRoutedStoreForID(%q) resolved %p, want the class binding %p — the shared resolver declines this id, so only a residence probe can reach it and the read would otherwise be answered by the work store's retained pre-migration copy", migrated.ID, got, class)
	}
}

// TestClassRoutedStoreForIDProbesRatherThanRoutesOnThePrefix is the
// candidate-list-and-probe property. A reserved class prefix is only an ADVISORY
// on work stores (config.ReservedPrefixWarnings warns; config.ValidateRigs does
// not reject), so a work store can legitimately hold an id inside the class
// namespace — and a resolver that routed unconditionally on the prefix would
// report that bead as absent.
func TestClassRoutedStoreForIDProbesRatherThanRoutesOnThePrefix(t *testing.T) {
	cityPath, work, class := byIDRouteCity(t)
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatal("no reserved graph prefix")
	}
	shadowed := prefix + "-9001"
	// The forced path, which is the only way a work store comes to hold an id
	// inside the class namespace: a reserved prefix is warned-and-allowed on a
	// work store, and bd's own --force keeps such an id verbatim.
	forcer, ok := work.(beads.ForeignIDCreator)
	if !ok {
		t.Fatalf("the work leaf %T cannot model a forced foreign-prefix create; this invariant is about an id the work store legitimately holds", work)
	}
	if _, err := forcer.CreateWithForeignID(beads.Bead{ID: shadowed, Title: "class-shaped id held by the work store", Type: "task"}); err != nil {
		t.Fatalf("seeding the shadowed row: %v", err)
	}
	if _, err := class.Get(shadowed); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("the class binding claims %s (%v); the premise is that only the work store holds it", shadowed, err)
	}

	got, err := classRoutedStoreForID(cityPath, shadowed, work)
	if err != nil {
		t.Fatalf("classRoutedStoreForID(%q): %v", shadowed, err)
	}
	if got != work {
		t.Errorf("classRoutedStoreForID(%q) resolved %p, want the work store %p — the class store MINTS the reserved namespace but minting is not holding, so the list must be probed and not routed", shadowed, got, work)
	}
}

// TestClassRoutedStoreForIDResolvesAClassResidentID is the ordinary case: an id
// the binding holds resolves to the binding, and never to the work store the
// caller opened.
func TestClassRoutedStoreForIDResolvesAClassResidentID(t *testing.T) {
	cityPath, work, class := byIDRouteCity(t)
	resident, err := class.Create(beads.Bead{Title: "graph root", Type: "task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := classRoutedStoreForID(cityPath, resident.ID, work)
	if err != nil {
		t.Fatalf("classRoutedStoreForID(%q): %v", resident.ID, err)
	}
	if got != class {
		t.Errorf("classRoutedStoreForID(%q) resolved %p, want the class binding %p", resident.ID, got, class)
	}
}

// TestClassRoutedStoreForIDTakesTheSharedResolversCandidateOrder pins that the
// probe list for an in-namespace id is literally storeref.ClassCandidates'
// answer, so the ORDER and the identity gate have exactly one owner. A local
// list that happened to agree today would drift the moment the resolver grows a
// leg.
func TestClassRoutedStoreForIDTakesTheSharedResolversCandidateOrder(t *testing.T) {
	_, work, class := byIDRouteCity(t)
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatal("no reserved graph prefix")
	}
	id := prefix + "-1"
	want := storeref.ClassCandidates(id, storeref.ClassRouting{Prefix: prefix, Class: class, Work: work})
	if len(want) == 0 {
		t.Fatal("the shared resolver produced no candidates for an in-namespace id on a relocated city")
	}
	got := classRouteCandidates(id, class, work)
	if len(got) != len(want) {
		t.Fatalf("classRouteCandidates produced %d candidates, want the shared resolver's %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidate %d = %p, want %p — the class store leads because it is the sole minter of the namespace, and work follows as the fallback leg", i, got[i], want[i])
		}
	}
}

// TestClassRoutedStoreForIDSurfacesAReadFailureRatherThanAbsence is the
// classification the whole by-id lane exists for. Reading "the binding could not
// answer" as "the bead is not there" is the root-loss shape, and it is the one
// misclassification a caller cannot recover from once it has been flattened.
func TestClassRoutedStoreForIDSurfacesAReadFailureRatherThanAbsence(t *testing.T) {
	cityPath := t.TempDir()
	work := splittest.NewWorkStore(t, "hq")
	boom := errors.New("binding unreachable")
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(errStore{err: boom}))

	if _, err := classRoutedStoreForID(cityPath, "hq-1", work); err == nil || !errors.Is(err, boom) {
		t.Fatalf("classRoutedStoreForID on an unreadable binding returned err=%v, want the read failure — absence here would answer from the work ledger about a bead the binding may hold", err)
	}
}

// TestClassRoutedStoreForIDKeepsWorkOnARefusedCity pins the one error that is
// not a fault. The one-shot funnel's standing refusal is this build's verdict
// about a CITY's storage configuration, not about a bead — and a refused city
// still serves WORK from its work ledger. So a work-shaped id keeps its existing
// path, while an id only the binding could own gets the refusal.
func TestClassRoutedStoreForIDKeepsWorkOnARefusedCity(t *testing.T) {
	cityPath := t.TempDir()
	work := splittest.NewWorkStore(t, "hq")
	refusal := errors.New("this city's storage cannot be served; run `gc storage migrate`")
	resetCLIStorageRoutes(t)
	entry := cliStorageRoutesEntryFor(filepath.Clean(cityPath))
	entry.once.Do(func() { entry.routes = refusingStorageRoutes("infra", refusal) })

	got, err := classRoutedStoreForID(cityPath, "hq-1", work)
	if err != nil {
		t.Fatalf("a work id on a refused city returned %v, want the work store: the refusal is a fact about the city and says nothing about a bead the work ledger still serves", err)
	}
	if got != work {
		t.Errorf("a work id on a refused city resolved %p, want the work store %p", got, work)
	}

	classPrefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatal("no reserved graph prefix")
	}
	if _, err := classRoutedStoreForID(cityPath, classPrefix+"-1", work); err == nil {
		t.Errorf("a reserved-prefix id on a refused city resolved without error; that id can only live in the binding, so the refusal IS the answer and must not be flattened into a work-store read")
	}
}

// errStore is a beads.Store whose every read fails, for the failure-vs-absence
// classification above. Only Get is exercised.
type errStore struct {
	beads.Store
	err error
}

func (s errStore) Get(string) (beads.Bead, error) { return beads.Bead{}, s.err }
