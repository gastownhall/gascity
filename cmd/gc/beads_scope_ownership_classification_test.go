package main

import (
	"context"
	"errors"
	"fmt"
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

// A stopped local binding stays bd-owned with or without its transient PID
// record. Credential isolation must not pin its listener or disable auto-start.
func TestProviderOwnedLocalRecoveryKeepsBdOwnership(t *testing.T) {
	for _, tt := range []struct {
		name, mode, pid string
	}{
		{name: "direct no PID", mode: "server"},
		{name: "direct stale PID", mode: "server", pid: "2147483647\n"},
		{name: "direct invalid PID", mode: "server", pid: "invalid\n"},
		{name: "direct unrelated live PID", mode: "server", pid: fmt.Sprintf("%d\n", os.Getpid())},
		{name: "proxied no PID", mode: "proxied-server"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			city := t.TempDir()
			bd := filepath.Join(city, "bd-fixture")
			if err := os.WriteFile(bd, []byte(`#!/bin/sh
set -eu
test -z "${BEADS_DOLT_SERVER_HOST:-}"
test -z "${BEADS_DOLT_SERVER_PORT:-}"
test -z "${BEADS_DOLT_SERVER_SOCKET:-}"
test -z "${BEADS_DOLT_AUTO_START:-}"
printf '%s\n' "$*" >> "$BEADS_DIR/calls"
`), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(fmt.Sprintf("[workspace]\nname = \"t\"\n[workspace.env]\nBD_BIN = %q\n[beads]\nprovider = \"bd\"\n", bd)), 0o644); err != nil {
				t.Fatal(err)
			}
			materializeBuiltinPacksForTest(t, city)
			writeScopeBeadsMetadata(t, city, fmt.Sprintf(`{"database":"dolt","backend":"dolt","dolt_mode":%q,"dolt_database":"work"}`, tt.mode))
			if err := os.MkdirAll(filepath.Join(city, ".beads", "dolt"), 0o755); err != nil {
				t.Fatal(err)
			}
			for name, content := range map[string]string{
				"config.yaml":      "dolt:\n  auto-start: true\n  shared-server: false\n",
				"dolt-server.port": "43123\n",
			} {
				if err := os.WriteFile(filepath.Join(city, ".beads", name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.pid != "" {
				if err := os.WriteFile(filepath.Join(city, ".beads", "dolt-server.pid"), []byte(tt.pid), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			transport := "direct"
			if tt.mode == "proxied-server" {
				transport = "proxied"
			}
			if err := persistProviderScopeOwnership(city, city, providerScopeIntent{Transport: transport, Target: "local"}); err != nil {
				t.Fatal(err)
			}
			if err := markProviderScopeOwnershipReady(city, city); err != nil {
				t.Fatal(err)
			}
			before, _, err := providerScopeOwnership(city, city)
			if err != nil {
				t.Fatal(err)
			}
			for _, op := range []string{"health", "recover", "stop"} {
				if err := runProviderOwnedScopeLifecycleOpContext(context.Background(), city, city, op); err != nil {
					t.Fatalf("%s: %v", op, err)
				}
			}
			data, err := os.ReadFile(filepath.Join(city, ".beads", "calls"))
			if err != nil || string(data) != "ping\ndolt stop\nping\ndolt stop\n" {
				t.Fatalf("local lifecycle commands = %q, error = %v", data, err)
			}
			after, _, err := providerScopeOwnership(city, city)
			if err != nil || !reflect.DeepEqual(before, after) || after.State != providerScopeReady {
				t.Fatalf("local ownership changed: %v", err)
			}
		})
	}
}

// A ready ownership record can survive a switch to hosted storage. Exercise
// the health entrypoint so hosted isolation must survive every later overlay.
func TestProviderOwnedLifecycleHostedCredentialFileIsolation(t *testing.T) {
	for _, hosted := range []bool{true, false} {
		t.Run(fmt.Sprintf("hosted=%t", hosted), func(t *testing.T) {
			city := t.TempDir()
			rig := filepath.Join(city, "rig")
			for _, kv := range os.Environ() {
				key, _, _ := strings.Cut(kv, "=")
				if (strings.HasPrefix(key, "GC_") && key != "GC_HOME") || strings.HasPrefix(key, "BEADS_") || strings.HasPrefix(key, "BD_") {
					t.Setenv(key, "")
				}
			}
			t.Setenv("HOME", t.TempDir())
			credentials := filepath.Join(city, "ambient-credentials")
			t.Setenv("BEADS_CREDENTIALS_FILE", credentials)
			t.Setenv(registryCredentialProviderEnv, `["/opt/gasworks","credential-provider"]`)
			if err := os.WriteFile(credentials, []byte("[untrusted.invalid:3307]\npassword=synthetic-only\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			wantFile, wantCommand := credentials, ""
			if hosted {
				wantFile = ""
				wantCommand = stubHostedBeadsCredentialExecutable(t, filepath.Join(city, "gc"))
			}
			bd := filepath.Join(city, "bd-fixture")
			if err := os.WriteFile(bd, []byte(`#!/bin/sh
set -eu
test "$#" = 1
test "$1" = ping
test "${BEADS_CREDENTIALS_FILE:-}" = "$(/bin/cat "$BEADS_DIR/expected-file")" || { echo 'hosted credential file isolation violated' >&2; exit 1; }
test "${BEADS_DOLT_CREDENTIAL_COMMAND:-}" = "$(/bin/cat "$BEADS_DIR/expected-command")" || { echo 'selected credential bridge lost' >&2; exit 1; }
printf 'ping\n' >> "$BEADS_DIR/calls"
`), 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := "[workspace]\nname = \"credential-isolation\"\n"
			if hosted {
				cfg = hostedBeadsCityTOML("https://beads.example/workspaces/infra", "gasworks", false)
			}
			cfg += fmt.Sprintf("\n[workspace.env]\nBD_BIN = %q\n[beads]\nprovider = \"bd\"\n[[rigs]]\nname = \"rig\"\npath = %q\n", bd, rig)
			if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			materializeBuiltinPacksForTest(t, city)
			for _, scope := range []string{city, rig} {
				writeCompleteStorageBinding(t, scope)
				for name, value := range map[string]string{"expected-file": wantFile, "expected-command": wantCommand} {
					if err := os.WriteFile(filepath.Join(scope, ".beads", name), []byte(value), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := persistProviderScopeOwnership(city, scope, providerScopeIntent{Transport: "direct", Target: "external"}); err != nil {
					t.Fatal(err)
				}
				if err := markProviderScopeOwnershipReady(city, scope); err != nil {
					t.Fatal(err)
				}
			}
			if err := healthBeadsProvider(city); err != nil {
				t.Fatal(err)
			}
			for _, scope := range []string{city, rig} {
				data, err := os.ReadFile(filepath.Join(scope, ".beads", "calls"))
				if err != nil || string(data) != "ping\n" {
					t.Fatalf("scope %s did not reach bd once: %v", scope, err)
				}
			}
		})
	}
}

// Exercise endpoint-matched operator auth through the Go adapter and real
// provider script. Otherwise bd must receive no password and use its own chain.
func TestProviderOwnedLifecycleScopedOperatorAuth(t *testing.T) {
	for _, tt := range []struct {
		name, host, port, password, socket   string
		configHost, configPort, configSocket string
		local, defaultCredentials            bool
	}{
		{name: "scope local credentials"},
		{name: "scope selects its own credential file", defaultCredentials: true},
		{name: "operator password without selectors", password: "operator-synthetic"},
		{name: "operator host only", host: "city-db.invalid", password: "operator-synthetic"},
		{name: "operator port only", port: "3307", password: "operator-synthetic"},
		{name: "operator matches city", host: "city-db.invalid", port: "3307", password: "operator-synthetic"},
		{name: "operator mismatches port", host: "rig-db.invalid", port: "4406", password: "operator-synthetic"},
		{name: "operator matches rig", host: "rig-db.invalid", port: "3308", password: "operator-synthetic"},
		{name: "socket selector does not lend TCP password", host: "city-db.invalid", port: "3307", password: "operator-synthetic", socket: "/unused/socket"},
		{name: "bd local policy does not pin a stopped listener", host: "127.0.0.1", port: "3307", password: "operator-synthetic", local: true},
		{name: "config host mismatch uses normal ping", host: "city-db.invalid", port: "3307", password: "operator-synthetic", configHost: "other-db.invalid"},
		{name: "config port mismatch uses normal ping", host: "city-db.invalid", port: "3307", password: "operator-synthetic", configPort: "3309"},
		{name: "config socket mismatch uses normal ping", host: "city-db.invalid", port: "3307", password: "operator-synthetic", configSocket: "/unused/config.sock"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			city := t.TempDir()
			rig := filepath.Join(city, "rig")
			for _, kv := range os.Environ() {
				key, _, _ := strings.Cut(kv, "=")
				if (strings.HasPrefix(key, "GC_") && key != "GC_HOME") || strings.HasPrefix(key, "BEADS_") || strings.HasPrefix(key, "BD_") {
					t.Setenv(key, "")
				}
			}
			t.Setenv("HOME", t.TempDir())
			t.Setenv("GC_DOLT_HOST", tt.host)
			t.Setenv("GC_DOLT_PORT", tt.port)
			t.Setenv("GC_DOLT_PASSWORD", tt.password)
			t.Setenv("BEADS_DOLT_PASSWORD", "ambient-synthetic")
			t.Setenv("BEADS_DOLT_SERVER_SOCKET", tt.socket)
			t.Setenv("GC_BEADS_PING_DOLT_HOST", "untrusted.invalid")
			t.Setenv("GC_BEADS_PING_DOLT_PORT", "4406")
			credentials := filepath.Join(city, "credentials")
			t.Setenv("BEADS_CREDENTIALS_FILE", credentials)
			if tt.defaultCredentials {
				t.Setenv("BEADS_CREDENTIALS_FILE", "")
			}
			if err := os.WriteFile(credentials, []byte("[city-db.invalid:3307]\npassword=city-file-synthetic\n[rig-db.invalid:3308]\npassword=rig-file-synthetic\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			bd := filepath.Join(city, "bd-fixture")
			// Do not emulate bd's resolver. Assert the final bd environment,
			// leaving .env/endpoint-file resolution to bd's own chain.
			if err := os.WriteFile(bd, []byte(`#!/bin/sh
set -eu
test "$#" = 1
test "$1" = ping
expected=$("/bin/cat" "$BEADS_DIR/expected-password")
test "${BEADS_DOLT_PASSWORD:-}" = "$expected" || { echo 'wrong scoped operator password' >&2; exit 1; }
if [ -n "$expected" ]; then
    test "$BEADS_DOLT_SERVER_HOST:$BEADS_DOLT_SERVER_PORT" = "$("/bin/cat" "$BEADS_DIR/expected-endpoint")"
    test -z "${BEADS_DOLT_SERVER_SOCKET:-}"
    test "$BEADS_DOLT_AUTO_START" = 0
    test "$BEADS_DOLT_SHARED_SERVER" = 0
else
    test -z "${BEADS_DOLT_SERVER_HOST:-}"
    test -z "${BEADS_DOLT_SERVER_PORT:-}"
    test -z "${BEADS_DOLT_SERVER_SOCKET:-}"
    test -z "${BEADS_DOLT_AUTO_START:-}"
fi
expected_file=$("/bin/cat" "$BEADS_DIR/expected-file")
if [ -n "$expected_file" ]; then
    test "${BEADS_CREDENTIALS_FILE:-}" = "$expected_file" || { echo 'scoped credential file selector lost' >&2; exit 1; }
    test -f "$BEADS_CREDENTIALS_FILE"
else
    test -z "${BEADS_CREDENTIALS_FILE+x}" || { echo 'empty projection blocks bd scope credential source' >&2; exit 1; }
fi
printf 'ping\n' >> "$BEADS_DIR/calls"
`), 0o755); err != nil {
				t.Fatal(err)
			}
			cityConfig := fmt.Sprintf("[workspace]\nname = \"t\"\n[workspace.env]\nBD_BIN = %q\n[beads]\nprovider = \"bd\"\n[[rigs]]\nname = \"rig\"\npath = %q\n", bd, rig)
			if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(cityConfig), 0o644); err != nil {
				t.Fatal(err)
			}
			materializeBuiltinPacksForTest(t, city)
			for i, scope := range []string{city, rig} {
				host, port, database, user, origin := "city-db.invalid", 3307, "city_work", "city_user", "city_canonical"
				if i == 1 {
					host, port, database, user, origin = "rig-db.invalid", 3308, "rig_work", "rig_user", "explicit"
				}
				if tt.local {
					host = "127.0.0.1"
				}
				writeScopeBeadsMetadata(t, scope, fmt.Sprintf(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":%q,"dolt_server_host":%q,"dolt_server_port":%d,"dolt_server_user":%q}`, database, host, port, user))
				password, credentialFile := "", credentials
				if tt.host == host && tt.port == fmt.Sprint(port) && tt.socket == "" && !tt.local && tt.configHost == "" && tt.configPort == "" && tt.configSocket == "" {
					password = tt.password
				}
				if tt.defaultCredentials {
					credentialFile = ""
				}
				configHost, configPort := host, fmt.Sprint(port)
				if tt.configHost != "" {
					configHost = tt.configHost
				}
				if tt.configPort != "" {
					configPort = tt.configPort
				}
				endpointConfig := fmt.Sprintf("  host: %s\n  port: %s\n", configHost, configPort)
				if tt.configSocket != "" {
					endpointConfig = fmt.Sprintf("  socket: %s\n", tt.configSocket)
				}
				for name, content := range map[string]string{
					"config.yaml":       fmt.Sprintf("dolt:\n  mode: server\n%s  auto-start: %t\ngc:\n  endpoint_origin: %s\n  endpoint_status: verified\n", endpointConfig, tt.local, origin),
					".env":              "BEADS_DOLT_PASSWORD=" + database + "-synthetic\nBEADS_CREDENTIALS_FILE=" + credentials + "\n",
					"expected-password": password,
					"expected-file":     credentialFile,
					"expected-endpoint": fmt.Sprintf("%s:%d", host, port),
					"dolt-server.port":  "43123\n", // stale local bookkeeping must not redirect an operator password
					"dolt-server.pid":   "2147483647\n",
				} {
					if err := os.WriteFile(filepath.Join(scope, ".beads", name), []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := persistProviderScopeOwnership(city, scope, providerScopeIntent{Transport: "direct", Target: "external"}); err != nil {
					t.Fatal(err)
				}
				if err := markProviderScopeOwnershipReady(city, scope); err != nil {
					t.Fatal(err)
				}
			}
			if err := runProviderOwnedScopesLifecycleOp(city, "health"); err != nil {
				t.Fatal(err)
			}
			for _, scope := range []string{city, rig} {
				data, err := os.ReadFile(filepath.Join(scope, ".beads", "calls"))
				if err != nil || string(data) != "ping\n" {
					t.Fatalf("scope %s did not reach bd exactly once: %v", scope, err)
				}
			}
		})
	}
}
