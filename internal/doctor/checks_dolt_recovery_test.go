package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
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

	// A directory two databases register holds whichever synced to it last,
	// so start-time recovery refuses it (backup_restore_refusal); hq
	// registering pa's directory under any name makes it no restore source
	// for pa, whichever of its two names pa registers it under.
	for _, name := range []string{"default", "pa-backup"} {
		t.Run("backup directory another database registers is not a restore source: "+name, func(t *testing.T) {
			dir := setupManagedDoltCity(t)
			writeCommittedDoltStore(t, dir, "hq")
			writeCommittedDoltStore(t, dir, "pa")
			dataDir := filepath.Join(dir, ".beads", "dolt")
			shared := "file://" + nonEmptyBackupDir(t, t.TempDir(), "pa")
			writeRepoStateBackups(t, dataDir, "pa", map[string]string{name: shared})
			writeRepoStateBackups(t, dataDir, "hq", map[string]string{"hq-backup": shared})
			r := NewDoltUnboundDatabasesCheck(dir, false).Run(&CheckContext{})
			if r.Status != StatusWarning {
				t.Fatalf("status = %d, want Warning (pa's backup directory is shared with hq); msg = %s", r.Status, r.Message)
			}
			if !strings.Contains(r.Message, "no usable local backup: pa") {
				t.Errorf("message = %q, want it to name pa as having no usable local backup", r.Message)
			}
		})
	}

	t.Run("backup URL that is not file:// is not a local backup", func(t *testing.T) {
		dir := setupManagedDoltCity(t)
		writeCommittedDoltStore(t, dir, "pa")
		bare := nonEmptyBackupDir(t, t.TempDir(), "pa")
		writeRepoStateBackups(t, filepath.Join(dir, ".beads", "dolt"), "pa", map[string]string{"pa-backup": bare})
		r := NewDoltUnboundDatabasesCheck(dir, false).Run(&CheckContext{})
		if r.Status != StatusWarning {
			t.Fatalf("status = %d, want Warning (a backup URL without file:// is not restored); msg = %s", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "no usable local backup: pa") {
			t.Errorf("message = %q, want it to name pa as having no usable local backup", r.Message)
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

	t.Run("another database registering its own directory", func(t *testing.T) {
		dir := setupManagedDoltCity(t)
		writeCommittedDoltStore(t, dir, "hq")
		writeCommittedDoltStore(t, dir, "pa")
		dataDir := filepath.Join(dir, ".beads", "dolt")
		backups := t.TempDir()
		writeRepoStateBackups(t, dataDir, "pa", map[string]string{"default": "file://" + nonEmptyBackupDir(t, backups, "pa")})
		writeRepoStateBackups(t, dataDir, "hq", map[string]string{"default": "file://" + nonEmptyBackupDir(t, backups, "hq")})
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

// TestDoltUnboundDatabasesCheck_WarnsWhenStoreCannotBeChecked: an orphan
// whose noms/manifest cannot be stat'ed for a reason other than not-exist
// may hold a committed store, so it is named instead of being dropped as a
// phantom.
func TestDoltUnboundDatabasesCheck_WarnsWhenStoreCannotBeChecked(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeCommittedDoltStore(t, dir, "hq")
	// noms is a file, so the stat of noms/manifest fails with ENOTDIR.
	noms := filepath.Join(dir, ".beads", "dolt", "pa", ".dolt", "noms")
	if err := os.MkdirAll(filepath.Dir(noms), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(noms, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewDoltUnboundDatabasesCheck(dir, false).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "whose store cannot be checked: pa (") {
		t.Errorf("message = %q, want it to name pa as a store that cannot be checked", r.Message)
	}
	if strings.Contains(r.Message, "hq") {
		t.Errorf("message = %q, must not name the bound database hq", r.Message)
	}
}

// TestDoltUnboundDatabasesCheck_WarnsWhenDataDirCannotBeListed: a data dir
// that cannot be listed hides every database in it, so the check warns
// instead of reading as skipped.
func TestDoltUnboundDatabasesCheck_WarnsWhenDataDirCannotBeListed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0000 does not block reads")
	}
	dir := setupManagedDoltCity(t)
	writeCommittedDoltStore(t, dir, "pa")
	dataDir := filepath.Join(dir, ".beads", "dolt")
	if err := os.Chmod(dataDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dataDir, 0o755) })
	r := NewDoltUnboundDatabasesCheck(dir, false).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "cannot list the managed dolt data dir "+dataDir) {
		t.Errorf("message = %q, want it to name the data dir that cannot be listed", r.Message)
	}
}

// seedUnrecoverableState puts an unbound committed store (pa) in the data
// dir and a preserved corrupt-aside entry (prod.20261022T010203Z) in the
// pack state dir: exactly the state a skipped run must not name.
func seedUnrecoverableState(t *testing.T, dir string) {
	t.Helper()
	writeCommittedDoltStore(t, dir, "pa")
	aside := filepath.Join(dir, ".gc", "runtime", "packs", "dolt", "corrupt-aside", "prod.20261022T010203Z")
	if err := os.MkdirAll(aside, 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestManagedDoltRecoveryChecks_SkipWithoutNamingAnyDatabase pins AC-7
// (#1979): on a bd-owned proxied city, an external-endpoint city, or
// GC_DOLT=skip, both recovery checks report OK as skipped and name no
// database — even with an unbound store and a corrupt-aside entry on disk.
func TestManagedDoltRecoveryChecks_SkipWithoutNamingAnyDatabase(t *testing.T) {
	fs := fsys.OSFS{}
	for _, tc := range []struct {
		name string
		skip bool
		set  func(t *testing.T) string
	}{
		{name: "GC_DOLT=skip", skip: true, set: func(t *testing.T) string {
			dir := setupManagedDoltCity(t)
			seedUnrecoverableState(t, dir)
			return dir
		}},
		{name: "external endpoint city", skip: false, set: func(t *testing.T) string {
			dir := setupCity(t, "[workspace]\nname = \"test\"\n")
			writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
				IssuePrefix:    "gc",
				EndpointOrigin: contract.EndpointOriginCityCanonical,
				EndpointStatus: contract.EndpointStatusVerified,
				DoltHost:       "db.example.com",
				DoltPort:       "4411",
			})
			writeDoctorCanonicalMetadata(t, fs, dir, "hq")
			seedUnrecoverableState(t, dir)
			return dir
		}},
		{name: "bd-owned proxied city", skip: false, set: func(t *testing.T) string {
			dir := t.TempDir()
			writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
				EndpointOrigin: contract.EndpointOriginManagedCity,
				DoltMode:       "proxied-server",
			})
			writeDoctorProxiedMetadata(t, dir, "hq")
			seedUnrecoverableState(t, dir)
			return dir
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.set(t)
			// gc doctor registers the ForConfig constructors with the
			// loaded city config; none of these cities declares a rig.
			cfg := &config.City{}
			for _, c := range []struct {
				via   string
				check Check
			}{
				{via: "raw", check: NewDoltUnboundDatabasesCheck(dir, tc.skip)},
				{via: "raw", check: NewDoltCorruptAsideCheck(dir, tc.skip)},
				{via: "ForConfig", check: NewDoltUnboundDatabasesCheckForConfig(dir, tc.skip, cfg, nil)},
				{via: "ForConfig", check: NewDoltCorruptAsideCheckForConfig(dir, tc.skip, cfg, nil)},
			} {
				r := c.check.Run(&CheckContext{})
				if r.Status != StatusOK {
					t.Fatalf("%s (%s): status = %d, want OK; msg = %s", c.check.Name(), c.via, r.Status, r.Message)
				}
				if !strings.Contains(r.Message, "skipped") {
					t.Errorf("%s (%s): message = %q, want skipped", c.check.Name(), c.via, r.Message)
				}
				for _, db := range []string{"pa", "hq", "prod"} {
					if strings.Contains(r.Message, db) {
						t.Errorf("%s (%s): message = %q, must not name %s in a skipped city", c.check.Name(), c.via, r.Message, db)
					}
				}
			}
		})
	}
}

