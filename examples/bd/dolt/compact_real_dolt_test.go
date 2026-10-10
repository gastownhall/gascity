//go:build integration || dolt_integration

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

	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/test/dolttest"
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
	// This history is the city's own (not adopted), so opt it into full
	// flattening; the remote push itself needs the federated opt-in (#5958).
	if err := os.WriteFile(filepath.Join(dbDir, ".compact-full-history"), nil, 0o644); err != nil {
		t.Fatalf("write .compact-full-history: %v", err)
	}

	port, pid := startRealDoltServerForCompactTest(t, doltPath, dataDir)
	writeManagedRuntimeStateForScriptWithPID(t, cityPath, port, pid)
	waitForDoltServerQueryForCompactTest(t, doltPath, port)

	out, err := runCompactScriptForRealDoltTest(t, doltPath, root, cityPath, dataDir, port, nil,
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=1",
		"GC_DOLT_COMPACT_ALLOW_FEDERATED=1",
		"GC_DOLT_COMPACT_CALL_TIMEOUT_SECS=20",
		"GC_DOLT_COMPACT_PUSH_TIMEOUT_SECS=20",
	)
	if err != nil {
		t.Fatalf("compact script failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "remote=origin pushed compacted main") {
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

// doltCallMargin is the part of the test's own deadline a dolt CLI call may not
// use. A call that really is hung is killed this long before the package
// timeout, which leaves the test time to report it and t.Cleanup time to stop
// its sql-server (up to 10s) instead of the whole test binary panicking.
const doltCallMargin = 30 * time.Second

// doltCallWaitDelay bounds how long a call's Wait may keep blocking on an output
// pipe after the call has exited or been killed. A child that outlives its
// parent (the dolt processes run.sh starts) holds the pipe open, and without a
// delay Wait blocks until that child exits too. It is a var so a test can
// shorten it.
var doltCallWaitDelay = 10 * time.Second

// testDeadline is the part of *testing.T that bounds a dolt CLI call, so a test
// can drive the bound with a fake deadline.
type testDeadline interface {
	Deadline() (deadline time.Time, ok bool)
}

// doltCallContext returns the context for one external dolt CLI call. The call
// may use whatever is left of the test's own deadline less doltCallMargin; that
// margin is capped at half of what is left, so a short deadline still gives the
// call time and the test can still report. With no test deadline
// (go test -timeout 0) nothing bounds the call. There is deliberately no fixed
// per-call budget: under suite load a merely slow dolt call outlasts any fixed
// one and is SIGKILLed, which fails its test with "signal: killed".
func doltCallContext(t testDeadline) (context.Context, context.CancelFunc) {
	deadline, ok := t.Deadline()
	if !ok {
		return context.WithCancel(context.Background())
	}
	held := min(doltCallMargin, max(0, time.Until(deadline))/2)
	return context.WithDeadline(context.Background(), deadline.Add(-held))
}

func runDoltForCompactTest(t *testing.T, doltPath, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := doltCallContext(t)
	defer cancel()
	cmd := exec.CommandContext(ctx, doltPath, args...)
	cmd.Dir = dir
	cmd.WaitDelay = doltCallWaitDelay
	// Newer dolt CLIs colorize `dolt log` output even without a TTY; ANSI
	// escapes would corrupt hash parsing in doltHeadForCompactTest.
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dolt %s failed in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// compactDoltPortPicker, when set, chooses the port startRealDoltServerForCompactTest
// starts dolt on in place of probing for a free one. A test sets it to hand the
// helper a port that another process already holds; it is nil otherwise.
var compactDoltPortPicker func() (int, error)

func TestStartRealDoltServerForCompactTestOwnsItsPort(t *testing.T) {
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}

	// Another process holds the first port the helper is handed and accepts
	// connections on it, as a sibling test's sql-server would.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("holding a port: %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })
	go func() {
		for {
			conn, err := held.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	heldPort := held.Addr().(*net.TCPAddr).Port

	picks := 0
	original := compactDoltPortPicker
	compactDoltPortPicker = func() (int, error) {
		picks++
		if picks == 1 {
			return heldPort, nil
		}
		probe, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		defer func() { _ = probe.Close() }()
		return probe.Addr().(*net.TCPAddr).Port, nil
	}
	t.Cleanup(func() { compactDoltPortPicker = original })

	port, pid := startRealDoltServerForCompactTest(t, doltPath, t.TempDir())

	if port == heldPort {
		t.Fatalf("helper returned port %d, which another process holds, with dolt pid %d; the server it started cannot hold that port", port, pid)
	}
	// Hosts without socket tables cannot say who holds a port; there the
	// difference from the held port is all this can check.
	if holder, readable := pidutil.ListenerPID(port, pid); readable && holder != pid {
		t.Fatalf("port %d is held by pid %d, not by the dolt server the helper started (pid %d)", port, holder, pid)
	}
}

func startRealDoltServerForCompactTest(t *testing.T, doltPath, dataDir string) (int, int) {
	t.Helper()
	srv := dolttest.StartSQLServer(t, dolttest.SQLServerSpec{
		Dolt: doltPath,
		Args: func(port int) []string {
			return []string{
				"sql-server",
				"-H", "127.0.0.1",
				"-P", fmt.Sprintf("%d", port),
				"--data-dir", dataDir,
				"--loglevel", "warning",
			}
		},
		LogPath:  filepath.Join(dataDir, "sql-server.log"),
		PickPort: compactDoltPortPicker,
	})
	t.Cleanup(func() {
		_ = srv.Process.Kill()
		<-srv.Exited
	})
	return srv.Port, srv.PID
}

func waitForDoltServerQueryForCompactTest(t *testing.T, doltPath string, port int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastOut []byte
	var lastErr error
	for time.Now().Before(deadline) {
		// A per-attempt cap, deliberately not the call bound of doltCallContext:
		// this probe is retried until the readiness window above closes, so an
		// attempt that outlives it is retried, not fatal.
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
		cmd.WaitDelay = doltCallWaitDelay
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
	rows := doltServerQueryForCompactTest(t, doltPath, port, "SELECT HASHOF('HEAD')")
	if len(rows) == 0 || strings.TrimSpace(rows[0]) == "" {
		t.Fatalf("unexpected server HEAD output: %q", rows)
	}
	return strings.TrimSpace(rows[0])
}

// doltServerQueryForCompactTest runs query against the beads database on the
// test sql-server and returns the CSV rows without the header.
func doltServerQueryForCompactTest(t *testing.T, doltPath string, port int, query string) []string {
	t.Helper()
	ctx, cancel := doltCallContext(t)
	defer cancel()
	cmd := exec.CommandContext(ctx, doltPath,
		"--host", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
		"--user", "root",
		"--no-tls",
		"--use-db", "beads",
		"sql", "-r", "csv", "-q", query,
	)
	cmd.Env = append(filteredEnv("DOLT_CLI_PASSWORD"), "DOLT_CLI_PASSWORD=", "NO_COLOR=1")
	cmd.WaitDelay = doltCallWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("query server %q: %v\n%s", query, err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) <= 1 {
		return nil
	}
	return lines[1:]
}

// runCompactScriptForRealDoltTest runs commands/compact/run.sh against a real
// managed-looking Dolt sql-server. extraEnv entries override the defaults.
func runCompactScriptForRealDoltTest(t *testing.T, doltPath, root, cityPath, dataDir string, port int, args []string, extraEnv ...string) (string, error) {
	t.Helper()
	ctx, cancel := doltCallContext(t)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", append([]string{filepath.Join(root, "commands", "compact", "run.sh")}, args...)...)
	cmd.WaitDelay = doltCallWaitDelay
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
		"GC_DOLT_COMPACT_ALLOW_FEDERATED",
		"GC_DOLT_COMPACT_DRY_RUN",
		"GC_DOLT_COMPACT_BARE_GC",
		"GC_DOLT_COMPACT_SKIP_FETCH",
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
		"GC_DOLT_COMPACT_CALL_TIMEOUT_SECS=30",
		"GC_DOLT_COMPACT_PUSH_TIMEOUT_SECS=30",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
