package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/pidutil"
)

type managedDoltStopReport struct {
	HadPID bool
	PID    int
	Forced bool
}

func stopManagedDoltProcess(cityPath, port string) (managedDoltStopReport, error) {
	return stopManagedDoltProcessWithOptions(cityPath, port, true)
}

// resolveManagedDoltStopTimeout returns the SIGTERM→SIGKILL grace for the
// managed dolt subprocess. It reads `[daemon].dolt_stop_timeout` from city.toml
// when available, falling back to config.DefaultDoltStopTimeout if the config
// cannot be loaded. Independent of `[daemon].shutdown_timeout` so a slow agent
// drain cannot steal dolt's flush window (see gastownhall/gascity#2090).
//
// An empty cityPath returns the default without attempting a config load:
// loadCityConfig("", …) would resolve "city.toml" relative to the current
// working directory, materializing builtin packs under cwd and reading an
// unrelated ./city.toml. Recovery/startup-cleanup callers may pass an empty
// cityPath, so this guard keeps that path from loading a stray config.
func resolveManagedDoltStopTimeout(cityPath string) time.Duration {
	if strings.TrimSpace(cityPath) == "" {
		return config.DefaultDoltStopTimeout
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil || cfg == nil {
		return config.DefaultDoltStopTimeout
	}
	return cfg.Daemon.DoltStopTimeoutDuration()
}

// managedDoltStopPollInterval returns the liveness-poll interval for the
// SIGTERM wait loop. It is normally 500ms, but is shrunk to the grace period
// itself when the configured grace is shorter than one poll — otherwise a
// sub-500ms grace would sleep clean past the deadline before the first check.
// A non-positive grace keeps the 500ms default; the wait loop exits on the
// already-past deadline before it ever sleeps.
func managedDoltStopPollInterval(gracePeriod time.Duration) time.Duration {
	pollInterval := 500 * time.Millisecond
	if gracePeriod > 0 && gracePeriod < pollInterval {
		pollInterval = gracePeriod
	}
	return pollInterval
}

const managedDoltProcessExitPollInterval = 20 * time.Millisecond

// waitForManagedDoltProcessExit observes process liveness for at most timeout.
// It probes immediately, then sleeps in bounded increments so a caller returns
// as soon as the process exits without overshooting the requested maximum.
func waitForManagedDoltProcessExit(pid int, timeout time.Duration, alive func(int) bool) {
	if !alive(pid) || timeout <= 0 {
		return
	}

	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		time.Sleep(min(managedDoltProcessExitPollInterval, remaining))
		if !alive(pid) {
			return
		}
	}
}

