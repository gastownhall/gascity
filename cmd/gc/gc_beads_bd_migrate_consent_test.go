package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGcBeadsBdInitMigratesOnlyADatabaseItBootstraps pins whose consent init's
// `bd migrate schema` carries. The verb is bd's consent to a one-way schema
// change for every client of the database (gastownhall/beads#5920 on a shared
// server, #5764 for every existing database), so init runs it only on a
// database it is bootstrapping: one this init created, or an interrupted
// bootstrap this init just reset. Any other database that was already on the
// server keeps its schema, whatever it holds and however far it is from the
// pinned bd's schema (whether it is behind, current or ahead does not change
// what init does; gc start, gc reload and gc doctor name the gap, see
// bd_schema_gap.go), and init still reports the scope ready instead of
// failing. Creation needs proof: a database the server did not list, whose
// backing directory was absent, and which held nothing bd wrote right after
// the CREATE. Bootstrap authority cannot come from the environment. The
// remote-backed case is TestGcBeadsBdInitWarnsOnlyOnBdsRemoteMigrateRefusal.
func TestGcBeadsBdInitMigratesOnlyADatabaseItBootstraps(t *testing.T) {
	for _, tc := range []struct {
		name      string
		issueRows int
		markers   []string
		env       []string
		// onDisk creates the database's backing directory under the data
		// directory before init runs.
		onDisk      bool
		wantReset   bool
		wantMigrate bool
		// wantLock: the init lock was taken, and was released again by the
		// time init wrote the issue prefix. Init takes it only where the
		// host has flock, so such a row needs it.
		wantLock bool
	}{
		{
			name:        "new database this init creates",
			markers:     []string{"database-absent", "init-done", "reset-done"},
			wantMigrate: true,
			wantLock:    true,
		},
		{
			name:        "interrupted bootstrap this init resets",
			markers:     []string{"init-done"},
			wantReset:   true,
			wantMigrate: true,
			wantLock:    true,
		},
		{
			name:    "existing database, clean, no issues",
			markers: []string{"init-done", "reset-done"},
		},
		{
			name:      "existing database with issues",
			issueRows: 3,
			markers:   []string{"init-done", "reset-done"},
		},
		{
			name:      "existing database with issues and uncommitted writes",
			issueRows: 3,
			markers:   []string{"init-done"},
		},
		{
			name:     "existing database whose bootstrap signature cleared once the lock was held",
			markers:  []string{"init-done", "status-dirty-once"},
			wantLock: true,
		},
		{
			name:    "existing database the server did not list, with its directory on disk",
			markers: []string{"database-absent", "init-done", "reset-done"},
			onDisk:  true,
		},
		{
			name:    "existing database that read as absent, holding bd's tables",
			markers: []string{"database-absent", "tables-present", "init-done", "reset-done"},
		},
		{
			name:    "existing database, bootstrap authority in the environment",
			markers: []string{"init-done", "reset-done"},
			env:     []string{"INIT_CREATED_DATABASE=true", "INIT_HEALED_BOOTSTRAP=true", "GC_DATABASE_CREATED_BY_ENSURE=true", "GC_STORE_PREEXISTED=false"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantLock {
				requireFlockForTest(t)
			}
			markers := tc.markers
			if tc.wantLock {
				// The probe takes the lock itself, creating its file, so only
				// a row that expects the lock file anyway may ask for it.
				markers = append([]string{"probe-init-lock"}, markers...)
			}
			h := newGcBeadsBdHealTest(t, true, tc.issueRows, markers...)
			if tc.onDisk {
				if err := os.MkdirAll(filepath.Join(h.cityPath, ".beads", "dolt", "hq", ".dolt"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			lockDir := t.TempDir()

			out, err := h.runInit(t, append([]string{"GC_DOLT_INIT_LOCK_DIR=" + lockDir}, tc.env...)...)
			if err != nil {
				t.Fatalf("gc-beads-bd init failed: %v\n%s", err, out)
			}
			sql, bdArgs := h.logs(t)
			if reset := strings.Contains(sql, "DOLT_RESET"); reset != tc.wantReset {
				t.Fatalf("DOLT_RESET ran = %v, want %v:\n%s\noutput:\n%s", reset, tc.wantReset, sql, out)
			}
			if migrated := strings.Contains(bdArgs, "migrate"); migrated != tc.wantMigrate {
				t.Fatalf("bd migrate ran = %v, want %v; bd was invoked with:\n%s\noutput:\n%s", migrated, tc.wantMigrate, bdArgs, out)
			}
			_, statErr := os.Stat(filepath.Join(lockDir, "hq.lock"))
			if locked := statErr == nil; locked != tc.wantLock {
				t.Fatalf("init lock taken = %v, want %v (stat err = %v)\noutput:\n%s", locked, tc.wantLock, statErr, out)
			}
			if !tc.wantLock {
				return
			}
			markerDir := filepath.Dir(h.sqlLog)
			if _, err := os.Stat(filepath.Join(markerDir, "init-lock-held")); err == nil {
				t.Fatalf("init still held its init lock when it wrote the issue prefix:\n%s", out)
			}
			if _, err := os.Stat(filepath.Join(markerDir, "init-lock-free")); err != nil {
				t.Fatalf("the issue_prefix write never probed the init lock: %v\noutput:\n%s", err, out)
			}
		})
	}
}
