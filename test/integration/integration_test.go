//go:build integration

// Package integration provides end-to-end tests that exercise the real gc
// binary against real session providers (tmux or subprocess). Tests validate
// the tutorial experiences: gc init, gc start, gc stop, bead CRUD, etc.
//
// By default tests use tmux. Set GC_SESSION=subprocess to use the subprocess
// provider instead (no tmux required).
//
// Session safety: all test cities use the "gctest-<8hex>" naming prefix.
// Three layers of cleanup (pre-sweep, per-test t.Cleanup, post-sweep)
// prevent orphan tmux sessions on developer boxes.
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
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/test/tmuxtest"
)

// gcBinary is the path to the built gc binary, set by TestMain.
var gcBinary string

// bdBinary is the path to the bd binary, discovered by TestMain.
var bdBinary string

// testGCHome isolates integration-test supervisor state from the developer's
// real ~/.gc registry, config, and logs.
var testGCHome string

// testRuntimeDir isolates the supervisor lock/socket from the developer's
// real XDG runtime directory.
var testRuntimeDir string

const (
	integrationGCCommandTimeout     = 60 * time.Second
	integrationGCDoltCommandTimeout = 120 * time.Second
	integrationBDCommandTimeout     = 15 * time.Second
)

// TestMain builds the gc binary and runs pre/post sweeps of orphan sessions.
func TestMain(m *testing.M) {
	subprocess := os.Getenv("GC_SESSION") == "subprocess"

	// Tmux check: skip all tests if tmux not available AND not using subprocess.
	if !subprocess {
		if _, err := exec.LookPath("tmux"); err != nil {
			os.Exit(0)
		}
		// Pre-sweep: kill any orphaned gc-gctest-* sessions from prior crashes.
		tmuxtest.KillAllTestSessions(&mainTB{})
	} else {
		// Best-effort pre-sweep of stale subprocess integration cities and
		// their descendant pollers from prior interrupted runs.
		sweepSubprocessTestProcesses()
	}

	// Build gc binary to a temp directory.
	tmpDir, err := os.MkdirTemp("", "gc-integration-*")
	if err != nil {
		panic("integration: creating temp dir: " + err.Error())
	}
	defer os.RemoveAll(tmpDir)

	testGCHome = filepath.Join(tmpDir, "gc-home")
	if err := os.MkdirAll(testGCHome, 0o755); err != nil {
		panic("integration: creating GC_HOME: " + err.Error())
	}
	testRuntimeDir = filepath.Join(tmpDir, "runtime")
	if err := os.MkdirAll(testRuntimeDir, 0o755); err != nil {
		panic("integration: creating XDG_RUNTIME_DIR: " + err.Error())
	}
	port, err := reserveLoopbackPort()
	if err != nil {
		panic("integration: reserving supervisor port: " + err.Error())
	}
	supervisorConfig := fmt.Sprintf("[supervisor]\nport = %d\nbind = \"127.0.0.1\"\n", port)
	if err := os.WriteFile(filepath.Join(testGCHome, "supervisor.toml"), []byte(supervisorConfig), 0o644); err != nil {
		panic("integration: writing supervisor config: " + err.Error())
	}

	gcBinary = filepath.Join(tmpDir, "gc")
	buildCmd := exec.Command("go", "build", "-o", gcBinary, "./cmd/gc")
	buildCmd.Dir = findModuleRoot()
	buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		panic("integration: building gc binary: " + err.Error() + "\n" + string(out))
	}

	// Discover bd binary — required for bead operations.
	bdBinary, err = exec.LookPath("bd")
	if err != nil {
		// bd not available — skip all integration tests.
		os.Exit(0)
	}

	// Run tests.
	code := m.Run()

	// Best-effort: stop any isolated supervisor that survived test cleanup.
	if gcBinary != "" {
		stopCmd := exec.Command(gcBinary, "supervisor", "stop")
		stopCmd.Env = integrationEnv()
		_ = stopCmd.Run()
	}

	// Post-sweep: clean up any sessions that survived individual test cleanup.
	if !subprocess {
		tmuxtest.KillAllTestSessions(&mainTB{})
	} else {
		sweepSubprocessTestProcesses()
	}

	os.Exit(code)
}

