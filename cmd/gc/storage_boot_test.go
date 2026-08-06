package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
	sqlitebinding "github.com/gastownhall/gascity/internal/storebinding/sqlite"
)

// countStorageRegistryConstructions replaces the plan's registry constructor
// with a counting one, so a test can prove a path constructed none.
func countStorageRegistryConstructions(t *testing.T) *int {
	t.Helper()
	count := 0
	prev := newStorageRegistryForPlan
	newStorageRegistryForPlan = func() (*storebinding.ProviderRegistry, error) {
		count++
		return prev()
	}
	t.Cleanup(func() { newStorageRegistryForPlan = prev })
	return &count
}

// directoryHolds reports whether parent's own listing names child. It is a
// POSITIVE read: the parent is listed and the answer is derived from what is
// actually in it, so an unreadable parent fails the test rather than reading as
// "the child is absent" — which is how a stat-based check turns a fault into
// evidence of the thing it was asked about.
func directoryHolds(t *testing.T, parent, child string) bool {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("listing %s: %v", parent, err)
	}
	for _, entry := range entries {
		if entry.Name() == child {
			return true
		}
	}
	return false
}

// assertNoConvergenceMarker proves, from a directory listing rather than a
// stat, that nothing recorded convergence on this binding. because names what
// the absence is evidence for.
//
// It picks the directory by how far the path under test got, and both arms read
// a directory that certainly exists. A refusal that never opened the
// destination leaves no binding root at all, so the root's PARENT is listed and
// must not name it — which also rules out anything inside it. One that did open
// the destination leaves a root, so the ROOT is listed and must not name the
// marker. A stat of the marker path instead answers "absent" for a path typo, a
// moved fixture and a permission fault exactly as it does for the thing it was
// asked about.
func assertNoConvergenceMarker(t *testing.T, target infraBindingTarget, because string) {
	t.Helper()
	if !directoryHolds(t, filepath.Dir(target.Root), filepath.Base(target.Root)) {
		return
	}
	if directoryHolds(t, target.Root, infraMigratedMarkerName) {
		t.Errorf("the convergence marker %s exists: %s", target.MarkerPath(), because)
	}
}

// TestStorageGateBypassesEverythingWithoutConfig is the compatibility contract:
// a city that authors no [storage] section reaches none of this PR's code.
//
// It asserts a NEGATIVE — no registry, no plan, no store opened, no byte read
// from a binding — because that is the only form of the claim that cannot drift.
// An assertion that the routes came back nil would still pass if the gate built
// a registry, resolved a plan and then discarded it, and every one of those
// steps is a refusal mode a no-config city must not acquire.
func TestStorageGateBypassesEverythingWithoutConfig(t *testing.T) {
	registries := countStorageRegistryConstructions(t)
	refuseInfraMigrationSource(t)

	for _, cfg := range []*config.City{nil, {}, {Beads: config.BeadsConfig{Provider: "bd"}}} {
		var stderr bytes.Buffer
		routes, err := storageBootGate(t.TempDir(), cfg, "gc start", nil, &stderr)
		if err != nil {
			t.Fatalf("a city with no [storage] was refused: %v", err)
		}
		if routes != nil {
			t.Fatalf("a city with no [storage] resolved routes %+v, want none", routes)
		}
		if stderr.Len() != 0 {
			t.Errorf("a city with no [storage] wrote to stderr: %q", stderr.String())
		}
	}
	if *registries != 0 {
		t.Errorf("the no-config path constructed %d provider registr(ies); the bypass must short-circuit before any of this runs", *registries)
	}
}

const (
	// storageRegistryConstructor is the composition root that builds and
	// freezes this binary's storage provider registry.
	storageRegistryConstructor = "newStorageProviderRegistry"
	// storageRegistryPlanVar is the one variable that is allowed to name it,
	// and the seam the counting-constructor tests above swap.
	storageRegistryPlanVar = "newStorageRegistryForPlan"
)

// TestStorageRegistryConstructorHasOneCaller is the census behind the counting
// constructor: countStorageRegistryConstructions can only observe the
// constructions that go through newStorageRegistryForPlan, so every claim built
// on it — above all "the no-config path constructs no registry" — holds exactly
// as long as that variable is the only thing in non-test source that names the
// constructor.
//
// A second caller would not fail any other test. It would build a second frozen
// registry the counting seam never sees, and the compatibility negative would
// go on passing while being false.
func TestStorageRegistryConstructorHasOneCaller(t *testing.T) {
	root := moduleRoot(t)
	references := map[string]int{}
	initializers := map[string]int{}

	for _, rel := range moduleGoFiles(t, root) {
		if filepath.Dir(rel) != "cmd/gc" {
			continue
		}
		file := parseModuleFile(t, root, rel)
		ast.Inspect(file, func(node ast.Node) bool {
			if ident, ok := node.(*ast.Ident); ok && ident.Name == storageRegistryConstructor {
				references[rel]++
			}
			return true
		})
		for _, decl := range file.Decls {
			switch typed := decl.(type) {
			case *ast.FuncDecl:
				// The declaration names itself; that ident is not a caller.
				if typed.Recv == nil && typed.Name.Name == storageRegistryConstructor {
					references[rel]--
				}
			case *ast.GenDecl:
				if typed.Tok != token.VAR {
					continue
				}
				for _, spec := range typed.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
						continue
					}
					initializer, ok := value.Values[0].(*ast.Ident)
					if !ok {
						continue
					}
					if value.Names[0].Name == storageRegistryPlanVar && initializer.Name == storageRegistryConstructor {
						initializers[rel]++
					}
				}
			}
		}
		if references[rel] == 0 {
			delete(references, rel)
		}
	}

	total := 0
	for _, count := range references {
		total += count
	}
	if total != 1 {
		t.Fatalf("non-test cmd/gc source names %s %d time(s) outside its declaration (%v), want exactly 1: the %s initializer",
			storageRegistryConstructor, total, references, storageRegistryPlanVar)
	}
	if len(initializers) != 1 {
		t.Fatalf("the %s initializer is declared in %v, want exactly one declaration", storageRegistryPlanVar, initializers)
	}
	for rel := range initializers {
		if references[rel] != 1 {
			t.Fatalf("the one reference to %s is not the %s initializer in %s (%v)",
				storageRegistryConstructor, storageRegistryPlanVar, rel, references)
		}
	}
}

