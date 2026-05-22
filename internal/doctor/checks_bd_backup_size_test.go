package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestBdBackupSizeCheck(cityPath string) *BdBackupSizeCheck {
	c := NewBdBackupSizeCheck(cityPath)
	c.measureDir = sumDirBytes
	return c
}

func TestBdBackupSizeCheck_NoBackupDir(t *testing.T) {
	c := newTestBdBackupSizeCheck(t.TempDir())
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "no bd backup directory present") {
		t.Errorf("message = %q, want no-backup-dir message", r.Message)
	}
}

func TestBdBackupSizeCheck_EmptyBackupDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads", "backup"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := newTestBdBackupSizeCheck(dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestBdBackupSizeCheck_OKUnderThreshold(t *testing.T) {
	dir := t.TempDir()
	backupDir := filepath.Join(dir, ".beads", "backup")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Small file far below warn threshold.
	if err := os.WriteFile(filepath.Join(backupDir, "manifest"), []byte("small content"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newTestBdBackupSizeCheck(dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK under threshold; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "bd auto-backup directory") {
		t.Errorf("message = %q, want size summary", r.Message)
	}
}

func TestBdBackupSizeCheck_WarnAtThreshold(t *testing.T) {
	dir := t.TempDir()
	backupDir := filepath.Join(dir, ".beads", "backup")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	c := NewBdBackupSizeCheck(dir)
	c.measureDir = func(string) (int64, bool, error) {
		return bdBackupWarnBytes + 1, true, nil
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.FixHint, "bd-backup-cleanup.md") {
		t.Errorf("FixHint = %q, want cleanup doc pointer", r.FixHint)
	}
}

func TestBdBackupSizeCheck_ErrorAtThreshold(t *testing.T) {
	dir := t.TempDir()
	backupDir := filepath.Join(dir, ".beads", "backup")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	c := NewBdBackupSizeCheck(dir)
	c.measureDir = func(string) (int64, bool, error) {
		return bdBackupErrorBytes + 1, true, nil
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.FixHint, "bd-backup-cleanup.md") {
		t.Errorf("FixHint = %q, want cleanup doc pointer", r.FixHint)
	}
}

func TestBdBackupSizeCheck_MeasureErrorWarns(t *testing.T) {
	dir := t.TempDir()
	backupDir := filepath.Join(dir, ".beads", "backup")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	c := NewBdBackupSizeCheck(dir)
	c.measureDir = func(string) (int64, bool, error) {
		return 0, true, os.ErrPermission
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning on measure error; msg = %s", r.Status, r.Message)
	}
}

func TestBdBackupSizeCheck_CanFixFalse(t *testing.T) {
	c := newTestBdBackupSizeCheck(t.TempDir())
	if c.CanFix() {
		t.Error("CanFix() = true, want false")
	}
}

func TestBdBackupSizeCheck_Name(t *testing.T) {
	c := newTestBdBackupSizeCheck(t.TempDir())
	if got, want := c.Name(), "bd-backup-size"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}
