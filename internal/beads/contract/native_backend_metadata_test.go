package contract

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestLoadMetadataStateNativePostgres(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	if err := os.WriteFile(path, []byte(`{
  "backend": "postgres",
  "postgres_dsn": "postgres://operator@db.example.com:5432/beads?sslmode=require",
  "postgres_schema": "hq"
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state, ok, err := LoadMetadataState(fsys.OSFS{}, path)
	if err != nil || !ok {
		t.Fatalf("LoadMetadataState ok=%v err=%v", ok, err)
	}
	if state.PostgresSchema != "hq" || state.PostgresDSN == "" {
		t.Fatalf("state = %+v", state)
	}
}

func TestLoadMetadataStateNativeSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.json")
	if err := os.WriteFile(path, []byte(`{"backend":"sqlite","sqlite_path":"beads.db"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state, ok, err := LoadMetadataState(fsys.OSFS{}, path)
	if err != nil || !ok {
		t.Fatalf("LoadMetadataState ok=%v err=%v", ok, err)
	}
	if state.SQLitePath != "beads.db" {
		t.Fatalf("state = %+v", state)
	}
}