type procSnapshot struct {
	pid  int
	ppid int
	cmd  string
}

func sweepSubprocessTestProcesses() {
	procs := readProcessSnapshot()
	if len(procs) == 0 {
		return
	}

	agentScript := filepath.Join(findModuleRoot(), "test", "agents", "graph-workflow.sh")
	roots := make(map[int]bool)
	for pid, info := range procs {
		if isSubprocessTestRoot(info.cmd, agentScript) {
			roots[pid] = true
		}
	}

	killSet := make(map[int]bool)
	for pid, info := range procs {
		if isSubprocessTestLeaf(info.cmd, agentScript) || hasProcessAncestor(pid, roots, procs) {
			killSet[pid] = true
		}
	}
	if len(killSet) == 0 {
		return
	}

	for pid := range killSet {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	time.Sleep(150 * time.Millisecond)
	for pid := range killSet {
		if err := syscall.Kill(pid, syscall.Signal(0)); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

func readProcessSnapshot() map[int]procSnapshot {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	procs := make(map[int]procSnapshot)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil || len(cmdline) == 0 {
			continue
		}
		status, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			continue
		}
		ppid := parsePPid(string(status))
		if ppid == 0 {
			continue
		}
		cmd := strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " "))
		if cmd == "" {
			continue
		}
		procs[pid] = procSnapshot{pid: pid, ppid: ppid, cmd: cmd}
	}
	return procs
}

func parsePPid(status string) int {
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, "PPid:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0
		}
		return ppid
	}
	return 0
}

func isSubprocessTestRoot(cmd, agentScript string) bool {
	switch {
	case strings.Contains(cmd, agentScript):
		return true
	case strings.Contains(cmd, "gc convoy control --serve --follow control-dispatcher") && strings.Contains(cmd, "gc-integration-"):
		return true
	case strings.Contains(cmd, "gc supervisor run") && strings.Contains(cmd, "gc-integration-"):
		return true
	default:
		return false
	}
}

func isSubprocessTestLeaf(cmd, agentScript string) bool {
	switch {
	case strings.Contains(cmd, "bd ready --label=pool:polecat --unassigned --json --limit=1"):
		return true
	case strings.Contains(cmd, "bd ready --assignee=worker --json --limit=1"):
		return true
	case strings.Contains(cmd, agentScript):
		return true
	default:
		return false
	}
}

func hasProcessAncestor(pid int, roots map[int]bool, procs map[int]procSnapshot) bool {
	seen := make(map[int]bool)
	cur := pid
	for cur != 0 && !seen[cur] {
		seen[cur] = true
		if roots[cur] {
			return true
		}
		info, ok := procs[cur]
		if !ok {
			return false
		}
		cur = info.ppid
	}
	return false
}

// gc runs the gc binary with the given args. If dir is non-empty, it sets
// the working directory. Returns combined stdout+stderr and any error.
func gc(dir string, args ...string) (string, error) {
	return runCommand(dir, integrationEnv(), integrationGCCommandTimeout, gcBinary, args...)
}

// gcDolt runs the gc binary with the given args using the isolated integration
// supervisor state, but without forcing GC_DOLT=skip. Use this for tests that
// need the real bd+dolt-backed bead store.
func gcDolt(dir string, args ...string) (string, error) {
	return runCommand(dir, integrationEnvDolt(), integrationGCDoltCommandTimeout, gcBinary, args...)
}

// bd runs the bd binary with the given args. If dir is non-empty, it sets
// the working directory. Returns combined stdout+stderr and any error.
func bd(dir string, args ...string) (string, error) {
	return runCommand(dir, os.Environ(), integrationBDCommandTimeout, bdBinary, args...)
}

