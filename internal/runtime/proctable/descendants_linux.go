//go:build linux

package proctable

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// snapshotProcesses walks /proc for a host-wide pid/ppid/comm table, plus
// each process's GC_SESSION_ID (from /proc/<pid>/environ) captured in the
// same walk — no liveScanGuard (that guard protects the orphan sweep in
// ScanBySessionID, not this read-only liveness snapshot) and no root
// filtering: every process gets its raw SessionID, if any.
func snapshotProcesses() ([]ProcessRecord, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var records []ProcessRecord
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		ppid, ok, err := readParentPID(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil || !ok {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		rec := ProcessRecord{PID: pid, PPID: ppid, Name: strings.TrimSpace(string(comm))}
		if env, err := parseEnvironFile(filepath.Join("/proc", e.Name(), "environ")); err == nil {
			rec.SessionID = env["GC_SESSION_ID"]
		}
		records = append(records, rec)
	}
	return records, nil
}
