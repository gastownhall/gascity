package main

// The boot gate against the compiled beads workspace provider.
//
// These are the arms that need no workspace at all: a city whose work store
// still holds infrastructure beads never reaches the binding, and a city whose
// configured workspace is not there is refused by the open. The serving arm
// needs a real workspace and lives beside these under the integration tag.
//
// None of them stands in the city. The working directory is deliberately left
// wherever the test binary started, because a gate that resolved a binding
// against the process rather than against the city would pass every test that
// chdir'd into the city first — and would send every city a supervisor hosts
// to the same wrong directory in production.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storebinding/beadsworkspace"
)

// workspaceSplitConfig is infraSplitConfig's spelling for a city whose
// infrastructure classes are served from a beads workspace: the same whole
// split, named by configuration reference instead of by path.
func workspaceSplitConfig(ref string) *config.City {
	cfg := infraSplitConfig("")
	cfg.Storage.Bindings["infra"] = config.StorageBindingConfig{
		Provider:  string(beadsworkspace.ProviderID),
		ConfigRef: ref,
	}
	return cfg
}

// TestWorkspaceBindingConfigIsValidCityConfiguration proves the provider is
// reachable from a city.toml at all: config validation admits a binding that
// names it with a configuration reference, and refuses the path spelling that
// belongs to the built-in engine.
func TestWorkspaceBindingConfigIsValidCityConfiguration(t *testing.T) {
	if err := config.ValidateStorageConfig(workspaceSplitConfig("infra")); err != nil {
		t.Fatalf("a city served from a beads workspace was refused: %v", err)
	}

	withPath := workspaceSplitConfig("infra")
	withPath.Storage.Bindings["infra"] = config.StorageBindingConfig{
		Provider: string(beadsworkspace.ProviderID),
		Path:     ".gc/store",
	}
	err := config.ValidateStorageConfig(withPath)
	if err == nil {
		t.Fatal("a workspace binding spelled with a path was accepted")
	}
	if !strings.Contains(err.Error(), "path is only supported by provider") {
		t.Errorf("the refusal does not say which providers accept a path: %v", err)
	}

	withoutRef := workspaceSplitConfig("")
	if err := config.ValidateStorageConfig(withoutRef); err == nil {
		t.Fatal("a workspace binding naming no workspace was accepted")
	}
}

// TestStorageGateRefusesWorkspaceBindingWhenWorkStoreHoldsInfraBeads is the
// born-split discipline against the real provider rather than a renamed fake:
// a compiled provider this build carries no migration for still may not serve
// a city whose infrastructure beads are sitting in the work store.
func TestStorageGateRefusesWorkspaceBindingWhenWorkStoreHoldsInfraBeads(t *testing.T) {
	cityPath := t.TempDir()
	source := stubInfraMigrationSource(t)
	strayed := mustCreateInfraBead(t, source, beads.Bead{Title: "landed in work", Type: "session", Labels: []string{"gc:session"}})

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, workspaceSplitConfig("infra"), "gc start", nil, &stderr)
	if err == nil {
		_ = routes.close()
		t.Fatal("a workspace-backed city with an infrastructure bead in the work store served")
	}
	if !strings.Contains(err.Error(), strayed.ID) {
		t.Errorf("the refusal does not name the bead %s: %v", strayed.ID, err)
	}
	if !strings.Contains(err.Error(), `binding "infra"`) {
		t.Errorf("the refusal does not name the binding: %v", err)
	}
}

// TestStorageGateRefusesAWorkspaceThatIsNotThere pins the other end: the city
// is clean, the plan resolves, the provider goes to open the workspace — and
// says the directory it was pointed at is not one, naming the path rather than
// creating anything there.
//
// It also pins the note ordering. A boot that refuses records no served
// binding, because the note is history and a city that never served has none:
// writing it first is what turned a mistyped configuration reference into a
// durable claim that had to be attested away before the typo could be fixed.
func TestStorageGateRefusesAWorkspaceThatIsNotThere(t *testing.T) {
	cityPath := t.TempDir()
	stubInfraMigrationSource(t)
	root, err := beadsworkspace.WorkspaceRoot(cityPath, "infra")
	if err != nil {
		t.Fatalf("resolving the workspace root: %v", err)
	}

	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, workspaceSplitConfig("infra"), "gc start", nil, &stderr)
	if err == nil {
		_ = routes.close()
		t.Fatal("a city whose workspace does not exist served")
	}
	if !errors.Is(err, beadsworkspace.ErrWorkspaceUnavailable) {
		t.Fatalf("the gate refused with %v, want %v", err, beadsworkspace.ErrWorkspaceUnavailable)
	}
	if !strings.Contains(err.Error(), root) {
		t.Errorf("the refusal does not name the workspace directory %s: %v", root, err)
	}
	if _, present, noteErr := readBornSplitServedNote(cityPath); noteErr != nil {
		t.Fatalf("reading the served-binding note: %v", noteErr)
	} else if present {
		t.Errorf("a boot that refused to serve recorded a served binding at %s", bornSplitServedNotePath(cityPath))
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat of %s after a refused boot = %v, want the directory never to have been created", root, err)
	}
}

