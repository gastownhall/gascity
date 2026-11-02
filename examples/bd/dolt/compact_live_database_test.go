package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installVanishDirShim wraps the fixture's fake dolt with a shim that removes
// the fixture's beads database directory from the data dir after its
// successful GC, while SQL keeps answering: the failure mode the
// live-database gate exists to catch. The fake's SQL answers keep working
// because the shim only deletes the directory; open handles and the fake's
// state are unaffected.
func installVanishDirShim(t *testing.T, f compactScriptFixture) {
	t.Helper()
	realDolt := filepath.Join(f.binDir, "dolt.real")
	if err := os.Rename(filepath.Join(f.binDir, "dolt"), realDolt); err != nil {
		t.Fatalf("move fake dolt aside: %v", err)
	}
	shim := `#!/bin/sh
"` + realDolt + `" "$@"
rc=$?
case "$*" in
  *DOLT_GC*)
    if [ "$rc" -eq 0 ]; then
      rm -rf -- "$GC_DOLT_DATA_DIR/beads"
    fi
    ;;
esac
exit "$rc"
`
	if err := os.WriteFile(filepath.Join(f.binDir, "dolt"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write dolt shim: %v", err)
	}
}

// TestCompactScriptLiveGateQuarantinesVanishedDataDir pins the live-database
// gate's primary evidence: when a flatten plus full GC leaves the database out
// of the data dir even though SQL still answers, the pass must quarantine with
// the live-database reason carrying step and probe evidence, alert through
// the quarantine path, and never report the compaction as a success.
func TestCompactScriptLiveGateQuarantinesVanishedDataDir(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	if err := os.MkdirAll(filepath.Join(fixture.dataDir, "zed", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir second db: %v", err)
	}
	installVanishDirShim(t, fixture)

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
	if err == nil {
		t.Fatalf("compact must fail when a database leaves the data dir after its full GC:\n%s", out)
	}
	if !strings.Contains(out, "live-database gate failed") {
		t.Fatalf("output missing live-gate failure diagnostic:\n%s", out)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	if reason := compactMarkerValue(t, marker, "reason"); reason != "live database unavailable after compaction" {
		t.Fatalf("quarantine reason = %q, want the live-database reason:\n%s", reason, out)
	}
	assertCompactMarkerHasEvidence(t, marker,
		"live_gate_steps=flatten,post-flatten full GC",
		"live_gate_failed_step=post-flatten full GC",
		"live_gate_probe_head_after=compactcommit",
		"live_gate_data_dir="+filepath.Join(fixture.dataDir, "beads", ".dolt"),
		"live_gate_data_dir_present_before=1",
		"live_gate_data_dir_present_after=0",
		"live_gate_pre_compaction_head=headcommit",
		"decision=preserve_marker_manual_review_required",
	)
	assertCompactBeadsQuarantineAlert(t, fixture, "mayor", marker, "compact-quarantine", "live database unavailable after compaction")
	if strings.Contains(out, "db=beads commits=600->600 duration=") {
		t.Fatalf("a vanished database must not be reported as a successful compaction:\n%s", out)
	}
	pendingGC := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
	if _, statErr := os.Stat(pendingGC); !os.IsNotExist(statErr) {
		t.Fatalf("live-gate failure must not write a pending-GC marker; stat=%v:\n%s", statErr, out)
	}
}

func liveGateMarkerPath(fixture compactScriptFixture) string {
	return filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
}

// TestCompactScriptLiveGateQuarantinesUnservedDatabaseAfterFlatten pins the
// gate's other leg: the data-dir entry stays intact, but the server no longer
// serves the database's name. The post-flatten gate check must quarantine
// with the live-database reason and probe-failure evidence before any full
// GC runs.
func TestCompactScriptLiveGateQuarantinesUnservedDatabaseAfterFlatten(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
		"GC_FAKE_DOLT_LIVE_PROBE_UNSERVED=1")
	if err == nil {
		t.Fatalf("compact must fail when the server stops serving a database it just flattened:\n%s", out)
	}
	if !strings.Contains(out, "live-database gate failed at step flatten") {
		t.Fatalf("output missing live-gate failure at the flatten step:\n%s", out)
	}
	marker := liveGateMarkerPath(fixture)
	if reason := compactMarkerValue(t, marker, "reason"); reason != "live database unavailable after compaction" {
		t.Fatalf("quarantine reason = %q, want the live-database reason:\n%s", reason, out)
	}
	assertCompactMarkerHasEvidence(t, marker,
		"live_gate_steps=flatten",
		"live_gate_failed_step=flatten",
		"live_gate_probe_failure=served-HEAD probe failed rc=1 database not found: beads",
		"live_gate_data_dir_present_before=1",
		"live_gate_data_dir_present_after=1",
		"live_gate_pre_compaction_head=headcommit",
	)
	assertCompactBeadsQuarantineAlert(t, fixture, "mayor", marker, "compact-quarantine", "live database unavailable after compaction")
	logData, readErr := os.ReadFile(fixture.doltLog)
	if readErr != nil {
		t.Fatalf("read dolt log: %v", readErr)
	}
	if strings.Contains(string(logData), "DOLT_GC") {
		t.Fatalf("a database that stopped being served must not proceed to full GC:\n%s", logData)
	}
	if !strings.Contains(string(logData), "DOLT_RESET") {
		t.Fatalf("the flatten itself should have run before the gate caught the unserved database:\n%s", logData)
	}
}

// TestCompactScriptLiveGateFailsEvenWhenGCSucceeds pins that a successful
// rewrite command is not the success criterion: a full GC that exits 0 must
// still fail the pass when the gate's probe cannot reach the database.
func TestCompactScriptLiveGateFailsEvenWhenGCSucceeds(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
		"GC_FAKE_DOLT_LIVE_PROBE_FAIL_AFTER_GC=1")
	if err == nil {
		t.Fatalf("compact must fail when the database stops being served after a successful GC:\n%s", out)
	}
	if !strings.Contains(out, "live-database gate failed at step post-flatten full GC") {
		t.Fatalf("output missing live-gate failure at the full-GC step:\n%s", out)
	}
	if strings.Contains(out, "db=beads commits=600->600 duration=") {
		t.Fatalf("an unserved database must not be reported as a successful compaction:\n%s", out)
	}
	marker := liveGateMarkerPath(fixture)
	assertCompactMarkerHasEvidence(t, marker,
		"live_gate_steps=flatten,post-flatten full GC",
		"live_gate_failed_step=post-flatten full GC",
		"live_gate_probe_failure=served-HEAD probe failed rc=1 database not found: beads",
		"live_gate_data_dir_present_before=1",
		"live_gate_data_dir_present_after=1",
		"live_gate_pre_compaction_head=headcommit",
	)
}

// TestCompactScriptLiveGateQuarantinesWhenProbeFailsAfterFailedGC covers the
// failed-rewrite leg: the GC itself failed, and the database is also no
// longer served. The gate must quarantine (not just count an ordinary GC
// failure), and the fresh pending-GC marker must stay suppressed.
func TestCompactScriptLiveGateQuarantinesWhenProbeFailsAfterFailedGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "gc_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
		"GC_FAKE_DOLT_LIVE_PROBE_FAIL_AFTER_GC=1")
	if err == nil {
		t.Fatalf("compact must fail when the database stops being served after a failed GC:\n%s", out)
	}
	if !strings.Contains(out, "DOLT_GC failed rc=45") {
		t.Fatalf("the GC's own failure diagnostics must be preserved:\n%s", out)
	}
	if !strings.Contains(out, "live-database gate failed at step post-flatten full GC") {
		t.Fatalf("output missing live-gate failure at the full-GC step:\n%s", out)
	}
	marker := liveGateMarkerPath(fixture)
	if reason := compactMarkerValue(t, marker, "reason"); reason != "live database unavailable after compaction" {
		t.Fatalf("quarantine reason = %q, want the live-database reason:\n%s", reason, out)
	}
	pendingGC := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
	if _, statErr := os.Stat(pendingGC); !os.IsNotExist(statErr) {
		t.Fatalf("live-gate failure must not write a pending-GC marker; stat=%v:\n%s", statErr, out)
	}
}

