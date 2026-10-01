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
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

const (
	bdInitTimeout          = 60 * time.Second
	doltServerStartupLimit = 10 * time.Second
	// doltServerReadyTimeout budgets startDoltServerOnAllInterfaces's
	// TCP-dial readiness wait (dolt_config_test.go). Kept separate from
	// doltServerStartupLimit — same shape of wait, but a distinct call
	// site — so a future tuning of one doesn't silently retune the other;
	// that kind of implicit coupling is what let runBDInitCompat's timeout
	// drift out of sync with bdInitTimeout in the first place (ga-gajll3).
	doltServerReadyTimeout = 60 * time.Second
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
	// bd 1.0.4 (deps.env BD_PREV_VERSION, the contract-tested floor) was tagged
	// before #3691 restored bd's empty-DB auto-import guard, so it silently
	// auto-imports into an empty on-disk DB and trips gascity's
	// ErrBDSilentFallback guard (#2080). That is bd behavior, not a gascity
	// regression; v1.0.5 is the first tag that contains #3691.
	helpers.RequireBDAtLeast(t, realBDBinary, "v1.0.5", "the #3691 empty-DB auto-import guard")
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

		return beads.NewBdStore(wsDir, pinnedBdStoreCommandRunner())
	}

	// Run conformance suite. We skip RunSequentialIDTests because BdStore
	// uses bd's ID format (prefix-XXXX), not gc-N sequential format.
	// BdStore hands ParentID to bd as --parent, which bd resolves
	// unconditionally, so it cannot keep a parent bd does not have. The
	// ParentID row therefore asserts that refusal instead of the weak-reference
	// contract the other stores meet; it still executes (ga-6mfvtl).
	beadstest.RunStoreTestsWithOptions(t, newStore, beadstest.Options{RefusesUnresolvableParent: true})
	beadstest.RunMetadataTests(t, newStore)
	beadstest.RunCloseReasonTests(t, newStore)
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
// --quiet makes bd return before its success banner and the report-only
// post-init diagnostics, which this harness never reads: a bd init that
// finished its work must not be killed by the bound while it runs them.
func runBDInit(t *testing.T, env []string, dir, prefix, port string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), bdInitTimeout)
	defer cancel()

	bdInit := exec.CommandContext(ctx, bdBinary, "init", "--quiet", "--server", "--server-host", "127.0.0.1", "--server-port", port, "-p", prefix, "--skip-hooks", "--skip-agents")
	bdInit.Dir = dir
	bdInit.Env = isolateBdHomeEnv(env)
	out, err := bdInit.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("bd init timed out after %s: %s", bdInitTimeout, out)
	}
	if err != nil {
		t.Fatalf("bd init: %v: %s", err, out)
	}
}

// TestBdStoreMailWispInsert verifies that creating an ephemeral mail message
// bead via BdStore succeeds through the full bd CLI → Dolt SQL stack.
//
// Regression tripwire for the 2026-06-11 P0 incident: gc mail send broke in
// production with "Field 'id' doesn't have a default value" because the bd CLI
// code omitted id on INSERT INTO wisp_events while a newer schema migration had
// dropped the DEFAULT (UUID()). The e2e mail tests use the file beads provider
// and never touch Dolt SQL; this test closes that gap by exercising the
// BdStore → bd create --ephemeral → Dolt → wisp_events INSERT path directly.
func TestBdStoreMailWispInsert(t *testing.T) {
	requireDoltIntegration(t)
	env := newIsolatedToolEnv(t, true)

	rootDir := t.TempDir()
	doltDataDir := filepath.Join(rootDir, "dolt")
	wsDir := filepath.Join(rootDir, "ws")
	serverPort := startSharedDoltServer(t, env, doltDataDir)

	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("creating workspace: %v", err)
	}
	gitCmd := exec.Command("git", "init", "--quiet")
	gitCmd.Dir = wsDir
	if out, err := gitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	runBDInit(t, env, wsDir, "mc", serverPort)
	configureCustomTypes(t, env, wsDir, doctor.RequiredCustomTypes)

	store := beads.NewBdStore(wsDir, isolatedBdStoreCommandRunner(env))

	// Create an ephemeral message bead — exercises bd create --ephemeral →
	// Dolt SQL INSERT INTO wisps + INSERT INTO wisp_events.
	// A NOT NULL / no-DEFAULT failure on wisp_events.id reproduces the incident.
	sent, err := store.Create(beads.Bead{
		Title:     "hello from bdstore mail regression",
		Type:      "message",
		Assignee:  "builder",
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("BdStore Create ephemeral message (wisp_events INSERT): %v", err)
	}
	if !sent.Ephemeral {
		t.Fatalf("Ephemeral = false on returned bead %s, want true", sent.ID)
	}
	if sent.ID == "" {
		t.Fatal("returned bead has empty ID")
	}

	// List with TierWisps to confirm the bead is readable after the INSERT.
	results, err := store.List(beads.ListQuery{
		TierMode: beads.TierWisps,
		Assignee: "builder",
	})
	if err != nil {
		t.Fatalf("BdStore List wisp beads: %v", err)
	}
	var found bool
	for _, b := range results {
		if b.ID == sent.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("sent bead %s not in BdStore List(TierWisps); got %d beads total", sent.ID, len(results))
	}
}

