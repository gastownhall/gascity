package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/pidutil"
)

type managedDoltStartReport struct {
	Ready        bool
	PID          int
	Port         int
	AddressInUse bool
	Attempts     int
}

type managedDoltStartedProcess struct {
	CityPath    string
	PID         int
	WatchdogPID int
	DisarmFile  string
	DisarmReady bool
	// StartTimeTicks is /proc/<pid>/stat field 22 captured at registration;
	// the test reaper re-reads it before signaling so PID reuse cannot cause
	// us to terminate an unrelated process that landed on the same PID after
	// dolt exited. Zero on hosts without /proc; in that case the identity
	// guard falls back to StartIdentity. Mirrors the production reap
	// algorithm in cmd_dolt_cleanup.go:sameReapProcessIdentity.
	StartTimeTicks uint64
	// StartIdentity is the portable `ps -o lstart=` formatted start timestamp,
	// used as a fallback when StartTimeTicks is unavailable (macOS, locked-down
	// /proc). Mirrors the production reap algorithm.
	StartIdentity string
}

const (
	managedDoltTestModeEnv      = "GC_MANAGED_DOLT_TEST_MODE"
	managedDoltTestParentPIDEnv = "GC_MANAGED_DOLT_TEST_PARENT_PID"
	managedDoltTestWatchdogArg  = "__gc-managed-dolt-test-watchdog"
	// The first ExtraFiles entry is exposed to the child as fd 3.
	managedDoltTestParentPipeFD = 3
)

var (
	managedDoltTestMode                 = isTestBinary
	managedDoltTestExecutable           = os.Executable
	managedDoltTestWatchdogPIDTimeout   = 5 * time.Second
	managedDoltTestProcessRegistry      sync.Map
	managedDoltTestTerminateProcess     = terminateManagedDoltTestPID
	managedDoltTestReadStartTimeTicks   = readProcStartTimeTicks
	managedDoltTestReadStartIdentity    = readProcStartIdentity
	managedDoltTestProcessGroupKillWait = 2 * time.Second
)

// init is the re-entry point for the dolt-managed-test watchdog. The watchdog
// is a sibling process the test framework re-exec's via this binary so the
// managed `dolt sql-server` outlives the test parent and can be reliably
// reaped on parent exit (gastownhall/gascity#2306). It lives in init() —
// not in the cobra command tree — because the binary is re-exec'd as a
// child of the test parent, not invoked via `gc <subcommand>`. The cobra
// dispatch never runs in this mode; os.Exit terminates the process so no
// subsequent dispatch can produce a misleading "unknown command" error.
//
// The argv[1] sentinel is the sole, sufficient guard. It is a private
// re-exec marker (managedDoltTestWatchdogArg) that no production `gc`
// invocation ever passes, so its presence is itself the authorization to
// enter the watchdog. Checking it first means the watchdog works whether
// the re-exec target is a Go test binary OR a real `gc` binary —
// integration tests (e.g. TestInheritedExternalBdRigStoreConsistent...,
// TestCmdSessionWait...) start managed dolt through a real `gc` subprocess
// that re-execs itself as the watchdog, whose argv[0] does not contain
// ".test". A prior `isTestBinary()` pre-gate blocked that path: the
// sentinel argv fell through to cobra, which printed usage and exited 1
// ("dolt server could not start via gc helper") on every CI shard that
// exercised the real-binary dolt path (gastownhall/gascity#2313 follow-up
// CI regression).
//
// The stray-`GC_MANAGED_DOLT_TEST_MODE=1`-in-production threat is handled
// at the spawn decision (managedDoltTestWatchdogEnabled), not here — a
// production process is never re-exec'd with this sentinel, so reaching
// init() with it set is already proof of an intentional test re-exec.
func init() {
	if len(os.Args) < 2 || os.Args[1] != managedDoltTestWatchdogArg {
		return
	}
	os.Exit(runManagedDoltTestWatchdog(os.Args[2:], os.Stdout, os.Stderr))
}

func startManagedDoltProcess(cityPath, host, port, user, logLevel string, timeout time.Duration) (managedDoltStartReport, error) {
	return startManagedDoltProcessWithOptions(cityPath, host, port, user, logLevel, -1, timeout, true)
}

