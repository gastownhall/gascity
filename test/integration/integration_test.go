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
	"encoding/json"
	"errors"
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
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/test/tmuxtest"
)

// gcBinary is the path to the built gc binary, set by TestMain.
var gcBinary string

// bdBinary is the path to the bd binary, discovered by TestMain.
var (
	bdBinary              string
	realBDBinary          string
	doltBinary            string
	integrationToolBinDir string
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
	integrationGCLifecycleTimeout   = 120 * time.Second
	integrationGCDoltCommandTimeout = 120 * time.Second
	integrationBDCommandTimeout     = 15 * time.Second
)

const (
	integrationGCBinaryEnv     = "GC_INTEGRATION_GC_BINARY"
	integrationRealBDBinaryEnv = "GC_INTEGRATION_REAL_BD"
	integrationDoltBinaryEnv   = "GC_INTEGRATION_DOLT_BINARY"
	integrationDoltIdentityEnv = "GC_INTEGRATION_DOLT_IDENTITY_MODE"
	doltIdentityModeIsolated   = "isolated"
	doltIdentityModeGlobal     = "global"
	doltIdentityModeSkip       = "skip"
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
	integrationToolBinDir = filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(integrationToolBinDir, 0o755); err != nil {
		panic("integration: creating integration tool bin dir: " + err.Error())
	}

	if override, ok, err := binaryOverride(integrationGCBinaryEnv); err != nil {
		panic("integration: resolving GC override: " + err.Error())
	} else if ok {
		gcBinary = filepath.Join(integrationToolBinDir, "gc")
		if err := writeExecShim(gcBinary, override); err != nil {
			panic("integration: writing gc shim: " + err.Error())
		}
	} else {
		gcBinary = filepath.Join(integrationToolBinDir, "gc")
		buildCmd := exec.Command("go", "build", "-o", gcBinary, "./cmd/gc")
		buildCmd.Dir = findModuleRoot()
		buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := buildCmd.CombinedOutput(); err != nil {
			panic("integration: building gc binary: " + err.Error() + "\n" + string(out))
		}
	}

	if override, ok, err := binaryOverride(integrationRealBDBinaryEnv); err != nil {
		panic("integration: resolving bd override: " + err.Error())
	} else if ok {
		realBDBinary = override
	} else {
		var err error
		realBDBinary, err = exec.LookPath("bd")
		if err != nil {
			// bd not available — skip all integration tests.
			os.Exit(0)
		}
	}
	bdBinary = filepath.Join(integrationToolBinDir, "bd")
	shimCmd := exec.Command("go", "build", "-o", bdBinary, "./test/integration/filebdshim")
	shimCmd.Dir = findModuleRoot()
	shimCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := shimCmd.CombinedOutput(); err != nil {
		panic("integration: building bd shim: " + err.Error() + "\n" + string(out))
	}
	if err := os.Setenv(integrationRealBDBinaryEnv, realBDBinary); err != nil {
		panic("integration: setting GC_INTEGRATION_REAL_BD: " + err.Error())
	}

	if override, ok, err := binaryOverride(integrationDoltBinaryEnv); err != nil {
		panic("integration: resolving dolt override: " + err.Error())
	} else if ok {
		doltBinary = filepath.Join(integrationToolBinDir, "dolt")
		if err := writeExecShim(doltBinary, override); err != nil {
			panic("integration: writing dolt shim: " + err.Error())
		}
	} else if resolved, err := exec.LookPath("dolt"); err == nil {
		doltBinary = filepath.Join(integrationToolBinDir, "dolt")
		if err := writeExecShim(doltBinary, resolved); err != nil {
			panic("integration: writing dolt shim: " + err.Error())
		}
	}

	port, err := reserveLoopbackPort()
	if err != nil {
		panic("integration: reserving supervisor port: " + err.Error())
	}
	supervisorConfig := fmt.Sprintf("[supervisor]\nport = %d\nbind = \"127.0.0.1\"\n", port)
	if err := os.WriteFile(filepath.Join(testGCHome, "supervisor.toml"), []byte(supervisorConfig), 0o644); err != nil {
		panic("integration: writing supervisor config: " + err.Error())
	}
	if err := seedDoltIdentityForRoot(testGCHome); err != nil {
		panic("integration: writing dolt config: " + err.Error())
	}

	// Run tests.
	code := m.Run()

	// Best-effort: stop any isolated supervisor that survived test cleanup.
	// Use --wait so the sweep blocks until the supervisor and its managed
	// cities have actually shut down, avoiding a race with process-table
	// cleanup below.
	if gcBinary != "" {
		stopCmd := exec.Command(gcBinary, "supervisor", "stop", "--wait")
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

func binaryOverride(envName string) (string, bool, error) {
	raw := strings.TrimSpace(os.Getenv(envName))
	if raw == "" {
		return "", false, nil
	}
	path := raw
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", false, fmt.Errorf("%s=%q: make absolute: %w", envName, raw, err)
		}
		path = abs
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", false, fmt.Errorf("%s=%q: %w", envName, raw, err)
	}
	if info.IsDir() {
		return "", false, fmt.Errorf("%s=%q points to a directory", envName, raw)
	}
	return path, true, nil
}