// TestCompactScriptLiveGateQuarantinesWhenProbeFailsAfterFailedFlatten pins
// the failed-flatten leg: the flatten transaction failed and the database is
// no longer served. The restore attempt still runs, and the gate still
// quarantines instead of returning an ordinary failure.
func TestCompactScriptLiveGateQuarantinesWhenProbeFailsAfterFailedFlatten(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "flatten_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
		"GC_FAKE_DOLT_LIVE_PROBE_UNSERVED=1")
	if err == nil {
		t.Fatalf("compact must fail when the database stops being served after a failed flatten:\n%s", out)
	}
	if !strings.Contains(out, "flatten failed rc=44") {
		t.Fatalf("the flatten's own failure diagnostics must be preserved:\n%s", out)
	}
	if !strings.Contains(out, "restoring pre-flatten HEAD") {
		t.Fatalf("the pre-flatten HEAD restore must still be attempted:\n%s", out)
	}
	if !strings.Contains(out, "live-database gate failed at step flatten") {
		t.Fatalf("output missing live-gate failure at the flatten step:\n%s", out)
	}
	marker := liveGateMarkerPath(fixture)
	assertCompactMarkerHasEvidence(t, marker,
		"live_gate_steps=flatten",
		"live_gate_failed_step=flatten",
		"live_gate_pre_compaction_head=headcommit",
	)
}

