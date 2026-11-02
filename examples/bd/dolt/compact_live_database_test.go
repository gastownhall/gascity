// Fake-Dolt coverage for the live-database gate: every storage-rewriting
// pass (flatten, full GC, bare GC, pending-GC retry, --gc-only) must end by
// proving the database is still served, quarantining the database and
// aborting the run when it is not, instead of reporting the pass as success
// and moving on to the next database.

package dolt_test

import (
	"errors"
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
