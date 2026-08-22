package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRigBindingScanSkipsBuiltinPackRefresh guards the sys-s3pd fix: enumerating
// registered rig bindings (the registry scan on resolveContext's city-needing
// hot path) must read cfg.Rigs WITHOUT materializing each city's builtin packs.
//
// The builtin-pack refresh (ensureBuiltinPacksForConfigLoad → EnsureBuiltinRuntimeAssets)
// is ~400ms per city and is pure runtime-asset prep for *operating* a city — it
// has no bearing on which rigs a city declares. A default-provider city uses the
// bd store contract, so a full loadCityConfig-with-refresh would write the
// gc-beads-bd shim. Asserting that shim is absent after a name-based rig-binding
// scan proves the scan took the cheaper no-refresh path.
func TestRigBindingScanSkipsBuiltinPackRefresh(t *testing.T) {
	resetFlags(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := setupCity(t, "epsilon")
	rigDir := filepath.Join(t.TempDir(), "svc")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	registerRigBindingForResolution(t, gcHome, cityPath, "epsilon", "svc", rigDir)

	// The shim marker is only meaningful when the city uses the bd store
	// contract (the default), since only then does the refresh write it.
	if !cityUsesBdStoreContract(cityPath) {
		t.Skip("test city does not use the bd store contract; shim marker unavailable")
	}
	shim := gcBeadsBdScriptPath(cityPath)
	if _, err := os.Stat(shim); err == nil {
		t.Fatalf("precondition failed: shim %s already present before scan", shim)
	}

	// Resolve the rig binding by name — this is the registry-scan path
	// (registeredRigBindings) that resolveContext/resolveRigToContext use.
	matches, _, err := registeredRigBindingsByName("svc", false)
	if err != nil {
		t.Fatalf("registeredRigBindingsByName error: %v", err)
	}
	if len(matches) != 1 || matches[0].Rig.Name != "svc" {
		t.Fatalf("rig binding not resolved correctly: %#v", matches)
	}

	// The scan must NOT have triggered builtin-pack materialization.
	if _, err := os.Stat(shim); err == nil {
		t.Fatalf("registry rig-binding scan materialized builtin-pack shim %s; "+
			"the scan must not refresh builtin packs (sys-s3pd)", shim)
	}
}