// TestCompactScriptLiveGateValidatesProbeResponse pins what the gate accepts
// as a passing probe: the single cell must name the requested database
// (case-insensitively) and carry a nonempty alphanumeric HEAD. Malformed,
// wrong-name, and empty-HEAD responses quarantine with the probe failure
// recorded; a case-variant database name passes.
func TestCompactScriptLiveGateValidatesProbeResponse(t *testing.T) {
	t.Run("malformed head fails the gate", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
			"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
			"GC_FAKE_DOLT_LIVE_PROBE_CELL=beads:not-a-head")
		if err == nil {
			t.Fatalf("a malformed probe response must fail the gate:\n%s", out)
		}
		assertCompactMarkerHasEvidence(t, liveGateMarkerPath(fixture),
			"live_gate_failed_step=flatten",
			"live_gate_probe_failure=malformed probe response: beads:not-a-head",
		)
	})
	t.Run("wrong database name fails the gate", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
			"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
			"GC_FAKE_DOLT_LIVE_PROBE_CELL=other:abc123")
		if err == nil {
			t.Fatalf("a probe answer naming another database must fail the gate:\n%s", out)
		}
		assertCompactMarkerHasEvidence(t, liveGateMarkerPath(fixture),
			"live_gate_failed_step=flatten",
			"live_gate_probe_failure=malformed probe response: other:abc123",
		)
	})
	t.Run("empty HEAD fails the gate", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
			"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
			"GC_FAKE_DOLT_LIVE_PROBE_CELL=beads:")
		if err == nil {
			t.Fatalf("a probe answer without a HEAD must fail the gate:\n%s", out)
		}
		assertCompactMarkerHasEvidence(t, liveGateMarkerPath(fixture),
			"live_gate_failed_step=flatten",
			"live_gate_probe_failure=malformed probe response: beads:",
		)
	})
	t.Run("case-variant database name passes", func(t *testing.T) {
		fixture := newCompactScriptFixture(t)
		out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
			"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
			"GC_FAKE_DOLT_LIVE_PROBE_CELL=BEADS:compactcommit")
		if err != nil {
			t.Fatalf("Dolt serves database names case-insensitively, so a case-variant answer must pass the gate:\n%s", out)
		}
		if !strings.Contains(out, "db=beads commits=600->600 duration=") {
			t.Fatalf("the pass should have completed after the gate passed:\n%s", out)
		}
	})
}

