package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCommittedBeadsHandoffOwnsScopeProjection(t *testing.T) {
	city := t.TempDir()
	beadsDir := filepath.Join(city, ".beads")
	if err := os.Mkdir(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(beadsDir, "ownership-handoff.json")
	write := func(phase, owner string) {
		t.Helper()
		var journal handoffProjectionJournal
		journal.Request.CityRoot = city
		journal.Request.Root = city
		journal.Request.Database = "beads"
		journal.Request.Workspace = "test"
		journal.Request.Endpoint.Host = "127.0.0.1"
		journal.Request.Endpoint.Port = 3307
		journal.Request.Owner = "legacy-gc"
		journal.Phase, journal.Owner = phase, owner
		if phase == "committed" {
			journal.SnapshotCaptured = true
			journal.MutationOccurred = true
			journal.CommitHookRan = true
			journal.Snapshot.TargetPID = 42
			journal.Snapshot.TargetBirth = "birth"
			journal.Snapshot.TargetDataDir = filepath.Join(city, ".beads", "dolt")
			journal.Snapshot.Metadata = []byte(`{"schema_version":1,"operation":"handoff-inspect","result":"eligible","owner":"legacy-gc","identity_token":"token"}`)
		}
		body, err := json.Marshal(journal)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("committed", "bd")
	if got, err := committedBeadsHandoffOwnsScope(city); err != nil || !got {
		t.Fatalf("committed projection = %t, %v; want true, nil", got, err)
	}
	write("target_configured", "legacy-gc")
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("pending projection did not fail closed")
	}
	write("rolled_back", "legacy-gc")
	if got, err := committedBeadsHandoffOwnsScope(city); err != nil || got {
		t.Fatalf("restored rollback projection = %t, %v; want false, nil", got, err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("malformed projection did not fail closed")
	}
}

func TestCommittedBeadsHandoffRejectsIncompleteCheckpoint(t *testing.T) {
	city := t.TempDir()
	if err := os.Mkdir(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(city, ".beads", "ownership-handoff.json")
	if err := os.WriteFile(path, []byte(`{"request":{"city_root":"`+city+`","root":"`+city+`"},"phase":"committed","owner":"bd"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("truncated committed journal admitted")
	}
}
