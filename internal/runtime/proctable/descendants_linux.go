//go:build linux

package proctable

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// snapshotProcessTree walks /proc once for a host-wide process snapshot. PPID,
// PGID, session, and start time come from the same /proc/<pid>/stat read, so
// teardown selection never has to stitch identity together with per-node live
// probes. Reading stat never touches the process's memory, unlike its environ
// or cmdline. There is no liveScanGuard (that guard protects the orphan sweep
// in ScanBySessionID, not this read-only snapshot) and no root filtering.
func snapshotProcessTree() ([]ProcessRecord, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var records []ProcessRecord
	for _, e := range entries {
		if rec, ok := procStatRecord(e.Name()); ok {
			records = append(records, rec)
		}
	}
	return records, nil
}

// snapshotProcesses is snapshotProcessTree with each process's comm and
// GC_SESSION_ID read right after its stat. A process whose comm cannot be
// read is left out.
func snapshotProcesses() ([]ProcessRecord, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var records []ProcessRecord
	for _, e := range entries {
		rec, ok := procStatRecord(e.Name())
		if !ok {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		rec.Name = strings.TrimSpace(string(comm))
		if env, err := parseEnvironFile(filepath.Join("/proc", e.Name(), "environ")); err == nil {
			rec.SessionID = env["GC_SESSION_ID"]
		}
		records = append(records, rec)
	}
	return records, nil
}

// procStatRecord reads the record of the /proc entry named entry from its stat
// alone. ok is false for an entry that is not a process, or a process whose
// stat is gone or unreadable.
func procStatRecord(entry string) (ProcessRecord, bool) {
	pid, err := strconv.Atoi(entry)
	if err != nil {
		return ProcessRecord{}, false
	}
	stat, ok, err := readProcStat(filepath.Join("/proc", entry, "stat"))
	if err != nil || !ok {
		return ProcessRecord{}, false
	}
	return ProcessRecord{
		PID:           pid,
		PPID:          stat.ppid,
		PGID:          stat.pgid,
		StartTime:     stat.startTime,
		SessionLeader: stat.sid == pid,
	}, true
}