// TestBdStoreMailWispInsertIsolatesHOMEFromSharedServerConfig mirrors
// TestBdStoreMailWispInsert but deliberately points env's HOME at a
// shared-server config.yaml before running the exact same
// runBDInit/configureCustomTypes/pinnedBdStoreCommandRunnerWithEnv chain.
//
// newIsolatedToolEnv sets env's own HOME explicitly (via isolateGCHomeEnv/
// integrationEnvFor), so t.Setenv("HOME", ...) cannot reach it. This test
// substitutes a controlled, worst-case stand-in for "whatever the
// real invoking user's real home happens to contain" (on a fleet host that
// runs a real shared bd/dolt server out of that real home — this one does —
// that's a real shared-server config, not a hypothetical) so the
// reproduction is deterministic and machine-independent rather than
// depending on whatever this specific test run's real $HOME happens to hold.
func TestBdStoreMailWispInsertIsolatesHOMEFromSharedServerConfig(t *testing.T) {
	requireDoltIntegration(t)
	env := newIsolatedToolEnv(t, true)

	pollutedHome := t.TempDir()
	beadsDir := filepath.Join(pollutedHome, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("creating polluted HOME .beads dir: %v", err)
	}
	cfg := "no-db: true\ndolt:\n    shared-server: true\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("writing polluted HOME config.yaml: %v", err)
	}
	env = replaceEnv(env, "HOME", pollutedHome)
	// Pollute the process HOME too, so the BdStore runner below inherits it
	// unless its own HOME pin wins.
	t.Setenv("HOME", pollutedHome)

	rootDir := t.TempDir()
	doltDataDir := filepath.Join(rootDir, "dolt")
	wsDir := filepath.Join(rootDir, "ws")
	serverPort := startSharedDoltServer(t, env, doltDataDir)

	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("creating workspace: %v", err)
	}
	gitCmd := exec.Command("git", "init", "--quiet")
	gitCmd.Dir = wsDir
	if out, err := gitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	runBDInit(t, env, wsDir, "hi", serverPort)
	configureCustomTypes(t, env, wsDir, doctor.RequiredCustomTypes)

	isoHome := parseEnvList(isolateBdHomeEnv(env))["HOME"]
	if isoHome == pollutedHome {
		t.Fatalf("isolateBdHomeEnv left HOME at the polluted %s; env has no GC_HOME to isolate to", pollutedHome)
	}
	store := beads.NewBdStore(wsDir, pinnedBdStoreCommandRunnerWithEnv(map[string]string{"HOME": isoHome}))

	sent, err := store.Create(beads.Bead{
		Title:     "home-isolation probe",
		Type:      "message",
		Assignee:  "builder",
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("BdStore Create under a shared-server HOME: %v", err)
	}

	results, err := store.List(beads.ListQuery{
		TierMode: beads.TierWisps,
		Assignee: "builder",
	})
	if err != nil {
		t.Fatalf("BdStore List under a shared-server HOME: %v", err)
	}
	var found bool
	for _, b := range results {
		if b.ID == sent.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("sent bead %s not in BdStore List(TierWisps) under a shared-server HOME; got %d beads total", sent.ID, len(results))
	}
}

func configureCustomTypes(t *testing.T, env []string, wsDir string, customTypes []string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), bdInitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bdBinary, "config", "set", "types.custom", strings.Join(customTypes, ","))
	cmd.Dir = wsDir
	cmd.Env = isolateBdHomeEnv(env)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("bd config set types.custom timed out after %s: %s", bdInitTimeout, out)
	}
	if err != nil {
		t.Fatalf("bd config set types.custom: %v: %s", err, out)
	}
}
