//go:build integration

package integration

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/doctor"
)

const (
	bdInitTimeout          = 15 * time.Second
	doltServerStartupLimit = 10 * time.Second
)

// TestBdStoreConformance runs the beads conformance suite against BdStore
// backed by a real dolt server. This proves the full stack works:
// dolt server → bd CLI → BdStore → beads.Store interface.
//
// Each subtest gets a fresh database directory where bd auto-starts a
// dolt server on a unique port. This avoids port conflicts and lets bd
// manage the server lifecycle.
//
// Requires: Dolt and bd binaries configured via PATH or the integration
// override env vars.
func TestBdStoreConformance(t *testing.T) {
	requireDoltIntegration(t)
	env := newIsolatedToolEnv(t, true)

	rootDir := t.TempDir()
	doltDataDir := filepath.Join(rootDir, "dolt")
	workspacesDir := filepath.Join(rootDir, "workspaces")
	serverPort := startSharedDoltServer(t, env, doltDataDir)
	var dbCounter atomic.Int64

	// Factory: each call creates a fresh workspace bound to the shared Dolt
	// server. This avoids the slow startup/shutdown tail from embedded local
	// server mode and keeps the conformance suite within CI time limits.
	newStore := func() beads.Store {
		n := dbCounter.Add(1)
		prefix := fmt.Sprintf("ct%d", n)

		// Create isolated workspace directory.
		wsDir := filepath.Join(workspacesDir, fmt.Sprintf("ws-%d", n))
		if err := os.MkdirAll(wsDir, 0o755); err != nil {
			t.Fatalf("creating workspace: %v", err)
		}

		// Initialize git repo (bd init requires it).
		gitCmd := exec.Command("git", "init", "--quiet")
		gitCmd.Dir = wsDir
		if out, err := gitCmd.CombinedOutput(); err != nil {
			t.Fatalf("git init: %v: %s", err, out)
		}

		runBDInit(t, env, wsDir, prefix, serverPort)

		configureCustomTypes(t, env, wsDir, doctor.RequiredCustomTypes)

		return beads.NewBdStore(wsDir, beads.ExecCommandRunner())
	}

	// Run conformance suite. We skip RunSequentialIDTests because BdStore
	// uses bd's ID format (prefix-XXXX), not gc-N sequential format.
	beadstest.RunStoreTests(t, newStore)
	beadstest.RunMetadataTests(t, newStore)
}

// startSharedDoltServer starts one explicit Dolt SQL server for the test and
// returns its port. Using a shared server keeps bd commands fast and avoids
// the embedded local-server shutdown delays seen in CI.
func startSharedDoltServer(t *testing.T, env []string, dataDir string) string {
	t.Helper()

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("creating dolt data dir: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocating dolt port: %v", err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatalf("closing dolt port probe: %v", err)
	}

	logPath := filepath.Join(dataDir, "sql-server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("creating dolt log file: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, doltBinary, "sql-server", "-H", "127.0.0.1", "-P", port, "--data-dir", dataDir)
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("starting dolt sql-server: %v", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	deadline := time.Now().Add(doltServerStartupLimit)
	addr := net.JoinHostPort("127.0.0.1", port)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			t.Cleanup(func() {
				cancel()
				<-waitCh
				_ = logFile.Close()
			})
			return port
		}
		time.Sleep(100 * time.Millisecond)
	}

	cancel()
	<-waitCh
	_ = logFile.Close()
	logBytes, _ := os.ReadFile(logPath)
	t.Fatalf("dolt sql-server did not become ready on %s within %s:\n%s", addr, doltServerStartupLimit, logBytes)
	return ""
}

// runBDInit initializes beads against the shared Dolt server with a bounded wait.
func runBDInit(t *testing.T, env []string, dir, prefix, port string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), bdInitTimeout)
	defer cancel()

	bdInit := exec.CommandContext(ctx, bdBinary, "init", "--server", "--server-host", "127.0.0.1", "--server-port", port, "-p", prefix, "--skip-hooks", "--skip-agents")
	bdInit.Dir = dir
	bdInit.Env = env
	out, err := bdInit.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("bd init timed out after %s: %s", bdInitTimeout, out)
	}
	if err != nil {
		t.Fatalf("bd init: %v: %s", err, out)
	}
}

func configureCustomTypes(t *testing.T, env []string, wsDir string, customTypes []string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), bdInitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bdBinary, "config", "set", "types.custom", strings.Join(customTypes, ","))
	cmd.Dir = wsDir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("bd config set types.custom timed out after %s: %s", bdInitTimeout, out)
	}
	if err != nil {
		t.Fatalf("bd config set types.custom: %v: %s", err, out)
	}
}
