package main

import (
	"strconv"
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
	if state, err := readDoltRuntimeStateFile(layout.StateFile); err == nil {
		report.StatePort = state.Port
	}
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
