package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFreshInheritedCity lays down a city bound to a store gc does not serve
// — a complete opaque storage binding — and returns the city path plus the
// path of a rig directory that exists but has not been configured yet. That
// is the state `gc rig add` is in when it asks whether to set up managed Dolt.
func writeFreshInheritedCity(t *testing.T, cityMetadata string) (cityPath, rigPath string) {
	t.Helper()
	cityPath = t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte("issue_prefix: gc\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(cityMetadata), 0o600); err != nil {
		t.Fatal(err)
	}
	rigPath = filepath.Join(cityPath, "rigs", "fresh")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	return cityPath, rigPath
}

// TestScopeSkipsManagedDoltForInitInheritsCityBindingForAFreshRig pins the
// defect that made `gc rig add` unusable on a city whose bead store lives on
// someone else's server (gas-4cu): the inheritance arm demanded an
// authoritative scope config.yaml, but `gc rig add` writes the rig's
// config.yaml only *after* this gate runs. A directory created moments ago
// therefore had nothing authoritative to resolve, fell through to the
// managed-Dolt lane, and the add died on a Dolt server that does not exist:
//
//	gc rig add: exec beads init: managed Dolt server unreachable while
//	inspecting existing store 'gas'; refusing to force-reinitialize
//
// A scope carrying no metadata of its own has not chosen a backend. It
// inherits the city's, and that is decidable from the city alone.
func TestScopeSkipsManagedDoltForInitInheritsCityBindingForAFreshRig(t *testing.T) {
	// "bd", not "file": the gate returns early for a city that does not use
	// the bd store contract, so a provider outside that contract makes this
	// test pass without ever reaching the inheritance decision it is about.
	t.Setenv("GC_BEADS", "bd")
	cityPath, rigPath := writeFreshInheritedCity(t, `{"database":"beads","backend":"mysql","storage_endpoint":"opaque-remote","storage_database":"anthony_beads"}`)

	got, err := scopeSkipsManagedDoltForInit(cityPath, rigPath)
	if err != nil {
		t.Fatalf("scopeSkipsManagedDoltForInit: %v", err)
	}
	if !got {
		t.Fatal("scopeSkipsManagedDoltForInit = false for a fresh rig under a city with a complete storage binding; " +
			"the rig gets managed-Dolt setup and the add dies on a Dolt server that does not exist")
	}
}

// TestScopeSkipsManagedDoltForInitStillClaimsAFreshRigOnAManagedDoltCity is
// the other half of the same gate: inheriting must not become a blanket
// escape. A city gc serves itself keeps its fresh rigs on the managed lane.
func TestScopeSkipsManagedDoltForInitStillClaimsAFreshRigOnAManagedDoltCity(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath, rigPath := writeFreshInheritedCity(t, `{"database":"beads.db","backend":"dolt","dolt_database":"hq"}`)

	got, err := scopeSkipsManagedDoltForInit(cityPath, rigPath)
	if err != nil {
		t.Fatalf("scopeSkipsManagedDoltForInit: %v", err)
	}
	if got {
		t.Fatal("scopeSkipsManagedDoltForInit = true for a fresh rig on a managed-Dolt city; managed setup would be skipped")
	}
}

// TestInitAndHookDirLeavesAFreshInheritedRigUnpinned extends the invariant
// TestInitAndHookDirSkipsDoltInitForInheritedCityPostgresRig states for a rig
// that already declares inherited_city, to the rig that has not been
// configured yet. Skipping managed Dolt must not turn into pinning a copy of
// the city's binding: an inherited scope resolves the city's on each use, so
// a copy would only go stale the moment the city's store moves.
func TestInitAndHookDirLeavesAFreshInheritedRigUnpinned(t *testing.T) {
	cityPath, rigPath := writeFreshInheritedCity(t, `{"database":"beads","backend":"postgres","storage_endpoint":"postgres://bd@db.example.test:5432","storage_database":"beads_pg"}`)

	callsFile := filepath.Join(t.TempDir(), "provider-calls.log")
	script := filepath.Join(t.TempDir(), "gc-beads-bd")
	scriptBody := fmt.Sprintf("#!/bin/sh\nset -eu\nprintf '%%s\\n' \"$*\" >> %q\nexit 99\n", callsFile)
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

	if err := initAndHookDir(cityPath, rigPath, "fresh"); err != nil {
		t.Fatalf("initAndHookDir: %v", err)
	}

	if data, err := os.ReadFile(callsFile); err == nil {
		t.Fatalf("provider init should not run for a fresh inherited rig; calls:\n%s", data)
	} else if !os.IsNotExist(err) {
		t.Fatalf("read provider calls: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rigPath, ".beads", "metadata.json")); !os.IsNotExist(err) {
		t.Fatalf("fresh inherited rig should not be pinned with local metadata, stat err = %v", err)
	}
}

// TestScopeSkipsManagedDoltForInitStillValidatesAScopeWithItsOwnConfig pins
// the boundary of the inheritance shortcut above. "No metadata.json" is not
// on its own evidence of a freshly-created rig: a scope that has already
// written its own .beads/config.yaml has chosen something, and a config
// declaring managed_city is invalid at rig scope. That must still surface as
// a hard error rather than being waved through as inheritance, so the
// shortcut stays scoped to a directory carrying no config of its own — which
// is exactly the state `gc rig add` is in when this gate runs.
func TestScopeSkipsManagedDoltForInitStillValidatesAScopeWithItsOwnConfig(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath, rigPath := writeFreshInheritedCity(t, `{"database":"beads","backend":"mysql","storage_endpoint":"opaque-remote","storage_database":"anthony_beads"}`)

	// The rig carries its own config declaring an origin that is invalid at
	// rig scope, but still has no metadata.json of its own.
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte("issue_prefix: fresh\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := scopeSkipsManagedDoltForInit(cityPath, rigPath)
	if err == nil {
		t.Fatal("scopeSkipsManagedDoltForInit returned no error for a rig scope declaring managed_city; " +
			"the endpoint-origin validation was skipped because the scope has no metadata.json")
	}
	if !strings.Contains(err.Error(), "endpoint origin is invalid for rig scope") {
		t.Fatalf("scopeSkipsManagedDoltForInit error = %v, want the rig-scope endpoint-origin validation failure", err)
	}
}

// A city whose binding is partial — some but not all of backend /
// storage_endpoint / storage_database — is broken config. The fresh-rig
// inheritance arm surfaces that as a hard error rather than silently taking
// the managed-Dolt lane, matching what the configured-inherited arm already
// does for the same city.
func TestScopeSkipsManagedDoltForInitFailsClosedOnAPartialCityBinding(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityPath, rigPath := writeFreshInheritedCity(t, `{"database":"beads","backend":"mysql","storage_endpoint":"opaque-remote"}`)

	_, err := scopeSkipsManagedDoltForInit(cityPath, rigPath)
	if err == nil || !strings.Contains(err.Error(), "partial beads storage binding") {
		t.Fatalf("scopeSkipsManagedDoltForInit error = %v, want a partial-binding refusal", err)
	}
}
