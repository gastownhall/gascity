package dashboardbff

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// cleanEnvPath returns the PATH entry cleanEnv assembled, for assertions.
func cleanEnvPath(t *testing.T) string {
	t.Helper()
	for _, kv := range cleanEnv() {
		if rest, ok := strings.CutPrefix(kv, "PATH="); ok {
			return rest
		}
	}
	t.Fatal("cleanEnv() returned no PATH entry")
	return ""
}

// TestCleanEnvDefaultPathIncludesHomebrewOnDarwin pins gastownhall/gascity#7979:
// execBdPing runs `bd ping` under cleanEnv's sandboxed environment, and on
// Apple Silicon the only place Homebrew installs `dolt` is /opt/homebrew/bin.
// The hardcoded default (home + "/.local/bin:/usr/local/bin:/usr/bin:/bin")
// never included it, so the probe reported a healthy, running Dolt-backed rig
// as disconnected. cleanEnv's default PATH now goes through
// internal/searchpath (the same helper gc init's provider-readiness probes
// use), which adds the platform's package-manager prefixes.
func TestCleanEnvDefaultPathIncludesHomebrewOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-specific PATH expansion")
	}
	t.Setenv("ADMIN_PATH", "")
	t.Setenv("HOME", t.TempDir())

	path := cleanEnvPath(t)
	for _, want := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"} {
		if !strings.Contains(path, want) {
			t.Errorf("cleanEnv() PATH = %q, want it to contain %q", path, want)
		}
	}
}

// TestCleanEnvRespectsAdminPathOverrideVerbatim pins the escape hatch: an
// operator who sets ADMIN_PATH gets exactly that PATH, not the expanded
// default layered on top of it.
func TestCleanEnvRespectsAdminPathOverrideVerbatim(t *testing.T) {
	const override = "/custom/bin:/another/bin"
	t.Setenv("ADMIN_PATH", override)

	if path := cleanEnvPath(t); path != override {
		t.Errorf("cleanEnv() PATH = %q, want the ADMIN_PATH override %q verbatim", path, override)
	}
}

// TestCleanEnvFallsBackToTmpHomeWhenUnset pins the pre-existing HOME fallback
// so the searchpath-based rewrite does not regress it: an unset HOME must not
// panic filepath.Join inside internal/searchpath, and must still resolve.
func TestCleanEnvFallsBackToTmpHomeWhenUnset(t *testing.T) {
	t.Setenv("ADMIN_PATH", "")
	originalHome := os.Getenv("HOME")
	t.Cleanup(func() { _ = os.Setenv("HOME", originalHome) })
	if err := os.Unsetenv("HOME"); err != nil {
		t.Fatalf("Unsetenv(HOME): %v", err)
	}

	path := cleanEnvPath(t)
	if path == "" {
		t.Error("cleanEnv() PATH is empty with HOME unset")
	}
}
