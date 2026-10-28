package fsys

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestIsDirNotEmptyRecognizesRemovingANonEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "full")
	if err := os.Mkdir(full, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(full, "entry"), nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err := os.Remove(full)
	if err == nil {
		t.Fatal("removing a non-empty directory succeeded")
	}
	if !IsDirNotEmpty(err) {
		t.Fatalf("IsDirNotEmpty(%v) = false, want true", err)
	}
}

func TestIsDirNotEmptyRejectsOtherErrors(t *testing.T) {
	missing := os.Remove(filepath.Join(t.TempDir(), "missing"))
	if missing == nil {
		t.Fatal("removing a missing path succeeded")
	}
	for name, err := range map[string]error{
		"missing path": missing,
		"nil":          nil,
		"unrelated":    errors.New("unrelated"),
	} {
		if IsDirNotEmpty(err) {
			t.Errorf("IsDirNotEmpty(%s: %v) = true, want false", name, err)
		}
	}
}
