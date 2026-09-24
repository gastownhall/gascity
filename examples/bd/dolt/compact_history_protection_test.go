package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fake-Dolt coverage for #5958: the shared-history guard and the
// gc-compact-base provenance watermark. Real-Dolt counterparts live in
// compact_history_protection_real_dolt_test.go.

func readCompactDoltLog(t *testing.T, fixture compactScriptFixture) string {
	t.Helper()
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read dolt log: %v", err)
	}
	return string(data)
}

func setCompactWatermark(t *testing.T, fixture compactScriptFixture, hash string) {
	t.Helper()
	content := ""
	if hash != "" {
		content = hash + "\n"
	}
	if err := os.WriteFile(fixture.tagStateFile, []byte(content), 0o644); err != nil {
		t.Fatalf("write fake tag state: %v", err)
	}
}

func compactWatermark(t *testing.T, fixture compactScriptFixture) string {
	t.Helper()
	data, err := os.ReadFile(fixture.tagStateFile)
	if err != nil {
		t.Fatalf("read fake tag state: %v", err)
	}
	return strings.TrimSpace(string(data))
}

func compactStateMarkerPath(fixture compactScriptFixture, kind string) string {
	return filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", kind, "beads")
}

func writeCompactStateMarker(t *testing.T, fixture compactScriptFixture, kind, data string) string {
	t.Helper()
	path := compactStateMarkerPath(fixture, kind)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir marker dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	return path
}

// Any configured remote protects history, independent of remote selection,
// .no-sync, --skip-fetch, and --dry-run. The database gets a bare GC instead.
func TestCompactScriptSharedHistoryGuardSkipsFlattenAndPush(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   string
		remote string
		noSync bool
		args   []string
		dryRun bool
	}{
		{name: "origin", mode: "remote_success", remote: "origin"},
		{name: "non-origin", mode: "explicit_backup_remote", remote: "backup"},
		{name: "multiple-without-origin", mode: "multiple_remotes_no_origin", remote: "backup"},
		{name: "no-sync", mode: "remote_success", remote: "origin", noSync: true},
		{name: "skip-fetch", mode: "remote_success", remote: "origin", args: []string{"--skip-fetch"}},
		{name: "dry-run", mode: "remote_success", remote: "origin", args: []string{"--dry-run"}, dryRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			setCompactWatermark(t, fixture, "")
			if tc.noSync {
				writeNoSyncMarker(t, fixture.dataDir)
			}
			out, err := fixture.runWithArgs(t, tc.mode, tc.args, "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
			if err != nil {
				t.Fatalf("guarded database must not fail: %v\n%s", err, out)
			}
			if !strings.Contains(out, "db=beads remote="+tc.remote) || !strings.Contains(out, "skipping flatten and remote push") {
				t.Fatalf("guard did not name the database and remote:\n%s", out)
			}
			log := readCompactDoltLog(t, fixture)
			for _, forbidden := range []string{"DOLT_RESET", "DOLT_COMMIT", "DOLT_PUSH", "DOLT_FETCH", "DOLT_TAG", "DOLT_GC('--full')"} {
				if strings.Contains(log, forbidden) {
					t.Fatalf("guarded database reached %s:\n%s", forbidden, log)
				}
			}
			if tc.dryRun {
				if strings.Contains(log, "CALL ") || !strings.Contains(out, "dry-run (would bare GC)") {
					t.Fatalf("dry run must only report the bare GC:\n%s\n%s", out, log)
				}
			} else if !strings.Contains(log, "CALL DOLT_GC()") || !strings.Contains(out, "db=beads bare-gc duration=") {
				t.Fatalf("guarded database must still get a bare GC:\n%s\n%s", out, log)
			}
			if got := compactWatermark(t, fixture); got != "" {
				t.Fatalf("guarded database was stamped with gc-compact-base=%s", got)
			}
		})
	}
}