// TestResolveClassStoreIsIdentityWithoutRoutes pins the other half of the
// compatibility contract: with no routes, every class resolves to the EXACT
// store value the caller passed in.
//
// Value identity, not equivalence. The call sites assert optional capabilities
// (GraphApplyFor, HandlesFor, StorageCreateStore, Counter) on whatever comes
// back, so a resolver that returned a freshly wrapped store carrying the same
// rows would be a silent capability regression rather than a visible one.
func TestResolveClassStoreIsIdentityWithoutRoutes(t *testing.T) {
	work := beads.NewMemStore()
	for _, class := range []string{
		config.BeadClassWork, config.BeadClassGraph, config.BeadClassSessions,
		config.BeadClassMessaging, config.BeadClassOrders, config.BeadClassNudges,
	} {
		got := resolveClassStore(nil, work, nil, "", class, nil)
		if got != beads.Store(work) {
			t.Errorf("class %s resolved to %p, want the work store %p", class, got, work)
		}
	}
}

// TestResolveClassStoreLeavesWorkOnTheWorkStore proves the routes relocate the
// five infrastructure classes and nothing else, which is what keeps work on the
// work ledger in a split city.
func TestResolveClassStoreLeavesWorkOnTheWorkStore(t *testing.T) {
	work := beads.NewMemStore()
	binding := beads.NewMemStore()
	routes := &storageRoutes{binding: "infra", stores: map[coordclass.Class]beads.Store{
		coordclass.ClassGraph:     binding,
		coordclass.ClassSessions:  binding,
		coordclass.ClassMessaging: binding,
		coordclass.ClassOrders:    binding,
		coordclass.ClassNudges:    binding,
	}}
	if got := resolveClassStore(routes, work, nil, "", config.BeadClassWork, nil); got != beads.Store(work) {
		t.Errorf("work resolved to %p, want the work store %p", got, work)
	}
	for _, class := range []string{
		config.BeadClassGraph, config.BeadClassSessions,
		config.BeadClassMessaging, config.BeadClassOrders, config.BeadClassNudges,
	} {
		if got := resolveClassStore(routes, work, nil, "", class, nil); got != beads.Store(binding) {
			t.Errorf("class %s resolved to %p, want the binding store %p", class, got, binding)
		}
	}
}

// TestStorageSplitShapeAgreesWithTheMigrationTarget pins the agreement §5.5
// depends on: the shape the gate is willing to SERVE and the shape the
// migration is willing to CONVERGE are the same shape.
//
// If they ever disagree, one of two things ships: a city the gate serves but
// nothing can migrate onto, or a city that migrates and then refuses to start.
func TestStorageSplitShapeAgreesWithTheMigrationTarget(t *testing.T) {
	root := t.TempDir()
	whole := infraSplitConfig(filepath.Join(root, "store"))

	partial := infraSplitConfig(filepath.Join(root, "store"))
	partial.Storage.Classes.Nudges = config.StorageWorkBinding

	relocatedWork := infraSplitConfig(filepath.Join(root, "store"))
	relocatedWork.Storage.Classes.Work = "infra"

	allWork := &config.City{Storage: &config.StorageConfig{Classes: config.StorageClasses{
		Work: config.StorageWorkBinding, Graph: config.StorageWorkBinding,
		Sessions: config.StorageWorkBinding, Messaging: config.StorageWorkBinding,
		Orders: config.StorageWorkBinding, Nudges: config.StorageWorkBinding,
	}}}

	for name, tc := range map[string]struct {
		cfg          *config.City
		shape        storageSplitShape
		targetsSplit bool
	}{
		"the whole split":    {whole, storageSplitWhole, true},
		"a partial split":    {partial, storageSplitUnsupported, false},
		"work relocated":     {relocatedWork, storageSplitUnsupported, false},
		"everything on work": {allWork, storageSplitNone, false},
		"no storage section": {&config.City{}, storageSplitNone, false},
	} {
		t.Run(name, func(t *testing.T) {
			shape, _ := storageSplitShapeOf(tc.cfg.EffectiveStorage())
			if shape != tc.shape {
				t.Errorf("shape = %d, want %d", shape, tc.shape)
			}
			_, ok, err := resolveInfraBindingTarget(root, tc.cfg)
			if err != nil {
				t.Fatalf("resolveInfraBindingTarget: %v", err)
			}
			if ok != tc.targetsSplit {
				t.Errorf("the migration target resolved = %t, want %t; the served shape and the convergeable shape must be the same shape", ok, tc.targetsSplit)
			}
		})
	}
}

