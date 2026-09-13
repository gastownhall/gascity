package supervisor

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBuiltinDefaultHomeAvoidsSharedTempFallback guards #3650 (sibling of
// #3506): when the user home cannot be resolved, builtinDefaultHome() must not
// hand back the shared os.TempDir()/.gc path. That path is world-writable and
// shared across every process and user on the host, so concurrent processes
// clobber each other's state and unrelated city scans pick it up as a real
// city. builtinDefaultHome() carries no *.test guard, so it is called directly.
func TestBuiltinDefaultHomeAvoidsSharedTempFallback(t *testing.T) {
	t.Setenv("HOME", "") // forces os.UserHomeDir() to fail on unix

	got := builtinDefaultHome()

	if shared := filepath.Join(os.TempDir(), ".gc"); got == shared {
		t.Fatalf("builtinDefaultHome() = %q, want a process-isolated path, not the shared %q", got, shared)
	}
	// Must never be empty: callers join the result into a path, so "" silently
	// becomes a CWD-relative path and writes state to the wrong place.
	if got == "" {
		t.Fatal("builtinDefaultHome() returned an empty path; callers would write state to a CWD-relative path")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("builtinDefaultHome() = %q, want an absolute process-isolated path", got)
	}
}
