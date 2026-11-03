package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// compactLiveGateReason is the quarantine reason compact records when a
// database it rewrote stops being served.
const compactLiveGateReason = "live database unavailable after compaction"

func compactQuarantineMarkerPath(fixture compactScriptFixture, db string) string {
	return filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", db)
}

func compactPendingGCMarkerPath(fixture compactScriptFixture, db string) string {
	return filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-gc", db)
}

// setCompactLiveFault arms a one-shot fake-dolt fault that fires after the
// next rewrite of kind ("gc" or "flatten") on db finishes.
func setCompactLiveFault(t *testing.T, fixture compactScriptFixture, kind, db, action string) {
	t.Helper()
	path := filepath.Join(fixture.liveFaultDir, "after-"+kind+"-"+db)
	if err := os.WriteFile(path, []byte(action+"\n"), 0o644); err != nil {
		t.Fatalf("write live fault %s: %v", path, err)
	}
}

func compactDoltLogQueries(log, db, fragment string) []string {
	var matches []string
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "db="+db+" query=") && strings.Contains(line, fragment) {
			matches = append(matches, line)
		}
	}
	return matches
}

func assertCompactLiveGateMarker(t *testing.T, fixture compactScriptFixture, db string, want map[string]string) {
	t.Helper()
	marker := compactQuarantineMarkerPath(fixture, db)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("live gate quarantine marker %s missing: %v", marker, err)
	}
	if reason := compactMarkerValue(t, marker, "reason"); reason != compactLiveGateReason {
		t.Fatalf("quarantine reason = %q, want %q", reason, compactLiveGateReason)
	}
	for key, value := range want {
		if got := compactMarkerValue(t, marker, key); got != value {
			t.Fatalf("quarantine marker %s = %q, want %q", key, got, value)
		}
	}
}

func TestCompactScriptAbortsWhenDatabaseLeavesDataDirAfterGC(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	setCompactLiveFault(t, fixture, "gc", "beads", "vanish")

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded after the database left the data dir:\n%s", out)
	}
	entry := filepath.Join(fixture.dataDir, "beads", ".dolt")
	assertCompactLiveGateMarker(t, fixture, "beads", map[string]string{
		"live_gate_steps":                 "flatten,post-flatten full GC",
		"live_gate_failed_step":           "post-flatten full GC",
		"live_gate_data_dir_entry":        entry,
		"live_gate_data_dir_entry_before": "present",
		"live_gate_data_dir_entry_after":  "absent",
		"live_gate_pre_compaction_head":   "headcommit",
	})
	if !strings.Contains(out, "live database gate failed after post-flatten full GC") {
		t.Fatalf("compact output missing the live gate diagnostic:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "compact: db=beads ") && strings.HasSuffix(line, "— ok") {
			t.Fatalf("compact reported success for a database that left the data dir: %q\n%s", line, out)
		}
	}
	assertCompactBeadsQuarantineAlert(t, fixture, "mayor", compactQuarantineMarkerPath(fixture, "beads"), "compact-quarantine", compactLiveGateReason)
	if _, err := os.Stat(compactPendingGCMarkerPath(fixture, "beads")); !os.IsNotExist(err) {
		t.Fatalf("live gate failure left a pending-GC marker (stat err=%v):\n%s", err, out)
	}
}

func TestCompactScriptLiveGateQuarantineRefusalShowsEvidenceAndRestoreGuidance(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	setCompactBeadsLiveFault(t, fixture, "gc", "vanish")
	if out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500"); err == nil {
		t.Fatalf("compact succeeded after the database left the data dir:\n%s", out)
	}

	out, err := fixture.runWithArgs(t, "success", []string{"--gc-only"})
	if err == nil {
		t.Fatalf("compact reclaimed a database quarantined by the live gate:\n%s", out)
	}
	marker := compactBeadsQuarantineMarkerPath(fixture)
	for _, want := range []string{
		"reason=" + compactLiveGateReason,
		"live_gate_failed_step=post-flatten full GC",
		"live_gate_failure=data-dir entry",
		"live_gate_data_dir_entry_after=absent",
		"live_gate_pre_compaction_head=headcommit",
		"compact: db=beads quarantine recovery: restore the database",
		"then remove " + marker,
		"Compact Quarantine Reasons",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("quarantine refusal missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "rerun gc dolt compact --gc-only") {
		t.Fatalf("quarantine refusal told the operator to GC a database that is not served:\n%s", out)
	}
}

func TestCompactScriptLiveGateQuarantinesWhenFlattenRemovesDataDir(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	setCompactLiveFault(t, fixture, "flatten", "beads", "vanish")

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded after the flatten removed the database:\n%s", out)
	}
	assertCompactLiveGateMarker(t, fixture, "beads", map[string]string{
		"live_gate_steps":                 "flatten",
		"live_gate_failed_step":           "flatten",
		"live_gate_data_dir_entry_before": "present",
		"live_gate_data_dir_entry_after":  "absent",
		"live_gate_pre_compaction_head":   "headcommit",
	})
	assertCompactBeadsQuarantineAlert(t, fixture, "mayor", compactQuarantineMarkerPath(fixture, "beads"), "compact-quarantine", compactLiveGateReason)
	if gcCalls := compactDoltLogQueries(readCompactDoltLog(t, fixture), "beads", "CALL DOLT_GC("); len(gcCalls) != 0 {
		t.Fatalf("compact ran GC on a database the flatten removed: %q", gcCalls)
	}
}
