package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
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
	// WatchdogStartTimeTicks and WatchdogStartIdentity snapshot the supervising
	// watchdog's OS start identity at spawn, with the same precedence and
	// fallback as StartTimeTicks/StartIdentity. The parent reaps the watchdog
	// with a background cmd.Wait() just like the dolt child, so a watchdog PID
	// can be freed and reused mid-cleanup; startup-failure cleanup re-verifies
	// these before signaling WatchdogPID. Zero/empty when there is no watchdog.
	WatchdogStartTimeTicks uint64
	WatchdogStartIdentity  string
}

// defaultManagedDoltBindHost is the listener host a managed dolt sql-server
// binds when no explicit host is configured. Loopback by default: the work
// ledger must not listen on a wildcard (LAN-reachable) interface unless the
// operator explicitly opts in with GC_DOLT_HOST=0.0.0.0. Distinct from
// defaultManagedDoltHost (bd_env.go), which is the client-side connect
// default.
const defaultManagedDoltBindHost = "127.0.0.1"

// normalizeManagedDoltBindHost resolves the listener host for a managed dolt
// sql-server. Blank means "no explicit choice" and resolves to the loopback
// default; any explicit value — including the 0.0.0.0 wildcard opt-out for
// multi-host deployments — is preserved.
func normalizeManagedDoltBindHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return defaultManagedDoltBindHost
	}
	return host
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