// TestDoltCorruptAsideCheck_WarnsNamingPreservedStore pins AC-8 (#1979):
// while corrupt-aside/prod.20261022T010203Z/ exists, the check warns,
// naming the database, the timestamp and the aside path.
func TestDoltCorruptAsideCheck_WarnsNamingPreservedStore(t *testing.T) {
	dir := setupManagedDoltCity(t)
	aside := filepath.Join(dir, ".gc", "runtime", "packs", "dolt", "corrupt-aside", "prod.20261022T010203Z")
	if err := os.MkdirAll(aside, 0o755); err != nil {
		t.Fatal(err)
	}
	r := NewDoltCorruptAsideCheck(dir, false).Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "prod") {
		t.Errorf("message = %q, want it to name the database prod", r.Message)
	}
	if !strings.Contains(r.Message, "20261022T010203Z") {
		t.Errorf("message = %q, want it to name the timestamp 20261022T010203Z", r.Message)
	}
	if !strings.Contains(r.Message, aside) {
		t.Errorf("message = %q, want it to name the aside path %s", r.Message, aside)
	}
}

// TestDoltCorruptAsideCheck_OKWhenEmptyOrAbsent pins AC-8's OK half: an
// empty or absent corrupt-aside dir reads as OK.
func TestDoltCorruptAsideCheck_OKWhenEmptyOrAbsent(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		dir := setupManagedDoltCity(t)
		if err := os.MkdirAll(filepath.Join(dir, ".gc", "runtime", "packs", "dolt", "corrupt-aside"), 0o755); err != nil {
			t.Fatal(err)
		}
		r := NewDoltCorruptAsideCheck(dir, false).Run(&CheckContext{})
		if r.Status != StatusOK {
			t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
		}
	})
	t.Run("absent", func(t *testing.T) {
		dir := setupManagedDoltCity(t)
		r := NewDoltCorruptAsideCheck(dir, false).Run(&CheckContext{})
		if r.Status != StatusOK {
			t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
		}
	})
}

