package main

// The boot gate against the compiled beads workspace provider.
//
// These are the arms that need no workspace at all: a city whose work store
// still holds infrastructure beads never reaches the binding, and a city whose
// configured workspace is not there is refused by the open. The serving arm
// needs a real workspace and lives beside these under the integration tag.

import (
	"bytes"
	"errors"
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
	t.Chdir(cityPath)
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
// is clean, the plan resolves, the provider opens the engine — and says the
// workspace it was pointed at does not exist, naming the directory rather than
// creating one nobody configured.
func TestStorageGateRefusesAWorkspaceThatIsNotThere(t *testing.T) {
	cityPath := t.TempDir()
	t.Chdir(cityPath)
	stubInfraMigrationSource(t)
	root, err := beadsworkspace.WorkspaceRoot("infra")
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
	// The city's own .gc is there — the gate records the served binding before
	// it opens anything — so the evidence is that the storage tree inside it
	// was never created.
	if directoryHolds(t, filepath.Join(cityPath, ".gc"), "storage") {
		t.Errorf("a refused boot created the workspace directory tree under %s", cityPath)
	}
}