func writeExecShim(path, target string) error {
	script := "#!/bin/sh\nexec " + singleQuoteShell(target) + ` "$@"` + "\n"
	return os.WriteFile(path, []byte(script), 0o755)
}

func singleQuoteShell(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
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
	killSet := subprocessTestKillSet(procs, agentScript)
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

func subprocessTestKillSet(procs map[int]procSnapshot, agentScript string) map[int]bool {
	roots := make(map[int]bool)
	children := make(map[int][]int, len(procs))
	for pid, info := range procs {
		if isSubprocessTestRoot(info.cmd, agentScript) {
			roots[pid] = true
		}
		children[info.ppid] = append(children[info.ppid], pid)
	}

	killSet := make(map[int]bool)
	queue := make([]int, 0, len(roots))
	for pid := range roots {
		queue = append(queue, pid)
	}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if killSet[pid] {
			continue
		}
		killSet[pid] = true
		queue = append(queue, children[pid]...)
	}

	for pid, info := range procs {
		if isSubprocessTestLeaf(info.cmd, agentScript) {
			killSet[pid] = true
		}
	}
	return killSet
}

// gc runs the gc binary with the given args. If dir is non-empty, it sets
// the working directory. Returns combined stdout+stderr and any error.
func gc(dir string, args ...string) (string, error) {
	return runCommand(dir, commandEnvForDir(commandEnvLookupDir(dir, args), false), gcCommandTimeout(args), gcBinary, args...)
}

// gcDolt runs the gc binary with the given args using the isolated integration
// supervisor state, but without forcing GC_DOLT=skip. Use this for tests that
// need the real bd+dolt-backed bead store.
func gcDolt(dir string, args ...string) (string, error) {
	return runCommand(dir, commandEnvForDir(commandEnvLookupDir(dir, args), true), integrationGCDoltCommandTimeout, gcBinary, args...)
}

func commandEnvLookupDir(dir string, args []string) string {
	if dir != "" {
		return dir
	}
	for _, arg := range args {
		if _, ok := cityCommandEnv.Load(arg); ok {
			return arg
		}
	}
	return ""
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
		if port, ok := ensureManagedDoltPortForTest(dir); ok {
			env = filterEnv(env, "GC_DOLT_PORT")
			env = append(env, "GC_DOLT_PORT="+port)
		}
	}
	out, err := runCommand(dir, env, integrationBDCommandTimeout, bdBinary, args...)
	if err == nil || dir == "" || !managedDoltTransportRetryable(out) {
		return out, err
	}
	if port, ok := ensureManagedDoltPortForTest(dir); ok {
		env = filterEnv(env, "GC_DOLT_PORT")
		env = append(env, "GC_DOLT_PORT="+port)
		return runCommand(dir, env, integrationBDCommandTimeout, bdBinary, args...)
	}
	return out, err
}

