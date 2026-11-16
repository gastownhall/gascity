package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

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

// TestScanErrorLogPacesUnreadableEntries (mc-ygved): the orphan sweep logs a
// scan of unreadable entries once per shape, again after
// processScanErrorLogRepeat or when the shape changes, per city; a failure
// of the whole scan is logged every time, as it bars rule 2.
func TestScanErrorLogPacesUnreadableEntries(t *testing.T) {
	l := &scanErrorLog{last: map[string]scanErrorLogged{}}
	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	perm := entryScanError("reading environ for pid PID: permission denied", 11, 12)
	steps := []struct {
		name string
		city string
		err  error
		at   time.Time
		want bool
	}{
		{"first", "/c", perm, t0, true},
		{"same shape, other pids and count", "/c", entryScanError("reading environ for pid PID: permission denied", 13, 14, 15), t0.Add(time.Minute), false},
		{"another city", "/d", perm, t0.Add(time.Minute), true},
		{"a new kind", "/c", entryScanError("reading owner for pid PID: no such device", 16), t0.Add(2 * time.Minute), true},
		{"the new kind again", "/c", entryScanError("reading owner for pid PID: no such device", 17), t0.Add(3 * time.Minute), false},
		{"after the repeat window", "/c", entryScanError("reading owner for pid PID: no such device", 18), t0.Add(2*time.Minute + processScanErrorLogRepeat), true},
		{"a whole-scan failure", "/c", errors.New("enumerating /proc: permission denied"), t0.Add(13 * time.Minute), true},
		{"the whole-scan failure again", "/c", errors.New("enumerating /proc: permission denied"), t0.Add(13*time.Minute + time.Second), true},
	}
	for _, s := range steps {
		if got := l.report(s.city, s.err, s.at); got != s.want {
			t.Fatalf("%s: report = %t, want %t", s.name, got, s.want)
		}
	}
}

// TestOrphanSweepLogsUnreadableEntriesOnce (mc-ygved, legacy and v2's
// shared tick phase): two sweeps whose scans hit the same unreadable
// entries log the scan error once.
func TestOrphanSweepLogsUnreadableEntriesOnce(t *testing.T) {
	saved := processScanErrorLogs
	processScanErrorLogs = &scanErrorLog{last: map[string]scanErrorLogged{}}
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
