// Package dolt_test validates that the dolt pack's health.sh script
// completes within a bounded time even when the Dolt server is
// unresponsive. This is a regression guard for the hang reported in
// the atlas city (deacon patrol, 2026-04-17).
package dolt_test

import (
	"encoding/json"
	"errors"
	"fmt"
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

func filteredEnv(keys ...string) []string {
	blocked := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		blocked[key] = struct{}{}
	}
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, skip := blocked[key]; skip {
				continue
			}
		}
		env = append(env, entry)
	}
	return env
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

	// Stub gc on PATH: metadata_files would otherwise call the real gc
	// with the 30s rig-list bound from runtime.sh, leaving this budget
	// hostage to host `gc rig list` latency — worst exactly on the busy
	// hosts this hang guard is for. The bounded dolt probe is what this
	// test measures; rig discovery is incidental.
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "gc"), "#!/bin/sh\nexit 1\n")

	cmd := exec.Command("sh", script)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
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

	// With gc stubbed out, the largest per-call bound left is the 5s
	// dolt probe. Allow generous slack for CI jitter, but fail hard
	// well before "indefinite hang".
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

// TestHealthOrphansFromCleanupAuthority pins #3200: the orphan list is
// sourced from `gc dolt-cleanup --json` (.dropped.names), the rig-protected
// dry-run authority — never from health's own metadata scan, which flagged
// every live rig DB as an orphan when rig metadata was sparse/unreachable. A
// live rig DB present on disk but absent from dropped.names must NOT appear in
// orphans (acting on that false positive could delete a live rig ledger).
func TestHealthOrphansFromCleanupAuthority(t *testing.T) {
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not installed; skipping")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed; skipping")
	}
	if _, errT := exec.LookPath("timeout"); errT != nil {
		if _, errG := exec.LookPath("gtimeout"); errG != nil {
			t.Skip("neither timeout nor gtimeout installed; skipping")
		}
	}

	// TCP-reachable but never speaks MySQL → server.reachable=false, so the
	// per-database (open_beads) probe is skipped and we exercise the orphan
	// path in isolation.
	port, cleanup := startDeadTCPListener(t)
	defer cleanup()

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	// Dolt data dir: three live rig DBs plus one genuine orphan.
	dataDir := t.TempDir()
	for _, db := range []string{"hq", "gco", "tga", "stale_orphan"} {
		if err := os.MkdirAll(filepath.Join(dataDir, db, ".dolt"), 0o755); err != nil {
			t.Fatalf("mkdir db %s: %v", db, err)
		}
	}

	// Stub `gc` on PATH. `gc dolt-cleanup --json` returns the rig-protected
	// dry-run authority: stale_orphan is the only drop candidate; the live rig
	// DBs are protected. Any other gc call (e.g. `gc rig list`) is a no-op.
	binDir := t.TempDir()
	stub := "#!/bin/sh\n" +
		"if [ \"$1\" = \"dolt-cleanup\" ]; then\n" +
		"  printf '%s' '{\"ok\":true,\"rigs_protected\":[{\"rig\":\"gchq\",\"db\":\"hq\"},{\"rig\":\"gco\",\"db\":\"gco\"},{\"rig\":\"tga\",\"db\":\"tga\"}],\"dropped\":{\"count\":1,\"names\":[\"stale_orphan\"]}}'\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "gc"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write gc stub: %v", err)
	}

	root := repoRoot(t)
	script := filepath.Join(root, healthScript)
	cmd := exec.Command("sh", script, "--json")
	cmd.Env = append(filteredEnv("GC_DOLT_PORT", "PATH", "GC_DOLT_DATA_DIR"),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT="+strconv.Itoa(port),
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_HEALTH_SKIP_ZOMBIE_SCAN=1",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("health.sh --json failed: %v\n%s", err, out)
	}

	var report struct {
		Orphans []struct {
			Name string `json:"name"`
		} `json:"orphans"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("parse health JSON: %v\n%s", err, out)
	}
	got := make([]string, 0, len(report.Orphans))
	for _, o := range report.Orphans {
		got = append(got, o.Name)
	}
	if len(got) != 1 || got[0] != "stale_orphan" {
		t.Errorf("orphans = %v, want [stale_orphan] (from cleanup authority)", got)
	}
	for _, o := range got {
		switch o {
		case "hq", "gco", "tga":
			t.Errorf("live rig DB %q misclassified as orphan", o)
		}
	}
}

// TestHealthScriptToleratesSlowRigListDiscovery pins the health half of
// gascity#2740: `gc rig list --json` regularly takes longer than the old 5s
// discovery bound on busy hosts. When the bound expires, metadata_files
// silently degrades to a city-only filesystem scan that cannot see
// externally-pathed rigs, so their metadata drops out of downstream
// consumers — surfacing here as a live external rig DB misclassified as an
// orphan, a false positive automation may act on. Answer rig list after 7s
// with an external rig and require the result to be consumed.
func TestHealthScriptToleratesSlowRigListDiscovery(t *testing.T) {
	if _, errT := exec.LookPath("timeout"); errT != nil {
		if _, errG := exec.LookPath("gtimeout"); errG != nil {
			t.Skip("neither timeout nor gtimeout installed; skipping")
		}
	}

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	// External rig OUTSIDE the city directory: only the rig-list result can
	// discover it — the fallback find scan is rooted at the city path.
	extRig := t.TempDir()
	if err := os.MkdirAll(filepath.Join(extRig, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir external rig: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extRig, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_database":"extdb"}`), 0o644); err != nil {
		t.Fatalf("write external rig metadata: %v", err)
	}

	// Data dir holds the city DB and the external rig's DB.
	dataDir := t.TempDir()
	for _, db := range []string{"hq", "extdb"} {
		if err := os.MkdirAll(filepath.Join(dataDir, db, ".dolt"), 0o755); err != nil {
			t.Fatalf("mkdir db %s: %v", db, err)
		}
	}

	binDir := t.TempDir()
	// Fake gc: `rig list` answers after 7s — past the old 5s bound, inside
	// the 30s one — naming the external rig. Everything else (notably
	// `gc dolt-cleanup`) fails, so orphan detection takes the metadata
	// referenced-set fallback, which protects extdb only when the rig-list
	// result was consumed instead of the city-only scan.
	writeExecutable(t, filepath.Join(binDir, "gc"), `#!/bin/sh
if [ "${1:-}" = "rig" ] && [ "${2:-}" = "list" ]; then
  sleep 7
  printf '{"rigs":[{"path": "`+extRig+`"}]}\n'
  exit 0
fi
exit 1
`)
	// No live server: lsof/nc fail, so the bounded dolt probe is skipped.
	writeExecutable(t, filepath.Join(binDir, "lsof"), "#!/bin/sh\nexit 1\n")
	writeExecutable(t, filepath.Join(binDir, "nc"), "#!/bin/sh\nexit 1\n")
	writeExecutable(t, filepath.Join(binDir, "dolt"), "#!/bin/sh\nexit 1\n")

	root := repoRoot(t)
	cmd := exec.Command("sh", filepath.Join(root, healthScript), "--json")
	cmd.Env = append(filteredEnv("GC_DOLT_PORT", "PATH", "GC_DOLT_DATA_DIR", "GC_DOLT_RIG_LIST_TIMEOUT_SECS"),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+dataDir,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT=3306",
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_HEALTH_SKIP_ZOMBIE_SCAN=1",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("health.sh --json failed: %v\n%s", err, out)
	}

	var report struct {
		Orphans []struct {
			Name string `json:"name"`
		} `json:"orphans"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("parse health JSON: %v\n%s", err, out)
	}
	for _, o := range report.Orphans {
		if o.Name == "extdb" {
			t.Fatalf("external rig DB extdb misclassified as orphan: a rig list answering within the 30s bound must be consumed, not dropped for the city-only fallback scan\n%s", out)
		}
	}
}

func TestRuntimeScriptPortPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, cityPath string) string
		wantExit78 bool
	}{
		{
			name: "managed state beats compatibility port mirror",
			setup: func(t *testing.T, cityPath string) string {
				t.Helper()
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("Listen: %v", err)
				}
				t.Cleanup(func() { _ = listener.Close() })
				port := listener.Addr().(*net.TCPAddr).Port
				writeManagedRuntimeStateForScript(t, cityPath, port)
				if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cityPath, ".beads", "dolt-server.port"), []byte("1111\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return strconv.Itoa(port)
			},
		},
		{
			name: "invalid managed state falls back to provider state",
			setup: func(t *testing.T, cityPath string) string {
				t.Helper()
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("Listen: %v", err)
				}
				t.Cleanup(func() { _ = listener.Close() })
				port := listener.Addr().(*net.TCPAddr).Port
				stateDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")
				if err := os.MkdirAll(stateDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(stateDir, "dolt-state.json"), []byte(`not-json`), 0o644); err != nil {
					t.Fatal(err)
				}
				writeManagedRuntimeStateFileForScript(t, cityPath, "dolt-provider-state.json", port, os.Getpid())
				return strconv.Itoa(port)
			},
		},
		{
			name: "corrupt managed state exits 78 despite compatibility port mirror",
			setup: func(t *testing.T, cityPath string) string {
				t.Helper()
				stateDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")
				if err := os.MkdirAll(stateDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(stateDir, "dolt-state.json"), []byte(`not-json`), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cityPath, ".beads", "dolt-server.port"), []byte("45785\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return ""
			},
			wantExit78: true,
		},
	}

	root := repoRoot(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			want := tt.setup(t, cityPath)

			cmd := exec.Command("sh", "-c", `. "$GC_PACK_DIR/assets/scripts/runtime.sh"; printf '%s\n' "$GC_DOLT_PORT"`)
			cmd.Env = filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_PORT", "GC_DOLT_HOST")
			cmd.Env = append(cmd.Env,
				"GC_CITY_PATH="+cityPath,
				"GC_PACK_DIR="+root,
			)
			out, err := cmd.CombinedOutput()
			if tt.wantExit78 {
				assertRuntimePortExit78(t, err, out, filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-state.json"), cityPath)
				return
			}
			if err != nil {
				t.Fatalf("runtime.sh failed: %v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != want {
				t.Fatalf("GC_DOLT_PORT = %q, want %q", got, want)
			}
		})
	}
}

func TestRuntimeScriptManagedStateBeatsStaleEnvPort(t *testing.T) {
	cityPath := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	writeManagedRuntimeStateForScript(t, cityPath, port)

	root := repoRoot(t)
	cmd := exec.Command("sh", "-c", `. "$GC_PACK_DIR/assets/scripts/runtime.sh"; printf '%s\n' "$GC_DOLT_PORT"`)
	cmd.Env = append(filteredEnv(
		"GC_CITY_PATH",
		"GC_PACK_DIR",
		"GC_CITY_RUNTIME_DIR",
		"GC_PACK_STATE_DIR",
		"GC_DOLT_DATA_DIR",
		"GC_DOLT_LOG_FILE",
		"GC_DOLT_STATE_FILE",
		"GC_DOLT_PID_FILE",
		"GC_DOLT_PORT",
		"GC_DOLT_HOST",
	),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_PORT=4406",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("runtime.sh failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != strconv.Itoa(port) {
		t.Fatalf("GC_DOLT_PORT = %q, want live managed state port %d", got, port)
	}
}

func TestRuntimeScriptPortPrecedenceToleratesInconclusiveLsof(t *testing.T) {
	tests := []struct {
		name        string
		lsofBody    string
		ncBody      func(port string) string
		wantManaged bool
		wantExit78  bool
	}{
		{
			name:     "inconclusive lsof accepts reachable port",
			lsofBody: "#!/bin/sh\nexit 0\n",
			ncBody: func(port string) string {
				return `#!/bin/sh
host="$2"
probe_port="$3"
if [ "$1" = "-z" ] && [ "$host" = "127.0.0.1" ] && [ "$probe_port" = "` + port + `" ]; then
  exit 0
fi
exit 1
`
			},
			wantManaged: true,
		},
		{
			name:     "mismatched lsof pid still rejects port",
			lsofBody: "#!/bin/sh\necho $$\nsleep 5\n",
			ncBody: func(_ string) string {
				return `#!/bin/sh
exit 0
`
			},
			wantExit78: true,
		},
		{
			name:     "inconclusive lsof with unreachable port still rejects port",
			lsofBody: "#!/bin/sh\nexit 0\n",
			ncBody: func(_ string) string {
				return `#!/bin/sh
exit 1
`
			},
			wantExit78: true,
		},
	}

	root := repoRoot(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			fakeBin := t.TempDir()

			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("Listen: %v", err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			port := listener.Addr().(*net.TCPAddr).Port
			managedPort := strconv.Itoa(port)
			want := "3307"
			if tt.wantManaged {
				want = managedPort
			}

			writeManagedRuntimeStateForScript(t, cityPath, port)
			writeExecutable(t, filepath.Join(fakeBin, "lsof"), tt.lsofBody)
			writeExecutable(t, filepath.Join(fakeBin, "nc"), tt.ncBody(managedPort))

			cmd := exec.Command("sh", "-c", `. "$GC_PACK_DIR/assets/scripts/runtime.sh"; printf '%s\n' "$GC_DOLT_PORT"`)
			cmd.Env = filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_PORT", "GC_DOLT_HOST", "PATH")
			cmd.Env = append(cmd.Env,
				"GC_CITY_PATH="+cityPath,
				"GC_PACK_DIR="+root,
				"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			)
			out, err := cmd.CombinedOutput()
			if tt.wantExit78 {
				assertRuntimePortExit78(t, err, out, filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-state.json"), cityPath)
				return
			}
			if err != nil {
				t.Fatalf("runtime.sh failed: %v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != want {
				t.Fatalf("GC_DOLT_PORT = %q, want %q", got, want)
			}
		})
	}
}

func assertRuntimePortExit78(t *testing.T, err error, out []byte, stateFile, cityPath string) {
	t.Helper()
	if err == nil {
		t.Fatalf("runtime.sh exited 0, want exit 78\n%s", out)
	}
	exitErr := &exec.ExitError{}
	ok := errors.As(err, &exitErr)
	if !ok {
		t.Fatalf("runtime.sh returned non-exit error: %v\n%s", err, out)
	}
	if exitErr.ExitCode() != 78 {
		t.Fatalf("runtime.sh exit code = %d, want 78\n%s", exitErr.ExitCode(), out)
	}
	if got, want := string(out), expectedPortResolveErrorWithProvider(stateFile, cityPath, "present but not running"); got != want {
		t.Fatalf("runtime.sh output = %q, want %q", got, want)
	}
}

func TestRuntimeScriptPortPrecedenceAcceptsPsConfirmedPid(t *testing.T) {
	tests := []struct {
		name     string
		lsofBody string
		ncBody   func(port string) string
	}{
		{
			name:     "listener pid match via ps fallback",
			lsofBody: "#!/bin/sh\necho 424242\n",
			ncBody: func(_ string) string {
				return `#!/bin/sh
exit 1
`
			},
		},
		{
			name:     "reachable port via ps fallback when lsof is inconclusive",
			lsofBody: "#!/bin/sh\nexit 0\n",
			ncBody: func(port string) string {
				return `#!/bin/sh
host="$2"
probe_port="$3"
if [ "$1" = "-z" ] && [ "$host" = "127.0.0.1" ] && [ "$probe_port" = "` + port + `" ]; then
  exit 0
fi
exit 1
`
			},
		},
	}

	root := repoRoot(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			fakeBin := t.TempDir()

			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("Listen: %v", err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			port := listener.Addr().(*net.TCPAddr).Port
			managedPort := strconv.Itoa(port)

			writeManagedRuntimeStateForScriptWithPID(t, cityPath, port, 424242)
			writeExecutable(t, filepath.Join(fakeBin, "lsof"), tt.lsofBody)
			writeExecutable(t, filepath.Join(fakeBin, "nc"), tt.ncBody(managedPort))
			writeExecutable(t, filepath.Join(fakeBin, "ps"), `#!/bin/sh
if [ "$1" = "-p" ] && [ "$2" = "424242" ]; then
  echo " 424242"
  exit 0
fi
exit 1
`)

			cmd := exec.Command("sh", "-c", `. "$GC_PACK_DIR/assets/scripts/runtime.sh"; printf '%s\n' "$GC_DOLT_PORT"`)
			cmd.Env = filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_PORT", "GC_DOLT_HOST", "PATH")
			cmd.Env = append(cmd.Env,
				"GC_CITY_PATH="+cityPath,
				"GC_PACK_DIR="+root,
				"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("runtime.sh failed: %v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != managedPort {
				t.Fatalf("GC_DOLT_PORT = %q, want %q", got, managedPort)
			}
		})
	}
}

func TestRuntimeScriptPortPrecedenceParsesManagedRuntimeStateWithPortableSed(t *testing.T) {
	cityPath := t.TempDir()
	fakeBin := t.TempDir()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	managedPort := strconv.Itoa(port)

	realSed, err := exec.LookPath("sed")
	if err != nil {
		t.Fatalf("LookPath(sed): %v", err)
	}

	writeManagedRuntimeStateForScript(t, cityPath, port)
	writeExecutable(t, filepath.Join(fakeBin, "sed"), fmt.Sprintf(`#!/bin/sh
case "$2" in
  *'\\(true\\|false\\)'*)
    exit 0
    ;;
