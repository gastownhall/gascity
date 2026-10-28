//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

// writeDirectProviderOwnedCity makes city a provider-owned city in bd's direct
// (`dolt_mode: server`) transport, as `gc init --beads-transport direct` leaves
// it: gc's ownership journal names the provider, and bd's own files carry no gc
// endpoint keys.
func writeDirectProviderOwnedCity(t *testing.T, city string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"direct\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(city, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: "hq",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, ".beads", "config.yaml"), []byte("# Beads Configuration File\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeProviderScopeOwnershipJournal(city, providerScopeOwnershipJournal{
		Version: 1,
		Scopes: map[string]providerScopeOwnershipEntry{
			"city": {
				ScopePath:      normalizePathForCompare(city),
				LifecycleOwner: providerScopeLifecycleOwner,
				State:          providerScopeReady,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestBdEnvLeavesABdOwnedCitysReadinessToBd pins that building the bd
// environment for a provider-owned direct city runs no health check. With no
// Dolt target of gc's own, the resolver's last rung ran `bd ping` for every
// scope before every bd command gc made, and its answer did not change the
// environment.
func TestBdEnvLeavesABdOwnedCitysReadinessToBd(t *testing.T) {
	n := newNativeBdTest(t)
	writeDirectProviderOwnedCity(t, n.city)
	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(n.city))
	t.Setenv("BD_BIN", n.bdBin)
	t.Setenv(nativeFakeBdLogEnv, n.log)
	if !cityUsesBdStoreContract(n.city) {
		t.Fatal("premise: the fixture is not a bd city, so the env builder would never reach the Dolt ladder")
	}
	if owned, err := cityScopeProviderOwned(n.city); err != nil || !owned {
		t.Fatalf("premise: the fixture is not provider-owned: (%t, %v)", owned, err)
	}

	env, err := bdRuntimeEnvWithError(n.city)
	if err != nil {
		t.Fatalf("bdRuntimeEnvWithError: %v", err)
	}
	for _, name := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT"} {
		if value, set := env[name]; set && value != "" {
			t.Errorf("env %s = %q; bd owns this server, so gc must not name one", name, value)
		}
	}
	n.requireArgs()

	if _, ok, err := resolvedRuntimeCityDoltTargetContext(t.Context(), n.city, true); ok || err != nil {
		t.Fatalf("resolvedRuntimeCityDoltTargetContext = (ok %t, %v), want no target of gc's own and no error", ok, err)
	}
	n.requireArgs()
}

func TestCityDoltReadinessBelongsToBdOnlyForAProviderOwnedCity(t *testing.T) {
	owned := t.TempDir()
	writeDirectProviderOwnedCity(t, owned)
	if !cityDoltReadinessBelongsToBd(owned) {
		t.Error("a provider-owned city's Dolt readiness is bd's")
	}
	plain := t.TempDir()
	if err := os.WriteFile(filepath.Join(plain, "city.toml"), []byte("[workspace]\nname = \"plain\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cityDoltReadinessBelongsToBd(plain) {
		t.Error("a city gc has not handed to the provider keeps gc's recovery")
	}
}
