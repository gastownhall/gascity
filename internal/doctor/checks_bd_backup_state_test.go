package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBackupStateFixture(t *testing.T, root string, files map[string]string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBdBackupStateCheck_CleanScopeIsOK(t *testing.T) {
	city := t.TempDir()
	writeBackupStateFixture(t, city, nil, ".beads/dolt")

	check := NewBdBackupStateCheckForScopeRoots(city, []string{city})
	r := check.Run(nil)
	if r.Status != StatusOK {
		t.Fatalf("Status = %v (%s), want OK", r.Status, r.Message)
	}
}

func TestBdBackupStateCheck_FlagsCorruptQuarantine(t *testing.T) {
	// A corruption event quarantines the store as .beads/<name>.corrupt-<ts>
	// and nothing ever reclaims it: a 1.8GB quarantine sat unnoticed from
	// 2026-05-27 to 2026-06-10 (ga-yfbs28). Doctor must surface it.
	city := t.TempDir()
	writeBackupStateFixture(t, city, map[string]string{
		".beads/dolt.corrupt-20260527T145828Z/ga/noms/chunk": "x",
	}, ".beads/dolt")

	check := NewBdBackupStateCheckForScopeRoots(city, []string{city})
	r := check.Run(nil)
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v (%s), want Warning", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "dolt.corrupt-20260527T145828Z") {
		t.Errorf("Message = %q, want quarantine dir named", r.Message)
	}
	if r.FixHint == "" {
		t.Error("FixHint empty; operators need the archive-then-remove recipe")
	}
}

func TestBdBackupStateCheck_FlagsStaleBackupRegistration(t *testing.T) {
	// dolt-backup.json pointing at a deleted path means backup syncs fall
	// back to the live .beads/backup dir while the registration looks
	// healthy. Observed live: backup_url=file:///data/tmp/tmp.y9AoDggtwt/...
	// (a long-deleted mktemp dir).
	city := t.TempDir()
	writeBackupStateFixture(t, city, map[string]string{
		".beads/dolt-backup.json": `{"backup_url":"file:///nonexistent/tmp.gone/.beads/backup","backup_name":"default"}`,
	}, ".beads/dolt")

	check := NewBdBackupStateCheckForScopeRoots(city, []string{city})
	r := check.Run(nil)
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v (%s), want Warning", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "dolt-backup.json") {
		t.Errorf("Message = %q, want stale registration named", r.Message)
	}
}

func TestBdBackupStateCheck_LiveBackupRegistrationIsOK(t *testing.T) {
	city := t.TempDir()
	backupDir := filepath.Join(city, "backups")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBackupStateFixture(t, city, map[string]string{
		".beads/dolt-backup.json": `{"backup_url":"file://` + backupDir + `","backup_name":"default"}`,
	}, ".beads/dolt")

	check := NewBdBackupStateCheckForScopeRoots(city, []string{city})
	r := check.Run(nil)
	if r.Status != StatusOK {
		t.Fatalf("Status = %v (%s), want OK for live registration", r.Status, r.Message)
	}
}

func TestBdBackupStateCheck_NonFileBackupURLIsOK(t *testing.T) {
	// Remote (non-file) backup URLs cannot be liveness-checked from this
	// host; the check must not guess.
	city := t.TempDir()
	writeBackupStateFixture(t, city, map[string]string{
		".beads/dolt-backup.json": `{"backup_url":"https://dolthub.com/some/remote","backup_name":"default"}`,
	}, ".beads/dolt")

	check := NewBdBackupStateCheckForScopeRoots(city, []string{city})
	r := check.Run(nil)
	if r.Status != StatusOK {
		t.Fatalf("Status = %v (%s), want OK for non-file URL", r.Status, r.Message)
	}
}

func TestBdBackupStateCheck_MissingBeadsDirIsOK(t *testing.T) {
	city := t.TempDir()
	check := NewBdBackupStateCheckForScopeRoots(city, []string{city})
	r := check.Run(nil)
	if r.Status != StatusOK {
		t.Fatalf("Status = %v (%s), want OK when scope has no .beads", r.Status, r.Message)
	}
}

func TestBdBackupStateCheck_AggregatesAcrossScopes(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "beads")
	writeBackupStateFixture(t, city, nil, ".beads/dolt")
	writeBackupStateFixture(t, rig, map[string]string{
		".beads/dolt.corrupt-20260601T000000Z/x": "x",
	}, ".beads/dolt")

	check := NewBdBackupStateCheckForScopeRoots(city, []string{city, rig})
	r := check.Run(nil)
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v (%s), want Warning from rig scope", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "dolt.corrupt-20260601T000000Z") {
		t.Errorf("Message = %q, want rig quarantine named", r.Message)
	}
}