func startManagedDoltProcessWithOptions(cityPath, host, port, user, logLevel string, archiveLevel int, timeout time.Duration, publish bool) (managedDoltStartReport, error) {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return managedDoltStartReport{}, err
	}
	portNum, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil || portNum <= 0 {
		return managedDoltStartReport{}, fmt.Errorf("invalid port %q", port)
	}
	if strings.TrimSpace(host) == "" {
		host = "0.0.0.0"
	}
	if strings.TrimSpace(user) == "" {
		user = "root"
	}
	if strings.TrimSpace(logLevel) == "" {
		logLevel = "warning"
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	archiveLevel = resolveDoltArchiveLevel(archiveLevel)

	report := managedDoltStartReport{}
	currentPort := portNum
	for attempt := 1; attempt <= 5; attempt++ {
		report.Attempts = attempt
		report.AddressInUse = false

		if err := managedDoltPreflightCleanupFn(cityPath); err != nil {
			return report, err
		}
		if err := writeManagedDoltConfigFile(layout.ConfigFile, host, strconv.Itoa(currentPort), layout.DataDir, logLevel, archiveLevel); err != nil {
			return report, err
		}

		logOffset, err := managedDoltLogSize(layout.LogFile)
		if err != nil {
			return report, err
		}

		logFile, err := os.OpenFile(layout.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return report, fmt.Errorf("open log file: %w", err)
		}

		started, err := startManagedDoltSQLServer(cityPath, layout.ConfigFile, layout.LogFile, logFile)
		if err != nil {
			_ = logFile.Close()
			return report, err
		}
		_ = logFile.Close()

		report.PID = started.PID
		report.Port = currentPort
		if err := os.MkdirAll(filepath.Dir(layout.PIDFile), 0o755); err != nil {
			terminateManagedDoltStartedProcess(started)
			return report, fmt.Errorf("create pid dir: %w", err)
		}
		if err := os.WriteFile(layout.PIDFile, []byte(strconv.Itoa(started.PID)+"\n"), 0o644); err != nil {
			terminateManagedDoltStartedProcess(started)
			return report, fmt.Errorf("write pid file: %w", err)
		}
		if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
			Running:   true,
			PID:       started.PID,
			Port:      currentPort,
			DataDir:   layout.DataDir,
			StartedAt: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			terminateManagedDoltStartedProcess(started)
			_ = os.Remove(layout.PIDFile)
			return report, fmt.Errorf("write provider state: %w", err)
		}

		readyReport, readyErr := waitForManagedDoltReady(cityPath, host, strconv.Itoa(currentPort), user, started.PID, timeout, false)
		if readyErr == nil && readyReport.Ready {
			report.Ready = true
			if publish {
				if err := publishManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
					return report, fmt.Errorf("publish managed dolt runtime state: %w", err)
				}
			}
			disarmManagedDoltStartedProcess(started)
			return report, nil
		}

		if readyReport.PIDAlive {
			terminateManagedDoltStartedProcess(started)
			_ = os.Remove(layout.PIDFile)
			_ = writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
				Running:   false,
				PID:       0,
				Port:      currentPort,
				DataDir:   layout.DataDir,
				StartedAt: time.Now().UTC().Format(time.RFC3339),
			})
			return report, fmt.Errorf("dolt server started (pid %d) but did not become query-ready within %s (check %s)", started.PID, timeout, layout.LogFile)
		}

		_ = os.Remove(layout.PIDFile)
		_ = writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
			Running:   false,
			PID:       0,
			Port:      currentPort,
			DataDir:   layout.DataDir,
			StartedAt: time.Now().UTC().Format(time.RFC3339),
		})

		startupOutput, readErr := managedDoltLogSuffix(layout.LogFile, logOffset)
		if readErr == nil && strings.Contains(strings.ToLower(startupOutput), "address already in use") {
			report.AddressInUse = true
			currentPort = nextAvailableManagedDoltPort(currentPort + 1)
			report.Port = currentPort
			continue
		}
		if readyErr != nil {
			return report, fmt.Errorf("dolt server exited during startup: %w", readyErr)
		}
		return report, fmt.Errorf("dolt server exited during startup (check %s)", layout.LogFile)
	}

	return report, fmt.Errorf("dolt server could not find a free port after repeated address-in-use failures (last port %d)", report.Port)
}

