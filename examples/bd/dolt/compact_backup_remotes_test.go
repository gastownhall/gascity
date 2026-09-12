package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCompactScriptReconcilesFileBackupRemoteAlongsideOrigin asserts that a
// file:// backup remote gets fetched and pushed using the same
// push_remote_after_compaction protocol as the authoritative remote, right
// alongside the origin push during an announced compaction window.
func TestCompactScriptReconcilesFileBackupRemoteAlongsideOrigin(t *testing.T) {
	fixture := newCompactScriptFixture(t)

	out, err := fixture.run(t, "backup_remote_reconcile", "GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact should succeed with a reconcilable file:// backup remote: %v\n%s", err, out)
	}

	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)

	if !strings.Contains(log, "DOLT_FETCH('backup')") {
		t.Fatalf("file:// backup remote must be fetched during compaction:\n%s", log)
	}
	if !strings.Contains(log, "DOLT_PUSH('--force', '--set-upstream', 'backup', 'main')") {
		t.Fatalf("file:// backup remote must be pushed using the same protocol as origin:\n%s", log)
	}

	primaryMarker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	if _, err := os.Stat(primaryMarker); !os.IsNotExist(err) {
		t.Fatalf("origin push succeeded; must not leave a pending-push marker (stat err=%v)", err)
	}
	backupMarker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push-backup", "beads.backup")
	if _, err := os.Stat(backupMarker); !os.IsNotExist(err) {
		t.Fatalf("backup push succeeded; must not leave a backup pending-push marker (stat err=%v)", err)
	}
}

// TestCompactScriptIsolatesBackupPushFailureFromPrimaryPush asserts that a
// failed push to a file:// backup remote is deferred into its own
// compact-pending-push-backup marker, namespaced separately from the
// authoritative remote's compact-pending-push marker, and never blocks or
// fails the authoritative push.
func TestCompactScriptIsolatesBackupPushFailureFromPrimaryPush(t *testing.T) {
	fixture := newCompactScriptFixture(t)

	out, err := fixture.run(t, "backup_remote_push_failure", "GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("a failed backup push must not fail the overall compact run: %v\n%s", err, out)
	}

	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)

	if !strings.Contains(log, "DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')") {
		t.Fatalf("authoritative remote must still be pushed when the backup push fails:\n%s", log)
	}

	primaryMarker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push", "beads")
	if _, err := os.Stat(primaryMarker); !os.IsNotExist(err) {
		t.Fatalf("origin push succeeded; a failing backup push must not create a primary pending-push marker (stat err=%v)", err)
	}

	backupMarker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", "compact-pending-push-backup", "beads.backup")
	if _, err := os.Stat(backupMarker); err != nil {
		t.Fatalf("failed backup push must be deferred into a compact-pending-push-backup marker: %v\n%s", err, out)
	}
	if remote := compactMarkerValue(t, backupMarker, "remote"); remote != "backup" {
		t.Fatalf("backup marker must record remote=backup, got %q", remote)
	}
}

// TestCompactScriptExcludesNonFileRemotesAndAuthoritativeFromBackupReconciliation
// asserts that backup reconciliation only ever targets file:// remotes other
// than the authoritative remote: a non-file:// remote (e.g. a second hosted
// mirror) is never fetched or pushed as a backup, and the authoritative
// remote is never reconciled a second time against itself.
func TestCompactScriptExcludesNonFileRemotesAndAuthoritativeFromBackupReconciliation(t *testing.T) {
	fixture := newCompactScriptFixture(t)

	out, err := fixture.run(t, "backup_remote_filters_non_file_and_authoritative", "GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err != nil {
		t.Fatalf("compact should succeed with a mixed-scheme remote set: %v\n%s", err, out)
	}

	data, err := os.ReadFile(fixture.doltLog)
	if err != nil {
		t.Fatalf("read dolt log: %v", err)
	}
	log := string(data)

	if !strings.Contains(log, "DOLT_FETCH('backup')") {
		t.Fatalf("file:// backup remote must still be fetched:\n%s", log)
	}
	if !strings.Contains(log, "DOLT_PUSH('--force', '--set-upstream', 'backup', 'main')") {
		t.Fatalf("file:// backup remote must still be pushed:\n%s", log)
	}
	if strings.Contains(log, "mirror") {
		t.Fatalf("non-file:// remote must never be fetched or pushed as a backup:\n%s", log)
	}
	if got := strings.Count(log, "DOLT_PUSH('--force', '--set-upstream', 'origin', 'main')"); got != 1 {
		t.Fatalf("authoritative remote must be pushed exactly once (as primary, not reconciled again as a backup), got %d:\n%s", got, log)
	}
}