// TestStorageGateRefusesAnArrangementItCannotServe covers the partial fan-out:
// the plan machinery can resolve it, this runtime cannot serve it, and routing
// half a split by silence is the failure the gate exists to prevent.
func TestStorageGateRefusesAnArrangementItCannotServe(t *testing.T) {
	root := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(root, "store"))
	cfg.Storage.Classes.Nudges = config.StorageWorkBinding

	var stderr bytes.Buffer
	routes, err := storageBootGate(root, cfg, "gc start", nil, &stderr)
	if err == nil {
		t.Fatal("a partial split started the city")
	}
	if routes != nil {
		t.Errorf("a refused gate still returned routes %+v", routes)
	}
	if !strings.Contains(err.Error(), "whole infrastructure split or none of it") {
		t.Errorf("the refusal does not say what this build serves: %v", err)
	}
}

// TestStorageGateRefusesAnUnconvergedCityAndNamesTheCommand is the core of the
// boot-refusal design, and it asserts two separate things.
//
// The first is the refusal itself, naming the exact command spelling — a
// refusal that names no remedy is a city an operator cannot recover.
//
// The second is that the refusal did NOT migrate. That is asserted with
// POSITIVE filesystem evidence: the binding root's parent is listed and the
// root is not among its entries. Asserting on a stat error instead would pass
// on a permission fault, a path typo, or an unmounted volume — three ways to
// call a copy that DID run "no copy".
func TestStorageGateRefusesAnUnconvergedCityAndNamesTheCommand(t *testing.T) {
	cityPath := t.TempDir()
	bindingParent := t.TempDir()
	bindingRoot := filepath.Join(bindingParent, "store")
	cfg := infraSplitConfig(bindingRoot)

	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "a session", Type: "session"})
	mustCreateInfraBead(t, source, beads.Bead{Title: "real work", Type: "task"})
	before := infraStoreFingerprint(t, source)

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err == nil {
		t.Fatal("a city configured for a binding it never converged on started")
	}
	if routes != nil {
		t.Errorf("a refused gate still returned routes %+v", routes)
	}
	if !strings.Contains(err.Error(), storageMigrationCommand) {
		t.Errorf("the refusal does not name %q: %v", storageMigrationCommand, err)
	}
	if !strings.Contains(err.Error(), "Boot never migrates") {
		t.Errorf("the refusal does not say boot never migrates: %v", err)
	}

	if directoryHolds(t, bindingParent, "store") {
		entries, _ := os.ReadDir(bindingRoot)
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("the refusing boot created the binding root %s (holding %v); boot must not migrate", bindingRoot, names)
	}
	if got := stderr.String(); strings.Contains(got, "migrated to") || strings.Contains(got, "beads copied") {
		t.Errorf("the refusing boot reported a copy on stderr: %q", got)
	}
	if got := infraStoreFingerprint(t, source); !equalStrings(before, got) {
		t.Errorf("the refusing boot changed the work store: %v -> %v", before, got)
	}
}

// TestStorageGateRefusalNamesTheStrandedIDs pins where the stranded ids have to
// live: in the refusal STRING.
//
// The supervisor records the error it was handed and nothing else, so a bead id
// attached to a report field, an event payload or a stderr line the supervisor
// never captured is an id the operator recovering this city never sees. The
// count alone is an alarm nobody can act on.
func TestStorageGateRefusalNamesTheStrandedIDs(t *testing.T) {
	cityPath, cfg, source, _ := convergedInfraCity(t)
	stranded := mustCreateInfraBead(t, source, beads.Bead{Title: "landed after the proof", Type: "session", Labels: []string{"gc:session"}})

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err == nil {
		_ = routes.close()
		t.Fatal("a city with a stranded infrastructure write started")
	}
	if !strings.Contains(err.Error(), stranded.ID) {
		t.Errorf("the refusal does not name the stranded bead %s: %v", stranded.ID, err)
	}
	if strings.Contains(err.Error(), "see the ids above") {
		t.Errorf("the refusal defers to output the supervisor never records: %v", err)
	}
}

