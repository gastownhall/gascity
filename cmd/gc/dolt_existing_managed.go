package main

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type managedDoltExistingReport struct {
	ManagedPID              int
	ManagedOwned            bool
	DeletedInodes           bool
	StatePort               int
	Ready                   bool
	Reusable                bool
	PortHolderPID           int
	PortHolderOwned         bool
	PortHolderDeletedInodes bool
}

func assessExistingManagedDolt(cityPath, host, port, user string, timeout time.Duration) (managedDoltExistingReport, error) {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return managedDoltExistingReport{}, err
	}
	info, err := inspectManagedDoltProcess(cityPath, port)
	if err != nil {
		return managedDoltExistingReport{}, err
	}
	report := managedDoltExistingReport{
		ManagedPID:              info.ManagedPID,
		ManagedOwned:            info.ManagedOwned,
		DeletedInodes:           info.ManagedDeletedInodes,
		PortHolderPID:           info.PortHolderPID,
		PortHolderOwned:         info.PortHolderOwned,
		PortHolderDeletedInodes: info.PortHolderDeletedInodes,
	}
	report.StatePort = managedDoltExistingStatePort(cityPath, layout, report.ManagedPID)
	if report.ManagedPID <= 0 || !report.ManagedOwned || report.StatePort <= 0 || report.DeletedInodes || timeout <= 0 {
		return report, nil
	}
	readyReport, err := waitForManagedDoltReady(cityPath, host, strconv.Itoa(report.StatePort), user, report.ManagedPID, timeout, true)
	report.Ready = readyReport.Ready
	if readyReport.DeletedInodes || processHasDeletedDataInodesWithin(report.ManagedPID, layout.DataDir, 300*time.Millisecond) {
		report.DeletedInodes = true
	}
	if err == nil && report.Ready && !report.DeletedInodes {
		report.Reusable = true
	}
	return report, nil
}

func managedDoltExistingStatePort(cityPath string, layout managedDoltRuntimeLayout, managedPID int) int {
	if managedPID <= 0 {
		return 0
	}
	for _, path := range []string{layout.StateFile, managedDoltStatePath(cityPath)} {
		state, err := readDoltRuntimeStateFile(path)
		if err != nil {
			continue
		}
		if !managedDoltExistingStateMatches(state, cityPath, managedPID) {
			continue
		}
		return state.Port
	}
	return 0
}

func managedDoltExistingStateMatches(state doltRuntimeState, cityPath string, managedPID int) bool {
	if managedPID <= 0 || state.PID != managedPID || !state.Running || state.Port <= 0 {
		return false
	}
	expectedDataDir := filepath.Join(cityPath, ".beads", "dolt")
	if !samePath(strings.TrimSpace(state.DataDir), expectedDataDir) {
		return false
	}
	return pidAlive(state.PID)
}

func managedDoltExistingFields(report managedDoltExistingReport) []string {
	return []string{
		"managed_pid\t" + strconv.Itoa(report.ManagedPID),
		"managed_owned\t" + strconv.FormatBool(report.ManagedOwned),
		"deleted_inodes\t" + strconv.FormatBool(report.DeletedInodes),
		"state_port\t" + strconv.Itoa(report.StatePort),
		"ready\t" + strconv.FormatBool(report.Ready),
		"reusable\t" + strconv.FormatBool(report.Reusable),
		"port_holder_pid\t" + strconv.Itoa(report.PortHolderPID),
		"port_holder_owned\t" + strconv.FormatBool(report.PortHolderOwned),
		"port_holder_deleted_inodes\t" + strconv.FormatBool(report.PortHolderDeletedInodes),
	}
}
