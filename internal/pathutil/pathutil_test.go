package pathutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizePathForCompare(t *testing.T) {
	dir := t.TempDir()
	got := NormalizePathForCompare(dir)
	if got == "" {
		t.Error("expected non-empty normalized path")
	}
	// Normalized path should be absolute and clean.
	if !filepath.IsAbs(got) {
		t.Errorf("expected absolute path, got %q", got)
	}
}

func TestNormalizePathForCompareEmpty(t *testing.T) {
	if got := NormalizePathForCompare(""); got != "" {
		t.Errorf("expected empty for empty input, got %q", got)
	}
}

func TestSamePath(t *testing.T) {
	dir := t.TempDir()
	if !SamePath(dir, dir) {
		t.Errorf("expected same path for identical inputs")
	}
}

func TestSamePathSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip("symlinks not supported")
	}
	if !SamePath(dir, link) {
		t.Errorf("expected same path through symlink: %q vs %q", dir, link)
	}
}

func TestSamePathDifferent(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	if SamePath(a, b) {
		t.Errorf("expected different paths: %q vs %q", a, b)
	}
}
