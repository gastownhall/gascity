package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
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

// TestTheProofAndTheRefusedFunnelSpellTheBindingRefTheSameWay holds the two
// ends of the fix together.
//
// The rows above each assemble one end. The proof keys its verdict by the ref
// of the binding IT built — over an opened sqlite engine, from the classes the
// storage plan assigns it. The refused by-id path looks a ref up in that
// verdict using the ref of the binding the REFUSED funnel built — over five
// refusedClassStore values grouped by equality, from the classes
// refusingStorageRoutes decided to route. Those are two constructions on two
// different days of a city's life, and nothing states that they spell the ref
// the same way.
//
// They do, because both go through residencyBindingsFromRoutes and so through
// storeref.ClassRef over the same infrastructure class set. If either side ever
// narrowed to the classes its own routes happened to name, the lookup would
// miss and the denial would vanish: no error, ok=false, the caller's scan serves
// the pre-migration copy the migration left in the work ledger — exactly the
// ga-q8ick symptom, restored silently. The denial row above would catch that,
// but only by its absence, and a row that fails for "the denial went missing"
// says nothing about WHY. This one names the reason.
func TestTheProofAndTheRefusedFunnelSpellTheBindingRefTheSameWay(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	relic := classResidentWorkShapedBead(t, classStore, "gc-relic1", "carried across by the migration")

	refuseTheseCities(t, theRefusalARefusedCityCarries(), cityPath)

	proven := provenRelicRefsForCity(cityPath)
	if len(proven) != 1 {
		t.Fatalf("the live census proved %d ref(s) for a city whose binding holds %s; with nothing proved the agreement below would be vacuous", len(proven), relic.ID)
	}

	refusedBindings, refused := cliResidencyBindings(cityPath)
	if refused == nil {
		t.Fatal("the fixture city is not refused; the funnel side of the agreement is the REFUSED one")
	}
	if len(refusedBindings) != 1 {
		t.Fatalf("the refused funnel grouped %d binding(s), want one", len(refusedBindings))
	}
	if !proven[refusedBindings[0].Leg.Ref] {
		t.Errorf("the census proved %v and the refused funnel asks about %q; the two spell the same physical binding differently, so the lookup misses and the denial silently disappears", refsOf(proven), refusedBindings[0].Leg.Ref)
	}
}

// refsOf renders a proof verdict for a failure message.
func refsOf(proven map[storeref.StoreRef]bool) []string {
	out := make([]string, 0, len(proven))
	for ref := range proven {
		out = append(out, string(ref))
	}
	sort.Strings(out)
	return out
}

// controllerDownForBeadsShow answers the `gc beads show` API seam "no
// controller", which is the only state in which the command reaches the store
// funnel at all.
//
// The seam is stubbed rather than left alone because apiClient() probes for a
// live controller and loads config: unstubbed, the row would pass or fail on
// whether something happened to be listening for the fixture's city, and on a
// machine where one was it would never run the fallback it exists to pin.
func controllerDownForBeadsShow(t *testing.T) {
	t.Helper()
	prev := beadsShowAPIClient
	beadsShowAPIClient = func(string) (*api.Client, string) { return nil, "no controller for this fixture city" }
	t.Cleanup(func() { beadsShowAPIClient = prev })
}

