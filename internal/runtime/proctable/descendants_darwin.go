//go:build darwin

package proctable

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// snapshotProcesses shells out to `ps` for a host-wide pid/ppid/comm table,
// plus (via the eww flag) each process's inline environment so GC_SESSION_ID
// can be captured in the same read — no second ps invocation, no
// liveScanGuard (that guard protects the orphan sweep in ScanBySessionID, not
// this read-only liveness snapshot).
func snapshotProcesses() ([]ProcessRecord, error) {
	out, err := exec.Command("ps", "eww", "-ax", "-o", "pid=,ppid=,comm=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("running ps: %w", err)
	}
	var records []ProcessRecord
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		rec := ProcessRecord{PID: pid, PPID: ppid, Name: filepath.Base(fields[2])}
		if len(fields) > 3 {
			rec.SessionID = parseInlineEnv(fields[3:])["GC_SESSION_ID"]
		}
		records = append(records, rec)
	}
	return records, nil
}
