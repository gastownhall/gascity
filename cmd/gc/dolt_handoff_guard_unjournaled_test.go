package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The managed-Dolt lifecycle fence has to use the same ownership answer the
// rest of the feature does (R1): journal OR bd's persisted proxied binding. A
// workspace migrated in place with `bd migrate from-server-to-proxied-server`,
// or cloned from a proxied city, carries the binding in committed metadata and
// has no journal at all — and consulting only the journal let `gc dolt-state
// start-managed` raise a second, GC-managed sql-server over bd's proxy root.
func TestAdmitLegacyManagedDoltLifecycleRefusesUnJournaledProxiedCity(t *testing.T) {
	city := t.TempDir()
	writeScopeBeadsMetadata(t, city, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`)
	if err := os.MkdirAll(filepath.Join(city, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(city, ".gc", scopeOwnershipFile)); !os.IsNotExist(err) {
		t.Fatalf("fixture has an ownership journal; the test would pass vacuously: %v", err)
	}

	err := admitLegacyManagedDoltLifecycle(city)
	if err == nil {
		t.Fatal("an un-journaled proxied city admitted the managed Dolt lifecycle")
	}
	if !strings.Contains(err.Error(), "provider scope ownership") {
		t.Fatalf("refusal = %v, want a typed provider-ownership refusal", err)
	}
}

// A legacy direct city still gets the managed lifecycle: the fence must not
// grow into a refusal for every city.
func TestAdmitLegacyManagedDoltLifecycleAllowsLegacyDirectCity(t *testing.T) {
	city := t.TempDir()
	writeScopeBeadsMetadata(t, city, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`)
	if err := admitLegacyManagedDoltLifecycle(city); err != nil {
		t.Fatalf("legacy direct city refused the managed Dolt lifecycle: %v", err)
	}
}
