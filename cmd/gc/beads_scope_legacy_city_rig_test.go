package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLegacyManagedCity builds the production shape D6 says is untouched: a
// GC-managed direct city — metadata dolt_mode server, canonical config with
// gc.endpoint_origin managed_city, bd auto-start off — with no scope-ownership
// journal, plus one fresh rig declared in city.toml.
func writeLegacyManagedCity(t *testing.T) (cityPath, rigPath, logPath string) {
	t.Helper()
	cityPath = t.TempDir()
	rigPath = filepath.Join(cityPath, "rigs", "fresh")
	logPath = filepath.Join(cityPath, "provider-ops")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(cityPath, "gc-beads-bd.sh")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + logPath + "\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatal(err)
	}
	writeScopeBeadsMetadata(t, cityPath, `{"backend":"dolt","database":"dolt","dolt_mode":"server","dolt_database":"hq"}`)
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"),
		[]byte("issue_prefix: hq\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.mode: server\ndolt.auto-start: false\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	cityTOML := "[workspace]\nname = \"legacy\"\n[beads]\nprovider = \"exec:" + script + "\"\n" +
		"[[rigs]]\nname = \"fresh\"\npath = \"rigs/fresh\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	return cityPath, rigPath, logPath
}

// Provider-owned inheritance applies only when the CITY is provider-owned. A
// rig added to a grandfathered GC-managed direct city was journaled
// direct/local and initialized with a bare `bd init --server`, so bd spawned
// its own sql-server for the rig instead of creating a database on the city's
// managed server: two Dolt lifecycles in one city, and `gc dolt` orders and
// backups that only know the managed one.
func TestFreshRigUnderLegacyManagedCityKeepsTheLegacyPath(t *testing.T) {
	cityPath, rigPath, _ := writeLegacyManagedCity(t)
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureFreshRigProviderOwnership(cityPath, cfg); err != nil {
		t.Fatalf("ensureFreshRigProviderOwnership: %v", err)
	}
	if _, owned, err := providerScopeOwnership(cityPath, rigPath); err != nil || owned {
		t.Fatalf("fresh rig ownership = (%t, %v), want the legacy inherited path", owned, err)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gc", scopeOwnershipFile)); !os.IsNotExist(err) {
		t.Fatalf("a grandfathered city wrote a provider ownership journal: %v", err)
	}

	// The same holds for the rig-add front door's own boundary.
	if err := ensureProviderScopeOwnershipBeforeInit(cityPath, rigPath); err != nil {
		t.Fatalf("ensureProviderScopeOwnershipBeforeInit: %v", err)
	}
	if _, owned, err := providerScopeOwnership(cityPath, rigPath); err != nil || owned {
		t.Fatalf("rig-add boundary ownership = (%t, %v), want the legacy inherited path", owned, err)
	}
}

// `gc rig add` on that city must reach the legacy managed-Dolt initializer,
// which pins the rig to the city's server, and never the provider-owned
// `bd init --server` that gives the rig a server of its own.
func TestRigAddUnderLegacyManagedCityDoesNotRunProviderOwnedInit(t *testing.T) {
	clearGCEnv(t)
	cityPath, rigPath, logPath := writeLegacyManagedCity(t)
	t.Setenv("GC_DOLT", "skip")
	if _, err := initDirIfReady(cityPath, rigPath, "fresh"); err != nil {
		t.Fatalf("initDirIfReady: %v", err)
	}
	if _, owned, err := providerScopeOwnership(cityPath, rigPath); err != nil || owned {
		t.Fatalf("rig ownership = (%t, %v), want the legacy inherited path", owned, err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "--server") {
		t.Fatalf("rig add ran a provider-owned bd init on a grandfathered city:\n%s", data)
	}
}

// A city that IS provider-owned still hands its topology to a fresh rig: that
// is the inheritance the proxied default depends on.
func TestFreshRigUnderProviderOwnedCityStillInherits(t *testing.T) {
	cityPath, rigPath, _ := writeLegacyManagedCity(t)
	writeScopeBeadsMetadata(t, cityPath, `{"backend":"dolt","database":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`)
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureFreshRigProviderOwnership(cityPath, cfg); err != nil {
		t.Fatalf("ensureFreshRigProviderOwnership: %v", err)
	}
	entry, owned, err := providerScopeOwnership(cityPath, rigPath)
	if err != nil || !owned {
		t.Fatalf("fresh rig ownership = (%+v, %t, %v), want provider-owned", entry, owned, err)
	}
	if want := (providerScopeIntent{Transport: "proxied", Target: "local"}); entry.Intent != want {
		t.Fatalf("fresh rig intent = %+v, want %+v", entry.Intent, want)
	}
}

// So does a journaled bd-owned direct city — the explicit escape hatch. The
// city's own record, not its dolt_mode, is what makes the rig inheritable.
func TestFreshRigUnderJournaledDirectCityStillInherits(t *testing.T) {
	cityPath, rigPath, _ := writeLegacyManagedCity(t)
	if err := persistProviderScopeOwnership(cityPath, cityPath, providerScopeIntent{Transport: "direct", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(cityPath, cityPath); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureFreshRigProviderOwnership(cityPath, cfg); err != nil {
		t.Fatalf("ensureFreshRigProviderOwnership: %v", err)
	}
	entry, owned, err := providerScopeOwnership(cityPath, rigPath)
	if err != nil || !owned {
		t.Fatalf("fresh rig ownership = (%+v, %t, %v), want provider-owned", entry, owned, err)
	}
	if want := (providerScopeIntent{Transport: "direct", Target: "local"}); entry.Intent != want {
		t.Fatalf("fresh rig intent = %+v, want %+v", entry.Intent, want)
	}
}
