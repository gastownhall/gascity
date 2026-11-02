package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installVanishDirShim wraps the fixture's fake dolt with a shim that removes
// the named database's directory from the data dir after its successful GC,
// while SQL keeps answering: the failure mode the live-database gate exists
// to catch. The fake's SQL answers keep working because the shim only deletes
// the directory; open handles and the fake's state are unaffected.
func installVanishDirShim(t *testing.T, f compactScriptFixture, db string) {
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
      rm -rf -- "$GC_DOLT_DATA_DIR/` + db + `"
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
	installVanishDirShim(t, fixture, "beads")

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
