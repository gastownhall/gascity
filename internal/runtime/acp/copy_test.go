package acp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyToRejectsNestedDestinationSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workdir")
	outsideDir := filepath.Join(root, "outside")
	srcDir := filepath.Join(root, "source")
	for _, dir := range []string{filepath.Join(workDir, "dest"), outsideDir, filepath.Join(srcDir, "nested")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %q: %v", dir, err)
		}
	}
	if err := os.Symlink(outsideDir, filepath.Join(workDir, "dest", "nested")); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "nested", "pwn"), []byte("escaped"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	p := &Provider{workDirs: map[string]string{"worker": workDir}}

	err := p.CopyTo("worker", srcDir, "dest")
	if _, statErr := os.Stat(filepath.Join(outsideDir, "pwn")); !os.IsNotExist(statErr) {
		t.Fatalf("CopyTo() error = %v; outside destination stat error = %v, want not exist", err, statErr)
	}
	if err == nil {
		t.Fatal("CopyTo() succeeded, want nested symlink escape error")
	}
}
