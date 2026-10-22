package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- DoltUnboundDatabasesCheck (#1979) ---

// writeCommittedDoltStore lays down a committed database store —
// <data_dir>/<db>/.dolt/noms/manifest — in a managed city's data dir.
func writeCommittedDoltStore(t *testing.T, cityDir, db string) {
	t.Helper()
	manifest := filepath.Join(cityDir, ".beads", "dolt", db, ".dolt", "noms", "manifest")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("noms manifest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writePhantomDoltStore lays down a phantom database — a directory with
// .dolt but no committed store — which no recovery flow may treat as live.
func writePhantomDoltStore(t *testing.T, cityDir, db string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads", "dolt", db, ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeRepoStateBackupsLegacyString registers one backup remote in the
// legacy plain-string form older dolt versions recorded
// ({"backups": {"<name>": "<url>"}}). writeRepoStateBackups (in
// checks_dolt_backup_test.go) covers the object form.
func writeRepoStateBackupsLegacyString(t *testing.T, cityDir, db, name, url string) {
	t.Helper()
	writeRepoState(t, cityDir, db, fmt.Sprintf("{\"backups\":{%q:%q}}", name, url))
}

func writeRepoState(t *testing.T, cityDir, db, state string) {
	t.Helper()
	statePath := filepath.Join(cityDir, ".beads", "dolt", db, ".dolt", "repo_state.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
}

// nonEmptyBackupDir returns a non-empty directory at <root>/<name>, shaped
// like a mol-dog-backup artifact dir a usable file:// backup points at.
func nonEmptyBackupDir(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backup.chunk"), []byte("chunk"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestDoltUnboundDatabasesCheck_WarnsNamingUnboundDatabase pins the warn
// half of #1979's unbound-database check: a committed store in the shared
// data dir that no scope is bound to and no usable backup covers is named;
// the bound database, phantoms and moved-aside stores never are.
func TestDoltUnboundDatabasesCheck_WarnsNamingUnboundDatabase(t *testing.T) {
	t.Run("unbound database with no backup", func(t *testing.T) {
		dir := setupManagedDoltCity(t)
		writeCommittedDoltStore(t, dir, "hq") // bound by the city scope
		writeCommittedDoltStore(t, dir, "pa") // unbound, no backup
		r := NewDoltUnboundDatabasesCheck(dir, false).Run(&CheckContext{})
		if r.Status != StatusWarning {
			t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "pa") {
			t.Errorf("message = %q, want it to name the unbound database pa", r.Message)
		}
		if strings.Contains(r.Message, "hq") {
			t.Errorf("message = %q, must not name the bound database hq", r.Message)
		}
	})

	t.Run("default backup naming another database is not a restore source", func(t *testing.T) {
		dir := setupManagedDoltCity(t)
		writeCommittedDoltStore(t, dir, "pa")
		other := nonEmptyBackupDir(t, t.TempDir(), "other")
		writeRepoStateBackups(t, filepath.Join(dir, ".beads", "dolt"), "pa", map[string]string{"default": "file://" + other})
		r := NewDoltUnboundDatabasesCheck(dir, false).Run(&CheckContext{})
		if r.Status != StatusWarning {
			t.Fatalf("status = %d, want Warning (default backup names another database); msg = %s", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "pa") {
			t.Errorf("message = %q, want it to name the unbound database pa", r.Message)
		}
	})

	t.Run("phantom and moved-aside stores are not named", func(t *testing.T) {
		dir := setupManagedDoltCity(t)
		writePhantomDoltStore(t, dir, "ghost")           // .dolt without a committed store
		writeCommittedDoltStore(t, dir, "pa.replaced-1") // dotted name: a moved-aside store
		r := NewDoltUnboundDatabasesCheck(dir, false).Run(&CheckContext{})
		if r.Status != StatusOK {
			t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
		}
		if strings.Contains(r.Message, "ghost") || strings.Contains(r.Message, "pa.replaced-1") {
			t.Errorf("message = %q, must not name phantom or moved-aside stores", r.Message)
		}
	})
}

// TestDoltUnboundDatabasesCheck_OKOnceUsableBackupRegistered pins the pass
// half: once the unbound database has a usable local file:// backup — bd's
// `default` destination naming the database, or the legacy <db>-backup
// name — the check passes.
func TestDoltUnboundDatabasesCheck_OKOnceUsableBackupRegistered(t *testing.T) {
	t.Run("bd default backup naming the database", func(t *testing.T) {
		dir := setupManagedDoltCity(t)
		writeCommittedDoltStore(t, dir, "pa")
		backupDir := nonEmptyBackupDir(t, t.TempDir(), "pa")
		writeRepoStateBackups(t, filepath.Join(dir, ".beads", "dolt"), "pa", map[string]string{"default": "file://" + backupDir})
		r := NewDoltUnboundDatabasesCheck(dir, false).Run(&CheckContext{})
		if r.Status != StatusOK {
			t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
		}
	})

	t.Run("legacy <db>-backup name", func(t *testing.T) {
		dir := setupManagedDoltCity(t)
		writeCommittedDoltStore(t, dir, "pa")
		backupDir := nonEmptyBackupDir(t, t.TempDir(), "prod-backup")
		writeRepoStateBackups(t, filepath.Join(dir, ".beads", "dolt"), "pa", map[string]string{"pa-backup": "file://" + backupDir})
		r := NewDoltUnboundDatabasesCheck(dir, false).Run(&CheckContext{})
		if r.Status != StatusOK {
			t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
		}
	})

	t.Run("legacy plain-string backups form", func(t *testing.T) {
		dir := setupManagedDoltCity(t)
		writeCommittedDoltStore(t, dir, "pa")
		backupDir := nonEmptyBackupDir(t, t.TempDir(), "pa")
		writeRepoStateBackupsLegacyString(t, dir, "pa", "pa-backup", "file://"+backupDir)
		r := NewDoltUnboundDatabasesCheck(dir, false).Run(&CheckContext{})
		if r.Status != StatusOK {
			t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
		}
	})
}

// TestDoltUnboundDatabasesCheck_SkipsWhenTargetsUnresolved: when a scope's
// binding cannot be resolved, the check must read as skipped — an
// incomplete bound set must never be reported as a flock of false orphans.
func TestDoltUnboundDatabasesCheck_SkipsWhenTargetsUnresolved(t *testing.T) {
	dir := setupManagedDoltCity(t)
	// A city config that cannot be canonically resolved — managed_city is
	// not allowed to track an endpoint — marks the target inventory
	// unresolved; which databases are bound is unknowable.
	if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte(
		"issue_prefix: gc\n"+
			"gc.endpoint_origin: managed_city\n"+
			"gc.endpoint_status: verified\n"+
			"dolt.host: 127.0.0.1\n"+
			"dolt.port: \"1234\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCommittedDoltStore(t, dir, "pa") // would be a false orphan
	r := NewDoltUnboundDatabasesCheck(dir, false).Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "skipped") {
		t.Errorf("message = %q, want skipped", r.Message)
	}
	if strings.Contains(r.Message, "pa") {
		t.Errorf("message = %q, must not name an orphan from an unresolved inventory", r.Message)
	}
}
