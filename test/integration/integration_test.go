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
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/test/tmuxtest"
)

// gcBinary is the path to the built gc binary, set by TestMain.
var gcBinary string

// bdBinary is the path to the bd binary, discovered by TestMain.
var (
	bdBinary     string
	realBDBinary string
)

// testGCHome isolates integration-test supervisor state from the developer's
// real ~/.gc registry, config, and logs.
var testGCHome string

// testRuntimeDir isolates the supervisor lock/socket from the developer's
// real XDG runtime directory.
var testRuntimeDir string

var cityCommandEnv sync.Map

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
	if err := seedIsolatedDoltConfig(testGCHome); err != nil {
		panic("integration: writing dolt config: " + err.Error())
	}

	gcBinary = filepath.Join(tmpDir, "gc")
	buildCmd := exec.Command("go", "build", "-o", gcBinary, "./cmd/gc")
	buildCmd.Dir = findModuleRoot()
	buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		panic("integration: building gc binary: " + err.Error() + "\n" + string(out))
	}

	// Discover bd binary — required for bead operations.
	realBDBinary, err = exec.LookPath("bd")
	if err != nil {
		// bd not available — skip all integration tests.
		os.Exit(0)
	}
	bdBinary = filepath.Join(tmpDir, "bd")
	shimCmd := exec.Command("go", "build", "-o", bdBinary, "./test/integration/filebdshim")
	shimCmd.Dir = findModuleRoot()
	shimCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := shimCmd.CombinedOutput(); err != nil {
		panic("integration: building bd shim: " + err.Error() + "\n" + string(out))
	}
	if err := os.Setenv("GC_INTEGRATION_REAL_BD", realBDBinary); err != nil {
		panic("integration: setting GC_INTEGRATION_REAL_BD: " + err.Error())
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

	agentScript := filepath.Join(findModuleRoot(), "test", "agents", "graph-dispatch.sh")
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
	return runCommand(dir, commandEnvForDir(dir, false), integrationGCCommandTimeout, gcBinary, args...)
}

// gcDolt runs the gc binary with the given args using the isolated integration
// supervisor state, but without forcing GC_DOLT=skip. Use this for tests that
// need the real bd+dolt-backed bead store.
func gcDolt(dir string, args ...string) (string, error) {
	return runCommand(dir, commandEnvForDir(dir, true), integrationGCDoltCommandTimeout, gcBinary, args...)
}

// bd runs the bd binary with the given args. If dir is non-empty, it sets
// the working directory. Returns combined stdout+stderr and any error.
func bd(dir string, args ...string) (string, error) {
	out, err := runCommand(dir, commandEnvForDir(dir, false), integrationBDCommandTimeout, bdBinary, args...)
	if err == nil || !shouldUseFileStoreBDFallback(dir, out, args) {
		return out, err
	}
	return runFileStoreBD(dir, args...)
}

// bdDolt runs bd against a Dolt-backed city using the same isolated runtime
// env as integration gc commands plus the city's managed Dolt port.
func bdDolt(dir string, args ...string) (string, error) {
	env := commandEnvForDir(dir, true)
	if dir != "" {
		env = filterEnv(env, "GC_CITY")
		env = filterEnv(env, "GC_CITY_PATH")
		env = filterEnv(env, "GC_CITY_ROOT")
		env = filterEnv(env, "GC_CITY_RUNTIME_DIR")
		env = append(env,
			"GC_CITY="+dir,
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

func runGCWithEnv(env []string, dir string, args ...string) (string, error) {
	return runCommand(dir, env, integrationGCCommandTimeout, gcBinary, args...)
}

func runGCDoltWithEnv(env []string, dir string, args ...string) (string, error) {
	return runCommand(dir, env, integrationGCDoltCommandTimeout, gcBinary, args...)
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

func shouldUseFileStoreBDFallback(dir, output string, args []string) bool {
	if dir == "" || len(args) == 0 || args[0] == "init" {
		return false
	}
	if !strings.Contains(output, "no beads database found") {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, ".gc", "beads.json"))
	return err == nil
}

func runFileStoreBD(dir string, args ...string) (string, error) {
	store, recorder, err := openFileStoreBeads(dir)
	if err != nil {
		return "", err
	}
	defer recorder.Close() //nolint:errcheck // best-effort test cleanup

	switch args[0] {
	case "create":
		if len(args) < 2 {
			return "", fmt.Errorf("bd create: missing title")
		}
		created, err := store.Create(beads.Bead{Title: args[1]})
		if err != nil {
			return "", err
		}
		recorder.Record(events.Event{
			Type:    events.BeadCreated,
			Actor:   "human",
			Subject: created.ID,
			Message: created.Title,
		})
		return fmt.Sprintf("Created bead: %s\n", created.ID), nil
	case "show":
		if len(args) < 2 {
			return "", fmt.Errorf("bd show: missing bead id")
		}
		b, err := store.Get(args[1])
		if err != nil {
			return "", err
		}
		return renderFileStoreBead(b), nil
	case "list":
		items, err := store.List(beads.ListQuery{AllowScan: true})
		if err != nil {
			return "", err
		}
		return renderFileStoreBeadList(items), nil
	case "close":
		if len(args) < 2 {
			return "", fmt.Errorf("bd close: missing bead id")
		}
		if err := store.Close(args[1]); err != nil {
			return "", err
		}
		recorder.Record(events.Event{
			Type:    events.BeadClosed,
			Actor:   "human",
			Subject: args[1],
		})
		return "", nil
	case "update":
		if len(args) < 2 {
			return "", fmt.Errorf("bd update: missing bead id")
		}
		var opts beads.UpdateOpts
		supported := false
		for _, arg := range args[2:] {
			if strings.HasPrefix(arg, "--assignee=") {
				assignee := strings.TrimPrefix(arg, "--assignee=")
				opts.Assignee = &assignee
				supported = true
			}
		}
		if !supported {
			return "", fmt.Errorf("bd update fallback only supports --assignee")
		}
		if err := store.Update(args[1], opts); err != nil {
			return "", err
		}
		recorder.Record(events.Event{
			Type:    events.BeadUpdated,
			Actor:   "human",
			Subject: args[1],
		})
		return "", nil
	default:
		return "", fmt.Errorf("bd %s not supported by file-store fallback", args[0])
	}
}

func openFileStoreBeads(dir string) (beads.Store, *events.FileRecorder, error) {
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(dir, ".gc", "beads.json"))
	if err != nil {
		return nil, nil, err
	}
	store.SetLocker(beads.NewFileFlock(filepath.Join(dir, ".gc", "beads.json.lock")))
	recorder, err := events.NewFileRecorder(filepath.Join(dir, ".gc", "events.jsonl"), io.Discard)
	if err != nil {
		return nil, nil, err
	}
	return store, recorder, nil
}

func renderFileStoreBead(b beads.Bead) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "ID: %s\n", b.ID)
	fmt.Fprintf(&sb, "Title: %s\n", b.Title)
	fmt.Fprintf(&sb, "Status: %s\n", b.Status)
	if b.Assignee != "" {
		fmt.Fprintf(&sb, "Assignee: %s\n", b.Assignee)
	}
	return sb.String()
}

