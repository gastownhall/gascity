package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	compactPushMainCall    = "CALL DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')"
	compactRemoteMainProbe = "SELECT hash FROM dolt_remote_branches WHERE name = 'remotes/origin/main'"
)

func compactPendingPushMarkerPath(fixture compactScriptFixture) string {
	return filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
}

// TestCompactScriptVerifiesRemoteTipAfterPush pins the success contract: a
// push is reported as pushed only after the remote-tracking ref is re-probed
// (after DOLT_PUSH, in the same run) and matches the local tip.
func TestCompactScriptVerifiesRemoteTipAfterPush(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "remote=origin pushed compacted main remote_head=compactcommit") {
		t.Fatalf("success line should carry the verified remote tip:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	pushAt := strings.Index(log, compactPushMainCall)
	if pushAt < 0 {
		t.Fatalf("dolt log missing push:\n%s", log)
	}
	if !strings.Contains(log[pushAt:], compactRemoteMainProbe) {
		t.Fatalf("remote tip must be re-probed after the push:\n%s", log)
	}
	if !strings.Contains(log, "SELECT hash FROM dolt_branches WHERE name = 'main'") {
		t.Fatalf("local tip must be probed for the post-push comparison:\n%s", log)
	}
	if _, err := os.Stat(compactPendingPushMarkerPath(fixture)); !os.IsNotExist(err) {
		t.Fatalf("verified push must not leave a pending-push marker (stat err=%v)", err)
	}
}

// TestCompactScriptKeepsPendingPushWhenPushReportsSuccessButRemoteTipDoesNotMove
// reproduces the incident shape: DOLT_PUSH exits 0 but the remote-tracking ref
// never advances (the proxied client was killed by the managed sql-server's
// read_timeout and still exited 0). The old code cleared the marker and
// printed "pushed" while the remote silently diverged. Now the marker is
// written, both hashes are logged, and the run fails.
func TestCompactScriptKeepsPendingPushWhenPushReportsSuccessButRemoteTipDoesNotMove(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	out, err := fixture.run(t, "remote_push_tip_unmoved", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("a push whose remote tip did not move must fail the run:\n%s", out)
	}
	if !strings.Contains(out, "push reported success rc=0 but remote HEAD=headcommit does not match local HEAD=compactcommit") {
		t.Fatalf("output should log both hashes:\n%s", out)
	}
	if strings.Contains(out, "pushed compacted") {
		t.Fatalf("unverified push must not be reported as pushed:\n%s", out)
	}
	if !strings.Contains(out, "1 database(s) failed compaction") {
		t.Fatalf("caller failure path should run:\n%s", out)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)
	for _, want := range []string{"DOLT_RESET", "DOLT_COMMIT", "DOLT_GC"} {
		if !strings.Contains(log, want) {
			t.Fatalf("local compaction should still complete; missing %s:\n%s", want, log)
		}
	}
	if got := strings.Count(log, compactPushMainCall); got != 1 {
		t.Fatalf("push should be attempted exactly once, got %d:\n%s", got, log)
	}
	marker := compactPendingPushMarkerPath(fixture)
	if reason := compactMarkerValue(t, marker, "reason"); reason != "remote push reported success but remote HEAD did not move" {
		t.Fatalf("marker reason = %q", reason)
	}
	assertCompactMarkerHasEvidence(t, marker,
		"remote=origin",
		"expected_remote_head=headcommit",
		"expected_remote_head_verified=1",
		"compacted_from_head=headcommit",
		"local_branch=main",
		"remote_branch=main",
	)
}

// TestCompactScriptRetryKeepsPendingPushWhenRemoteTipDoesNotMove covers the
// same shape on the pending-push retry path, then proves the kept marker still
// drives a successful retry once the push really lands.
func TestCompactScriptRetryKeepsPendingPushWhenRemoteTipDoesNotMove(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "remote_push_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("initial compact should defer the push: %v\n%s", err, firstOut)
	}
	marker := compactPendingPushMarkerPath(fixture)
	createdAt := compactMarkerValue(t, marker, "created_at")

	secondOut, err := fixture.run(t, "remote_push_tip_unmoved", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("retry whose remote tip did not move must fail the run:\n%s", secondOut)
	}
	if !strings.Contains(secondOut, "pending_push=present") ||
		!strings.Contains(secondOut, "remote HEAD=headcommit does not match local HEAD=compactcommit") {
		t.Fatalf("retry should log both hashes:\n%s", secondOut)
	}
	if got := compactMarkerValue(t, marker, "created_at"); got != createdAt {
		t.Fatalf("unverified retry must preserve marker age: before=%s after=%s", createdAt, got)
	}

	thirdOut, err := fixture.run(t, "remote_success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("retry should succeed once the push lands: %v\n%s", err, thirdOut)
	}
	if !strings.Contains(thirdOut, "pushed compacted main remote_head=compactcommit") {
		t.Fatalf("landed retry should report the verified tip:\n%s", thirdOut)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("landed retry should clear marker (stat err=%v)", err)
	}
}

