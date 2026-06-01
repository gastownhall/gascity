//go:build darwin

package proctable

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ScanBySessionID returns live agent root processes whose environment carries
// GC_SESSION_ID equal to id. Empty id returns all roots with any GC_SESSION_ID.
func ScanBySessionID(id string) ([]runtime.LiveRuntime, error) {
	if _, err := liveScanRoot(); err != nil {
		return []runtime.LiveRuntime{}, err
	}
	records, err := psRecords()
	if err != nil {
		return []runtime.LiveRuntime{}, err
	}
	var out []runtime.LiveRuntime
	for _, record := range records {
		if record.pid <= 1 {
			continue
		}
		sessionID := record.env["GC_SESSION_ID"]
		if sessionID == "" {
			continue
		}
		if id != "" && sessionID != id {
			continue
		}
		if parent, ok := records[record.ppid]; ok && parent.env["GC_SESSION_ID"] == sessionID && !isInfrastructureCommand(parent.command) {
			continue
		}
		epoch, _ := strconv.Atoi(record.env["GC_RUNTIME_EPOCH"])
		out = append(out, runtime.LiveRuntime{
			SessionID: sessionID,
			Epoch:     epoch,
			PID:       record.pid,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].PID < out[j].PID
	})
	if out == nil {
		out = []runtime.LiveRuntime{}
	}
	return out, nil
}

// IsScanRoot reports whether pid is outside its GC_SESSION_ID parent's
// envelope and should be treated as an agent root.
func IsScanRoot(pid int) bool {
	if _, err := liveScanRoot(); err != nil {
		return false
	}
	if pid == 1 {
		return true
	}
	if pid <= 0 {
		return false
	}
	if pid == os.Getpid() {
		return false
	}
	records, err := psRecords()
	if err != nil {
		return false
	}
	record, ok := records[pid]
	if !ok {
		return false
	}
	sessionID := record.env["GC_SESSION_ID"]
	if sessionID == "" {
		return false
	}
	parent, ok := records[record.ppid]
	return !ok || parent.env["GC_SESSION_ID"] != sessionID || isInfrastructureCommand(parent.command)
}

type psRecord struct {
	pid     int
	ppid    int
	command string
	env     map[string]string
}

func psRecords() (map[int]psRecord, error) {
	out, err := exec.Command("ps", "eww", "-ax", "-o", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("running ps: %w", err)
	}
	records := make(map[int]psRecord)
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
		records[pid] = psRecord{
			pid:     pid,
			ppid:    ppid,
			command: darwinPSCommand(fields),
			env:     parseInlineEnv(fields[2:]),
		}
	}
	return records, nil
}

func parseInlineEnv(fields []string) map[string]string {
	env := make(map[string]string)
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok || key == "" {
			continue
		}
		env[key] = value
	}
	return env
}
