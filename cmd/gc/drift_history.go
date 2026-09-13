package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/supervisor"
)

// driftRestartHistoryFile is the on-disk JSON file holding the
// timestamps of past supervisor auto-restart attempts. It lives under
// supervisor.DefaultHome() so it co-locates with other supervisor
// runtime state and inherits its uid/permission regime. Persistence is
// required so the 3-in-60s loop-guard threshold survives the
// per-invocation lifecycle of `gc start`; an in-memory guard would
// reset every cycle and never refuse a runaway loop.
const driftRestartHistoryFile = "drift-restart-history.json"

// driftRestartHistoryPath returns the absolute path of the on-disk
// restart-attempt log. Resolved at call time so changes to GC_HOME
// (notably during integration tests) are honored.
func driftRestartHistoryPath() string {
	return filepath.Join(supervisor.DefaultHome(), driftRestartHistoryFile)
}

// loadDriftRestartHistory reads the persisted restart-attempt
// timestamps from path. A missing file is treated as an empty
// history, not an error: fresh installs and freshly-isolated test
// homes start with no attempts on record. Read errors of any other
// shape (corrupt JSON, unexpected schema) also collapse to "empty"
// rather than block startup — the loop guard exists to refuse the
// truly pathological case (4 restarts in 60s), and we'd rather
// allow one extra restart than refuse one because of a torn file.
func loadDriftRestartHistory(path string) []time.Time {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var stamps []int64
	if err := json.Unmarshal(data, &stamps); err != nil {
		return nil
	}
	out := make([]time.Time, 0, len(stamps))
	for _, s := range stamps {
		out = append(out, time.Unix(0, s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// saveDriftRestartHistory atomically writes the supplied attempt
// timestamps to path. Atomicity (temp file + rename) means concurrent
// `gc start` invocations either see the pre-write or post-write state,
// never a partial file. Errors are returned but callers may ignore
// them: a transient write failure should not block the restart, and
// the next successful write will replace the file wholesale.
func saveDriftRestartHistory(path string, attempts []time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	stamps := make([]int64, 0, len(attempts))
	for _, t := range attempts {
		stamps = append(stamps, t.UnixNano())
	}
	data, err := json.Marshal(stamps)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}

// pruneRestartHistory drops entries older than (now - window) from the
// supplied attempts slice. The returned slice is freshly allocated so
// callers can safely persist it without aliasing.
func pruneRestartHistory(attempts []time.Time, now time.Time, window time.Duration) []time.Time {
	cutoff := now.Add(-window)
	out := make([]time.Time, 0, len(attempts))
	for _, t := range attempts {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}

// recordDriftRestartAttempt is the cross-invocation companion to
// restartLoopGuard.allowAt. It loads the persistent history at path,
// prunes entries outside the window, and either appends `now` and
// returns true (within budget) or returns false without appending
// (budget exhausted). When over-budget it still rewrites the pruned
// history so stale entries don't accumulate forever in the file.
//
// maxAttempts=3, window=60s matches the architect's threshold from the
// design brief; runStartDriftCheck wires those constants directly.
func recordDriftRestartAttempt(path string, maxAttempts int, window time.Duration, now time.Time) bool {
	attempts := loadDriftRestartHistory(path)
	pruned := pruneRestartHistory(attempts, now, window)
	if len(pruned) >= maxAttempts {
		_ = saveDriftRestartHistory(path, pruned)
		return false
	}
	pruned = append(pruned, now)
	_ = saveDriftRestartHistory(path, pruned)
	return true
}
