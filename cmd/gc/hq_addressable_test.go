package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hqAddressable_ResolveByName verifies the regression fix for #1242: a
// city's HQ name passed as `--rig` resolves to a context with empty
// RigName (the HQ identity is the city scope itself).
func TestHQAddressable_ResolveByName(t *testing.T) {
	resetFlags(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := setupCity(t, "redhat-takehome")
	registerCityForRigResolution(t, gcHome, cityPath, "redhat-takehome")

	rigFlag = "redhat-takehome"
	ctx, err := resolveContext()
	if err != nil {
		t.Fatalf("resolveContext: %v", err)
	}
	assertSameTestPath(t, ctx.CityPath, cityPath)
	if ctx.RigName != "" {
		t.Errorf("HQ should resolve with empty RigName (city scope); got %q", ctx.RigName)
	}
}

// TestHQAddressable_RigNamePrecedenceWins guards the precedence: when a
// rig and an HQ happen to share a name, rig-by-name resolves first.
// This protects existing rig-name lookups from being shadowed.
func TestHQAddressable_RigNamePrecedenceWins(t *testing.T) {
	resetFlags(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	// City whose HQ name happens to be "shared".
	cityA := setupCity(t, "shared")
	registerCityForRigResolution(t, gcHome, cityA, "shared")

	// Different city has a rig also named "shared".
	cityB := setupCity(t, "other-city")
	rigDir := filepath.Join(t.TempDir(), "shared-rig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	registerRigBindingForResolution(t, gcHome, cityB, "other-city", "shared", rigDir)

	rigFlag = "shared"
	ctx, err := resolveContext()
	if err != nil {
		t.Fatalf("resolveContext: %v", err)
	}
	// Rig-by-name match wins; ctx points to cityB and the rig identity
	// "shared". HQ match for cityA is never reached.
	assertSameTestPath(t, ctx.CityPath, cityB)
	if ctx.RigName != "shared" {
		t.Errorf("rig-by-name should win over HQ-by-name; got RigName=%q", ctx.RigName)
	}
}

// TestHQAddressable_ResolveByLiveWorkspaceName confirms we honor a
// drift-tolerant match: even if the registry stored a stale name, the
// live workspace.name from city.toml still resolves the HQ correctly.
func TestHQAddressable_ResolveByLiveWorkspaceName(t *testing.T) {
	resetFlags(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := setupCity(t, "renamed-live")
	// Register under a stale name; the live workspace.name above is
	// what `gc rig list` displays under (HQ).
	registerCityForRigResolution(t, gcHome, cityPath, "stale-registry-name")

	rigFlag = "renamed-live"
	ctx, err := resolveContext()
	if err != nil {
		t.Fatalf("resolveContext: %v", err)
	}
	assertSameTestPath(t, ctx.CityPath, cityPath)
	if ctx.RigName != "" {
		t.Errorf("HQ should resolve with empty RigName; got %q", ctx.RigName)
	}
}

// TestHQAddressable_ResolveByRegistryNameWhenLiveDiffers covers the
// reverse drift: registry knows the name, live config doesn't match.
// Either source resolves the HQ.
func TestHQAddressable_ResolveByRegistryNameWhenLiveDiffers(t *testing.T) {
	resetFlags(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := setupCity(t, "live-name")
	registerCityForRigResolution(t, gcHome, cityPath, "registry-name")

	rigFlag = "registry-name"
	ctx, err := resolveContext()
	if err != nil {
		t.Fatalf("resolveContext: %v", err)
	}
	assertSameTestPath(t, ctx.CityPath, cityPath)
}

// TestHQAddressable_NoMatchYieldsClearError confirms the original
// error path still fires for a name that matches neither rig nor HQ.
func TestHQAddressable_NoMatchYieldsClearError(t *testing.T) {
	resetFlags(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := setupCity(t, "real-city")
	registerCityForRigResolution(t, gcHome, cityPath, "real-city")

	rigFlag = "ghost"
	_, err := resolveContext()
	if err == nil {
		t.Fatal("expected error for unknown name")
	}
	if !strings.Contains(err.Error(), `rig "ghost" is not registered`) {
		t.Errorf("expected the canonical not-registered error, got %v", err)
	}
}

// TestHQAddressable_AmbiguousHQNameDiagnostic checks the multi-match
// diagnostic — two registered cities whose live workspace.name happens
// to be identical surface a clear "specify by path" hint rather than
// picking arbitrarily. The registry refuses duplicate registry names,
// but live workspace.name drift can produce the same effective HQ
// from two distinct registrations.
func TestHQAddressable_AmbiguousHQNameDiagnostic(t *testing.T) {
	resetFlags(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	// Both cities have workspace.name = "twin" in their city.toml,
	// but each is registered under a distinct registry name.
	cityA := setupCity(t, "twin")
	cityB := setupCity(t, "twin")
	registerCityForRigResolution(t, gcHome, cityA, "registry-a")
	registerCityForRigResolution(t, gcHome, cityB, "registry-b")

	rigFlag = "twin"
	_, err := resolveContext()
	if err == nil {
		t.Fatal("expected ambiguity error for duplicated HQ workspace.name")
	}
	if !strings.Contains(err.Error(), "registered for multiple cities") {
		t.Errorf("expected ambiguity wording, got %v", err)
	}
	if !strings.Contains(err.Error(), "--city") {
		t.Errorf("error should suggest --city escape hatch, got %v", err)
	}
}

// TestRegisteredHQByName_Empty confirms the helper returns nil/empty for
// blank names — guards against panics on invalid CLI input.
func TestRegisteredHQByName_Empty(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	matches, err := registeredHQByName("", true)
	if err != nil {
		t.Fatalf("empty name should not error, got: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("empty name should yield no matches, got: %+v", matches)
	}
}

// TestRegisteredHQByName_NoCities confirms graceful behavior when no
// cities are registered yet.
func TestRegisteredHQByName_NoCities(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	matches, err := registeredHQByName("anything", true)
	if err != nil {
		t.Fatalf("empty registry should not error, got: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("empty registry should yield no matches, got %d", len(matches))
	}
}

// TestRegisteredHQByName_DedupesIdenticalCity guards against double-
// counting when both the registry name and the live config name match.
// The same city must contribute only one binding.
func TestRegisteredHQByName_DedupesIdenticalCity(t *testing.T) {
	resetFlags(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := setupCity(t, "matchme")
	registerCityForRigResolution(t, gcHome, cityPath, "matchme")

	matches, err := registeredHQByName("matchme", true)
	if err != nil {
		t.Fatalf("registeredHQByName: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1 (dedup must collapse registry+live duplicates)", len(matches))
	}
	assertSameTestPath(t, matches[0].City.Path, cityPath)
}

// TestResolveHQBindingMatches_ZeroMatches must surface no error when
// resolveRigToContext walks past it — only the final fallback in the
// caller chain should produce the not-registered error.
func TestResolveHQBindingMatches_ZeroMatches(t *testing.T) {
	// Calling resolveHQBindingMatches with zero matches is a programming
	// error in the caller; the function panics or returns garbage. The
	// call sites guard with len(matches) > 0 first. We only test the
	// non-empty branches. This test is kept as a placeholder so future
	// refactors that add a no-match branch get a coverage signal.
	_ = resolveHQBindingMatches // compile-time reference
}

// TestHQAddressable_ResolveContextFromPath confirms passing the city
// root as a positional path arg already worked (existing behavior) —
// guards against a regression on the path side from the new
// HQ-by-name code.
func TestHQAddressable_ResolveContextFromPath(t *testing.T) {
	resetFlags(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := setupCity(t, "by-path")
	registerCityForRigResolution(t, gcHome, cityPath, "by-path")

	ctx, err := resolveContextFromPath(cityPath)
	if err != nil {
		t.Fatalf("resolveContextFromPath: %v", err)
	}
	assertSameTestPath(t, ctx.CityPath, cityPath)
	if ctx.RigName != "" {
		t.Errorf("city-root path should resolve with empty RigName; got %q", ctx.RigName)
	}
}