// TestStorageGateDoesNotCallAnUnreadableBindingUnmigrated covers the message on
// a converged city whose binding root has vanished — an unmounted volume, which
// reads on disk exactly as a city that never cut over reads.
//
// The refusal may not assert which of the two it is looking at, and it must put
// the hazard ahead of the remedy: running the copy against a mountpoint whose
// volume is absent lands the retained work store on the bare directory and
// leaves two divergent infrastructure stores.
func TestStorageGateDoesNotCallAnUnreadableBindingUnmigrated(t *testing.T) {
	cityPath, cfg, _, target := convergedInfraCity(t)
	if err := os.RemoveAll(target.Root); err != nil {
		t.Fatalf("removing the binding root: %v", err)
	}

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err == nil {
		_ = routes.close()
		t.Fatal("a city whose binding could not be read started")
	}
	if strings.Contains(err.Error(), "has not migrated onto it") {
		t.Errorf("a binding this boot could not read was reported as a city that never migrated: %v", err)
	}
	if !strings.Contains(err.Error(), "is not evidence") {
		t.Errorf("the refusal does not say the missing marker proves nothing here: %v", err)
	}
	if !strings.Contains(err.Error(), "not mounted") || !strings.Contains(err.Error(), "divergent") {
		t.Errorf("the refusal does not warn that copying onto a bare mountpoint diverges the two stores: %v", err)
	}
	if !strings.Contains(err.Error(), "Do NOT revert") {
		t.Errorf("the refusal does not withhold the revert for a binding it could not read: %v", err)
	}
}

// TestGenesisRaceLoserIsToldWhatHappened covers the two-process genesis: the
// loser must be told that another process won and that there is nothing to do,
// not handed the driver's own "table already exists".
//
// A raw driver error is an operator paging someone at 3am over a city that is
// fine. The fault arm is asserted in the same test so the recognition cannot
// widen into "every open failure is a race".
func TestGenesisRaceLoserIsToldWhatHappened(t *testing.T) {
	target := mustResolveInfraTarget(t, t.TempDir(), infraSplitConfig(filepath.Join(t.TempDir(), "store")))

	race := infraGenesisOpenFailure(target, errors.New("applying sqlite schema: SQL logic error: table kv already exists (1)"))
	if !strings.Contains(race.Error(), "another process created binding") {
		t.Errorf("the genesis race is not explained: %v", race)
	}
	if !strings.Contains(race.Error(), "Start the city again") {
		t.Errorf("the genesis race does not say what to do: %v", race)
	}

	fault := infraGenesisOpenFailure(target, errors.New("permission denied"))
	if strings.Contains(fault.Error(), "another process created binding") {
		t.Errorf("an ordinary open failure was reported as a lost race: %v", fault)
	}
	if !strings.Contains(fault.Error(), "creating binding") || !strings.Contains(fault.Error(), "permission denied") {
		t.Errorf("an ordinary open failure lost its cause: %v", fault)
	}
}

// TestStorageGateGenesisRecordsAnEmptyCopy covers the third branch: a city
// configured for a split with nothing to move starts, and records that it had
// nothing to move.
//
// The manifest matters as much as the marker. An absent manifest turns stranded
// -write detection off for the city's whole life; an empty one keeps it armed
// from the first boot, which is why genesis writes one rather than skipping it.
func TestStorageGateGenesisRecordsAnEmptyCopy(t *testing.T) {
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	stubInfraMigrationSource(t)
	target := mustResolveInfraTarget(t, cityPath, cfg)

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err != nil {
		t.Fatalf("a genesis city was refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })
	if routes == nil {
		t.Fatal("a genesis city resolved no routes")
	}

	proven, recorded, err := readInfraCopyManifest(target)
	if err != nil {
		t.Fatalf("reading the manifest: %v", err)
	}
	if !recorded {
		t.Fatal("genesis recorded no proven-copy manifest, so stranded-write detection is off for this city forever")
	}
	if len(proven) != 0 {
		t.Errorf("genesis recorded %d proven bead(s), want none", len(proven))
	}
	markerBefore, err := os.Stat(target.MarkerPath())
	if err != nil {
		t.Fatalf("genesis wrote no marker: %v", err)
	}

	// The second boot is the real assertion: a converged city must be silent,
	// and must not rewrite the record it already holds.
	var second bytes.Buffer
	again, err := storageBootGate(cityPath, cfg, "gc start", nil, &second)
	if err != nil {
		t.Fatalf("the second boot of a converged city was refused: %v", err)
	}
	t.Cleanup(func() { _ = again.close() })
	if second.Len() != 0 {
		t.Errorf("the second boot of a converged city wrote to stderr: %q", second.String())
	}
	markerAfter, err := os.Stat(target.MarkerPath())
	if err != nil {
		t.Fatalf("stat marker after the second boot: %v", err)
	}
	if !markerAfter.ModTime().Equal(markerBefore.ModTime()) {
		t.Error("the second boot rewrote the convergence marker; a converged boot must change nothing")
	}
}

// TestStorageGateServesAConvergedCityFromTheBinding is the open branch, proved
// through the read path rather than through the gate's own return value: a
// session bead written through the class resolver lands in the binding database
// and NOT in the retained work store.
func TestStorageGateServesAConvergedCityFromTheBinding(t *testing.T) {
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "carried across", Type: "session"})

	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("the migration reported %s: %s", got.Outcome, log.String())
	}

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err != nil {
		t.Fatalf("a converged city was refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })

	sessions := resolveSessionStore(routes, source, cfg, cityPath, nil)
	if sessions == source {
		t.Fatal("the session class still resolves to the work store on a converged city")
	}
	written, err := sessions.Create(beads.Bead{Title: "written after cutover", Type: "session"})
	if err != nil {
		t.Fatalf("writing a session bead through the routed store: %v", err)
	}

	binding := openMigratedDestination(t, mustResolveInfraTarget(t, cityPath, cfg))
	if _, err := binding.Get(written.ID); err != nil {
		t.Errorf("the routed write did not land in the binding: %v", err)
	}
	if _, err := source.Get(written.ID); err == nil {
		t.Errorf("the routed write also landed in the work store as %s; a relocated class must not double-write", written.ID)
	}
	if got := resolveClassStore(routes, source, cfg, cityPath, config.BeadClassWork, nil); got != source {
		t.Error("work stopped resolving to the work store on a converged city")
	}
}

// TestStorageGateRefusesWhatItCouldNotCheck covers the uncheckable verdict: a
// marker is present and a read failed, so nothing proved the binding is safe to
// serve.
//
// Three separate claims, and the last two are what the outcome taxonomy exists
// for: the refusal names the read that failed, never calls the city
// unconverged, and never prints the revert — which on a city carrying a marker
// would abandon everything written since cutover.
func TestStorageGateRefusesWhatItCouldNotCheck(t *testing.T) {
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "carried across", Type: "session"})

	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("the migration reported %s: %s", got.Outcome, log.String())
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	// A manifest that is a directory cannot be read as a file, and the failure
	// is a fact about the read rather than about the city.
	if err := os.Remove(target.ManifestPath()); err != nil {
		t.Fatalf("removing the manifest: %v", err)
	}
	if err := os.Mkdir(target.ManifestPath(), 0o755); err != nil {
		t.Fatalf("replacing the manifest with a directory: %v", err)
	}

	var stderr bytes.Buffer
	if _, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr); err == nil {
		t.Fatal("a city whose convergence could not be checked started anyway")
	} else {
		if strings.Contains(err.Error(), "has not migrated onto it") {
			t.Errorf("an unreadable check was reported as non-convergence: %v", err)
		}
		if strings.Contains(err.Error(), "reverting loses nothing") {
			t.Errorf("an unreadable check printed the revert: %v", err)
		}
		if !strings.Contains(err.Error(), "Do NOT revert") {
			t.Errorf("the refusal does not withhold the revert explicitly: %v", err)
		}
	}
	if !strings.Contains(stderr.String(), target.ManifestPath()) {
		t.Errorf("the refusal does not name the read that failed: %q", stderr.String())
	}
}

