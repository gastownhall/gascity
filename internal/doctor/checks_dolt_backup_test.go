package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// writeRigMetadata writes a minimal .beads/metadata.json for a rig with the
// given dolt database name.
func writeRigMetadata(t *testing.T, rigPath, dbName string) {
	t.Helper()
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatalf("create .beads dir: %v", err)
	}
	meta := map[string]any{
		"backend":       "dolt",
		"database":      "dolt",
		"dolt_database": dbName,
		"dolt_mode":     "server",
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), data, 0o600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
}

// writeRepoStateWithBackup writes a minimal managed-Dolt repo_state.json that
// includes a backup entry named <dbName>-backup.
func writeRepoStateWithBackup(t *testing.T, doltDataDir, dbName, backupURL string) {
	t.Helper()
	doltDir := filepath.Join(doltDataDir, dbName, ".dolt")
	if err := os.MkdirAll(doltDir, 0o700); err != nil {
		t.Fatalf("create .dolt dir: %v", err)
	}
	state := map[string]any{
		"head":    "refs/heads/main",
		"remotes": map[string]any{},
		"backups": map[string]any{
			dbName + "-backup": map[string]any{
				"name": dbName + "-backup",
				"url":  backupURL,
			},
		},
		"branches": map[string]any{},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal repo_state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(doltDir, "repo_state.json"), data, 0o600); err != nil {
		t.Fatalf("write repo_state: %v", err)
	}
}

func TestDoltBackupCheck_NoBackup_Warns(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "testdb")

	rig := config.Rig{Name: "testrig", Path: rigPath}
	c := NewDoltBackupCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want StatusWarning (no backup configured)", r.Status)
	}
	expectedDir := filepath.Join(cityPath, ".dolt-backup", "testdb")
	if !strings.Contains(r.Message, expectedDir) {
		t.Errorf("Message missing expected backup dir %q: %s", expectedDir, r.Message)
	}
	if !strings.Contains(r.Message, "testrig") {
		t.Errorf("Message missing rig name %q: %s", "testrig", r.Message)
	}
	// Fix command must be copy-pasteable and reach the user.
	if !strings.Contains(r.FixHint, "DOLT_BACKUP") {
		t.Errorf("FixHint missing DOLT_BACKUP invocation: %s", r.FixHint)
	}
	if !strings.Contains(r.FixHint, "'testdb-backup'") {
		t.Errorf("FixHint missing backup-remote name 'testdb-backup': %s", r.FixHint)
	}
	if !strings.Contains(r.FixHint, "file://"+expectedDir) {
		t.Errorf("FixHint missing file:// URL %q: %s", expectedDir, r.FixHint)
	}
}

func TestDoltBackupCheck_BackupDirExists_OK(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "testdb")
	backupDir := filepath.Join(cityPath, ".dolt-backup", "testdb")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "sync.marker"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	rig := config.Rig{Name: "testrig", Path: rigPath}
	c := NewDoltBackupCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusOK {
		t.Fatalf("status = %d, want StatusOK (backup dir present); message=%s hint=%s",
			r.Status, r.Message, r.FixHint)
	}
}

func TestDoltBackupCheck_EmptyBackupDirFallsThroughToWarning(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "testdb")
	if err := os.MkdirAll(filepath.Join(cityPath, ".dolt-backup", "testdb"), 0o700); err != nil {
		t.Fatal(err)
	}

	rig := config.Rig{Name: "testrig", Path: rigPath}
	c := NewDoltBackupCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want StatusWarning (empty backup dir is weak evidence); message=%s",
			r.Status, r.Message)
	}
}

func TestDoltBackupCheck_RepoStateRemoteRegisteredInManagedDataDir_OK(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "testdb")
	// No backup dir, but remote is registered in the managed Dolt data dir.
	writeRepoStateWithBackup(t, doltDataDir, "testdb",
		"file://"+filepath.Join(cityPath, ".dolt-backup", "testdb"))

	rig := config.Rig{Name: "testrig", Path: rigPath}
	c := NewDoltBackupCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusOK {
		t.Fatalf("status = %d, want StatusOK (remote registered); message=%s",
			r.Status, r.Message)
	}
}

