package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestEnsureProjectIDCmdRequiresCityFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newEnsureProjectIDCmd(&stdout, &stderr)
	metadataPath := filepath.Join(t.TempDir(), ".beads", "metadata.json")
	cmd.SetArgs([]string{
		"--metadata", metadataPath,
		"--port", "3306",
		"--database", "hq",
	})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("ensure-project-id without --city succeeded")
	}
	if !strings.Contains(err.Error(), `required flag(s) "city" not set`) {
		t.Fatalf("ensure-project-id error = %v, want required --city", err)
	}
}

func TestResolveProviderOwnedProjectIdentityTarget(t *testing.T) {
	clearProxyEnvironment := func(t *testing.T) {
		t.Helper()
		for _, name := range []string{
			proxyendpoint.RootPathEnv,
			proxyendpoint.DoltDataDirEnv,
			proxyendpoint.SharedServerModeEnv,
			proxyendpoint.SharedServerDirEnv,
		} {
			t.Setenv(name, "")
		}
	}
	writeMetadata := func(t *testing.T, scope string, extra map[string]any) string {
		t.Helper()
		metadataPath := writeProjectIDMetadataFile(t, scope, "")
		data, err := os.ReadFile(metadataPath)
		if err != nil {
			t.Fatal(err)
		}
		var metadata map[string]any
		if err := json.Unmarshal(data, &metadata); err != nil {
			t.Fatal(err)
		}
		for key, value := range extra {
			metadata[key] = value
		}
		data, err = json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(metadataPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return metadataPath
	}
	writeConfig := func(t *testing.T, scope, config string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(scope, ".beads", "config.yaml"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSidecar := func(t *testing.T, scope, rootPath string) {
		t.Helper()
		data, err := json.Marshal(proxyendpoint.Sidecar{RootPath: rootPath})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(proxyendpoint.SidecarPath(filepath.Join(scope, ".beads")), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	makeProxyRecord := func(t *testing.T, root string, pid, port int) proxyendpoint.Record {
		t.Helper()
		rootID, err := proxyendpoint.RootID(root)
		if err != nil {
			t.Fatalf("identify proxy root: %v", err)
		}
		return proxyendpoint.Record{
			PID:         pid,
			Port:        port,
			UpstreamID:  "step30-test-upstream",
			Schema:      proxyendpoint.SchemaV2,
			Kind:        proxyendpoint.RecordKind,
			Birth:       proxyendpoint.BirthToken("test-boot", fmt.Sprint(pid)),
			RootID:      rootID,
			ControlPort: port + 1,
		}
	}
	writeProxyRecord := func(t *testing.T, root string, record proxyendpoint.Record) {
		t.Helper()
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(proxyendpoint.PIDPath(root), data, 0o600); err != nil {
			t.Fatalf("write proxy endpoint record: %v", err)
		}
	}
	processTable := func(record proxyendpoint.Record, alive bool, argv []string, birth string) proxyendpoint.ProcessTable {
		return proxyendpoint.ProcessTable{
			Alive: func(pid int) bool { return alive && pid == record.PID },
			Argv:  func(int) ([]string, error) { return argv, nil },
			Birth: func(int) (string, error) { return birth, nil },
		}
	}
	proxyArgv := func(root string) []string {
		return []string{"bd", proxyendpoint.ChildVerb, proxyendpoint.RootFlag, root, proxyendpoint.IdleTimeoutFlag, "0"}
	}
	proxiedScope := func(t *testing.T, rootPath string) (string, string) {
		t.Helper()
		scope := t.TempDir()
		metadataPath := writeMetadata(t, scope, map[string]any{"dolt_mode": "proxied-server"})
		writeConfig(t, scope, "issue_prefix: gc\n")
		writeSidecar(t, scope, rootPath)
		return scope, metadataPath
	}

	t.Run("direct local reads bd server port", func(t *testing.T) {
		clearProxyEnvironment(t)
		scope := t.TempDir()
		metadataPath := writeMetadata(t, scope, nil)
		writeConfig(t, scope, "issue_prefix: gc\n")
		listener := listenOnRandomPort(t)
		t.Cleanup(func() {
			if err := listener.Close(); err != nil {
				t.Errorf("close bd server fixture listener: %v", err)
			}
		})
		port := fmt.Sprintf("%d", listener.Addr().(*net.TCPAddr).Port)
		beadsDir := filepath.Join(scope, ".beads")
		if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.pid"), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte(port+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		target, err := resolveProviderOwnedProjectIdentityTargetWithProcessTable(metadataPath, scope, "hq", "provider-user", proxyendpoint.ProcessTable{})
		if err != nil {
			t.Fatalf("resolve provider-owned target: %v", err)
		}
		if target.Host != "127.0.0.1" || target.Port != port || target.Database != "hq" || target.User != "provider-user" {
			t.Fatalf("target = %+v, want local bd endpoint 127.0.0.1:%s/hq as provider-user", target, port)
		}
	})

	t.Run("direct external socket reads bd binding", func(t *testing.T) {
		clearProxyEnvironment(t)
		scope := t.TempDir()
		metadataPath := writeMetadata(t, scope, map[string]any{
			"dolt_server_socket": "/run/dolt/provider.sock",
		})
		writeConfig(t, scope, "issue_prefix: gc\n")
		target, err := resolveProviderOwnedProjectIdentityTargetWithProcessTable(metadataPath, scope, "hosted", "", proxyendpoint.ProcessTable{})
		if err != nil {
			t.Fatalf("resolve provider-owned target: %v", err)
		}
		if target.Socket != "/run/dolt/provider.sock" || target.Database != "hosted" {
			t.Fatalf("target = %+v, want socket endpoint and hosted database", target)
		}
	})

	t.Run("proxied local reads live proxy port", func(t *testing.T) {
		clearProxyEnvironment(t)
		scope, metadataPath := proxiedScope(t, "relocated/proxy-root")
		beadsDir := filepath.Join(scope, ".beads")
		root := filepath.Join(beadsDir, "relocated", "proxy-root")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		record := makeProxyRecord(t, root, 4242, 45123)
		writeProxyRecord(t, root, record)
		target, err := resolveProviderOwnedProjectIdentityTargetWithProcessTable(
			metadataPath, scope, "hq", "", processTable(record, true, proxyArgv(root), record.Birth),
		)
		if err != nil {
			t.Fatalf("resolve provider-owned target: %v", err)
		}
		if target.Host != proxyendpoint.Host || target.Port != "45123" || target.Database != "hq" {
			t.Fatalf("target = %+v, want canonical endpoint %s:45123/hq", target, proxyendpoint.Host)
		}
	})

	t.Run("proxied local follows ProviderRoot environment precedence", func(t *testing.T) {
		clearProxyEnvironment(t)
		scope, metadataPath := proxiedScope(t, "persisted/proxy-root")
		beadsDir := filepath.Join(scope, ".beads")
		persistedRoot := filepath.Join(beadsDir, "persisted", "proxy-root")
		envRoot := filepath.Join(t.TempDir(), "operator-proxy-root")
		for _, root := range []string{persistedRoot, envRoot} {
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		persistedRecord := makeProxyRecord(t, persistedRoot, 4243, 45124)
		writeProxyRecord(t, persistedRoot, persistedRecord)
		envRecord := makeProxyRecord(t, envRoot, 4244, 45125)
		writeProxyRecord(t, envRoot, envRecord)
		t.Setenv(proxyendpoint.RootPathEnv, envRoot)
		target, err := resolveProviderOwnedProjectIdentityTargetWithProcessTable(
			metadataPath, scope, "hq", "", processTable(envRecord, true, proxyArgv(envRoot), envRecord.Birth),
		)
		if err != nil {
			t.Fatalf("resolve provider-owned target: %v", err)
		}
		if target.Host != proxyendpoint.Host || target.Port != "45125" {
			t.Fatalf("target = %+v, want environment-selected endpoint %s:45125", target, proxyendpoint.Host)
		}
	})

	t.Run("proxied local refuses a copied wrong-root record", func(t *testing.T) {
		clearProxyEnvironment(t)
		scope, metadataPath := proxiedScope(t, "proxy-root")
		root := filepath.Join(scope, ".beads", "proxy-root")
		foreignRoot := filepath.Join(t.TempDir(), "original-proxy-root")
		for _, path := range []string{root, foreignRoot} {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		record := makeProxyRecord(t, foreignRoot, 4245, 45126)
		writeProxyRecord(t, root, record)
		aliveCalled := false
		table := proxyendpoint.ProcessTable{
			Alive: func(int) bool { aliveCalled = true; return true },
			Argv:  func(int) ([]string, error) { return proxyArgv(root), nil },
			Birth: func(int) (string, error) { return record.Birth, nil },
		}
		if _, err := resolveProviderOwnedProjectIdentityTargetWithProcessTable(metadataPath, scope, "hq", "", table); err == nil || !strings.Contains(err.Error(), "not_ours") {
			t.Fatalf("resolve copied proxy record error = %v, want canonical not_ours refusal", err)
		}
		if aliveCalled {
			t.Fatal("process liveness was checked for a copied wrong-root record")
		}
	})

	for _, tc := range []struct {
		name     string
		alive    bool
		argv     func(root string) []string
		birth    func(record proxyendpoint.Record) string
		wantText string
	}{
		{
			name:     "dead generation",
			alive:    false,
			argv:     proxyArgv,
			birth:    func(record proxyendpoint.Record) string { return record.Birth },
			wantText: "dead",
		},
		{
			name:     "foreign process",
			alive:    true,
			argv:     func(string) []string { return []string{"unrelated-process"} },
			birth:    func(record proxyendpoint.Record) string { return record.Birth },
			wantText: "foreign_process",
		},
		{
			name:     "restarted generation",
			alive:    true,
			argv:     proxyArgv,
			birth:    func(proxyendpoint.Record) string { return "different-generation" },
			wantText: "birth_mismatch",
		},
	} {
		t.Run("proxied local refuses "+tc.name, func(t *testing.T) {
			clearProxyEnvironment(t)
			scope, metadataPath := proxiedScope(t, "proxy-root")
			root := filepath.Join(scope, ".beads", "proxy-root")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			record := makeProxyRecord(t, root, 4246, 45127)
			writeProxyRecord(t, root, record)
			table := processTable(record, tc.alive, tc.argv(root), tc.birth(record))
			if _, err := resolveProviderOwnedProjectIdentityTargetWithProcessTable(metadataPath, scope, "hq", "", table); err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("resolve non-live proxy error = %v, want canonical %s refusal", err, tc.wantText)
			}
		})
	}
}

func writeProjectIDMetadataFile(t *testing.T, scopeRoot string, projectID string) string {
	t.Helper()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{
		"backend":       "dolt",
		"database":      "dolt",
		"dolt_database": "hq",
		"dolt_mode":     "server",
	}
	if projectID != "" {
		meta["project_id"] = projectID
	}
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	if err := os.WriteFile(metadataPath, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	return metadataPath
}

func startProjectIDTestServer(t *testing.T, setupQueries ...string) (string, func()) {
	t.Helper()
	repoDir := filepath.Join(t.TempDir(), "hq")
	_, port, _, cleanup := startPasswordedDoltServer(t, repoDir, setupQueries...)
	return fmt.Sprintf("%d", port), cleanup
}

func seedDatabaseProjectIDQueries(projectID string) []string {
	return []string{
		"CREATE TABLE IF NOT EXISTS metadata (`key` VARCHAR(255) PRIMARY KEY, value LONGTEXT)",
		fmt.Sprintf("INSERT INTO metadata (`key`, value) VALUES ('_project_id', '%s') ON DUPLICATE KEY UPDATE value = VALUES(value)", projectID),
	}
}

// nativeStorageFixtureBootTimeout bounds fixture cold-boot of a passworded
// Dolt server before beads.OpenNativeStorage. Isolated cost is ~4s; 60s
// leaves headroom for shard-parallel host contention (ga-uswva7).
const nativeStorageFixtureBootTimeout = 60 * time.Second

// TestNativeStorageFixtureBootTimeoutSurvivesShardContention guards against
// ga-uswva7: a 15s budget left ~11s margin over the ~4s isolated cold-boot,
// which shard-parallel contention intermittently exceeded.
func TestNativeStorageFixtureBootTimeoutSurvivesShardContention(t *testing.T) {
	const minSafeBootTimeout = 60 * time.Second
	if nativeStorageFixtureBootTimeout < minSafeBootTimeout {
		t.Fatalf("nativeStorageFixtureBootTimeout = %s, want >= %s (see ga-uswva7: isolated cold-boot ~4s, shard-parallel contention intermittently exceeded 15s)",
			nativeStorageFixtureBootTimeout, minSafeBootTimeout)
	}
}

func startPasswordedDoltServer(t *testing.T, repoDir string, setupQueries ...string) (string, int, int, func()) {
	t.Helper()
	skipSlowCmdGCTest(t, "requires a real Dolt server; run make test-cmd-gc-process for full coverage")
	configureTestDoltIdentityEnv(t)

	doltPath := os.Getenv("GC_DOLT_REAL_BINARY")
	var err error
	if doltPath == "" {
		doltPath, err = exec.LookPath("dolt")
		if err != nil {
			t.Skip("dolt not installed")
		}
	}
	if repoDir == "" {
		repoDir = t.TempDir()
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", repoDir, err)
	}

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(doltPath, args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("dolt %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	run("init")
	for _, query := range setupQueries {
		run("sql", "-q", query)
	}
	run("sql", "-q", "CREATE USER 'root'@'%' IDENTIFIED BY 'secret'; GRANT ALL ON *.* TO 'root'@'%';")

	port := reserveRandomTCPPort(t)
	cmd := exec.Command(doltPath, "sql-server", "--host", "127.0.0.1", "--port", fmt.Sprintf("%d", port), "--allow-cleartext-passwords", "--loglevel=warning")
	cmd.Dir = repoDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start passworded dolt sql-server: %v", err)
	}

	t.Setenv("GC_DOLT_PASSWORD", "secret")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := managedDoltQueryProbeDirect("127.0.0.1", fmt.Sprintf("%d", port), "root"); err == nil {
			cleanup := func() {
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				_, _ = cmd.Process.Wait()
			}
			return repoDir, port, cmd.Process.Pid, cleanup
		}
		time.Sleep(250 * time.Millisecond)
	}

	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	t.Fatalf("passworded dolt sql-server on %d did not become query-ready", port)
	return "", 0, 0, func() {}
}

func TestManagedDoltHealthCheckWithPasswordUsesDirectHelpersAgainstRealServer(t *testing.T) {
	binDir := t.TempDir()
	realDolt, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}
	t.Setenv("GC_DOLT_REAL_BINARY", realDolt)
	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, port, _, cleanup := startPasswordedDoltServer(t, "")
	defer cleanup()

	report, err := managedDoltHealthCheck("0.0.0.0", fmt.Sprintf("%d", port), "root", true)
	if err != nil {
		t.Fatalf("managedDoltHealthCheck() error = %v", err)
	}
	if !report.QueryReady || report.ReadOnly != "false" {
		t.Fatalf("managedDoltHealthCheck() = %+v, want query-ready writable server", report)
	}
	if report.ConnectionCount == "" {
		t.Fatalf("managedDoltHealthCheck() = %+v, want connection count", report)
	}
}

func TestManagedDoltWaitReadyWithPasswordUsesDirectQueryProbe(t *testing.T) {
	binDir := t.TempDir()
	realDolt, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}
	t.Setenv("GC_DOLT_REAL_BINARY", realDolt)
	fakeDolt := filepath.Join(binDir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	repoDir, port, pid, cleanup := startPasswordedDoltServer(t, "")
	defer cleanup()

	report, err := waitForManagedDoltReady(repoDir, "0.0.0.0", fmt.Sprintf("%d", port), "root", pid, 5*time.Second, false)
	if err != nil {
		t.Fatalf("waitForManagedDoltReady() error = %v", err)
	}
	if !report.Ready || !report.PIDAlive {
		t.Fatalf("waitForManagedDoltReady() = %+v, want ready pid_alive", report)
	}
}

func TestRecoverManagedDoltProcessWithPasswordReusesHealthyRealServer(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")
	cityPath := t.TempDir()
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("resolveManagedDoltRuntimeLayout: %v", err)
	}
	if err := os.MkdirAll(layout.DataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(data dir): %v", err)
	}

	_, port, pid, cleanup := startPasswordedDoltServer(t, layout.DataDir, "CREATE DATABASE IF NOT EXISTS `hq`")
	defer cleanup()
	t.Cleanup(func() {
		if state, err := readDoltRuntimeStateFile(layout.StateFile); err == nil && state.PID > 0 {
			_ = terminateManagedDoltPID("", state.PID)
		}
	})

	if err := os.MkdirAll(filepath.Dir(layout.PIDFile), 0o755); err != nil {
		t.Fatalf("MkdirAll(runtime dir): %v", err)
	}
	if err := os.WriteFile(layout.PIDFile, []byte(fmt.Sprintf("%d\n", pid)), 0o644); err != nil {
		t.Fatalf("WriteFile(pid): %v", err)
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   true,
		PID:       pid,
		Port:      port,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltRuntimeStateFile: %v", err)
	}

	report, err := recoverManagedDoltProcess(cityPath, "127.0.0.1", fmt.Sprintf("%d", port), "root", "warning", 10*time.Second)
	if err != nil {
		t.Fatalf("recoverManagedDoltProcess() error = %v", err)
	}
	if !report.Ready || !report.Healthy {
		t.Fatalf("recoverManagedDoltProcess() = %+v, want ready healthy", report)
	}
	if !report.HadPID {
		t.Fatalf("recoverManagedDoltProcess() HadPID = false, want true")
	}
	if report.PID != pid {
		t.Fatalf("recoverManagedDoltProcess() pid = %d, want reused pid %d", report.PID, pid)
	}
	if report.Port != port {
		t.Fatalf("recoverManagedDoltProcess() port = %d, want %d", report.Port, port)
	}
	if report.Restarted {
		t.Fatalf("recoverManagedDoltProcess() Restarted = true, want false")
	}
}

func TestProjectIdentityL3AdapterContractAndManagedComposition(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")
	cityDir := t.TempDir()
	repoDir := filepath.Join(cityDir, "hq")
	_, port, _, cleanup := startPasswordedDoltServer(t, repoDir)
	defer cleanup()
	portString := fmt.Sprintf("%d", port)

	db, err := managedDoltOpenDatabase("127.0.0.1", portString, "root", "hq")
	if err != nil {
		t.Fatalf("managedDoltOpenDatabase: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("PingContext with password: %v", err)
	}

	runProjectIdentityL3SeedContract(
		t,
		func(ctx context.Context) (string, bool, error) {
			return readDatabaseProjectID(ctx, db)
		},
		func(ctx context.Context, projectID string) (bool, error) {
			return seedDatabaseProjectID(ctx, db, projectID)
		},
	)

	if _, err := db.ExecContext(ctx, "DELETE FROM metadata WHERE `key` = '_project_id'"); err != nil {
		t.Fatalf("delete database _project_id: %v", err)
	}
	if projectID, ok, err := readDatabaseProjectID(ctx, db); err != nil || ok || projectID != "" {
		t.Fatalf("L3 after contract reset = (%q, %v, %v), want absent", projectID, ok, err)
	}

	scopeRoot := filepath.Join(cityDir, "rigs", "demo")
	metadataPath := writeProjectIDMetadataFile(t, scopeRoot, "composition-id")
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, scopeRoot, "composition-id"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	recorder := &projectIdentityApplyRecordingRecorder{}
	report, err := ensureManagedDoltProjectIDWithRecorder(metadataPath, "127.0.0.1", portString, "root", "hq", cityDir, recorder)
	if err != nil {
		t.Fatalf("ensureManagedDoltProjectIDWithRecorder: %v", err)
	}
	wantReport := managedDoltProjectIDReport{
		ProjectID:       "composition-id",
		DatabaseUpdated: true,
		Source:          "l3-seed",
		Layer:           "l1",
	}
	if report != wantReport {
		t.Fatalf("report = %+v, want %+v", report, wantReport)
	}
	assertProjectIdentityApplyStampedEvents(t, recorder.records, []projectIdentityApplyStampedEvent{
		{source: "cache_repair", layer: "L3", newID: "composition-id"},
	})
	l1, l1OK, err := contract.ReadProjectIdentity(fsys.OSFS{}, scopeRoot)
	if err != nil {
		t.Fatalf("ReadProjectIdentity: %v", err)
	}
	l2, err := readManagedMetadataProjectID(metadataPath)
	if err != nil {
		t.Fatalf("readManagedMetadataProjectID: %v", err)
	}
	l3, l3OK, err := readDatabaseProjectID(ctx, db)
	if err != nil {
		t.Fatalf("readDatabaseProjectID: %v", err)
	}
	if !l1OK || !l3OK || l1 != "composition-id" || l2 != "composition-id" || l3 != "composition-id" {
		t.Fatalf("composition state = (L1:%q/%v L2:%q L3:%q/%v), want composition-id in all layers", l1, l1OK, l2, l3, l3OK)
	}
}