esac
exec %q "$@"
`, realSed))

	root := repoRoot(t)
	cmd := exec.Command("sh", "-c", `. "$GC_PACK_DIR/assets/scripts/runtime.sh"; printf '%s\n' "$GC_DOLT_PORT"`)
	cmd.Env = filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_PORT", "GC_DOLT_HOST", "PATH")
	cmd.Env = append(cmd.Env,
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("runtime.sh failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != managedPort {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, managedPort)
	}
}

func TestHealthScriptReportsRunningWhenLsofIsInconclusive(t *testing.T) {
	cityPath := t.TempDir()
	fakeBin := t.TempDir()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)

	writeExecutable(t, filepath.Join(fakeBin, "lsof"), `#!/bin/sh
exit 0
`)
	writeExecutable(t, filepath.Join(fakeBin, "nc"), `#!/bin/sh
host="$2"
probe_port="$3"
if [ "$1" = "-z" ] && [ "$host" = "127.0.0.1" ] && [ "$probe_port" = "`+port+`" ]; then
  exit 0
fi
exit 1
`)
	writeExecutable(t, filepath.Join(fakeBin, "dolt"), `#!/bin/sh
exit 0
`)

	root := repoRoot(t)
	cmd := exec.Command("sh", filepath.Join(root, healthScript), "--json")
	cmd.Env = append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD", "GC_HEALTH_SKIP_ZOMBIE_SCAN", "PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_HOST=",
		"GC_DOLT_PORT="+port,
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"GC_HEALTH_SKIP_ZOMBIE_SCAN=1",
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("health.sh failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"running": true`) {
		t.Fatalf("health output missing running=true:\n%s", out)
	}
}