func renderFileStoreBeadList(items []beads.Bead) string {
	if len(items) == 0 {
		return "No beads.\n"
	}
	var sb strings.Builder
	for _, b := range items {
		fmt.Fprintf(&sb, "%s  %s  %s\n", b.ID, b.Status, b.Title)
	}
	return sb.String()
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
	return integrationEnvFor(testGCHome, testRuntimeDir, false)
}

func integrationEnvDolt() []string {
	return integrationEnvFor(testGCHome, testRuntimeDir, true)
}

func integrationEnvFor(gcHome, runtimeDir string, useDolt bool) []string {
	env := filterEnv(os.Environ(), "GC_BEADS")
	env = filterEnv(env, "GC_DOLT")
	env = filterEnv(env, "PATH")
	env = filterEnv(env, "HOME")
	env = filterEnv(env, "GC_HOME")
	env = filterEnv(env, "XDG_RUNTIME_DIR")
	env = filterEnv(env, "GC_INTEGRATION_REAL_BD")
	env = filterEnv(env, "DOLT_ROOT_PATH")
	if !useDolt {
		env = append(env, "GC_DOLT=skip")
	}
	env = append(env, "HOME="+gcHome)
	env = append(env, "GC_HOME="+gcHome)
	env = append(env, "XDG_RUNTIME_DIR="+runtimeDir)
	env = append(env, "GC_INTEGRATION_REAL_BD="+realBDBinary)
	env = append(env, "DOLT_ROOT_PATH="+gcHome)
	env = append(env, "PATH="+filepath.Dir(gcBinary)+":"+filepath.Dir(bdBinary)+":"+os.Getenv("PATH"))
	return env
}

func newIsolatedCommandEnv(t *testing.T, useDolt bool) []string {
	t.Helper()

	root := t.TempDir()
	gcHome := filepath.Join(root, "gc-home")
	runtimeDir := filepath.Join(root, "runtime")
	if err := os.MkdirAll(gcHome, 0o755); err != nil {
		t.Fatalf("creating isolated GC_HOME: %v", err)
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("creating isolated runtime dir: %v", err)
	}
	port, err := reserveLoopbackPort()
	if err != nil {
		t.Fatalf("reserving isolated supervisor port: %v", err)
	}
	supervisorConfig := fmt.Sprintf("[supervisor]\nport = %d\nbind = \"127.0.0.1\"\n", port)
	if err := os.WriteFile(filepath.Join(gcHome, "supervisor.toml"), []byte(supervisorConfig), 0o644); err != nil {
		t.Fatalf("writing isolated supervisor config: %v", err)
	}
	if err := seedIsolatedDoltConfig(gcHome); err != nil {
		t.Fatalf("writing isolated dolt config: %v", err)
	}
	env := integrationEnvFor(gcHome, runtimeDir, useDolt)

	shimDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatalf("creating isolated shim dir: %v", err)
	}
	for _, name := range []string{"systemctl", "launchctl"} {
		path := filepath.Join(shimDir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("writing %s shim: %v", name, err)
		}
	}
	envMap := parseEnvList(env)
	env = replaceEnv(env, "PATH", shimDir+":"+envMap["PATH"])
	startIsolatedSupervisor(t, env, gcHome)
	return env
}

