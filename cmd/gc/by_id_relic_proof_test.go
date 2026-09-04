package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storeref"
)

// refuseTheseCities installs the same standing storage refusal for several
// cities at once, without standing up a binding for any of them.
//
// The funnel cannot be seeded twice through resetCLIStorageRoutes: it closes
// the whole memo first, so a second call would drop the first city's routes. A
// row that needs a city and its control refused at the same moment resets once
// and seeds both.
func refuseTheseCities(t *testing.T, refusal error, cityPaths ...string) {
	t.Helper()
	resetCLIStorageRoutes(t)
	for _, cityPath := range cityPaths {
		entry := cliStorageRoutesEntryFor(filepath.Clean(cityPath))
		entry.once.Do(func() { entry.routes = refusingStorageRoutes("infra", refusal) })
		dropDerivedResidencyMemo(t, cityPath)
	}
}

// theRefusalARefusedCityCarries is the boot gate's own sentence. The denial
// rows below share it with their controls, so what changes the outcome is the
// proof rather than the wording of the refusal.
func theRefusalARefusedCityCarries() error {
	return errors.New("storage refused: this city has not converged on its configured [storage] binding; run `gc storage migrate`")
}

// countStoragePlanResolutions wraps the registry constructor so a row can prove
// the relic proof was NOT taken. Every path into the proof resolves a storage
// plan, and a plan resolution constructs exactly one registry.
func countStoragePlanResolutions(t *testing.T) *int {
	t.Helper()
	var n int
	prev := newStorageRegistryForPlan
	newStorageRegistryForPlan = func() (*storebinding.ProviderRegistry, error) {
		n++
		return prev()
	}
	t.Cleanup(func() { newStorageRegistryForPlan = prev })
	return &n
}

// TestRefusedCityDeniesTheRelicItsLiveCensusProves is the ga-q8ick fix with the
// proof computed where it is used.
//
// A refused city still serves WORK, so the resolver tolerates the standing
// refusal on a residence probe and the surface falls through to its own scan.
// That is right until the binding is proven to hold work-prefixed relics: `gc
// storage migrate` preserved those ids and deleted nothing, so the scan finds
// the retained pre-migration copy in the city work store, serves it, and the
// close that follows writes it. Exit 0, no diagnostic.
//
// Nothing on disk records the proof. The binding IS readable — the boot gate
// refused to SERVE it, which is a different claim — so the census that decides
// this is taken here, against a handle opened for the read and closed after it.
func TestRefusedCityDeniesTheRelicItsLiveCensusProves(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	relic := classResidentWorkShapedBead(t, classStore, "gc-relic1", "carried across by the migration")

	// The control city is refused identically and its binding holds nothing,
	// which is what makes the denial below attributable to the PROOF rather
	// than to the refusal.
	control, _ := foreignProviderCity(t)
	refuseTheseCities(t, theRefusalARefusedCityCarries(), cityPath, control)

	_, ok, err := cliByIDBindingOwner(cityPath, relic.ID)
	if err == nil {
		t.Fatalf("a refused city whose binding holds %s resolved to ok=%v with no error; the caller falls through to its scan and serves the copy the migration retained in the work store", relic.ID, ok)
	}
	if !storeref.IsStandingRefusal(err) {
		t.Errorf("the denial came back as %v, want the standing storage refusal", err)
	}
	if !errors.Is(err, storeref.ErrProvenRelicRefusal) {
		t.Errorf("the denial reads %q and carries nothing that tells it apart from the refusal an in-namespace id takes", err)
	}
	if !strings.Contains(err.Error(), "gc doctor") {
		t.Errorf("the denial reads %q with no route back to a served city", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("the denial is multi-line (%q); callers print it as `gc <cmd>: %%v`, so everything after the newline loses the command that refused", err)
	}

	if _, ok, err := cliByIDBindingOwner(control, relic.ID); err != nil || ok {
		t.Errorf("the same refusal over a city whose binding holds no relic resolved to ok=%v err=%v, want a clean fall-through; a census that found nothing proves nothing, and denying there takes work-bead reads away from every unconverged city", ok, err)
	}
}

// TestRefusedCityThatCannotOpenItsBindingFallsThrough is the proof's absent
// case, and it is deliberately the tolerant one.
//
// A city whose binding cannot be OPENED has said nothing about what that
// binding holds, and this bit only ever denies: an unknown must never assert
// it. So the read falls through exactly as it does today, and the operator
// keeps the work-bead reads a refused city depends on.
//
// The relic is seeded first so the row cannot pass by finding an empty binding.
// What is pinned is that an unreadable binding counts as no evidence even when
// the evidence is really there.
func TestRefusedCityThatCannotOpenItsBindingFallsThrough(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a mode-0 directory is still readable, so the binding cannot be made unopenable this way")
	}
	cityPath, classStore := foreignProviderCity(t)
	relic := classResidentWorkShapedBead(t, classStore, "gc-relic1", "carried across by the migration")

	refuseTheseCities(t, theRefusalARefusedCityCarries(), cityPath)
	sealTheBindingRoot(t, cityPath)

	owner, ok, err := cliByIDBindingOwner(cityPath, relic.ID)
	if err != nil {
		t.Fatalf("a refused city whose binding could not be opened resolved %s to err=%v; a census that could not run is not proof, and denying on it takes work-bead reads away from every city whose binding is merely unreachable", relic.ID, err)
	}
	if ok {
		t.Errorf("a refusing binding reported ownership of %s (%p)", relic.ID, owner.Store)
	}
}

