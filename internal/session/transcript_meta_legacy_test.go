package session_test

import (
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	storebindingsqlite "github.com/gastownhall/gascity/internal/storebinding/sqlite"
	_ "modernc.org/sqlite"
)

// TestPersistedTranscriptMetaSnapshotReadsLegacySessionsDB keeps the historical
// supervisor pass compatible with cities that still have their authoritative
// session identities in .gc/store/sessions.db. The current store wins an ID
// collision: the legacy database is an additive recovery source, never a way
// to overwrite the active store's identity.
func TestPersistedTranscriptMetaSnapshotReadsLegacySessionsDB(t *testing.T) {
	city := t.TempDir()
	databasePath := legacyTranscriptMetaDatabase(t, city)
	legacyTranscriptMetaInsert(t, databasePath, "legacy-codex", "codex", "codex-key")
	legacyTranscriptMetaInsert(t, databasePath, "legacy-claude", "claude", "claude-key")
	legacyTranscriptMetaInsert(t, databasePath, "current", "legacy-overwrite", "wrong-key")

	before := legacyTranscriptMetaHash(t, databasePath)
	manager := session.NewManagerWithOptions(beads.NewMemStoreFrom(1, []beads.Bead{{
		ID: "current", Type: session.BeadType, Status: "closed", Labels: []string{session.LabelSession},
		Metadata: map[string]string{"provider": "current-provider", "session_key": "current-key", "work_dir": "/current"},
	}}, nil), runtime.NewFake(), session.WithCityPath(city))

	legacy, err := storebindingsqlite.ReadLegacySessionsSnapshot(city)
	if err != nil {
		t.Fatalf("ReadLegacySessionsSnapshot: %v", err)
	}
	infos, err := manager.PersistedTranscriptMetaSnapshotWithSupplemental(legacy)
	if err != nil {
		t.Fatalf("PersistedTranscriptMetaSnapshot: %v", err)
	}
	if got, want := transcriptMetaIDs(infos), []string{"current", "legacy-claude", "legacy-codex"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot IDs = %v, want %v", got, want)
	}
	byID := make(map[string]session.Info, len(infos))
	for _, info := range infos {
		byID[info.ID] = info
	}
	if got := byID["legacy-codex"]; got.Provider != "codex" || got.SessionKey != "codex-key" {
		t.Errorf("legacy Codex = %+v, want persisted exact key", got)
	}
	if got := byID["legacy-claude"]; got.Provider != "claude" || got.SessionKey != "claude-key" {
		t.Errorf("legacy Claude = %+v, want persisted exact key", got)
	}
	if got := byID["current"]; got.Provider != "current-provider" || got.SessionKey != "current-key" {
		t.Errorf("current collision row = %+v, want active-store row unchanged", got)
	}
	if after := legacyTranscriptMetaHash(t, databasePath); after != before {
		t.Fatalf("legacy sessions.db changed during snapshot: before=%x after=%x", before, after)
	}
}

func TestPersistedTranscriptMetaSnapshotLegacySessionsDBAbsentIsEmpty(t *testing.T) {
	city := t.TempDir()
	infos, err := storebindingsqlite.ReadLegacySessionsSnapshot(city)
	if err != nil {
		t.Fatalf("PersistedTranscriptMetaSnapshot with absent legacy db: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("snapshot with absent legacy db = %v, want empty", infos)
	}
}

func TestPersistedTranscriptMetaSnapshotRefusesLegacySessionsDBSidecarContent(t *testing.T) {
	for _, suffix := range []string{"-wal", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			city := t.TempDir()
			databasePath := legacyTranscriptMetaDatabase(t, city)
			legacyTranscriptMetaInsert(t, databasePath, "legacy", "codex", "key")
			if err := os.WriteFile(databasePath+suffix, []byte("authoritative sidecar bytes"), 0o600); err != nil {
				t.Fatalf("WriteFile(%s): %v", suffix, err)
			}
			if _, err := storebindingsqlite.ReadLegacySessionsSnapshot(city); err == nil {
				t.Fatalf("snapshot with nonzero %s succeeded, want refusal", suffix)
			}
		})
	}
}

func legacyTranscriptMetaDatabase(t *testing.T, city string) string {
	t.Helper()
	path := filepath.Join(city, ".gc", "store", "sessions.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll legacy store: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy sessions db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`PRAGMA user_version = 1; CREATE TABLE sessions (
		id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', bead_type TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'open', assignee TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
		labels TEXT NOT NULL DEFAULT '[]', meta TEXT NOT NULL DEFAULT '{}',
		state TEXT NOT NULL DEFAULT '', session_name TEXT NOT NULL DEFAULT '',
		configured_named_identity TEXT NOT NULL DEFAULT '', pool_slot TEXT NOT NULL DEFAULT '',
		generation TEXT NOT NULL DEFAULT '', instance_token TEXT NOT NULL DEFAULT '',
		pending_create_claim TEXT NOT NULL DEFAULT '', pending_create_started_at TEXT NOT NULL DEFAULT '',
		last_woke_at TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("create legacy sessions table: %v", err)
	}
	return path
}

func legacyTranscriptMetaInsert(t *testing.T, path, id, provider, key string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy sessions db: %v", err)
	}
	defer db.Close() //nolint:errcheck
	_, err = db.Exec(`INSERT INTO sessions (id,title,bead_type,status,assignee,description,created_at,updated_at,labels,meta)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, id, id, "session", "closed", "", "", time.Now().UnixNano(), time.Now().UnixNano(), `["gc:session"]`, `{"provider":"`+provider+`","session_key":"`+key+`","work_dir":"/legacy"}`)
	if err != nil {
		t.Fatalf("insert legacy session %s: %v", id, err)
	}
}

func legacyTranscriptMetaHash(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile legacy sessions db: %v", err)
	}
	return sha256.Sum256(contents)
}

func transcriptMetaIDs(infos []session.Info) []string {
	ids := make([]string, len(infos))
	for i, info := range infos {
		ids[i] = info.ID
	}
	return ids
}
