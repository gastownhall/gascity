package rig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// gas-4cu: a failed rig add must not leave a half-built .beads/ behind. The
// store init is the first step that writes into the rig directory, and when it
// fails partway the directory it created looks initialized to the next attempt
// — which then refuses with "already contains a beads store" and sends the
// operator hand-editing metadata.json.

// provisionWithFailingStoreInit wires a fresh add whose InitStore writes a
// partial store into the rig directory and then fails, the way a real init
// does when it writes metadata.json before the backing server rejects it.
func provisionWithFailingStoreInit(t *testing.T, initStore func(cityPath, dir, prefix string) (bool, error)) (Deps, ProvisionRequest) {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(originalCityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := stubDeps(cityPath)
	deps.NormalizeScopes = func(string, *config.City) error { return nil }
	deps.InitStore = initStore
	return deps, ProvisionRequest{Name: "storerig", Path: t.TempDir()}
}

// writePartialStore simulates the debris a failed init leaves: a .beads/ dir
// holding metadata.json, which is exactly what the fresh-add guard treats as
// an existing store on the retry.
func writePartialStore(t *testing.T, dir string) {
	t.Helper()
	beads := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beads, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beads, "metadata.json"), []byte(`{"backend":"dolt","dolt_database":"storerig"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProvisionRemovesPartialBeadsStoreWhenInitFails(t *testing.T) {
	initErr := errors.New("managed Dolt server unreachable while inspecting existing store")
	var rigPath string
	deps, req := provisionWithFailingStoreInit(t, func(_, dir, _ string) (bool, error) {
		writePartialStore(t, dir)
		return false, initErr
	})
	rigPath = req.Path

	_, _, provErr := Provision(deps, req)
	if provErr == nil {
		t.Fatal("expected the store-init failure to surface")
	}
	if !errors.Is(provErr, initErr) {
		t.Fatalf("error %v does not wrap the InitStore failure", provErr)
	}

	beadsPath := filepath.Join(rigPath, ".beads")
	if _, err := os.Stat(beadsPath); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(beadsPath)
		t.Fatalf("%s survived a failed rig add (stat err = %v, entries = %v); a retry will refuse with \"already contains a beads store\"", beadsPath, err, entries)
	}
	// The rig directory itself is the user's repo — never ours to remove.
	if _, err := os.Stat(rigPath); err != nil {
		t.Fatalf("rig directory must survive the rollback: %v", err)
	}
}

func TestProvisionKeepsPreExistingBeadsStoreWhenInitFails(t *testing.T) {
	initErr := errors.New("boom")
	deps, req := provisionWithFailingStoreInit(t, func(_, _, _ string) (bool, error) {
		return false, initErr
	})
	// A store that was already on disk before the add is not ours to delete,
	// even when the add fails.
	writePartialStore(t, req.Path)
	marker := filepath.Join(req.Path, ".beads", "metadata.json")

	if _, _, provErr := Provision(deps, req); provErr == nil {
		t.Fatal("expected the store-init failure to surface")
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("pre-existing bead store was destroyed by the rollback: %v", err)
	}
}

func TestProvisionKeepsAdoptedBeadsStoreWhenInitFails(t *testing.T) {
	initErr := errors.New("boom")
	deps, req := provisionWithFailingStoreInit(t, func(_, _, _ string) (bool, error) {
		return false, initErr
	})
	req.Adopt = true
	writePartialStore(t, req.Path)
	if err := os.WriteFile(filepath.Join(req.Path, ".beads", "config.yaml"), []byte("issue_prefix: storerig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(req.Path, ".beads", "metadata.json")

	if _, _, provErr := Provision(deps, req); provErr == nil {
		t.Fatal("expected the store-init failure to surface")
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("--adopt store was destroyed by the rollback: %v", err)
	}
}