// TestServedCityPaysNothingForTheRelicProof is the healthy control, and it is
// the cost bound as well as the behavior one.
//
// The proof exists for a city whose boot REFUSED. A served city's bindings were
// censused live when the funnel opened them, so a second read could prove
// nothing the first did not — and taking one would put an engine open on the
// by-id path of every converged city. Zero plan resolutions is the only shape
// that rules that out.
func TestServedCityPaysNothingForTheRelicProof(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	relic := classResidentWorkShapedBead(t, classStore, "gc-relic1", "carried across by the migration")
	binding := recensusAfterSeedingARelic(t, cityPath)
	if _, err := workStoreFor(t, cityPath).Create(beads.Bead{Title: "the copy the migration left in the work ledger", Type: "task"}); err != nil {
		t.Fatalf("seeding the work store's retained copy: %v", err)
	}

	resolutions := countStoragePlanResolutions(t)

	owner, ok, err := cliByIDBindingOwner(cityPath, relic.ID)
	if err != nil {
		t.Fatalf("a served city resolved %s to err=%v", relic.ID, err)
	}
	if !ok {
		t.Fatalf("a served city's binding did not answer for %s; the caller falls through to the work ledger's frozen copy", relic.ID)
	}
	if owner.Store != binding {
		t.Errorf("the owner is %p, want the class binding %p", owner.Store, binding)
	}
	if *resolutions != 0 {
		t.Errorf("a served city resolved %d storage plan(s) on the by-id path; the relic proof is for a REFUSED city, and a served one must not pay an engine open per command", *resolutions)
	}
}

// sealTheBindingRoot makes this city's configured binding unopenable, and only
// for the length of the test.
//
// The fixture's provider is this build's own sqlite engine behind a
// configuration reference, so removing the directory's permissions is what
// "cannot open the binding" means for it. The bead already inside stays there,
// which is the point: the proof is absent because the census could not RUN, not
// because there was nothing to find.
func sealTheBindingRoot(t *testing.T, cityPath string) {
	t.Helper()
	root := filepath.Join(cityPath, ".gc", "engine-infra")
	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("the fixture's binding root is not at %s: %v", root, err)
	}
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatalf("sealing %s: %v", root, err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, info.Mode().Perm()) })
}
