//go:build darwin

package proctable

import (
	"fmt"
	"strconv"

	"golang.org/x/sys/unix"
)

// eprocSessionLeader is EPROC_SLEADER from <sys/sysctl.h>: the kinfo_proc
// e_flag bit the kernel sets on a session's leader.
const eprocSessionLeader = 0x2

// snapshotProcessTree reads every destructive field from one kern.proc.all
// result and nothing else. It leaves Name empty, as Linux does, so no kill
// path can come to depend on a field only darwin's tree snapshot would fill.
func snapshotProcessTree() ([]ProcessRecord, error) {
	records, err := kinfoProcessRecords()
	if err != nil {
		return nil, err
	}
	for i := range records {
		records[i].Name = ""
	}
	return records, nil
}

// snapshotProcesses is snapshotProcessTree with each process's command name,
// from the same kern.proc.all result, enriched by a ps read. The ps read only
// adds liveness metadata; it never splices a new PID identity onto
// parent/group edges captured for teardown.
func snapshotProcesses() ([]ProcessRecord, error) {
	records, err := kinfoProcessRecords()
	if err != nil {
		return nil, err
	}
	metadata, err := psRecords()
	if err != nil {
		return nil, fmt.Errorf("enriching process snapshot: %w", err)
	}
	for i := range records {
		if enrichment, ok := metadata[records[i].PID]; ok {
			records[i].SessionID = enrichment.env["GC_SESSION_ID"]
		}
	}
	return records, nil
}

// kinfoProcessRecords reads one kern.proc.all result, command names included.
func kinfoProcessRecords() ([]ProcessRecord, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("snapshotting process table with kern.proc.all: %w", err)
	}
	records := make([]ProcessRecord, 0, len(processes))
	for _, process := range processes {
		if record, ok := darwinProcessRecord(process); ok {
			records = append(records, record)
		}
	}
	return records, nil
}

func darwinProcessRecord(process unix.KinfoProc) (ProcessRecord, bool) {
	pid := int(process.Proc.P_pid)
	ppid := int(process.Eproc.Ppid)
	pgid := int(process.Eproc.Pgid)
	startTime, err := darwinStartIdentity(process.Proc.P_starttime)
	if pid <= 0 || ppid < 0 || pgid < 0 || err != nil {
		return ProcessRecord{}, false
	}
	return ProcessRecord{
		PID:           pid,
		PPID:          ppid,
		PGID:          pgid,
		StartTime:     startTime,
		SessionLeader: process.Eproc.Flag&eprocSessionLeader != 0,
		Name:          unix.ByteSliceToString(process.Proc.P_comm[:]),
	}, true
}

func darwinStartIdentity(start unix.Timeval) (string, error) {
	if start.Sec <= 0 || start.Usec < 0 || start.Usec >= 1_000_000 {
		return "", fmt.Errorf("invalid process start time")
	}
	return strconv.FormatInt(unix.TimevalToNsec(start), 10), nil
}
