//go:build dolt_integration

package dolt_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompactScriptRealDoltRemotePush(t *testing.T) {
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}

	root := repoRoot(t)
	cityPath := t.TempDir()
	dataDir := filepath.Join(cityPath, ".beads", "dolt")
	dbDir := filepath.Join(dataDir, "beads")
	remoteDir := filepath.Join(t.TempDir(), "remote")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir db dir: %v", err)
	}
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("mkdir remote dir: %v", err)
	}

	runDoltForCompactTest(t, doltPath, remoteDir, "init", "--name", "Gas City", "--email", "test@example.com")
	runDoltForCompactTest(t, doltPath, dbDir, "init", "--name", "Gas City", "--email", "test@example.com")
	runDoltForCompactTest(t, doltPath, dbDir, "sql", "-q",
		"CREATE TABLE beads (id int primary key, name varchar(20)); INSERT INTO beads VALUES (1, 'first');")
	runDoltForCompactTest(t, doltPath, dbDir, "add", ".")
	runDoltForCompactTest(t, doltPath, dbDir, "commit", "-m", "seed first bead")
	runDoltForCompactTest(t, doltPath, dbDir, "sql", "-q", "INSERT INTO beads VALUES (2, 'second');")
	runDoltForCompactTest(t, doltPath, dbDir, "commit", "-Am", "seed second bead")
	runDoltForCompactTest(t, doltPath, dbDir, "remote", "add", "origin", "file://"+remoteDir)
	runDoltForCompactTest(t, doltPath, dbDir, "push", "--force", "--set-upstream", "origin", "main")

	port, pid := startRealDoltServerForCompactTest(t, doltPath, dataDir)
	writeManagedRuntimeStateForScriptWithPID(t, cityPath, port, pid)
	waitForDoltServerQueryForCompactTest(t, doltPath, port)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", filepath.Join(root, "commands", "compact", "run.sh"))
	cmd.Env = append(filteredEnv(
		"PATH",
		"GC_CITY_PATH",
		"GC_PACK_DIR",
		"GC_DOLT_DATA_DIR",
		"GC_DOLT_PORT",
		"GC_DOLT_HOST",
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
		"GC_DOLT_MANAGED_LOCAL",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS",
		"GC_DOLT_COMPACT_CALL_TIMEOUT_SECS",
		"GC_DOLT_COMPACT_PUSH_TIMEOUT_SECS",
	),
		"PATH="+filepath.Dir(doltPath)+":"+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		fmt.Sprintf("GC_DOLT_PORT=%d", port),
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_DOLT_MANAGED_LOCAL=1",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=1",
		"GC_DOLT_COMPACT_CALL_TIMEOUT_SECS=20",
		"GC_DOLT_COMPACT_PUSH_TIMEOUT_SECS=20",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("compact script failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "remote=origin pushed compacted main") {
		t.Fatalf("compact output missing remote push success:\n%s", out)
	}

	localHead := doltServerHeadForCompactTest(t, doltPath, port)
	cloneParent := t.TempDir()
	runDoltForCompactTest(t, doltPath, cloneParent, "clone", "file://"+remoteDir, "cloned")
	remoteHead := doltHeadForCompactTest(t, doltPath, filepath.Join(cloneParent, "cloned"))
	if localHead != remoteHead {
		t.Fatalf("remote HEAD = %s, want local compacted HEAD %s", remoteHead, localHead)
	}
}

func runDoltForCompactTest(t *testing.T, doltPath, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, doltPath, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dolt %s failed in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func startRealDoltServerForCompactTest(t *testing.T, doltPath, dataDir string) (int, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocating dolt port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("closing dolt port probe: %v", err)
	}

	logPath := filepath.Join(dataDir, "sql-server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create dolt server log: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, doltPath, "sql-server",
		"-H", "127.0.0.1",
		"-P", fmt.Sprintf("%d", port),
		"--data-dir", dataDir,
		"--loglevel", "warning",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("start dolt sql-server: %v", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	cleanup := func() {
		cancel()
		select {
		case <-waitCh:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-waitCh
		}
		_ = logFile.Close()
	}
	t.Cleanup(cleanup)
	return port, cmd.Process.Pid
}

func waitForDoltServerQueryForCompactTest(t *testing.T, doltPath string, port int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastOut []byte
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cmd := exec.CommandContext(ctx, doltPath,
			"--host", "127.0.0.1",
			"--port", fmt.Sprintf("%d", port),
			"--user", "root",
			"--no-tls",
			"--use-db", "beads",
			"sql", "-q", "SELECT 1",
		)
		cmd.Env = append(filteredEnv("DOLT_CLI_PASSWORD"), "DOLT_CLI_PASSWORD=")
		lastOut, lastErr = cmd.CombinedOutput()
		cancel()
		if lastErr == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("dolt sql-server did not become query-ready on port %d: %v\n%s", port, lastErr, lastOut)
}

func doltHeadForCompactTest(t *testing.T, doltPath, dir string) string {
	t.Helper()
	out := runDoltForCompactTest(t, doltPath, dir, "log", "--oneline", "-n", "1")
	fields := strings.Fields(out)
	if len(fields) == 0 {
		t.Fatalf("empty dolt log output in %s", dir)
	}
	return fields[0]
}

func doltServerHeadForCompactTest(t *testing.T, doltPath string, port int) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, doltPath,
		"--host", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
		"--user", "root",
		"--no-tls",
		"--use-db", "beads",
		"sql", "-r", "csv", "-q", "SELECT commit_hash FROM dolt_log ORDER BY date DESC LIMIT 1",
	)
	cmd.Env = append(filteredEnv("DOLT_CLI_PASSWORD"), "DOLT_CLI_PASSWORD=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("query server HEAD: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[1]) == "" {
		t.Fatalf("unexpected server HEAD output:\n%s", out)
	}
	return strings.TrimSpace(lines[1])
}
