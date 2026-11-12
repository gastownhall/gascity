package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// entryScanError is a scan error of unreadable entries at pids.
func entryScanError(msg string, pids ...int) error {
	var err error
	for _, pid := range pids {
		err = errors.Join(err, &proctable.EntryError{PID: pid, Err: errors.New(strings.ReplaceAll(msg, "PID", strings.Repeat("9", pid%5+1)))})
	}
	return err
}

// TestOrphanSweepLogsUnreadableEntriesOnce (mc-ygved, legacy and v2's
// shared tick phase): two sweeps whose scans hit the same unreadable
// entries log the scan error once.
func TestOrphanSweepLogsUnreadableEntriesOnce(t *testing.T) {
	saved := processScanErrorLogs
	processScanErrorLogs = &proctable.ScanErrorLog{}
	t.Cleanup(func() { processScanErrorLogs = saved })
	sp := newProcessTableSweepProvider()
	sp.findErr = entryScanError("reading environ for pid PID: permission denied", 21, 22)
	var stderr bytes.Buffer
	city := t.TempDir()
	for range 2 {
		sweepProcessTableOrphans(sp, newSessionBeadSnapshot(nil), nil, beads.NewMemStore(), city, &stderr)
	}
	if n := strings.Count(stderr.String(), "scanning process table for orphaned runtimes"); n != 1 {
		t.Fatalf("logged %d times, want once; stderr=%q", n, stderr.String())
	}
}
