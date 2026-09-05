//go:build integration

// Package integration provides end-to-end tests that exercise the real gc
// binary against real session providers (tmux or subprocess). Tests validate
// the tutorial experiences: gc init, gc start, gc stop, bead CRUD, etc.
//
// By default tests use tmux. Set GC_SESSION=subprocess to use the subprocess
// provider instead (no tmux required).
//
// Session safety: no-guard test cities use randomized 6-letter lowercase
// names (see uniqueCityName) so they spread across distinct Dolt DB
// prefixes instead of all collapsing to "gc".
// Three layers of cleanup (pre-sweep, per-test t.Cleanup, post-sweep)
// prevent orphan tmux sessions on developer boxes.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/test/dolttest"
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

var runIntegrationSupervisorStopCommand = exec.CommandContext

const (
	integrationGCCommandTimeout      = 60 * time.Second
	integrationGCLifecycleTimeout    = 120 * time.Second
	integrationGCDoltCommandTimeout  = 120 * time.Second
	integrationBDCommandTimeout      = 15 * time.Second
	integrationSupervisorStopTimeout = 10 * time.Second
	integrationSupervisorWaitDelay   = 10 * time.Second
)

const (
	integrationGCBinaryEnv     = "GC_INTEGRATION_GC_BINARY"
	integrationRealBDBinaryEnv = "GC_INTEGRATION_REAL_BD"
	integrationDoltBinaryEnv   = "GC_INTEGRATION_DOLT_BINARY"
	integrationDoltIdentityEnv = "GC_INTEGRATION_DOLT_IDENTITY_MODE"
	managedDoltTestModeEnv     = "GC_MANAGED_DOLT_TEST_MODE"
	managedDoltTestParentEnv   = "GC_MANAGED_DOLT_TEST_PARENT_PID"
	doltIdentityModeIsolated   = "isolated"
	doltIdentityModeGlobal     = "global"
	doltIdentityModeSkip       = "skip"
)

// tmuxSocketAliveSentinel pins the alive-sentinel flock on this process's
// tmux socket parent dir for the binary's lifetime; see TestMain.
var tmuxSocketAliveSentinel *os.File

// TestMain builds the gc binary and runs pre/post sweeps of orphan sessions.
func TestMain(m *testing.M) {
	if os.Getenv("GC_INTEGRATION_SUPERVISOR_STOP_HELPER") == "1" {
		select {}
	}

	subprocess := os.Getenv("GC_SESSION") == "subprocess"

	// Build gc binary to a temp directory. The pid in the dir name lets a later
	// run reap this run's dolt orphans if it dies abnormally (issue #3640).
	tmpDir, err := os.MkdirTemp("", fmt.Sprintf("gc-integration-%d-*", os.Getpid()))
	if err != nil {
		panic("integration: creating temp dir: " + err.Error())
	}
	defer os.RemoveAll(tmpDir)

	// Create the tmux socket root under /tmp rather than $TMPDIR.
	// On macOS, $TMPDIR is ~80 chars (/private/var/folders/…/T/); nesting
	// tmux sockets inside it pushes socket paths past macOS's 104-byte limit.
	// /tmp is world-writable on macOS, Linux, and CI runners.
	//
	// NewSocketParentDir sweeps orphaned siblings left by a prior SIGKILL'd
	// run before creating this run's own dir. tmuxSocketAliveSentinel must
	// stay referenced for the process lifetime: the runtime finalizes
	// unreachable os.Files, which would close the descriptor and release
	// the lock, letting a concurrent sibling's sweep reclaim this still-
	// active directory (ga-djbcqt). Normal and skip exits call os.Exit, which
	// skips defers, so those paths remove the parent explicitly below; the
	// deferred removal here additionally covers a setup panic (which unwinds
	// through defers) so it cannot leak the parent until a later aged sweep.
	tmuxSocketParent, tmuxSentinel, tmuxParentErr := tmuxtest.NewSocketParentDir("/tmp", io.Discard)
	tmuxSocketAliveSentinel = tmuxSentinel
	defer func() {
		// Re-read tmuxSocketParent so the MkdirAll-failure path that clears it
		// below is honored and this never double-removes on a normal exit.
		if tmuxSocketParent != "" {
			_ = os.RemoveAll(tmuxSocketParent)
		}
	}()
	tmuxSocketRoot := filepath.Join(tmpDir, "tmux")
	if tmuxParentErr == nil {
		tmuxSocketRoot = filepath.Join(tmuxSocketParent, "tmux")
		if err := os.MkdirAll(tmuxSocketRoot, 0o700); err != nil {
			_ = tmuxSocketAliveSentinel.Close()
			tmuxSocketAliveSentinel = nil
			os.RemoveAll(tmuxSocketParent)
			tmuxSocketParent = ""
			tmuxSocketRoot = filepath.Join(tmpDir, "tmux")
		}
	}
	if err := tmuxtest.ConfigureProcessEnv(tmuxSocketRoot); err != nil {
		panic("integration: configuring tmux test env: " + err.Error())
	}

	// Tmux check: skip all tests if tmux not available AND not using subprocess.
	if !subprocess {
		if _, err := exec.LookPath("tmux"); err != nil {
			_ = os.RemoveAll(tmpDir)
			if tmuxSocketParent != "" {
				_ = os.RemoveAll(tmuxSocketParent)
			}
			os.Exit(0)
		}
		// Pre-sweep: kill this run's root plus stale sibling orphans.
		tmuxtest.KillAllTestSessions(&mainTB{})
	}
	// Best-effort pre-sweep of stale "gc supervisor run" / control-dispatcher
	// processes left by a prior interrupted or timed-out run. This is not
	// gated to the subprocess provider: both providers boot the same shared
	// TestMain supervisor via gcBinary/testGCHome, and a `go test -timeout`
	// panic bypasses per-test t.Cleanup for either one.
	sweepSubprocessTestProcesses()
	// Reap dolt sql-server orphans left by prior crashed runs (SIGKILL /
	// timeout bypasses in-process cleanup); scoped by owner-pid liveness so
	// concurrent runs are spared (issue #3640).
	dolttest.SweepStale(filepath.Dir(tmpDir), "gc-integration-")
	stopSignalSweeper := installIntegrationSignalSweeper(subprocess)
	defer stopSignalSweeper()

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
		realBDBinary, err = buildPinnedIntegrationBDBinary(tmpDir)
		if err != nil {
			panic("integration: building pinned bd binary: " + err.Error())
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
	stopIntegrationSupervisorWithTimeout(integrationSupervisorStopTimeout)

	// Post-sweep: clean up any sessions that survived individual test cleanup.
	if !subprocess {
		tmuxtest.KillAllTestSessions(&mainTB{})
	}
	sweepSubprocessTestProcesses()

	_ = os.RemoveAll(tmpDir)
	if tmuxSocketParent != "" {
		_ = os.RemoveAll(tmuxSocketParent)
	}
	os.Exit(code)
}

func installIntegrationSignalSweeper(subprocess bool) func() {
	signals := make(chan os.Signal, 2)
	done := make(chan struct{})
	// Catches an external interrupt (Ctrl-C, `kill`, a CI job cancellation)
	// so the run's supervisor/dolt/tmux state gets swept before the process
	// exits.
	// NOTE: `go test -timeout` does not normally reach this handler — the
	// in-binary deadline fires a panic() from an internal timer goroutine and
	// the runtime calls os.Exit(2) directly, so a timed-out run's orphans are
	// caught only by the next run's pre-sweep in TestMain. The handler still
	// has to stay registered: cmd/go sends SIGQUIT as a backstop once the
	// binary blows past testTimeout + WaitDelay (issue #3640).
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		select {
		case sig := <-signals:
			sweepIntegrationProcesses(subprocess)
			signal.Stop(signals)
			if s, ok := sig.(syscall.Signal); ok {
				signal.Reset(s)
				_ = syscall.Kill(os.Getpid(), s)
			}
		case <-done:
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
	}
}

func sweepIntegrationProcesses(subprocess bool) {
	stopIntegrationSupervisorWithTimeout(integrationSupervisorStopTimeout)
	// Reap dolt orphans under this run's home too — the per-test t.Cleanup that
	// normally does this is bypassed on a signal (issue #3640).
	if testGCHome != "" {
		cleanupIntegrationDoltSQLServersUnderRoot(testGCHome)
	}
	if !subprocess {
		tmuxtest.KillAllTestSessions(&mainTB{})
	}
	sweepSubprocessTestProcesses()
}

func stopIntegrationSupervisorWithTimeout(timeout time.Duration) {
	if gcBinary == "" {
		return
	}
	if timeout <= 0 {
		timeout = integrationSupervisorStopTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stopCmd := runIntegrationSupervisorStopCommand(ctx, gcBinary, "supervisor", "stop", "--wait")
	stopCmd.Env = integrationEnv()
	out, err := stopCmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		fmt.Fprintf(os.Stderr, "integration cleanup: supervisor stop timed out after %s; continuing cleanup\n%s", timeout, string(out)) //nolint:errcheck
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration cleanup: supervisor stop failed: %v; continuing cleanup\n%s", err, string(out)) //nolint:errcheck
	}
}

func TestIntegrationSupervisorStopHelperProcess(t *testing.T) {
	if os.Getenv("GC_INTEGRATION_SUPERVISOR_STOP_HELPER") != "1" {
		return
	}
	select {}
}

func TestStopIntegrationSupervisorWithTimeoutReturnsAfterDeadline(t *testing.T) {
	oldRunner := runIntegrationSupervisorStopCommand
	oldGCBinary := gcBinary
	oldGCHome := testGCHome
	oldRuntimeDir := testRuntimeDir
	oldToolBin := integrationToolBinDir
	oldRealBD := realBDBinary
	t.Cleanup(func() {
		runIntegrationSupervisorStopCommand = oldRunner
		gcBinary = oldGCBinary
		testGCHome = oldGCHome
		testRuntimeDir = oldRuntimeDir
		integrationToolBinDir = oldToolBin
		realBDBinary = oldRealBD
	})

	t.Setenv("GC_INTEGRATION_SUPERVISOR_STOP_HELPER", "1")
	gcBinary = os.Args[0]
	testGCHome = t.TempDir()
	testRuntimeDir = t.TempDir()
	integrationToolBinDir = filepath.Dir(os.Args[0])
	realBDBinary = "bd"
	runIntegrationSupervisorStopCommand = exec.CommandContext

	start := time.Now()
	stopIntegrationSupervisorWithTimeout(10 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("stopIntegrationSupervisorWithTimeout took %s, want bounded return", elapsed)
	}
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

// buildPinnedIntegrationBDBinary builds bd from the exact Beads module that
// the integration test binary and gc both import. Resolving PATH here lets an
// older host bd open the database after gc has migrated it, producing a schema
// skew that obscures the workflow under test.
func buildPinnedIntegrationBDBinary(tmpDir string) (string, error) {
	version, err := pinnedIntegrationBeadsModuleVersion()
	if err != nil {
		return "", err
	}
	binDir := filepath.Join(tmpDir, "pinned-bd")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("create pinned bd directory: %w", err)
	}
	// CGO_ENABLED=1 + gms_pure_go is the embedded-capable bd build (per beads
	// INSTALLING.md): the pinned bd's `bd init` defaults to embedded Dolt,
	// which a CGO_ENABLED=0 binary refuses at runtime.
	cmd := exec.Command("go", "install", "-tags", "gms_pure_go", "github.com/steveyegge/beads/cmd/bd@"+version)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1", "GOBIN="+binDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go install github.com/steveyegge/beads/cmd/bd@%s: %w\n%s", version, err, out)
	}
	return filepath.Join(binDir, "bd"), nil
}

