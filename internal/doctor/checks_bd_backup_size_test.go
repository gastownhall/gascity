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

func TestBdBackupSizeCheck_PathIsNotADirectory(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "backup"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newTestBdBackupSizeCheck(dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "is not a directory") {
		t.Errorf("message = %q, want not-a-directory warning", r.Message)
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

func TestBdBackupSizeCheck_RigScopedBackupWarns(t *testing.T) {
	dir := t.TempDir()
	rigDir := filepath.Join(dir, "rigs", "demo")
	backupDir := filepath.Join(rigDir, ".beads", "backup")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	c := NewBdBackupSizeCheck(dir)
	c.scopeRoots = []string{dir, rigDir}
	c.measureDir = func(root string) (int64, bool, error) {
		if root == backupDir {
			return bdBackupWarnBytes, true, nil
		}
		return 0, false, nil
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "scope rigs/demo") {
		t.Errorf("message = %q, want rig scope label", r.Message)
	}
}

func TestBdBackupSizeCheck_AggregateAcrossScopesWarns(t *testing.T) {
	dir := t.TempDir()
	rigDir := filepath.Join(dir, "rigs", "demo")
	cityBackupDir := filepath.Join(dir, ".beads", "backup")
	rigBackupDir := filepath.Join(rigDir, ".beads", "backup")
	for _, backupDir := range []string{cityBackupDir, rigBackupDir} {
		if err := os.MkdirAll(backupDir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	c := NewBdBackupSizeCheck(dir)
	c.scopeRoots = []string{dir, rigDir}
	c.measureDir = func(string) (int64, bool, error) {
		return bdBackupWarnBytes / 2, true, nil
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "aggregate bd auto-backup footprint") {
		t.Errorf("message = %q, want aggregate warning", r.Message)
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
		return bdBackupWarnBytes, true, nil
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
		return bdBackupErrorBytes, true, nil
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

func TestBdBackupSizeCheck_MeasureMissingReportsNoBackupDir(t *testing.T) {
	dir := t.TempDir()
	backupDir := filepath.Join(dir, ".beads", "backup")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	c := NewBdBackupSizeCheck(dir)
	c.measureDir = func(string) (int64, bool, error) {
		return 0, false, nil
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "no bd backup directory present") {
		t.Errorf("message = %q, want no-backup-dir message", r.Message)
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