// A pending-GC marker from an earlier flatten still gets its local full GC;
// the deferred push is dropped, not replayed and not re-recorded.
func TestCompactScriptSharedHistoryGuardRetriesPendingGCLocally(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	marker := writeCompactStateMarker(t, fixture, "compact-pending-gc",
		"reason=writer race during flatten deferred full GC\nremote=origin\nexpected_remote_head=headcommit\nexpected_remote_head_verified=1\ncompacted_from_head=headcommit\n")

	out, err := fixture.run(t, "remote_success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("pending-GC retry must succeed locally: %v\n%s", err, out)
	}
	log := readCompactDoltLog(t, fixture)
	if !strings.Contains(log, "CALL DOLT_GC('--full')") {
		t.Fatalf("pending-GC retry did not run DOLT_GC --full:\n%s", log)
	}
	for _, forbidden := range []string{"DOLT_PUSH", "DOLT_FETCH", "DOLT_RESET"} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("guarded pending-GC retry reached %s:\n%s", forbidden, log)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("pending-GC marker should be cleared after the local GC: %v", err)
	}
	if _, err := os.Stat(compactStateMarkerPath(fixture, "compact-pending-push")); !os.IsNotExist(err) {
		t.Fatalf("guarded pending-GC retry must not record a pending push: %v", err)
	}
	if !strings.Contains(out, "compacted_from_head=headcommit remote=origin — pre-flatten HEAD recorded before full GC") {
		t.Fatalf("pre-flatten HEAD from the marker must be logged before the full GC:\n%s", out)
	}
	if !strings.Contains(out, "deferred push dropped by shared-history guard") ||
		!strings.Contains(out, "remote=origin still holds the full shared history") {
		t.Fatalf("dropped push must be reported with operator guidance:\n%s", out)
	}
}

// A pending-push marker is held untouched for operator review; no force-push
// happens without the opt-in, and the database still gets a bare GC.
func TestCompactScriptSharedHistoryGuardHoldsPendingPush(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	const markerData = "reason=flatten and full GC succeeded but remote push failed\nremote=origin\nexpected_remote_head=headcommit\nexpected_remote_head_verified=1\n"
	marker := writeCompactStateMarker(t, fixture, "compact-pending-push", markerData)

	out, err := fixture.run(t, "remote_success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("held pending push must not fail the run: %v\n%s", err, out)
	}
	log := readCompactDoltLog(t, fixture)
	if strings.Contains(log, "DOLT_PUSH") || strings.Contains(log, "DOLT_FETCH") {
		t.Fatalf("guard replayed the deferred push:\n%s", log)
	}
	if !strings.Contains(log, "CALL DOLT_GC()") {
		t.Fatalf("guarded database must still get a bare GC:\n%s", log)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != markerData {
		t.Fatalf("guard changed the pending-push marker: %v\n%s", err, data)
	}
	if !strings.Contains(out, "NOT force-pushing") || !strings.Contains(out, "GC_DOLT_COMPACT_ALLOW_FEDERATED=1") {
		t.Fatalf("held marker must be reported with operator guidance:\n%s", out)
	}
	gcLog := readCompactGCLog(t, fixture)
	if !strings.Contains(gcLog, "event emit dolt.compact.quarantine --actor controller --message db=beads type=compact-pending-push-held") {
		t.Fatalf("held marker must emit a compactor event:\n%s", gcLog)
	}
	mails := compactGCLogLinesWithPrefix(gcLog, "gc mail send mayor --from controller -s dolt compact quarantine: beads compact-pending-push-held")
	if len(mails) != 1 {
		t.Fatalf("held marker must mail the alert recipient once, got %d:\n%s", len(mails), gcLog)
	}

	// A second cycle with the marker unchanged is deduplicated by the
	// renotify cadence: event again, no second mail; marker still untouched.
	resetCompactGCLog(t, fixture)
	if out, err := fixture.run(t, "remote_success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500"); err != nil {
		t.Fatalf("second cycle failed: %v\n%s", err, out)
	}
	gcLog = readCompactGCLog(t, fixture)
	if n := len(compactGCLogLinesWithPrefix(gcLog, "gc mail send")); n != 0 {
		t.Fatalf("unchanged held marker re-mailed inside the backstop window:\n%s", gcLog)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != markerData {
		t.Fatalf("alert bookkeeping rewrote the held marker: %v\n%s", err, data)
	}
}

// HEAD and root are resolved by ref and ancestry, never by commit date: a
// clock-skewed adopted commit must not become the watermark (#5958 review).
func TestCompactScriptWatermarkUsesRefHeadNotDateOrder(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	setCompactWatermark(t, fixture, "")

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	if got := compactWatermark(t, fixture); got != "headcommit" {
		t.Fatalf("gc-compact-base = %q, want the ref HEAD headcommit (not a date-ordered commit)\n%s", got, out)
	}
	log := readCompactDoltLog(t, fixture)
	if strings.Contains(log, "ORDER BY date DESC LIMIT 1") || strings.Contains(log, "FROM dolt_log ORDER BY date ASC LIMIT 1") {
		t.Fatalf("compact still resolves HEAD or root by commit date:\n%s", log)
	}
	if !strings.Contains(log, "SELECT HASHOF('HEAD')") || !strings.Contains(log, "WHERE a.parent_hash IS NULL") {
		t.Fatalf("compact must resolve HEAD by ref and root by ancestry:\n%s", log)
	}
}

func TestCompactScriptSharedHistoryGuardFailsClosedOnProbeFailure(t *testing.T) {
	for _, mode := range []string{"remote_count_failure", "remote_count_invalid"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			out, err := fixture.run(t, mode, "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
			if err == nil || !strings.Contains(out, "remote count probe") {
				t.Fatalf("remote probe failure must fail closed: %v\n%s", err, out)
			}
			if log := readCompactDoltLog(t, fixture); strings.Contains(log, "CALL ") {
				t.Fatalf("failed remote probe allowed a mutation:\n%s", log)
			}
		})
	}
}

// Reclaim-only modes never rewrite history, so the guard does not apply.
func TestCompactScriptSharedHistoryGuardLeavesReclaimModesAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  []string
		want string
	}{
		{name: "bare-gc", env: []string{"GC_DOLT_COMPACT_BARE_GC=1"}, want: "CALL DOLT_GC()"},
		{name: "gc-only", args: []string{"--gc-only"}, want: "CALL DOLT_GC('--full')"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			out, err := fixture.runWithArgs(t, "remote_success", tc.args, tc.env...)
			if err != nil {
				t.Fatalf("reclaim failed: %v\n%s", err, out)
			}
			log := readCompactDoltLog(t, fixture)
			if !strings.Contains(log, tc.want) || strings.Contains(log, "DOLT_RESET") || strings.Contains(log, "DOLT_PUSH") {
				t.Fatalf("reclaim must GC without rewriting history or pushing:\n%s", log)
			}
		})
	}
}

