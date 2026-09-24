package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeScopeBeadsMetadata writes a scope's .beads/metadata.json verbatim so a
// classification test can describe exactly what bd would have left behind.
func writeScopeBeadsMetadata(t *testing.T, scopeRoot, metadata string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A scope whose bd metadata says proxied-server is bd's to run even when Gas
// City never journaled the initialization: `bd migrate from-server-to-proxied-
// server` rewrites metadata in place, and a clone carries the committed
// metadata without any journal. Classifying those as legacy left gc stop and
// gc doctor skipping them entirely.
func TestScopeProviderOwnedClassifiesJournalOrPersistedProxiedBinding(t *testing.T) {
	for _, tt := range []struct {
		name      string
		metadata  string
		journal   bool
		want      bool
		wantState string
		wantErr   bool
	}{
		{name: "journal only", journal: true, want: true, wantState: providerScopeInitializing},
		{name: "metadata only", metadata: `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`, want: true, wantState: providerScopeReady},
		{name: "journal and metadata", metadata: `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`, journal: true, want: true, wantState: providerScopeInitializing},
		{name: "neither", want: false},
		{name: "direct server metadata stays legacy", metadata: `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`, want: false},
		{name: "metadata without a dolt mode stays legacy", metadata: `{"database":"dolt","backend":"dolt","dolt_database":"hq"}`, want: false},
		{name: "malformed metadata fails closed", metadata: `{"database":`, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			city := t.TempDir()
			if tt.metadata != "" {
				writeScopeBeadsMetadata(t, city, tt.metadata)
			}
			if tt.journal {
				if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
					t.Fatal(err)
				}
			}
			owned, err := scopeProviderOwned(city, city)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("scopeProviderOwned = (%v, nil), want a fail-closed error", owned)
				}
				return
			}
			if err != nil {
				t.Fatalf("scopeProviderOwned: %v", err)
			}
			if owned != tt.want {
				t.Fatalf("scopeProviderOwned = %v, want %v", owned, tt.want)
			}
			entry, stateOwned, err := providerOwnedScopeState(city, city)
			if err != nil {
				t.Fatalf("providerOwnedScopeState: %v", err)
			}
			if stateOwned != tt.want {
				t.Fatalf("providerOwnedScopeState owned = %v, want %v", stateOwned, tt.want)
			}
			if tt.want && entry.State != tt.wantState {
				t.Fatalf("providerOwnedScopeState state = %q, want %q", entry.State, tt.wantState)
			}
		})
	}
}

