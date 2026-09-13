//go:build darwin

package proctable

import (
	"fmt"
	"strconv"

	"golang.org/x/sys/unix"
)

// snapshotProcesses reads every destructive field from one kern.proc.all
// result. The later ps read only enriches liveness metadata; it never splices a
// new PID identity onto parent/group edges captured for teardown.
func snapshotProcesses() ([]ProcessRecord, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("snapshotting process table with kern.proc.all: %w", err)
	}
	metadata, err := psRecords()
	if err != nil {
		return nil, fmt.Errorf("enriching process snapshot: %w", err)
	}

	records := make([]ProcessRecord, 0, len(processes))
	for _, process := range processes {
		record, ok := darwinProcessRecord(process)
		if !ok {
			continue
		}
		if enrichment, ok := metadata[record.PID]; ok {
			record.SessionID = enrichment.env["GC_SESSION_ID"]
		}
		records = append(records, record)
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
		PID:       pid,
		PPID:      ppid,
		PGID:      pgid,
		StartTime: startTime,
		Name:      unix.ByteSliceToString(process.Proc.P_comm[:]),
	}, true
}

func darwinStartIdentity(start unix.Timeval) (string, error) {
	if start.Sec <= 0 || start.Usec < 0 || start.Usec >= 1_000_000 {
		return "", fmt.Errorf("invalid process start time")
	}
	return strconv.FormatInt(unix.TimevalToNsec(start), 10), nil
}
