package dolt_test

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestCompactScriptRejectsInvalidLiveProbeSettings asserts the live-gate
// probe settings are validated before compact takes the compaction lock or
// issues any database statement: an invalid value exits 2 with a diagnostic
// naming the variable.
func TestCompactScriptRejectsInvalidLiveProbeSettings(t *testing.T) {
	cases := []struct {
		name string
		env  string
	}{
		{"zero attempts", "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=0"},
		{"all-zero attempts", "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=000"},
		{"negative attempts", "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=-1"},
		{"non-numeric attempts", "GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=three"},
		{"negative delay", "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=-2"},
		{"non-numeric delay", "GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=soon"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			out, err := fixture.run(t, "success", tc.env)
			if err == nil {
				t.Fatalf("compact accepted invalid live-probe setting %s:\n%s", tc.env, out)
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
				t.Fatalf("compact must exit 2 on invalid live-probe setting, got %v:\n%s", err, out)
			}
			if !strings.Contains(out, "invalid GC_DOLT_COMPACT_LIVE_PROBE") {
				t.Fatalf("compact output missing invalid-setting diagnostic:\n%s", out)
			}
			// Validation must run before the compaction lock or any dolt
			// query, so the fake dolt log may not exist or be empty.
			if logData, readErr := os.ReadFile(fixture.doltLog); readErr == nil {
				if strings.Contains(string(logData), "query=") {
					t.Fatalf("invalid live-probe setting must exit before any dolt query:\n%s", logData)
				}
			}
		})
	}
}

// TestCompactScriptAcceptsValidLiveProbeSettings asserts the accepted shapes:
// empty settings fall back to the defaults, and decimal values with leading
// zeros (including all-zero delays) parse without octal or overflow errors.
func TestCompactScriptAcceptsValidLiveProbeSettings(t *testing.T) {
	for _, env := range []string{
		"GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=03",
		"GC_DOLT_COMPACT_LIVE_PROBE_ATTEMPTS=1",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=00",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
	} {
		t.Run(env, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			out, err := fixture.run(t, "below_threshold", env, "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
			if err != nil {
				t.Fatalf("compact rejected valid live-probe setting %s: %v\n%s", env, err, out)
			}
			if strings.Contains(out, "invalid GC_DOLT_COMPACT_LIVE_PROBE") {
				t.Fatalf("compact flagged valid live-probe setting %s:\n%s", env, out)
			}
		})
	}
}
