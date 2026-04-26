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

func TestRuntimeScriptPortPrecedence(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, cityPath string) string
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
			name: "corrupt managed state ignores compatibility port mirror",
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
				return "3307"
			},
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
			if err != nil {
				t.Fatalf("runtime.sh failed: %v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != want {
				t.Fatalf("GC_DOLT_PORT = %q, want %q", got, want)
			}
		})
	}
}

func TestRuntimeScriptPortPrecedenceToleratesInconclusiveLsof(t *testing.T) {
	tests := []struct {
		name        string
		lsofBody    string
		ncBody      func(port string) string
		wantManaged bool
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
			wantManaged: false,
		},
		{
			name:     "inconclusive lsof with unreachable port still rejects port",
			lsofBody: "#!/bin/sh\nexit 0\n",
			ncBody: func(_ string) string {
				return `#!/bin/sh
exit 1
`
			},
			wantManaged: false,
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
			if err != nil {
				t.Fatalf("runtime.sh failed: %v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != want {
				t.Fatalf("GC_DOLT_PORT = %q, want %q", got, want)
			}
		})
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
	if err := os.WriteFile(filepath.Join(stateDir, "dolt-state.json"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
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
