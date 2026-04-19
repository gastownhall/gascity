// Package dolt_test validates that the dolt pack's health.sh script
// completes within a bounded time even when the Dolt server is
// unresponsive. This is a regression guard for the hang reported in
// the atlas city (deacon patrol, 2026-04-17).
package dolt_test

import (
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// healthScript is the on-disk path to the health command script. The
// dolt pack wraps each CLI command in its own directory with a
// `run.sh` entry point (and a sibling `command.toml` descriptor), so
// the health script lives at `commands/health/run.sh`.
const healthScript = "commands/health/run.sh"

func repoRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Dir(filename)
}

// startDeadTCPListener accepts connections but never writes or reads —
// simulating a Dolt server whose goroutines are stuck before the MySQL
// handshake completes. Returns the port and a cleanup func.
func startDeadTCPListener(t *testing.T) (int, func()) {
	t.Helper()
	lc := net.ListenConfig{}
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			// Hold the connection open but send nothing. The Dolt
			// client blocks waiting for the server handshake, which
			// reproduces the hang mode the health script must tolerate.
			go func(c net.Conn) {
				<-stop
				_ = c.Close()
			}(c)
		}
	}()
	cleanup := func() {
		close(stop)
		_ = l.Close()
		wg.Wait()
	}
	return l.Addr().(*net.TCPAddr).Port, cleanup
}

// TestHealthScriptIsBounded runs commands/health.sh against a TCP
// listener that accepts connections but never speaks MySQL. The
// script used to hang indefinitely here because the per-database
// commit count ran `dolt log --oneline` directly against the on-disk
// database while the server held it open. The fix routes commit
// counts through SQL and wraps all dolt binary invocations in a
// timeout. We assert completion well under a minute.
func TestHealthScriptIsBounded(t *testing.T) {
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not installed; skipping")
	}
	if _, errT := exec.LookPath("timeout"); errT != nil {
		if _, errG := exec.LookPath("gtimeout"); errG != nil {
			t.Skip("neither timeout nor gtimeout installed; skipping")
		}
	}

	port, cleanup := startDeadTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Minimal metadata file so metadata_files has a target.
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_database":"at"}`), 0o644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	root := repoRoot(t)
	script := filepath.Join(root, healthScript)

	cmd := exec.Command("sh", script)
	cmd.Env = append(os.Environ(),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT="+strconv.Itoa(port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		// Skip zombie enumeration: we're testing bounded-probe
		// behavior, and per-PID `ps` calls on machines with many
		// ambient dolt processes dominate the runtime budget.
		"GC_HEALTH_SKIP_ZOMBIE_SCAN=1",
	)

	done := make(chan error, 1)
	stdout, stdoutW := io.Pipe()
	cmd.Stdout = stdoutW
	cmd.Stderr = stdoutW
	go func() {
		done <- cmd.Run()
		_ = stdoutW.Close()
	}()

	// Drain output so the pipe never fills.
	var buf strings.Builder
	go func() { _, _ = io.Copy(&buf, stdout) }()

	// The script has per-call 5s timeouts. Allow generous slack for
	// CI jitter, but fail hard well before "indefinite hang".
	const budget = 45 * time.Second
	select {
	case err := <-done:
		// Non-zero exit is expected here — the server isn't speaking
		// MySQL, so the health script should signal unhealthy. A
		// nil err means the script exited 0, which silently defeats
		// the exit-code regression guard. A non-ExitError means the
		// script couldn't even run (fork/exec failure, bad path) —
		// surface that distinctly so the failure points at the
		// right cause.
		if err == nil {
			t.Fatalf("health.sh exited 0 against unresponsive server; expected non-zero\n%s", buf.String())
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("health.sh produced non-exit error: %v\n%s", err, buf.String())
		}
	case <-time.After(budget):
		_ = cmd.Process.Kill()
		t.Fatalf("health.sh exceeded %s budget against unresponsive server\n%s", budget, buf.String())
	}
}

// TestHealthScriptDoesNotInvokeDoltLog is a cheap regression guard
// for the specific bug: the old script ran `dolt log --oneline`
// locally against each on-disk database, which deadlocked with the
// running dolt sql-server. Routing commit counts through SQL is
// the only safe option. If a future refactor reintroduces `dolt log`,
// the hang comes back.
//
// The regex matches `dolt log` as an executable call across the
// common invocation shapes: space-separated, tab-separated, and
// backslash-continued across lines. It deliberately does not match
// the SQL identifier `dolt_log` (the system table) or prose usages
// like "run `dolt log` to see commits". Line-by-line scanning with
// simple substring checks would miss `dolt \\<newline>log` and
// `dolt<tab>log`, which are both valid shell invocations.
var doltLogCallRe = regexp.MustCompile(`(?m)(^|[^_A-Za-z0-9])dolt[ \t\\]+\n?[ \t]*log(\s|$)`)

func TestHealthScriptDoesNotInvokeDoltLog(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, healthScript))
	if err != nil {
		t.Fatalf("read %s: %v", healthScript, err)
	}
	// Strip comment lines so the regex cannot false-positive on
	// explanatory prose (e.g. "historically ran `dolt log --oneline`").
	var body strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}
	if m := doltLogCallRe.FindString(body.String()); m != "" {
		t.Errorf("%s contains `dolt log` as an executable call (match: %q).\n"+
			"Commit counts must go through SQL (SELECT COUNT(*) FROM dolt_log) to avoid "+
			"deadlocking with the running sql-server.", healthScript, m)
	}
}

// TestHealthScriptJSONAlwaysExitsZero guards the JSON-mode exit
// contract. Automation consumers (notably the deacon patrol formula)
// parse the JSON payload and key health decisions off `server.reachable`.
// If `--json` exits non-zero on an unreachable server, a formula
// step executor may fail the step before stdout is parsed — the
// exact failure mode this PR was meant to diagnose. The human
// (non-JSON) form still returns non-zero on unhealthy servers; only
// `--json` is unconditionally exit 0.
func TestHealthScriptJSONAlwaysExitsZero(t *testing.T) {
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not installed; skipping")
	}
	if _, errT := exec.LookPath("timeout"); errT != nil {
		if _, errG := exec.LookPath("gtimeout"); errG != nil {
			t.Skip("neither timeout nor gtimeout installed; skipping")
		}
	}

	// Bind a socket to get a guaranteed-closed port, then release it.
	// Any residual latency in the OS accepting on a dead port is fine
	// — the script's 5s bounds dominate.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_database":"at"}`), 0o644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	root := repoRoot(t)
	script := filepath.Join(root, healthScript)

	cmd := exec.Command("sh", script, "--json")
	cmd.Env = append(os.Environ(),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT="+strconv.Itoa(port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_HEALTH_SKIP_ZOMBIE_SCAN=1",
	)

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("health.sh --json exited non-zero against unreachable server: %v\n%s", err, out)
	}
	// The payload MUST carry server.reachable so consumers can tell
	// the server is down without needing a non-zero exit code.
	if !strings.Contains(string(out), `"reachable": false`) {
		t.Errorf("JSON payload missing expected `\"reachable\": false`; got:\n%s", out)
	}
}
