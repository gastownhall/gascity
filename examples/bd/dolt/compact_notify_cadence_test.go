package dolt_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestCompactScriptStalePendingPushMarkerDoesNotRemailEveryCycle pins the
// "too loud" half of ga-0fdnyn: a stale pending-push marker that stays
// unresolved across repeated compact cycles must not page the operator on
// every single cycle. Previously ensure_remote_push_retry_fresh had no
// dedup at all, so an unresolved marker sent one identical mail per compact
// invocation (observed in production: 40 mails for one unchanged my_db
// marker). The event still fires every cycle so automation keeps observing
// each check; only the mail is gated.
//
// The strand gate spends one automatic retry before a stale marker is
// considered stranded (see TestCompactScriptStalePendingPushMarkerGetsOneAutomaticRetry),
// so the cycle that consumes that retry runs first here and the mail-cadence
// window covers the two stranded cycles that follow it.
func TestCompactScriptStalePendingPushMarkerDoesNotRemailEveryCycle(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "remote_push_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("first compact should succeed locally despite remote push failure: %v\n%s", err, firstOut)
	}
	pendingPush := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	replaceCompactMarkerCreatedAt(t, pendingPush, "1970-01-01T00:00:00Z")

	// Spend the one automatic retry the strand gate owes a stale marker; it
	// fails again, so the marker survives and every later cycle is stranded.
	retryOut, err := fixture.run(t, "remote_push_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("the one automatic retry should defer like any failed push: %v\n%s", err, retryOut)
	}
	resetCompactGCLog(t, fixture)

	secondOut, err := fixture.run(t, "remote_success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("stale pending-push retry succeeded without manual review:\n%s", secondOut)
	}
	thirdOut, err := fixture.run(t, "remote_success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("stale pending-push retry succeeded without manual review:\n%s", thirdOut)
	}

	log := readCompactGCLog(t, fixture)
	mailLines := compactGCLogLinesWithPrefix(log, "gc mail send ")
	if len(mailLines) != 1 {
		t.Fatalf("two stale-pending-push retry cycles over an unresolved marker should send exactly one operator mail, got %d\nlog:\n%s", len(mailLines), log)
	}
	if !strings.Contains(mailLines[0], "seen=") {
		t.Fatalf("stale-marker alert should carry dynamic context (e.g. seen count) so operators can gauge how long it has persisted\nline:\n%s\nlog:\n%s", mailLines[0], log)
	}
	eventLines := compactGCLogLinesWithPrefix(log, "gc event emit dolt.compact.quarantine")
	if len(eventLines) != 2 {
		t.Fatalf("each stale-pending-push retry cycle should still emit an event even when the mail is suppressed, got %d\nlog:\n%s", len(eventLines), log)
	}
}

// TestCompactScriptQuarantineRenotifiesAfterBackstopElapses pins the "too
// quiet" half of ga-0fdnyn: an unresolved quarantine with an UNCHANGED
// reason must still page again once the renotify backstop elapses.
// Previously quarantine_should_notify deduped on exact reason match with no
// time backstop, so a real, still-unresolved integrity failure was reported
// exactly once and then silently ignored forever (observed in production:
// gascity marker seen_count=26, notify_count=1). This must not regress
// TestCompactScriptQuarantineReasonChangeReMails (a changed reason still
// re-mails immediately, independent of the backstop) or
// TestCompactScriptExistingQuarantineMarkerAlertsOnceAcrossRepeatedCycles
// (an unchanged reason within the backstop window still dedups).
func TestCompactScriptQuarantineRenotifiesAfterBackstopElapses(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "row_count_decreases", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("first compact succeeded despite row-count decrease:\n%s", firstOut)
	}
	secondOut, err := fixture.run(t, "below_threshold")
	if err == nil {
		t.Fatalf("second compact succeeded despite quarantine:\n%s", secondOut)
	}

	log := readCompactGCLog(t, fixture)
	if mailLines := compactGCLogLinesWithPrefix(log, "gc mail send "); len(mailLines) != 1 {
		t.Fatalf("dedup should be established after two cycles, want 1 mail, got %d\nlog:\n%s", len(mailLines), log)
	}

	// Force the backstop to have elapsed by backdating the marker's own
	// notify bookkeeping, matching the file's existing convention of aging
	// marker fields directly rather than faking the clock.
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-quarantine", "beads")
	replaceCompactMarkerField(t, marker, "last_notified_ts", "2000-01-01T00:00:00Z")

	thirdOut, err := fixture.run(t, "below_threshold")
	if err == nil {
		t.Fatalf("third compact succeeded despite quarantine:\n%s", thirdOut)
	}

	log = readCompactGCLog(t, fixture)
	mailLines := compactGCLogLinesWithPrefix(log, "gc mail send ")
	if len(mailLines) != 2 {
		t.Fatalf("quarantine must re-notify once the renotify backstop elapses, want 2 mails, got %d\nlog:\n%s", len(mailLines), log)
	}
	if !strings.Contains(mailLines[1], "reason=post-flatten row count decreased") {
		t.Fatalf("backstop re-notify should still carry the original, unchanged reason\nline:\n%s\nlog:\n%s", mailLines[1], log)
	}
	if !strings.Contains(mailLines[1], "seen=") {
		t.Fatalf("repeat notification should carry dynamic context (e.g. seen count) distinguishing it from the first alert\nline:\n%s\nlog:\n%s", mailLines[1], log)
	}
	eventLines := compactGCLogLinesWithPrefix(log, "gc event emit dolt.compact.quarantine")
	if len(eventLines) != 3 {
		t.Fatalf("each compact cycle should still emit a dolt.compact.quarantine event even when the mail is suppressed, got %d\nlog:\n%s", len(eventLines), log)
	}
}
