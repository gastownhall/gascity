// Package testutil contains helpers shared by tests across platforms.
package testutil

import (
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/pathutil"
)

const shortTempRootMaxLen = 40

// CanonicalPath returns the production path-normalized form used for
// comparisons. This keeps tests stable on macOS where /tmp and /var can be
// reported through /private aliases.
func CanonicalPath(path string) string {
	return pathutil.NormalizePathForCompare(path)
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

// ShortTempDir returns a test-owned directory under a short root.
func ShortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	root := os.TempDir()
	if len(root) > shortTempRootMaxLen {
		root = "/var/tmp"
	}
	dir, err := os.MkdirTemp(root, prefix)
	if err != nil {
		t.Fatalf("MkdirTemp(%q, %q): %v", root, prefix, err)
	}
	t.Cleanup(func() {
		SaveFailureDiagnostics(t, dir)
		_ = os.RemoveAll(dir)
	})
	return dir
}