func TestHealthScriptPortableTimestampFallbacksRemainNumeric(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "bsd percent n literal", raw: "1776740122N"},
		{name: "empty percent n output", raw: ""},
	}

	root := repoRoot(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			fakeBin := t.TempDir()

			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("Listen: %v", err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)

			writeExecutable(t, filepath.Join(fakeBin, "lsof"), `#!/bin/sh
exit 0
`)
			writeExecutable(t, filepath.Join(fakeBin, "nc"), `#!/bin/sh
host="$2"
probe_port="$3"
if [ "$1" = "-z" ] && [ "$host" = "127.0.0.1" ] && [ "$probe_port" = "`+port+`" ]; then
  exit 0
fi
exit 1
`)
			writeExecutable(t, filepath.Join(fakeBin, "dolt"), `#!/bin/sh
exit 0
`)
			writeExecutable(t, filepath.Join(fakeBin, "date"), `#!/bin/sh
case "$1" in
  +%s%N)
    printf '%s' "${FAKE_DATE_PERCENT_SN_RAW-}"
    exit 0
    ;;
  +%s)
    counter_file="${FAKE_DATE_SECONDS_COUNTER_FILE:?}"
    if [ -f "$counter_file" ]; then
      count=$(cat "$counter_file")
    else
      count=0
    fi
    count=$((count + 1))
    printf '%s\n' "$count" > "$counter_file"
    case "$count" in
      1) printf '%s\n' "${FAKE_DATE_SECONDS_FIRST-1776740122}" ;;
      *) printf '%s\n' "${FAKE_DATE_SECONDS_SECOND-1776740123}" ;;
    esac
    exit 0
    ;;
  -u)
    printf '%s\n' '2026-04-23T00:00:00Z'
    exit 0
    ;;
esac
exec /bin/date "$@"
`)

			counterFile := filepath.Join(t.TempDir(), "date-counter")
			cmd := exec.Command("sh", filepath.Join(root, healthScript), "--json")
			cmd.Env = append(filteredEnv(
				"FAKE_DATE_PERCENT_SN_RAW",
				"FAKE_DATE_SECONDS_COUNTER_FILE",
				"FAKE_DATE_SECONDS_FIRST",
				"FAKE_DATE_SECONDS_SECOND",
				"GC_CITY_PATH",
				"GC_PACK_DIR",
				"GC_DOLT_HOST",
				"GC_DOLT_PORT",
				"GC_DOLT_USER",
				"GC_DOLT_PASSWORD",
				"GC_HEALTH_SKIP_ZOMBIE_SCAN",
				"PATH",
			),
				"FAKE_DATE_PERCENT_SN_RAW="+tt.raw,
				"FAKE_DATE_SECONDS_COUNTER_FILE="+counterFile,
				"FAKE_DATE_SECONDS_FIRST=1776740122",
				"FAKE_DATE_SECONDS_SECOND=1776740123",
				"GC_CITY_PATH="+cityPath,
				"GC_PACK_DIR="+root,
				"GC_DOLT_HOST=127.0.0.1",
				"GC_DOLT_PORT="+port,
				"GC_DOLT_USER=root",
				"GC_DOLT_PASSWORD=",
				"GC_HEALTH_SKIP_ZOMBIE_SCAN=1",
				"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			)

			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("health.sh --json failed: %v\n%s", err, out)
			}

			var report struct {
				Server struct {
					Running   bool `json:"running"`
					Reachable bool `json:"reachable"`
					LatencyMS int  `json:"latency_ms"`
				} `json:"server"`
			}
			if err := json.Unmarshal(out, &report); err != nil {
				t.Fatalf("health.sh --json returned invalid JSON: %v\n%s", err, out)
			}
			if !report.Server.Running {
				t.Fatalf("server.running = false, want true\n%s", out)
			}
			if !report.Server.Reachable {
				t.Fatalf("server.reachable = false, want true\n%s", out)
			}
			if report.Server.LatencyMS != 1000 {
				t.Fatalf("server.latency_ms = %d, want 1000 to prove seconds fallback ran\n%s", report.Server.LatencyMS, out)
			}
		})
	}
}