func startManagedDoltSQLServer(cityPath, configFile, logFilePath string, logFile *os.File) (managedDoltStartedProcess, error) {
	if managedDoltTestWatchdogEnabled() {
		return startManagedDoltSQLServerWithTestWatchdog(cityPath, configFile, logFilePath, logFile)
	}
	cmd := exec.Command("dolt", "sql-server", "--config", configFile)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = managedDoltSQLServerSysProcAttr()
	cmd.Env = doltServerEnv(os.Environ())
	if err := cmd.Start(); err != nil {
		return managedDoltStartedProcess{}, fmt.Errorf("start dolt sql-server: %w", err)
	}
	return managedDoltStartedProcess{CityPath: cityPath, PID: cmd.Process.Pid}, nil
}

func startManagedDoltSQLServerWithTestWatchdog(cityPath, configFile, logFilePath string, logFile *os.File) (managedDoltStartedProcess, error) {
	disarmFile, err := managedDoltTestWatchdogDisarmFile(logFilePath)
	if err != nil {
		return managedDoltStartedProcess{}, err
	}
	watchdogExecutable, err := managedDoltTestWatchdogExecutable()
	if err != nil {
		_ = os.Remove(disarmFile)
		return managedDoltStartedProcess{}, err
	}
	args := []string{managedDoltTestWatchdogArg, managedDoltTestParentPIDString(), configFile, logFilePath, disarmFile}
	var parentPipeRead *os.File
	var parentPipeWrite *os.File
	if !managedDoltTestHasExternalParent() {
		parentPipeRead, parentPipeWrite, err = os.Pipe()
		if err != nil {
			_ = os.Remove(disarmFile)
			return managedDoltStartedProcess{}, fmt.Errorf("create dolt test watchdog parent pipe: %w", err)
		}
		args = append(args, strconv.Itoa(managedDoltTestParentPipeFD))
	}
	cmd := exec.Command(watchdogExecutable, args...)
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.Env = doltServerEnv(os.Environ())
	if parentPipeRead != nil {
		cmd.ExtraFiles = []*os.File{parentPipeRead}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		if parentPipeRead != nil {
			_ = parentPipeRead.Close()
		}
		if parentPipeWrite != nil {
			_ = parentPipeWrite.Close()
		}
		_ = os.Remove(disarmFile)
		return managedDoltStartedProcess{}, fmt.Errorf("prepare dolt test watchdog: %w", err)
	}
	if err := cmd.Start(); err != nil {
		if parentPipeRead != nil {
			_ = parentPipeRead.Close()
		}
		if parentPipeWrite != nil {
			_ = parentPipeWrite.Close()
		}
		_ = os.Remove(disarmFile)
		return managedDoltStartedProcess{}, fmt.Errorf("start dolt test watchdog: %w", err)
	}
	if parentPipeRead != nil {
		_ = parentPipeRead.Close()
	}
	pid, err := readManagedDoltTestWatchdogPID(stdout, cmd.Process.Pid)
	if err != nil {
		_ = terminateManagedDoltPID(cityPath, cmd.Process.Pid)
		_ = cmd.Wait()
		if parentPipeWrite != nil {
			_ = parentPipeWrite.Close()
		}
		_ = os.Remove(disarmFile)
		return managedDoltStartedProcess{}, err
	}
	go func() {
		_ = cmd.Wait()
		if parentPipeWrite != nil {
			_ = parentPipeWrite.Close()
		}
	}()
	started := managedDoltStartedProcess{
		CityPath:    cityPath,
		PID:         pid,
		WatchdogPID: cmd.Process.Pid,
		DisarmFile:  disarmFile,
		DisarmReady: managedDoltTestDisarmOnReady(),
	}
	registerManagedDoltTestProcess(started)
	return started, nil
}

func managedDoltTestWatchdogExecutable() (string, error) {
	executable, executableErr := managedDoltTestExecutable()
	if executableErr == nil && strings.TrimSpace(executable) != "" {
		return executable, nil
	}
	fallback := strings.TrimSpace(os.Args[0])
	if fallback == "" {
		if executableErr != nil {
			return "", fmt.Errorf("resolve dolt test watchdog executable: os.Executable: %w", executableErr)
		}
		return "", fmt.Errorf("resolve dolt test watchdog executable: os.Executable returned empty path")
	}
	if filepath.IsAbs(fallback) {
		return fallback, nil
	}
	abs, err := filepath.Abs(fallback)
	if err != nil {
		return "", fmt.Errorf("resolve dolt test watchdog executable from argv %q: %w", fallback, err)
	}
	return abs, nil
}

