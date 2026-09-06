package main

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
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
		cityPath := t.TempDir()
		var stderr bytes.Buffer
		routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
		if err != nil {
			t.Fatalf("a city with no [storage] was refused: %v", err)
		}
		if routes != nil {
			t.Fatalf("a city with no [storage] resolved routes %+v, want none", routes)
		}
		if stderr.Len() != 0 {
			t.Errorf("a city with no [storage] wrote to stderr: %q", stderr.String())
		}
		// The one read the bypass performs must also create nothing: a city
		// that was never configured is byte-identical after the gate ran.
		if entries := directoryEntryNames(t, cityPath); len(entries) != 0 {
			t.Errorf("the bypass left %v in a city that authored no [storage]", entries)
		}
	}
	if *registries != 0 {
		t.Errorf("the no-config path constructed %d provider registr(ies); the bypass must short-circuit before any of this runs", *registries)
	}
}

// directoryEntryNames lists a directory's own names, failing on a directory it
// cannot read rather than reporting it as empty.
func directoryEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("listing %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// TestDeletingTheStorageSectionCannotAbandonAServedBinding closes the last way
// past the served-binding hold.
//
// The refusal ladder used to point straight at this edit: pointing the classes
// back at work refuses while the note stands, so the operator deletes the
// section the classes live in — and that reached the bypass, served everything
// from the work store, and left months of session, graph and mail state in a
// binding nothing would open again. The city has to be held on this edit too,
// and the hold has to name the note, because deleting the note is the only
// attestation that says the binding's contents are recovered or abandoned.
func TestDeletingTheStorageSectionCannotAbandonAServedBinding(t *testing.T) {
	cityPath := t.TempDir()
	if err := writeBornSplitServedNote(cityPath, bornSplitServedNote{
		Binding:  "infra",
		Provider: "outoftree-engine",
		Location: filepath.Join(cityPath, ".gc", "storage", "infra"),
	}); err != nil {
		t.Fatalf("recording the served binding: %v", err)
	}

	registries := countStorageRegistryConstructions(t)
	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, &config.City{}, "gc start", nil, &stderr)
	if err == nil {
		_ = routes.close()
		t.Fatal("a city that served a split booted with its [storage] section deleted")
	}
	if !strings.Contains(err.Error(), bornSplitServedNotePath(cityPath)) {
		t.Errorf("the refusal does not name the note that holds it: %v", err)
	}
	if !strings.Contains(err.Error(), "infra") {
		t.Errorf("the refusal does not name the binding the city served from: %v", err)
	}
	// Held, not investigated: the hold is one read of one path, and a city
	// this build must not serve does not get a registry or a plan built for it.
	if *registries != 0 {
		t.Errorf("the held bypass constructed %d provider registr(ies), want none", *registries)
	}
}