// engineLessProvider is a provider that resolves and cannot serve. Embedding
// the Provider interface promotes exactly Provider's own methods, so whatever
// the wrapped value implements beyond it — OpenEngine among them — is hidden.
type engineLessProvider struct{ storebinding.Provider }

// engineLessProviderFactory mints the compiled provider under its own ID and
// then strips the engine seam off it, so a plan resolved against a registry
// holding this factory carries a binding that resolves and cannot serve.
type engineLessProviderFactory struct{ inner storebinding.ProviderFactory }

func (f engineLessProviderFactory) ID() storebinding.ProviderID { return f.inner.ID() }

func (f engineLessProviderFactory) New(spec storebinding.BindingSpec) (storebinding.Provider, error) {
	provider, err := f.inner.New(spec)
	if err != nil {
		return nil, err
	}
	return engineLessProvider{provider}, nil
}

// TestStorageRoutesRefuseAProviderThatOpensNoEngine pins the loud half of the
// EngineOpener seam: a binding whose provider does not implement it is a
// refusal that names the provider, never a fall-through to the work store.
//
// The fall-through is the failure worth a test of its own. It would serve a
// relocated class out of the very ledger the class was moved off, and every
// read would succeed while answering from the wrong store.
//
// The engine-less binding has to be IN the plan openStorageRoutes resolves, or
// the refusal under test is never reached: a plan that does not carry the
// binding at all fails one step earlier, on the name, and that refusal says
// nothing about the seam. So the registry the plan is resolved against is the
// thing that is swapped, and the binding it produces is the one the routes are
// asked to open.
func TestStorageRoutesRefuseAProviderThatOpensNoEngine(t *testing.T) {
	root := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(root, "store"))

	// The control: the compiled provider DOES open an engine, so a refusal
	// below is about the stripped seam rather than about this build.
	compiled, err := resolveCityStoragePlan(root, cfg)
	if err != nil {
		t.Fatalf("resolving the plan: %v", err)
	}
	if _, ok := storebinding.EngineOpenerFor(compiled.Bindings()[0]); !ok {
		t.Fatal("the built-in provider does not implement EngineOpener, so the refusal below proves nothing")
	}

	prev := newStorageRegistryForPlan
	newStorageRegistryForPlan = func() (*storebinding.ProviderRegistry, error) {
		registry := storebinding.NewProviderRegistry()
		if err := registry.Register(engineLessProviderFactory{sqlitebinding.BeadsProviderFactory{}}); err != nil {
			return nil, err
		}
		if err := registry.Freeze(); err != nil {
			return nil, err
		}
		return registry, nil
	}
	t.Cleanup(func() { newStorageRegistryForPlan = prev })

	plan, err := resolveCityStoragePlan(root, cfg)
	if err != nil {
		t.Fatalf("resolving the plan against the engine-less registry: %v", err)
	}
	target := mustResolveInfraTarget(t, root, cfg)
	planned := plan.Bindings()[0]
	if string(planned.Name) != target.Binding {
		t.Fatalf("the resolved plan carries binding %q, want the target's %q", planned.Name, target.Binding)
	}
	if _, ok := storebinding.EngineOpenerFor(planned); ok {
		t.Fatal("the binding in the resolved plan still offers an engine opener")
	}

	routes, err := openStorageRoutes(plan, target)
	if err == nil {
		_ = routes.close()
		t.Fatal("routes opened for a binding whose provider opens no bead engine; the classes assigned to it would have fallen through to the work store")
	}
	if routes != nil {
		t.Errorf("a refused open still returned routes %+v", routes)
	}
	if !strings.Contains(err.Error(), "does not open a bead engine") {
		t.Errorf("the refusal does not say the provider opens no engine: %v", err)
	}
	if !strings.Contains(err.Error(), string(planned.ProviderID)) {
		t.Errorf("the refusal does not name the provider %q: %v", planned.ProviderID, err)
	}
	// Positive evidence that the refusal opened nothing: the binding parent is
	// listed and the root is not among its entries. A stat of the database
	// path would report "absent" just as readily for a path typo.
	if directoryHolds(t, root, filepath.Base(target.Root)) {
		t.Errorf("a refused route open created the binding root %s (the database would be %s)", target.Root, target.Database)
	}
}

