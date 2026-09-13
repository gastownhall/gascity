package main

import (
	"strconv"
	"time"
)

type managedDoltProbeReport struct {
	Running                 bool
	PortHolderPID           int
	PortHolderOwned         bool
	PortHolderDeletedInodes bool
	TCPReachable            bool
}

func probeManagedDolt(cityPath, host, port string) (managedDoltProbeReport, error) {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return managedDoltProbeReport{}, err
	}
	info, err := inspectManagedDoltProcess(cityPath, port)
	if err != nil {
		return managedDoltProbeReport{}, err
	}
	report := managedDoltProbeReport{
		PortHolderPID:           info.PortHolderPID,
		PortHolderOwned:         info.PortHolderOwned,
		PortHolderDeletedInodes: info.PortHolderDeletedInodes,
		TCPReachable:            managedDoltTCPReachable(host, port),
	}
	if report.PortHolderPID > 0 && report.PortHolderOwned && !report.PortHolderDeletedInodes {
		report.PortHolderDeletedInodes = processHasDeletedDataInodesWithin(report.PortHolderPID, layout.DataDir, 300*time.Millisecond)
	}
	report.Running = report.PortHolderPID > 0 && report.PortHolderOwned && !report.PortHolderDeletedInodes && report.TCPReachable
	return report, nil
}

func managedDoltProbeFields(report managedDoltProbeReport) []string {
	return []string{
		"running\t" + strconv.FormatBool(report.Running),
		"port_holder_pid\t" + strconv.Itoa(report.PortHolderPID),
		"port_holder_owned\t" + strconv.FormatBool(report.PortHolderOwned),
		"port_holder_deleted_inodes\t" + strconv.FormatBool(report.PortHolderDeletedInodes),
		"tcp_reachable\t" + strconv.FormatBool(report.TCPReachable),
	}
}