func TestCompactScriptRejectsInvalidAllowFederated(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_success", "GC_DOLT_COMPACT_ALLOW_FEDERATED=maybe")
	if err == nil || !strings.Contains(out, "invalid GC_DOLT_COMPACT_ALLOW_FEDERATED=maybe") {
		t.Fatalf("invalid opt-in must be rejected: %v\n%s", err, out)
	}
	if log := readCompactDoltLog(t, fixture); log != "" {
		t.Fatalf("invalid opt-in reached Dolt:\n%s", log)
	}
}

// First sight of history this compactor did not grow stamps HEAD and flattens
// nothing, even far above the threshold.
func TestCompactScriptWatermarkFirstSightProtectsAdoptedHistory(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	setCompactWatermark(t, fixture, "")

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("first sight must succeed: %v\n%s", err, out)
	}
	if got := compactWatermark(t, fixture); got != "headcommit" {
		t.Fatalf("gc-compact-base = %q, want headcommit", got)
	}
	if !strings.Contains(out, "set gc-compact-base=headcommit: history not grown by this compactor") ||
		!strings.Contains(out, "commits=600 commits_since_base=0 base=headcommit below_threshold=500") {
		t.Fatalf("first sight output:\n%s", out)
	}
	if log := readCompactDoltLog(t, fixture); strings.Contains(log, "DOLT_RESET") || strings.Contains(log, "DOLT_GC") {
		t.Fatalf("adopted history was flattened on first sight:\n%s", log)
	}
}