// pinnedBdStoreCommandRunner keeps direct BdStore integration tests on the
// same bd shim used by their setup commands. The default runner resolves the
// ambient process PATH before its per-command environment applies, so using it
// directly could select a host bd whose schema knowledge predates the pinned
// Beads module that created the test database.
func pinnedBdStoreCommandRunner() beads.CommandRunner {
	runner := beads.ExecCommandRunner()
	return func(dir, name string, args ...string) ([]byte, error) {
		if name == "bd" {
			name = bdBinary
		}
		return runner(dir, name, args...)
	}
}

func pinnedIntegrationBeadsModuleVersion() (string, error) {
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Version}}", "github.com/steveyegge/beads")
	cmd.Dir = findModuleRoot()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolve github.com/steveyegge/beads module version: %w\n%s", err, out)
	}
	version := strings.TrimSpace(string(out))
	if version == "" {
		return "", errors.New("github.com/steveyegge/beads module version is empty")
	}
	return version, nil
}

func TestPinnedIntegrationBeadsModuleVersion(t *testing.T) {
	version, err := pinnedIntegrationBeadsModuleVersion()
	if err != nil {
		t.Fatalf("pinnedIntegrationBeadsModuleVersion() error = %v", err)
	}
	const want = "v1.1.1-0.20260805093327-bf97b73749ac"
	if version != want {
		t.Errorf("pinnedIntegrationBeadsModuleVersion() = %q, want %q", version, want)
	}
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
	killSet := subprocessTestKillSet(procs, agentScript, integrationPIDAlive)
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
	waitForPIDsReaped(killSet)
}

func configureIntegrationSupervisorCommand(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = integrationSupervisorWaitDelay
}

func registerIntegrationDoltSQLServerCleanup(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		cleanupIntegrationDoltSQLServersUnderRoot(root)
	})
}

func cleanupIntegrationDoltSQLServersUnderRoot(root string) {
	procs := readProcessSnapshot()
	if len(procs) == 0 {
		return
	}
	terminateIntegrationPIDs(integrationDoltSQLServerKillSet(procs, root))
}

func integrationDoltSQLServerKillSet(procs map[int]procSnapshot, root string) map[int]bool {
	killSet := make(map[int]bool)
	for pid, info := range procs {
		configPath := integrationDoltSQLServerConfigPath(info.cmd)
		if configPath == "" || !pathWithinIntegrationRoot(root, configPath) {
			continue
		}
		killSet[pid] = true
	}
	return killSet
}

func integrationDoltSQLServerConfigPath(cmd string) string {
	fields := strings.Fields(cmd)
	if !integrationLooksLikeDoltSQLServer(fields) {
		return ""
	}
	for i, field := range fields {
		if field == "--config" {
			if i+1 < len(fields) {
				return fields[i+1]
			}
			return ""
		}
		if strings.HasPrefix(field, "--config=") {
			return strings.TrimPrefix(field, "--config=")
		}
	}
	return ""
}

func integrationLooksLikeDoltSQLServer(fields []string) bool {
	for i := 0; i+1 < len(fields); i++ {
		if filepath.Base(fields[i]) == "dolt" && fields[i+1] == "sql-server" {
			return true
		}
	}
	return false
}

func pathWithinIntegrationRoot(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	cleanRoot := filepath.Clean(root)
	if cleanRoot == "." || cleanRoot == string(os.PathSeparator) {
		return false
	}
	cleanPath := filepath.Clean(path)
	return cleanPath == cleanRoot || strings.HasPrefix(cleanPath, cleanRoot+string(os.PathSeparator))
}

func terminateIntegrationPIDs(killSet map[int]bool) {
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
	waitForPIDsReaped(killSet)
}

