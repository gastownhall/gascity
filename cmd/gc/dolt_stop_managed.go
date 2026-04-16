package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

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
	deadline := time.Now().Add(5 * time.Second)
	for managedStopPIDAlive(targetPID) && time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
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