func managedDoltTestWatchdogDisarmFile(logFilePath string) (string, error) {
	dir := filepath.Dir(logFilePath)
	file, err := os.CreateTemp(dir, ".dolt-watchdog-disarm-*")
	if err != nil {
		return "", fmt.Errorf("create dolt test watchdog disarm file: %w", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close dolt test watchdog disarm file: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("remove dolt test watchdog disarm file: %w", err)
	}
	return path, nil
}

func readManagedDoltTestWatchdogPID(r io.Reader, watchdogPID int) (int, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadString('\n')
		ch <- result{line: line, err: err}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			return 0, fmt.Errorf("read dolt test watchdog pid: %w", res.err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(res.line))
		if err != nil || pid <= 0 {
			return 0, fmt.Errorf("read dolt test watchdog pid: invalid pid %q", strings.TrimSpace(res.line))
		}
		return pid, nil
	case <-time.After(managedDoltTestWatchdogPIDTimeout):
		return 0, fmt.Errorf("dolt test watchdog pid timed out (watchdog pid %d)", watchdogPID)
	}
}

func managedDoltSQLServerSysProcAttr() *syscall.SysProcAttr {
	if managedDoltTestModeEnabled() {
		return nil
	}
	return &syscall.SysProcAttr{Setpgid: true}
}

func managedDoltTestWatchdogEnabled() bool {
	return managedDoltTestModeEnabled() && os.Getenv("GC_MANAGED_DOLT_TEST_WATCHDOG") != "0"
}

func managedDoltTestModeEnabled() bool {
	return managedDoltTestMode() || os.Getenv(managedDoltTestModeEnv) == "1"
}

func managedDoltTestModeFromEnvOnly() bool {
	return !managedDoltTestMode() && os.Getenv(managedDoltTestModeEnv) == "1"
}

func managedDoltTestParentPID() int {
	raw := strings.TrimSpace(os.Getenv(managedDoltTestParentPIDEnv))
	if raw != "" {
		if pid, err := strconv.Atoi(raw); err == nil && pid > 0 {
			return pid
		}
	}
	return os.Getpid()
}

func managedDoltTestParentPIDString() string {
	return strconv.Itoa(managedDoltTestParentPID())
}

func managedDoltTestHasExternalParent() bool {
	raw := strings.TrimSpace(os.Getenv(managedDoltTestParentPIDEnv))
	if raw == "" {
		return false
	}
	pid, err := strconv.Atoi(raw)
	return err == nil && pid > 0 && pid != os.Getpid()
}

func managedDoltTestDisarmOnReady() bool {
	return managedDoltTestModeFromEnvOnly() && !managedDoltTestHasExternalParent()
}

func terminateManagedDoltStartedProcess(started managedDoltStartedProcess) {
	unregisterManagedDoltStartedProcess(started)
	_ = terminateManagedDoltPID(started.CityPath, started.PID)
	if started.WatchdogPID > 0 {
		_ = terminateManagedDoltPID(started.CityPath, started.WatchdogPID)
	}
	if started.DisarmFile != "" {
		_ = os.Remove(started.DisarmFile)
	}
}

func unregisterManagedDoltStartedProcess(started managedDoltStartedProcess) {
	unregisterManagedDoltTestProcess(started.PID)
	unregisterManagedDoltTestProcess(started.WatchdogPID)
}

// terminateManagedDoltTestPID stops a managed dolt test process. When the
// target is its own process-group leader (Setpgid was applied at spawn — see
// runManagedDoltTestWatchdog), it signals the entire process group so descendant
// dolt workers do not outlive the test parent (gastownhall/gascity#2313 follow-up
// M3). When the target is NOT a group leader (e.g. the watchdog itself, which
// inherits the test binary's process group), it falls back to leader-only
// termination so we never accidentally signal the test binary's group.
func terminateManagedDoltTestPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil || pgid != pid || pgid <= 1 {
		return terminateManagedDoltPID("", pid)
	}
	if killErr := syscall.Kill(-pgid, syscall.SIGTERM); killErr != nil {
		return terminateManagedDoltPID("", pid)
	}
	deadline := time.Now().Add(managedDoltTestProcessGroupKillWait)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	time.Sleep(250 * time.Millisecond)
	return nil
}

func disarmManagedDoltStartedProcess(started managedDoltStartedProcess) {
	if started.DisarmFile == "" || !started.DisarmReady {
		return
	}
	if err := os.WriteFile(started.DisarmFile, []byte("ready\n"), 0o644); err != nil {
		return
	}
	unregisterManagedDoltTestProcess(started.PID)
}

