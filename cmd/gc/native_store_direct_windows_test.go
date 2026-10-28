//go:build windows

package main

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// bdDirectScope describes what gc init with the direct transport leaves in a
// scope.
type bdDirectScope struct {
	doltMode  string
	projectID string
	// serverPID and serverPort are bd's record of the server it started;
	// no record is written when serverPID is 0.
	serverPID  int
	serverPort string
	// externalPort binds the scope to an external server instead.
	externalPort int
	ready        bool
}

// writeBdDirectScope lays out bd's own config.yaml, metadata, and server
// record in scope, and journals the scope as provider-owned.
func writeBdDirectScope(t *testing.T, city, scope string, s bdDirectScope) {
	t.Helper()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{
		"database": "dolt", "backend": "dolt", "dolt_mode": s.doltMode, "dolt_database": "hq", "project_id": s.projectID,
	}
	if s.externalPort != 0 {
		meta["dolt_server_host"] = "127.0.0.1"
		meta["dolt_server_port"] = s.externalPort
	}
	metadata, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"config.yaml":   "issue_prefix: gc\n",
		"metadata.json": string(metadata),
	}
	if s.serverPID != 0 {
		files["dolt-server.pid"] = strconv.Itoa(s.serverPID) + "\n"
		files["dolt-server.port"] = s.serverPort + "\n"
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(beadsDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	intent := providerScopeIntent{Transport: "direct", Target: "local"}
	if s.doltMode == "proxied-server" {
		intent.Transport = "proxied"
	}
	if s.externalPort != 0 {
		intent.Target = "external"
	}
	if err := persistProviderScopeOwnership(city, scope, intent); err != nil {
		t.Fatalf("persistProviderScopeOwnership: %v", err)
	}
	if s.ready {
		if err := markProviderScopeOwnershipReady(city, scope); err != nil {
			t.Fatalf("markProviderScopeOwnershipReady: %v", err)
		}
	}
}

// listenLocalPort returns a loopback port that accepts connections until the
// test ends.
func listenLocalPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// writeLiveManagedRuntime records a running gc-managed Dolt for city, on a
// port that answers.
func writeLiveManagedRuntime(t *testing.T, city string) {
	t.Helper()
	dir := filepath.Join(city, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	state, err := json.Marshal(map[string]any{
		"running": true, "pid": os.Getpid(), "port": listenLocalPortNumber(t), "data_dir": filepath.Join(city, ".beads", "dolt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dolt-state.json"), state, 0o644); err != nil {
		t.Fatal(err)
	}
}

func listenLocalPortNumber(t *testing.T) int {
	t.Helper()
	port, err := strconv.Atoi(listenLocalPort(t))
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// closedLocalPort returns a loopback port nothing listens on.
func closedLocalPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close()
	return port
}

// exitedPID returns the PID of a process that has already exited.
func exitedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("cmd.exe", "/c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.ProcessState.Pid()
}

// A provider-owned direct scope's config is bd's, so it never resolves as
// canonical; the identity check reads its project_id from the server bd runs.
func TestIdentityCheckProbesBdsServerForAProviderOwnedDirectScope(t *testing.T) {
	repoDir := filepath.Join(t.TempDir(), "hq")
	_, port, pid, cleanup := startPasswordedDoltServer(t, repoDir)
	defer cleanup()

	city := t.TempDir()
	writeBdDirectScope(t, city, city, bdDirectScope{doltMode: "server", projectID: "p1", serverPID: pid, serverPort: strconv.Itoa(port), ready: true})

	target, ok, err := preflightProbeTarget(city, city)
	if err != nil || !ok {
		t.Fatalf("preflightProbeTarget = (%+v, %v, %v), want bd's server", target, ok, err)
	}
	if target.Host != "127.0.0.1" || target.Port != strconv.Itoa(port) || target.Database != "hq" || target.DoltMode != "server" || target.External {
		t.Fatalf("probe target = %+v, want the local server-mode database hq on port %d", target, port)
	}
}

// A stale bd record must not become admissible just because the recorded PID
// and the recorded port are both live independently.
func TestIdentityCheckRejectsProviderOwnedDirectScopeWhenRecordedPIDDoesNotOwnPort(t *testing.T) {
	repoDir := filepath.Join(t.TempDir(), "hq")
	_, port, serverPID, cleanup := startPasswordedDoltServer(t, repoDir, seedDatabaseProjectIDQueries("p-unrelated")...)
	defer cleanup()
	holderRepo := filepath.Join(t.TempDir(), "holder")
	_, holderPort, recordedPID, cleanupHolder := startPasswordedDoltServer(t, holderRepo)
	defer cleanupHolder()
	t.Logf("real server PID=%d on port %d; deliberately unrelated recorded Dolt PID=%d on port %d", serverPID, port, recordedPID, holderPort)

	for _, tc := range []struct {
		name string
		rig  bool
	}{
		{name: "city"},
		{name: "rig", rig: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			scope := city
			if tc.rig {
				scope = filepath.Join(city, "rig")
			}
			writeBdDirectScope(t, city, scope, bdDirectScope{
				doltMode: "server", projectID: "p-scope", serverPID: recordedPID,
				serverPort: strconv.Itoa(port), ready: true,
			})

			target, accepted, err := preflightProbeTarget(city, scope)
			databaseID, read, readErr := preflightDatabaseProjectIDReader(city)(scope)
			t.Logf("accepted=%v target=%+v err=%v databaseID=%q read=%v readErr=%v", accepted, target, err, databaseID, read, readErr)
			if accepted || read {
				t.Fatalf("identity probe accepted Dolt pid %d on port %d even though .beads/dolt-server.pid names unrelated Dolt pid %d", serverPID, port, recordedPID)
			}
			if err != nil || readErr != nil {
				t.Fatalf("stale record declined with errors: preflight=%v read=%v", err, readErr)
			}
		})
	}
}

// Every other scope keeps the canonical target as its only endpoint.
func TestIdentityCheckProbesNoOtherNonCanonicalScope(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope func(t *testing.T) bdDirectScope
		// disown removes the ownership journal after the scope is written.
		disown bool
		// managed also records a live gc-managed Dolt for the city.
		managed bool
	}{
		{name: "not provider-owned", disown: true, scope: func(t *testing.T) bdDirectScope {
			return bdDirectScope{doltMode: "server", serverPID: os.Getpid(), serverPort: listenLocalPort(t), ready: true}
		}},
		{name: "still initializing", scope: func(t *testing.T) bdDirectScope {
			return bdDirectScope{doltMode: "server", serverPID: os.Getpid(), serverPort: listenLocalPort(t)}
		}},
		{name: "server process gone", scope: func(t *testing.T) bdDirectScope {
			return bdDirectScope{doltMode: "server", serverPID: exitedPID(t), serverPort: listenLocalPort(t), ready: true}
		}},
		{name: "port not answering", scope: func(t *testing.T) bdDirectScope {
			return bdDirectScope{doltMode: "server", serverPID: os.Getpid(), serverPort: closedLocalPort(t), ready: true}
		}},
		{name: "server process is not dolt", scope: func(t *testing.T) bdDirectScope {
			return bdDirectScope{doltMode: "server", serverPID: os.Getpid(), serverPort: listenLocalPort(t), ready: true}
		}},
		{name: "embedded mode", scope: func(t *testing.T) bdDirectScope {
			return bdDirectScope{doltMode: "embedded", serverPID: os.Getpid(), serverPort: listenLocalPort(t), ready: true}
		}},
		{name: "proxied transport", scope: func(t *testing.T) bdDirectScope {
			return bdDirectScope{doltMode: "proxied-server", serverPID: os.Getpid(), serverPort: listenLocalPort(t), ready: true}
		}},
		{name: "external server", scope: func(t *testing.T) bdDirectScope {
			return bdDirectScope{doltMode: "server", externalPort: listenLocalPortNumber(t), ready: true}
		}},
		{name: "resolves to gc's managed server", managed: true, scope: func(t *testing.T) bdDirectScope {
			return bdDirectScope{doltMode: "server", serverPID: os.Getpid(), serverPort: listenLocalPort(t), ready: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			s := tc.scope(t)
			s.projectID = "p1"
			writeBdDirectScope(t, city, city, s)
			if tc.managed {
				writeLiveManagedRuntime(t, city)
			}
			if tc.disown {
				if err := os.RemoveAll(filepath.Join(city, ".gc")); err != nil {
					t.Fatal(err)
				}
			}
			if target, ok, err := preflightProbeTarget(city, city); ok || err != nil {
				t.Fatalf("preflightProbeTarget = (%+v, %v, %v), want no endpoint", target, ok, err)
			}
		})
	}
}

// Against a real Dolt server: the identity check reads the project_id of the
// database bd's server holds, not the one the scope's metadata claims, so a
// mismatch between them still surfaces.
func TestIdentityCheckReadsProjectIDFromBdsDirectServer(t *testing.T) {
	repoDir := filepath.Join(t.TempDir(), "hq")
	_, port, pid, cleanup := startPasswordedDoltServer(t, repoDir, seedDatabaseProjectIDQueries("p-real")...)
	defer cleanup()
	city := t.TempDir()
	writeBdDirectScope(t, city, city, bdDirectScope{doltMode: "server", projectID: "p-metadata", serverPID: pid, serverPort: strconv.Itoa(port), ready: true})

	projectID, ok, err := preflightDatabaseProjectIDReader(city)(city)
	if err != nil || !ok || projectID != "p-real" {
		t.Fatalf("database project_id = (%q, %v, %v), want p-real from bd's server", projectID, ok, err)
	}
}
