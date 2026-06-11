package main

// Scope-death watchdog for production managed dolt sql-servers (ga-gz19s4).
//
// gc spawns one managed `dolt sql-server` per scope (city, worktree, PR
// clone), but nothing owned the server once its scope was deleted: the
// server orphaned to ppid 1 and ran forever. On one dev box 314 servers
// accumulated this way — 230 with deleted working directories — holding
// ~44GB RSS and a sustained ~4.5 cores in aggregate.
//
// The watchdog closes the ownership gap at the source instead of relying on
// after-the-fact reaping: production servers are spawned under a supervisor
// process (a gc re-exec, modeled on the managed-dolt TEST watchdog in
// dolt_start_managed.go) that terminates the server when its scope is
// provably gone. "Provably gone" means the server's --config file no longer
// exists on disk for two consecutive checks — the second check is the grace
// window that protects transient states (crash-adoption moves, mid-flight
// renames) from being misread as deletion. The check queries live
// filesystem state every cycle; there are no status files.
//
// Memory cost: a gc re-exec initializes the full cmd/gc dependency graph
// and holds it for the life of the scope — measured at ~97MB RSS per
// watchdog, of which ~20MB is private dirty (the rest is binary text shared
// across all gc processes). The marginal cost is therefore ~20MB of
// unshareable memory per live scope. This is a deliberate trade against the
// multi-GB orphan leak above; it scales only with live scopes because each
// watchdog dies with its server.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// managedDoltScopeWatchdogArg is the argv[1] re-exec marker for the
	// production scope watchdog. No production `gc` invocation collides with
	// it; reaching init() with it set is proof of an intentional re-exec.
	managedDoltScopeWatchdogArg = "__gc-managed-dolt-scope-watchdog"

	// managedDoltScopeWatchdogEnv disables the production scope watchdog
	// when set to "0" (the managed server is then spawned directly, exactly
	// the pre-watchdog behavior).
	managedDoltScopeWatchdogEnv = "GC_DOLT_SCOPE_WATCHDOG"

	// managedDoltScopeWatchdogIntervalEnv overrides the scope poll interval
	// in milliseconds. Tests use it to shrink the deleted-scope reaction
	// time from tens of seconds to tens of milliseconds.
	managedDoltScopeWatchdogIntervalEnv = "GC_DOLT_SCOPE_WATCHDOG_INTERVAL_MS"

	// managedDoltScopeWatchdogDefaultInterval is the production poll
	// cadence. Scope deletion is rare and the response does not need to be
	// fast — it needs to be reliable. A slow cadence keeps the watchdog's
	// steady-state CPU cost negligible; the per-watchdog memory footprint
	// is the re-exec cost documented in the file header.
	managedDoltScopeWatchdogDefaultInterval = 30 * time.Second

	// managedDoltScopeGoneConfirmations is how many consecutive polls must
	// observe the scope missing before the server is terminated. Two checks
	// one full interval apart distinguish "scope permanently deleted" from
	// "scope momentarily absent" (crash-adoption window, transient rename).
	managedDoltScopeGoneConfirmations = 2
)

func init() {
	if len(os.Args) < 2 || os.Args[1] != managedDoltScopeWatchdogArg {
		return
	}
	os.Exit(runManagedDoltScopeWatchdog(os.Args[2:], os.Stdout, os.Stderr))
}

// managedDoltScopeWatchdogEnabled reports whether production managed dolt
// servers are spawned under the scope watchdog. Default on; opt out with
// GC_DOLT_SCOPE_WATCHDOG=0. Always off in managed-dolt test mode: test
// scopes are owned by the test watchdog (or deliberately watchdog-free),
// and interposing a second supervisor would change test process topology.
func managedDoltScopeWatchdogEnabled() bool {
	return managedDoltScopeWatchdogEnabledFor(managedDoltTestModeEnabled(), os.Getenv(managedDoltScopeWatchdogEnv))
}

// managedDoltScopeWatchdogEnabledFor is the pure decision behind
// managedDoltScopeWatchdogEnabled, split out for tests (the test binary is
// always in test mode, so the production default is unreachable in-process).
func managedDoltScopeWatchdogEnabledFor(testMode bool, envValue string) bool {
	if testMode {
		return false
	}
	return strings.TrimSpace(envValue) != "0"
}

// managedDoltScopeWatchdogInterval resolves the poll interval, honoring the
// millisecond test override when it parses to a positive value.
func managedDoltScopeWatchdogInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv(managedDoltScopeWatchdogIntervalEnv))
	if raw == "" {
		return managedDoltScopeWatchdogDefaultInterval
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return managedDoltScopeWatchdogDefaultInterval
	}
	return time.Duration(ms) * time.Millisecond
}

// managedDoltScopeGone reports whether the scope anchoring a managed dolt
// server has been deleted, using the server's --config file as the anchor:
// the config lives inside the scope's runtime directory, so the scope
// disappearing takes the config with it. Only a definitive not-exist counts;
// stat errors (permissions, I/O) lean toward "alive" so a degraded
// filesystem never kills a healthy server.
func managedDoltScopeGone(configFile string) bool {
	if strings.TrimSpace(configFile) == "" {
		return false
	}
	_, err := os.Stat(configFile)
	return errors.Is(err, fs.ErrNotExist)
}