// TestCompactScriptLiveGateAbortsBeforeTouchingFurtherDatabases pins the
// run-level consequence of a gate failure: with more selected databases
// waiting, the run must stop before issuing any statement for them, exit 1,
// and name the databases it did not reach.
func TestCompactScriptLiveGateAbortsBeforeTouchingFurtherDatabases(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	if err := os.MkdirAll(filepath.Join(fixture.dataDir, "zed", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir second db: %v", err)
	}
	installVanishDirShim(t, fixture)

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
	if err == nil {
		t.Fatalf("compact must fail when a database leaves the data dir mid-run:\n%s", out)
	}
	if !strings.Contains(out, "selected databases not reached: zed") {
		t.Fatalf("the abort diagnostic must name the databases the run did not reach:\n%s", out)
	}
	logData, readErr := os.ReadFile(fixture.doltLog)
	if readErr != nil {
		t.Fatalf("read dolt log: %v", readErr)
	}
	if strings.Contains(string(logData), "db=zed") {
		t.Fatalf("no statement may be issued for a database after a live-gate failure:\n%s", logData)
	}
	if reason := compactMarkerValue(t, liveGateMarkerPath(fixture), "reason"); reason != "live database unavailable after compaction" {
		t.Fatalf("quarantine reason = %q, want the live-database reason:\n%s", reason, out)
	}
}

// TestCompactScriptLiveGateAbortSaysNoneWhenNothingElseSelected pins the
// not-reached wording for a single-database selection: "none", not an empty
// list.
func TestCompactScriptLiveGateAbortSaysNoneWhenNothingElseSelected(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	installVanishDirShim(t, fixture)

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
	if err == nil {
		t.Fatalf("compact must fail when a database leaves the data dir mid-run:\n%s", out)
	}
	if !strings.Contains(out, "selected databases not reached: none") {
		t.Fatalf("the abort diagnostic must say none when no further database was selected:\n%s", out)
	}
}

// TestCompactScriptLiveGateAbortNotReachedHonorsOnlyDbs pins that the
// not-reached list respects GC_DOLT_COMPACT_ONLY_DBS: a discovered database
// the run would have skipped anyway is not claimed as unreached work, while a
// selected database without a data-dir entry still is.
func TestCompactScriptLiveGateAbortNotReachedHonorsOnlyDbs(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	if err := os.MkdirAll(filepath.Join(fixture.dataDir, "zed", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir second db: %v", err)
	}
	installVanishDirShim(t, fixture)

	out, err := fixture.run(t, "success",
		"GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
		"GC_DOLT_COMPACT_ONLY_DBS=beads,other")
	if err == nil {
		t.Fatalf("compact must fail when a database leaves the data dir mid-run:\n%s", out)
	}
	if !strings.Contains(out, "selected databases not reached: other") {
		t.Fatalf("the not-reached list must name the selected databases and skip unselected ones:\n%s", out)
	}
}

// TestCompactScriptLiveGateAbortsEvenWhenMarkerCannotPersist pins that the
// abort does not depend on the quarantine marker landing: when the marker
// directory becomes unusable after the rewrite, the evidence goes to stderr
// and the run still stops before any further database work.
func TestCompactScriptLiveGateAbortsEvenWhenMarkerCannotPersist(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	if err := os.MkdirAll(filepath.Join(fixture.dataDir, "zed", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir second db: %v", err)
	}
	realDolt := filepath.Join(fixture.binDir, "dolt.real")
	if err := os.Rename(filepath.Join(fixture.binDir, "dolt"), realDolt); err != nil {
		t.Fatalf("move fake dolt aside: %v", err)
	}
	// After beads' successful GC: drop its data-dir entry and replace the
	// quarantine directory with a plain file, so the gate's marker write
	// fails although every writability probe before the flatten passed.
	shim := `#!/bin/sh
"` + realDolt + `" "$@"
rc=$?
case "$*" in
  *DOLT_GC*)
    if [ "$rc" -eq 0 ]; then
      rm -rf -- "$GC_DOLT_DATA_DIR/beads"
      qdir="$GC_CITY_PATH/.gc/runtime/packs/dolt/compact-quarantine"
      rm -rf -- "$qdir"
      printf 'blocked\n' > "$qdir"
    fi
    ;;
esac
exit "$rc"
`
	if err := os.WriteFile(filepath.Join(fixture.binDir, "dolt"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write dolt shim: %v", err)
	}

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
	if err == nil {
		t.Fatalf("compact must fail even when the quarantine marker cannot be persisted:\n%s", out)
	}
	if !strings.Contains(out, "unable to persist the live-database quarantine marker") {
		t.Fatalf("output missing the marker-persistence failure evidence:\n%s", out)
	}
	if !strings.Contains(out, "failed_step=post-flatten full GC") {
		t.Fatalf("the fallback evidence must name the failed gate step:\n%s", out)
	}
	if !strings.Contains(out, "selected databases not reached: zed") {
		t.Fatalf("the run must still abort before touching further databases:\n%s", out)
	}
	if strings.Contains(out, "db=beads commits=600->600 duration=") {
		t.Fatalf("a gate failure must not be reported as a successful compaction:\n%s", out)
	}
}

// countFakeDoltQueries returns how many invocations in the fake dolt log
// contain the given SQL fragment.
func countFakeDoltQueries(t *testing.T, fixture compactScriptFixture, fragment string) int {
	t.Helper()
	logData, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	return strings.Count(string(logData), fragment)
}

// TestCompactScriptLiveGateRecoversAfterTransientProbeFailure pins the
// transient leg: a probe that fails once and then answers must not quarantine
// anything. The retry recovers within the configured attempts, says so, and
// the run finishes as if the gate had passed on the first try.
func TestCompactScriptLiveGateRecoversAfterTransientProbeFailure(t *testing.T) {
	fixture := newCompactScriptFixture(t)

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
		"GC_FAKE_DOLT_LIVE_PROBE_FAIL_FIRST=1")
	if err != nil {
		t.Fatalf("a transient probe failure must not fail the run:\n%s", out)
	}
	if !strings.Contains(out, "live-database probe recovered on attempt 2/3") {
		t.Fatalf("the retry must say it recovered on the second attempt:\n%s", out)
	}
	if !strings.Contains(out, "db=beads commits=600->600 duration=") {
		t.Fatalf("the pass should have completed after the probe recovered:\n%s", out)
	}
	if _, statErr := os.Stat(liveGateMarkerPath(fixture)); !os.IsNotExist(statErr) {
		t.Fatalf("a recovered probe must not leave a quarantine marker; stat=%v:\n%s", statErr, out)
	}
}

// TestCompactScriptLiveGateRecoveryKeepsOrdinaryFailureOutcome pins that a
// recovered probe never masks the rewrite's own result: with the full GC
// failing and the probe recovering, the run keeps the ordinary pending-GC
// failure path — no quarantine, pending-GC marker as before.
func TestCompactScriptLiveGateRecoveryKeepsOrdinaryFailureOutcome(t *testing.T) {
	fixture := newCompactScriptFixture(t)

	out, err := fixture.run(t, "gc_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
		"GC_FAKE_DOLT_LIVE_PROBE_FAIL_FIRST=1")
	if err == nil {
		t.Fatalf("the GC's own failure must still fail the run:\n%s", out)
	}
	if !strings.Contains(out, "live-database probe recovered on attempt 2/3") {
		t.Fatalf("the probe retry should have recovered before the gate check:\n%s", out)
	}
	if !strings.Contains(out, "DOLT_GC failed rc=45") {
		t.Fatalf("the GC's own failure diagnostics must be preserved:\n%s", out)
	}
	if !strings.Contains(out, "1 database(s) failed compaction") {
		t.Fatalf("the failure must be reported as an ordinary compaction failure:\n%s", out)
	}
	if _, statErr := os.Stat(liveGateMarkerPath(fixture)); !os.IsNotExist(statErr) {
		t.Fatalf("a recovered probe must not turn an ordinary failure into a quarantine; stat=%v:\n%s", statErr, out)
	}
	pendingGC := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
	if _, statErr := os.Stat(pendingGC); statErr != nil {
		t.Fatalf("a recovered probe must leave the pending-GC marker path intact (stat err=%v):\n%s", statErr, out)
	}
}

// TestCompactScriptLiveGateHealthyRunProbeCount pins the gate's probe budget
// on a healthy pass: exactly one probe per armed step (the flatten check and
// the post-full-GC check), so retries only happen on real failure.
func TestCompactScriptLiveGateHealthyRunProbeCount(t *testing.T) {
	fixture := newCompactScriptFixture(t)

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
	if err != nil {
		t.Fatalf("healthy compaction must succeed:\n%s", out)
	}
	if got := countFakeDoltQueries(t, fixture, "CONCAT(DATABASE(), ':', HASHOF('HEAD'))"); got != 2 {
		t.Fatalf("healthy pass issued %d served-HEAD probes, want 2 (one per rewrite step):\n%s", got, out)
	}
}

// TestCompactScriptLiveGateCoversGcOnlyReclaim pins that the --gc-only entry
// path is gated: a full GC whose database stops being served afterwards must
// quarantine at the gc-only step instead of reporting the reclaim as done.
func TestCompactScriptLiveGateCoversGcOnlyReclaim(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.runWithArgs(t, "below_threshold", []string{"--gc-only"},
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
		"GC_FAKE_DOLT_LIVE_PROBE_FAIL_AFTER_GC=1")
	if err == nil {
		t.Fatalf("gc-only reclaim must fail when the database stops being served after its full GC:\n%s", out)
	}
	assertCompactMarkerHasEvidence(t, liveGateMarkerPath(fixture),
		"live_gate_steps=gc-only reclaim",
		"live_gate_failed_step=gc-only reclaim",
		"live_gate_probe_failure=served-HEAD probe failed rc=1 database not found: beads",
	)
	if strings.Contains(out, "gc-only reclaim duration=") {
		t.Fatalf("a gate failure must not be reported as a successful reclaim:\n%s", out)
	}
	pendingGC := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", "beads")
	if _, statErr := os.Stat(pendingGC); !os.IsNotExist(statErr) {
		t.Fatalf("a gate failure must not leave a pending-GC marker; stat=%v:\n%s", statErr, out)
	}
}

// TestCompactScriptLiveGateCoversBareGCMode pins that the bare-GC mode's
// working-set GC is gated: a database that leaves the data dir after its bare
// GC must quarantine at the bare-GC step, not report "— ok".
func TestCompactScriptLiveGateCoversBareGCMode(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	installVanishDirShim(t, fixture)
	out, err := fixture.run(t, "success",
		"GC_DOLT_COMPACT_BARE_GC=1",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0")
	if err == nil {
		t.Fatalf("bare-GC compact must fail when the database leaves the data dir after its GC:\n%s", out)
	}
	if !strings.Contains(out, "live-database gate failed at step bare GC") {
		t.Fatalf("output missing live-gate failure at the bare-GC step:\n%s", out)
	}
	assertCompactMarkerHasEvidence(t, liveGateMarkerPath(fixture),
		"live_gate_steps=bare GC",
		"live_gate_failed_step=bare GC",
		"live_gate_data_dir_present_before=1",
		"live_gate_data_dir_present_after=0",
	)
	if strings.Contains(out, "bare-gc duration=") {
		t.Fatalf("a gate failure must not be reported as a successful bare GC:\n%s", out)
	}
}

// TestCompactScriptLiveGateCoversSharedHistoryBareGC pins that the
// shared-history path's bare GC (the no-flatten reclaim for a database with
// a remote) is gated too.
func TestCompactScriptLiveGateCoversSharedHistoryBareGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_success",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
		"GC_FAKE_DOLT_LIVE_PROBE_FAIL_AFTER_GC=1")
	if err == nil {
		t.Fatalf("shared-history bare GC must fail when the database stops being served:\n%s", out)
	}
	if !strings.Contains(out, "live-database gate failed at step shared-history bare GC") {
		t.Fatalf("output missing live-gate failure at the shared-history bare-GC step:\n%s", out)
	}
	assertCompactMarkerHasEvidence(t, liveGateMarkerPath(fixture),
		"live_gate_steps=shared-history bare GC",
		"live_gate_failed_step=shared-history bare GC",
	)
	logData, readErr := os.ReadFile(fixture.doltLog)
	if readErr != nil {
		t.Fatalf("read dolt log: %v", readErr)
	}
	if strings.Contains(string(logData), "DOLT_RESET") {
		t.Fatalf("the shared-history path must not have flattened:\n%s", logData)
	}
}

// TestCompactScriptLiveGateCoversCriticalDiskHeldBareGC pins the
// critical-disk fallback: when the disk floor holds the pending-GC full GC
// and the database still gets its working-set GC, that bare GC is gated too,
// and the held pending-GC marker survives the quarantine.
func TestCompactScriptLiveGateCoversCriticalDiskHeldBareGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	marker := writeCompactPendingGCMarker(t, fixture, "beads")
	_, outcomeEnv := newCompactOutcomeFile(t)

	// A scheduled run (not --only-db recovery-only) takes the hold path:
	// the full GC stays held below the disk floor and the database gets its
	// working-set GC.
	out, err := fixture.run(t, "remote_success",
		"GC_FAKE_DF_MODE=df_critical",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
		"GC_FAKE_DOLT_LIVE_PROBE_FAIL_AFTER_GC=1",
		outcomeEnv)
	if err == nil {
		t.Fatalf("the held working-set GC must fail when the database stops being served:\n%s", out)
	}
	if !strings.Contains(out, "skipping full GC under critical disk") {
		t.Fatalf("the run should have taken the critical-disk hold path:\n%s", out)
	}
	if !strings.Contains(out, "live-database gate failed at step shared-history bare GC") {
		t.Fatalf("output missing live-gate failure at the held bare-GC step:\n%s", out)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("the held pending-GC marker must survive a gate quarantine: %v", statErr)
	}
}

// TestCompactScriptLiveGateCoversSharedHistoryPendingGCRetry pins that the
// shared-history pending-GC retry (a local DOLT_GC --full for a database the
// guard protects) is gated, keeps the marker on failure, and never pushes.
func TestCompactScriptLiveGateCoversSharedHistoryPendingGCRetry(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	marker := writeCompactPendingGCMarker(t, fixture, "beads")

	out, err := fixture.runWithArgs(t, "remote_success", []string{"--only-db", "beads"},
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
		"GC_FAKE_DOLT_LIVE_PROBE_FAIL_AFTER_GC=1")
	if err == nil {
		t.Fatalf("the shared-history pending-GC retry must fail when the database stops being served:\n%s", out)
	}
	if !strings.Contains(out, "live-database gate failed at step shared-history pending-GC retry") {
		t.Fatalf("output missing live-gate failure at the shared-history retry step:\n%s", out)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("a failed retry must keep the pending-GC marker: %v", statErr)
	}
	logData, readErr := os.ReadFile(fixture.doltLog)
	if readErr != nil {
		t.Fatalf("read dolt log: %v", readErr)
	}
	if strings.Contains(string(logData), "DOLT_PUSH") {
		t.Fatalf("a gated shared-history retry must not push:\n%s", logData)
	}
}

// TestCompactScriptLiveGateCoversPendingGCRetry pins the ordinary pending-GC
// retry in the flatten flow: gated, marker kept, no flatten, no push.
func TestCompactScriptLiveGateCoversPendingGCRetry(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	marker := writeCompactPendingGCMarker(t, fixture, "beads")

	out, err := fixture.run(t, "below_threshold",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
		"GC_FAKE_DOLT_LIVE_PROBE_FAIL_AFTER_GC=1")
	if err == nil {
		t.Fatalf("the pending-GC retry must fail when the database stops being served:\n%s", out)
	}
	if !strings.Contains(out, "live-database gate failed at step pending-GC retry") {
		t.Fatalf("output missing live-gate failure at the pending-GC retry step:\n%s", out)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("a failed retry must keep the pending-GC marker: %v", statErr)
	}
	logData, readErr := os.ReadFile(fixture.doltLog)
	if readErr != nil {
		t.Fatalf("read dolt log: %v", readErr)
	}
	log := string(logData)
	if strings.Contains(log, "DOLT_PUSH") {
		t.Fatalf("a gated pending-GC retry must not push:\n%s", log)
	}
	if strings.Contains(log, "DOLT_RESET") {
		t.Fatalf("a pending-GC retry must not flatten:\n%s", log)
	}
}

// TestCompactScriptLiveGateBaselineUnavailableFailsPassBeforeRewrite pins the
// GC-first baseline rule: when the gate cannot establish pre-compaction
// service, the pass fails as an ordinary error before any rewrite — no
// quarantine, no DOLT_GC — and the run moves on to the next database.
func TestCompactScriptLiveGateBaselineUnavailableFailsPassBeforeRewrite(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	if err := os.MkdirAll(filepath.Join(fixture.dataDir, "zed", ".dolt"), 0o755); err != nil {
		t.Fatalf("mkdir second db: %v", err)
	}

	out, err := fixture.run(t, "success",
		"GC_DOLT_COMPACT_BARE_GC=1",
		"GC_DOLT_COMPACT_LIVE_PROBE_DELAY_SECS=0",
		"GC_FAKE_DOLT_LIVE_PROBE_UNSERVED=1")
	if err == nil {
		t.Fatalf("a pass whose baseline cannot be established must fail:\n%s", out)
	}
	if !strings.Contains(out, "cannot establish pre-compaction service for the live-database gate") {
		t.Fatalf("output missing the baseline-failure diagnostic:\n%s", out)
	}
	if !strings.Contains(out, "2 database(s) failed bare GC") {
		t.Fatalf("baseline failures must be reported as ordinary failures, one per database:\n%s", out)
	}
	if _, statErr := os.Stat(liveGateMarkerPath(fixture)); !os.IsNotExist(statErr) {
		t.Fatalf("a baseline failure must not quarantine; stat=%v:\n%s", statErr, out)
	}
	logData, readErr := os.ReadFile(fixture.doltLog)
	if readErr != nil {
		t.Fatalf("read dolt log: %v", readErr)
	}
	log := string(logData)
	if strings.Contains(log, "DOLT_GC") {
		t.Fatalf("a baseline failure must not rewrite anything:\n%s", log)
	}
	if !strings.Contains(log, "db=zed") {
		t.Fatalf("the run must still attempt the next database's baseline:\n%s", log)
	}
}
