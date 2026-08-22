// Package testutil contains helpers shared by tests across platforms.
package testutil

import (
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/pathutil"
)

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

// ShortTempDir returns a test-owned temporary directory rooted at /tmp so
// Unix socket paths stay under the 108-byte platform limit. It ignores TMPDIR
// deliberately: a long TMPDIR (e.g. /home/user/tmp/gascity-ga-XXXXX set by
// the deployer for test isolation) would push socket paths over the limit.
func ShortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatalf("MkdirTemp(/tmp, %q): %v", prefix, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
