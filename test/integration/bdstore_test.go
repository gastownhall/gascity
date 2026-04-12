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
// Requires: dolt and bd installed.
func TestBdStoreConformance(t *testing.T) {
	// Skip if dolt or bd not installed.
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not installed")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not installed")
	}

	// Ensure dolt identity is configured (mirrors git user.name/email).
	ensureDoltIdentity(t)

	rootDir := t.TempDir()
	doltDataDir := filepath.Join(rootDir, "dolt")
	workspacesDir := filepath.Join(rootDir, "workspaces")
	serverPort := startSharedDoltServer(t, doltDataDir)
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

		runBDInit(t, wsDir, prefix, serverPort)

		// Explicitly set issue_prefix (required for bd create).
		bdConfig := exec.Command("bd", "config", "set", "issue_prefix", prefix)
		bdConfig.Dir = wsDir
		if out, err := bdConfig.CombinedOutput(); err != nil {
			t.Fatalf("bd config set: %v: %s", err, out)
		}
		customTypes := exec.Command("bd", "config", "set", "types.custom", strings.Join(doctor.RequiredCustomTypes, ","))
		customTypes.Dir = wsDir
		if out, err := customTypes.CombinedOutput(); err != nil {
			t.Fatalf("bd config set types.custom: %v: %s", err, out)
		}

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
func startSharedDoltServer(t *testing.T, dataDir string) string {
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
	cmd := exec.CommandContext(ctx, "dolt", "sql-server", "-H", "127.0.0.1", "-P", port, "--data-dir", dataDir)
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
func runBDInit(t *testing.T, dir, prefix, port string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), bdInitTimeout)
	defer cancel()

	bdInit := exec.CommandContext(ctx, "bd", "init", "--server", "--server-host", "127.0.0.1", "--server-port", port, "-p", prefix, "--skip-hooks", "--skip-agents")
	bdInit.Dir = dir
	out, err := bdInit.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("bd init timed out after %s: %s", bdInitTimeout, out)
	}
	if err != nil {
		t.Fatalf("bd init: %v: %s", err, out)
	}
}

// ensureDoltIdentity ensures dolt has user.name and user.email set.
// Copies from git config if available, otherwise sets defaults.
func ensureDoltIdentity(t *testing.T) {
	t.Helper()

	// Check if dolt identity is already set.
	name, _ := exec.Command("dolt", "config", "--global", "--get", "user.name").Output()
	email, _ := exec.Command("dolt", "config", "--global", "--get", "user.email").Output()

	if len(name) > 0 && len(email) > 0 {
		return
	}

	// Copy from git config.
	if len(name) == 0 {
		gitName, _ := exec.Command("git", "config", "--global", "user.name").Output()
		if len(gitName) > 0 {
			exec.Command("dolt", "config", "--global", "--add", "user.name", string(gitName)).Run()
		} else {
			exec.Command("dolt", "config", "--global", "--add", "user.name", "test").Run()
		}
	}
	if len(email) == 0 {
		gitEmail, _ := exec.Command("git", "config", "--global", "user.email").Output()
		if len(gitEmail) > 0 {
			exec.Command("dolt", "config", "--global", "--add", "user.email", string(gitEmail)).Run()
		} else {
			exec.Command("dolt", "config", "--global", "--add", "user.email", "test@test.com").Run()
		}
	}
}