// TestCorruptNoteHoldsTheDeletedSectionToo keeps the bypass's new question
// fail-closed in the same way every other reader of the note is: a note that
// cannot be read withholds exactly what a readable one would, because a corrupt
// file must not grant what its absence grants.
func TestCorruptNoteHoldsTheDeletedSectionToo(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("creating the city .gc: %v", err)
	}
	if err := os.WriteFile(bornSplitServedNotePath(cityPath), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("writing the corrupt note: %v", err)
	}

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, nil, "gc start", nil, &stderr)
	if err == nil {
		_ = routes.close()
		t.Fatal("a city with an unreadable served-binding note booted through the bypass")
	}
	if !strings.Contains(err.Error(), bornSplitServedNotePath(cityPath)) {
		t.Errorf("the refusal does not name the note it could not read: %v", err)
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

// TestStorageGateRefusesARelocatedWorkBinding is the same gate applied to the
// one class whose relocation this build has no way to carry out.
//
// infraMigrationClasses excludes work by construction, so nothing in cmd/gc
// would move the ledger; the saga that could — internal/storebinding's work
// participants, prepare receipts and provider proofs — has no caller in the
// binary yet. A city that pointed [storage.classes].work at another binding and
// STARTED would therefore serve an empty ledger while every bead it owns sat in
// the work store, unread and unreferenced. Refusing is the only safe answer
// this build has, and it is the epic's fail-closed rule reaching its hardest
// case.
//
// The shape table above pins storageSplitShapeOf's verdict on this config. That
// is one function; this is the gate, which is what a starting city actually
// meets. Nothing between them was tested, so the classification could have been
// correct and unconsulted.
func TestStorageGateRefusesARelocatedWorkBinding(t *testing.T) {
	root := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(root, "store"))
	cfg.Storage.Classes.Work = "infra"

	var stderr bytes.Buffer
	routes, err := storageBootGate(root, cfg, "gc start", nil, &stderr)
	if err == nil {
		t.Fatal("a city that moved its work ledger off the reserved binding started; it is now serving a binding that holds none of its work")
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
	// The refusal must also say what recovery looks like: the ids alone tell
	// the operator what is wrong, not what to do, and this string is the only
	// output the supervisor records.
	if !strings.Contains(err.Error(), "gc storage status") {
		t.Errorf("the refusal names no recovery re-check: %v", err)
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
	if !strings.Contains(err.Error(), contract.BackendNotOpenedGuarantee) {
		t.Errorf("the refusal does not tell the operator their storage is untouched: %v", err)
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
	plan, err := storebinding.ResolveStoragePlan(registry, (&config.City{}).EffectiveStorage(), pins, "")
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
//
// The refusal also enumerates the providers this binary does carry. "Provider
// not found" alone cannot tell an operator whether they typed the ID wrong or
// are running a build that never had it, and those have different fixes.
func TestStorageGateRefusesAnUnknownProvider(t *testing.T) {
	root := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(root, "store"))
	cfg.Storage.Bindings["infra"] = config.StorageBindingConfig{Provider: "not-compiled-in", Path: filepath.Join(root, "store")}

	_, err := storageBootGate(root, cfg, "gc start", nil, io.Discard)
	if err == nil {
		t.Fatal("a binding naming an uncompiled provider started the city")
	}
	if !errors.Is(err, storebinding.ErrUnknownProvider) {
		t.Fatalf("the refusal is %v, want an %v", err, storebinding.ErrUnknownProvider)
	}
	if !strings.Contains(err.Error(), `"not-compiled-in"`) {
		t.Errorf("the refusal does not name the provider: %v", err)
	}
	for _, factory := range compiledStorageProviderFactories() {
		if !strings.Contains(err.Error(), string(factory.ID())) {
			t.Errorf("the refusal omits compiled provider %q: %v", factory.ID(), err)
		}
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
	if cmd := newStorageCmdFromSpellings("not a command", storageRecoveryCommand, io.Discard, io.Discard); cmd.RunE == nil {
		t.Error("an undecomposable migration spelling built a command that reports nothing")
	}
	if cmd := newStorageCmdFromSpellings(storageMigrationCommand, "not a command", io.Discard, io.Discard); cmd.RunE == nil {
		t.Error("an undecomposable recovery spelling built a command that reports nothing")
	}
	// Two operator commands under different parents would build one tree and
	// leave the other spelling naming a command that does not resolve.
	if cmd := newStorageCmdFromSpellings(storageMigrationCommand, "gc elsewhere recover-stranded --from-work", io.Discard, io.Discard); cmd.RunE == nil {
		t.Error("spellings under different namespaces built a tree instead of reporting the conflict")
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

// treeFingerprint returns every path under root against a digest of its
// contents, so a test can prove a read-only path created, removed, and rewrote
// nothing.
//
// Contents rather than size: the rewrite these tests exist to catch is a SQLite
// checkpoint applying WAL frames into an already-sized main database, which
// routinely lands on the same byte count. A size comparison reports that as
// unchanged, so a helper built on one could only ever prove path stability
// while its failure message claimed byte identity.
//
// Directories carry no digest. A directory's reported size tracks its entry
// count on some filesystems, which is a real addition being counted twice; the
// path set itself already carries every addition and removal.
func treeFingerprint(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			out = append(out, path+":dir")
			return nil
		}
		contents, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		out = append(out, fmt.Sprintf("%s:%x", path, sha256.Sum256(contents)))
		return nil
	})
	if err != nil {
		t.Fatalf("fingerprinting %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

// assertReadOnlyDiagnosticResidue is the claim the two read-only diagnostics can
// make about a binding directory they opened a bead engine in.
//
// They open through openInfraBindingReadOnly, whose mode=ro connection cannot
// take the write lock — so it can neither mutate a row nor checkpoint the WAL on
// close. That is exactly what makes it safe to point at a binding a controller
// is serving, and it is also why the tree is not identical afterwards: SQLite
// materializes the -wal and -shm sidecars it needs to read a WAL-mode database,
// and a connection that cannot take the write lock cannot remove them on close
// either. Removing them is the writer opener's ability, and it removes them by
// checkpointing — which rewrites the main database and the -wal of a store
// something else may be serving. That is the harm, not the guarantee.
//
// The residue is confined to the case a deploy gate does not care about. On a
// serving city both sidecars are already on disk, held open by the process
// serving the binding, so a read-only diagnostic adds nothing at all and
// changes nothing — TestTheReadOnlyDiagnosticsLeaveALiveWALIntact pins that
// directly, on the bytes. Only on a quiescent binding, where a clean close
// already checkpointed and removed them, do the two files come back.
//
// So the claim is split in two and both halves are asserted: every path that
// was there before is still there byte-for-byte, and the only additions are
// those two sidecars, named exactly rather than tolerated as "some new files".
// The first half is a content digest rather than a size, because the rewrite it
// exists to catch — a checkpoint applying WAL frames into an already-sized
// database — routinely preserves the byte count; treeFingerprint carries the
// reasoning.
func assertReadOnlyDiagnosticResidue(t *testing.T, before, after []string, database string) {
	t.Helper()
	beforeByPath := fingerprintByPath(t, before)
	afterByPath := fingerprintByPath(t, after)
	for path, entry := range beforeByPath {
		got, ok := afterByPath[path]
		if !ok {
			t.Errorf("a read-only diagnostic removed %s", path)
			continue
		}
		if got != entry {
			t.Errorf("a read-only diagnostic rewrote a file it was asked to read: %s before, %s after", entry, got)
		}
	}
	allowed := map[string]bool{database + "-wal": true, database + "-shm": true}
	for path, entry := range afterByPath {
		if _, ok := beforeByPath[path]; ok {
			continue
		}
		if !allowed[path] {
			t.Errorf("a read-only diagnostic left %s behind, which is neither the -wal nor the -shm sidecar SQLite needs to read %s", entry, database)
		}
	}
}

// fingerprintByPath indexes a treeFingerprint by path, so a comparison can tell
// a changed entry apart from an added one.
func fingerprintByPath(t *testing.T, fingerprint []string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(fingerprint))
	for _, entry := range fingerprint {
		cut := strings.LastIndex(entry, ":")
		if cut < 0 {
			t.Fatalf("fingerprint entry %q carries no digest", entry)
		}
		out[entry[:cut]] = entry
	}
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

// renamedEngineProviderFactory re-registers this build's own engine factory
// under a foreign provider ID. A plan resolved against it carries a binding
// that resolveInfraBindingTarget refuses — the provider is not the built-in
// engine — while the binding itself still opens a real bead engine. That is
// the exact shape of an out-of-tree engine provider, built from compiled
// parts so the test proves the gate, not a mock.
type renamedEngineProviderFactory struct {
	inner storebinding.ProviderFactory
	id    storebinding.ProviderID
}

func (f renamedEngineProviderFactory) ID() storebinding.ProviderID { return f.id }

func (f renamedEngineProviderFactory) New(spec storebinding.BindingSpec) (storebinding.Provider, error) {
	spec.Provider = f.inner.ID()
	provider, err := f.inner.New(spec)
	if err != nil {
		return nil, err
	}
	return renamedEngineProvider{Provider: provider, innerID: f.inner.ID()}, nil
}

// renamedEngineProvider forwards the whole provider facade and re-spells the
// binding spec's provider before delegating OpenEngine, mirroring what a real
// out-of-tree provider does with its own spec: the inner engine's "refuse a
// foreign spec" defense stays armed, and the seam under test stays honest.
type renamedEngineProvider struct {
	storebinding.Provider
	innerID storebinding.ProviderID
}

func (p renamedEngineProvider) OpenEngine(spec storebinding.BindingSpec, classes storebinding.ClassSet) (beads.Store, io.Closer, error) {
	opener, ok := p.Provider.(storebinding.EngineOpener)
	if !ok {
		return nil, nil, errors.New("renamed inner provider opens no engine")
	}
	spec.Provider = p.innerID
	return opener.OpenEngine(spec, classes)
}

// bornSplitCity configures a city whose infrastructure binding is served by a
// registered non-built-in engine provider, and returns the in-memory work
// store the born-split discipline scans.
func bornSplitCity(t *testing.T) (cityPath string, cfg *config.City, source beads.Store) {
	t.Helper()
	cityPath = t.TempDir()
	cfg, source = bornSplitCityAt(t, cityPath)
	return cityPath, cfg, source
}

// bornSplitCityAt is bornSplitCity against a caller-owned city directory, for
// tests that re-point an existing city rather than starting fresh.
func bornSplitCityAt(t *testing.T, cityPath string) (cfg *config.City, source beads.Store) {
	t.Helper()
	source = stubInfraMigrationSource(t)
	cfg = infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	binding := cfg.Storage.Bindings["infra"]
	binding.Provider = "outoftree-engine"
	cfg.Storage.Bindings["infra"] = binding

	prev := newStorageRegistryForPlan
	newStorageRegistryForPlan = func() (*storebinding.ProviderRegistry, error) {
		registry := storebinding.NewProviderRegistry()
		if err := registry.Register(renamedEngineProviderFactory{
			inner: sqlitebinding.BeadsProviderFactory{},
			id:    "outoftree-engine",
		}); err != nil {
			return nil, err
		}
		if err := registry.Freeze(); err != nil {
			return nil, err
		}
		return registry, nil
	}
	t.Cleanup(func() { newStorageRegistryForPlan = prev })
	return cfg, source
}

// TestStorageGateServesBornSplitBindingWithCleanWorkStore proves the discipline's
// serving half: a provider this build cannot migrate onto still serves the
// split when the work store holds no infrastructure bead, and the routes it
// returns reach a live engine.
func TestStorageGateServesBornSplitBindingWithCleanWorkStore(t *testing.T) {
	cityPath, cfg, source := bornSplitCity(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "plain work", Type: "task"})

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err != nil {
		t.Fatalf("a born-split city with a clean work store refused to serve: %v\nstderr: %s", err, stderr.String())
	}
	if routes == nil {
		t.Fatal("the gate served but returned no routes")
	}
	defer routes.close() //nolint:errcheck // test cleanup
	store, ok := routes.stores[coordclass.ClassSessions]
	if !ok {
		t.Fatal("the routes carry no store for the session class")
	}
	if _, err := store.Create(beads.Bead{Title: "born on the binding", Type: "session", Labels: []string{"gc:session"}}); err != nil {
		t.Fatalf("writing through the born-split routes: %v", err)
	}
}

// TestStorageGateRefusesBornSplitBindingWhenWorkStoreHoldsInfraBeads proves the
// refusing half, and pins where the evidence lives: the ids and the advice are
// in the refusal STRING, because the supervisor records that string and
// nothing else.
func TestStorageGateRefusesBornSplitBindingWhenWorkStoreHoldsInfraBeads(t *testing.T) {
	cityPath, cfg, source := bornSplitCity(t)
	strayed := mustCreateInfraBead(t, source, beads.Bead{Title: "landed in work", Type: "session", Labels: []string{"gc:session"}})

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err == nil {
		_ = routes.close()
		t.Fatal("a born-split city with an infrastructure bead in the work store served")
	}
	if !strings.Contains(err.Error(), strayed.ID) {
		t.Errorf("the refusal does not name the bead %s: %v", strayed.ID, err)
	}
	if !strings.Contains(err.Error(), `binding "infra"`) {
		t.Errorf("the refusal does not name the binding: %v", err)
	}
	if !strings.Contains(err.Error(), "Do NOT revert") {
		t.Errorf("the refusal does not withhold the revert: %v", err)
	}
	if !strings.Contains(err.Error(), "then delete them from the work store") {
		t.Errorf("the refusal does not state the full recovery; the re-check reads only the work store: %v", err)
	}
}

// TestStorageGateBornSplitCheckFailureIsUncheckableNotUnconverged proves that a
// work store that cannot be read decides nothing about the city: the refusal
// is a fact about the check, and it does not hand the operator a migration
// this build cannot perform.
func TestStorageGateBornSplitCheckFailureIsUncheckableNotUnconverged(t *testing.T) {
	cityPath, cfg, _ := bornSplitCity(t)
	failInfraMigrationSourceWith(t, func(string) (beads.Store, error) {
		return nil, errors.New("injected work-store open failure")
	})

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err == nil {
		_ = routes.close()
		t.Fatal("an unprovable born-split invariant served")
	}
	if !strings.Contains(err.Error(), "could NOT be verified") {
		t.Errorf("the refusal does not report an uncheckable city: %v", err)
	}
	if strings.Contains(err.Error(), storageMigrationCommand) {
		t.Errorf("an uncheckable born-split city was handed the migration command this build cannot honor here: %v", err)
	}
	if !strings.Contains(stderr.String(), "injected work-store open failure") {
		t.Errorf("stderr does not carry the check's failure reason: %s", stderr.String())
	}
	if strings.Contains(err.Error(), "point [storage.classes] back at") {
		t.Errorf("an uncheckable city was handed the revert instruction: %v", err)
	}
}

// TestStorageGateBornSplitListFailureIsUncheckable proves the same for a work
// store that opens but cannot be listed.
func TestStorageGateBornSplitListFailureIsUncheckable(t *testing.T) {
	cityPath, cfg, _ := bornSplitCity(t)
	failInfraMigrationSourceWith(t, func(string) (beads.Store, error) {
		return unlistableInfraSource{Store: beads.NewMemStore(), err: errors.New("injected list failure")}, nil
	})

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err == nil {
		_ = routes.close()
		t.Fatal("an unlistable work store served a born-split binding")
	}
	if !strings.Contains(err.Error(), "could NOT be verified") {
		t.Errorf("the refusal does not report an uncheckable city: %v", err)
	}
	if !strings.Contains(stderr.String(), "injected list failure") {
		t.Errorf("stderr does not carry the check's failure reason: %s", stderr.String())
	}
	if strings.Contains(err.Error(), "point [storage.classes] back at") {
		t.Errorf("an uncheckable city was handed the revert instruction: %v", err)
	}
}

// TestStorageGateBornSplitBlocksOnClosedInfraBead pins the scan's closed-tier
// reach: a closed infrastructure bead is exactly the row a weakened List query
// (dropping IncludeClosed or TierBoth) would stop seeing, and it must still
// block the binding.
func TestStorageGateBornSplitBlocksOnClosedInfraBead(t *testing.T) {
	cityPath, cfg, source := bornSplitCity(t)
	closed := mustCreateInfraBead(t, source, beads.Bead{Title: "closed session", Type: "session", Labels: []string{"gc:session"}})
	if err := source.Close(closed.ID); err != nil {
		t.Fatalf("closing the infra bead: %v", err)
	}

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err == nil {
		_ = routes.close()
		t.Fatal("a closed infrastructure bead in the work store did not block the born-split binding")
	}
	if !strings.Contains(err.Error(), closed.ID) {
		t.Errorf("the refusal does not name the closed bead %s: %v", closed.ID, err)
	}
}

// TestStorageGateBornSplitRecordsOutcomeEvents pins the event stream to the
// gate's actual decisions: exactly one converged event on a clean serve,
// exactly one unconverged event on a blocked boot, and nothing else.
func TestStorageGateBornSplitRecordsOutcomeEvents(t *testing.T) {
	cityPath, cfg, source := bornSplitCity(t)

	rec := events.NewFake()
	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", rec, &stderr)
	if err != nil {
		t.Fatalf("clean born-split boot refused: %v", err)
	}
	defer routes.close() //nolint:errcheck // test cleanup
	if got := len(rec.Events); got != 1 || rec.Events[0].Type != events.StorageBindingConverged {
		t.Fatalf("clean serve recorded %d event(s) %+v, want one %s", got, rec.Events, events.StorageBindingConverged)
	}

	mustCreateInfraBead(t, source, beads.Bead{Title: "stray", Type: "session", Labels: []string{"gc:session"}})
	blockedRec := events.NewFake()
	blocked, err := storageBootGate(cityPath, cfg, "gc start", blockedRec, &stderr)
	if err == nil {
		_ = blocked.close()
		t.Fatal("a dirtied work store served")
	}
	if got := len(blockedRec.Events); got != 1 || blockedRec.Events[0].Type != events.StorageBindingUnconverged {
		t.Fatalf("blocked boot recorded %d event(s) %+v, want one %s", got, blockedRec.Events, events.StorageBindingUnconverged)
	}
}

// TestStorageGateBornSplitEngineLessProviderRecordsNoConvergedEvent pins the
// ordering fix: a compiled provider without the engine seam must refuse BEFORE
// any outcome is recorded, or a permanently unbootable city publishes
// converged on every boot.
func TestStorageGateBornSplitEngineLessProviderRecordsNoConvergedEvent(t *testing.T) {
	cityPath, cfg, _ := bornSplitCity(t)
	prev := newStorageRegistryForPlan
	newStorageRegistryForPlan = func() (*storebinding.ProviderRegistry, error) {
		registry := storebinding.NewProviderRegistry()
		if err := registry.Register(engineLessProviderFactory{renamedEngineProviderFactory{
			inner: sqlitebinding.BeadsProviderFactory{},
			id:    "outoftree-engine",
		}}); err != nil {
			return nil, err
		}
		if err := registry.Freeze(); err != nil {
			return nil, err
		}
		return registry, nil
	}
	t.Cleanup(func() { newStorageRegistryForPlan = prev })

	rec := events.NewFake()
	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", rec, &stderr)
	if err == nil {
		_ = routes.close()
		t.Fatal("an engine-less binding served")
	}
	if !strings.Contains(err.Error(), "does not open a bead engine") {
		t.Errorf("the refusal does not name the missing engine seam: %v", err)
	}
	if len(rec.Events) != 0 {
		t.Errorf("an unservable binding recorded %d event(s): %+v", len(rec.Events), rec.Events)
	}
}

// TestBornSplitServeBlocksLaterBuiltinGenesis pins the served-binding note's
// whole reason to exist: serve under born-split once, re-point the classes at
// this build's own engine, and genesis must refuse rather than bless an empty
// store while the city's infrastructure state lives in a binding this build
// cannot read. Removing the note is the operator's attestation, and after it
// genesis proceeds.
func TestBornSplitServeBlocksLaterBuiltinGenesis(t *testing.T) {
	cityPath, cfg, _ := bornSplitCity(t)

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err != nil {
		t.Fatalf("clean born-split boot refused: %v", err)
	}
	if err := routes.close(); err != nil {
		t.Fatalf("closing born-split routes: %v", err)
	}
	notePath := bornSplitServedNotePath(cityPath)
	if _, err := os.Stat(notePath); err != nil {
		t.Fatalf("serving under born-split left no served-binding note: %v", err)
	}

	// The operator re-points the infrastructure classes at this build's own
	// engine. The work store is clean and no marker exists — exactly the
	// genesis premise — and the note is what proves the premise false.
	builtin := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	prev := newStorageRegistryForPlan
	newStorageRegistryForPlan = newStorageProviderRegistry
	t.Cleanup(func() { newStorageRegistryForPlan = prev })

	blocked, err := storageBootGate(cityPath, builtin, "gc start", nil, &stderr)
	if err == nil {
		_ = blocked.close()
		t.Fatal("genesis blessed an empty store while the served-binding note stood")
	}
	if !strings.Contains(err.Error(), notePath) {
		t.Errorf("the refusal does not name the note whose removal is the attestation: %v", err)
	}
	if !strings.Contains(err.Error(), "outoftree-engine") {
		t.Errorf("the refusal does not name the previously served provider: %v", err)
	}

	if err := os.Remove(notePath); err != nil {
		t.Fatalf("removing the served-binding note: %v", err)
	}
	served, err := storageBootGate(cityPath, builtin, "gc start", nil, &stderr)
	if err != nil {
		t.Fatalf("genesis still refused after the note was removed: %v", err)
	}
	if served == nil {
		t.Fatal("the gate served but returned no routes")
	}
	if err := served.close(); err != nil {
		t.Fatalf("closing genesis routes: %v", err)
	}
}

// TestStorageStatusReportsBornSplitStates pins the diagnosis command to the
// same discipline boot enforces: a serving born-split city reports clean and
// exits zero, a blocked one names the ids and exits non-zero — never "every
// class is served by the work store".
func TestStorageStatusReportsBornSplitStates(t *testing.T) {
	cityPath, cfg, source := bornSplitCity(t)

	var stdout, stderr bytes.Buffer
	if code := doStorageStatus(storageOperatorRequest{CityPath: cityPath, Cfg: cfg}, &stdout, &stderr); code != 0 {
		t.Fatalf("status on a clean born-split city = %d, want 0\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "born-split: clean") {
		t.Errorf("status does not report the born-split discipline: %s", stdout.String())
	}
	if strings.Contains(stdout.String(), "every class is served by the work store") {
		t.Errorf("status claims the work store serves classes it does not: %s", stdout.String())
	}

	strayed := mustCreateInfraBead(t, source, beads.Bead{Title: "stray", Type: "session", Labels: []string{"gc:session"}})
	stdout.Reset()
	if code := doStorageStatus(storageOperatorRequest{CityPath: cityPath, Cfg: cfg}, &stdout, &stderr); code == 0 {
		t.Fatalf("status on a blocked born-split city = 0, want non-zero\nstdout: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), strayed.ID) {
		t.Errorf("status does not name the blocking bead %s: %s", strayed.ID, stdout.String())
	}
}

// TestMigrateRefusesWhileServedBindingNoteStands pins the operator command to
// the same hold boot's genesis honors: a proven copy of the work store's slice
// would bless a destination that silently omits everything in the previously
// served binding.
func TestMigrateRefusesWhileServedBindingNoteStands(t *testing.T) {
	cityPath := t.TempDir()
	_ = stubInfraMigrationSource(t)
	if err := writeBornSplitServedNote(cityPath, bornSplitServedNote{Binding: "infra", Provider: "outoftree-engine"}); err != nil {
		t.Fatalf("writing the served-binding note: %v", err)
	}
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))

	var log bytes.Buffer
	report := migrateInfraClasses(t, cityPath, cfg, &log)
	if report.Outcome != infraMigrationGenesisBlocked {
		t.Fatalf("migrate outcome = %v, want genesis-blocked; log: %s", report.Outcome, log.String())
	}
	advice := infraMigrationOperatorAdvice(report, "gc storage migrate")
	if !strings.Contains(advice, bornSplitServedNotePath(cityPath)) {
		t.Errorf("the advice does not name the note whose removal is the attestation: %s", advice)
	}
	if !strings.Contains(advice, "outoftree-engine") {
		t.Errorf("the advice does not name the previously served provider: %s", advice)
	}
}

// TestBornSplitRepointToOtherBindingRefusesUntilAttested pins the symmetric
// hold: the note is last-writer-wins only through the operator's attestation,
// never through a config edit. Re-pointing a born-split city at a different
// binding must refuse naming the served one, not overwrite the record of it.
func TestBornSplitRepointToOtherBindingRefusesUntilAttested(t *testing.T) {
	cityPath, cfg, _ := bornSplitCity(t)
	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err != nil {
		t.Fatalf("first born-split boot refused: %v", err)
	}
	if err := routes.close(); err != nil {
		t.Fatalf("closing routes: %v", err)
	}

	repointed := infraSplitConfig(filepath.Join(cityPath, ".gc", "other-store"))
	other := repointed.Storage.Bindings["infra"]
	other.Provider = "outoftree-engine"
	delete(repointed.Storage.Bindings, "infra")
	repointed.Storage.Bindings["infra2"] = other
	repointed.Storage.Classes.Graph = "infra2"
	repointed.Storage.Classes.Sessions = "infra2"
	repointed.Storage.Classes.Messaging = "infra2"
	repointed.Storage.Classes.Orders = "infra2"
	repointed.Storage.Classes.Nudges = "infra2"

	blocked, err := storageBootGate(cityPath, repointed, "gc start", nil, &stderr)
	if err == nil {
		_ = blocked.close()
		t.Fatal("re-pointing at a different binding served while the note named the first")
	}
	if !strings.Contains(err.Error(), `"infra"`) {
		t.Errorf("the refusal does not name the served binding: %v", err)
	}
	if !strings.Contains(err.Error(), bornSplitServedNotePath(cityPath)) {
		t.Errorf("the refusal does not name the note: %v", err)
	}
	if strings.Contains(err.Error(), "point [storage.classes] back at") {
		t.Errorf("a note hold granted the revert instruction: %v", err)
	}

	note, present, err := readBornSplitServedNote(cityPath)
	if err != nil || !present || note.Binding != "infra" {
		t.Fatalf("the refusal overwrote the served-binding note: %+v present=%v err=%v", note, present, err)
	}
}

// TestBuiltinGenesisCityRepointToOutOfTreeRefuses pins the outbound leg: a
// city that genesised on this build's engine holds all its infrastructure
// state in the binding and none in the work store, so a clean work store
// alone must not license serving another binding.
func TestBuiltinGenesisCityRepointToOutOfTreeRefuses(t *testing.T) {
	cityPath := t.TempDir()
	_ = stubInfraMigrationSource(t)
	builtin := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, builtin, "gc start", nil, &stderr)
	if err != nil {
		t.Fatalf("genesis boot refused: %v", err)
	}
	if err := routes.close(); err != nil {
		t.Fatalf("closing genesis routes: %v", err)
	}

	cityCfg, source := bornSplitCityAt(t, cityPath)
	_ = source
	blocked, err := storageBootGate(cityPath, cityCfg, "gc start", nil, &stderr)
	if err == nil {
		_ = blocked.close()
		t.Fatal("a genesis city re-pointed at an out-of-tree binding served on a clean work store")
	}
	if !strings.Contains(err.Error(), config.StorageProviderSQLiteBeads) {
		t.Errorf("the refusal does not name the served provider: %v", err)
	}
}

// TestConvergedCityWithForeignNoteRefusesOnMarkedPath pins the marked arm: a
// standing note naming another binding must hold even when the built-in
// marker and database are intact, or the interim binding's state is orphaned
// with no refusal.
func TestConvergedCityWithForeignNoteRefusesOnMarkedPath(t *testing.T) {
	cityPath, cfg, _, _ := convergedInfraCity(t)
	if err := writeBornSplitServedNote(cityPath, bornSplitServedNote{Binding: "elsewhere", Provider: "outoftree-engine"}); err != nil {
		t.Fatalf("writing the note: %v", err)
	}

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err == nil {
		_ = routes.close()
		t.Fatal("a converged city served through the marked path while the note named another binding")
	}
	if !strings.Contains(err.Error(), `"elsewhere"`) {
		t.Errorf("the refusal does not name the served binding: %v", err)
	}
	if strings.Contains(err.Error(), "point [storage.classes] back at") {
		t.Errorf("a note hold granted the revert instruction: %v", err)
	}
}

// TestCorruptServedNoteHoldsWithoutGrantingAnything pins the unreadable-note
// contract end to end: the hold fires, the note path is named, and neither
// the revert grant nor genesis is reachable.
func TestCorruptServedNoteHoldsWithoutGrantingAnything(t *testing.T) {
	cityPath := t.TempDir()
	_ = stubInfraMigrationSource(t)
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bornSplitServedNotePath(cityPath), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	builtin := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, builtin, "gc start", nil, &stderr)
	if err == nil {
		_ = routes.close()
		t.Fatal("a corrupt served-binding note granted genesis")
	}
	if !strings.Contains(err.Error(), bornSplitServedNotePath(cityPath)) {
		t.Errorf("the refusal does not name the note: %v", err)
	}
	if strings.Contains(err.Error(), "point [storage.classes] back at") {
		t.Errorf("a corrupt note granted the revert instruction: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "store")); !os.IsNotExist(err) {
		t.Errorf("a held boot still created the built-in destination: %v", err)
	}
}

// TestStorageStatusBornSplitKeepsDeployGateContract pins the status exit code
// to what boot decides: an unresolvable provider or a missing engine seam must
// exit non-zero, never report may-serve.
func TestStorageStatusBornSplitKeepsDeployGateContract(t *testing.T) {
	cityPath, cfg, _ := bornSplitCity(t)

	// Unknown provider: same config, stock registry.
	prev := newStorageRegistryForPlan
	newStorageRegistryForPlan = newStorageProviderRegistry
	var stdout, stderr bytes.Buffer
	if code := doStorageStatus(storageOperatorRequest{CityPath: cityPath, Cfg: cfg}, &stdout, &stderr); code == 0 {
		t.Fatalf("status = 0 for a provider this build cannot resolve\nstdout: %s", stdout.String())
	}
	newStorageRegistryForPlan = prev

	// Engine-less provider: registered but without the seam.
	newStorageRegistryForPlan = func() (*storebinding.ProviderRegistry, error) {
		registry := storebinding.NewProviderRegistry()
		if err := registry.Register(engineLessProviderFactory{renamedEngineProviderFactory{
			inner: sqlitebinding.BeadsProviderFactory{},
			id:    "outoftree-engine",
		}}); err != nil {
			return nil, err
		}
		if err := registry.Freeze(); err != nil {
			return nil, err
		}
		return registry, nil
	}
	t.Cleanup(func() { newStorageRegistryForPlan = prev })
	stdout.Reset()
	if code := doStorageStatus(storageOperatorRequest{CityPath: cityPath, Cfg: cfg}, &stdout, &stderr); code == 0 {
		t.Fatalf("status = 0 for a provider without the engine seam\nstdout: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "does not open a bead engine") {
		t.Errorf("status does not name the missing seam: %s", stdout.String())
	}
}

// TestRevertToWorkRefusesWhileNoteStands pins the splitNone arm: pointing
// every class back at work is the exact re-point the note exists to hold, and
// the natural full edit (binding definition removed too) must refuse.
func TestRevertToWorkRefusesWhileNoteStands(t *testing.T) {
	cityPath, cfg, _ := bornSplitCity(t)
	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err != nil {
		t.Fatalf("born-split boot refused: %v", err)
	}
	if err := routes.close(); err != nil {
		t.Fatal(err)
	}

	reverted := &config.City{Storage: &config.StorageConfig{
		Classes: config.StorageClasses{
			Work:      config.StorageWorkBinding,
			Graph:     config.StorageWorkBinding,
			Sessions:  config.StorageWorkBinding,
			Messaging: config.StorageWorkBinding,
			Orders:    config.StorageWorkBinding,
			Nudges:    config.StorageWorkBinding,
		},
	}}
	blocked, err := storageBootGate(cityPath, reverted, "gc start", nil, &stderr)
	if err == nil {
		if blocked != nil {
			_ = blocked.close()
		}
		t.Fatal("reverting every class to work served while the note stood")
	}
	if !strings.Contains(err.Error(), `"infra"`) {
		t.Errorf("the refusal does not name the served binding: %v", err)
	}
}

// TestBornSplitSameNamePathRepointRefuses pins the location leg of the note:
// keeping the binding's name and provider while moving its storage is the
// same outbound orphan, reached through a path edit instead of a name edit.
func TestBornSplitSameNamePathRepointRefuses(t *testing.T) {
	cityPath, cfg, _ := bornSplitCity(t)
	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err != nil {
		t.Fatalf("born-split boot refused: %v", err)
	}
	if err := routes.close(); err != nil {
		t.Fatal(err)
	}

	moved := cfg
	binding := moved.Storage.Bindings["infra"]
	binding.Path = filepath.Join(cityPath, ".gc", "moved-store")
	moved.Storage.Bindings["infra"] = binding

	blocked, err := storageBootGate(cityPath, moved, "gc start", nil, &stderr)
	if err == nil {
		_ = blocked.close()
		t.Fatal("moving the binding's storage under the same name served while the note recorded the old location")
	}
	if !strings.Contains(err.Error(), bornSplitServedNotePath(cityPath)) {
		t.Errorf("the refusal does not name the note: %v", err)
	}
}

// TestBuiltinPathEditRefusesUntilAttested pins the same location leg on the
// built-in arm: editing the binding's path with the marker left at the old
// root used to reach genesis at the new root; the note now holds it.
func TestBuiltinPathEditRefusesUntilAttested(t *testing.T) {
	cityPath := t.TempDir()
	_ = stubInfraMigrationSource(t)
	original := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, original, "gc start", nil, &stderr)
	if err != nil {
		t.Fatalf("genesis boot refused: %v", err)
	}
	if err := routes.close(); err != nil {
		t.Fatal(err)
	}

	moved := infraSplitConfig(filepath.Join(cityPath, ".gc", "moved-store"))
	blocked, err := storageBootGate(cityPath, moved, "gc start", nil, &stderr)
	if err == nil {
		_ = blocked.close()
		t.Fatal("a path edit reached genesis at the new root while the note recorded the old one")
	}
	if !strings.Contains(err.Error(), bornSplitServedNotePath(cityPath)) {
		t.Errorf("the refusal does not name the note: %v", err)
	}
}

// TestStorageStatusHonorsNoteHoldOnBothArms pins the diagnosis command's exit
// code to the hold on the sqlite arm and the born-split arm alike.
func TestStorageStatusHonorsNoteHoldOnBothArms(t *testing.T) {
	cityPath, cfg, _, _ := convergedInfraCity(t)
	if err := writeBornSplitServedNote(cityPath, bornSplitServedNote{Binding: "elsewhere", Provider: "outoftree-engine", Location: "ref"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doStorageStatus(storageOperatorRequest{CityPath: cityPath, Cfg: cfg}, &stdout, &stderr); code == 0 {
		t.Fatalf("status = 0 on a converged city whose note names another binding\nstdout: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"elsewhere"`) {
		t.Errorf("status does not name the served binding: %s", stdout.String())
	}

	bornPath, bornCfg, _ := bornSplitCity(t)
	if err := writeBornSplitServedNote(bornPath, bornSplitServedNote{Binding: "old-binding", Provider: "outoftree-engine", Location: "old-ref"}); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := doStorageStatus(storageOperatorRequest{CityPath: bornPath, Cfg: bornCfg}, &stdout, &stderr); code == 0 {
		t.Fatalf("status = 0 on a born-split city whose note names another binding\nstdout: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"old-binding"`) {
		t.Errorf("status does not name the served binding: %s", stdout.String())
	}
}

// TestStoragePlanRefusesACityWithNoPath closes the back door into the defect
// the city root exists to remove.
//
// filepath.Abs("") is the working directory. A caller that reached plan
// resolution without a city — and there is no such production caller today,
// which is exactly why nothing would notice — would have stamped that
// directory into every binding specification, and every provider would have
// resolved against it as if it were the city.
func TestStoragePlanRefusesACityWithNoPath(t *testing.T) {
	cfg := infraSplitConfig(config.DefaultSQLiteStoragePath)

	plan, err := resolveCityStoragePlan("", cfg)
	if err == nil {
		t.Fatalf("a plan resolved for no city at all: %+v", plan)
	}
	if !strings.Contains(err.Error(), "no city path") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}

	var stderr bytes.Buffer
	routes, gateErr := storageBootGate("", cfg, "gc start", nil, &stderr)
	if gateErr == nil {
		_ = routes.close()
		t.Fatal("the boot gate served a city it has no path for")
	}
}

// TestHalfRevertRefusalCarriesTheNoteThatHoldsIt fixes an operator dead end.
//
// Pointing the classes back at work while leaving the binding defined is the
// obvious half of a revert, and plan resolution rejects it — correctly, and
// for a reason that says nothing about the hold that actually matters. An
// operator who reads only "unreferenced storage binding" deletes the binding,
// gets past that error, and meets the note refusal one edit later. Both
// refusals are true, so the boot carries both.
func TestHalfRevertRefusalCarriesTheNoteThatHoldsIt(t *testing.T) {
	cityPath := t.TempDir()
	if err := writeBornSplitServedNote(cityPath, bornSplitServedNote{
		Binding:  "infra",
		Provider: "outoftree-engine",
		Location: filepath.Join(cityPath, ".gc", "storage", "infra"),
	}); err != nil {
		t.Fatalf("recording the served binding: %v", err)
	}
	// Classes reverted to work, binding left behind: what an operator's first
	// revert edit actually looks like.
	cfg := &config.City{Storage: &config.StorageConfig{
		Classes: config.StorageClasses{
			Work: config.StorageWorkBinding, Graph: config.StorageWorkBinding,
			Sessions: config.StorageWorkBinding, Messaging: config.StorageWorkBinding,
			Orders: config.StorageWorkBinding, Nudges: config.StorageWorkBinding,
		},
		Bindings: map[string]config.StorageBindingConfig{
			"infra": {Provider: "outoftree-engine", ConfigRef: "infra"},
		},
	}}

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err == nil {
		_ = routes.close()
		t.Fatal("a half-reverted city with a standing served-binding note booted")
	}
	if !strings.Contains(err.Error(), "unreferenced") {
		t.Errorf("the refusal drops the configuration error the operator will hit first: %v", err)
	}
	if !strings.Contains(err.Error(), bornSplitServedNotePath(cityPath)) {
		t.Errorf("the refusal does not name the note that holds the revert: %v", err)
	}
}

// closeCountingProviderFactory wraps a compiled engine provider and counts every
// close of the handles it hands out, so a caller's cleanup is observable from
// outside the provider.
type closeCountingProviderFactory struct {
	inner  storebinding.ProviderFactory
	closes *int
}

func (f closeCountingProviderFactory) ID() storebinding.ProviderID { return f.inner.ID() }

func (f closeCountingProviderFactory) New(spec storebinding.BindingSpec) (storebinding.Provider, error) {
	provider, err := f.inner.New(spec)
	if err != nil {
		return nil, err
	}
	return closeCountingProvider{Provider: provider, closes: f.closes}, nil
}

type closeCountingProvider struct {
	storebinding.Provider
	closes *int
}

func (p closeCountingProvider) OpenEngine(spec storebinding.BindingSpec, classes storebinding.ClassSet) (beads.Store, io.Closer, error) {
	opener, ok := p.Provider.(storebinding.EngineOpener)
	if !ok {
		return nil, nil, errors.New("wrapped provider opens no engine")
	}
	store, closer, err := opener.OpenEngine(spec, classes)
	if err != nil {
		return nil, nil, err
	}
	return store, countingCloser{inner: closer, closes: p.closes}, nil
}

type countingCloser struct {
	inner  io.Closer
	closes *int
}

func (c countingCloser) Close() error {
	*c.closes++
	return c.inner.Close()
}

// TestFailedNoteWriteReleasesTheBindingItJustOpened covers the ordering the
// post-open note write created and nothing else reaches: the engine is open,
// the durable record fails, and the boot returns an error. A process that
// refuses to start while still holding the binding's engine is a boot the
// operator cannot retry.
func TestFailedNoteWriteReleasesTheBindingItJustOpened(t *testing.T) {
	cityPath := t.TempDir()
	stubInfraMigrationSource(t)
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	binding := cfg.Storage.Bindings["infra"]
	binding.Provider = "outoftree-engine"
	cfg.Storage.Bindings["infra"] = binding

	closes := 0
	prevRegistry := newStorageRegistryForPlan
	newStorageRegistryForPlan = func() (*storebinding.ProviderRegistry, error) {
		registry := storebinding.NewProviderRegistry()
		if err := registry.Register(closeCountingProviderFactory{
			inner:  renamedEngineProviderFactory{inner: sqlitebinding.BeadsProviderFactory{}, id: "outoftree-engine"},
			closes: &closes,
		}); err != nil {
			return nil, err
		}
		if err := registry.Freeze(); err != nil {
			return nil, err
		}
		return registry, nil
	}
	t.Cleanup(func() { newStorageRegistryForPlan = prevRegistry })

	prevWrite := writeServedBindingNote
	writeServedBindingNote = func(string, bornSplitServedNote) error {
		return errors.New("injected served-binding note failure")
	}
	t.Cleanup(func() { writeServedBindingNote = prevWrite })

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &stderr)
	if err == nil {
		_ = routes.close()
		t.Fatal("a boot whose served-binding note could not be written reported success")
	}
	if routes != nil {
		t.Errorf("a failed boot handed back routes %+v the caller would have to close", routes)
	}
	if !strings.Contains(err.Error(), "injected served-binding note failure") {
		t.Errorf("the refusal does not carry the write failure: %v", err)
	}
	if closes != 1 {
		t.Errorf("the opened binding was closed %d time(s), want exactly 1: a boot that refuses must not keep the engine it opened", closes)
	}
}

// TestStorageWorkPinsResolveHQPrefixTheSameWayTheCityMintsIt covers the HQ pin
// for a city that never declares a workspace prefix, which is the ordinary
// case: [workspace] prefix is not in the default city.toml.
//
// HQ then mints under the prefix derived from the city name, and the pin has
// to state that same value. Pinning a fixed literal instead makes the pin
// disagree with reality for every such city, and the disagreement is only
// visible when some rig genuinely holds the literal: two pinned work scopes
// land on one prefix and the resolver refuses the pair, so a city that boots
// today cannot boot once it configures [storage].
//
// gc is the natural prefix for a rig tracking gascity, so this is not exotic.
// The rig arm of the same function already resolves through
// Rig.EffectivePrefix; only HQ read a raw field and a literal.
func TestStorageWorkPinsResolveHQPrefixTheSameWayTheCityMintsIt(t *testing.T) {
	root := t.TempDir()
	cfg := &config.City{
		// Neither ResolvedWorkspacePrefix nor Workspace.Prefix is set, so the
		// prefix comes from the city name, exactly as EffectiveHQPrefix's
		// third step resolves it.
		ResolvedWorkspaceName: "ds-research",
		Rigs: []config.Rig{
			{Name: "gascity", Prefix: "gc", Path: filepath.Join(root, "gascity")},
		},
	}

	want := config.EffectiveHQPrefix(cfg)
	if want == "" {
		t.Fatal("the city derives no HQ prefix, so this case cannot state what the pin should carry")
	}

	pins := cityStorageWorkPins(root, cfg)
	if pins.HQ.Prefix != want {
		t.Errorf("HQ pinned prefix %q, want %q: the pin disagrees with the prefix HQ mints under",
			pins.HQ.Prefix, want)
	}

	// The consequence the operator meets. A pin that repeats the rig's prefix
	// collides with it, and the resolver refuses any pair where one prefix
	// selects the other.
	registry := storebinding.NewProviderRegistry()
	if err := registry.Freeze(); err != nil {
		t.Fatalf("freezing an empty registry: %v", err)
	}
	if _, err := storebinding.ResolveStoragePlan(registry, cfg.EffectiveStorage(), pins, root); err != nil {
		t.Fatalf("a city with a rig at prefix %q cannot resolve its storage plan: %v", cfg.Rigs[0].Prefix, err)
	}
}

// TestStorageWorkPinsUseADeclaredWorkspacePrefix covers the middle step of the
// resolution, where a city declares [workspace] prefix but nothing has
// populated the resolved field. That path reached the pin only after this
// function stopped reading the resolved field alone; before, such a city was
// pinned at the fallback while HQ minted under its declared prefix.
//
// The pin carries storageWorkPrefix's normalisation of the declared value, not
// the raw value. That normalisation is older than this resolution and applies
// equally to the resolved field, so a declared prefix that is not already
// lower case and dash-free still pins a value the backend does not mint under.
// Canonicalising prefixes once for both minting and pinning is issue #5204.
func TestStorageWorkPinsUseADeclaredWorkspacePrefix(t *testing.T) {
	root := t.TempDir()
	cfg := &config.City{
		ResolvedWorkspaceName: "ds-research",
		Workspace:             config.Workspace{Prefix: "hq"},
		Rigs: []config.Rig{
			{Name: "gascity", Prefix: "gc", Path: filepath.Join(root, "gascity")},
		},
	}

	pins := cityStorageWorkPins(root, cfg)
	if pins.HQ.Prefix != "hq" {
		t.Errorf("HQ pinned prefix %q, want %q: a declared workspace prefix does not reach the pin",
			pins.HQ.Prefix, "hq")
	}
	// The declared value wins over the name-derived one, which is the ordering
	// EffectiveHQPrefix states.
	if derived := config.DeriveBeadsPrefix("ds-research"); pins.HQ.Prefix == derived {
		t.Errorf("HQ pinned the name-derived prefix %q over the declared one", derived)
	}
}

// TestStorageWorkConfigContextDistinguishesDerivedHQPrefixes covers the digest
// that states which configuration a plan was resolved from.
//
// Two cities can leave the workspace prefix unset and still pin different HQ
// prefixes, because the prefix is then derived from the city name. A digest
// taken over the unset field alone reads those two as the same configuration
// while their pins disagree, which is the one thing the digest exists to
// prevent.
func TestStorageWorkConfigContextDistinguishesDerivedHQPrefixes(t *testing.T) {
	root := t.TempDir()
	research := &config.City{ResolvedWorkspaceName: "ds-research"}
	factory := &config.City{ResolvedWorkspaceName: "gas-town"}

	if config.EffectiveHQPrefix(research) == config.EffectiveHQPrefix(factory) {
		t.Fatalf("both cities derive prefix %q, so this case cannot tell the digests apart",
			config.EffectiveHQPrefix(research))
	}
	if cityStorageWorkPins(root, research).HQ.Prefix == cityStorageWorkPins(root, factory).HQ.Prefix {
		t.Fatal("the two cities pin the same HQ prefix, so the digest has nothing to distinguish")
	}

	if storageWorkConfigContext(root, research) == storageWorkConfigContext(root, factory) {
		t.Error("two cities with different HQ pins share one config digest, so a plan cannot name the configuration it came from")
	}
}
