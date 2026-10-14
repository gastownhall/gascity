package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A scope directory with no city.toml is the shape every lifecycle probe meets
// before a city exists: a file-provider fixture, a GC_DOLT=skip scaffold, a
// bare directory a rig is about to be added to. The provider-ownership code
// answers questions about such a directory, so the questions have to be
// answerable — and answering them must not write to the city.

func TestProviderOwnedScopeRootsTreatsAbsentCityConfigAsNoRigs(t *testing.T) {
	cityPath := t.TempDir()

	for _, op := range []string{"health", "start", "stop"} {
		roots, err := providerOwnedLifecycleScopeRoots(cityPath, op)
		if err != nil {
			t.Fatalf("op %q: %v", op, err)
		}
		if len(roots) != 1 || !samePath(roots[0], cityPath) {
			t.Fatalf("op %q roots = %v, want just the city root", op, roots)
		}
	}
}

func TestProviderOwnedScopeRootsStillRefusesUnparseableCityConfig(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("this is not = = toml\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := providerOwnedLifecycleScopeRoots(cityPath, "health"); err == nil {
		t.Fatal("a starting op guessed past a city.toml that will not parse")
	}
	// A retiring op still has to reach bd's processes: a config error must not
	// strand them.
	if _, err := providerOwnedLifecycleScopeRoots(cityPath, "stop"); err != nil {
		t.Fatalf("stop refused to enumerate scopes for a broken config: %v", err)
	}
}

// TestProviderOwnedScopeRootsDoesNotMaterializeBuiltinPacks pins that asking
// which scopes an op visits is a read. The ordinary config loader materializes
// builtin packs on the way in, which rewrote pack scripts under .gc/system
// from a health check — including over a pack script an operator or a test had
// deliberately replaced.
func TestProviderOwnedScopeRootsDoesNotMaterializeBuiltinPacks(t *testing.T) {
	cityPath := t.TempDir()
	script := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	const sentinel = "#!/bin/sh\nexit 0\n# sentinel\n"
	if err := os.WriteFile(script, []byte(sentinel), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := providerOwnedLifecycleScopeRoots(cityPath, "health"); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("pack script disappeared: %v", err)
	}
	if string(got) != sentinel {
		t.Fatalf("enumerating scope roots rewrote %s", script)
	}
}

func TestEnsureProviderScopeOwnershipBeforeInitSkipsConfiglessCity(t *testing.T) {
	cityPath := t.TempDir()
	t.Setenv("GC_BEADS", "bd")

	if err := ensureProviderScopeOwnershipBeforeInit(cityPath, cityPath); err != nil {
		t.Fatalf("configless city was refused: %v", err)
	}
	if _, err := os.Stat(providerScopeOwnershipPath(cityPath)); err == nil {
		t.Fatal("a topology was journaled for a city with no config to read it from")
	}
	owned, err := scopeProviderOwned(cityPath, cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if owned {
		t.Fatal("configless city classified as provider-owned")
	}
}

// TestScopeOwnershipReadsSurviveNonDirectoryBeadsPath covers the broken-scope
// shape `gc rig add` rolls back from: .beads is a regular file, so every path
// under it returns ENOTDIR. That is "the artifact is not there", and reporting
// it as a malformed ownership record buried the failure the caller was
// actually handling.
func TestScopeOwnershipReadsSurviveNonDirectoryBeadsPath(t *testing.T) {
	scope := t.TempDir()
	if err := os.WriteFile(filepath.Join(scope, ".beads"), []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	proxied, err := scopeBindingIsProviderOwnedProxied(scope)
	if err != nil {
		t.Fatalf("scopeBindingIsProviderOwnedProxied: %v", err)
	}
	if proxied {
		t.Fatal("a scope with no .beads directory was classified as bd-owned proxied")
	}
	transferred, err := committedBeadsHandoffOwnsScope(scope)
	if err != nil {
		t.Fatalf("committedBeadsHandoffOwnsScope: %v", err)
	}
	if transferred {
		t.Fatal("a scope with no .beads directory was classified as handed off")
	}
	if _, err := proxiedScopeHasExternalUpstream(scope); err != nil {
		t.Fatalf("proxiedScopeHasExternalUpstream: %v", err)
	}
}
