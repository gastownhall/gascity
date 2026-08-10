package beads

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeOrphanScopeMetadata(t *testing.T, scopeRoot string, fields map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "metadata.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func makeDoltRepo(t *testing.T, scopeRoot, sub, database string) string {
	t.Helper()
	path := filepath.Join(scopeRoot, ".beads", sub, database)
	if err := os.MkdirAll(filepath.Join(path, ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestOrphanedBeadDatabaseReportsOnlyADecidableDisagreement is the false-positive
// budget for the guard, written as the list of shapes it must stay quiet about.
//
// Every negative here is a scope some real deployment has. A guard that fired on
// any of them would turn an empty ledger into a hard error on cities that are
// working correctly, which is a worse failure than the one being fixed — the
// silent empty at least answers.
func TestOrphanedBeadDatabaseReportsOnlyADecidableDisagreement(t *testing.T) {
	t.Run("server metadata with an embedded database left behind", func(t *testing.T) {
		scope := t.TempDir()
		want := makeDoltRepo(t, scope, "embeddeddolt", "jc")
		writeOrphanScopeMetadata(t, scope, map[string]string{
			"database": "dolt", "backend": "dolt", "dolt_mode": "server", "dolt_database": "jc",
		})
		orphan, active, ok := OrphanedBeadDatabase(scope)
		if !ok || orphan != want || active != "dolt" {
			t.Fatalf("OrphanedBeadDatabase = (%q, %q, %v), want (%q, %q, true)", orphan, active, ok, want, "dolt")
		}
	})

	t.Run("embedded metadata with a server database left behind", func(t *testing.T) {
		scope := t.TempDir()
		want := makeDoltRepo(t, scope, "dolt", "jc")
		writeOrphanScopeMetadata(t, scope, map[string]string{
			"database": "dolt", "backend": "dolt", "dolt_mode": "embedded", "dolt_database": "jc",
		})
		orphan, active, ok := OrphanedBeadDatabase(scope)
		if !ok || orphan != want || active != "embeddeddolt" {
			t.Fatalf("OrphanedBeadDatabase = (%q, %q, %v), want (%q, %q, true)", orphan, active, ok, want, "embeddeddolt")
		}
	})

	for name, build := range map[string]func(t *testing.T) string{
		"no .beads at all": func(t *testing.T) string { return t.TempDir() },
		// A store with no directory must never resolve ".beads" against the
		// process working directory and answer about whatever city that is.
		"no scope root at all": func(_ *testing.T) string { return "" },
		"metadata pointing at the database it has": func(t *testing.T) string {
			scope := t.TempDir()
			makeDoltRepo(t, scope, "embeddeddolt", "jc")
			writeOrphanScopeMetadata(t, scope, map[string]string{
				"database": "dolt", "backend": "dolt", "dolt_mode": "embedded", "dolt_database": "jc",
			})
			return scope
		},
		"a different database name": func(t *testing.T) string {
			scope := t.TempDir()
			makeDoltRepo(t, scope, "embeddeddolt", "other")
			writeOrphanScopeMetadata(t, scope, map[string]string{
				"database": "dolt", "backend": "dolt", "dolt_mode": "server", "dolt_database": "jc",
			})
			return scope
		},
		"a directory with no dolt repository": func(t *testing.T) string {
			scope := t.TempDir()
			if err := os.MkdirAll(filepath.Join(scope, ".beads", "embeddeddolt", "jc"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeOrphanScopeMetadata(t, scope, map[string]string{
				"database": "dolt", "backend": "dolt", "dolt_mode": "server", "dolt_database": "jc",
			})
			return scope
		},
		"a non-dolt backend": func(t *testing.T) string {
			scope := t.TempDir()
			makeDoltRepo(t, scope, "embeddeddolt", "jc")
			writeOrphanScopeMetadata(t, scope, map[string]string{
				"database": "sqlite", "backend": "sqlite", "dolt_mode": "server", "dolt_database": "jc",
			})
			return scope
		},
		"no recorded mode": func(t *testing.T) string {
			scope := t.TempDir()
			makeDoltRepo(t, scope, "embeddeddolt", "jc")
			writeOrphanScopeMetadata(t, scope, map[string]string{
				"database": "dolt", "backend": "dolt", "dolt_database": "jc",
			})
			return scope
		},
		"no recorded database": func(t *testing.T) string {
			scope := t.TempDir()
			makeDoltRepo(t, scope, "embeddeddolt", "jc")
			writeOrphanScopeMetadata(t, scope, map[string]string{
				"database": "dolt", "backend": "dolt", "dolt_mode": "server",
			})
			return scope
		},
		"a database name that is a path": func(t *testing.T) string {
			scope := t.TempDir()
			makeDoltRepo(t, scope, "embeddeddolt", "jc")
			writeOrphanScopeMetadata(t, scope, map[string]string{
				"database": "dolt", "backend": "dolt", "dolt_mode": "server", "dolt_database": "../jc",
			})
			return scope
		},
		"malformed metadata": func(t *testing.T) string {
			scope := t.TempDir()
			makeDoltRepo(t, scope, "embeddeddolt", "jc")
			if err := os.WriteFile(filepath.Join(scope, ".beads", "metadata.json"), []byte("{not json"), 0o644); err != nil {
				t.Fatal(err)
			}
			return scope
		},
	} {
		t.Run(name, func(t *testing.T) {
			if orphan, active, ok := OrphanedBeadDatabase(build(t)); ok {
				t.Fatalf("OrphanedBeadDatabase reported %q orphaned (active %q); this shape is not a decidable disagreement", orphan, active)
			}
		})
	}
}

// TestOrphanedStoreRefusalNamesWhatAnOperatorNeeds pins the message, because a
// refusal that only says "no" moves the dead end instead of removing it. The
// reader has to learn which store answered, which one did not, and the two ways
// out — without the source in front of them.
func TestOrphanedStoreRefusalNamesWhatAnOperatorNeeds(t *testing.T) {
	err := OrphanedBeadStoreRefusal("bd ready", "/cities/demo", "/cities/demo/.beads/embeddeddolt/jc", "dolt")
	if !errors.Is(err, ErrOrphanedBeadStore) {
		t.Fatalf("refusal does not wrap ErrOrphanedBeadStore: %v", err)
	}
	for _, want := range []string{
		"bd ready",
		"/cities/demo",
		"/cities/demo/.beads/embeddeddolt/jc",
		`"dolt"`,
		"gc doctor",
		"bd import --dry-run",
		"indistinguishable from a real one",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
	// It must not claim rows were lost — that is not knowable from disk, and a
	// message that overstates gets ignored the next time it is right.
	for _, forbidden := range []string{"lost", "deleted", "corrupt"} {
		if strings.Contains(strings.ToLower(err.Error()), forbidden) {
			t.Errorf("refusal claims %q, which the on-disk evidence does not support: %v", forbidden, err)
		}
	}
}

// TestBeadDatabaseDirForDoltModeIsTheSharedFact keeps the warning gc prints
// when it re-points a workspace and the refusal a later read returns pointing
// at the same directory. Two answers to "which database does this mode use"
// would let gc warn about one path and refuse over another.
func TestBeadDatabaseDirForDoltModeIsTheSharedFact(t *testing.T) {
	scope := t.TempDir()
	embedded := makeDoltRepo(t, scope, "embeddeddolt", "jc")

	if got, ok := BeadDatabaseDirForDoltMode(scope, "embedded", "jc"); !ok || got != embedded {
		t.Fatalf("BeadDatabaseDirForDoltMode(embedded) = (%q, %v), want (%q, true)", got, ok, embedded)
	}
	if got, ok := BeadDatabaseDirForDoltMode(scope, "local", "jc"); !ok || got != embedded {
		t.Fatalf("BeadDatabaseDirForDoltMode(local) = (%q, %v), want (%q, true): bd writes both spellings", got, ok, embedded)
	}
	if _, ok := BeadDatabaseDirForDoltMode(scope, "server", "jc"); ok {
		t.Fatal("BeadDatabaseDirForDoltMode(server) found a database that is not there")
	}
	for _, mode := range []string{"", "sqlite", "doltlite"} {
		if _, ok := BeadDatabaseDirForDoltMode(scope, mode, "jc"); ok {
			t.Errorf("BeadDatabaseDirForDoltMode(%q) claimed a directory for a mode it cannot classify", mode)
		}
	}

	writeOrphanScopeMetadata(t, scope, map[string]string{
		"database": "dolt", "backend": "dolt", "dolt_mode": "server", "dolt_database": "jc",
	})
	orphan, _, ok := OrphanedBeadDatabase(scope)
	if !ok || orphan != embedded {
		t.Fatalf("OrphanedBeadDatabase = (%q, %v), want the same %q BeadDatabaseDirForDoltMode reports", orphan, ok, embedded)
	}
}