// setupManagedDoltCityWithRig is setupManagedDoltCity plus a rig, demo,
// bound to database de and inheriting the city's endpoint. The rig lives
// outside the city dir and only the returned config declares it, as the
// config gc doctor loads does: a check that took its bound set from anywhere
// but that config would not know de is bound.
func setupManagedDoltCityWithRig(t *testing.T) (string, *config.City) {
	t.Helper()
	dir := setupManagedDoltCity(t)
	rigDir := t.TempDir()
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fs, rigDir, "de")
	return dir, &config.City{Rigs: []config.Rig{{Name: "demo", Path: rigDir}}}
}

// TestDoltUnboundDatabasesCheckForConfig runs the check through the
// constructor gc doctor registers (cmd/gc/cmd_doctor.go), which takes the
// bound set from the loaded config: the rig's database is bound, so only pa
// is named, and with the city and the rig bound there is nothing to name.
// TestManagedDoltRecoveryChecks_SkipWithoutNamingAnyDatabase runs the skip
// cases through it.
func TestDoltUnboundDatabasesCheckForConfig(t *testing.T) {
	t.Run("names only the database no scope binds", func(t *testing.T) {
		dir, cfg := setupManagedDoltCityWithRig(t)
		for _, db := range []string{"hq", "de", "pa"} {
			writeCommittedDoltStore(t, dir, db)
		}
		r := NewDoltUnboundDatabasesCheckForConfig(dir, false, cfg, nil).Run(&CheckContext{})
		if r.Status != StatusWarning {
			t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
		}
		if want := "unbound dolt databases with no usable local backup: pa"; r.Message != want {
			t.Errorf("message = %q, want %q: hq is bound by the city and de by the rig", r.Message, want)
		}
	})

	t.Run("OK when the city and the rig bind every database", func(t *testing.T) {
		dir, cfg := setupManagedDoltCityWithRig(t)
		writeCommittedDoltStore(t, dir, "hq")
		writeCommittedDoltStore(t, dir, "de")
		r := NewDoltUnboundDatabasesCheckForConfig(dir, false, cfg, nil).Run(&CheckContext{})
		if r.Status != StatusOK {
			t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
		}
	})
}

// TestDoltCorruptAsideCheckForConfig runs the check through the constructor
// gc doctor registers (cmd/gc/cmd_doctor.go): OK while corrupt-aside/ is
// absent, then a warning naming the store once one is preserved there.
// TestManagedDoltRecoveryChecks_SkipWithoutNamingAnyDatabase runs the skip
// cases through it.
func TestDoltCorruptAsideCheckForConfig(t *testing.T) {
	dir, cfg := setupManagedDoltCityWithRig(t)
	check := NewDoltCorruptAsideCheckForConfig(dir, false, cfg, nil)
	if r := check.Run(&CheckContext{}); r.Status != StatusOK {
		t.Fatalf("status = %d, want OK with no corrupt-aside dir; msg = %s", r.Status, r.Message)
	}
	aside := filepath.Join(dir, ".gc", "runtime", "packs", "dolt", "corrupt-aside", "prod.20261022T010203Z")
	if err := os.MkdirAll(aside, 0o755); err != nil {
		t.Fatal(err)
	}
	r := check.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "prod (as of 20261022T010203Z) at "+aside) {
		t.Errorf("message = %q, want it to name prod, its timestamp and %s", r.Message, aside)
	}
}