// waitForPIDsReaped blocks until every PID in killSet is gone (signal-0 errors)
// or a bounded deadline elapses. Without it, a SIGKILL returns before the
// kernel has torn the process down and released its open files: a following
// t.TempDir() RemoveAll then races a dying managed Dolt server under
// cityDir/.beads/dolt ("directory not empty"), and a following test can
// re-bind the just-freed managed Dolt port and adopt a half-dead server whose
// DB still has prior tables ("alter pre-existing dirty tables"). The deadline
// guarantees a wedged process can never hang the suite.
func waitForPIDsReaped(killSet map[int]bool) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		alive := false
		for pid := range killSet {
			if err := syscall.Kill(pid, syscall.Signal(0)); err == nil {
				alive = true
				break
			}
		}
		if !alive {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// readProcessSnapshot returns the live process table. /proc gives an exact,
// dependency-free read on Linux (CI); macOS has no /proc, so readProcessSnapshot
// falls back to shelling out to `ps` there. Without the fallback, every sweep
// built on this snapshot (sweepSubprocessTestProcesses, subprocessTestKillSet)
// silently no-ops on macOS dev boxes: orphaned "gc supervisor run" processes
// from a timed-out or killed run are never reaped, on that run or any later
// one (issue: orphan supervisor from rest-full ran 49+ minutes in a temp city).
func readProcessSnapshot() map[int]procSnapshot {
	if procs := readProcessSnapshotProc(); procs != nil {
		return procs
	}
	return readProcessSnapshotPS()
}

func readProcessSnapshotProc() map[int]procSnapshot {
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

// readProcessSnapshotPS shells out to `ps` (BSD/macOS and Linux both support
// this invocation) to build the same pid->{ppid,cmd} view /proc gives for
// free on Linux. Best-effort: a `ps` failure returns nil, same as a missing
// /proc, so callers treat "can't determine the process table" uniformly.
func readProcessSnapshotPS() map[int]procSnapshot {
	out, err := exec.Command("ps", "-axwwo", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil
	}
	procs := make(map[int]procSnapshot)
	for _, line := range strings.Split(string(out), "\n") {
		pid, ppid, cmd, ok := parsePSLine(line)
		if !ok {
			continue
		}
		procs[pid] = procSnapshot{pid: pid, ppid: ppid, cmd: cmd}
	}
	return procs
}

// parsePSLine parses one line of `ps -axwwo pid=,ppid=,command=` output.
// It scans by whitespace runs for the first two fields (pid, ppid) rather
// than splitting the whole line, so internal spaces in the command string
// (arguments, paths) survive intact.
func parsePSLine(line string) (pid, ppid int, cmd string, ok bool) {
	rest := strings.TrimLeft(line, " \t")
	pidStr, rest := nextPSField(rest)
	ppidStr, rest := nextPSField(rest)
	cmd = strings.TrimSpace(rest)
	if pidStr == "" || ppidStr == "" || cmd == "" {
		return 0, 0, "", false
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return 0, 0, "", false
	}
	ppid, err = strconv.Atoi(ppidStr)
	if err != nil {
		return 0, 0, "", false
	}
	return pid, ppid, cmd, true
}

// nextPSField splits s on the first run of whitespace, returning the field
// before it and the remainder (with leading whitespace trimmed).
func nextPSField(s string) (field, rest string) {
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimLeft(s[i:], " \t")
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

// integrationOwnerPIDFromCmd parses the owning test-run pid out of a cmdline
// that references a "gc-integration-<pid>-<rand>" run root, mirroring
// dolttest's ownerPIDFromRunDir so both sweeps scope stale state the same way.
func integrationOwnerPIDFromCmd(cmd string) (int, bool) {
	const marker = "gc-integration-"
	i := strings.Index(cmd, marker)
	if i < 0 {
		return 0, false
	}
	tok := cmd[i+len(marker):]
	end := 0
	for end < len(tok) && tok[end] >= '0' && tok[end] <= '9' {
		end++
	}
	pid, err := strconv.Atoi(tok[:end])
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// integrationPIDAlive reports whether pid still exists. Signal 0 probes
// existence without delivering a signal; EPERM means the process exists but
// is not ours to signal — treat as alive (don't reap).
func integrationPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// subprocessTestRootIsReapable reports whether a matched root belongs to a
// dead run or to this one. Roots matched by their run root in argv carry an
// owner pid ("gc-integration-<pid>-<rand>"): reap only when that owner is gone
// (a stale orphan) or is us (our own leftovers during the post-sweep), so a
// live concurrent run's supervisor — and its descendant subtree — is spared
// (issue #3640). Roots matched via agentScript carry no run root in argv, so
// they keep the prior unscoped behavior.
func subprocessTestRootIsReapable(cmd string, alive func(int) bool) bool {
	owner, ok := integrationOwnerPIDFromCmd(cmd)
	if !ok {
		return true
	}
	return !alive(owner) || owner == os.Getpid()
}

func subprocessTestKillSet(procs map[int]procSnapshot, agentScript string, alive func(int) bool) map[int]bool {
	roots := make(map[int]bool)
	children := make(map[int][]int, len(procs))
	for pid, info := range procs {
		if isSubprocessTestRoot(info.cmd, agentScript) && subprocessTestRootIsReapable(info.cmd, alive) {
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
	envDir := commandCityDirForArgs(dir, args)
	return runCommand(dir, commandEnvForDir(envDir, false), gcCommandTimeout(args), gcBinary, args...)
}

// gcDolt runs the gc binary with the given args using the isolated integration
// supervisor state, but without forcing GC_DOLT=skip. Use this for tests that
// need the real bd+dolt-backed bead store.
func gcDolt(dir string, args ...string) (string, error) {
	return gcDoltWithTimeout(dir, gcDoltCommandTimeout(args), args...)
}

func gcDoltWithTimeout(dir string, timeout time.Duration, args ...string) (string, error) {
	envDir := commandCityDirForArgs(dir, args)
	return runCommand(dir, commandEnvForDir(envDir, true), timeout, gcBinary, args...)
}

// bd runs the bd binary with the given args. If dir is non-empty, it sets
// the working directory. Returns combined stdout+stderr and any error.
func bd(dir string, args ...string) (string, error) {
	env := commandEnvForDir(dir, false)
	if usesStandaloneBDWorkspace(dir, env) {
		env = standaloneBDEnvForDir(dir)
	}
	out, err := runCommand(dir, env, integrationBDCommandTimeout, bdBinary, args...)
	if err == nil || !shouldUseFileStoreBDFallback(dir, out, args) {
		return out, err
	}
	return runFileStoreBD(dir, args...)
}

func standaloneBDEnvForDir(dir string) []string {
	base := parseEnvList(integrationEnv())
	keep := []string{
		"HOME",
		"PATH",
		"TMPDIR",
		"USER",
		"LOGNAME",
		"LANG",
		"LC_ALL",
		"TZ",
		"DOLT_ROOT_PATH",
		integrationRealBDBinaryEnv,
		integrationGCBinaryEnv,
		integrationDoltBinaryEnv,
	}
	env := make([]string, 0, len(keep)+3)
	for _, key := range keep {
		if value, ok := base[key]; ok {
			env = append(env, key+"="+value)
		}
	}
	// integrationEnv pins HOME to the real passwd-db home for gc start/supervisor
	// start subprocesses. This helper only execs the bd binary, so re-isolate HOME
	// back to the caller-owned dir instead of leaking the real home through.
	env = replaceEnv(env, "HOME", dir)
	// Keep DOLT_ROOT_PATH from integrationEnv so standalone bd commands use
	// the suite's seeded Dolt identity instead of an unseeded per-workspace root.
	// BEADS_DIR and XDG_RUNTIME_DIR are temp-scoped by caller-owned test dirs;
	// bd's embedded-mode default needs no server shutdown, and server-mode tests
	// should use their own explicit lifecycle instead of hiding it in this env.
	env = append(env, "XDG_RUNTIME_DIR="+dir)
	env = append(env, "BEADS_DIR="+filepath.Join(dir, ".beads"))
	return append(env, "BEADS_DOLT_AUTO_START=1")
}

func usesStandaloneBDWorkspace(dir string, env []string) bool {
	if parseEnvList(env)["GC_BEADS"] == "file" {
		return false
	}
	return hasStandaloneBDWorkspace(dir)
}

func hasStandaloneBDWorkspace(dir string) bool {
	if dir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, ".beads", "config.yaml")); err == nil {
		return true
	}
	return false
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
			env = appendManagedDoltEndpointEnv(env, port)
		}
	}
	out, err := runCommand(dir, env, integrationBDCommandTimeout, bdBinary, args...)
	if err == nil || dir == "" || !managedDoltTransportRetryable(out) {
		return out, err
	}
	if _, readyErr := waitForManagedDoltCityReady(env, dir, 20*time.Second); readyErr == nil {
		if port, ok := currentManagedDoltPortForTest(dir); ok {
			env = appendManagedDoltEndpointEnv(env, port)
		}
		return runCommand(dir, env, integrationBDCommandTimeout, bdBinary, args...)
	}
	if port, ok := ensureManagedDoltPortForTest(dir); ok {
		env = appendManagedDoltEndpointEnv(env, port)
		if delay := managedDoltRetryDelay(out); delay > 0 {
			time.Sleep(delay)
		}
		return runCommand(dir, env, integrationBDCommandTimeout, bdBinary, args...)
	}
	return out, err
}

func appendManagedDoltEndpointEnv(env []string, port string) []string {
	env = filterEnvMany(env, "GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT")
	return append(env,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT="+port,
		"BEADS_DOLT_SERVER_HOST=127.0.0.1",
		"BEADS_DOLT_SERVER_PORT="+port,
	)
}

func runGCWithEnv(env []string, dir string, args ...string) (string, error) {
	return runCommand(dir, env, gcCommandTimeout(args), gcBinary, args...)
}

func runGCDoltWithEnv(env []string, dir string, args ...string) (string, error) {
	return runCommand(dir, env, gcDoltCommandTimeout(args), gcBinary, args...)
}

func gcDoltCommandTimeout(args []string) time.Duration {
	if len(args) > 0 && args[0] == "sling" {
		for _, arg := range args[1:] {
			if strings.HasPrefix(arg, "--on=") {
				return 4 * time.Minute
			}
		}
	}
	return integrationGCDoltCommandTimeout
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

// buildCommand is the single construction point for every *exec.Cmd this
// file runs -- runCommand and runCommandStdout both delegate here so the
// repository's subprocess-call-site census sees one site, not two.
func buildCommand(ctx context.Context, dir string, env []string, binary string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = 2 * time.Second
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	return cmd
}

func runCommand(dir string, env []string, timeout time.Duration, binary string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := buildCommand(ctx, dir, env, binary, args...)
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

// runCommandStdout runs the command like runCommand, but captures stdout and
// stderr into separate buffers so a value-bearing caller only ever observes
// stdout -- diagnostics the subprocess writes to stderr (e.g. bd's own
// logging) must never contaminate a parsed value. On failure the stderr
// content is folded into the returned error so it remains available for
// diagnosis; only the clean value is lost from the returned string, and only
// when there is no clean value to report (the command failed).
func runCommandStdout(dir string, env []string, timeout time.Duration, binary string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := buildCommand(ctx, dir, env, binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	output := stdout.String()
	if ctx.Err() == context.DeadlineExceeded {
		return output, fmt.Errorf("timed out after %s running %s", timeout, renderCommand(binary, args...))
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return output, nil
	}
	if err != nil && stderr.Len() > 0 {
		return output, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
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
	env = filterEnv(env, "BEADS_DIR")
	env = filterEnv(env, "GC_BEADS_SCOPE_ROOT")
	env = filterEnv(env, "GC_DOLT")
	env = filterEnv(env, "PATH")
	env = filterEnv(env, "GC_HOME")
	env = filterEnv(env, "GC_DIR")
	env = filterEnv(env, "GC_CITY")
	env = filterEnv(env, "GC_CITY_PATH")
	env = filterEnv(env, "GC_CITY_ROOT")
	env = filterEnv(env, "GC_CITY_RUNTIME_DIR")
	env = filterEnv(env, "GC_AGENT")
	env = filterEnv(env, "GC_RIG")
	env = filterEnv(env, "GC_RIG_ROOT")
	env = filterEnv(env, "GC_TEMPLATE")
	env = filterEnv(env, "GC_SESSION_NAME")
	env = filterEnv(env, "XDG_RUNTIME_DIR")
	env = filterEnv(env, integrationRealBDBinaryEnv)
	env = filterEnv(env, "DOLT_ROOT_PATH")
	env = filterEnv(env, "BEADS_ACTOR")
	env = filterEnv(env, "GC_DOLT_HOST")
	env = filterEnv(env, "GC_DOLT_PORT")
	env = filterEnv(env, "GC_DOLT_USER")
	env = filterEnv(env, "GC_DOLT_PASSWORD")
	env = filterEnv(env, managedDoltTestModeEnv)
	env = filterEnv(env, managedDoltTestParentEnv)
	env = filterEnv(env, "BEADS_DOLT_SERVER_HOST")
	env = filterEnv(env, "BEADS_DOLT_SERVER_PORT")
	env = filterEnv(env, "BEADS_DOLT_SERVER_USER")
	env = filterEnv(env, "BEADS_DOLT_HOST")
	env = filterEnv(env, "BEADS_DOLT_PORT")
	env = filterEnv(env, "BEADS_DOLT_USER")
	env = filterEnv(env, "BEADS_DOLT_DATABASE")
	env = filterEnv(env, "BEADS_DOLT_DATA_DIR")
	env = filterEnv(env, "BEADS_DOLT_PASSWORD")
	env = filterEnv(env, "GC_SUPERVISOR_ENV")
	env = filterEnv(env, "GC_SUPERVISOR_PRESERVE_SESSIONS_ON_SIGNAL")
	env = filterEnv(env, "GC_SUPERVISOR_LOG_TEE")
	env = filterEnv(env, "DOLT_HOST")
	env = filterEnv(env, "DOLT_PORT")
	env = filterEnv(env, "DOLT_USER")
	env = filterEnv(env, "DOLT_PASSWORD")
	env = filterEnv(env, integrationGCBinaryEnv)
	env = filterEnv(env, integrationDoltBinaryEnv)
	env = filterEnv(env, "BEADS_DOLT_AUTO_START")
	if !useDolt {
		env = append(env, "GC_DOLT=skip")
	}
	env = append(env, "GC_HOME="+gcHome)
	env = append(env, "XDG_RUNTIME_DIR="+runtimeDir)
	env = append(env, managedDoltTestModeEnv+"=1")
	env = append(env, managedDoltTestParentEnv+"="+strconv.Itoa(os.Getpid()))
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
	env = pinRealHomeEnv(env)
	return env
}

// pinRealHomeEnv pins HOME to the real passwd-db home for the current uid.
// Test runners (sandboxes, CI containers) commonly run with HOME pointed at
// something other than the invoking user's real home; left unchanged, that
// ambient HOME propagates into the gc subprocess these tests exec and trips
// platformSupervisorHomeOverrideError (cmd/gc/cmd_supervisor_lifecycle.go),
// which blocks non-delegated `gc start`/`gc supervisor start` when HOME
// differs from the real home. GC_HOME (set separately, above) remains the
// isolated per-test root; only the OS-level HOME is pinned. Mirrors
// cmd/gc/cmd_supervisor_test.go's pinRealHome, reimplemented here because
// that helper is test-only in a different package. Fails open (leaves env
// untouched) if the lookup errors or returns an empty home dir, matching
// platformSupervisorHomeOverrideError's own tolerance.
func pinRealHomeEnv(env []string) []string {
	lu, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || strings.TrimSpace(lu.HomeDir) == "" {
		return env
	}
	return replaceEnv(env, "HOME", lu.HomeDir)
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
	registerIntegrationDoltSQLServerCleanup(t, root)
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

func commandCityDirForArgs(dir string, args []string) string {
	if dir != "" || len(args) < 2 {
		return dir
	}
	switch args[0] {
	case "start", "stop", "restart", "suspend", "resume":
		if filepath.IsAbs(args[1]) {
			return args[1]
		}
	}
	return dir
}

func commandEnvLookupDir(dir string, args []string) string {
	return commandCityDirForArgs(dir, args)
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
		"dolt circuit breaker is open",
		"server appears down, failing fast",
		"dolt server unreachable",
		"dial tcp",
		"connection refused",
		"broken pipe",
		"unexpected eof",
		"bad connection",
		"dolt circuit breaker is open",
		"server appears down",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func managedDoltRetryDelay(out string) time.Duration {
	msg := strings.ToLower(out)
	if strings.Contains(msg, "dolt circuit breaker is open") || strings.Contains(msg, "server appears down, failing fast") {
		return 5 * time.Second
	}
	return 0
}

func TestManagedDoltTransportRetryableIncludesCircuitBreaker(t *testing.T) {
	out := `{"error":"failed to open database: dolt circuit breaker is open: server appears down, failing fast (cooldown 5s)"}`
	if !managedDoltTransportRetryable(out) {
		t.Fatalf("managedDoltTransportRetryable(%q) = false, want true", out)
	}
}

// doltDirtyTableMigrationRaceRetryable reports whether out is the known
// transient beads#4566 signature: a bd Dolt schema-migration bootstrap
// racing a still-settling prior schema state under concurrent test load
// (ga-38xsx4). Narrowly scoped to that one signature only — any other gc
// init / bd init failure, including the stdout-contract regression this
// suite exists to catch (ga-rsktma), must never be retried away here.
func doltDirtyTableMigrationRaceRetryable(out string) bool {
	return strings.Contains(strings.ToLower(out), "pending schema migrations alter pre-existing dirty tables")
}

func TestDoltDirtyTableMigrationRaceRetryableMatchesKnownSignatureOnly(t *testing.T) {
	known := "Error: failed to open Dolt store: failed to initialize schema: schema migration: pending schema migrations alter pre-existing dirty tables: dependencies; run 'bd dolt commit' to commit the working set at the current schema, then re-run the migration (gastownhall/beads#4566)"
	if !doltDirtyTableMigrationRaceRetryable(known) {
		t.Fatalf("doltDirtyTableMigrationRaceRetryable(%q) = false, want true", known)
	}
	for _, table := range []string{"issues", "events", "dolt_schemas"} {
		variant := strings.Replace(known, "dependencies", table, 1)
		if !doltDirtyTableMigrationRaceRetryable(variant) {
			t.Fatalf("doltDirtyTableMigrationRaceRetryable(%q) = false, want true", variant)
		}
	}
	other := "Error: failed to open Dolt store: dial tcp 127.0.0.1:3306: connect: connection refused"
	if doltDirtyTableMigrationRaceRetryable(other) {
		t.Fatalf("doltDirtyTableMigrationRaceRetryable(%q) = true, want false (must not swallow unrelated errors)", other)
	}
	stdoutRegression := "unexpected extra stdout: circuit-breaker cleanup log leaked onto stdout"
	if doltDirtyTableMigrationRaceRetryable(stdoutRegression) {
		t.Fatalf("doltDirtyTableMigrationRaceRetryable(%q) = true, want false (must not mask the stdout-contract regression this test exists to catch)", stdoutRegression)
	}
}

// retryOnDoltDirtyTableMigrationRace runs cmd, retrying up to a small bound
// ONLY when the result matches the known-transient beads#4566 dirty-table
// migration race (ga-38xsx4). Any other outcome — success or a different
// failure — returns immediately on the first attempt, so a real regression
// (e.g. ga-rsktma's stdout contract) still fails the test instead of being
// retried away. Bounded, not a blind retry-until-green.
//
// The inter-attempt wait is delegated to backoff.Retry rather than a local
// time.Sleep: the delay then lives inside the already-imported backoff
// library's own implementation instead of adding another fixed-sleep call
// site to this file's static resource census (internal/testpolicy/resourcecensus).
func retryOnDoltDirtyTableMigrationRace(cmd func() (string, error)) (string, error) {
	const maxAttempts = 3
	const retryDelay = 2 * time.Second

	var out string
	var lastErr error

	bo := backoff.WithMaxRetries(backoff.NewConstantBackOff(retryDelay), maxAttempts-1)
	_ = backoff.Retry(func() error {
		out, lastErr = cmd()
		if lastErr == nil {
			return nil
		}
		if !doltDirtyTableMigrationRaceRetryable(out) {
			return backoff.Permanent(lastErr)
		}
		return lastErr
	}, bo)

	return out, lastErr
}

func TestRetryOnDoltDirtyTableMigrationRaceRetriesOnlyKnownSignature(t *testing.T) {
	const raceOutput = "schema migration: pending schema migrations alter pre-existing dirty tables: issues (gastownhall/beads#4566)"

	t.Run("retries until success", func(t *testing.T) {
		calls := 0
		out, err := retryOnDoltDirtyTableMigrationRace(func() (string, error) {
			calls++
			if calls < 3 {
				return raceOutput, errors.New("exit status 1")
			}
			return "ok", nil
		})
		if err != nil {
			t.Fatalf("err = %v, want nil after eventual success", err)
		}
		if out != "ok" {
			t.Fatalf("out = %q, want %q", out, "ok")
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want 3", calls)
		}
	})

	t.Run("does not retry unrelated errors", func(t *testing.T) {
		calls := 0
		wantErr := errors.New("boom")
		_, err := retryOnDoltDirtyTableMigrationRace(func() (string, error) {
			calls++
			return "unrelated failure", wantErr
		})
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1 (must not retry a non-4566 failure)", calls)
		}
	})

	t.Run("gives up after bounded attempts", func(t *testing.T) {
		calls := 0
		_, err := retryOnDoltDirtyTableMigrationRace(func() (string, error) {
			calls++
			return raceOutput, errors.New("exit status 1")
		})
		if err == nil {
			t.Fatalf("err = nil, want non-nil after exhausting retries on a persistent race")
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want 3 (bounded, not unbounded retry-until-green)", calls)
		}
	})
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

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, gcBinary, "supervisor", "run")
	configureIntegrationSupervisorCommand(cmd)
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
				cancel()
				waitForIntegrationSupervisorDone(cmd, done, integrationSupervisorWaitDelay)
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

	cancel()
	waitForIntegrationSupervisorDone(cmd, done, integrationSupervisorWaitDelay)
	_ = logFile.Close()
	logData, _ := os.ReadFile(logPath)
	t.Fatalf("isolated supervisor did not become ready:\n%s", string(logData))
}

func waitForIntegrationSupervisorDone(cmd *exec.Cmd, done <-chan error, timeout time.Duration) {
	select {
	case <-done:
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
	}
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

func TestIntegrationEnvForPinsRealHome(t *testing.T) {
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
	t.Setenv("BEADS_DIR", "/host/beads")
	t.Setenv("GC_DOLT_HOST", "ambient-host")
	t.Setenv("GC_DOLT_PORT", "0")
	t.Setenv("GC_DOLT_USER", "ambient-user")
	t.Setenv("GC_DOLT_PASSWORD", "ambient-password")
	t.Setenv("BEADS_DIR", "/host/beads")
	t.Setenv("BEADS_ACTOR", "ambient-actor")
	t.Setenv("BEADS_DIR", "/host/repo/.beads")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient-beads-host")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "0")
	t.Setenv("BEADS_DOLT_SERVER_USER", "ambient-beads-user")
	t.Setenv("BEADS_DOLT_HOST", "ambient-legacy-host")
	t.Setenv("BEADS_DOLT_PORT", "0")
	t.Setenv("BEADS_DOLT_USER", "ambient-legacy-user")
	t.Setenv("BEADS_DOLT_DATABASE", "ambient-legacy-db")
	t.Setenv("BEADS_DOLT_DATA_DIR", filepath.Join(t.TempDir(), "ambient-dolt-data"))
	t.Setenv("BEADS_DOLT_PASSWORD", "ambient-beads-password")
	t.Setenv("DOLT_HOST", "ambient-raw-host")
	t.Setenv("DOLT_PORT", "0")
	t.Setenv("DOLT_USER", "ambient-raw-user")
	t.Setenv("DOLT_PASSWORD", "ambient-raw-password")
	t.Setenv("BEADS_DIR", "/host/beads")
	t.Setenv("BEADS_ACTOR", "host-agent")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "/host/scope")
	t.Setenv("GC_DIR", "/host/gc-dir")
	t.Setenv("GC_CITY", "/host/city")
	t.Setenv("GC_CITY_PATH", "/host/city-path")
	t.Setenv("GC_CITY_ROOT", "/host/city-root")
	t.Setenv("GC_CITY_RUNTIME_DIR", "/host/runtime")
	t.Setenv("GC_AGENT", "host-agent")
	t.Setenv("GC_RIG", "host-rig")
	t.Setenv("GC_RIG_ROOT", "/host/rig")
	t.Setenv("GC_TEMPLATE", "host/template")
	t.Setenv("GC_SESSION_NAME", "host-session")
	env := integrationEnv()
	got := parseEnvList(env)

	lu, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		t.Fatalf("looking up real home for assertion: %v", err)
	}
	if got["HOME"] != lu.HomeDir {
		t.Fatalf("HOME = %q, want real passwd-db home %q (ambient HOME=/host/home must not leak through)", got["HOME"], lu.HomeDir)
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
		"BEADS_DIR",
		"GC_BEADS_SCOPE_ROOT",
		"GC_DOLT_HOST",
		"GC_DOLT_PORT",
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
		"BEADS_ACTOR",
		"BEADS_DIR",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER",
		"BEADS_DOLT_HOST",
		"BEADS_DOLT_PORT",
		"BEADS_DOLT_USER",
		"BEADS_DOLT_DATABASE",
		"BEADS_DOLT_DATA_DIR",
		"BEADS_DOLT_PASSWORD",
		"DOLT_HOST",
		"DOLT_PORT",
		"DOLT_USER",
		"DOLT_PASSWORD",
		"BEADS_DIR",
		"BEADS_ACTOR",
		"GC_BEADS_SCOPE_ROOT",
		"GC_DIR",
		"GC_CITY",
		"GC_CITY_PATH",
		"GC_CITY_ROOT",
		"GC_CITY_RUNTIME_DIR",
		"GC_AGENT",
		"GC_RIG",
		"GC_RIG_ROOT",
		"GC_TEMPLATE",
		"GC_SESSION_NAME",
	} {
		if _, ok := got[key]; ok {
			t.Fatalf("%s leaked into integration env: %v", key, got[key])
		}
	}
}

func TestManagedDoltTransportRetryableRecognizesCircuitBreaker(t *testing.T) {
	output := `{"error":"failed to open database: dolt circuit breaker is open: server appears down, failing fast (cooldown 5s)"}`
	if !managedDoltTransportRetryable(output) {
		t.Fatalf("managedDoltTransportRetryable(%q) = false, want true", output)
	}
	if got := managedDoltRetryDelay(output); got < 5*time.Second {
		t.Fatalf("managedDoltRetryDelay(%q) = %s, want at least 5s", output, got)
	}
	if got := managedDoltRetryDelay("dial tcp 127.0.0.1:3306: connect: connection refused"); got != 0 {
		t.Fatalf("managedDoltRetryDelay for plain transport error = %s, want 0", got)
	}
}

func TestStandaloneBDEnvAllowsBDAutoStart(t *testing.T) {
	oldGCHome := testGCHome
	oldRuntimeDir := testRuntimeDir
	oldRealBDBinary := realBDBinary
	oldToolBinDir := integrationToolBinDir
	t.Cleanup(func() {
		testGCHome = oldGCHome
		testRuntimeDir = oldRuntimeDir
		realBDBinary = oldRealBDBinary
		integrationToolBinDir = oldToolBinDir
	})

	testGCHome = filepath.Join(t.TempDir(), "gc-home")
	testRuntimeDir = filepath.Join(t.TempDir(), "runtime")
	realBDBinary = "/usr/bin/bd"
	integrationToolBinDir = filepath.Join(t.TempDir(), "bin")

	t.Setenv("BEADS_DOLT_AUTO_START", "0")
	t.Setenv("BEADS_DIR", "/host/beads")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "ambient-host")
	t.Setenv("GC_DOLT_PORT", "1234")
	t.Setenv("GC_DOLT_USER", "ambient-user")
	t.Setenv("GC_DOLT_PASSWORD", "ambient-password")
	t.Setenv("GC_DOLT_STATE_FILE", "/host/dolt-state.json")
	t.Setenv("GC_DOLT_CONFIG_FILE", "/host/dolt-config.yaml")
	t.Setenv("GC_DOLT_DATA_DIR", "/host/dolt-data")
	t.Setenv("GC_DOLT_LOG_FILE", "/host/dolt.log")
	t.Setenv("GC_DOLT_PID_FILE", "/host/dolt.pid")
	t.Setenv("GC_DOLT_LOCK_FILE", "/host/dolt.lock")
	t.Setenv("GC_DOLT_MANAGED_LOCAL", "1")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient-beads-host")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "5678")
	t.Setenv("BEADS_DOLT_SERVER_USER", "ambient-beads-user")
	t.Setenv("BEADS_DOLT_PASSWORD", "ambient-beads-password")
	t.Setenv("BEADS_DOLT_HOST", "ambient-legacy-host")
	t.Setenv("BEADS_DOLT_PORT", "9012")
	t.Setenv("BEADS_DOLT_USER", "ambient-legacy-user")
	t.Setenv("BEADS_DOLT_DATABASE", "ambient-legacy-db")
	t.Setenv("BEADS_DOLT_DATA_DIR", filepath.Join(t.TempDir(), "ambient-dolt-data"))
	t.Setenv("GC_CITY", "/host/city")
	t.Setenv("GC_CITY_PATH", "/host/city")
	t.Setenv("GC_CITY_RUNTIME_DIR", "/host/runtime")

	dir := t.TempDir()
	env := standaloneBDEnvForDir(dir)
	got := parseEnvList(env)

	if got["BEADS_DOLT_AUTO_START"] != "1" {
		t.Fatalf("BEADS_DOLT_AUTO_START = %q, want 1", got["BEADS_DOLT_AUTO_START"])
	}
	if got["BEADS_DIR"] != filepath.Join(dir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got["BEADS_DIR"], filepath.Join(dir, ".beads"))
	}
	if got["DOLT_ROOT_PATH"] != testGCHome {
		t.Fatalf("DOLT_ROOT_PATH = %q, want seeded integration root %q", got["DOLT_ROOT_PATH"], testGCHome)
	}
	if got["XDG_RUNTIME_DIR"] != dir {
		t.Fatalf("XDG_RUNTIME_DIR = %q, want %q", got["XDG_RUNTIME_DIR"], dir)
	}
	for _, key := range []string{
		"GC_DOLT",
		"GC_DOLT_HOST",
		"GC_DOLT_PORT",
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
		"GC_DOLT_STATE_FILE",
		"GC_DOLT_CONFIG_FILE",
		"GC_DOLT_DATA_DIR",
		"GC_DOLT_LOG_FILE",
		"GC_DOLT_PID_FILE",
		"GC_DOLT_LOCK_FILE",
		"GC_DOLT_MANAGED_LOCAL",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER",
		"BEADS_DOLT_PASSWORD",
		"BEADS_DOLT_HOST",
		"BEADS_DOLT_PORT",
		"BEADS_DOLT_USER",
		"BEADS_DOLT_DATABASE",
		"BEADS_DOLT_DATA_DIR",
		"GC_CITY",
		"GC_CITY_PATH",
		"GC_CITY_RUNTIME_DIR",
	} {
		if _, ok := got[key]; ok {
			t.Fatalf("%s leaked into standalone bd env: %v", key, got[key])
		}
	}
}

func TestStandaloneBDEnvForDirIsolatesHome(t *testing.T) {
	oldGCHome := testGCHome
	oldRuntimeDir := testRuntimeDir
	oldRealBDBinary := realBDBinary
	oldToolBinDir := integrationToolBinDir
	t.Cleanup(func() {
		testGCHome = oldGCHome
		testRuntimeDir = oldRuntimeDir
		realBDBinary = oldRealBDBinary
		integrationToolBinDir = oldToolBinDir
	})

	testGCHome = filepath.Join(t.TempDir(), "gc-home")
	testRuntimeDir = filepath.Join(t.TempDir(), "runtime")
	realBDBinary = "/usr/bin/bd"
	integrationToolBinDir = filepath.Join(t.TempDir(), "bin")

	t.Setenv("HOME", "/host/home")

	dir := t.TempDir()
	env := standaloneBDEnvForDir(dir)
	got := parseEnvList(env)

	lu, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		t.Fatalf("looking up real home for assertion: %v", err)
	}
	if got["HOME"] == lu.HomeDir {
		t.Fatalf("HOME = %q, leaked the real passwd-db home; standalone bd only execs the bd binary (never gc start/supervisor start), so it must not inherit the real-HOME pin meant for gc-start consumers", got["HOME"])
	}
	if got["HOME"] != dir {
		t.Fatalf("HOME = %q, want dir-scoped %q, matching this helper's own XDG_RUNTIME_DIR/BEADS_DIR isolation root", got["HOME"], dir)
	}
}

func TestUsesStandaloneBDWorkspaceKeepsFileProviderOnShim(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	if usesStandaloneBDWorkspace(dir, []string{"GC_BEADS=file"}) {
		t.Fatal("file provider city should keep using the file-store bd shim")
	}
	if usesStandaloneBDWorkspace(dir, []string{"GC_BEADS=dolt"}) {
		t.Fatal("bare .beads directory should not select the standalone bd env")
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte("issue_prefix: test\n"), 0o644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	if usesStandaloneBDWorkspace(dir, []string{"GC_BEADS=file"}) {
		t.Fatal("file provider city with config.yaml should keep using the file-store bd shim")
	}
	if !usesStandaloneBDWorkspace(dir, []string{"GC_BEADS=dolt"}) {
		t.Fatal("standalone .beads workspace with config.yaml should use the standalone bd env")
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

func TestStandaloneBdEnvIsolatesAmbientDoltConfig(t *testing.T) {
	t.Setenv("HOME", "/host/home")
	t.Setenv("GC_CITY", "/host/city")
	t.Setenv("GC_CITY_PATH", "/host/city")
	t.Setenv("GC_RIG", "host-rig")
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "/host/repo")
	t.Setenv("GC_DOLT", "server")
	t.Setenv("GC_DOLT_HOST", "127.0.0.1")
	t.Setenv("GC_DOLT_PORT", "0")
	t.Setenv("GC_DOLT_USER", "ambient-user")
	t.Setenv("GC_DOLT_PASSWORD", "ambient-password")
	t.Setenv("BEADS_DIR", "/host/beads")
	t.Setenv("BEADS_DOLT_AUTO_START", "0")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "127.0.0.1")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "0")
	t.Setenv("BEADS_DOLT_SERVER_USER", "ambient-user")
	t.Setenv("BEADS_DOLT_PASSWORD", "ambient-password")

	dir := filepath.Join(t.TempDir(), "standalone")
	got := parseEnvList(standaloneBdEnv(t, dir))

	if got["HOME"] == "/host/home" || got["HOME"] == "" {
		t.Fatalf("HOME = %q, want isolated non-empty home", got["HOME"])
	}
	if got["HOME"] != got["GC_HOME"] {
		t.Fatalf("HOME = %q, want GC_HOME %q", got["HOME"], got["GC_HOME"])
	}
	if got["BEADS_DIR"] != filepath.Join(dir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want standalone beads dir", got["BEADS_DIR"])
	}
	if got["BD_NON_INTERACTIVE"] != "1" {
		t.Fatalf("BD_NON_INTERACTIVE = %q, want 1", got["BD_NON_INTERACTIVE"])
	}
	for _, key := range []string{
		"GC_CITY",
		"GC_CITY_PATH",
		"GC_RIG",
		"GC_BEADS",
		"GC_BEADS_SCOPE_ROOT",
		"GC_DOLT",
		"GC_DOLT_HOST",
		"GC_DOLT_PORT",
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
		"BEADS_DOLT_AUTO_START",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER",
		"BEADS_DOLT_PASSWORD",
	} {
		if _, ok := got[key]; ok {
			t.Fatalf("%s leaked into standalone bd env: %v", key, got)
		}
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
	var explicitAgents []config.Agent
	for _, agent := range cfg.Agents {
		if !agent.Implicit {
			explicitAgents = append(explicitAgents, agent)
		}
	}
	if len(explicitAgents) != 1 || explicitAgents[0].Name != "worker" {
		t.Fatalf("explicit agents = %+v, want restored worker; all agents = %+v", explicitAgents, cfg.Agents)
	}
	if got := explicitAgents[0].StartCommand; got != "VERSION=v2 sleep 3600" {
		t.Fatalf("StartCommand = %q, want updated command", got)
	}
	var userNamedSessions []config.NamedSession
	for _, ns := range cfg.NamedSessions {
		if ns.Template != config.ControlDispatcherAgentName {
			userNamedSessions = append(userNamedSessions, ns)
		}
	}
	if len(userNamedSessions) != 2 {
		t.Fatalf("len(userNamedSessions) = %d, want 2; all named sessions = %+v\ncity.toml:\n%s\npack.toml:\n%s", len(userNamedSessions), cfg.NamedSessions, cityData, packData)
	}
	var workerSession config.NamedSession
	for _, ns := range userNamedSessions {
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

	// Owner pid 123 is reported dead so the stale root is reapable; injecting
	// the predicate keeps the fixture deterministic instead of depending on
	// whether pid 123 happens to exist on the host.
	got := subprocessTestKillSet(procs, agentScript, func(int) bool { return false })

	for _, pid := range []int{10, 11, 12, 20, 21, 30} {
		if !got[pid] {
			t.Fatalf("kill set missing pid %d: %#v", pid, got)
		}
	}
	if got[40] {
		t.Fatalf("kill set unexpectedly included unrelated pid 40: %#v", got)
	}
}

// TestSubprocessTestKillSetSparesLiveForeignIntegrationRun pins the ownership
// scoping that makes the ungated sweep safe: the sweep now runs for both
// providers, so a starting run's pre-sweep must not SIGTERM/SIGKILL the
// supervisor of a live concurrent run. A root is reapable only when its owner
// pid is dead (a stale orphan) or is this process (our own leftovers).
func TestSubprocessTestKillSetSparesLiveForeignIntegrationRun(t *testing.T) {
	agentScript := "/tmp/test/agents/graph-dispatch.sh"
	self := os.Getpid()
	procs := map[int]procSnapshot{
		10: {pid: 10, ppid: 1, cmd: "/tmp/gc-integration-123-abc/bin/gc supervisor run"},
		11: {pid: 11, ppid: 10, cmd: "child of stale supervisor"},
		20: {pid: 20, ppid: 1, cmd: fmt.Sprintf("/tmp/gc-integration-%d-xyz/bin/gc supervisor run", self)},
		30: {pid: 30, ppid: 1, cmd: "/tmp/gc-integration-999-def/bin/gc supervisor run"},
		31: {pid: 31, ppid: 30, cmd: "child of live foreign supervisor"},
	}
	alive := func(pid int) bool { return pid == 999 || pid == self }

	got := subprocessTestKillSet(procs, agentScript, alive)

	for _, pid := range []int{10, 11, 20} {
		if !got[pid] {
			t.Fatalf("kill set missing pid %d (stale orphan or own run): %#v", pid, got)
		}
	}
	for _, pid := range []int{30, 31} {
		if got[pid] {
			t.Fatalf("kill set included pid %d from a live concurrent run: %#v", pid, got)
		}
	}
}

// TestParsePSLineSurvivesInternalWhitespaceAndRejectsMalformedInput is the
// falsifiable-floor check for the macOS ps(1) fallback: it must parse real
// `ps -axwwo pid=,ppid=,command=` rows (including a multi-arg orphaned
// supervisor command, the exact shape reported for issue's orphan pid) and
// must reject rows that don't have the pid/ppid/command shape, so a future
// ps(1) output-format change fails loudly instead of silently returning an
// empty, "looks clean" process table.
func TestParsePSLineSurvivesInternalWhitespaceAndRejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		wantOK   bool
		wantPID  int
		wantPPID int
		wantCmd  string
	}{
		{
			name:     "orphaned supervisor with args and spaces",
			line:     "62765     1 /var/folders/2t/xxx/T/gc-integration-49858-3331992158/bin/gc supervisor run",
			wantOK:   true,
			wantPID:  62765,
			wantPPID: 1,
			wantCmd:  "/var/folders/2t/xxx/T/gc-integration-49858-3331992158/bin/gc supervisor run",
		},
		{
			name:     "leading whitespace from column padding",
			line:     "   104     1 /usr/libexec/logd",
			wantOK:   true,
			wantPID:  104,
			wantPPID: 1,
			wantCmd:  "/usr/libexec/logd",
		},
		{name: "empty line", line: "", wantOK: false},
		{name: "header-only garbage", line: "PID PPID COMMAND", wantOK: false},
		{name: "missing command", line: "10 1", wantOK: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pid, ppid, cmd, ok := parsePSLine(c.line)
			if ok != c.wantOK {
				t.Fatalf("parsePSLine(%q) ok = %v, want %v", c.line, ok, c.wantOK)
			}
			if !c.wantOK {
				return
			}
			if pid != c.wantPID || ppid != c.wantPPID || cmd != c.wantCmd {
				t.Fatalf("parsePSLine(%q) = (%d, %d, %q), want (%d, %d, %q)",
					c.line, pid, ppid, cmd, c.wantPID, c.wantPPID, c.wantCmd)
			}
		})
	}
}

// TestReadProcessSnapshotPSFindsRealProcesses exercises the ps(1) query
// itself on every host, including Linux CI, so a future flag or output-format
// change fails here instead of leaving the macOS sweep silently blind — the
// exact failure mode this fallback exists to fix. `ps -axwwo
// pid=,ppid=,command=` is accepted by both BSD ps and procps-ng.
func TestReadProcessSnapshotPSFindsRealProcesses(t *testing.T) {
	procs := readProcessSnapshotPS()
	if len(procs) == 0 {
		t.Fatal("readProcessSnapshotPS() returned no processes; known-positive control failed")
	}
	self := os.Getpid()
	if _, ok := procs[self]; !ok {
		t.Fatalf("readProcessSnapshotPS() did not include this process's own pid %d among %d entries", self, len(procs))
	}
}

// TestReadProcessSnapshotFallsBackToPSAndFindsRealProcesses is the
// known-positive control for the fallback path added by this change: on a
// host with no /proc (every macOS dev box, including CI running locally
// here), readProcessSnapshotProc must return nil, and readProcessSnapshot's
// ps(1) fallback must come back non-empty and contain this test binary's own
// pid — proving the query is not a silently-blind zero.
func TestReadProcessSnapshotFallsBackToPSAndFindsRealProcesses(t *testing.T) {
	if _, err := os.Stat("/proc"); err == nil {
		t.Skip("host has /proc; this test targets the no-/proc (macOS) fallback path")
	}
	if procs := readProcessSnapshotProc(); procs != nil {
		t.Fatalf("readProcessSnapshotProc() = %v entries on a host with no /proc, want nil", len(procs))
	}
	procs := readProcessSnapshot()
	if len(procs) == 0 {
		t.Fatal("readProcessSnapshot() returned no processes via the ps(1) fallback; known-positive control failed")
	}
	self := os.Getpid()
	if _, ok := procs[self]; !ok {
		t.Fatalf("readProcessSnapshot() via ps(1) fallback did not include this process's own pid %d among %d entries", self, len(procs))
	}
}

func TestConfigureIntegrationSupervisorCommandUsesGracefulCancel(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "gc", "supervisor", "run")
	configureIntegrationSupervisorCommand(cmd)

	if cmd.Cancel == nil {
		t.Fatal("supervisor command Cancel is nil, want SIGTERM cancel")
	}
	if cmd.WaitDelay != 10*time.Second {
		t.Fatalf("supervisor command WaitDelay = %s, want 10s", cmd.WaitDelay)
	}
}