// The derived topology of an un-journaled proxied scope comes from bd's own
// sidecar, never from the proxy's loopback listener.
func TestProviderOwnedScopeIntentFromBindingReadsSidecarUpstream(t *testing.T) {
	for _, tt := range []struct {
		name       string
		sidecar    string
		wantTarget string
	}{
		{name: "local proxy has no sidecar upstream", wantTarget: "local"},
		{name: "external upstream", sidecar: `{"root_path":"","external":{"host":"db.example.invalid","port":3306}}`, wantTarget: "external"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scope := t.TempDir()
			writeScopeBeadsMetadata(t, scope, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`)
			if tt.sidecar != "" {
				if err := os.WriteFile(filepath.Join(scope, ".beads", "proxied_server_client_info.json"), []byte(tt.sidecar), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			intent, proxied, err := providerOwnedScopeIntentFromBinding(scope)
			if err != nil || !proxied {
				t.Fatalf("providerOwnedScopeIntentFromBinding = (%+v, %v, %v)", intent, proxied, err)
			}
			if intent.Transport != "proxied" || intent.Target != tt.wantTarget {
				t.Fatalf("intent = %+v, want proxied/%s", intent, tt.wantTarget)
			}
		})
	}
}

// A rig detached from city.toml keeps its bd processes; only stop may reach
// them. Reviving one from start or health would resurrect a scope the operator
// removed, which is why the fan-out is asymmetric.
func TestProviderOwnedLifecycleScopeRootsIncludeDetachedRecordsOnlyWhenRetiring(t *testing.T) {
	city := t.TempDir()
	configured := filepath.Join(city, "rigs", "configured")
	detached := filepath.Join(city, "rigs", "detached")
	for _, dir := range []string{configured, detached} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"t\"\n[beads]\nprovider = \"bd\"\n[[rigs]]\nname = \"configured\"\npath = \"rigs/configured\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, detached, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(city, detached); err != nil {
		t.Fatal(err)
	}
	if err := removeProviderScopeOwnershipRecord(city, "rig:detached"); err != nil {
		t.Fatal(err)
	}

	start, err := providerOwnedLifecycleScopeRoots(city, "start")
	if err != nil {
		t.Fatalf("providerOwnedLifecycleScopeRoots(start): %v", err)
	}
	if want := []string{normalizePathForCompare(city), normalizePathForCompare(configured)}; !reflect.DeepEqual(start, want) {
		t.Fatalf("start roots = %#v, want %#v", start, want)
	}
	stop, err := providerOwnedLifecycleScopeRoots(city, "stop")
	if err != nil {
		t.Fatalf("providerOwnedLifecycleScopeRoots(stop): %v", err)
	}
	want := []string{normalizePathForCompare(city), normalizePathForCompare(configured), normalizePathForCompare(detached)}
	if !reflect.DeepEqual(stop, want) {
		t.Fatalf("stop roots = %#v, want %#v", stop, want)
	}
}

// A city.toml that no longer parses must not strand bd's processes: stop still
// enumerates the city and every journaled scope.
func TestProviderOwnedLifecycleScopeRootsSurviveInvalidCityConfigWhenRetiring(t *testing.T) {
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("this is not toml = = =\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := providerOwnedLifecycleScopeRoots(city, "start"); err == nil {
		t.Fatal("start must not proceed past an unreadable city config")
	}
	roots, err := providerOwnedLifecycleScopeRoots(city, "stop")
	if err != nil {
		t.Fatalf("providerOwnedLifecycleScopeRoots(stop): %v", err)
	}
	if want := []string{normalizePathForCompare(city)}; !reflect.DeepEqual(roots, want) {
		t.Fatalf("stop roots = %#v, want %#v", roots, want)
	}
}

// gc stop retires every provider-owned scope, including a rig detached from
// city.toml whose proxy would otherwise outlive the city.
func TestProviderOwnedStopCoversDetachedPathRecords(t *testing.T) {
	city := t.TempDir()
	detached := filepath.Join(city, "rigs", "detached")
	if err := os.MkdirAll(detached, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(city, "provider-ops")
	script := filepath.Join(city, "provider.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s:%s\\n' \"$1\" \"$BEADS_DIR\" >> \""+logPath+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"t\"\n[beads]\nprovider = \"exec:"+script+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{city, detached} {
		if err := persistProviderScopeOwnership(city, scope, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
			t.Fatal(err)
		}
		if err := markProviderScopeOwnershipReady(city, scope); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeProviderScopeOwnershipRecord(city, "rig:detached"); err != nil {
		t.Fatal(err)
	}
	if err := runProviderOwnedScopesLifecycleOp(city, "stop"); err != nil {
		t.Fatalf("runProviderOwnedScopesLifecycleOp(stop): %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"stop:" + filepath.Join(normalizePathForCompare(city), ".beads"),
		"stop:" + filepath.Join(normalizePathForCompare(detached), ".beads"),
	}
	if got := strings.Fields(string(data)); !reflect.DeepEqual(got, want) {
		t.Fatalf("provider stop fan-out = %#v, want %#v", got, want)
	}
}

// gc stop's supervisor-unregistered and unreadable-config branches both reach
// the bead store through stopCityManagedBeadsProvider. A proxied city
// publishes no managed Dolt port, so gating on one skipped them entirely.
func TestStopCityManagedBeadsProviderStopsProviderOwnedScopeWithoutManagedPort(t *testing.T) {
	for _, tt := range []struct {
		name         string
		cityTOML     string
		wantAttempts int
	}{
		{name: "configured city", cityTOML: "[workspace]\nname = \"t\"\n[beads]\nprovider = \"bd\"\n", wantAttempts: 1},
		{name: "unreadable city config", cityTOML: "this is not toml = = =\n", wantAttempts: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			city := t.TempDir()
			if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(tt.cityTOML), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
				t.Fatal(err)
			}
			if err := markProviderScopeOwnershipReady(city, city); err != nil {
				t.Fatal(err)
			}
			attempts := 0
			previous := shutdownBeadsProviderForStop
			shutdownBeadsProviderForStop = func(string) error {
				attempts++
				return nil
			}
			t.Cleanup(func() { shutdownBeadsProviderForStop = previous })

			stopped, err := stopCityManagedBeadsProvider(city)
			if err != nil {
				t.Fatalf("stopCityManagedBeadsProvider: %v", err)
			}
			if !stopped {
				t.Fatal("stopCityManagedBeadsProvider reported nothing to stop for a provider-owned city")
			}
			if attempts != tt.wantAttempts {
				t.Fatalf("bead store stop attempts = %d, want %d", attempts, tt.wantAttempts)
			}
		})
	}
}

// A legacy bd city with neither provider ownership nor a live managed port
// keeps its historical "nothing to stop" answer.
func TestStopCityManagedBeadsProviderLeavesLegacyCityAlone(t *testing.T) {
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"t\"\n[beads]\nprovider = \"bd\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := shutdownBeadsProviderForStop
	shutdownBeadsProviderForStop = func(string) error {
		t.Fatal("legacy city stopped a bead store it does not own")
		return nil
	}
	t.Cleanup(func() { shutdownBeadsProviderForStop = previous })
	stopped, err := stopCityManagedBeadsProvider(city)
	if err != nil || stopped {
		t.Fatalf("stopCityManagedBeadsProvider = (%v, %v), want (false, nil)", stopped, err)
	}
}

// A workspace cloned from a proxied city carries bd's committed metadata but
// not the gitignored store. Serving it would silently create an empty tracker,
// so gc refuses without invoking bd at all.
func TestProviderOwnedProxiedScopeWithoutStoreRefusesWithoutMutation(t *testing.T) {
	city := t.TempDir()
	logPath := filepath.Join(city, "bd-invocations")
	script := filepath.Join(city, "provider.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+logPath+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"t\"\n[beads]\nprovider = \"exec:"+script+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeScopeBeadsMetadata(t, city, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`)

	for _, op := range []string{"start", "ensure-ready", "health", "recover"} {
		err := runProviderOwnedScopeLifecycleOpContext(context.Background(), city, city, op)
		if err == nil {
			t.Fatalf("%s served a proxied scope with no store", op)
		}
		if !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), "Refusing to create an empty store") {
			t.Fatalf("%s error = %v, want a typed missing-store refusal", op, err)
		}
	}
	if _, err := runProviderOwnedScopeInit(city, city, "hq", script); err == nil {
		t.Fatal("init served a proxied scope with no store")
	}
	if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("refusal invoked bd: %v\n%s", err, data)
	}
	if _, err := os.Stat(filepath.Join(city, ".beads", "dolt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refusal created a provider store: %v", err)
	}

	// Restoring the store clears the refusal: the scope is bd's to run again.
	if err := os.MkdirAll(filepath.Join(city, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runProviderOwnedScopeLifecycleOpContext(context.Background(), city, city, "health"); err != nil {
		t.Fatalf("restored proxied scope health: %v", err)
	}
}

// Stop must still reach a proxied scope whose store is missing: a refusal
// there would be a refusal to clean up.
func TestProviderOwnedProxiedScopeWithoutStoreStillStops(t *testing.T) {
	city := t.TempDir()
	logPath := filepath.Join(city, "bd-invocations")
	script := filepath.Join(city, "provider.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+logPath+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"t\"\n[beads]\nprovider = \"exec:"+script+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeScopeBeadsMetadata(t, city, `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`)
	if err := runProviderOwnedScopesLifecycleOp(city, "stop"); err != nil {
		t.Fatalf("runProviderOwnedScopesLifecycleOp(stop): %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "stop" {
		t.Fatalf("stop invocations = %q, want %q", got, "stop")
	}
}