// TestBeadsShowRefusesTheRelicItWouldOtherwiseServeFrozen drives the denial at
// the surface ga-q8ick was reported on.
//
// TestRefusedCityDeniesTheRelicItsLiveCensusProves pins the same fixture one
// frame below, at cliByIDBindingOwner, and that is not the same claim. The bead
// exists because `gc beads show` answered with a stale pre-migration row, exit
// 0, no diagnostic — so what has to be pinned is the COMMAND's exit and the
// COMMAND's output, not the error value a helper hands it. Between the two sits
// doBeadsShowFallback's classification, and the failure that reopens ga-q8ick is
// a change there that reads every error as "no binding answered, run your own
// scan": the denial disappears, the scan finds the copy the migration retained
// in the work ledger, and the command reports it as current.
//
// The frozen copy is therefore seeded for real. Without it the row could pass
// against a fall-through that simply found nothing, which is a different exit
// code for a different reason and says nothing about serving stale rows.
func TestBeadsShowRefusesTheRelicItWouldOtherwiseServeFrozen(t *testing.T) {
	const frozenTitle = "the copy the migration left in the work ledger"

	cityPath, classStore := foreignProviderCity(t)
	frozen, err := workStoreFor(t, cityPath).Create(beads.Bead{Title: frozenTitle, Type: "task"})
	if err != nil {
		t.Fatalf("seeding the work store's retained copy: %v", err)
	}
	relic := classResidentWorkShapedBead(t, classStore, frozen.ID, "the row the binding holds now")

	controllerDownForBeadsShow(t)
	refuseTheseCities(t, theRefusalARefusedCityCarries(), cityPath)

	var stdout, stderr bytes.Buffer
	code := cmdBeadsShow(relic.ID, "json", &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc beads show %s exited 0 on a refused city whose binding is proven to hold relics; stdout = %s", relic.ID, stdout.String())
	}
	if strings.Contains(stdout.String(), frozenTitle) {
		t.Fatalf("gc beads show served the pre-migration copy from the work ledger: %s — this is the ga-q8ick symptom, a row frozen at migration time reported as current", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gc beads show:") {
		t.Errorf("the denial reached stderr as %q without naming the command that refused; an operator reading this cannot tell which surface gave up", stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc doctor") {
		t.Errorf("gc beads show printed %q with no route back to a served city", stderr.String())
	}
	// One printed line, because the caller's format string is
	// `gc beads show: %v\n`: a second line in the error value arrives without
	// that prefix and the operator cannot tell which surface it came from.
	if got := strings.Count(strings.TrimRight(stderr.String(), "\n"), "\n"); got != 0 {
		t.Errorf("the denial printed %d extra line(s): %q — everything after the first loses the `gc beads show:` prefix", got, stderr.String())
	}

	// The control, and the reason this row is about the PROOF rather than about
	// the refusal: the same city, the same refusal, the same id, with the
	// evidence taken out of reach. A refused city still serves work, so the
	// command must fall through to its scan and answer — from the retained copy,
	// which is the behavior every unconverged city depends on. If this arm ever
	// fails, the pin above has degenerated into "a refused city refuses
	// everything" and would hold against code that dropped the census entirely.
	if os.Geteuid() == 0 {
		t.Skip("running as root: the proof cannot be taken out of reach, so the control arm below cannot run")
	}
	sealTheBindingRoot(t, cityPath)
	dropCLIResidencyBindings(filepath.Clean(cityPath))

	var unprovenOut, unprovenErr bytes.Buffer
	if code := cmdBeadsShow(relic.ID, "json", &unprovenOut, &unprovenErr); code != 0 {
		t.Fatalf("gc beads show %s exited %d on the same refusal with nothing proved: %s — absence of evidence is not evidence, and refusing here takes work-bead reads away from every unconverged city", relic.ID, code, unprovenErr.String())
	}
	if !strings.Contains(unprovenOut.String(), frozenTitle) {
		t.Errorf("the unproven arm served %q, want the work ledger's copy; if the scan cannot reach it then the arm above proved nothing about a stale answer being suppressed", unprovenOut.String())
	}
}

// TestBeadsShowServesAConvergedCityThroughTheSameEntryPoint is the healthy
// control for the row above, taken at the same entry point rather than at the
// fallback it calls.
//
// TestBeadsShowFallbackServesTheBindingCopy already pins this property one frame
// down. What it cannot pin is that `gc beads show` still REACHES that frame: a
// guard added to cmdBeadsShow or routeBeadsShow that refused a city whose
// storage looked unusual would leave that row green and break every converged
// city's reads. Exit 0 here, from the binding's copy, is what keeps the denial
// above a statement about proven relics instead of about this funnel.
func TestBeadsShowServesAConvergedCityThroughTheSameEntryPoint(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	shadow, err := workStoreFor(t, cityPath).Create(beads.Bead{Title: "the retained work copy", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the work store: %v", err)
	}
	relocated := classResidentWorkShapedBead(t, classStore, shadow.ID, "the class-binding copy")
	recensusAfterSeedingARelic(t, cityPath)

	controllerDownForBeadsShow(t)

	var stdout, stderr bytes.Buffer
	if code := cmdBeadsShow(relocated.ID, "json", &stdout, &stderr); code != 0 {
		t.Fatalf("gc beads show %s exited %d on a served city: %s", relocated.ID, code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "the class-binding copy") {
		t.Errorf("gc beads show served %s, want the binding's copy — the work store's is frozen at migration time", stdout.String())
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