// Indirections for the inner loop of startManagedDoltProcessWithOptions so
// the address-in-use retry branch is exercisable by unit tests without
// spawning a real dolt subprocess. Production wires these to the same
// concrete functions invoked previously; tests in
// dolt_start_address_in_use_retry_window_test.go drive the loop body by
// stubbing them. Reassigning these in production code paths is a bug.
var (
	managedDoltStartSQLServerFn = startManagedDoltSQLServer
	managedDoltWaitForReadyFn   = waitForManagedDoltReady
	managedDoltLogSuffixFn      = managedDoltLogSuffix
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
// integration tests (e.g.
// TestManagedBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore and
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

//nolint:unparam // archiveLevel is an explicit override hook; current callers use config/env fallback.
func startManagedDoltProcessWithOptions(cityPath, host, port, user, logLevel string, archiveLevel int, timeout time.Duration, publish bool) (managedDoltStartReport, error) {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return managedDoltStartReport{}, err
	}
	if err := checkManagedDoltDiskPreflight(layout.DataDir, doltDiskMinFreeBytes(), doltDiskWarnFreeBytes(), os.Stderr); err != nil {
		return managedDoltStartReport{}, err
	}
	portNum, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil || portNum <= 0 {
		return managedDoltStartReport{}, fmt.Errorf("invalid port %q", port)
	}
	host = normalizeManagedDoltBindHost(host)
	if strings.TrimSpace(user) == "" {
		user = "root"
	}
	if strings.TrimSpace(logLevel) == "" {
		logLevel = "warning"
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	report := managedDoltStartReport{}
	doltConfig, err := resolveManagedDoltConfigForStart(cityPath, archiveLevel)
	if err != nil {
		return report, err
	}

	// Lock-keyed singleton guard (gastownhall/gascity#3174). Dolt holds an
	// exclusive flock on each database's `.dolt/noms/LOCK` until its chunk
	// journal is flushed and the store is closed; TCP teardown happens
	// earlier. Waiting on the lock — not the port — guarantees a prior
	// instance has finished writing before we bind the same data_dir. If the
	// lock is still held after the configured window, fail closed: refusing
	// to start is recoverable, a corrupted noms journal is not.
	if err := waitForManagedDoltDataDirLockFree(layout.DataDir, managedDoltLockReleaseTimeoutFn(cityPath)); err != nil {
		return report, fmt.Errorf("refusing to start dolt sql-server for %s: %w", layout.DataDir, err)
	}

	currentPort := portNum
	// retryWindow is resolved once before the loop so an in-progress
	// city.toml edit cannot change the wait policy mid-flight.
	retryWindow := managedDoltStartAddressInUseRetryWindowFn(cityPath)
	// waitedPorts records each port we have already slept on for the
	// address-in-use retry. Each port gets at most ONE retry window's worth
	// of wait — if dolt still cannot bind after that, we fall through to
	// `nextAvailableManagedDoltPort` rather than burn another window. This
	// bounds the worst-case wall time per startManagedDoltProcessWithOptions
	// to (retryWindow + per-attempt-startup) × min(5, distinct-ports-tried)
	// rather than (retryWindow × 5).
	waitedPorts := make(map[int]bool, 5)
	for attempt := 1; attempt <= 5; attempt++ {
		report.Attempts = attempt
		report.AddressInUse = false

		if err := managedDoltPreflightCleanupFn(cityPath); err != nil {
			return report, err
		}
		if err := writeManagedDoltConfigFile(layout.ConfigFile, host, strconv.Itoa(currentPort), layout.DataDir, logLevel, doltConfig); err != nil {
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

		started, err := managedDoltStartSQLServerFn(cityPath, layout.ConfigFile, layout.LogFile, logFile)
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

		readyReport, readyErr := managedDoltWaitForReadyFn(cityPath, host, strconv.Itoa(currentPort), user, started.PID, timeout, false)
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

		startupOutput, readErr := managedDoltLogSuffixFn(layout.LogFile, logOffset)
		if readErr == nil && strings.Contains(strings.ToLower(startupOutput), "address already in use") {
			report.AddressInUse = true
			// Wait briefly on the originally requested port to outlast a
			// TIME_WAIT socket before bumping ports. See
			// `DoltStartAddressInUseRetryWindow` doc for the design rationale.
			// Each port gets at most one wait; if dolt still cannot bind
			// after that, fall through to the next free port.
			if retryWindow > 0 && !waitedPorts[currentPort] &&
				managedDoltStartWaitForPortFree(host, currentPort, retryWindow) {
				waitedPorts[currentPort] = true
				continue // retry the same port without bumping
			}
			waitedPorts[currentPort] = true
			currentPort = nextAvailableManagedDoltPortForHost(host, currentPort+1)
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

// managedDoltLockReleaseTimeoutFn resolves the configured wait window for
// dolt's on-disk exclusive store lock in the start guard. Package-level var
// so tests can shim the window without writing a city.toml. Production
// points at resolveManagedDoltLockReleaseTimeout.
var managedDoltLockReleaseTimeoutFn = resolveManagedDoltLockReleaseTimeout

// managedDoltStartAddressInUseRetryWindowFn resolves the configured retry window for
// the address-in-use loop in startManagedDoltProcessWithOptions. It is a
// package-level var so tests can shim the resolution without writing a
// city.toml. Production points at resolveManagedDoltStartAddressInUseRetryWindow.
var managedDoltStartAddressInUseRetryWindowFn = resolveManagedDoltStartAddressInUseRetryWindow

// resolveManagedDoltStartAddressInUseRetryWindow returns how long the managed-dolt
// start path should wait on the originally requested port before falling back
// to a higher port when bind fails with "address already in use". Reads
// `[daemon].dolt_start_address_in_use_retry_window` from city.toml when available;
// falls back to config.DefaultDoltStartAddressInUseRetryWindow when the config
// cannot be loaded.
//
// Mirrors resolveManagedDoltStopTimeout's empty-cityPath guard: recovery /
// startup-cleanup callers may pass an empty cityPath, and loadCityConfig("",…)
// would resolve "city.toml" relative to the current working directory,
// materializing builtin packs under cwd and reading an unrelated config.
func resolveManagedDoltStartAddressInUseRetryWindow(cityPath string) time.Duration {
	if strings.TrimSpace(cityPath) == "" {
		return config.DefaultDoltStartAddressInUseRetryWindow
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil || cfg == nil {
		return config.DefaultDoltStartAddressInUseRetryWindow
	}
	return cfg.Daemon.DoltStartAddressInUseRetryWindowDuration()
}

// managedDoltStartWaitForPortFree polls managedDoltPortAvailableFn for
// host:port every managedDoltStartAddressInUsePollInterval(retryWindow) until
// the port becomes free or retryWindow expires. Returns true if the port
// became free within the window. A non-positive retryWindow returns false
// immediately (no wait).
//
// The host argument matches the host dolt will bind to (typically
// "127.0.0.1" in production); using the same host for the probe and the
// bind avoids
// false-positive availability reports caused by interface-specific bind
// states. The poll interval is shrunk to the retry window when the window is
// shorter than the default 2s, so a sub-2s window still gets one check
// before falling through. A final post-deadline check catches a port that
// freed up between the last sleep and the deadline.
func managedDoltStartWaitForPortFree(host string, port int, retryWindow time.Duration) bool {
	if retryWindow <= 0 {
		return false
	}
	poll := managedDoltStartAddressInUsePollInterval(retryWindow)
	deadline := time.Now().Add(retryWindow)
	for time.Now().Before(deadline) {
		if managedDoltPortAvailableFn(host, port) {
			return true
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			break
		}
		if remain < poll {
			time.Sleep(remain)
		} else {
			time.Sleep(poll)
		}
	}
	return managedDoltPortAvailableFn(host, port)
}

// managedDoltStartAddressInUsePollInterval returns the per-iteration sleep for
// managedDoltStartWaitForPortFree. Normally 2s; shrunk to the retry window
// when the window is shorter so at least one check happens before the
// deadline.
func managedDoltStartAddressInUsePollInterval(retryWindow time.Duration) time.Duration {
	const defaultPoll = 2 * time.Second
	if retryWindow > 0 && retryWindow < defaultPoll {
		return retryWindow
	}
	return defaultPoll
}

// managedDoltPortAvailableFn is a package-level indirection over the host:port
// availability probe so tests can simulate TIME_WAIT clearing without holding
// real sockets. Production points at managedDoltPortAvailableForHost, which
// runs a real net.Listen probe on the same host dolt will bind to.
var managedDoltPortAvailableFn = managedDoltPortAvailableForHost

// managedDoltPortAvailableForHost reports whether host:port can currently be
// bound by a Go net.Listen call. Mirrors managedDoltPortAvailable's check but
// uses the configured host instead of forcing 127.0.0.1, so the probe is
// faithful to what dolt's bind will attempt (interface-specific TIME_WAIT
// state on a wildcard bind is not seen by a localhost probe). A "*" host is
// an explicit wildcard spelling and normalizes to "0.0.0.0"; a blank host
// normalizes to the loopback bind default, matching
// startManagedDoltProcessWithOptions.
func managedDoltPortAvailableForHost(host string, port int) bool {
	if strings.TrimSpace(host) == "*" {
		host = "0.0.0.0"
	}
	host = normalizeManagedDoltBindHost(host)
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = listener.Close() //nolint:errcheck // best-effort cleanup
	return true
}

func startManagedDoltSQLServer(cityPath, configFile, logFilePath string, logFile *os.File) (managedDoltStartedProcess, error) {
	if managedDoltTestWatchdogEnabled() {
		return startManagedDoltSQLServerWithTestWatchdog(cityPath, configFile, logFilePath, logFile)
	}
	if managedDoltScopeWatchdogEnabled() {
		return startManagedDoltSQLServerWithScopeWatchdog(cityPath, configFile, logFilePath, logFile)
	}
	cmd := exec.Command("dolt", "sql-server", "--config", configFile)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = managedDoltSQLServerSysProcAttr()
	cmd.Env = doltServerEnv(cityPath, os.Environ())
	if err := cmd.Start(); err != nil {
		return managedDoltStartedProcess{}, fmt.Errorf("start dolt sql-server: %w", err)
	}
	// Snapshot the child's OS-level start identity while it is still definitely
	// alive — before the reap goroutine below can Wait() it and free the PID.
	// A same-attempt startup-failure cleanup (terminateManagedDoltStartedProcess)
	// re-reads this before signaling, so a numeric PID reused after this child
	// exits and is reaped is never signaled. Mirrors the test reaper's
	// registration snapshot and the production reaper
	// (cmd_dolt_cleanup.go:sameReapProcessIdentity): start-time ticks first, ps
	// lstart as the no-/proc fallback.
	pid := cmd.Process.Pid
	startTimeTicks, startIdentity := snapshotManagedDoltStartIdentity(pid)
	started := managedDoltStartedProcess{
		CityPath:       cityPath,
		PID:            pid,
		StartTimeTicks: startTimeTicks,
		StartIdentity:  startIdentity,
	}
	// Reap the child when it exits so it does not linger as a zombie under a
	// non-reaping PID-1 controller. This server is managed out-of-band by PID
	// (health checks, terminateManagedDoltPID); the returned struct carries no
	// *exec.Cmd, so this goroutine is the sole Wait — matching the scope/test
	// watchdog paths, which already reap via a background cmd.Wait().
	go func() { _ = cmd.Wait() }()
	return started, nil
}

// snapshotManagedDoltStartIdentity captures a live PID's OS-level start identity
// for the PID-reuse guard. It reads /proc start-time ticks first and only forks
// `ps -o lstart=` when ticks are unavailable (macOS, locked-down /proc), so the
// common Linux path — the container target — never spawns ps on the managed-dolt
// startup critical path. The precedence mirrors the guard itself
// (managedDoltStartedPIDIdentityMatches): where ticks are present the ps fallback
// is never consulted, so reading it eagerly only burdens the hot path (and, on
// the scope-watchdog handshake, couples the parent's read to ps's own timeout).
func snapshotManagedDoltStartIdentity(pid int) (uint64, string) {
	if ticks := readProcStartTimeTicks(pid); ticks != 0 {
		return ticks, ""
	}
	return 0, readProcStartIdentity(pid)
}

func startManagedDoltSQLServerWithTestWatchdog(cityPath, configFile, logFilePath string, logFile *os.File) (managedDoltStartedProcess, error) {
	disarmFile, err := managedDoltTestWatchdogDisarmFile(logFilePath)
	if err != nil {
		return managedDoltStartedProcess{}, err
	}
	watchdogExecutable, err := managedDoltWatchdogExecutable()
	if err != nil {
		_ = os.Remove(disarmFile)
		return managedDoltStartedProcess{}, err
	}
	args := []string{managedDoltTestWatchdogArg, managedDoltTestParentPIDString(), configFile, logFilePath, disarmFile, cityPath}
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
	cmd.Env = doltServerEnv(cityPath, os.Environ())
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

// managedDoltWatchdogExecutable resolves the gc binary to re-exec as a
// managed-dolt watchdog. It serves both the test watchdog and the
// production scope watchdog (dolt_scope_watchdog.go).
func managedDoltWatchdogExecutable() (string, error) {
	executable, executableErr := managedDoltTestExecutable()
	if executableErr == nil && strings.TrimSpace(executable) != "" {
		return executable, nil
	}
	fallback := strings.TrimSpace(os.Args[0])
	if fallback == "" {
		if executableErr != nil {
			return "", fmt.Errorf("resolve dolt watchdog executable: os.Executable: %w", executableErr)
		}
		return "", fmt.Errorf("resolve dolt watchdog executable: os.Executable returned empty path")
	}
	if filepath.IsAbs(fallback) {
		return fallback, nil
	}
	abs, err := filepath.Abs(fallback)
	if err != nil {
		return "", fmt.Errorf("resolve dolt watchdog executable from argv %q: %w", fallback, err)
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

// readManagedDoltWatchdogLine reads one newline-terminated handshake line from a
// watchdog's stdout, bounded by managedDoltTestWatchdogPIDTimeout so a wedged
// watchdog cannot hang the caller. Shared by the test watchdog (PID only) and
// the production scope watchdog (PID + start identity).
func readManagedDoltWatchdogLine(r io.Reader, watchdogPID int) (string, error) {
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
			return "", res.err
		}
		return res.line, nil
	case <-time.After(managedDoltTestWatchdogPIDTimeout):
		return "", fmt.Errorf("dolt watchdog handshake timed out (watchdog pid %d)", watchdogPID)
	}
}

func readManagedDoltTestWatchdogPID(r io.Reader, watchdogPID int) (int, error) {
	line, err := readManagedDoltWatchdogLine(r, watchdogPID)
	if err != nil {
		return 0, fmt.Errorf("read dolt test watchdog pid: %w", err)
	}
	trimmed := strings.TrimSpace(line)
	pid, err := strconv.Atoi(trimmed)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("read dolt test watchdog pid: invalid pid %q", trimmed)
	}
	return pid, nil
}

// managedDoltWatchdogStartFieldSep separates the PID, start-time ticks, and
// start-identity fields on the scope watchdog's one-line stdout handshake. Tab
// is safe because the ps-lstart identity contains spaces but never a tab or a
// newline, so the identity survives as a single trailing field.
const managedDoltWatchdogStartFieldSep = "\t"

// formatManagedDoltWatchdogStartLine renders the scope watchdog's stdout
// handshake: the dolt child PID plus the OS start identity the parent uses to
// guard against PID reuse. Either identity field may be zero/empty on hosts
// without /proc or ps; the parent then behaves exactly like the direct-spawn
// path with no snapshot.
func formatManagedDoltWatchdogStartLine(pid int, startTimeTicks uint64, startIdentity string) string {
	return strings.Join([]string{
		strconv.Itoa(pid),
		strconv.FormatUint(startTimeTicks, 10),
		startIdentity,
	}, managedDoltWatchdogStartFieldSep)
}

// parseManagedDoltWatchdogStartLine parses the scope watchdog handshake back
// into the dolt child PID and its OS start identity. A legacy bare-PID line
// (no identity fields) parses with a zero identity, keeping the reader tolerant
// of an older watchdog binary. An empty or non-numeric PID field is an error.
func parseManagedDoltWatchdogStartLine(line string) (pid int, startTimeTicks uint64, startIdentity string, err error) {
	fields := strings.SplitN(strings.TrimRight(line, "\r\n"), managedDoltWatchdogStartFieldSep, 3)
	pidField := strings.TrimSpace(fields[0])
	pid, err = strconv.Atoi(pidField)
	if err != nil || pid <= 0 {
		return 0, 0, "", fmt.Errorf("invalid dolt watchdog pid %q", pidField)
	}
	if len(fields) >= 2 {
		startTimeTicks, _ = strconv.ParseUint(strings.TrimSpace(fields[1]), 10, 64)
	}
	if len(fields) >= 3 {
		startIdentity = strings.TrimSpace(fields[2])
	}
	return pid, startTimeTicks, startIdentity, nil
}

// readManagedDoltScopeWatchdogStart reads the scope watchdog's stdout handshake
// (PID + OS start identity), bounded by the same timeout as the test watchdog
// PID read so a wedged watchdog cannot hang startup. The identity lets the
// parent guard startup-failure cleanup against numeric PID reuse on the
// production scope-watchdog path.
func readManagedDoltScopeWatchdogStart(r io.Reader, watchdogPID int) (pid int, startTimeTicks uint64, startIdentity string, err error) {
	line, err := readManagedDoltWatchdogLine(r, watchdogPID)
	if err != nil {
		return 0, 0, "", fmt.Errorf("read dolt scope watchdog start: %w", err)
	}
	return parseManagedDoltWatchdogStartLine(line)
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

// managedDoltCleanupLogf records a managed-dolt startup-failure cleanup
// diagnostic. It defaults to a GC_DEBUG-gated stderr line so the decision is
// observable to an operator without noising the default CLI; the production
// reaper keeps an analogous audit via appendProtectedPID. It is a package var
// so tests can capture the message.
var managedDoltCleanupLogf = func(format string, args ...any) {
	if !gcDebugEnabled() {
		return
	}
	fmt.Fprintf(os.Stderr, "managed-dolt cleanup: "+format+"\n", args...)
}

func terminateManagedDoltStartedProcess(started managedDoltStartedProcess) {
	unregisterManagedDoltStartedProcess(started)
	// The reap goroutine in startManagedDoltSQLServer (and the scope watchdog)
	// Wait()s a failed child and frees its PID, so an unrelated process can reuse
	// it before — or during — this same-attempt cleanup; signaling by bare PID
	// would then hit the wrong process. terminateManagedDoltPIDGuarded re-verifies
	// the snapshotted start identity immediately before SIGTERM and again before
	// the SIGKILL escalation, skipping (and logging) the signal on a mismatch.
	// When no identity was snapshotted (no /proc and no ps) the guard keeps the
	// legacy terminate.
	_ = terminateManagedDoltPIDGuarded(started.CityPath, started.PID, func() bool {
		return managedDoltPIDStartIdentityMatches(started.PID, started.StartTimeTicks, started.StartIdentity)
	})
	// The watchdog PID carries the same reuse hazard — the parent reaps it too —
	// so guard it against its own snapshot rather than signaling it blind.
	if started.WatchdogPID > 0 {
		_ = terminateManagedDoltPIDGuarded(started.CityPath, started.WatchdogPID, func() bool {
			return managedDoltPIDStartIdentityMatches(started.WatchdogPID, started.WatchdogStartTimeTicks, started.WatchdogStartIdentity)
		})
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
		if started.PID > 0 && pidAlive(started.PID) && managedDoltStartedPIDIdentityMatches(started) {
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

// managedDoltStartedPIDIdentityMatches re-reads the OS-level start identity for
// started.PID and compares it against the snapshot taken when the process was
// started/registered. It guards both the background test reaper
// (reapManagedDoltTestProcesses) and the synchronous production startup-failure
// cleanup (terminateManagedDoltStartedProcess). A present snapshot that
// disagrees with the re-read means the PID was reused — we must NOT terminate.
func managedDoltStartedPIDIdentityMatches(started managedDoltStartedProcess) bool {
	return managedDoltPIDStartIdentityMatches(started.PID, started.StartTimeTicks, started.StartIdentity)
}

// managedDoltPIDStartIdentityMatches re-reads pid's OS-level start identity and
// compares it against a snapshot (startTimeTicks/startIdentity captured while the
// process was known alive). It backs managedDoltStartedPIDIdentityMatches and the
// SIGTERM/SIGKILL re-checks in terminateManagedDoltPIDGuarded, including the
// watchdog PID's own snapshot.
//
// Only the identity PRECEDENCE mirrors the production reaper's
// sameReapProcessIdentity (cmd_dolt_cleanup.go): start-time ticks first, ps
// lstart as the no-/proc fallback. The unconfirmed-re-read DEFAULT intentionally
// diverges. Where the production reaper protects (returns false) when it cannot
// re-confirm a live process, this returns true — keep the legacy terminate —
// because these callers reach here only for a PID they just observed alive and
// own: if the re-read now yields nothing the PID has genuinely vanished, so the
// ensuing signal is a harmless ESRCH no-op, while still cleaning up our own
// failed child on a host that momentarily cannot read /proc. When neither
// snapshot was captured at all (no /proc and no ps) the guard likewise cannot
// verify and keeps the legacy terminate.
func managedDoltPIDStartIdentityMatches(pid int, startTimeTicks uint64, startIdentity string) bool {
	if startTimeTicks != 0 {
		current := managedDoltTestReadStartTimeTicks(pid)
		if current == 0 {
			return true
		}
		return current == startTimeTicks
	}
	if startIdentity != "" {
		current := managedDoltTestReadStartIdentity(pid)
		if current == "" {
			return true
		}
		return current == startIdentity
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

func resolveManagedDoltConfigForStart(cityPath string, explicitArchiveLevel int) (config.DoltConfig, error) {
	doltConfig := config.DoltConfig{}
	if strings.TrimSpace(cityPath) != "" {
		tomlPath := filepath.Join(cityPath, "city.toml")
		if _, err := os.Stat(tomlPath); err != nil {
			if !os.IsNotExist(err) {
				return doltConfig, fmt.Errorf("stat city dolt config: %w", err)
			}
		} else {
			if cfg, err := loadCityConfig(cityPath, io.Discard); err != nil {
				return doltConfig, fmt.Errorf("load city dolt config: %w", err)
			} else if cfg != nil {
				doltConfig = cfg.Dolt
			}
		}
	}
	if explicitArchiveLevel >= 0 {
		doltConfig.ArchiveLevel = &explicitArchiveLevel
	} else if doltConfig.ArchiveLevel == nil {
		if v := os.Getenv("GC_DOLT_ARCHIVE_LEVEL"); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil {
				doltConfig.ArchiveLevel = &parsed
			}
		}
	}
	if doltConfig.AutoGCEnabled == nil {
		if parsed, ok := parseEnvAutoGCEnabled(os.Getenv("GC_DOLT_AUTO_GC_ENABLED")); ok {
			doltConfig.AutoGCEnabled = &parsed
		}
	}
	if doltConfig.MaxConnections <= 0 {
		doltConfig.MaxConnections = positiveEnvInt("GC_DOLT_MAX_CONNECTIONS")
	}
	if doltConfig.ReadTimeoutMillis <= 0 {
		doltConfig.ReadTimeoutMillis = positiveEnvInt("GC_DOLT_READ_TIMEOUT_MILLIS")
	}
	if doltConfig.WriteTimeoutMillis <= 0 {
		doltConfig.WriteTimeoutMillis = positiveEnvInt("GC_DOLT_WRITE_TIMEOUT_MILLIS")
	}
	return doltConfig, nil
}

// parseEnvAutoGCEnabled parses the GC_DOLT_AUTO_GC_ENABLED override.
// Accepts Go bool spellings (strconv.ParseBool) plus Dolt's ON/OFF.
// Unset or unparseable values report ok=false so callers keep the default.
func parseEnvAutoGCEnabled(raw string) (value, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false, false
	}
	switch strings.ToUpper(raw) {
	case "ON":
		return true, true
	case "OFF":
		return false, true
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return false, false
	}
	return parsed, true
}

func positiveEnvInt(key string) int {
	raw := os.Getenv(key)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return n
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
//
// When cityPath is known, SIGKILL is additionally gated on the dolt exclusive
// store lock being free (gastownhall/gascity#3174): a holder is mid-flush,
// and killing it tears the noms journal. The wait is extended by the
// lock-release window while the lock is held; if the process still holds it
// after that, the function returns an error instead of killing. An empty
// cityPath (test watchdog, recovery cleanup without a city) keeps the legacy
// unconditional SIGKILL.
func terminateManagedDoltPID(cityPath string, pid int) error {
	return terminateManagedDoltPIDGuarded(cityPath, pid, nil)
}

// terminateManagedDoltPIDGuarded is terminateManagedDoltPID with a PID-reuse
// guard. When identityMatches is non-nil it is evaluated immediately before the
// SIGTERM and again immediately before the SIGKILL escalation; a false result
// means the numeric PID no longer belongs to the process we intended to signal
// (it exited, was reaped, and the number was reused), so the pending signal is
// skipped and the decision is logged.
//
// The SIGKILL re-check is the load-bearing one. terminateManagedDoltPID's own
// caller (terminateManagedDoltStartedProcess) checks identity once at entry, but
// the target can exit and its PID be reused during the SIGTERM grace below —
// pidAlive then reports the reused process as alive, the grace runs to its
// deadline, and a bare-PID SIGKILL would land on the stranger. Re-verifying just
// before the kill closes that window. A nil guard keeps the legacy unconditional
// behavior for callers that never snapshotted an identity (test watchdog,
// recovery cleanup, pre-handshake scope watchdog).
func terminateManagedDoltPIDGuarded(cityPath string, pid int, identityMatches func() bool) error {
	if pid <= 0 {
		return nil
	}
	if identityMatches != nil && !identityMatches() {
		managedDoltCleanupLogf("skipping terminate of pid %d: start identity changed (PID reused)", pid)
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
	if dataDir := terminateManagedDoltDataDir(cityPath); dataDir != "" {
		if err := waitManagedDoltSIGKILLLockGate(pid, dataDir, pidAlive, gracePeriod, managedDoltLockReleaseTimeoutFn(cityPath), pollInterval); err != nil {
			return err
		}
		if !pidAlive(pid) {
			return nil
		}
	}
	// Re-verify identity immediately before the forced kill: the target outlived
	// the SIGTERM grace, but if it exited and the PID was reused mid-grace,
	// pidAlive is now reporting the reused process. Skip the SIGKILL on a
	// mismatch so it never lands on an unrelated process.
	if identityMatches != nil && !identityMatches() {
		managedDoltCleanupLogf("skipping SIGKILL of pid %d: start identity changed (PID reused)", pid)
		return nil
	}
	_ = process.Signal(syscall.SIGKILL)
	time.Sleep(250 * time.Millisecond)
	return nil
}

// terminateManagedDoltDataDir resolves the managed data dir for the SIGKILL
// lock gate in terminateManagedDoltPID. Returns "" when no city is known so
// callers without a layout keep the legacy unconditional SIGKILL.
func terminateManagedDoltDataDir(cityPath string) string {
	if strings.TrimSpace(cityPath) == "" {
		return ""
	}
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return ""
	}
	return layout.DataDir
}

func runManagedDoltTestWatchdog(args []string, stdout, stderr *os.File) int {
	if !managedDoltTestModeEnabled() {
		fmt.Fprintln(stderr, "managed dolt test watchdog is only available in managed Dolt test mode") //nolint:errcheck
		return 2
	}
	if len(args) < 4 || len(args) > 6 {
		fmt.Fprintf(stderr, "usage: %s <parent-pid> <config-file> <log-file> <disarm-file> [city-path] [parent-pipe-fd]\n", managedDoltTestWatchdogArg) //nolint:errcheck
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
	cityPath := ""
	parentPipeArg := ""
	if len(args) == 5 {
		if _, parseErr := strconv.Atoi(args[4]); parseErr == nil {
			parentPipeArg = args[4]
		} else {
			cityPath = args[4]
		}
	}
	if len(args) == 6 {
		cityPath = args[4]
		parentPipeArg = args[5]
	}
	var parentDone <-chan struct{}
	if parentPipeArg != "" {
		done, closeParentDone, err := managedDoltTestParentDone(parentPipeArg)
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
	cmd.Env = doltServerEnv(cityPath, os.Environ())
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
func doltServerEnv(cityPath string, parent []string) []string {
	env := removeEnvKey(parent, "DOLT_DISABLE_EVENT_FLUSH")
	if managedDoltDisableEventFlush(cityPath) {
		// Disable Dolt usage telemetry for managed servers by default. The
		// `dolt send-metrics` event-flush reporter spawns transient
		// `dolt send-metrics` processes that were observed burning 80-94% CPU
		// on a busy managed city. Operators can opt back in with
		// `.beads/config.yaml`:
		//   dolt:
		//     disable-event-flush: false
		env = append(env, "DOLT_DISABLE_EVENT_FLUSH=true")
	}
	return env
}

func managedDoltDisableEventFlush(cityPath string) bool {
	if strings.TrimSpace(cityPath) == "" {
		return true
	}
	cfg, _, err := contract.ReadDoltConfig(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "config.yaml"))
	if err != nil {
		return true
	}
	return cfg.DisableEventFlushEnabled()
}
