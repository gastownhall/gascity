package dolt_test

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestCompactScriptRejectsInvalidLiveProbeSettings asserts that a malformed
// live-probe setting stops compact with exit 2 before it takes the lock or
// sends a single statement to the server.
func TestCompactScriptRejectsInvalidLiveProbeSettings(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want string
	}{
		{name: "zero attempts", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=0", want: "invalid GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=0"},
		{name: "all-zero attempts", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=000", want: "invalid GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=000"},
		{name: "negative attempts", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=-1", want: "invalid GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=-1"},
		{name: "non-numeric attempts", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=three", want: "invalid GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=three"},
		{name: "overflowing attempts", env: "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=99999999999999999999", want: "invalid GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=99999999999999999999"},
		{name: "negative delay", env: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=-2", want: "invalid GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=-2"},
		{name: "fractional delay", env: "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=1.5", want: "invalid GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=1.5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", tt.env)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
				t.Fatalf("compact must reject %s with exit 2: got err=%v\n%s", tt.env, err, out)
			}
			if !strings.Contains(out, "compact: "+tt.want) {
				t.Fatalf("output missing %q diagnostic:\n%s", tt.want, out)
			}
			if strings.Contains(out, "another compaction already running") {
				t.Fatalf("validation must run before the compaction lock:\n%s", out)
			}
			if data, err := os.ReadFile(fixture.doltLog); err == nil && len(data) > 0 {
				t.Fatalf("validation must fail before any database query, dolt log:\n%s", data)
			}
		})
	}
}

// TestCompactScriptAcceptsLiveProbeSettings asserts that empty settings fall
// back to the defaults and that leading zeros are read as decimal.
func TestCompactScriptAcceptsLiveProbeSettings(t *testing.T) {
	tests := []struct {
		name string
		env  []string
	}{
		{name: "empty uses defaults", env: []string{"GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=", "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS="}},
		{name: "leading zeros are decimal", env: []string{"GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=08", "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=00"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			env := append([]string{"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500"}, tt.env...)
			out, err := fixture.run(t, "below_threshold", env...)
			if err != nil {
				t.Fatalf("compact must accept %v: %v\n%s", tt.env, err, out)
			}
			if !strings.Contains(out, "below_threshold=500 — skip") {
				t.Fatalf("compact should reach the threshold skip:\n%s", out)
			}
		})
	}
}
