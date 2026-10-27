package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	unsatisfiableEventType = "type=compact-pending-push-unsatisfiable"
	// recurringPendingPushType is the stale-marker alert type the unsatisfiable
	// report must replace. The trailing " marker=" keeps it from matching the
	// longer type above.
	recurringPendingPushType = "type=compact-pending-push marker="
)

// stagePendingPushMarker runs a compaction whose remote fetch fails, so the
// database ends up with a pending-push marker naming remote=origin, then
// optionally backdates it past the retry window the way the production marker
// aged. It returns the fixture and the marker path.
func stagePendingPushMarker(t *testing.T, stale bool) (compactScriptFixture, string) {
	t.Helper()
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_fetch_failure", "GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("initial compact should defer the push: %v\n%s", err, out)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("fetch failure should write pending-push marker: %v", err)
	}
	if !strings.Contains(string(data), "remote=origin\n") {
		t.Fatalf("fixture marker should name remote=origin:\n%s", data)
	}
	if stale {
		replaceCompactMarkerCreatedAt(t, marker, "1970-01-01T00:00:00Z")
	}
	resetCompactGCLog(t, fixture)
	return fixture, marker
}

func readCompactFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestCompactScriptPendingPushToRemovedRemoteReportsUnsatisfiableOnce pins
// ga-zvgiw5: a pending-push marker whose remote has since been removed can
// never be satisfied. It must be reported once as its own defect type (not the
// recurring compact-pending-push alert), must stop retrying the remote, and
// must leave the evidence marker byte-for-byte untouched for the operator.
// The stale variant is the production shape: past the retry window the stale
// check used to fire before any push was attempted, so the removed remote was
// never noticed.
func TestCompactScriptPendingPushToRemovedRemoteReportsUnsatisfiableOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stale bool
	}{
		{"stale marker", true},
		{"fresh marker", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, marker := stagePendingPushMarker(t, tc.stale)
			markerBefore := readCompactFile(t, marker)
			fetchesBefore := strings.Count(readCompactFile(t, fixture.doltLog), "DOLT_FETCH")

			// below_threshold reports no origin remote, as after `dolt remote remove origin`.
			for cycle := 1; cycle <= 2; cycle++ {
				out, err := fixture.run(t, "below_threshold", "GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
				if err == nil {
					t.Fatalf("cycle %d: an unsatisfiable pending push must stay a failure for manual review:\n%s", cycle, out)
				}
			}

			log := readCompactGCLog(t, fixture)
			mailLines := compactGCLogLinesWithPrefix(log, "gc mail send ")
			if len(mailLines) != 1 {
				t.Fatalf("two cycles over an unsatisfiable marker should send exactly one mail, got %d\nlog:\n%s", len(mailLines), log)
			}
			if !strings.Contains(mailLines[0], "compact-pending-push-unsatisfiable") {
				t.Fatalf("mail subject should name the unsatisfiable defect, not the recurring alert\nline:\n%s", mailLines[0])
			}
			if !strings.Contains(mailLines[0], "origin") {
				t.Fatalf("mail should name the removed remote\nline:\n%s", mailLines[0])
			}
			eventLines := compactGCLogLinesWithPrefix(log, "gc event emit dolt.compact.quarantine")
			if len(eventLines) != 2 {
				t.Fatalf("each cycle should still emit one event, got %d\nlog:\n%s", len(eventLines), log)
			}
			for _, line := range eventLines {
				if !strings.Contains(line, unsatisfiableEventType) {
					t.Fatalf("event should carry %s\nline:\n%s", unsatisfiableEventType, line)
				}
			}
			if strings.Contains(log, recurringPendingPushType) {
				t.Fatalf("the recurring compact-pending-push alert must not fire for a removed remote:\n%s", log)
			}
			if got := strings.Count(readCompactFile(t, fixture.doltLog), "DOLT_FETCH"); got != fetchesBefore {
				t.Fatalf("a marker whose remote is gone must not retry the fetch: %d fetches before, %d after", fetchesBefore, got)
			}
			if got := readCompactFile(t, marker); got != markerBefore {
				t.Fatalf("evidence marker must be left untouched\nbefore:\n%s\nafter:\n%s", markerBefore, got)
			}
		})
	}
}