func registerManagedDoltTestProcess(started managedDoltStartedProcess) {
	if started.PID <= 0 || !managedDoltTestModeEnabled() {
		return
	}
	// Snapshot the OS-level start identity at registration. We re-read it
	// just before signaling in reapManagedDoltTestProcesses so PID reuse
	// (a fresh process landing on the same numeric PID after dolt exited)
	// cannot cause us to kill an unrelated process. Either field may be
	// empty/zero depending on the host (no /proc, no usable ps); the reap
	// path checks both with the same fallback ordering as the production
	// reaper's sameReapProcessIdentity.
	if started.StartTimeTicks == 0 {
		started.StartTimeTicks = managedDoltTestReadStartTimeTicks(started.PID)
	}
	if started.StartIdentity == "" {
		started.StartIdentity = managedDoltTestReadStartIdentity(started.PID)
	}
	managedDoltTestProcessRegistry.Store(started.PID, started)
}

func unregisterManagedDoltTestProcess(pid int) {
	if pid <= 0 {
		return
	}
	managedDoltTestProcessRegistry.Delete(pid)
}

func reapManagedDoltTestProcesses() {
	managedDoltTestProcessRegistry.Range(func(key, value any) bool {
		started, ok := value.(managedDoltStartedProcess)
		if !ok {
			managedDoltTestProcessRegistry.Delete(key)
			return true
		}
		if started.PID > 0 && pidAlive(started.PID) && managedDoltTestPIDIdentityMatches(started) {
			_ = managedDoltTestTerminateProcess(started.PID)
		}
		if started.WatchdogPID > 0 && pidAlive(started.WatchdogPID) {
			// Watchdog identity is not snapshotted; the watchdog is short-lived
			// and exits with the dolt sql-server. Terminate leader-only.
			_ = managedDoltTestTerminateProcess(started.WatchdogPID)
		}
		managedDoltTestProcessRegistry.Delete(key)
		return true
	})
}

// managedDoltTestPIDIdentityMatches re-reads the OS-level start identity for
// started.PID and compares it against the snapshot taken at registration. If
// both snapshots are present and disagree, the PID was reused — we must NOT
// terminate. If neither snapshot is present, we can't verify and fall through
// to the existing behavior (terminate). This mirrors the production reaper's
// sameReapProcessIdentity (cmd_dolt_cleanup.go) precedence: ticks first, ps
// lstart as fallback.
func managedDoltTestPIDIdentityMatches(started managedDoltStartedProcess) bool {
	if started.StartTimeTicks != 0 {
		current := managedDoltTestReadStartTimeTicks(started.PID)
		if current == 0 {
			return true
		}
		return current == started.StartTimeTicks
	}
	if started.StartIdentity != "" {
		current := managedDoltTestReadStartIdentity(started.PID)
		if current == "" {
			return true
		}
		return current == started.StartIdentity
	}
	return true
}

func managedDoltStartFields(report managedDoltStartReport) []string {
	return []string{
		"ready\t" + strconv.FormatBool(report.Ready),
		"pid\t" + strconv.Itoa(report.PID),
		"port\t" + strconv.Itoa(report.Port),
		"address_in_use\t" + strconv.FormatBool(report.AddressInUse),
		"attempts\t" + strconv.Itoa(report.Attempts),
	}
}

func managedDoltLogSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return info.Size(), nil
}

func managedDoltLogSuffix(path string, offset int64) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if offset >= int64(len(data)) {
		return "", nil
	}
	if offset < 0 {
		offset = 0
	}
	return string(data[offset:]), nil
}

// resolveDoltArchiveLevel resolves the archive level for dolt auto_gc.
// Explicit non-negative values are returned as-is. Negative values trigger
// env-var fallback (GC_DOLT_ARCHIVE_LEVEL), defaulting to 0.
func resolveDoltArchiveLevel(explicit int) int {
	if explicit >= 0 {
		return explicit
	}
	if v := os.Getenv("GC_DOLT_ARCHIVE_LEVEL"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			return parsed
		}
	}
	return 0
}