func runGCWithEnv(env []string, dir string, args ...string) (string, error) {
	return runCommand(dir, env, gcCommandTimeout(args), gcBinary, args...)
}

func runGCDoltWithEnv(env []string, dir string, args ...string) (string, error) {
	return runCommand(dir, env, integrationGCDoltCommandTimeout, gcBinary, args...)
}

func gcCommandTimeout(args []string) time.Duration {
	if len(args) == 0 {
		return integrationGCCommandTimeout
	}
	switch args[0] {
	case "init", "start", "stop", "restart":
		return integrationGCLifecycleTimeout
	case "supervisor":
		if len(args) > 1 && args[1] == "stop" {
			return integrationGCLifecycleTimeout
		}
	}
	return integrationGCCommandTimeout
}

func runCommand(dir string, env []string, timeout time.Duration, binary string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = 2 * time.Second
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	output := string(out)
	if ctx.Err() == context.DeadlineExceeded {
		return output, fmt.Errorf("timed out after %s running %s", timeout, renderCommand(binary, args...))
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return output, nil
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
	env = filterEnv(env, "GC_HOME")
	env = filterEnv(env, "XDG_RUNTIME_DIR")
	env = filterEnv(env, integrationRealBDBinaryEnv)
	env = filterEnv(env, "DOLT_ROOT_PATH")
	env = filterEnv(env, "GC_DOLT_HOST")
	env = filterEnv(env, "GC_DOLT_PORT")
	env = filterEnv(env, "GC_DOLT_USER")
	env = filterEnv(env, "GC_DOLT_PASSWORD")
	env = filterEnv(env, "BEADS_DOLT_SERVER_HOST")
	env = filterEnv(env, "BEADS_DOLT_SERVER_PORT")
	env = filterEnv(env, "BEADS_DOLT_SERVER_USER")
	env = filterEnv(env, "BEADS_DOLT_PASSWORD")
	env = filterEnv(env, integrationGCBinaryEnv)
	env = filterEnv(env, integrationDoltBinaryEnv)
	env = filterEnv(env, "BEADS_DOLT_AUTO_START")
	if !useDolt {
		env = append(env, "GC_DOLT=skip")
	}
	env = append(env, "GC_HOME="+gcHome)
	env = append(env, "XDG_RUNTIME_DIR="+runtimeDir)
	env = append(env, integrationRealBDBinaryEnv+"="+realBDBinary)
	env = append(env, "DOLT_ROOT_PATH="+gcHome)
	env = append(env, "PATH="+prependPath(integrationToolBinDir, os.Getenv("PATH")))
	// Match production: suppress bd's CLI Dolt auto-start so integration
	// tests can't spawn rogue servers when the managed Dolt port file is
	// stale between subtests. bd's auto-start logic ignores the
	// dolt.auto-start:false config written into .beads/config.yaml
	// (resolveAutoStart priority bug), so the env var is the only
	// reliable kill-switch. Mirrors bdRuntimeEnv in cmd/gc/bd_env.go.
	env = append(env, "BEADS_DOLT_AUTO_START=0")
	return env
}

func prependPath(paths ...string) string {
	parts := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		parts = append(parts, path)
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

func newIsolatedToolEnv(t *testing.T, useDolt bool) []string {
	t.Helper()

	_, _, env := newIsolatedEnvRoot(t, useDolt)
	return env
}

func newIsolatedCommandEnv(t *testing.T, useDolt bool) []string {
	t.Helper()

	gcHome, _, env := newIsolatedEnvRoot(t, useDolt)

	root := filepath.Dir(gcHome)
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
	env = replaceEnv(env, "PATH", prependPath(shimDir, envMap["PATH"]))
	startIsolatedSupervisor(t, env, gcHome)
	return env
}

func newIsolatedEnvRoot(t *testing.T, useDolt bool) (string, string, []string) {
	t.Helper()

	root, err := os.MkdirTemp("", "gc-int-env-")
	if err != nil {
		t.Fatalf("creating isolated env root: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(root)
	})
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
	if err := seedDoltIdentityForRoot(gcHome); err != nil {
		t.Fatalf("writing isolated dolt config: %v", err)
	}
	env := integrationEnvFor(gcHome, runtimeDir, useDolt)
	return gcHome, runtimeDir, env
}

func seedDoltIdentityForRoot(gcHome string) error {
	switch mode := doltIdentityMode(); mode {
	case doltIdentityModeIsolated:
		return seedIsolatedDoltConfig(gcHome)
	case doltIdentityModeSkip:
		return nil
	case doltIdentityModeGlobal:
		if err := ensureGlobalDoltIdentity(); err != nil {
			return err
		}
		return seedIsolatedDoltConfig(gcHome)
	default:
		return fmt.Errorf("%s=%q is invalid", integrationDoltIdentityEnv, mode)
	}
}

func doltIdentityMode() string {
	mode := strings.TrimSpace(os.Getenv(integrationDoltIdentityEnv))
	if mode == "" {
		return doltIdentityModeIsolated
	}
	return mode
}

func ensureGlobalDoltIdentity() error {
	if doltBinary == "" {
		return fmt.Errorf("dolt binary is required when %s=%s", integrationDoltIdentityEnv, doltIdentityModeGlobal)
	}

	name, _ := trimmedCommandOutput(doltBinary, "config", "--global", "--get", "user.name")
	email, _ := trimmedCommandOutput(doltBinary, "config", "--global", "--get", "user.email")
	if name != "" && email != "" {
		return nil
	}

	if name == "" {
		gitName, _ := trimmedCommandOutput("git", "config", "--global", "user.name")
		if gitName == "" {
			gitName = "gc-test"
		}
		if out, err := exec.Command(doltBinary, "config", "--global", "--add", "user.name", gitName).CombinedOutput(); err != nil {
			return fmt.Errorf("set dolt user.name: %w: %s", err, string(out))
		}
	}
	if email == "" {
		gitEmail, _ := trimmedCommandOutput("git", "config", "--global", "user.email")
		if gitEmail == "" {
			gitEmail = "gc-test@test.local"
		}
		if out, err := exec.Command(doltBinary, "config", "--global", "--add", "user.email", gitEmail).CombinedOutput(); err != nil {
			return fmt.Errorf("set dolt user.email: %w: %s", err, string(out))
		}
	}
	return nil
}

func trimmedCommandOutput(binary string, args ...string) (string, error) {
	out, err := exec.Command(binary, args...).CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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

func currentManagedDoltPortForTest(cityDir string) (string, bool) {
	if cityDir == "" {
		return "", false
	}
	if data, err := os.ReadFile(filepath.Join(cityDir, ".beads", "dolt-server.port")); err == nil {
		if port := strings.TrimSpace(string(data)); port != "" && port != "0" && testPortReachable(port) {
			return port, true
		}
	}
	data, err := os.ReadFile(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-state.json"))
	if err != nil {
		return "", false
	}
	var state struct {
		Running bool `json:"running"`
		Port    int  `json:"port"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return "", false
	}
	if !state.Running || state.Port <= 0 {
		return "", false
	}
	port := strconv.Itoa(state.Port)
	if !testPortReachable(port) {
		return "", false
	}
	return port, true
}

func ensureManagedDoltPortForTest(cityDir string) (string, bool) {
	if port, ok := currentManagedDoltPortForTest(cityDir); ok {
		return port, true
	}
	if cityDir == "" {
		return "", false
	}
	startOut, startErr := runGCDoltWithEnv(commandEnvForDir(cityDir, true), "", "start", cityDir)
	if startErr != nil && !isGCStartAlreadyRunning(startOut) {
		return "", false
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if port, ok := currentManagedDoltPortForTest(cityDir); ok {
			return port, true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", false
}

func managedDoltTransportRetryable(out string) bool {
	msg := strings.ToLower(out)
	for _, marker := range []string{
		"dolt server unreachable",
		"dial tcp",
		"connection refused",
		"broken pipe",
		"unexpected eof",
		"bad connection",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func testPortReachable(port string) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func requireDoltIntegration(t *testing.T) {
	t.Helper()

	if doltBinary == "" {
		t.Skip("dolt not configured; set GC_INTEGRATION_DOLT_BINARY or add dolt to PATH")
	}
	if realBDBinary == "" || bdBinary == "" {
		t.Skip("bd not configured; set GC_INTEGRATION_REAL_BD or add bd to PATH")
	}
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
				// --wait so runCommand blocks until the supervisor fully
				// shut down, aligning with the cmd.Wait() synchronization below.
				_, _ = runCommand("", env, 15*time.Second, gcBinary, "supervisor", "stop", "--wait")
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

func restartIsolatedSupervisor(t *testing.T, env []string) {
	t.Helper()

	_, _ = runCommand("", env, 15*time.Second, gcBinary, "supervisor", "stop", "--wait")

	gcHome := parseEnvList(env)["GC_HOME"]
	if gcHome == "" {
		t.Fatal("isolated env missing GC_HOME")
	}
	startIsolatedSupervisor(t, env, gcHome)
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
	oldToolBinDir, oldDoltBinary := integrationToolBinDir, doltBinary
	t.Cleanup(func() {
		testGCHome = oldGCHome
		testRuntimeDir = oldRuntimeDir
		gcBinary = oldGCBinary
		bdBinary = oldBDBinary
		realBDBinary = oldRealBDBinary
		integrationToolBinDir = oldToolBinDir
		doltBinary = oldDoltBinary
	})

	testGCHome = filepath.Join(t.TempDir(), "gc-home")
	testRuntimeDir = filepath.Join(t.TempDir(), "runtime")
	gcBinary = filepath.Join(t.TempDir(), "gc")
	bdBinary = filepath.Join(t.TempDir(), "bd")
	realBDBinary = "/usr/bin/bd"
	doltBinary = "/usr/bin/dolt"
	integrationToolBinDir = filepath.Join(t.TempDir(), "bin")

	t.Setenv("HOME", "/host/home")
	t.Setenv("GC_DOLT_HOST", "ambient-host")
	t.Setenv("GC_DOLT_PORT", "0")
	t.Setenv("GC_DOLT_USER", "ambient-user")
	t.Setenv("GC_DOLT_PASSWORD", "ambient-password")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient-beads-host")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "0")
	t.Setenv("BEADS_DOLT_SERVER_USER", "ambient-beads-user")
	t.Setenv("BEADS_DOLT_PASSWORD", "ambient-beads-password")
	env := integrationEnv()
	got := parseEnvList(env)

	if got["HOME"] != "/host/home" {
		t.Fatalf("HOME = %q, want %q", got["HOME"], "/host/home")
	}
	if got["GC_HOME"] != testGCHome {
		t.Fatalf("GC_HOME = %q, want %q", got["GC_HOME"], testGCHome)
	}
	if got["XDG_RUNTIME_DIR"] != testRuntimeDir {
		t.Fatalf("XDG_RUNTIME_DIR = %q, want %q", got["XDG_RUNTIME_DIR"], testRuntimeDir)
	}
	if got[integrationRealBDBinaryEnv] != realBDBinary {
		t.Fatalf("%s = %q, want %q", integrationRealBDBinaryEnv, got[integrationRealBDBinaryEnv], realBDBinary)
	}
	if path := got["PATH"]; !strings.HasPrefix(path, integrationToolBinDir+string(os.PathListSeparator)) && path != integrationToolBinDir {
		t.Fatalf("PATH = %q, want prefix %q", path, integrationToolBinDir)
	}
	if got["BEADS_DOLT_AUTO_START"] != "0" {
		t.Fatalf("BEADS_DOLT_AUTO_START = %q, want %q; tests must match bdRuntimeEnv and suppress bd's rogue auto-start", got["BEADS_DOLT_AUTO_START"], "0")
	}
	for _, key := range []string{
		"GC_DOLT_HOST",
		"GC_DOLT_PORT",
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER",
		"BEADS_DOLT_PASSWORD",
	} {
		if _, ok := got[key]; ok {
			t.Fatalf("%s leaked into integration env: %v", key, got[key])
		}
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

func TestCommandEnvLookupDirUsesRegisteredPathArg(t *testing.T) {
	cityDir := filepath.Join(t.TempDir(), "city")
	registerCityCommandEnv(cityDir, []string{"GC_HOME=/tmp/isolated"})
	t.Cleanup(func() { unregisterCityCommandEnv(cityDir) })

	if got := commandEnvLookupDir("", []string{"start", cityDir}); got != cityDir {
		t.Fatalf("commandEnvLookupDir with path arg = %q, want %q", got, cityDir)
	}
	if got := commandEnvLookupDir("/tmp/cwd", []string{"start", cityDir}); got != "/tmp/cwd" {
		t.Fatalf("commandEnvLookupDir with cwd = %q, want cwd", got)
	}
}

func TestRenderE2ETomlPlainAgentUsesNamedSessionWithoutSingletonCap(t *testing.T) {
	toml := renderE2EToml(e2eCity{
		Agents: []e2eAgent{{Name: "worker", StartCommand: "sleep 3600"}},
	})
	if !strings.Contains(toml, "[[named_session]]\ntemplate = \"worker\"\nmode = \"always\"") {
		t.Fatalf("rendered TOML missing named session:\n%s", toml)
	}
	if strings.Contains(toml, "max_active_sessions = 1") {
		t.Fatalf("plain E2E agent should not render singleton cap:\n%s", toml)
	}
}

func TestRewriteE2ETomlPreservingNamedSessionsRestoresInlineAgent(t *testing.T) {
	cityDir := t.TempDir()
	initial := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[named_session]]
template = "worker"
mode = "on_demand"

[[named_session]]
template = "worker"
mode = "always"

[[named_session]]
template = "worker"
name = "worker-extra"
mode = "on_demand"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(initial), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}

	rewriteE2ETomlPreservingNamedSessions(t, cityDir, e2eCity{
		Agents: []e2eAgent{{Name: "worker", StartCommand: "VERSION=v2 sleep 3600"}},
	})

	cityData, err := os.ReadFile(filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	packData, err := os.ReadFile(filepath.Join(cityDir, "pack.toml"))
	if err != nil {
		t.Fatalf("reading pack.toml: %v", err)
	}
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("loading city.toml: %v\ncity.toml:\n%s\npack.toml:\n%s", err, cityData, packData)
	}
	if cfg.Workspace.Name != "test-city" {
		t.Fatalf("Workspace.Name = %q, want test-city", cfg.Workspace.Name)
	}
	if len(cfg.Agents) != 1 || cfg.Agents[0].Name != "worker" {
		t.Fatalf("Agents = %+v, want restored worker", cfg.Agents)
	}
	if got := cfg.Agents[0].StartCommand; got != "VERSION=v2 sleep 3600" {
		t.Fatalf("StartCommand = %q, want updated command", got)
	}
	if len(cfg.NamedSessions) != 2 {
		t.Fatalf("len(NamedSessions) = %d, want 2\ncity.toml:\n%s\npack.toml:\n%s", len(cfg.NamedSessions), cityData, packData)
	}
	var workerSession config.NamedSession
	for _, ns := range cfg.NamedSessions {
		if ns.QualifiedName() == "worker" {
			workerSession = ns
			break
		}
	}
	if workerSession.Template == "" {
		t.Fatalf("worker named session not found\ncity.toml:\n%s\npack.toml:\n%s", cityData, packData)
	}
	if got := workerSession.Mode; got != "always" {
		t.Fatalf("worker named session mode = %q, want always\ncity.toml:\n%s\npack.toml:\n%s", got, cityData, packData)
	}
	if got := strings.Count(string(cityData), "[[named_session]]"); got != 1 {
		t.Fatalf("city.toml named_session blocks = %d, want 1\n%s", got, cityData)
	}
	if !strings.Contains(string(cityData), `name = "worker-extra"`) {
		t.Fatalf("city.toml should preserve non-conflicting worker-extra named session:\n%s", cityData)
	}
	if got := strings.Count(string(packData), "[[named_session]]"); got != 1 {
		t.Fatalf("pack.toml named_session blocks = %d, want 1\n%s", got, packData)
	}
}

func TestNewIsolatedToolEnvSeedsLocalDoltIdentity(t *testing.T) {
	env := newIsolatedToolEnv(t, true)
	got := parseEnvList(env)
	cfgPath := filepath.Join(got["DOLT_ROOT_PATH"], ".dolt", "config_global.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read isolated dolt config: %v", err)
	}
	if !strings.Contains(string(data), `"user.name":"gc-test"`) {
		t.Fatalf("isolated dolt config missing user.name: %s", string(data))
	}
	if !strings.Contains(string(data), `"user.email":"gc-test@test.local"`) {
		t.Fatalf("isolated dolt config missing user.email: %s", string(data))
	}
}

func TestNewIsolatedToolEnvSkipIdentityModeSkipsConfigWrite(t *testing.T) {
	t.Setenv(integrationDoltIdentityEnv, doltIdentityModeSkip)

	env := newIsolatedToolEnv(t, true)
	got := parseEnvList(env)
	cfgPath := filepath.Join(got["DOLT_ROOT_PATH"], ".dolt", "config_global.json")
	if _, err := os.Stat(cfgPath); err == nil {
		t.Fatalf("expected no isolated dolt config at %s when %s=%s", cfgPath, integrationDoltIdentityEnv, doltIdentityModeSkip)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat isolated dolt config: %v", err)
	}
}

func TestSubprocessTestKillSetIncludesRootsDescendantsAndLeaves(t *testing.T) {
	agentScript := "/tmp/test/agents/graph-dispatch.sh"
	procs := map[int]procSnapshot{
		10: {pid: 10, ppid: 1, cmd: "/tmp/gc-integration-123/gc supervisor run"},
		11: {pid: 11, ppid: 10, cmd: "child of supervisor"},
		12: {pid: 12, ppid: 11, cmd: "grandchild of supervisor"},
		20: {pid: 20, ppid: 1, cmd: "sh " + agentScript},
		21: {pid: 21, ppid: 20, cmd: "child of graph dispatch"},
		30: {pid: 30, ppid: 1, cmd: "bd ready --label=pool:polecat --unassigned --json --limit=1"},
		40: {pid: 40, ppid: 1, cmd: "ordinary unrelated process"},
	}

	got := subprocessTestKillSet(procs, agentScript)

	for _, pid := range []int{10, 11, 12, 20, 21, 30} {
		if !got[pid] {
			t.Fatalf("kill set missing pid %d: %#v", pid, got)
		}
	}
	if got[40] {
		t.Fatalf("kill set unexpectedly included unrelated pid 40: %#v", got)
	}
}

func TestRunCommandDoesNotHangOnInheritedStdoutFromBackgroundChild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "leak-stdout.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30 &\necho \"$!\" > \"$1\"\necho leaked-stdout-ok\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	start := time.Now()
	out, err := runCommand("", nil, 5*time.Second, script, pidFile)
	if err != nil {
		t.Fatalf("runCommand: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "leaked-stdout-ok" {
		t.Fatalf("output = %q, want %q", strings.TrimSpace(out), "leaked-stdout-ok")
	}
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("runCommand took %s, want it to return before timeout", elapsed)
	}

	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse child pid: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
	})
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