func writeManagedRuntimeStateForScript(t *testing.T, cityPath string, port int) {
	t.Helper()
	writeManagedRuntimeStateForScriptWithPID(t, cityPath, port, os.Getpid())
}

func writeManagedRuntimeStateForScriptWithPID(t *testing.T, cityPath string, port int, pid int) {
	t.Helper()
	writeManagedRuntimeStateFileForScript(t, cityPath, "dolt-state.json", port, pid)
}

func writeManagedRuntimeStateFileForScript(t *testing.T, cityPath string, filename string, port int, pid int) {
	t.Helper()
	stateDir := filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(cityPath, ".beads", "dolt")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte(fmt.Sprintf(
		`{"running":true,"pid":%d,"port":%d,"data_dir":%q,"started_at":"2026-04-20T00:00:00Z"}`,
		pid,
		port,
		dataDir,
	))
	if err := os.WriteFile(filepath.Join(stateDir, filename), payload, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func TestHealthScriptProbesConfiguredExternalHost(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_database":"city"}`), 0o644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	root := repoRoot(t)
	fakeBin := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "dolt.args")
	emptyDataDir := t.TempDir()

	// Force the local managed-server precheck to fail. External hosts must still
	// be probed via SQL against GC_DOLT_HOST:GC_DOLT_PORT.
	writeExecutable(t, filepath.Join(fakeBin, "gc"), "#!/bin/sh\nexit 1\n")
	writeExecutable(t, filepath.Join(fakeBin, "lsof"), "#!/bin/sh\nexit 1\n")
	writeExecutable(t, filepath.Join(fakeBin, "nc"), "#!/bin/sh\nexit 1\n")
	writeExecutable(t, filepath.Join(fakeBin, "dolt"), `#!/bin/sh
printf '%s\n' "$@" > "$FAKE_DOLT_ARGS"
exit 0
`)

	cmd := exec.Command("sh", filepath.Join(root, healthScript), "--json")
	cmd.Env = append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_HOST", "GC_DOLT_PORT",
		"GC_DOLT_USER", "GC_DOLT_PASSWORD", "GC_HEALTH_SKIP_ZOMBIE_SCAN", "PATH", "FAKE_DOLT_ARGS",
		"GC_DOLT_DATA_DIR"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_DATA_DIR="+emptyDataDir,
		"GC_DOLT_HOST=superlzy-dolt",
		"GC_DOLT_PORT=3306",
		"GC_DOLT_USER=superlzy",
		"GC_DOLT_PASSWORD=secret",
		"GC_HEALTH_SKIP_ZOMBIE_SCAN=1",
		"FAKE_DOLT_ARGS="+argsFile,
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("health.sh --json failed: %v\n%s", err, out)
	}

	var report struct {
		Server struct {
			Running   bool `json:"running"`
			Reachable bool `json:"reachable"`
		} `json:"server"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("health.sh --json returned invalid JSON: %v\n%s", err, out)
	}
	if report.Server.Running {
		t.Fatalf("server.running = true for external host; want false local-managed signal\n%s", out)
	}
	if !report.Server.Reachable {
		t.Fatalf("server.reachable = false; want configured external host SQL probe to succeed\n%s", out)
	}

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("fake dolt was not invoked: %v\nhealth output:\n%s", err, out)
	}
	gotArgs := "\n" + string(args)
	for _, want := range []string{"\n--host\nsuperlzy-dolt\n", "\n--port\n3306\n", "\n--user\nsuperlzy\n", "\nsql\n", "\n-q\n", "\nSELECT 1\n"} {
		if !strings.Contains(gotArgs, want) {
			t.Fatalf("fake dolt args missing %q; got:\n%s\nhealth output:\n%s", want, args, out)
		}
	}
}

// TestHealthScriptZombieScanExcludesRigLocalServers verifies that
// Dolt processes on rig-configured ports are not flagged as zombies.
// Regression guard for the bug where deacon patrol killed rig-local
// Dolt servers because the zombie scan treated every non-city-server
// dolt sql-server PID as a zombie.
func runHealthScriptZombieScanExcludesRigLocalServers(t *testing.T, rigConfig string) {
	cityPath := t.TempDir()
	fakeBin := t.TempDir()

	mainPort := "19901"
	rigPort := "19902"

	mainPID := "424201"
	rigPID := "424202"
	zombiePID := "424203"

	// City .beads directory with metadata.
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"dolt_database":"city"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Rig directory with config.yaml containing dolt.port.
	rigBeads := filepath.Join(cityPath, "rigs", "enterprise", ".beads")
	if err := os.MkdirAll(rigBeads, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigBeads, "metadata.json"),
		[]byte(`{"dolt_database":"enterprise"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigBeads, "config.yaml"),
		[]byte(rigConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fake gc: fail so metadata_files() falls back to find.
	writeExecutable(t, filepath.Join(fakeBin, "gc"), "#!/bin/sh\nexit 1\n")

	// Fake pgrep: returns rig PID and zombie PID (main PID excluded
	// by server_pid check, not by pgrep filtering).
	writeExecutable(t, filepath.Join(fakeBin, "pgrep"),
		fmt.Sprintf("#!/bin/sh\necho %s\necho %s\necho %s\n", mainPID, rigPID, zombiePID))

	// Fake lsof: maps ports to PIDs.
	writeExecutable(t, filepath.Join(fakeBin, "lsof"),
		fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    -iTCP:%s) echo %s; exit 0 ;;
    -iTCP:%s) echo %s; exit 0 ;;
  esac
done
exit 1
`, mainPort, mainPID, rigPort, rigPID))

	// Fake ps: handles pid_is_running (`-p <pid> -o pid=`) and the zombie
	// scan's single process-table pass (`ps -eo pid=,stat=,args=`, #2482).
	// All three dolt PIDs appear as live sql-servers; the script excludes the
	// city server (server_pid) and the rig-local dolt, leaving the orphan.
	writeExecutable(t, filepath.Join(fakeBin, "ps"), fmt.Sprintf(`#!/bin/sh
if [ "$1" = "-eo" ]; then
  echo "%s Sl dolt sql-server"
  echo "%s Sl dolt sql-server"
  echo "%s Sl dolt sql-server"
  exit 0
fi
if [ "$1" = "-p" ] && [ "$3" = "-o" ] && [ "$4" = "pid=" ]; then
  printf ' %%s\n' "$2"
  exit 0
fi
exit 1
`, mainPID, rigPID, zombiePID))

	// Fake nc: unreachable (no real server).
	writeExecutable(t, filepath.Join(fakeBin, "nc"), "#!/bin/sh\nexit 1\n")

	// Fake dolt: SELECT 1 fails (no real server).
	writeExecutable(t, filepath.Join(fakeBin, "dolt"), "#!/bin/sh\nexit 1\n")

	root := repoRoot(t)
	cmd := exec.Command("sh", filepath.Join(root, healthScript), "--json")
	cmd.Env = append(
		filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_HOST", "GC_DOLT_PORT",
			"GC_DOLT_USER", "GC_DOLT_PASSWORD", "GC_HEALTH_SKIP_ZOMBIE_SCAN", "PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT="+mainPort,
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("health.sh failed: %v\n%s", err, out)
	}

	output := string(out)

	// The true zombie (424203) should be counted.
	if !strings.Contains(output, `"zombie_count": 1`) {
		t.Errorf("expected zombie_count 1; got:\n%s", output)
	}

	// The rig PID (424202) must NOT appear in zombie_pids.
	if strings.Contains(output, rigPID) {
		t.Errorf("rig-local Dolt PID %s should not be in zombie_pids; got:\n%s", rigPID, output)
	}

	// The true zombie PID (424203) must appear in zombie_pids.
	if !strings.Contains(output, zombiePID) {
		t.Errorf("true zombie PID %s should be in zombie_pids; got:\n%s", zombiePID, output)
	}
}

func TestHealthScriptZombieScanExcludesRigLocalServers(t *testing.T) {
	tests := []struct {
		name      string
		rigConfig string
	}{
		{
			name:      "bare port",
			rigConfig: "dolt.port: 19902\n",
		},
		{
			name:      "quoted port",
			rigConfig: "dolt.port: \"19902\"\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runHealthScriptZombieScanExcludesRigLocalServers(t, tc.rigConfig)
		})
	}
}

// TestHealthScriptZombieScanExcludesForeignManagedServers verifies that
// Dolt servers managed by OTHER cities on the same host are not flagged
// as zombies. Regression guard for the bug where deacon patrol in every
// city on a shared dev host counted the other cities' Dolt servers as
// zombies, because the rig-local PID filter only sees rigs of the calling
// city. The scan extracts `--config <path>` from the args column of the
// single bounded `ps -eo` pass and checks for a sibling `dolt.pid`
// claiming the PID — the same shape gc itself uses when it manages Dolt
// for any city. This drives the bounded pass (its awk + the matched-server
// shell loop), NOT a per-PID `ps -p` shim.
func TestHealthScriptZombieScanExcludesForeignManagedServers(t *testing.T) {
	cityPath := t.TempDir()
	foreignCityPath := t.TempDir()
	fakeBin := t.TempDir()

	mainPort := "19903"
	mainPID := "424301"
	foreignPID := "424302"
	zombiePID := "424303"

	// Foreign city: dolt-config.yaml + dolt.pid sibling claiming foreignPID.
	foreignDoltDir := filepath.Join(foreignCityPath, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(foreignDoltDir, 0o755); err != nil {
		t.Fatal(err)
	}
	foreignConfigPath := filepath.Join(foreignDoltDir, "dolt-config.yaml")
	if err := os.WriteFile(foreignConfigPath, []byte("# foreign city dolt config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreignDoltDir, "dolt.pid"),
		[]byte(foreignPID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// City .beads directory with metadata.
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"dolt_database":"city"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fake gc: fail so metadata_files() falls back to find.
	writeExecutable(t, filepath.Join(fakeBin, "gc"), "#!/bin/sh\nexit 1\n")

	// Fake pgrep: returns main PID, foreign PID, and a true zombie PID.
	writeExecutable(t, filepath.Join(fakeBin, "pgrep"),
		fmt.Sprintf("#!/bin/sh\necho %s\necho %s\necho %s\n", mainPID, foreignPID, zombiePID))

	// Fake lsof: maps main port to mainPID.
	writeExecutable(t, filepath.Join(fakeBin, "lsof"),
		fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    -iTCP:%s) echo %s; exit 0 ;;
  esac
done
exit 1
`, mainPort, mainPID))

	// Fake ps: the bounded scan calls `ps -eo pid=,stat=,args=`. Emit the
	// foreign PID's args line WITH `--config <path>` so the awk pass extracts
	// it; the others are plain sql-servers. Also answer the `-p <pid> -o pid=`
	// liveness probe used by managed_runtime_listener_pid.
	writeExecutable(t, filepath.Join(fakeBin, "ps"), fmt.Sprintf(`#!/bin/sh
if [ "$1" = "-eo" ]; then
  echo "%s Sl dolt sql-server"
  echo "%s Sl dolt sql-server --config %s"
  echo "%s Sl dolt sql-server"
  exit 0
fi
if [ "$1" = "-p" ] && [ "$3" = "-o" ] && [ "$4" = "pid=" ]; then
  printf ' %%s\n' "$2"
  exit 0
fi
exit 1
`, mainPID, foreignPID, foreignConfigPath, zombiePID))

	// Fake nc: unreachable (no real server).
	writeExecutable(t, filepath.Join(fakeBin, "nc"), "#!/bin/sh\nexit 1\n")

	// Fake dolt: SELECT 1 fails (no real server).
	writeExecutable(t, filepath.Join(fakeBin, "dolt"), "#!/bin/sh\nexit 1\n")

	root := repoRoot(t)
	cmd := exec.Command("sh", filepath.Join(root, healthScript), "--json")
	cmd.Env = append(
		filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_HOST", "GC_DOLT_PORT",
			"GC_DOLT_USER", "GC_DOLT_PASSWORD", "GC_HEALTH_SKIP_ZOMBIE_SCAN", "PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT="+mainPort,
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("health.sh failed: %v\n%s", err, out)
	}
	output := string(out)

	// Only the true zombie (424303) should be counted; foreign-managed is excluded.
	if !strings.Contains(output, `"zombie_count": 1`) {
		t.Errorf("expected zombie_count 1; got:\n%s", output)
	}
	if strings.Contains(output, foreignPID) {
		t.Errorf("foreign-managed Dolt PID %s should not be in zombie_pids; got:\n%s", foreignPID, output)
	}
	if !strings.Contains(output, zombiePID) {
		t.Errorf("true zombie PID %s should be in zombie_pids; got:\n%s", zombiePID, output)
	}
}

