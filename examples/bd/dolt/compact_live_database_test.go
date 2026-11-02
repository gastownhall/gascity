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

// compactQuarantineMarkerPath is the integrity-quarantine marker path for
// the fixture's default database.
func compactQuarantineMarkerPath(fixture compactScriptFixture) string {
	return filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
}

// assertNoStatementsAfterFlattenButGateProbes asserts that the dolt log
// shows nothing after the flatten transaction except the live-gate's own
// served-HEAD probes: a failed gate must end the pass before any integrity
// check, GC, push, or other statement.
func assertNoStatementsAfterFlattenButGateProbes(t *testing.T, logData string) {
	t.Helper()
	logLines := strings.Split(logData, "\n")
	sawFlatten := false
	for i, line := range logLines {
		if strings.Contains(line, "compaction: flatten history") {
			sawFlatten = true
			for _, rest := range logLines[i+1:] {
				rest = strings.TrimSpace(rest)
				if rest == "" {
					continue
				}
				if !strings.Contains(rest, "SELECT CONCAT(DATABASE(), ':', HASHOF('HEAD'))") {
					t.Fatalf("no database statement but the gate's own probes may follow a failed live gate:\n%s", rest)
				}
			}
		}
	}
	if !sawFlatten {
		t.Fatalf("dolt log missing the flatten transaction:\n%s", logData)
	}
}

// assertNoPendingRemediationMarkers asserts the failed gate suppressed new
// flatten-remediation state for the database.
func assertNoPendingRemediationMarkers(t *testing.T, fixture compactScriptFixture, out string) {
	t.Helper()
	for _, markerDir := range []string{"compact-pending-gc", "compact-pending-push"} {
		if _, statErr := os.Stat(filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", markerDir, "beads")); !os.IsNotExist(statErr) {
			t.Fatalf("failed live gate must not write a %s marker (stat err=%v):\n%s", markerDir, statErr, out)
		}
	}
}

// TestCompactScriptLiveGateQuarantinesUnservedDatabaseAfterFlatten asserts
// the gate's served-HEAD probe: a flatten whose data-dir entry survives but
// whose database the server no longer serves is quarantined before any
// integrity check or GC runs.
func TestCompactScriptLiveGateQuarantinesUnservedDatabaseAfterFlatten(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_FAKE_DOLT_LIVE_PROBE_FAIL_ALL=1")
	if err == nil {
		t.Fatalf("compact succeeded although beads was unserved after its flatten:\n%s", out)
	}
	marker := compactQuarantineMarkerPath(fixture)
	assertCompactMarkerHasEvidence(t, marker,
		"reason=live database unavailable after compaction",
		"live_gate_dir_before=present",
		"live_gate_dir_after=present",
		"live_gate_failed_step=flatten history",
		"live_gate_pre_compaction_head=headcommit",
		"live_gate_probe_failure=served HEAD probe failed",
		"decision=preserve_marker_manual_review_required",
	)
	assertCompactBeadsQuarantineAlert(t, fixture, "mayor", marker, "compact-quarantine",
		"live database unavailable after compaction")

	logData, readErr := os.ReadFile(fixture.doltLog)
	if readErr != nil {
		t.Fatalf("read dolt log: %v", readErr)
	}
	assertNoStatementsAfterFlattenButGateProbes(t, string(logData))
	if strings.Contains(string(logData), "DOLT_GC") {
		t.Fatalf("compact must not run GC after the database became unserved:\n%s", logData)
	}
	if strings.Contains(out, "— ok") {
		t.Fatalf("compact must not report success after the database became unserved:\n%s", out)
	}
	assertNoPendingRemediationMarkers(t, fixture, out)
}

// TestCompactScriptLiveGateQuarantinesUnservedDatabaseAfterSuccessfulGC
// asserts the gate does not trust a rewrite's exit status: a full GC that
// reports success still ends quarantined when the database it rewrote is
// unserved afterwards.
func TestCompactScriptLiveGateQuarantinesUnservedDatabaseAfterSuccessfulGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_FAKE_DOLT_LIVE_PROBE_FAIL_AT=2")
	if err == nil {
		t.Fatalf("compact succeeded although beads was unserved after its full GC:\n%s", out)
	}
	marker := compactQuarantineMarkerPath(fixture)
	assertCompactMarkerHasEvidence(t, marker,
		"reason=live database unavailable after compaction",
		"live_gate_dir_after=present",
		"live_gate_failed_step=post-flatten full GC",
		"live_gate_probe_failure=served HEAD probe failed",
		"live_gate_steps=flatten history; post-flatten full GC",
	)

	logData, readErr := os.ReadFile(fixture.doltLog)
	if readErr != nil {
		t.Fatalf("read dolt log: %v", readErr)
	}
	if !strings.Contains(string(logData), "DOLT_GC") {
		t.Fatalf("expected the full GC to have run before the failed gate:\n%s", logData)
	}
	if strings.Contains(out, "— ok") {
		t.Fatalf("compact must not report success after the database became unserved:\n%s", out)
	}
	assertNoPendingRemediationMarkers(t, fixture, out)
}

// TestCompactScriptLiveGateQuarantinesUnservedDatabaseAfterFailedGC asserts
// a failed rewrite still runs the gate: a full GC that failed AND left the
// database unserved is quarantined for the liveness loss, not recorded as
// an ordinary GC failure with a pending-GC marker.
func TestCompactScriptLiveGateQuarantinesUnservedDatabaseAfterFailedGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "gc_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_FAKE_DOLT_LIVE_PROBE_FAIL_AT=2")
	if err == nil {
		t.Fatalf("compact succeeded although beads was unserved after its failed GC:\n%s", out)
	}
	if !strings.Contains(out, "gc exploded") {
		t.Fatalf("output missing the GC failure stderr:\n%s", out)
	}
	marker := compactQuarantineMarkerPath(fixture)
	assertCompactMarkerHasEvidence(t, marker,
		"reason=live database unavailable after compaction",
		"live_gate_failed_step=post-flatten full GC",
		"live_gate_probe_failure=served HEAD probe failed",
	)
	assertNoPendingRemediationMarkers(t, fixture, out)
}

// TestCompactScriptLiveGateQuarantineAllowsFlattenRestoration asserts the
// failed-flatten restoration still runs before the gate quarantines: the
// operator keeps the flatten-failure evidence even when the database is
// also unserved.
func TestCompactScriptLiveGateQuarantineAllowsFlattenRestoration(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "flatten_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_FAKE_DOLT_LIVE_PROBE_FAIL_ALL=1")
	if err == nil {
		t.Fatalf("compact succeeded although beads' flatten failed and it was unserved:\n%s", out)
	}
	if !strings.Contains(out, "reset exploded") {
		t.Fatalf("output missing the flatten failure stderr:\n%s", out)
	}
	marker := compactQuarantineMarkerPath(fixture)
	assertCompactMarkerHasEvidence(t, marker,
		"reason=live database unavailable after compaction",
		"live_gate_failed_step=flatten history",
		"live_gate_probe_failure=served HEAD probe failed",
	)
	assertNoPendingRemediationMarkers(t, fixture, out)
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
