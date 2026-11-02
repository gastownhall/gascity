package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeNoSyncMarker marks a database as excluded from remote sync, matching the
// contract the sync and pull commands already honor
// (commands/sync/run.sh, commands/pull/run.sh: "$data_dir/<db>/.no-sync").
func writeNoSyncMarker(t *testing.T, dataDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dataDir, "beads", ".no-sync"), nil, 0o644); err != nil {
		t.Fatalf("write .no-sync marker: %v", err)
	}
}

// TestCompactScriptSkipsRemotePhaseForNoSyncDatabase asserts that a database
// marked .no-sync gets local compaction only: no fetch, no push, and no
// pending-push marker. A .no-sync database has no push step to defer, so
// quarantining it on a failed push is a self-inflicted block.
func TestCompactScriptSkipsRemotePhaseForNoSyncDatabase(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	writeNoSyncMarker(t, fixture.dataDir)

	out, err := fixture.run(t, "remote_fetch_failure", "GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact should succeed locally for a .no-sync database: %v\n%s", err, out)
	}

	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)

	for _, want := range []string{"DOLT_RESET", "DOLT_COMMIT", "DOLT_GC"} {
		if !strings.Contains(log, want) {
			t.Fatalf(".no-sync must not block local compaction; missing %s:\n%s", want, log)
		}
	}
	if strings.Contains(log, "DOLT_FETCH") {
		t.Fatalf(".no-sync database must not be fetched:\n%s", log)
	}
	if strings.Contains(log, "DOLT_PUSH") {
		t.Fatalf(".no-sync database must not be pushed:\n%s", log)
	}

	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		markerData, _ := os.ReadFile(marker)
		t.Fatalf(".no-sync database must not get a pending-push marker (stat err=%v):\n%s", err, markerData)
	}
}

// TestCompactScriptClearsPendingPushMarkerWhenDatabaseBecomesNoSync asserts that
// an already-quarantined database recovers once it is marked .no-sync. Without
// this, the marker written before the .no-sync marker existed blocks flatten
// forever: the pending-push branch returns before the flatten path is reached,
// and past the max-age window the retry is refused as stale.
func TestCompactScriptClearsPendingPushMarkerWhenDatabaseBecomesNoSync(t *testing.T) {
	fixture := newCompactScriptFixture(t)

	firstOut, err := fixture.run(t, "remote_fetch_failure", "GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("initial compact should defer the push: %v\n%s", err, firstOut)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("fetch failure should write pending-push marker: %v", err)
	}

	// Operator marks the database no-sync: the deferred push is now meaningless.
	writeNoSyncMarker(t, fixture.dataDir)
	// Age the marker past the retry window, matching the production markers that
	// have been blocking compaction for weeks.
	replaceCompactMarkerCreatedAt(t, marker, "2026-01-01T00:00:00Z")

	secondOut, err := fixture.run(t, "remote_fetch_failure", "GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact must not stay blocked on a stale push marker for a .no-sync database: %v\n%s", err, secondOut)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf(".no-sync database must have its pending-push marker cleared (stat err=%v)\n%s", err, secondOut)
	}
	if strings.Contains(secondOut, "manual review required") {
		t.Fatalf(".no-sync database must not demand manual review for a push it will never make:\n%s", secondOut)
	}
}

// TestCompactScriptDryRunPreservesPendingPushMarkerForNoSyncDatabase pins that
// the .no-sync recovery obeys --dry-run: it reports the clear it would make and
// leaves the marker on disk.
func TestCompactScriptDryRunPreservesPendingPushMarkerForNoSyncDatabase(t *testing.T) {
	fixture := newCompactScriptFixture(t)

	firstOut, err := fixture.run(t, "remote_fetch_failure", "GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("initial compact should defer the push: %v\n%s", err, firstOut)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("fetch failure should write pending-push marker: %v", err)
	}
	writeNoSyncMarker(t, fixture.dataDir)

	out, err := fixture.run(t, "remote_fetch_failure", "GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500", "GC_DOLT_COMPACT_DRY_RUN=1")
	if err != nil {
		t.Fatalf("dry run should succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "dry-run (would clear deferred push)") {
		t.Fatalf("dry run should report the clear it would make:\n%s", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("dry run must not delete the pending-push marker: %v", err)
	}
}

// TestCompactScriptPendingPushMarkerBlocksFlattenEvenWhenFresh is a disproof
// artifact, not a bug report: it pins the fact that a pending-push marker blocks
// the flatten path by PRESENCE, not by staleness. It documents why "age the
// stale marker out" is not by itself a fix — a fresh marker still yields a
// push-retry-only run with no flatten.
func TestCompactScriptPendingPushMarkerBlocksFlattenEvenWhenFresh(t *testing.T) {
	fixture := newCompactScriptFixture(t)

	firstOut, err := fixture.run(t, "remote_fetch_failure", "GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("initial compact should defer the push: %v\n%s", err, firstOut)
	}
	marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("fetch failure should write pending-push marker: %v", err)
	}
	if err := os.Truncate(fixture.doltLog, 0); err != nil {
		t.Fatalf("truncate dolt log: %v", err)
	}

	// Marker is well inside the retry window — staleness cannot be the blocker.
	replaceCompactMarkerCreatedAt(t, marker, nowStampForCompactMarker(t))

	secondOut, err := fixture.run(t, "remote_fetch_failure", "GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("fresh-marker retry should not hard-fail: %v\n%s", err, secondOut)
	}
	if strings.Contains(secondOut, "marker is stale") {
		t.Fatalf("marker should be fresh for this disproof:\n%s", secondOut)
	}
	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	if strings.Contains(string(data), "DOLT_RESET") {
		t.Fatalf("HYPOTHESIS DISPROVED DIFFERENTLY: a fresh pending-push marker DID allow flatten:\n%s", data)
	}
}

func nowStampForCompactMarker(t *testing.T) string {
	t.Helper()
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}