func TestIntegrationDoltSQLServerKillSetMatchesOnlyRootedConfigs(t *testing.T) {
	root := "/tmp/gcit-123"
	procs := map[int]procSnapshot{
		10: {pid: 10, ppid: 1, cmd: "dolt sql-server --config /tmp/gcit-123/cities/x/.gc/runtime/packs/dolt/dolt-config.yaml"},
		11: {pid: 11, ppid: 1, cmd: "dolt sql-server --config=/tmp/gcit-123/cities/y/.gc/runtime/packs/dolt/dolt-config.yaml"},
		12: {pid: 12, ppid: 1, cmd: "dolt sql-server --config /tmp/gcit-1234/cities/z/.gc/runtime/packs/dolt/dolt-config.yaml"},
		13: {pid: 13, ppid: 1, cmd: "dolt sql-server --config /home/u/projects/foo/.gc/runtime/packs/dolt/dolt-config.yaml"},
		14: {pid: 14, ppid: 1, cmd: "dolt sql --config /tmp/gcit-123/cities/x/.gc/runtime/packs/dolt/dolt-config.yaml"},
	}

	got := integrationDoltSQLServerKillSet(procs, root)
	for _, pid := range []int{10, 11} {
		if !got[pid] {
			t.Fatalf("kill set missing pid %d: %#v", pid, got)
		}
	}
	for _, pid := range []int{12, 13, 14} {
		if got[pid] {
			t.Fatalf("kill set unexpectedly included pid %d: %#v", pid, got)
		}
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

// TestRunCommandStdoutExcludesStderr guards against ga-rsktma: a value-bearing
// caller (e.g. bdDoltInRig's `bd config get`) must never observe stderr
// diagnostics mixed into the value it parses. Regression trigger: any
// legitimate stderr output from the subprocess (e.g. bd's own diagnostic
// logging) corrupted assertions that only expected the clean value on stdout.
func TestRunCommandStdoutExcludesStderr(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "split-streams.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'clean-value'\nprintf 'diagnostic-noise' 1>&2\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	out, err := runCommandStdout("", nil, 5*time.Second, script)
	if err != nil {
		t.Fatalf("runCommandStdout: %v\n%s", err, out)
	}
	if out != "clean-value" {
		t.Fatalf("output = %q, want %q (stderr must not be mixed into a value-bearing capture)", out, "clean-value")
	}
}

// TestRunCommandStdoutIncludesStderrInErrorOnFailure verifies that excluding
// stderr from the returned value does not lose it for diagnosis: on failure
// the stderr content must still surface, via the returned error, so callers'
// %v-based failure messages remain informative.
func TestRunCommandStdoutIncludesStderrInErrorOnFailure(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fail-with-diagnostic.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'partial-value'\nprintf 'boom-diagnostic' 1>&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	out, err := runCommandStdout("", nil, 5*time.Second, script)
	if err == nil {
		t.Fatalf("runCommandStdout: want error for non-zero exit, got nil (output %q)", out)
	}
	if out != "partial-value" {
		t.Fatalf("output = %q, want %q", out, "partial-value")
	}
	if !strings.Contains(err.Error(), "boom-diagnostic") {
		t.Fatalf("err = %q, want it to contain stderr diagnostic %q", err.Error(), "boom-diagnostic")
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
