package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type managedDoltStopReport struct {
	HadPID bool
	PID    int
	Forced bool
}

func stopManagedDoltProcess(cityPath, port string) (managedDoltStopReport, error) {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return managedDoltStopReport{}, err
	}
	info, err := inspectManagedDoltProcess(cityPath, port)
	if err != nil {
		return managedDoltStopReport{}, err
	}
	report := managedDoltStopReport{}
	if info.ManagedPID <= 0 {
		if err := clearManagedDoltRuntime(layout, port); err != nil {
			return report, err
		}
		if err := clearManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
			return report, err
		}
		return report, nil
	}
	report.HadPID = true
	report.PID = info.ManagedPID
	if managedStopPIDAlive(info.ManagedPID) {
		if err := syscall.Kill(info.ManagedPID, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			return report, fmt.Errorf("signal %d with SIGTERM: %w", info.ManagedPID, err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for managedStopPIDAlive(info.ManagedPID) && time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
	}
	if managedStopPIDAlive(info.ManagedPID) {
		report.Forced = true
		if err := syscall.Kill(info.ManagedPID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return report, fmt.Errorf("signal %d with SIGKILL: %w", info.ManagedPID, err)
		}
		time.Sleep(time.Second)
	}
	if managedStopPIDAlive(info.ManagedPID) {
		return report, fmt.Errorf("pid %d still alive after forced stop", info.ManagedPID)
	}
	if err := clearManagedDoltRuntime(layout, port); err != nil {
		return report, err
	}
	if err := clearManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
		return report, err
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

func managedStopPIDAlive(pid int) bool {
	if !pidAlive(pid) {
		return false
	}
	statPath := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		return true
	}
	fields := strings.Fields(string(data))
	if len(fields) >= 3 && fields[2] == "Z" {
		return false
	}
	return true
}
