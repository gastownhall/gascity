package pathutil

import (
	"os"
	"path/filepath"
	"runtime"
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

func TestNormalizePathForCompareResolvesSymlinkAncestorForMissingLeaf(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real-parent")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	linkParent := filepath.Join(root, "link-parent")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Skip("symlinks not supported")
	}

	got := NormalizePathForCompare(filepath.Join(linkParent, "missing", "gc-home"))
	want := filepath.Join(realParent, "missing", "gc-home")
	if got != want {
		t.Fatalf("NormalizePathForCompare() = %q, want %q", got, want)
	}
}

func TestNormalizePathForCompareCollapsesDarwinPrivateVarAlias(t *testing.T) {
	got := NormalizePathForCompare("/private/var/folders/example/gc-home")
	want := filepath.Clean("/private/var/folders/example/gc-home")
	if runtime.GOOS == "darwin" {
		want = filepath.Clean("/var/folders/example/gc-home")
	}
	if got != want {
		t.Fatalf("NormalizePathForCompare() = %q, want %q", got, want)
	}
}

func TestNormalizePathForCompareCollapsesDarwinPrivateTmpAlias(t *testing.T) {
	got := NormalizePathForCompare("/private/tmp/gc-home")
	want := filepath.Clean("/private/tmp/gc-home")
	if runtime.GOOS == "darwin" {
		want = filepath.Clean("/tmp/gc-home")
	}
	if got != want {
		t.Fatalf("NormalizePathForCompare() = %q, want %q", got, want)
	}
}

func TestSamePathDifferent(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	if SamePath(a, b) {
		t.Errorf("expected different paths: %q vs %q", a, b)
	}
}