// terminateManagedDoltPID stops a managed dolt subprocess on startup-failure
// and failed-recovery cleanup. It honors the same configurable SIGTERM→SIGKILL
// grace as the stop/unregister/restart path (resolveManagedDoltStopTimeout) so
// a too-short hardcoded grace cannot SIGKILL dolt mid-flush on these paths
// either (gastownhall/gascity#2090). cityPath may be empty — the grace then
// falls back to config.DefaultDoltStopTimeout.
//
// The liveness-poll interval is clamped to the grace via
// managedDoltStopPollInterval, matching the stop/unregister path: without the
// clamp a sub-100ms configured grace would still sleep a fixed ~100ms before
// the first re-check, sending SIGKILL well past the intended deadline.
func terminateManagedDoltPID(cityPath string, pid int) error {
	if pid <= 0 {
		return nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	_ = process.Signal(syscall.SIGTERM)
	gracePeriod := resolveManagedDoltStopTimeout(cityPath)
	deadline := time.Now().Add(gracePeriod)
	pollInterval := managedDoltStopPollInterval(gracePeriod)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return nil
		}
		time.Sleep(pollInterval)
	}
	_ = process.Signal(syscall.SIGKILL)
	time.Sleep(250 * time.Millisecond)
	return nil
}

func runManagedDoltTestWatchdog(args []string, stdout, stderr *os.File) int {
	if !managedDoltTestModeEnabled() {
		fmt.Fprintln(stderr, "managed dolt test watchdog is only available in managed Dolt test mode") //nolint:errcheck
		return 2
	}
	if len(args) != 4 && len(args) != 5 {
		fmt.Fprintf(stderr, "usage: %s <parent-pid> <config-file> <log-file> <disarm-file> [parent-pipe-fd]\n", managedDoltTestWatchdogArg) //nolint:errcheck
		return 2
	}
	parentPID, err := strconv.Atoi(args[0])
	if err != nil || parentPID <= 0 {
		fmt.Fprintf(stderr, "invalid parent pid %q\n", args[0]) //nolint:errcheck
		return 2
	}
	configFile := args[1]
	logFilePath := args[2]
	disarmFile := args[3]
	var parentDone <-chan struct{}
	if len(args) == 5 {
		done, closeParentDone, err := managedDoltTestParentDone(args[4])
		if err != nil {
			fmt.Fprintf(stderr, "watch parent pipe: %v\n", err) //nolint:errcheck
			return 2
		}
		parentDone = done
		defer closeParentDone()
	}
	logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(stderr, "open dolt log: %v\n", err) //nolint:errcheck
		return 1
	}
	defer logFile.Close() //nolint:errcheck

	cmd := exec.Command("dolt", "sql-server", "--config", configFile)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	// Setpgid makes the dolt sql-server the leader of its own process group
	// so terminateManagedDoltTestPID can kill the entire descendant tree
	// (kill(-pgid, ...)). Without this, dolt children (e.g. auto_gc helpers,
	// archive workers) outlive their parent and leak across test runs
	// (gastownhall/gascity#2313 follow-up M3).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = doltServerEnv(os.Environ())
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "start dolt sql-server: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stdout, "%d\n", cmd.Process.Pid) //nolint:errcheck

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-signals:
			_ = terminateManagedDoltPID("", cmd.Process.Pid)
			<-done
			return 0
		case <-parentDone:
			if _, err := os.Stat(disarmFile); err == nil {
				_ = os.Remove(disarmFile)
				return 0
			}
			_ = terminateManagedDoltPID("", cmd.Process.Pid)
			<-done
			return 0
		case <-ticker.C:
			if _, err := os.Stat(disarmFile); err == nil {
				_ = os.Remove(disarmFile)
				return 0
			}
			if !pidutil.Alive(parentPID) {
				_ = terminateManagedDoltPID("", cmd.Process.Pid)
				<-done
				return 0
			}
		case err := <-done:
			if err != nil {
				return 1
			}
			return 0
		}
	}
}

func managedDoltTestParentDone(rawFD string) (<-chan struct{}, func(), error) {
	fd, err := strconv.Atoi(strings.TrimSpace(rawFD))
	if err != nil || fd <= 2 {
		return nil, nil, fmt.Errorf("invalid parent pipe fd %q", rawFD)
	}
	parentPipe := os.NewFile(uintptr(fd), "gc-managed-dolt-test-parent")
	if parentPipe == nil {
		return nil, nil, fmt.Errorf("open parent pipe fd %d", fd)
	}
	syscall.CloseOnExec(fd)
	done := make(chan struct{})
	go func() {
		var buf [1]byte
		_, _ = parentPipe.Read(buf[:])
		close(done)
	}()
	return done, func() { _ = parentPipe.Close() }, nil
}

// doltServerEnv returns the environment applied to every managed dolt
// sql-server we launch.
func doltServerEnv(parent []string) []string {
	return append([]string(nil), parent...)
}