func stopManagedDoltProcessWithOptions(cityPath, port string, clearPublishedState bool) (managedDoltStopReport, error) {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return managedDoltStopReport{}, err
	}
	info, err := inspectManagedDoltProcess(cityPath, port)
	if err != nil {
		return managedDoltStopReport{}, err
	}
	report := managedDoltStopReport{}
	targetPID := 0
	switch {
	case info.ManagedPID > 0 && info.ManagedOwned && managedDoltProcessControllable(info.ManagedPID, layout):
		targetPID = info.ManagedPID
	case info.PortHolderPID > 0 && info.PortHolderOwned && managedDoltProcessControllable(info.PortHolderPID, layout):
		targetPID = info.PortHolderPID
	}
	lockWindow := managedDoltLockReleaseTimeoutFn(cityPath)
	if targetPID <= 0 {
		// No controllable server process, but a crashed server's flushing
		// descendant can still hold the store lock. The stop contract says
		// success means the data dir is released — fail closed instead of
		// green-lighting a mid-flush data-dir consumer (backup, move,
		// delete) keyed on stop's success (gastownhall/gascity#3174).
		if err := waitForManagedDoltDataDirLockFree(layout.DataDir, lockWindow); err != nil {
			return report, fmt.Errorf("no controllable dolt process, but the data dir is not yet released: %w", err)
		}
		if err := clearManagedDoltRuntime(layout, port); err != nil {
			return report, err
		}
		if clearPublishedState {
			if err := clearManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
				return report, err
			}
		}
		return report, nil
	}
	report.HadPID = true
	report.PID = targetPID
	if managedStopPIDAlive(targetPID) {
		if err := syscall.Kill(targetPID, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			return report, fmt.Errorf("signal %d with SIGTERM: %w", targetPID, err)
		}
	}
	gracePeriod := resolveManagedDoltStopTimeout(cityPath)
	deadline := time.Now().Add(gracePeriod)
	pollInterval := managedDoltStopPollInterval(gracePeriod)
	for managedStopPIDAlive(targetPID) && time.Now().Before(deadline) {
		time.Sleep(pollInterval)
	}
	if managedStopPIDAlive(targetPID) {
		// The process outlived the SIGTERM grace. SIGKILL is only safe when
		// the dolt exclusive store lock is free — a holder is mid-flush, and
		// killing it tears the noms journal (gastownhall/gascity#3174).
		if err := waitManagedDoltSIGKILLLockGate(targetPID, layout.DataDir, managedStopPIDAlive, gracePeriod, lockWindow, pollInterval); err != nil {
			return report, err
		}
		// Re-verify the PID still belongs to our managed dolt server before the
		// forced kill. It outlived the SIGTERM grace, but if it actually exited
		// during the grace and the numeric PID was reused, managedStopPIDAlive now
		// reports that unrelated process as alive and a bare-PID SIGKILL would hit
		// it. managedDoltProcessControllable re-runs the ownership inspection
		// (cmdline/data-dir/cwd), which a reused unrelated PID fails.
		if managedDoltProcessControllable(targetPID, layout) {
			report.Forced = true
			if err := syscall.Kill(targetPID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				return report, fmt.Errorf("signal %d with SIGKILL: %w", targetPID, err)
			}
			waitForManagedDoltProcessExit(targetPID, time.Second, managedStopPIDAlive)
		} else {
			managedDoltCleanupLogf("skipping SIGKILL of pid %d: no longer an owned managed dolt process (PID reused)", targetPID)
		}
	}
	// Fail only when OUR server is still alive AND still owned. A live-but-unowned
	// PID here means our server exited during the grace and the number was reused:
	// the stop succeeded (our server is gone), and the data-dir lock wait below
	// still guarantees the dir is released before we report success.
	if managedDoltProcessControllable(targetPID, layout) {
		return report, fmt.Errorf("pid %d still alive after forced stop", targetPID)
	}
	// The server process is gone, but descendants (e.g. dolt gc workers) can
	// still hold the store lock while finishing a write. Block until release
	// so a follow-up start cannot bind the data_dir mid-flush.
	if err := waitForManagedDoltDataDirLockFree(layout.DataDir, lockWindow); err != nil {
		return report, fmt.Errorf("dolt process %d exited but the data dir is not yet released: %w", targetPID, err)
	}
	if err := clearManagedDoltRuntime(layout, port); err != nil {
		return report, err
	}
	if clearPublishedState {
		if err := clearManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
			return report, err
		}
	}
	return report, nil
}

func clearManagedDoltRuntime(layout managedDoltRuntimeLayout, portText string) error {
	port := 0
	if state, err := readDoltRuntimeStateFile(layout.StateFile); err == nil {
		port = state.Port
	}
	if port == 0 {
		parsed, err := strconv.Atoi(strings.TrimSpace(portText))
		if err == nil {
			port = parsed
		}
	}
	if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
		Running:   false,
		PID:       0,
		Port:      port,
		DataDir:   layout.DataDir,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return err
	}
	if err := os.Remove(layout.PIDFile); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func managedDoltStopFields(report managedDoltStopReport) []string {
	return []string{
		"had_pid\t" + strconv.FormatBool(report.HadPID),
		"pid\t" + strconv.Itoa(report.PID),
		"forced\t" + strconv.FormatBool(report.Forced),
	}
}

func managedDoltProcessControllable(pid int, layout managedDoltRuntimeLayout) bool {
	if pid <= 0 || !managedStopPIDAlive(pid) {
		return false
	}
	owned, _ := inspectManagedDoltOwnership(pid, layout)
	return owned
}

func managedStopPIDAlive(pid int) bool {
	return pidutil.Alive(pid)
}
