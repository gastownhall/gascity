package beads

import (
	"os"
	"path/filepath"
	"testing"
)

func makeDoltRepo(t *testing.T, scopeRoot, sub, database string) string {
	t.Helper()
	path := filepath.Join(scopeRoot, ".beads", sub, database)
	if err := os.MkdirAll(filepath.Join(path, ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBeadDatabaseDirForDoltModeIsTheSharedFact keeps the warning gc prints
// when it re-points a workspace pointing at the directory that mode actually
// selects. Two answers to "which database does this mode use" would let gc warn
// about one path while the scope reads another.
func TestBeadDatabaseDirForDoltModeIsTheSharedFact(t *testing.T) {
	scope := t.TempDir()
	embedded := makeDoltRepo(t, scope, "embeddeddolt", "jc")

	if got, ok := BeadDatabaseDirForDoltMode(scope, "embedded", "jc"); !ok || got != embedded {
		t.Fatalf("BeadDatabaseDirForDoltMode(embedded) = (%q, %v), want (%q, true)", got, ok, embedded)
	}
	if got, ok := BeadDatabaseDirForDoltMode(scope, "local", "jc"); !ok || got != embedded {
		t.Fatalf("BeadDatabaseDirForDoltMode(local) = (%q, %v), want (%q, true): bd writes both spellings", got, ok, embedded)
	}
	if got, ok := BeadDatabaseDirForDoltMode(scope, "server", "jc"); ok {
		t.Fatalf("BeadDatabaseDirForDoltMode(server) = %q, but no server database is there", got)
	}

	server := makeDoltRepo(t, scope, "dolt", "jc")
	if got, ok := BeadDatabaseDirForDoltMode(scope, "server", "jc"); !ok || got != server {
		t.Fatalf("BeadDatabaseDirForDoltMode(server) = (%q, %v), want (%q, true)", got, ok, server)
	}
}

// TestBeadDatabaseDirForDoltModeReportsAbsentForEveryUndecidableShape is the
// false-positive budget. Every negative here is a scope some real deployment
// has, and a caller that announced a database for one of them would be naming a
// directory holding nothing — which is how a warning stops being read.
func TestBeadDatabaseDirForDoltModeReportsAbsentForEveryUndecidableShape(t *testing.T) {
	scope := t.TempDir()
	makeDoltRepo(t, scope, "embeddeddolt", "jc")

	for name, probe := range map[string]struct{ root, mode, database string }{
		"no scope root at all":            {"", "embedded", "jc"},
		"a mode gc cannot classify":       {scope, "sqlite", "jc"},
		"no mode at all":                  {scope, "", "jc"},
		"no database name":                {scope, "embedded", ""},
		"a database name that is a path":  {scope, "embedded", "../jc"},
		"a different database name":       {scope, "embedded", "other"},
		"the mode whose database is gone": {scope, "server", "jc"},
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := BeadDatabaseDirForDoltMode(probe.root, probe.mode, probe.database); ok {
				t.Fatalf("BeadDatabaseDirForDoltMode(%q, %q, %q) = %q, want absent", probe.root, probe.mode, probe.database, got)
			}
		})
	}

	t.Run("a directory with no dolt repository", func(t *testing.T) {
		bare := t.TempDir()
		if err := os.MkdirAll(filepath.Join(bare, ".beads", "embeddeddolt", "jc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got, ok := BeadDatabaseDirForDoltMode(bare, "embedded", "jc"); ok {
			t.Fatalf("BeadDatabaseDirForDoltMode reported %q; an empty parent directory is not a database", got)
		}
	})
}
