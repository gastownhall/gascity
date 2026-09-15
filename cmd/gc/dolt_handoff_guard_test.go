package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func handoffGuardTestCity(t *testing.T) string {
	t.Helper()
	return handoffFixtureCity(t)
}

func TestHandoffJournalBlocksManagedDoltStart(t *testing.T) {
	city := handoffGuardTestCity(t)
	if err := os.Mkdir(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(city, ".beads", "ownership-handoff.json")
	for _, phase := range []string{"prepared", "target_configured", "old_owner_stopped", "verified", "committed"} {
		t.Run(phase, func(t *testing.T) {
			owner := "legacy-gc"
			if phase == "committed" {
				owner = "bd"
			}
			journal := baseHandoffJournal(city)
			journal.Phase, journal.Owner = phase, owner
			data, err := json.Marshal(journal)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := handoffJournalBlocksManagedDoltStart(city); err == nil {
				t.Fatal("handoff journal did not block managed start")
			}
		})
	}
}

func TestRecoverManagedDoltRefusesPendingHandoffBeforeLifecycleWork(t *testing.T) {
	city := handoffGuardTestCity(t)
	if err := os.Mkdir(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeHandoffJournal(t, city, pendingHandoffJournal(city, "old_owner_stopped"))
	called := false
	_, err := recoverManagedDoltProcessWithOps(city, "127.0.0.1", "3307", "root", "warning", time.Second, managedDoltRecoveryOps{
		queryProbe: func(string, string, string) error { called = true; return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "ownership handoff journal") {
		t.Fatalf("recover error = %v, want handoff refusal", err)
	}
	if called {
		t.Fatal("recovery probed or started despite pending handoff")
	}
}

func TestHandoffJournalAllowsManagedDoltStartOnlyWhenAbsent(t *testing.T) {
	city := handoffGuardTestCity(t)
	if err := handoffJournalBlocksManagedDoltStart(city); err != nil {
		t.Fatalf("absent journal blocked managed start: %v", err)
	}
	if err := os.Mkdir(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, ".beads", "ownership-handoff.json"), []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := handoffJournalBlocksManagedDoltStart(city); err == nil {
		t.Fatal("malformed journal did not fail closed")
	}
}

func TestCommittedBeadsHandoffLeavesFreshMissingScopeLegacy(t *testing.T) {
	fresh := filepath.Join(t.TempDir(), "rigs", "fresh")
	owned, err := committedBeadsHandoffOwnsScope(fresh)
	if err != nil || owned {
		t.Fatalf("missing fresh scope ownership = (%t, %v), want legacy", owned, err)
	}

	city := handoffGuardTestCity(t)
	if err := os.Symlink(filepath.Join(city, "outside-beads"), filepath.Join(city, ".beads")); err != nil {
		t.Fatal(err)
	}
	owned, err = committedBeadsHandoffOwnsScope(city)
	if err != nil || owned {
		t.Fatalf("legacy .beads symlink without journal ownership = (%t, %v), want legacy", owned, err)
	}
}

func TestHandoffJournalAllowsOnlyExactRestoredLegacyControls(t *testing.T) {
	city := handoffGuardTestCity(t)
	beadsDir := filepath.Join(city, ".beads")
	if err := os.Mkdir(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := []byte(`{"backend":"dolt","dolt_database":"beads"}`)
	config := []byte("gc.endpoint_origin: managed_city\ndolt.auto-start: false\n")
	port := []byte("3307")
	for _, file := range []struct {
		name string
		body []byte
	}{{"metadata.json", metadata}, {"config.yaml", config}, {"dolt-server.port", port}} {
		if err := os.WriteFile(filepath.Join(beadsDir, file.name), file.body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	journal := pendingHandoffJournal(city, "legacy_config_restored")
	journal.Snapshot.Metadata = handoffProjectionArtifact{Present: true, Data: metadata, Mode: 0o600}
	journal.Snapshot.Config = handoffProjectionArtifact{Present: true, Data: config, Mode: 0o600}
	journal.Snapshot.PortFile = handoffProjectionArtifact{Present: true, Data: port, Mode: 0o600}
	writeHandoffJournal(t, city, journal)
	if err := handoffJournalBlocksManagedDoltStart(city); err != nil {
		t.Fatalf("exact restored journal blocked start: %v", err)
	}

	// Drift AFTER the admission is gc's own canonicalisation on the restart the
	// rollback performs, and must not re-close the gate. See
	// admitRolledBackHandoff; TestRolledBackHandoffAdmissionSurvivesGCsOwnCanonicalRewrite
	// drives the same path through the projection.
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("dolt.auto-start: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := handoffJournalBlocksManagedDoltStart(city); err != nil {
		t.Fatalf("an admitted rollback blocked the managed start it had already allowed: %v", err)
	}
}

// A restored journal whose artifacts were never byte-exact has not proven the
// rollback completed, and never becomes an admission.
func TestHandoffJournalRefusesARestoreItNeverSawLandFirst(t *testing.T) {
	city := handoffGuardTestCity(t)
	beadsDir := filepath.Join(city, ".beads")
	if err := os.Mkdir(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := []byte(`{"backend":"dolt","dolt_database":"beads"}`)
	config := []byte("gc.endpoint_origin: managed_city\ndolt.auto-start: false\n")
	port := []byte("3307")
	for _, file := range []struct {
		name string
		body []byte
	}{{"metadata.json", metadata}, {"config.yaml", []byte("dolt.auto-start: true\n")}, {"dolt-server.port", port}} {
		if err := os.WriteFile(filepath.Join(beadsDir, file.name), file.body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	journal := pendingHandoffJournal(city, "legacy_config_restored")
	journal.Snapshot.Metadata = handoffProjectionArtifact{Present: true, Data: metadata, Mode: 0o600}
	journal.Snapshot.Config = handoffProjectionArtifact{Present: true, Data: config, Mode: 0o600}
	journal.Snapshot.PortFile = handoffProjectionArtifact{Present: true, Data: port, Mode: 0o600}
	writeHandoffJournal(t, city, journal)
	if err := handoffJournalBlocksManagedDoltStart(city); err == nil {
		t.Fatal("a restore that never landed admitted the managed start")
	}
}

func TestCommittedBeadsHandoffOwnsScopeOnlyAfterCommit(t *testing.T) {
	city := handoffGuardTestCity(t)
	beadsDir := filepath.Join(city, ".beads")
	if err := os.Mkdir(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeHandoffJournal(t, city, pendingHandoffJournal(city, "prepared"))
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("pending journal was admitted")
	}
	writeHandoffJournal(t, city, committedHandoffJournal(city))
	if got, err := committedBeadsHandoffOwnsScope(city); err != nil || !got {
		t.Fatalf("committed projection = %t, %v", got, err)
	}
}