// startManagedDoltSQLServerWithScopeWatchdog spawns the managed dolt
// sql-server under the production scope watchdog. The watchdog process
// (a gc re-exec) starts the server itself and reports the server PID on
// stdout using the same one-line protocol as the test watchdog, so callers
// observe the same managedDoltStartedProcess shape as a direct spawn plus
// the supervising WatchdogPID.
func startManagedDoltSQLServerWithScopeWatchdog(cityPath, configFile, logFilePath string, logFile *os.File) (managedDoltStartedProcess, error) {
	watchdogExecutable, err := managedDoltWatchdogExecutable()
	if err != nil {
		return managedDoltStartedProcess{}, err
	}
	cmd := exec.Command(watchdogExecutable, managedDoltScopeWatchdogArg, configFile, logFilePath, cityPath)
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = managedDoltSQLServerSysProcAttr()
	cmd.Env = doltServerEnv(cityPath, os.Environ())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return managedDoltStartedProcess{}, fmt.Errorf("prepare dolt scope watchdog: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return managedDoltStartedProcess{}, fmt.Errorf("start dolt scope watchdog: %w", err)
	}
	pid, err := readManagedDoltTestWatchdogPID(stdout, cmd.Process.Pid)
	if err != nil {
		_ = terminateManagedDoltPID(cityPath, cmd.Process.Pid)
		_ = cmd.Wait()
		return managedDoltStartedProcess{}, err
	}
	go func() { _ = cmd.Wait() }()
	return managedDoltStartedProcess{
		CityPath:    cityPath,
		PID:         pid,
		WatchdogPID: cmd.Process.Pid,
	}, nil
}

// runManagedDoltScopeWatchdog is the re-exec'd watchdog process body. It
// spawns the dolt sql-server as its own process-group leader, prints the
// server PID on stdout, then supervises: it terminates the server when the
// scope is gone for managedDoltScopeGoneConfirmations consecutive polls,
// forwards SIGTERM/SIGINT, and exits when the server exits on its own.
func runManagedDoltScopeWatchdog(args []string, stdout, stderr *os.File) int {
	if len(args) != 3 {
		fmt.Fprintf(stderr, "usage: %s <config-file> <log-file> <city-path>\n", managedDoltScopeWatchdogArg) //nolint:errcheck
		return 2
	}
	configFile := args[0]
	logFilePath := args[1]
	cityPath := args[2]
	if strings.TrimSpace(configFile) == "" {
		fmt.Fprintln(stderr, "empty config file path") //nolint:errcheck
		return 2
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
	// Setpgid: the dolt sql-server leads its own process group, matching
	// the direct production spawn (managedDoltSQLServerSysProcAttr) and the
	// test watchdog's layout, and keeping the server's descendants out of
	// the watchdog's own group. Termination here is leader-only:
	// terminateManagedDoltPID signals just this PID — group-kill
	// (kill(-pgid, ...)) exists only in terminateManagedDoltTestPID, the
	// test-registry reaper. Leader-only is accepted on this path because
	// the managed config disables auto_gc/stats helper workers (see
	// cmd_dolt_config.go), so descendant helpers are rare by construction,
	// and a SIGTERM'd dolt winds down its own children; only the SIGKILL
	// escalation of an unresponsive server could strand descendants.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = doltServerEnv(cityPath, os.Environ())
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "start dolt sql-server: %v\n", err) //nolint:errcheck
		return 1
	}
	fmt.Fprintf(stdout, "%d\n", cmd.Process.Pid) //nolint:errcheck

	interval := managedDoltScopeWatchdogInterval()
	fmt.Fprintf(logFile, "gc scope watchdog: supervising dolt sql-server pid %d (config %s, poll interval %s)\n", //nolint:errcheck
		cmd.Process.Pid, configFile, interval)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	goneStreak := 0
	for {
		select {
		case sig := <-signals:
			fmt.Fprintf(logFile, "gc scope watchdog: received %v; terminating dolt sql-server pid %d\n", sig, cmd.Process.Pid) //nolint:errcheck
			_ = terminateManagedDoltPID(cityPath, cmd.Process.Pid)
			<-done
			return 0
		case <-ticker.C:
			if !managedDoltScopeGone(configFile) {
				goneStreak = 0
				continue
			}
			goneStreak++
			if goneStreak < managedDoltScopeGoneConfirmations {
				continue
			}
			fmt.Fprintf(logFile, "gc scope watchdog: config %s gone for %d consecutive checks; terminating dolt sql-server pid %d\n", //nolint:errcheck
				configFile, goneStreak, cmd.Process.Pid)
			_ = terminateManagedDoltPID(cityPath, cmd.Process.Pid)
			<-done
			return 0
		case err := <-done:
			if err != nil {
				fmt.Fprintf(logFile, "gc scope watchdog: dolt sql-server pid %d exited with error: %v\n", cmd.Process.Pid, err) //nolint:errcheck
				return 1
			}
			fmt.Fprintf(logFile, "gc scope watchdog: dolt sql-server pid %d exited cleanly\n", cmd.Process.Pid) //nolint:errcheck
			return 0
		}
	}
}