// TestStorageWorkPinsDescribeEveryBoundScope proves the pins the plan is
// resolved against name HQ and every bound rig, and that the physical identity
// each one carries follows the store root it resolves to.
//
// Physical identity is not grouping, and the last arm pins the difference: the
// plan groups on the whole (opener, component, physical) triple and each rig is
// its own component, so two rigs sharing a root agree on the physical fact and
// still plan as two participants. Reading the shared identity as a group is
// what the comment on cityStorageWorkPins used to claim.
func TestStorageWorkPinsDescribeEveryBoundScope(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	cfg := &config.City{
		ResolvedWorkspacePrefix: "gc",
		Rigs: []config.Rig{
			{Name: "alpha", Prefix: "ga", Path: filepath.Join(root, "alpha")},
			{Name: "beta", Prefix: "gb", Path: shared},
			{Name: "gamma", Prefix: "gg", Path: shared},
			{Name: "unbound", Prefix: "gu"},
		},
	}
	pins := cityStorageWorkPins(root, cfg)
	if len(pins.Rigs) != 3 {
		t.Fatalf("pinned %d rig scopes, want 3 (the unbound rig has no workspace to pin)", len(pins.Rigs))
	}
	if pins.Recorded {
		t.Error("config-derived pins claim to be a durable record")
	}
	if len(pins.Observed) != 0 {
		t.Error("config-derived pins carry an observation, which can only trip the drift refusal")
	}
	if pins.Rigs[1].PhysicalID != pins.Rigs[2].PhysicalID {
		t.Error("two rigs on one workspace root pinned different physical identities")
	}
	if pins.Rigs[0].PhysicalID == pins.Rigs[1].PhysicalID {
		t.Error("two rigs on different workspace roots pinned the same physical identity")
	}

	registry := storebinding.NewProviderRegistry()
	if err := registry.Freeze(); err != nil {
		t.Fatalf("freezing an empty registry: %v", err)
	}
	plan, err := storebinding.ResolveStoragePlan(registry, (&config.City{}).EffectiveStorage(), pins)
	if err != nil {
		t.Fatalf("the config-derived pins do not resolve a default plan: %v", err)
	}
	participants, err := plan.WorkParticipants()
	if err != nil {
		t.Fatalf("reading the planned work participants: %v", err)
	}
	if len(participants) != 4 {
		t.Errorf("the plan grouped %d work participant(s), want 4 (HQ plus one per bound rig): two rigs on one root share a physical identity but not a component", len(participants))
	}
}

// TestStorageGateChecksTheRollbackSpelling pins what the runbook tells an
// operator to type when rolling a split back, because a rollback that refuses
// to boot is worse than no rollback at all.
//
// Both halves of the spelling are asserted: the class map goes back to work AND
// the binding definition goes with it, since a binding no class selects is a
// refusal. The half-finished revert is the third arm — it must be refused
// loudly rather than routed halfway.
func TestStorageGateChecksTheRollbackSpelling(t *testing.T) {
	allWork := func(root string) *config.City {
		cfg := infraSplitConfig(filepath.Join(root, "store"))
		cfg.Storage.Classes = config.StorageClasses{
			Work: config.StorageWorkBinding, Graph: config.StorageWorkBinding,
			Sessions: config.StorageWorkBinding, Messaging: config.StorageWorkBinding,
			Orders: config.StorageWorkBinding, Nudges: config.StorageWorkBinding,
		}
		return cfg
	}

	t.Run("the whole spelling boots", func(t *testing.T) {
		root := t.TempDir()
		cfg := allWork(root)
		cfg.Storage.Bindings = nil
		refuseInfraMigrationSource(t)

		routes, err := storageBootGate(root, cfg, "gc start", nil, io.Discard)
		if err != nil {
			t.Fatalf("the documented rollback refused to boot: %v", err)
		}
		if routes != nil {
			t.Errorf("a reverted city still opened routes %+v", routes)
		}
	})

	t.Run("keeping the binding definition refuses", func(t *testing.T) {
		root := t.TempDir()
		if _, err := storageBootGate(root, allWork(root), "gc start", nil, io.Discard); err == nil {
			t.Fatal("a binding no class selects was ignored; the runbook tells operators it is refused")
		} else if !errors.Is(err, storebinding.ErrUnreferencedBinding) {
			t.Errorf("the refusal is %v, want an %v", err, storebinding.ErrUnreferencedBinding)
		}
	})

	t.Run("a half-finished revert refuses", func(t *testing.T) {
		root := t.TempDir()
		cfg := allWork(root)
		cfg.Storage.Classes.Nudges = "infra"

		if _, err := storageBootGate(root, cfg, "gc start", nil, io.Discard); err == nil {
			t.Fatal("a class left on the binding was routed halfway")
		} else if !strings.Contains(err.Error(), "whole infrastructure split or none of it") {
			t.Errorf("the refusal does not say what this build serves: %v", err)
		}
	})
}