// assertCompactStaleMarkerAlertedOnce is the stranded-pending-push twin of
// assertCompactBeadsQuarantineAlert. The strand gate pages the operator exactly
// once, but under the shared marker notification cadence (upstream #5624) the
// dolt.compact.quarantine EVENT still fires on every cycle the marker is
// stranded, so the event count is asserted separately from the mail count.
func assertCompactStaleMarkerAlertedOnce(t *testing.T, fixture compactScriptFixture, recipient, markerPath, markerType, reason string, wantEvents int) {
	t.Helper()
	log := readCompactGCLog(t, fixture)
	mailLines := compactGCLogLinesWithPrefix(log, "gc mail send ")
	if len(mailLines) != 1 {
		t.Fatalf("a stranded pending-push marker should page the operator exactly once, got %d mail(s)\nlog:\n%s", len(mailLines), log)
	}
	eventLines := compactGCLogLinesWithPrefix(log, "gc event emit dolt.compact.quarantine")
	if len(eventLines) != wantEvents {
		t.Fatalf("stranded marker should emit one dolt.compact.quarantine event per cycle, want %d got %d\nlog:\n%s", wantEvents, len(eventLines), log)
	}
	for _, want := range []string{recipient, "beads", markerPath, markerType, reason, "--from controller"} {
		if !strings.Contains(mailLines[0], want) {
			t.Fatalf("mail alert line missing %q\nline:\n%s\nlog:\n%s", want, mailLines[0], log)
		}
	}
	for _, want := range []string{"beads", markerPath, markerType, reason, "--actor controller"} {
		if !strings.Contains(eventLines[0], want) {
			t.Fatalf("event alert line missing %q\nline:\n%s\nlog:\n%s", want, eventLines[0], log)
		}
	}
}

// TestCompactScriptStalePendingPushMarkerGetsOneAutomaticRetry pins the
// self-heal half of the strand gate: a marker past the age gate whose retry
// lands is cleared together with its bookkeeping sidecar, and nobody is paged.
func TestCompactScriptStalePendingPushMarkerGetsOneAutomaticRetry(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "remote_push_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("initial compact should defer the push: %v\n%s", err, firstOut)
	}
	marker := compactPendingPushMarkerPath(fixture)
	replaceCompactMarkerCreatedAt(t, marker, "1970-01-01T00:00:00Z")
	resetCompactGCLog(t, fixture)

	secondOut, err := fixture.run(t, "remote_success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("stale marker should get one automatic retry and self-heal: %v\n%s", err, secondOut)
	}
	if !strings.Contains(secondOut, "attempting one automatic remote push retry") ||
		!strings.Contains(secondOut, "pushed compacted main") {
		t.Fatalf("retry should be granted and land:\n%s", secondOut)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("self-healed retry should clear marker (stat err=%v)", err)
	}
	if _, err := os.Stat(marker + ".retry-state"); !os.IsNotExist(err) {
		t.Fatalf("self-healed retry should clear the retry-state sidecar (stat err=%v)", err)
	}
	if log := readCompactGCLog(t, fixture); len(compactGCLogLinesWithPrefix(log, "gc mail send ")) != 0 {
		t.Fatalf("self-heal must not page the operator:\n%s", log)
	}
}

// TestCompactScriptStalePendingPushEscalatesOnceThenResetsWhenSidecarRemoved
// walks the full strand-gate lifecycle: failed automatic retry, exactly one
// alert, silence, then the operator removes the sidecar and the gate grants one
// more automatic retry, which lands and clears everything.
func TestCompactScriptStalePendingPushEscalatesOnceThenResetsWhenSidecarRemoved(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	firstOut, err := fixture.run(t, "remote_push_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("initial compact should defer the push: %v\n%s", err, firstOut)
	}
	marker := compactPendingPushMarkerPath(fixture)
	sidecar := marker + ".retry-state"
	replaceCompactMarkerCreatedAt(t, marker, "1970-01-01T00:00:00Z")
	resetCompactGCLog(t, fixture)

	// Cycle 1 past the gate: the one automatic retry, which fails again.
	out, err := fixture.run(t, "remote_push_failure", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("automatic retry should defer on push failure: %v\n%s", err, out)
	}
	if got := compactMarkerValue(t, sidecar, "stale_retry_attempted"); got != "1" {
		t.Fatalf("sidecar should record the consumed retry, got stale_retry_attempted=%q", got)
	}
	if got := compactMarkerValue(t, sidecar, "marker_created_at"); got != "1970-01-01T00:00:00Z" {
		t.Fatalf("sidecar should be bound to the marker's created_at, got %q", got)
	}

	// Cycle 2: escalate exactly once.
	out, err = fixture.run(t, "remote_success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("stranded marker must fail the run:\n%s", out)
	}
	assertCompactStaleMarkerAlertedOnce(t, fixture, "mayor", marker, "compact-pending-push", "pending_push marker is stale", 1)
	if got := compactMarkerValue(t, marker, "notify_count"); got != "1" {
		t.Fatalf("marker should record the single escalation, got notify_count=%q", got)
	}
	if got := compactMarkerValue(t, marker, "last_notified_reason"); got != "pending_push marker is stale" {
		t.Fatalf("marker should record the escalated reason, got %q", got)
	}

	// Cycle 3: silent.
	out, err = fixture.run(t, "remote_success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("stranded marker must keep failing the run:\n%s", out)
	}
	if !strings.Contains(out, "alert already sent") {
		t.Fatalf("later cycle should explain it is not re-alerting:\n%s", out)
	}
	assertCompactStaleMarkerAlertedOnce(t, fixture, "mayor", marker, "compact-pending-push", "pending_push marker is stale", 2)
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if got := strings.Count(string(data), compactPushMainCall); got != 2 {
		t.Fatalf("stranded marker must not be pushed again (want initial + one automatic retry = 2), got %d:\n%s", got, data)
	}

	// Operator removes the sidecar: the gate grants one more automatic retry.
	if err := os.Remove(sidecar); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}
	out, err = fixture.run(t, "remote_success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("removing the sidecar should grant another automatic retry: %v\n%s", err, out)
	}
	if !strings.Contains(out, "attempting one automatic remote push retry") ||
		!strings.Contains(out, "pushed compacted main") {
		t.Fatalf("reset retry should be granted and land:\n%s", out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("landed retry should clear marker (stat err=%v)", err)
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Fatalf("landed retry should clear the sidecar (stat err=%v)", err)
	}
}
