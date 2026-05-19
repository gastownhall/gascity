package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestResolveFormulaScope_RigFlagWins verifies that an explicit --rig flag
// takes priority over the cwd, and that the rig's FormulaLayers are used.
func TestResolveFormulaScope_RigFlagWins(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "my-project")
	otherPath := filepath.Join(cityPath, "other-rig")
	for _, p := range []string{rigPath, otherPath} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}

	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "my-project", Path: rigPath},
			{Name: "other-rig", Path: otherPath},
		},
		FormulaLayers: config.FormulaLayers{
			City: []string{"/city/formulas"},
			Rigs: map[string][]string{
				"my-project": {"/city/formulas", "/rigs/my-project/formulas"},
				"other-rig":  {"/city/formulas", "/rigs/other-rig/formulas"},
			},
		},
	}

	t.Chdir(otherPath) // cwd would otherwise resolve to other-rig
	prev := rigFlag
	t.Cleanup(func() { rigFlag = prev })
	rigFlag = "my-project"

	scope, err := resolveFormulaScope(cfg, cityPath)
	if err != nil {
		t.Fatalf("resolveFormulaScope: %v", err)
	}
	if scope.storeRoot != rigPath {
		t.Errorf("storeRoot = %q, want %q", scope.storeRoot, rigPath)
	}
	want := []string{"/city/formulas", "/rigs/my-project/formulas"}
	if !reflect.DeepEqual(scope.searchPaths, want) {
		t.Errorf("searchPaths = %v, want %v", scope.searchPaths, want)
	}
}

// TestResolveFormulaScope_CwdInsideRig falls back to cwd when --rig is unset.
// Asserts searchPaths too — the core bug in #1004 was search paths dropping
// back to city layers even when storeRoot was rig-correct.
func TestResolveFormulaScope_CwdInsideRig(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}

	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "my-project", Path: rigPath},
		},
		FormulaLayers: config.FormulaLayers{
			City: []string{"/city/formulas"},
			Rigs: map[string][]string{
				"my-project": {"/city/formulas", "/rigs/my-project/formulas"},
			},
		},
	}

	t.Chdir(rigPath)
	prev := rigFlag
	t.Cleanup(func() { rigFlag = prev })
	rigFlag = ""

	scope, err := resolveFormulaScope(cfg, cityPath)
	if err != nil {
		t.Fatalf("resolveFormulaScope: %v", err)
	}
	if scope.storeRoot != rigPath {
		t.Errorf("storeRoot = %q, want %q", scope.storeRoot, rigPath)
	}
	want := []string{"/city/formulas", "/rigs/my-project/formulas"}
	if !reflect.DeepEqual(scope.searchPaths, want) {
		t.Errorf("searchPaths = %v, want %v", scope.searchPaths, want)
	}
}

// TestResolveFormulaScope_CityScopeWhenNoRig returns city defaults when the
// cwd is inside the city root but outside any declared rig and --rig is unset.
func TestResolveFormulaScope_CityScopeWhenNoRig(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Rigs: []config.Rig{},
		FormulaLayers: config.FormulaLayers{
			City: []string{"/city/formulas"},
		},
	}

	t.Chdir(cityPath)
	prev := rigFlag
	t.Cleanup(func() { rigFlag = prev })
	rigFlag = ""

	scope, err := resolveFormulaScope(cfg, cityPath)
	if err != nil {
		t.Fatalf("resolveFormulaScope: %v", err)
	}
	if scope.storeRoot != cityPath {
		t.Errorf("storeRoot = %q, want %q", scope.storeRoot, cityPath)
	}
	want := []string{"/city/formulas"}
	if !reflect.DeepEqual(scope.searchPaths, want) {
		t.Errorf("searchPaths = %v, want %v", scope.searchPaths, want)
	}
}

// TestResolveFormulaScope_UnknownRigErrors surfaces a clear error when the
// user passes a --rig name that doesn't exist.
func TestResolveFormulaScope_UnknownRigErrors(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "real", Path: filepath.Join(cityPath, "real")}},
	}

	prev := rigFlag
	t.Cleanup(func() { rigFlag = prev })
	rigFlag = "ghost"

	_, err := resolveFormulaScope(cfg, cityPath)
	if err == nil {
		t.Fatal("expected error for unknown rig, got nil")
	}
	if !strings.Contains(err.Error(), `rig "ghost" not found`) {
		t.Errorf("error = %v, want substring 'rig \"ghost\" not found'", err)
	}
}

// TestResolveFormulaScope_UnboundRigErrors rejects a declared rig that has
// no path binding — matching the gc bd error semantics.
func TestResolveFormulaScope_UnboundRigErrors(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "unbound", Path: ""}},
	}

	prev := rigFlag
	t.Cleanup(func() { rigFlag = prev })
	rigFlag = "unbound"

	_, err := resolveFormulaScope(cfg, cityPath)
	if err == nil {
		t.Fatal("expected error for unbound rig, got nil")
	}
	if !strings.Contains(err.Error(), "no path binding") {
		t.Errorf("error = %v, want substring 'no path binding'", err)
	}
}

// TestResolveFormulaScope_RigFallsBackToCityLayers covers the case where a
// rig is resolved but has no rig-specific FormulaLayers entry; SearchPaths
// should fall back to city layers.
func TestResolveFormulaScope_RigFallsBackToCityLayers(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "bare-rig")

	cfg := &config.City{
		Rigs: []config.Rig{{Name: "bare-rig", Path: rigPath}},
		FormulaLayers: config.FormulaLayers{
			City: []string{"/city/formulas"},
		},
	}

	prev := rigFlag
	t.Cleanup(func() { rigFlag = prev })
	rigFlag = "bare-rig"

	scope, err := resolveFormulaScope(cfg, cityPath)
	if err != nil {
		t.Fatalf("resolveFormulaScope: %v", err)
	}
	if scope.storeRoot != rigPath {
		t.Errorf("storeRoot = %q, want %q", scope.storeRoot, rigPath)
	}
	want := []string{"/city/formulas"}
	if !reflect.DeepEqual(scope.searchPaths, want) {
		t.Errorf("searchPaths = %v, want %v (city fallback)", scope.searchPaths, want)
	}
}