// TestCompactScriptDryRunDoesNotReportUnsatisfiablePendingPush pins that a dry
// run only reports what it would do: no mail, no event, marker left in place.
func TestCompactScriptDryRunDoesNotReportUnsatisfiablePendingPush(t *testing.T) {
	fixture, marker := stagePendingPushMarker(t, true)
	markerBefore := readCompactFile(t, marker)

	out, err := fixture.run(t, "below_threshold", "GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", "GC_DOLT_COMPACT_DRY_RUN=1")
	if err != nil {
		t.Fatalf("dry run should succeed: %v\n%s", err, out)
	}
	log := readCompactGCLog(t, fixture)
	if lines := compactGCLogLinesWithPrefix(log, "gc mail send "); len(lines) != 0 {
		t.Fatalf("dry run must not send mail, got:\n%s", log)
	}
	if lines := compactGCLogLinesWithPrefix(log, "gc event emit "); len(lines) != 0 {
		t.Fatalf("dry run must not emit events, got:\n%s", log)
	}
	if got := readCompactFile(t, marker); got != markerBefore {
		t.Fatalf("dry run must not touch the marker\nbefore:\n%s\nafter:\n%s", markerBefore, got)
	}
}

// TestCompactScriptPendingPushRemoteProbeFailureKeepsRecurringAlert pins that
// "the remote is gone" is only concluded from a probe that answered. When the
// existence probe itself fails, nothing is proven, so the marker is handled
// exactly as before: the recurring stale alert, no unsatisfiable report.
func TestCompactScriptPendingPushRemoteProbeFailureKeepsRecurringAlert(t *testing.T) {
	fixture, _ := stagePendingPushMarker(t, true)

	out, err := fixture.run(t, "remote_exists_probe_failure", "GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("stale marker with an unanswered probe should still need manual review:\n%s", out)
	}
	log := readCompactGCLog(t, fixture)
	if strings.Contains(log, unsatisfiableEventType) || strings.Contains(log, "compact-pending-push-unsatisfiable") {
		t.Fatalf("a failed probe must not be classified unsatisfiable:\n%s", log)
	}
	if !strings.Contains(log, recurringPendingPushType) {
		t.Fatalf("a failed probe should leave the recurring stale alert in place:\n%s", log)
	}
}

// TestCompactScriptPendingPushToExistingRemoteIsNotUnsatisfiable pins the
// healthy path: while the marker's remote still resolves, a stale marker keeps
// the recurring alert and a fresh one still retries the push and clears.
func TestCompactScriptPendingPushToExistingRemoteIsNotUnsatisfiable(t *testing.T) {
	t.Run("stale marker keeps recurring alert", func(t *testing.T) {
		fixture, _ := stagePendingPushMarker(t, true)
		out, err := fixture.run(t, "remote_success", "GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
		if err == nil {
			t.Fatalf("stale pending-push retry succeeded without manual review:\n%s", out)
		}
		log := readCompactGCLog(t, fixture)
		if strings.Contains(log, "compact-pending-push-unsatisfiable") {
			t.Fatalf("a remote that still resolves must not be reported unsatisfiable:\n%s", log)
		}
		if !strings.Contains(log, recurringPendingPushType) {
			t.Fatalf("stale marker should still raise the recurring alert:\n%s", log)
		}
	})
	t.Run("fresh marker retries and clears", func(t *testing.T) {
		fixture, marker := stagePendingPushMarker(t, false)
		out, err := fixture.run(t, "remote_success", "GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
		if err != nil {
			t.Fatalf("pending push retry against a live remote should succeed: %v\n%s", err, out)
		}
		if !strings.Contains(readCompactFile(t, fixture.doltLog), "DOLT_PUSH") {
			t.Fatalf("retry against a live remote should push:\n%s", readCompactFile(t, fixture.doltLog))
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("successful retry should clear the marker (stat err=%v)", err)
		}
		if strings.Contains(readCompactGCLog(t, fixture), "compact-pending-push-unsatisfiable") {
			t.Fatalf("a remote that still resolves must not be reported unsatisfiable")
		}
	})
}

// TestCompactScriptFetchFailureMarkerRecordsUnverifiedRemoteHead pins the third
// ga-zvgiw5 contract bullet. A fetch failure leaves the remote head unknowable,
// so the marker cannot record one; it says so explicitly with
// expected_remote_head_verified=0, and the retry adopts and verifies the head
// it fetches (push_remote_after_compaction, "recovered HEAD ... after unknown
// preflight HEAD"). The marker is therefore never silently unverifiable.
func TestCompactScriptFetchFailureMarkerRecordsUnverifiedRemoteHead(t *testing.T) {
	_, marker := stagePendingPushMarker(t, false)
	data := readCompactFile(t, marker)
	for _, want := range []string{"expected_remote_head=\n", "expected_remote_head_verified=0\n"} {
		if !strings.Contains(data, want) {
			t.Fatalf("marker should record %q:\n%s", strings.TrimSpace(want), data)
		}
	}
}