// TestStorageGateRefusesAnUnknownProvider proves the plan's structural refusals
// reach boot: a binding naming a provider this binary does not compile in stops
// the city instead of being ignored.
func TestStorageGateRefusesAnUnknownProvider(t *testing.T) {
	root := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(root, "store"))
	cfg.Storage.Bindings["infra"] = config.StorageBindingConfig{Provider: "not-compiled-in", Path: filepath.Join(root, "store")}

	if _, err := storageBootGate(root, cfg, "gc start", nil, io.Discard); err == nil {
		t.Fatal("a binding naming an uncompiled provider started the city")
	} else if !errors.Is(err, storebinding.ErrUnknownProvider) {
		t.Errorf("the refusal is %v, want an %v", err, storebinding.ErrUnknownProvider)
	}
}

// equalStrings reports whether two sorted id lists hold the same ids.
func equalStrings(want, got []string) bool {
	if len(want) != len(got) {
		return false
	}
	sort.Strings(want)
	sort.Strings(got)
	for i := range want {
		if want[i] != got[i] {
			return false
		}
	}
	return true
}

// storageTestRequest builds an operator request against a city whose path is
// canonical, which the migration guard requires.
func storageTestRequest(t *testing.T, cfg *config.City) storageOperatorRequest {
	t.Helper()
	cityPath, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalizing the city path: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("creating the city .gc directory: %v", err)
	}
	return storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: true}
}

// TestStorageMigrateRefusesWhileAnotherMigratorHoldsTheCity pins the guard: two
// migrators over one city is the one concurrency the command can exclude
// outright, and it does.
func TestStorageMigrateRefusesWhileAnotherMigratorHoldsTheCity(t *testing.T) {
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	request := storageTestRequest(t, cfg)
	stubInfraMigrationSource(t)
	stubInfraControllerPing(t, 0)

	held, err := storebinding.AcquireMigrationGuard(context.Background(), cityMigrationGuardDirectory(request.CityPath), storageMigrationGeneration)
	if err != nil {
		t.Fatalf("taking the first guard: %v", err)
	}
	t.Cleanup(func() { _ = held.Release() })

	var stdout, stderr bytes.Buffer
	if code := doStorageMigrate(context.Background(), request, &stdout, &stderr); code == 0 {
		t.Fatal("a second migrator ran while the first held the city")
	}
	if !strings.Contains(stderr.String(), "another storage migration holds this city") {
		t.Errorf("the refusal does not name the concurrent migrator: %q", stderr.String())
	}
	assertNoConvergenceMarker(t, mustResolveInfraTarget(t, request.CityPath, cfg),
		"a migrator that refused must record no convergence")
}

// TestStorageMigrateRefusesRigResidueByName covers the rig-scope preflight.
//
// The remedy is asserted as carefully as the refusal: it must not name an
// importer, because this binary carries none, and a remedy naming a command
// that does not exist is an instruction that fails at the shell.
func TestStorageMigrateRefusesRigResidueByName(t *testing.T) {
	rigPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	request := storageTestRequest(t, cfg)
	cfg.Rigs = []config.Rig{{Name: "alpha", Prefix: "ga", Path: rigPath}}

	stubInfraMigrationSource(t)
	stubInfraControllerPing(t, 0)

	rig := beads.NewMemStore()
	stray := mustCreateInfraBead(t, rig, beads.Bead{Title: "a session in a rig", Type: "session"})
	mustCreateInfraBead(t, rig, beads.Bead{Title: "ordinary rig work", Type: "task"})
	prev := openStorageScopeStore
	openStorageScopeStore = func(storePath, cityPath string) (beads.Store, error) {
		if storePath == rigPath {
			return rig, nil
		}
		return prev(storePath, cityPath)
	}
	t.Cleanup(func() { openStorageScopeStore = prev })

	var stdout, stderr bytes.Buffer
	if code := doStorageMigrate(context.Background(), request, &stdout, &stderr); code == 0 {
		t.Fatal("a city with infrastructure beads in a rig scope migrated anyway")
	}
	got := stderr.String()
	if !strings.Contains(got, stray.ID) || !strings.Contains(got, "rig alpha") {
		t.Errorf("the refusal does not name the bead and its rig: %q", got)
	}
	for _, forbidden := range []string{"import", "migrate storage"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("the remedy names %q, which this binary does not carry: %q", forbidden, got)
		}
	}
	assertNoConvergenceMarker(t, mustResolveInfraTarget(t, request.CityPath, cfg),
		"a migrator that refused must record no convergence")
}

