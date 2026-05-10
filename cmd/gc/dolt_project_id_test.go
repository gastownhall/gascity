package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnsureManagedDoltProjectIDGeneratesLocalIdentityWhenMetadataAndDatabaseMissing(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")
	doltPath := os.Getenv("GC_DOLT_REAL_BINARY")
	var err error
	if doltPath == "" {
		doltPath, err = exec.LookPath("dolt")
		if err != nil {
			t.Skip("dolt not installed")
		}
	}
	bdPath := waitTestRealBDPath(t)

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeBuiltinPacks(cityDir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}

	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitConfig := filepath.Join(homeDir, ".gitconfig")
	if err := os.WriteFile(gitConfig, []byte("[user]\n\tname = Test User\n\temail = test@example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("GIT_CONFIG_GLOBAL", gitConfig)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")
	t.Setenv("PATH", strings.Join([]string{filepath.Dir(bdPath), filepath.Dir(doltPath), os.Getenv("PATH")}, string(os.PathListSeparator)))

	if err := ensureBeadsProvider(cityDir); err != nil {
		t.Fatalf("ensureBeadsProvider: %v", err)
	}
	t.Cleanup(func() {
		_ = shutdownBeadsProvider(cityDir)
	})
	if err := initAndHookDir(cityDir, cityDir, "gc"); err != nil {
		t.Fatalf("initAndHookDir(city): %v", err)
	}

	portData, err := os.ReadFile(filepath.Join(cityDir, ".beads", "dolt-server.port"))
	if err != nil {
		t.Fatalf("ReadFile(dolt-server.port): %v", err)
	}
	port := strings.TrimSpace(string(portData))
	if port == "" {
		t.Fatal("dolt-server.port empty")
	}

	metadataPath := filepath.Join(cityDir, ".beads", "metadata.json")
	metadataData, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("ReadFile(metadata.json): %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metadataData, &meta); err != nil {
		t.Fatalf("Unmarshal(metadata.json): %v", err)
	}
	delete(meta, "project_id")
	patched, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent(metadata.json): %v", err)
	}
	patched = append(patched, '\n')
	if err := os.WriteFile(metadataPath, patched, 0o644); err != nil {
		t.Fatalf("WriteFile(metadata.json): %v", err)
	}

	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%s)/hq", port))
	if err != nil {
		t.Fatalf("sql.Open(hq): %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "DELETE FROM metadata WHERE `key` = '_project_id'"); err != nil {
		t.Fatalf("delete database _project_id: %v", err)
	}

	report, err := ensureManagedDoltProjectID(metadataPath, "127.0.0.1", port, "root", "hq")
	if err != nil {
		t.Fatalf("ensureManagedDoltProjectID: %v", err)
	}
	if report.Source != "generated" {
		t.Fatalf("report.Source = %q, want generated", report.Source)
	}
	if !report.MetadataUpdated {
		t.Fatal("report.MetadataUpdated = false, want true")
	}
	if !report.DatabaseUpdated {
		t.Fatal("report.DatabaseUpdated = false, want true")
	}
	if !strings.HasPrefix(report.ProjectID, "gc-local-") {
		t.Fatalf("report.ProjectID = %q, want gc-local-*", report.ProjectID)
	}

	metadataData, err = os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("ReadFile(metadata.json): %v", err)
	}
	meta = map[string]any{}
	if err := json.Unmarshal(metadataData, &meta); err != nil {
		t.Fatalf("Unmarshal(metadata.json): %v", err)
	}
	if got := strings.TrimSpace(fmt.Sprint(meta["project_id"])); got != report.ProjectID {
		t.Fatalf("metadata project_id = %q, want %q", got, report.ProjectID)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	var databaseProjectID string
	if err := db.QueryRowContext(ctx2, "SELECT value FROM metadata WHERE `key` = '_project_id'").Scan(&databaseProjectID); err != nil {
		t.Fatalf("read database _project_id: %v", err)
	}
	if got := strings.TrimSpace(databaseProjectID); got != report.ProjectID {
		t.Fatalf("database _project_id = %q, want %q", got, report.ProjectID)
	}
}

func startPasswordedDoltServer(t *testing.T, repoDir string, setupQueries ...string) (string, int, int, func()) {
	t.Helper()
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

	_, port, pid, cleanup := startPasswordedDoltServer(t, layout.DataDir)
	defer cleanup()
	t.Cleanup(func() {
		if state, err := readDoltRuntimeStateFile(layout.StateFile); err == nil && state.PID > 0 {
			_ = terminateManagedDoltPID(state.PID)
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

func TestEnsureManagedDoltProjectIDGeneratesLocalIdentityWithPasswordedServer(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed dolt server; run make test-cmd-gc-process for full coverage")
	cityDir := t.TempDir()
	metadataPath := filepath.Join(cityDir, ".beads", "metadata.json")
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	repoDir := filepath.Join(cityDir, ".beads", "dolt")
	_, port, _, cleanup := startPasswordedDoltServer(t, repoDir, "CREATE DATABASE IF NOT EXISTS `hq`; USE `hq`; CREATE TABLE IF NOT EXISTS metadata (`key` VARCHAR(255) PRIMARY KEY, value LONGTEXT);")
	defer cleanup()

	db, err := managedDoltOpenDatabase("127.0.0.1", fmt.Sprintf("%d", port), "root", "hq")
	if err != nil {
		t.Fatalf("managedDoltOpenDatabase: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("DELETE FROM metadata WHERE `key` = '_project_id'"); err != nil {
		t.Fatalf("delete database _project_id: %v", err)
	}

	report, err := ensureManagedDoltProjectID(metadataPath, "127.0.0.1", fmt.Sprintf("%d", port), "root", "hq")
	if err != nil {
		t.Fatalf("ensureManagedDoltProjectID: %v", err)
	}
	if report.Source != "generated" {
		t.Fatalf("report.Source = %q, want generated", report.Source)
	}
	if !report.MetadataUpdated || !report.DatabaseUpdated {
		t.Fatalf("report = %+v, want both metadata and database updated", report)
	}
	if !strings.HasPrefix(report.ProjectID, "gc-local-") {
		t.Fatalf("report.ProjectID = %q, want gc-local-*", report.ProjectID)
	}

	var databaseProjectID string
	if err := db.QueryRow("SELECT value FROM metadata WHERE `key` = '_project_id'").Scan(&databaseProjectID); err != nil {
		t.Fatalf("read database _project_id: %v", err)
	}
	if strings.TrimSpace(databaseProjectID) != report.ProjectID {
		t.Fatalf("database _project_id = %q, want %q", databaseProjectID, report.ProjectID)
	}
}
