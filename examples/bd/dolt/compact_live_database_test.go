package dolt_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

// installDoltShim wraps the fixture's fake dolt with a shell shim so a test
// can mutate the data dir after a chosen rewrite command succeeds while the
// server keeps answering SQL.
func installDoltShim(t *testing.T, fixture compactScriptFixture, dropAfter, dropDB string) {
	t.Helper()
	realDolt := filepath.Join(fixture.binDir, "dolt.real")
	if err := os.Rename(filepath.Join(fixture.binDir, "dolt"), realDolt); err != nil {
		t.Fatalf("move fake dolt aside: %v", err)
	}
	shim := `#!/bin/sh
"` + realDolt + `" "$@"
rc=$?
case "$*" in
  *` + dropAfter + `*)
    if [ "$rc" -eq 0 ] && [ -n "$GC_DOLT_DATA_DIR" ]; then
      rm -rf -- "$GC_DOLT_DATA_DIR/` + dropDB + `"
    fi
    ;;
esac
exit "$rc"
`
	if err := os.WriteFile(filepath.Join(fixture.binDir, "dolt"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write dolt shim: %v", err)
	}
}

// TestCompactScriptLiveGateQuarantinesVanishedDataDirDuringFlatten asserts
// the live-database gate catches a flatten that leaves the database out of
// the data dir even though the server still answers SQL: the pass must be
// quarantined and the run must fail before any post-flatten probe, integrity
// check, or GC runs.
func TestCompactScriptLiveGateQuarantinesVanishedDataDirDuringFlatten(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	installDoltShim(t, fixture, "DOLT_RESET", "beads")

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded although beads left the data dir during its flatten:\n%s", out)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	assertCompactMarkerHasEvidence(t, marker,
		"reason=live database unavailable after compaction",
		"live_gate_dir="+filepath.Join(fixture.dataDir, "beads", ".dolt"),
		"live_gate_dir_before=present",
		"live_gate_dir_after=absent",
		"live_gate_failed_step=flatten history",
		"live_gate_pre_compaction_head=headcommit",
		"decision=preserve_marker_manual_review_required",
	)
	assertCompactBeadsQuarantineAlert(t, fixture, "mayor", marker, "compact-quarantine",
		"live database unavailable after compaction")

	// The gate stops the pass before any post-flatten statement: nothing may
	// follow the flatten transaction in the dolt log.
	logData, readErr := os.ReadFile(fixture.doltLog)
	if readErr != nil {
		t.Fatalf("read dolt log: %v", readErr)
	}
	logLines := strings.Split(string(logData), "\n")
	for i, line := range logLines {
		// The flatten transaction (DOLT_RESET + DOLT_COMMIT in one call)
		// logs as several lines ending with the commit message.
		if strings.Contains(line, "compaction: flatten history") {
			if rest := strings.TrimSpace(strings.Join(logLines[i+1:], "\n")); rest != "" {
				t.Fatalf("no database statement may follow a failed live gate:\n%s", rest)
			}
		}
	}
	if strings.Contains(string(logData), "DOLT_GC") {
		t.Fatalf("compact must not run GC after the data dir vanished:\n%s", logData)
	}
	if strings.Contains(out, "— ok") {
		t.Fatalf("compact must not report success after the data dir vanished:\n%s", out)
	}
	// The failed gate suppresses new flatten-remediation state.
	for _, markerDir := range []string{"compact-pending-gc", "compact-pending-push"} {
		if _, statErr := os.Stat(filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", markerDir, "beads")); !os.IsNotExist(statErr) {
			t.Fatalf("failed live gate must not write a %s marker (stat err=%v):\n%s", markerDir, statErr, out)
		}
	}
}
