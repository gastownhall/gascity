//go:build darwin

package proctable

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

const processSnapshotTimeout = 10 * time.Second

// ScanBySessionID returns live agent root processes whose environment carries
// GC_SESSION_ID equal to id. Empty id is the orphan sweep's scan: the roots
// with any GC_SESSION_ID reparented to launchd ([OrphanCandidate]), as on
// Linux. Darwin reads one ps snapshot either way.
func ScanBySessionID(id string) ([]runtime.LiveRuntime, error) {
	if err := liveScanGuard(); err != nil {
		return []runtime.LiveRuntime{}, err
	}
	records, err := psRecords()
	if err != nil {
		return []runtime.LiveRuntime{}, err
	}
	roots := scanRecordsBySessionID(records, id)
	if id == "" {
		roots = orphanCandidates(roots, 0)
	}
	return withStartIdentities(roots), nil
}

// withStartIdentities fills each root's StartIdentity and StartedAt from the
// kernel's start time (ProcessIdentity: nanoseconds since the epoch). A root
// whose identity cannot be read keeps both empty.
func withStartIdentities(roots []runtime.LiveRuntime) []runtime.LiveRuntime {
	for i := range roots {
		identity, err := ProcessIdentity(roots[i].PID)
		if err != nil {
			continue
		}
		roots[i].StartIdentity = identity
		if ns, err := strconv.ParseInt(identity, 10, 64); err == nil {
			roots[i].StartedAt = time.Unix(0, ns).UTC()
		}
	}
	return roots
}

// ScanBySessionIDSince returns the Darwin session-ID scan. Darwin collects one
// complete ps snapshot, so it has no per-process inspection failures to bound.
func ScanBySessionIDSince(id string, _ time.Time) ([]runtime.LiveRuntime, error) {
	return ScanBySessionID(id)
}

// IsScanRoot reports whether pid should be treated as an agent root. A root
// carries a GC_SESSION_ID, is neither infrastructure nor the program-status
// relay — a tmux server or client is never a root, whoever its parent is,
// and the relay is looked through — and sits outside its parent's envelope:
// the parent is gone, carries a different GC_SESSION_ID, or is
// infrastructure or the relay.
func IsScanRoot(pid int) bool {
	if err := liveScanGuard(); err != nil {
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
	return isRecordScanRoot(records, record)
}

func psRecords() (map[int]psRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), processSnapshotTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "eww", "-ax", "-o", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("running ps: %w", err)
	}
	records := make(map[int]psRecord)
	for _, line := range strings.Split(string(out), "\n") {
		if record, ok := parsePSRecord(line); ok {
			records[record.pid] = record
		}
	}
	return records, nil
}