func seedIsolatedDoltConfig(gcHome string) error {
	doltDir := filepath.Join(gcHome, ".dolt")
	if err := os.MkdirAll(doltDir, 0o755); err != nil {
		return err
	}
	doltCfg := `{"user.name":"gc-test","user.email":"gc-test@test.local"}`
	return os.WriteFile(filepath.Join(doltDir, "config_global.json"), []byte(doltCfg), 0o644)
}

func registerCityCommandEnv(cityDir string, env []string) {
	cityCommandEnv.Store(cityDir, append([]string(nil), env...))
}

func unregisterCityCommandEnv(cityDir string) {
	cityCommandEnv.Delete(cityDir)
}

func commandEnvForDir(dir string, useDolt bool) []string {
	if dir != "" {
		if env, ok := cityCommandEnv.Load(dir); ok {
			return append([]string(nil), env.([]string)...)
		}
	}
	if useDolt {
		return integrationEnvDolt()
	}
	return integrationEnv()
}

func replaceEnv(env []string, name, value string) []string {
	env = filterEnv(env, name)
	return append(env, name+"="+value)
}

func startIsolatedSupervisor(t *testing.T, env []string, gcHome string) {
	t.Helper()

	logPath := filepath.Join(gcHome, "supervisor.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("creating isolated supervisor log: %v", err)
	}

	cmd := exec.Command(gcBinary, "supervisor", "run")
	cmd.Dir = gcHome
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("starting isolated supervisor: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out, err := runCommand("", env, 2*time.Second, gcBinary, "supervisor", "status")
		if err == nil && strings.Contains(out, "Supervisor is running") {
			t.Cleanup(func() {
				_, _ = runCommand("", env, 5*time.Second, gcBinary, "supervisor", "stop")
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					if cmd.Process != nil {
						_ = cmd.Process.Kill()
					}
					<-done
				}
				_ = logFile.Close()
			})
			return
		}
		select {
		case err := <-done:
			_ = logFile.Close()
			logData, _ := os.ReadFile(logPath)
			if err == nil {
				t.Fatalf("isolated supervisor exited early:\n%s", string(logData))
			}
			t.Fatalf("isolated supervisor exited early: %v\n%s", err, string(logData))
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}

	_ = logFile.Close()
	logData, _ := os.ReadFile(logPath)
	t.Fatalf("isolated supervisor did not become ready:\n%s", string(logData))
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

func TestIntegrationEnvForUsesIsolatedHome(t *testing.T) {
	oldGCHome, oldRuntimeDir := testGCHome, testRuntimeDir
	oldGCBinary, oldBDBinary, oldRealBDBinary := gcBinary, bdBinary, realBDBinary
	t.Cleanup(func() {
		testGCHome = oldGCHome
		testRuntimeDir = oldRuntimeDir
		gcBinary = oldGCBinary
		bdBinary = oldBDBinary
		realBDBinary = oldRealBDBinary
	})

	testGCHome = filepath.Join(t.TempDir(), "gc-home")
	testRuntimeDir = filepath.Join(t.TempDir(), "runtime")
	gcBinary = filepath.Join(t.TempDir(), "gc")
	bdBinary = filepath.Join(t.TempDir(), "bd")
	realBDBinary = "/usr/bin/bd"

	t.Setenv("HOME", "/host/home")
	env := integrationEnv()
	got := parseEnvList(env)

	if got["HOME"] != testGCHome {
		t.Fatalf("HOME = %q, want %q", got["HOME"], testGCHome)
	}
	if got["GC_HOME"] != testGCHome {
		t.Fatalf("GC_HOME = %q, want %q", got["GC_HOME"], testGCHome)
	}
	if got["XDG_RUNTIME_DIR"] != testRuntimeDir {
		t.Fatalf("XDG_RUNTIME_DIR = %q, want %q", got["XDG_RUNTIME_DIR"], testRuntimeDir)
	}
}

func TestCommandEnvForDirPrefersRegisteredCityEnv(t *testing.T) {
	cityDir := filepath.Join(t.TempDir(), "city")
	want := []string{"HOME=/tmp/isolated", "GC_HOME=/tmp/isolated", "PATH=/tmp/bin"}
	registerCityCommandEnv(cityDir, want)
	t.Cleanup(func() { unregisterCityCommandEnv(cityDir) })

	got := commandEnvForDir(cityDir, false)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commandEnvForDir(%q) = %v, want %v", cityDir, got, want)
	}
}

func parseEnvList(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}

// mainTB is a minimal testing.TB implementation for use in TestMain where
// no *testing.T is available. Only Helper() and Logf() are called by
// KillAllTestSessions.
type mainTB struct{ testing.TB }

func (mainTB) Helper()                         {}
func (mainTB) Logf(format string, args ...any) {}