func TestDoltBackupCheck_RelativeRigPathReadsMetadataFromCity(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rigs", "frontend")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "frontend_db")

	rig := config.Rig{Name: "frontend", Path: filepath.Join("rigs", "frontend")}
	c := NewDoltBackupCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if !strings.Contains(r.Message, filepath.Join(cityPath, ".dolt-backup", "frontend_db")) {
		t.Fatalf("Message should use metadata from normalized rig path: %s", r.Message)
	}
}

func TestDoltBackupCheck_CorruptMetadataFallbackAddsDetail(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(`{"dolt_database":`), 0o600); err != nil {
		t.Fatal(err)
	}

	rig := config.Rig{Name: "fallbackrig", Path: rigPath}
	c := NewDoltBackupCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want StatusWarning", r.Status)
	}
	if len(r.Details) == 0 || !strings.Contains(strings.Join(r.Details, "\n"), "parse metadata.json") {
		t.Fatalf("Details should surface corrupt metadata parse failure, got %#v", r.Details)
	}
}

func TestDoltBackupCheck_DBNameFallsBackToRigName(t *testing.T) {
	// When metadata.json is absent the check should fall back to rig.Name
	// for the dolt database name so it still surfaces a useful warning.
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	// No metadata.json written — exercise fallback.

	rig := config.Rig{Name: "fallbackrig", Path: rigPath}
	c := NewDoltBackupCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want StatusWarning", r.Status)
	}
	if !strings.Contains(r.Message, "fallbackrig") {
		t.Errorf("Message should reference rig.Name fallback %q: %s", "fallbackrig", r.Message)
	}
}

func TestDoltBackupCheck_CannotFix(t *testing.T) {
	// One-way door — registering a backup is operator policy, not auto-fix.
	rig := config.Rig{Name: "testrig", Path: t.TempDir()}
	c := NewDoltBackupCheck(t.TempDir(), rig, filepath.Join(t.TempDir(), ".beads", "dolt"))
	if c.CanFix() {
		t.Fatal("CanFix should return false (backup destination is operator policy)")
	}
}

func TestDoltBackupCheck_Name(t *testing.T) {
	rig := config.Rig{Name: "myrig", Path: t.TempDir()}
	c := NewDoltBackupCheck(t.TempDir(), rig, filepath.Join(t.TempDir(), ".beads", "dolt"))
	want := "rig:myrig:dolt-backup"
	if got := c.Name(); got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}

// writeScopeConfig writes a minimal .beads/config.yaml for a scope (city or rig).
func writeScopeConfig(t *testing.T, scopePath, body string) {
	t.Helper()
	beadsDir := filepath.Join(scopePath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatalf("create .beads dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

// A rig whose Dolt lives on an external endpoint self-manages its backups on
// that server; the local .dolt-backup / repo_state.json signals never apply, so
// the check must not warn or emit a localhost fix hint. Regression for #3868.
func TestDoltBackupCheck_ExternalEndpoint_NoWarn(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "extdb")
	writeScopeConfig(t, cityPath, "issue_prefix: gc\ngc.endpoint_origin: managed_city\ndolt.auto-start: false\n")
	writeScopeConfig(t, rigPath, "issue_prefix: fe\ngc.endpoint_origin: explicit\ndolt.host: db.example.com\ndolt.port: \"3307\"\ndolt.auto-start: false\n")

	rig := config.Rig{Name: "extrig", Path: rigPath}
	c := NewDoltBackupCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusOK {
		t.Fatalf("status = %d, want StatusOK (external endpoint self-manages backups); msg=%q", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "external") {
		t.Errorf("Message should note the external endpoint: %s", r.Message)
	}
	if !strings.Contains(r.Message, "db.example.com") {
		t.Errorf("Message should name the external host: %s", r.Message)
	}
	if r.FixHint != "" {
		t.Errorf("external endpoint must not emit a localhost fix hint: %s", r.FixHint)
	}
}