// TestStorageMigrateRequiresItsSourceExplicitly proves the command will not
// migrate from a source nobody named.
func TestStorageMigrateRequiresItsSourceExplicitly(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newStorageCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"migrate"})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); !errors.Is(err, errExit) {
		t.Fatalf("migrate with no source returned %v, want the exit sentinel", err)
	}
	if !strings.Contains(stderr.String(), "--from-work") {
		t.Errorf("the refusal does not name the source flag: %q", stderr.String())
	}
}

// TestStorageCommandTreeIsBuiltFromTheBlessedSpelling proves the boot refusal
// and the command tree cannot drift: the tree is decomposed from the same
// string the refusal prints, and it resolves.
func TestStorageCommandTreeIsBuiltFromTheBlessedSpelling(t *testing.T) {
	surface, err := parseOperatorCommandSpelling(storageMigrationCommand)
	if err != nil {
		t.Fatalf("the blessed spelling does not decompose: %v", err)
	}
	root := newStorageCmd(io.Discard, io.Discard)
	if root.Use != surface.Namespace {
		t.Errorf("the tree's namespace is %q, want %q", root.Use, surface.Namespace)
	}
	if root.Hidden {
		t.Error("the operator surface is hidden; it is the documented way to migrate a city")
	}
	found, _, err := root.Find([]string{surface.Verb})
	if err != nil {
		t.Fatalf("resolving %q: %v", surface.Verb, err)
	}
	if found.Flags().Lookup(surface.Flag) == nil {
		t.Errorf("the resolved command registers no --%s flag", surface.Flag)
	}
	if _, _, err := root.Find([]string{storageStatusVerb}); err != nil {
		t.Fatalf("resolving %q: %v", storageStatusVerb, err)
	}

	for _, bad := range []string{"", "gc storage migrate", "storage migrate --from-work", "gc storage migrate from-work", "gc --storage migrate --from-work"} {
		if _, err := parseOperatorCommandSpelling(bad); err == nil {
			t.Errorf("the spelling %q decomposed; it should not", bad)
		}
	}
	if cmd := newStorageCmdFromSpelling("not a command", io.Discard, io.Discard); cmd.RunE == nil {
		t.Error("an undecomposable spelling built a command that reports nothing")
	}
}

// TestStorageStatusCreatesNothing pins the read-only claim with a fingerprint
// of the whole binding parent taken before and after.
//
// A path-by-path assertion would only catch the paths someone thought to name.
// The fingerprint catches the directory, the database, its write-ahead log, its
// shared-memory index, the marker and the manifest at once — and the failure it
// exists to catch is exactly the one a status command has: opening the engine
// to describe it creates the very database the report is about.
func TestStorageStatusCreatesNothing(t *testing.T) {
	bindingParent := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(bindingParent, "store"))
	request := storageTestRequest(t, cfg)
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "a session", Type: "session"})

	before := treeFingerprint(t, bindingParent)
	var stdout, stderr bytes.Buffer
	if code := doStorageStatus(request, &stdout, &stderr); code == 0 {
		t.Errorf("status exited 0 on an unconverged city; a deployment script cannot gate on it. stdout=%q", stdout.String())
	}
	if got := treeFingerprint(t, bindingParent); !equalStrings(before, got) {
		t.Errorf("status changed the binding tree:\n before %v\n after  %v", before, got)
	}
	if !strings.Contains(stdout.String(), storageMigrationCommand) {
		t.Errorf("status does not name the command that would converge the city: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "1 infrastructure bead(s) retained") {
		t.Errorf("status does not report the retained source census: %q", stdout.String())
	}
}

// treeFingerprint returns every path under root with its size, so a test can
// prove a read-only path created and grew nothing.
func treeFingerprint(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, statErr := entry.Info()
		if statErr != nil {
			return statErr
		}
		out = append(out, fmt.Sprintf("%s:%d", path, info.Size()))
		return nil
	})
	if err != nil {
		t.Fatalf("fingerprinting %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

// TestNewCityRuntimeRefusesAnUnconvergedCity is the boundary the two production
// boot paths consume: the constructor is fallible, and the error it returns is
// the refusal with the command in it.
//
// The controller prints that error and exits 1; the supervisor returns it from
// its post-prepare step, which records city_runtime_failed and moves on to the
// next city. Both are one line at the call site, and both are worthless if the
// constructor cannot refuse — which is what this pins.
func TestNewCityRuntimeRefusesAnUnconvergedCity(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "a session", Type: "session"})

	var stderr bytes.Buffer
	cr, err := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		Cfg:      cfg,
		Stdout:   io.Discard,
		Stderr:   &stderr,
	})
	if err == nil {
		t.Fatal("newCityRuntime built a runtime for a city configured for a binding it never converged on")
	}
	if cr != nil {
		t.Error("a refused newCityRuntime still returned a runtime")
	}
	if !strings.Contains(err.Error(), storageMigrationCommand) {
		t.Errorf("the constructor error does not name %q: %v", storageMigrationCommand, err)
	}
}
