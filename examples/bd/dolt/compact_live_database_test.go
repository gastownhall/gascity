package dolt_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Fake-Dolt coverage for the compactor's live-database gate: after every
// history rewrite (flatten, full GC, bare GC) the database must still own its
// data directory and still be served, or the run quarantines it and stops.

const liveGateReason = "live database unavailable after compaction"

func liveGateEnv(extra ...string) []string {
	return append([]string{
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
	}, extra...)
}

func addCompactDatabases(t *testing.T, fixture compactScriptFixture, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.MkdirAll(filepath.Join(fixture.dataDir, name, ".dolt"), 0o755); err != nil {
			t.Fatalf("mkdir dolt db %s: %v", name, err)
		}
	}
}

func beadsMarker(fixture compactScriptFixture, kind string) string {
	return compactStateMarkerPath(fixture, kind)
}

// doltLogEntries splits the fake dolt log into one entry per query; a
// multi-line query (the flatten transaction) stays one entry.
func doltLogEntries(log string) []string {
	var entries []string
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "db=") || len(entries) == 0 {
			entries = append(entries, line)
			continue
		}
		entries[len(entries)-1] += "\n" + line
	}
	return entries
}

// beadsLogAfter returns the dolt log entries after the first beads query
// containing marker, or fails when no such query exists.
func beadsLogAfter(t *testing.T, log, marker string) string {
	t.Helper()
	entries := doltLogEntries(log)
	for i, entry := range entries {
		if strings.HasPrefix(entry, "db=beads ") && strings.Contains(entry, marker) {
			return strings.Join(entries[i+1:], "\n")
		}
	}
	t.Fatalf("dolt log has no %q query on db=beads:\n%s", marker, log)
	return ""
}

// compactExitCode returns the exit status of a compact run that failed.
func compactExitCode(t *testing.T, err error, out string) int {
	t.Helper()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("compact exit = %v, want a nonzero exit\n%s", err, out)
	}
	return exitErr.ExitCode()
}

func requireFailedRun(t *testing.T, err error, out string) {
	t.Helper()
	if code := compactExitCode(t, err, out); code != 1 {
		t.Fatalf("compact exit = %d, want 1\n%s", code, out)
	}
}

// beadsReportedSuccess reports whether a beads pass printed its success line.
func beadsReportedSuccess(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "compact: db=beads ") && strings.HasSuffix(line, "— ok") {
			return true
		}
	}
	return false
}

func requireNoMarker(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		data, _ := os.ReadFile(path)
		t.Fatalf("unexpected marker %s (stat err=%v):\n%s", path, err, data)
	}
}

// requireLiveGateQuarantine checks the quarantine marker, its alert, and the
// run abort left by a live-gate failure on beads during step.
func requireLiveGateQuarantine(t *testing.T, fixture compactScriptFixture, out, step string) string {
	t.Helper()
	marker := beadsMarker(fixture, "compact-quarantine")
	if got := compactMarkerValue(t, marker, "reason"); got != liveGateReason {
		t.Fatalf("quarantine reason = %q, want %q\n%s", got, liveGateReason, out)
	}
	if got := compactMarkerValue(t, marker, "live_gate_failed_step"); got != step {
		t.Fatalf("live_gate_failed_step = %q, want %q", got, step)
	}
	if got := compactMarkerValue(t, marker, "live_gate_data_dir"); got != filepath.Join(fixture.dataDir, "beads", ".dolt") {
		t.Fatalf("live_gate_data_dir = %q", got)
	}
	if got := compactMarkerValue(t, marker, "decision"); got != "preserve_marker_restore_before_clearing" {
		t.Fatalf("decision = %q", got)
	}
	if !strings.Contains(compactMarkerValue(t, marker, "recovery"), "restore") {
		t.Fatalf("marker must carry restore-before-clear guidance")
	}
	assertCompactBeadsQuarantineAlert(t, fixture, "mayor", marker, "compact-quarantine", liveGateReason)
	if want := "compact: db=beads live-database gate failed after " + step + " — aborting run"; !strings.Contains(out, want) {
		t.Fatalf("output missing run abort diagnostic %q:\n%s", want, out)
	}
	return marker
}

// AC-1: a data directory that disappears during a rewrite quarantines the
// database and fails the run even though SQL still answers.
func TestCompactScriptLiveGateQuarantinesVanishedDataDir(t *testing.T) {
	for _, tc := range []struct {
		name    string
		faultOn string
		step    string
	}{
		{name: "post-flatten full GC", faultOn: "DOLT_GC", step: "post-flatten full GC"},
		{name: "flatten", faultOn: "DOLT_RESET", step: "flatten"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			addCompactDatabases(t, fixture, "zed")
			out, err := fixture.run(t, "success", liveGateEnv(
				"GC_FAKE_DOLT_LIVE_FAULT=drop_dir",
				"GC_FAKE_DOLT_LIVE_FAULT_ON="+tc.faultOn,
			)...)
			requireFailedRun(t, err, out)
			marker := requireLiveGateQuarantine(t, fixture, out, tc.step)
			for key, want := range map[string]string{
				"live_gate_data_dir_before":     "present",
				"live_gate_data_dir_after":      "absent",
				"live_gate_pre_compaction_head": "headcommit",
			} {
				if got := compactMarkerValue(t, marker, key); got != want {
					t.Fatalf("%s = %q, want %q", key, got, want)
				}
			}
			if beadsReportedSuccess(out) {
				t.Fatalf("vanished database must not report compaction success:\n%s", out)
			}
			requireNoMarker(t, beadsMarker(fixture, "compact-pending-gc"))
			if after := beadsLogAfter(t, readCompactDoltLog(t, fixture), tc.faultOn); strings.TrimSpace(after) != "" {
				t.Fatalf("no statement may follow a vanished data directory:\n%s", after)
			}

			// A later run refuses the database and reports the live-gate
			// evidence with restore guidance; the reason never auto-clears.
			addCompactDatabases(t, fixture, "beads")
			rerun, err := fixture.run(t, "success", liveGateEnv()...)
			requireFailedRun(t, err, rerun)
			for _, want := range []string{
				"compact: db=beads live-gate evidence: live_gate_failed_step=" + tc.step,
				"live_gate_data_dir_after=absent",
				"restore it from its latest verified backup",
			} {
				if !strings.Contains(rerun, want) {
					t.Fatalf("rerun output missing %q:\n%s", want, rerun)
				}
			}
			if got := compactMarkerValue(t, marker, "reason"); got != liveGateReason {
				t.Fatalf("rerun replaced the live-gate quarantine reason with %q", got)
			}
		})
	}
}