// A database whose root child is a compactor flatten commit, or one the
// operator marked .compact-full-history, is stamped at root and flattened in
// full exactly as before.
func TestCompactScriptWatermarkFirstSightStampsRootForOwnedHistory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   string
		marker bool
		why    string
	}{
		{name: "flatten-fingerprint", mode: "watermark_owned_history", why: "history already flattened by this compactor"},
		{name: "full-history-marker", mode: "success", marker: true, why: "operator opt-in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			setCompactWatermark(t, fixture, "")
			if tc.marker {
				if err := os.WriteFile(filepath.Join(fixture.dataDir, "beads", ".compact-full-history"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			out, err := fixture.run(t, tc.mode, "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
			if err != nil {
				t.Fatalf("compact failed: %v\n%s", err, out)
			}
			if got := compactWatermark(t, fixture); got != "rootcommit" {
				t.Fatalf("gc-compact-base = %q, want rootcommit", got)
			}
			if !strings.Contains(out, "set gc-compact-base=rootcommit: "+tc.why) {
				t.Fatalf("stamp reason missing:\n%s", out)
			}
			if log := readCompactDoltLog(t, fixture); !strings.Contains(log, "CALL DOLT_RESET('--soft', 'rootcommit')") {
				t.Fatalf("owned history must flatten from root:\n%s", log)
			}
		})
	}
}

// With an existing watermark the flatten soft-resets to it, never to root.
func TestCompactScriptWatermarkFlattensOnlyCommitsSinceBase(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	setCompactWatermark(t, fixture, "basecommit")

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	log := readCompactDoltLog(t, fixture)
	if !strings.Contains(log, "CALL DOLT_RESET('--soft', 'basecommit')") || strings.Contains(log, "'rootcommit')") {
		t.Fatalf("flatten must soft-reset to the watermark:\n%s", log)
	}
	if strings.Contains(log, "DOLT_TAG") {
		t.Fatalf("existing watermark must not be re-stamped:\n%s", log)
	}
	if !strings.Contains(out, "commits_since_base=600 base=basecommit tables=") {
		t.Fatalf("flatten output must name the base:\n%s", out)
	}
}

func TestCompactScriptWatermarkThresholdCountsCommitsSinceBase(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	setCompactWatermark(t, fixture, "basecommit")

	out, err := fixture.run(t, "watermark_since_below_threshold", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "commits=600 commits_since_base=7 base=basecommit below_threshold=500 — skip") {
		t.Fatalf("threshold must count commits since the watermark:\n%s", out)
	}
	if log := readCompactDoltLog(t, fixture); strings.Contains(log, "DOLT_RESET") {
		t.Fatalf("below-threshold database was flattened:\n%s", log)
	}
}

// A watermark that is not an ancestor of HEAD (rollback/restore) refuses
// loudly with operator guidance and mutates nothing.
func TestCompactScriptWatermarkNotAncestorRefuses(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	setCompactWatermark(t, fixture, "basecommit")

	out, err := fixture.run(t, "watermark_not_ancestor", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact must fail when the watermark is not an ancestor of HEAD:\n%s", out)
	}
	for _, want := range []string{
		"REFUSING flatten: gc-compact-base=basecommit is not an ancestor of HEAD=headcommit",
		"CALL DOLT_TAG('-d', 'gc-compact-base')",
		".compact-full-history",
		"1 database(s) failed compaction",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("refusal missing %q:\n%s", want, out)
		}
	}
	if log := readCompactDoltLog(t, fixture); strings.Contains(log, "CALL ") {
		t.Fatalf("refused run mutated Dolt:\n%s", log)
	}
}

func TestCompactScriptWatermarkDryRunDoesNotStamp(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	setCompactWatermark(t, fixture, "")

	out, err := fixture.runWithArgs(t, "success", []string{"--dry-run"}, "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "would set gc-compact-base=headcommit") {
		t.Fatalf("dry run must report the stamp it would make:\n%s", out)
	}
	if got := compactWatermark(t, fixture); got != "" {
		t.Fatalf("dry run stamped gc-compact-base=%s", got)
	}
	if log := readCompactDoltLog(t, fixture); strings.Contains(log, "CALL ") {
		t.Fatalf("dry run mutated Dolt:\n%s", log)
	}
}

func TestCompactScriptWatermarkProbeAndWriteFailuresFailClosed(t *testing.T) {
	for _, mode := range []string{"watermark_tag_probe_failure", "watermark_tag_write_failure"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			setCompactWatermark(t, fixture, "")
			out, err := fixture.run(t, mode, "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
			if err == nil || !strings.Contains(out, "gc-compact-base") {
				t.Fatalf("watermark failure must fail closed: %v\n%s", err, out)
			}
			if log := readCompactDoltLog(t, fixture); strings.Contains(log, "DOLT_RESET") || strings.Contains(log, "DOLT_GC") {
				t.Fatalf("watermark failure allowed a flatten:\n%s", log)
			}
		})
	}
}

// The watermark is resolved before the threshold, so a database first seen
// below the threshold is stamped too (first sight means any commit count).
func TestCompactScriptWatermarkStampsBelowThreshold(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	setCompactWatermark(t, fixture, "")

	out, err := fixture.run(t, "below_threshold", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	if got := compactWatermark(t, fixture); got != "headcommit" {
		t.Fatalf("gc-compact-base = %q, want headcommit\n%s", got, out)
	}
}
