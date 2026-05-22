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

// IsOutsideDir is the post-Rel containment check used at every place
// that derives a relative path from a base and needs to refuse paths
// that would escape it. Cover all three branches: direct ".." escape,
// "../foo" prefix escape, and contained paths (".", "foo", "foo/bar").
func TestIsOutsideDir(t *testing.T) {
	sep := string(filepath.Separator)
	tests := []struct {
		rel  string
		want bool
	}{
		{"..", true},
		{".." + sep + "outside", true},
		{".." + sep + "deep" + sep + "escape", true},
		{".", false},
		{"foo", false},
		{"foo" + sep + "bar", false},
		{"", false},
		{"..foo", false},     // prefix-only — not an escape.
		{"..." + sep, false}, // three dots — not an escape.
	}
	for _, tt := range tests {
		if got := IsOutsideDir(tt.rel); got != tt.want {
			t.Errorf("IsOutsideDir(%q) = %v, want %v", tt.rel, got, tt.want)
		}
	}
}

func TestPathWithin(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "nested", "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if !PathWithin(root, child) {
		t.Fatalf("PathWithin(%q, %q) = false, want true", root, child)
	}
	if !PathWithin(root, root) {
		t.Fatalf("PathWithin(%q, %q) = false, want true for identical paths", root, root)
	}
}

func TestPathWithinSymlinkedMissingLeaf(t *testing.T) {
	root := t.TempDir()
	realPath := filepath.Join(root, "real")
	if err := os.MkdirAll(realPath, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realPath, link); err != nil {
		t.Skip("symlinks not supported")
	}
	candidate := filepath.Join(link, "missing", "leaf")
	if !PathWithin(realPath, candidate) {
		t.Fatalf("PathWithin(%q, %q) = false, want true through symlink ancestor", realPath, candidate)
	}
}
