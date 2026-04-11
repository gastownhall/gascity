// Package testutil contains helpers shared by tests across platforms.
package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// CanonicalPath returns a cleaned absolute path, resolving symlinks when the
// target exists. This keeps tests stable on macOS where /tmp and parts of
// /var are symlinked into /private.
func CanonicalPath(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	path = canonicalizeExistingPathPrefix(path)
	return filepath.Clean(path)
}

func canonicalizeExistingPathPrefix(path string) string {
	current := path
	var suffix []string
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

// AssertSamePath compares two filesystem paths after canonicalization.
func AssertSamePath(t *testing.T, got, want string) {
	t.Helper()
	got = CanonicalPath(got)
	want = CanonicalPath(want)
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

// ShortTempDir returns a test-owned temporary directory rooted at a short path
// on macOS so Unix socket paths stay under the platform limit.
func ShortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	root := os.TempDir()
	if runtime.GOOS == "darwin" {
		root = "/tmp"
	}
	dir, err := os.MkdirTemp(root, prefix)
	if err != nil {
		t.Fatalf("MkdirTemp(%q, %q): %v", root, prefix, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
