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
	if targetPID <= 0 {
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
		report.Forced = true
		if err := syscall.Kill(targetPID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return report, fmt.Errorf("signal %d with SIGKILL: %w", targetPID, err)
		}
		time.Sleep(time.Second)
	}
	if managedStopPIDAlive(targetPID) {
		return report, fmt.Errorf("pid %d still alive after forced stop", targetPID)
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
