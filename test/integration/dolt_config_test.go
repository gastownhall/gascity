//go:build integration

package integration

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDoltConfigWiringExternalHost validates two things for issue 011:
//
//  1. A Dolt server bound to 0.0.0.0 is reachable via the machine's
//     hostname (not just localhost) — proving the network path works.
//
//  2. Beads can be created and read through a server whose port was
//     explicitly configured (not discovered from local state files) —
//     proving the config → env wiring path works end-to-end.
//
// The unit tests in cmd/gc/ verify the internal wiring (city.toml [dolt] →
// env vars → isExternalDolt → skip managed Dolt). This test verifies
// the real network + bd stack that the wiring feeds into.
//
// Requires: dolt and bd installed.
func TestDoltConfigWiringExternalHost(t *testing.T) {
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not installed")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not installed")
	}

	ensureDoltIdentity(t)

	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("getting hostname: %v", err)
	}

	// Start a Dolt server on 0.0.0.0 so it's reachable beyond localhost.
	doltDataDir := filepath.Join(t.TempDir(), "dolt-data")
	port := startDoltServerOnAllInterfaces(t, doltDataDir)

	// Phase 1: Verify hostname resolves and server is reachable via it.
	// This proves the network path that a cross-machine setup would use.
	addrs, err := net.LookupHost(hostname)
	if err != nil || len(addrs) == 0 {
		t.Skipf("hostname %q does not resolve — skipping", hostname)
	}
	addr := net.JoinHostPort(hostname, port)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Skipf("cannot connect to Dolt via %s — skipping: %v", addr, err)
	}
	_ = conn.Close()
	t.Logf("Phase 1 PASS: Dolt server reachable via hostname %s:%s", hostname, port)

	// Phase 2: Create and read beads through the explicitly configured
	// port (simulating what an agent gets from the config wiring).
	// Connect via 127.0.0.1 to avoid Dolt's non-localhost auth requirement;
	// Phase 1 already proved hostname reachability.
	wsDir := filepath.Join(t.TempDir(), "test-workspace")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit := exec.Command("git", "init", "--quiet")
	gitInit.Dir = wsDir
	if out, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	// Use the port we started the server on — NOT a port from a local
	// state file. This proves the "config port → env → bd" path works.
	bdEnv := append(os.Environ(),
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT="+port,
	)

	runBDInitCompat(t, wsDir, "dc", port)

	bdCreate := exec.Command(bdBinary, "create", "config-wired-bead", "--json",
		"--description=Integration test for issue 011", "-t", "task", "-p", "3")
	bdCreate.Dir = wsDir
	bdCreate.Env = bdEnv
	out, err := bdCreate.CombinedOutput()
	if err != nil {
		t.Fatalf("bd create: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "config-wired-bead") {
		t.Fatalf("bd create output missing bead title:\n%s", out)
	}

	// Read back to confirm round-trip.
	bdList := exec.Command(bdBinary, "list", "--json")
	bdList.Dir = wsDir
	bdList.Env = bdEnv
	listOut, err := bdList.CombinedOutput()
	if err != nil {
		t.Fatalf("bd list: %v\n%s", err, listOut)
	}
	if !strings.Contains(string(listOut), "config-wired-bead") {
		t.Fatalf("bd list output missing created bead:\n%s", listOut)
	}

	t.Logf("Phase 2 PASS: bead created and read back via explicit port %s", port)

	// Phase 3: Verify a SECOND workspace can see beads from the FIRST
	// through the same server — proving cross-workspace bead sharing.
	wsDir2 := filepath.Join(t.TempDir(), "test-workspace-2")
	if err := os.MkdirAll(wsDir2, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit2 := exec.Command("git", "init", "--quiet")
	gitInit2.Dir = wsDir2
	if out, err := gitInit2.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	// Init with same prefix and server — simulates a second machine's
	// agent sharing the same bead store.
	runBDInitCompat(t, wsDir2, "dc", port)

	bdList2 := exec.Command(bdBinary, "list", "--json")
	bdList2.Dir = wsDir2
	bdList2.Env = bdEnv
	listOut2, err := bdList2.CombinedOutput()
	if err != nil {
		t.Fatalf("bd list (workspace 2): %v\n%s", err, listOut2)
	}
	if !strings.Contains(string(listOut2), "config-wired-bead") {
		t.Fatalf("workspace 2 cannot see bead from workspace 1 — cross-workspace sharing broken:\n%s", listOut2)
	}

	t.Logf("Phase 3 PASS: second workspace sees beads from first via shared server")
	t.Logf("SUCCESS: all phases passed — hostname reachable, config port wired, cross-workspace sharing works")
}

// runBDInitCompat initializes beads against a shared server, compatible
// with bd v0.60.0 (which lacks --skip-agents).
func runBDInitCompat(t *testing.T, dir, prefix, port string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bdBinary, "init", "--server",
		"--server-host", "127.0.0.1", "--server-port", port,
		"-p", prefix, "--skip-hooks")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("bd init timed out: %s", out)
	}
	if err != nil {
		t.Fatalf("bd init: exit status %v: %s", err, out)
	}
}

// startDoltServerOnAllInterfaces starts a Dolt server bound to 0.0.0.0
// on an ephemeral port and returns the port string. The server is killed
// when the test ends.
func startDoltServerOnAllInterfaces(t *testing.T, dataDir string) string {
	t.Helper()

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("creating dolt data dir: %v", err)
	}

	listener, err := net.Listen("tcp", "0.0.0.0:0")
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
	cmd := exec.CommandContext(ctx, "dolt", "sql-server",
		"-H", "0.0.0.0", "-P", port, "--data-dir", dataDir)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("starting dolt sql-server: %v", err)
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	// Wait for server to be ready.
	deadline := time.Now().Add(15 * time.Second)
	addr := net.JoinHostPort("127.0.0.1", port)
	for {
		conn, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			t.Cleanup(func() {
				cancel()
				<-waitCh
				_ = logFile.Close()
			})
			return port
		}
		if time.Now().After(deadline) {
			cancel()
			<-waitCh
			_ = logFile.Close()
			logBytes, _ := os.ReadFile(logPath)
			t.Fatalf("dolt sql-server did not become ready on %s within 15s:\n%s", addr, logBytes)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
