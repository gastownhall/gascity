package bootstrap

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultGCHomeAvoidsSharedTempFallback guards #3650 (sibling of #3506):
// when GC_HOME is unset and the user home cannot be resolved, defaultGCHome()
// must not hand back the shared os.TempDir()/.gc path. That path is
// world-writable and shared across every process and user on the host, so
// concurrent processes clobber each other's state and unrelated city scans
// pick it up as a real city.
func TestDefaultGCHomeAvoidsSharedTempFallback(t *testing.T) {
	// defaultGCHome() short-circuits to "" under a *.test binary to stay
	// hermetic. Temporarily mask that guard so the fallback path is reached.
	orig := os.Args[0]
	os.Args[0] = "gc"
	defer func() { os.Args[0] = orig }()

	t.Setenv("GC_HOME", "")
	t.Setenv("HOME", "") // forces os.UserHomeDir() to fail on unix

	got := defaultGCHome()

	if shared := filepath.Join(os.TempDir(), ".gc"); got == shared {
		t.Fatalf("defaultGCHome() = %q, want a process-isolated path, not the shared %q", got, shared)
	}
	// Must never be empty: callers join the result into a path, so "" silently
	// becomes a CWD-relative path and writes state to the wrong place.
	if got == "" {
		t.Fatal("defaultGCHome() returned an empty path; callers would write state to a CWD-relative path")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("defaultGCHome() = %q, want an absolute process-isolated path", got)
	}
}
