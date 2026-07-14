package supervisor

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBuiltinDefaultHomeFallbackAvoidsSharedTemp guards #3506: the
// fallback returned when home is unresolvable must not be the shared
// os.TempDir()/.gc path. That path is world-writable and shared across
// every process and user on the host, so concurrent processes clobber
// each other's state and unrelated city scans pick it up as a real city.
// builtinDefaultHomeFallback() carries no *.test guard, so it is called
// directly.
func TestBuiltinDefaultHomeFallbackAvoidsSharedTemp(t *testing.T) {
	got := builtinDefaultHomeFallback()

	if got == "" {
		// Must never be empty: callers join the result into a path, so "" silently
		// becomes a CWD-relative path and writes state to the wrong place.
		t.Fatal("builtinDefaultHomeFallback: got empty string")
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("builtinDefaultHomeFallback: got non-absolute path %q", got)
	}
	if got == filepath.Join(os.TempDir(), ".gc") {
		t.Fatalf("builtinDefaultHomeFallback: got shared temp fallback %q", got)
	}
}
