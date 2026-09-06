//go:build unix

package sqlite

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestReadLegacySessionsSnapshotRefusesSourceChangeAfterPrivateCopy(t *testing.T) {
	city := t.TempDir()
	path := filepath.Join(city, ".gc", "store", "sessions.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version=1; CREATE TABLE sessions (
		id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', bead_type TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'open', assignee TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, labels TEXT NOT NULL DEFAULT '[]', meta TEXT NOT NULL DEFAULT '{}', state TEXT NOT NULL DEFAULT '', session_name TEXT NOT NULL DEFAULT '', configured_named_identity TEXT NOT NULL DEFAULT '', pool_slot TEXT NOT NULL DEFAULT '', generation TEXT NOT NULL DEFAULT '', instance_token TEXT NOT NULL DEFAULT '', pending_create_claim TEXT NOT NULL DEFAULT '', pending_create_started_at TEXT NOT NULL DEFAULT '', last_woke_at TEXT NOT NULL DEFAULT '');`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	legacySessionsSnapshotAfterCopy = func() { _ = os.WriteFile(path+"-wal", []byte("new authoritative bytes"), 0o600) }
	t.Cleanup(func() { legacySessionsSnapshotAfterCopy = func() {} })
	if _, err := ReadLegacySessionsSnapshot(city); !errors.Is(err, errLegacySessionsSnapshotChanged) {
		t.Fatalf("ReadLegacySessionsSnapshot source change error = %v, want %v", err, errLegacySessionsSnapshotChanged)
	}
}