// TestStorageGateResolvesTheWorkspaceFromTheCityNotTheProcess is the defect the
// city root exists to close, in the shape it actually ships in: the supervisor
// hosts every registered city from one process whose working directory belongs
// to none of them.
//
// It asserts the divergence rather than one city's success, because a gate
// that resolved against the process would put both cities in the same place
// and a single-city assertion would not notice.
func TestStorageGateResolvesTheWorkspaceFromTheCityNotTheProcess(t *testing.T) {
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)

	first := t.TempDir()
	second := t.TempDir()
	firstRoot, err := beadsworkspace.WorkspaceRoot(first, "infra")
	if err != nil {
		t.Fatalf("resolving the first city's workspace: %v", err)
	}
	secondRoot, err := beadsworkspace.WorkspaceRoot(second, "infra")
	if err != nil {
		t.Fatalf("resolving the second city's workspace: %v", err)
	}
	if firstRoot == secondRoot {
		t.Fatalf("two cities sharing one configuration reference resolved the same workspace %s", firstRoot)
	}

	stubInfraMigrationSource(t)
	for _, city := range []struct {
		path string
		root string
	}{{first, firstRoot}, {second, secondRoot}} {
		var stderr bytes.Buffer
		routes, err := storageBootGate(city.path, workspaceSplitConfig("infra"), "gc start", nil, &stderr)
		if err == nil {
			_ = routes.close()
			t.Fatalf("city %s served from a workspace that does not exist", city.path)
		}
		if !strings.Contains(err.Error(), city.root) {
			t.Errorf("city %s was refused about %v, want its own workspace %s", city.path, err, city.root)
		}
	}
	// The process's own directory is where a working-directory base would have
	// sent every one of them.
	if directoryHolds(t, elsewhere, ".gc") {
		t.Errorf("the boot gate resolved a binding into the process working directory %s", elsewhere)
	}
}

// TestServedBindingLocationIsWhereTheProviderOpens pins the note's location
// against the provider's own answer for BOTH compiled providers, and pins the
// built-in one against the migration's city-side resolution.
//
// The two resolutions used to disagree whenever a relative path met a process
// standing outside the city: the migration wrote to the city's database and
// the boot served whatever the working directory pointed at. They are the same
// path now, and this is the assertion that keeps them so — a note recording
// one while the binding opens the other is a hold on every later boot.
func TestServedBindingLocationIsWhereTheProviderOpens(t *testing.T) {
	t.Chdir(t.TempDir())
	cityPath := t.TempDir()

	sqliteCfg := infraSplitConfig(config.DefaultSQLiteStoragePath)
	target, ok, err := resolveInfraBindingTarget(cityPath, sqliteCfg)
	if err != nil || !ok {
		t.Fatalf("resolving the built-in binding target = (%+v, %t, %v)", target, ok, err)
	}
	plan, err := resolveCityStoragePlan(cityPath, sqliteCfg)
	if err != nil {
		t.Fatalf("resolving the built-in city's plan: %v", err)
	}
	location, err := servedBindingLocation(plan, "infra", sqliteCfg.Storage.Bindings["infra"])
	if err != nil {
		t.Fatalf("resolving where the built-in binding serves from: %v", err)
	}
	if location != target.Database {
		t.Errorf("the built-in provider serves %s while the migration resolves %s; a relative path must mean the same database on both sides", location, target.Database)
	}
	if !strings.HasPrefix(location, cityPath) {
		t.Errorf("the built-in binding resolved %s, which is not under the city %s", location, cityPath)
	}

	workspaceCfg := workspaceSplitConfig("infra")
	workspacePlan, err := resolveCityStoragePlan(cityPath, workspaceCfg)
	if err != nil {
		t.Fatalf("resolving the workspace city's plan: %v", err)
	}
	workspaceLocation, err := servedBindingLocation(workspacePlan, "infra", workspaceCfg.Storage.Bindings["infra"])
	if err != nil {
		t.Fatalf("resolving where the workspace binding serves from: %v", err)
	}
	want, err := beadsworkspace.WorkspaceRoot(cityPath, "infra")
	if err != nil {
		t.Fatalf("resolving the workspace root: %v", err)
	}
	if workspaceLocation != want {
		t.Errorf("the workspace binding records %s, want the directory it opens %s", workspaceLocation, want)
	}
	if workspaceLocation == "infra" {
		t.Error("the note records the configuration reference; two cities carrying the same reference would be indistinguishable")
	}
	if directoryHolds(t, filepath.Dir(cityPath), filepath.Base(cityPath)) && directoryHolds(t, cityPath, ".gc") {
		t.Errorf("resolving where a binding serves from created %s", filepath.Join(cityPath, ".gc"))
	}
}
