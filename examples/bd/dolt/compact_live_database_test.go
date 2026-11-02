// Fake-Dolt coverage for the live-database gate: every storage-rewriting
// pass (flatten, full GC, bare GC, pending-GC retry, --gc-only) must end by
// proving the database is still served, quarantining the database and
// aborting the run when it is not, instead of reporting the pass as success
// and moving on to the next database.

package dolt_test

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// requireCompactExitCode fails the test unless compact exited with exactly
// the wanted status.
func requireCompactExitCode(t *testing.T, out string, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("compact exited 0, want %d:\n%s", want, out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("compact error is not an exit status: %v\n%s", err, out)
	}
	if exitErr.ExitCode() != want {
		t.Fatalf("compact exit code = %d, want %d:\n%s", exitErr.ExitCode(), want, out)
	}
}

// readCompactMarkerBody reads a marker file compact wrote.
func readCompactMarkerBody(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read marker %s: %v", path, err)
	}
	return string(data)
}

// compactLiveGateMarkerPath is the integrity-quarantine marker path for beads.
func compactLiveGateMarkerPath(fixture compactScriptFixture) string {
	return compactStateMarkerPath(fixture, "compact-quarantine")
}

// TestCompactScriptRejectsInvalidLiveProbeSettings: an invalid live-probe
// setting must exit 2 with a validation diagnostic before the compaction
// lock is taken or any database is queried, while empty settings fall back
// to the defaults and valid values (including leading zeros) are accepted.
func TestCompactScriptRejectsInvalidLiveProbeSettings(t *testing.T) {
	invalid := []struct {
		name string
		env  string
		want string
	}{
		{name: "attempts zero", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=0", want: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=0"},
		{name: "attempts all zero", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=000", want: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=000"},
		{name: "attempts negative", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=-1", want: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=-1"},
		{name: "attempts non-numeric", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=abc", want: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=abc"},
		{name: "attempts fractional", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=1.5", want: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=1.5"},
		{name: "delay negative", env: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=-1", want: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=-1"},
		{name: "delay non-numeric", env: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=abc", want: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=abc"},
		{name: "delay fractional", env: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=1.5", want: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=1.5"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			out, err := fixture.run(t, "below_threshold", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", tc.env)
			requireCompactExitCode(t, out, err, 2)
			if !strings.Contains(out, "invalid "+tc.want) {
				t.Fatalf("validation diagnostic must name the setting:\n%s", out)
			}
			if log := readCompactDoltLog(t, fixture); log != "" {
				t.Fatalf("invalid settings must be rejected before any database query:\n%s", log)
			}
		})
	}

	valid := []struct {
		name string
		env  string
	}{
		{name: "attempts empty uses default", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS="},
		{name: "attempts leading zeros", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=007"},
		{name: "delay empty uses default", env: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS="},
		{name: "delay zero", env: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0"},
		{name: "delay leading zeros", env: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=007"},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			out, err := fixture.run(t, "below_threshold", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", tc.env)
			if err != nil {
				t.Fatalf("valid setting rejected: %v\n%s", err, out)
			}
		})
	}
}

// TestCompactScriptLiveGateQuarantinesVanishedDataDir: a rewrite that leaves
// the database out of the data dir — while SQL may even still answer — must
// be quarantined with the live-database reason, alerted through the existing
// quarantine event/mail path, and fail the run, for both entry shapes
// (flatten's post-rewrite full GC and --gc-only).
func TestCompactScriptLiveGateQuarantinesVanishedDataDir(t *testing.T) {
	t.Run("flatten full gc drops the data dir", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		out, err := fixture.run(t, "live_vanish_after_gc",
			"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
			"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
		requireCompactExitCode(t, out, err, 1)
		if !strings.Contains(out, "live database unavailable after compaction") {
			t.Fatalf("compact must report the live-database quarantine reason:\n%s", out)
		}

		marker := compactLiveGateMarkerPath(fixture)
		body := readCompactMarkerBody(t, marker)
		for _, want := range []string{
			"db=beads",
			"reason=live database unavailable after compaction",
			"live_gate_steps=flatten, post-flatten full GC",
			"live_gate_failed_step=post-flatten full GC",
			"live_gate_dir_present_before=1",
			"live_gate_dir_present_after=0",
			"live_gate_pre_compaction_head=headcommit",
			"decision=preserve_marker_manual_review_required",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("quarantine marker missing %q:\n%s\noutput:\n%s", want, body, out)
			}
		}

		assertCompactBeadsQuarantineAlert(t, fixture, "mayor", marker, "compact-quarantine",
			"live database unavailable after compaction")

		if _, statErr := os.Stat(compactStateMarkerPath(fixture, "compact-pending-gc")); !os.IsNotExist(statErr) {
			t.Fatalf("a failed live gate must not record pending-GC bookkeeping:\n%s", out)
		}
		if strings.Contains(out, "commits=600->600") && strings.Contains(out, "— ok") {
			t.Fatalf("the failed pass must not be reported as success:\n%s", out)
		}
	})

	t.Run("gc-only drops the data dir", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		out, err := fixture.runWithArgs(t, "live_vanish_after_gc", []string{"--gc-only"},
			"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
		requireCompactExitCode(t, out, err, 1)
		marker := compactLiveGateMarkerPath(fixture)
		body := readCompactMarkerBody(t, marker)
		for _, want := range []string{
			"reason=live database unavailable after compaction",
			"live_gate_failed_step=gc-only reclaim",
			"live_gate_dir_present_before=1",
			"live_gate_dir_present_after=0",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("quarantine marker missing %q:\n%s\noutput:\n%s", want, body, out)
			}
		}
	})
}
