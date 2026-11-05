package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func writeMemoTestCity(t *testing.T) string {
	t.Helper()
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\nprefix = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityDir
}

// TestInvocationConfigMemoIsOffOutsideAScope pins that a process which never
// opens a scope (the supervisor, the API server) reloads on every call, as it
// always has: an edit to city.toml is seen by the next load.
func TestInvocationConfigMemoIsOffOutsideAScope(t *testing.T) {
	cityDir := writeMemoTestCity(t)
	tomlPath := filepath.Join(cityDir, "city.toml")
	before := composedCityConfigLoads.Load()
	if _, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"renamed\"\nprefix = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions)
	if err != nil {
		t.Fatal(err)
	}
	if got := composedCityConfigLoads.Load() - before; got != 2 {
		t.Fatalf("loads outside a scope = %d, want 2", got)
	}
	if cfg.Workspace.Name != "renamed" {
		t.Fatalf("second load outside a scope read %q, want the edited %q", cfg.Workspace.Name, "renamed")
	}
}

// TestInvocationConfigMemoSharesOneLoadInAScope pins the scope: one composed
// load per city, other option sets untouched, errors never remembered, and the
// scope closed by its end function.
func TestInvocationConfigMemoSharesOneLoadInAScope(t *testing.T) {
	cityDir := writeMemoTestCity(t)
	tomlPath := filepath.Join(cityDir, "city.toml")
	missing := filepath.Join(t.TempDir(), "city.toml")

	end := beginInvocationConfigMemo()
	// The scope is process-global: a t.Fatal before the explicit end() below
	// must not leave it open for the rest of the package's tests. end is
	// idempotent, so the explicit call stays the one under test.
	t.Cleanup(end)
	innerEnd := beginInvocationConfigMemo() // nested begin reuses the open scope
	innerEnd()
	if currentInvocationConfigMemo() == nil {
		t.Fatal("a nested scope's end closed the outer scope")
	}

	before := composedCityConfigLoads.Load()
	first, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions)
	if err != nil {
		t.Fatal(err)
	}
	if first.Workspace.Name != second.Workspace.Name {
		t.Fatal("a second load in the scope did not answer the first load's config")
	}
	if got := composedCityConfigLoads.Load() - before; got != 1 {
		t.Fatalf("loads in a scope = %d, want 1", got)
	}

	// Another option set is not the one-shot shape: it loads.
	before = composedCityConfigLoads.Load()
	if _, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, advisoryLoad); err != nil {
		t.Fatal(err)
	}
	if got := composedCityConfigLoads.Load() - before; got != 1 {
		t.Fatalf("advisory loads in a scope = %d, want 1 (not memoized)", got)
	}

	// A failed load is not remembered.
	before = composedCityConfigLoads.Load()
	for i := 0; i < 2; i++ {
		if _, _, err := loadComposedCityConfig(fsys.OSFS{}, missing, invocationMemoLoadOptions); err == nil {
			t.Fatal("loading a missing city.toml succeeded")
		}
	}
	if got := composedCityConfigLoads.Load() - before; got != 2 {
		t.Fatalf("failed loads in a scope = %d, want 2 (errors are not memoized)", got)
	}

	end()
	if currentInvocationConfigMemo() != nil {
		t.Fatal("end did not close the scope")
	}
}

// TestInvocationConfigMemoHandsEachCallerItsOwnRigs: helpers resolve rig paths
// in place (resolveRigPaths) on the config they load. A memo hit must not hand
// the next helper a config another helper already rewrote: each caller gets
// its own top-level City and its own Rigs and Agents slices.
func TestInvocationConfigMemoHandsEachCallerItsOwnRigs(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\nprefix = \"demo\"\n\n[[rigs]]\nname = \"r\"\npath = \"rigs/r\"\nprefix = \"rr\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(cityDir, "city.toml")
	defer beginInvocationConfigMemo()()

	first, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Rigs) != 1 || first.Rigs[0].Path != "rigs/r" {
		t.Fatalf("rigs = %+v, want one rig at rigs/r", first.Rigs)
	}
	resolveRigPaths(cityDir, first.Rigs)
	first.Workspace.Name = "mutated"

	second, _, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, invocationMemoLoadOptions)
	if err != nil {
		t.Fatal(err)
	}
	if second.Rigs[0].Path != "rigs/r" {
		t.Fatalf("second caller saw rig path %q, want the loaded rigs/r (another caller's in-place resolve leaked)", second.Rigs[0].Path)
	}
	if second.Workspace.Name != "demo" {
		t.Fatalf("second caller saw workspace %q, want demo", second.Workspace.Name)
	}
}