// bdDolt runs bd against a Dolt-backed city using the same isolated runtime
// env as integration gc commands plus the city's managed Dolt port.
func bdDolt(dir string, args ...string) (string, error) {
	env := integrationEnvDolt()
	if dir != "" {
		env = filterEnv(env, "GC_CITY")
		env = filterEnv(env, "GC_CITY_ROOT")
		env = filterEnv(env, "GC_CITY_PATH")
		env = filterEnv(env, "GC_CITY_RUNTIME_DIR")
		env = append(env,
			"GC_CITY="+dir,
			"GC_CITY_ROOT="+dir,
			"GC_CITY_PATH="+dir,
			"GC_CITY_RUNTIME_DIR="+filepath.Join(dir, ".gc", "runtime"),
		)
		if data, err := os.ReadFile(filepath.Join(dir, ".beads", "dolt-server.port")); err == nil {
			port := strings.TrimSpace(string(data))
			if port != "" {
				env = filterEnv(env, "GC_DOLT_PORT")
				env = append(env, "GC_DOLT_PORT="+port)
			}
		}
	}
	return runCommand(dir, env, integrationBDCommandTimeout, bdBinary, args...)
}

func runCommand(dir string, env []string, timeout time.Duration, binary string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	output := string(out)
	if ctx.Err() == context.DeadlineExceeded {
		return output, fmt.Errorf("timed out after %s running %s", timeout, renderCommand(binary, args...))
	}
	return output, err
}

func renderCommand(binary string, args ...string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, binary)
	parts = append(parts, args...)
	return strings.Join(parts, " ")
}

// findModuleRoot walks up from the current directory to find go.mod.
func findModuleRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		panic("integration: getting cwd: " + err.Error())
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("integration: go.mod not found")
		}
		dir = parent
	}
}

// filterEnv returns env with the named variable removed.
func filterEnv(env []string, name string) []string {
	prefix := name + "="
	result := make([]string, 0, len(env))
	for _, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			continue
		}
		result = append(result, e)
	}
	return result
}

func integrationEnv() []string {
	// Skip dolt server lifecycle so tests don't require dolt.
	// Prepend gc/bd binary dirs so agent sessions can find test binaries.
	env := filterEnv(os.Environ(), "GC_BEADS")
	env = filterEnv(env, "GC_DOLT")
	env = filterEnv(env, "PATH")
	env = filterEnv(env, "GC_HOME")
	env = filterEnv(env, "XDG_RUNTIME_DIR")
	env = append(env, "GC_DOLT=skip")
	env = append(env, "GC_HOME="+testGCHome)
	env = append(env, "XDG_RUNTIME_DIR="+testRuntimeDir)
	env = append(env, "PATH="+filepath.Dir(gcBinary)+":"+filepath.Dir(bdBinary)+":"+os.Getenv("PATH"))
	return env
}

func integrationEnvDolt() []string {
	env := filterEnv(os.Environ(), "GC_BEADS")
	env = filterEnv(env, "GC_DOLT")
	env = filterEnv(env, "PATH")
	env = filterEnv(env, "GC_HOME")
	env = filterEnv(env, "XDG_RUNTIME_DIR")
	env = append(env, "GC_HOME="+testGCHome)
	env = append(env, "XDG_RUNTIME_DIR="+testRuntimeDir)
	env = append(env, "PATH="+filepath.Dir(gcBinary)+":"+filepath.Dir(bdBinary)+":"+os.Getenv("PATH"))
	return env
}

func reserveLoopbackPort() (int, error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer lis.Close() //nolint:errcheck
	addr, ok := lis.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected addr type %T", lis.Addr())
	}
	return addr.Port, nil
}

// mainTB is a minimal testing.TB implementation for use in TestMain where
// no *testing.T is available. Only Helper() and Logf() are called by
// KillAllTestSessions.
type mainTB struct{ testing.TB }

func (mainTB) Helper()                         {}
func (mainTB) Logf(format string, args ...any) {}
