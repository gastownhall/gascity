package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
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