// TestHealthScriptZombieScanFlagsMismatchedForeignPidFile verifies that
// when a foreign --config exists but the sibling dolt.pid does NOT match
// the candidate PID (the recorded process died, a stranger reused the
// PID, etc.), the candidate is still treated as a zombie. The foreign
// recognition logic must self-validate against the sibling pid file
// rather than trust the config-file path alone. Like the test above this
// drives the bounded `ps -eo` pass, not a per-PID `ps -p` shim.
func TestHealthScriptZombieScanFlagsMismatchedForeignPidFile(t *testing.T) {
	cityPath := t.TempDir()
	foreignCityPath := t.TempDir()
	fakeBin := t.TempDir()

	mainPort := "19904"
	mainPID := "424401"
	suspectPID := "424402"
	// dolt.pid records a different PID — the sibling claim doesn't match
	// the candidate, so the candidate is still a zombie.
	recordedPID := "424499"

	foreignDoltDir := filepath.Join(foreignCityPath, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(foreignDoltDir, 0o755); err != nil {
		t.Fatal(err)
	}
	foreignConfigPath := filepath.Join(foreignDoltDir, "dolt-config.yaml")
	if err := os.WriteFile(foreignConfigPath, []byte("# foreign city dolt config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreignDoltDir, "dolt.pid"),
		[]byte(recordedPID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"dolt_database":"city"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	writeExecutable(t, filepath.Join(fakeBin, "gc"), "#!/bin/sh\nexit 1\n")
	writeExecutable(t, filepath.Join(fakeBin, "pgrep"),
		fmt.Sprintf("#!/bin/sh\necho %s\necho %s\n", mainPID, suspectPID))
	writeExecutable(t, filepath.Join(fakeBin, "lsof"),
		fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    -iTCP:%s) echo %s; exit 0 ;;
  esac
done
exit 1
`, mainPort, mainPID))
	// Bounded `ps -eo` pass: the suspect PID carries `--config <path>` but
	// its sibling dolt.pid records a different PID, so the foreign-managed
	// check must NOT exclude it.
	writeExecutable(t, filepath.Join(fakeBin, "ps"), fmt.Sprintf(`#!/bin/sh
if [ "$1" = "-eo" ]; then
  echo "%s Sl dolt sql-server"
  echo "%s Sl dolt sql-server --config %s"
  exit 0
fi
if [ "$1" = "-p" ] && [ "$3" = "-o" ] && [ "$4" = "pid=" ]; then
  printf ' %%s\n' "$2"
  exit 0
fi
exit 1
`, mainPID, suspectPID, foreignConfigPath))
	writeExecutable(t, filepath.Join(fakeBin, "nc"), "#!/bin/sh\nexit 1\n")
	writeExecutable(t, filepath.Join(fakeBin, "dolt"), "#!/bin/sh\nexit 1\n")

	root := repoRoot(t)
	cmd := exec.Command("sh", filepath.Join(root, healthScript), "--json")
	cmd.Env = append(
		filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_HOST", "GC_DOLT_PORT",
			"GC_DOLT_USER", "GC_DOLT_PASSWORD", "GC_HEALTH_SKIP_ZOMBIE_SCAN", "PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT="+mainPort,
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("health.sh failed: %v\n%s", err, out)
	}
	output := string(out)

	if !strings.Contains(output, `"zombie_count": 1`) {
		t.Errorf("expected zombie_count 1 (PID-mismatched config does not protect from zombie status); got:\n%s", output)
	}
	if !strings.Contains(output, suspectPID) {
		t.Errorf("suspect PID %s with mismatched dolt.pid should still be flagged as zombie; got:\n%s", suspectPID, output)
	}
}

// TestHealthScriptZombieScanExcludesExternallyManagedServers verifies that a
// dolt sql-server launched with an explicit `--config <path>` whose config
// directory has NO sibling `dolt.pid` is treated as externally managed (e.g.
// a launchd-managed server for an unrelated app) and is NOT flagged as a
// zombie. Regression guard for the bug where a healthy, unrelated dolt server
// (a separate app's launchd-managed `dolt sql-server` on its own datadir and
// port) was counted as a zombie because the foreign-managed exclusion only
// recognized gc-managed siblings (config dir + matching dolt.pid). gc always
// writes a dolt.pid next to a managed config; the absence of that sibling is
// positive evidence the process is owned by another tool, not a town stray
// the deacon patrol may kill. A plain `dolt sql-server` with no `--config`
// (a genuine unidentified orphan) is still flagged, locking the boundary.
func TestHealthScriptZombieScanExcludesExternallyManagedServers(t *testing.T) {
	cityPath := t.TempDir()
	externalConfigDir := t.TempDir()
	fakeBin := t.TempDir()

	mainPort := "19905"
	mainPID := "424501"
	externalPID := "424502"
	zombiePID := "424503"

	// External app's dolt config: a config file with NO sibling dolt.pid.
	// This is the launchd-managed / non-gc shape from the field report.
	externalConfigPath := filepath.Join(externalConfigDir, "config.yaml")
	if err := os.WriteFile(externalConfigPath, []byte("# unrelated app dolt config\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// City .beads directory with metadata.
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"dolt_database":"city"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fake gc: fail so metadata_files() falls back to find.
	writeExecutable(t, filepath.Join(fakeBin, "gc"), "#!/bin/sh\nexit 1\n")

	// Fake pgrep: returns main PID, external PID, and a true zombie PID.
	writeExecutable(t, filepath.Join(fakeBin, "pgrep"),
		fmt.Sprintf("#!/bin/sh\necho %s\necho %s\necho %s\n", mainPID, externalPID, zombiePID))

	// Fake lsof: maps main port to mainPID.
	writeExecutable(t, filepath.Join(fakeBin, "lsof"),
		fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    -iTCP:%s) echo %s; exit 0 ;;
  esac
done
exit 1
`, mainPort, mainPID))

	// Fake ps: the bounded scan calls `ps -eo pid=,stat=,args=`. Emit the
	// external PID's args line WITH `--config <path>` so the awk pass extracts
	// it; its config dir has no dolt.pid sibling, so it must be excluded as an
	// externally-managed server. The others are plain sql-servers.
	writeExecutable(t, filepath.Join(fakeBin, "ps"), fmt.Sprintf(`#!/bin/sh
if [ "$1" = "-eo" ]; then
  echo "%s Sl dolt sql-server"
  echo "%s Sl dolt sql-server --config %s"
  echo "%s Sl dolt sql-server"
  exit 0
fi
if [ "$1" = "-p" ] && [ "$3" = "-o" ] && [ "$4" = "pid=" ]; then
  printf ' %%s\n' "$2"
  exit 0
fi
exit 1
`, mainPID, externalPID, externalConfigPath, zombiePID))

	// Fake nc: unreachable (no real server).
	writeExecutable(t, filepath.Join(fakeBin, "nc"), "#!/bin/sh\nexit 1\n")

	// Fake dolt: SELECT 1 fails (no real server).
	writeExecutable(t, filepath.Join(fakeBin, "dolt"), "#!/bin/sh\nexit 1\n")

	root := repoRoot(t)
	cmd := exec.Command("sh", filepath.Join(root, healthScript), "--json")
	cmd.Env = append(
		filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_HOST", "GC_DOLT_PORT",
			"GC_DOLT_USER", "GC_DOLT_PASSWORD", "GC_HEALTH_SKIP_ZOMBIE_SCAN", "PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT="+mainPort,
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("health.sh failed: %v\n%s", err, out)
	}
	output := string(out)

	// Only the true zombie (424503) should be counted; the externally-managed
	// server is excluded.
	if !strings.Contains(output, `"zombie_count": 1`) {
		t.Errorf("expected zombie_count 1; got:\n%s", output)
	}
	if strings.Contains(output, externalPID) {
		t.Errorf("externally-managed Dolt PID %s (--config, no dolt.pid sibling) should not be in zombie_pids; got:\n%s", externalPID, output)
	}
	if !strings.Contains(output, zombiePID) {
		t.Errorf("true zombie PID %s should be in zombie_pids; got:\n%s", zombiePID, output)
	}
}

// TestHealthScriptZombieScanIsBoundedFork is the regression guard for #2482:
// the zombie scan must enumerate the process table a bounded number of times,
// independent of how many dolt processes (especially Z-state zombies) exist.
// The old loop forked one `ps -p <pid> -o args=` per `pgrep -x dolt` match, so
// under a non-reaping PID 1 it became an O(zombies) `ps` storm re-paid every
// 30s. We drive the real run.sh with a pgrep that reports many dolt PIDs and a
// ps shim that logs every invocation, then assert zero per-PID `-o args=`
// forks while the orphaned sql-servers are still classified.
func TestHealthScriptZombieScanIsBoundedFork(t *testing.T) {
	const candidateCount = 50
	const firstPID = 500000

	cityPath := t.TempDir()
	fakeBin := t.TempDir()
	psLog := filepath.Join(t.TempDir(), "ps_calls")

	mainPort := "19901"
	serverPID := strconv.Itoa(firstPID) // first candidate is the managed city server

	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"),
		[]byte(`{"dolt_database":"city"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// pgrep reports candidateCount dolt PIDs — the candidate set the old loop
	// forked `ps -p <pid> -o args=` over, one per PID.
	var pgrepBody strings.Builder
	pgrepBody.WriteString("#!/bin/sh\n")
	for i := 0; i < candidateCount; i++ {
		fmt.Fprintf(&pgrepBody, "echo %d\n", firstPID+i)
	}
	writeExecutable(t, filepath.Join(fakeBin, "pgrep"), pgrepBody.String())

	// gc fails -> metadata_files falls back to find (no rigs here).
	writeExecutable(t, filepath.Join(fakeBin, "gc"), "#!/bin/sh\nexit 1\n")
	// lsof maps the city port to the server PID so server_pid resolves.
	writeExecutable(t, filepath.Join(fakeBin, "lsof"),
		fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in -iTCP:%s) echo %s; exit 0 ;; esac; done\nexit 1\n", mainPort, serverPID))
	writeExecutable(t, filepath.Join(fakeBin, "nc"), "#!/bin/sh\nexit 1\n")
	writeExecutable(t, filepath.Join(fakeBin, "dolt"), "#!/bin/sh\nexit 1\n")

	// ps shim: log every invocation, answer the port->pid confirmation
	// (`-p <pid> -o pid=`) and the single process-table pass (`-eo ...`).
	psShim := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
if [ "$1" = "-eo" ]; then
  i=0
  while [ "$i" -lt %d ]; do
    echo "$((%d + i)) Sl dolt sql-server"
    i=$((i + 1))
  done
  exit 0
fi
if [ "$1" = "-p" ] && [ "$3" = "-o" ] && [ "$4" = "pid=" ]; then
  printf ' %%s\n' "$2"
  exit 0
fi
exit 1
`, psLog, candidateCount, firstPID)
	writeExecutable(t, filepath.Join(fakeBin, "ps"), psShim)

	root := repoRoot(t)
	cmd := exec.Command("sh", filepath.Join(root, healthScript), "--json")
	cmd.Env = append(
		filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_HOST", "GC_DOLT_PORT",
			"GC_DOLT_USER", "GC_DOLT_PASSWORD", "GC_HEALTH_SKIP_ZOMBIE_SCAN", "PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT="+mainPort,
		"GC_DOLT_USER=root",
		"GC_DOLT_PASSWORD=",
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("health.sh failed: %v\n%s", err, out)
	}

	callsRaw, readErr := os.ReadFile(psLog)
	if readErr != nil {
		t.Fatalf("read ps log: %v", readErr)
	}
	calls := strings.Split(strings.TrimSpace(string(callsRaw)), "\n")
	perPIDForks := 0
	tableForks := 0
	for _, line := range calls {
		switch {
		case strings.Contains(line, "args=") && strings.Contains(line, "-p "):
			perPIDForks++
		case strings.HasPrefix(line, "-eo"):
			tableForks++
		}
	}
	if perPIDForks != 0 {
		t.Errorf("zombie scan made %d per-PID `ps -p <pid> -o args=` forks across %d candidates; want 0 (must use a single bounded pass)\nps calls:\n%s",
			perPIDForks, candidateCount, callsRaw)
	}
	if tableForks > 1 {
		t.Errorf("zombie scan ran %d full `ps -eo` passes; want at most 1\nps calls:\n%s", tableForks, callsRaw)
	}
	// All candidates are orphaned sql-servers except the managed city server,
	// which is excluded by the server_pid check.
	wantCount := fmt.Sprintf(`"zombie_count": %d`, candidateCount-1)
	if !strings.Contains(string(out), wantCount) {
		t.Errorf("expected %s (candidates minus the city server); got:\n%s", wantCount, out)
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
