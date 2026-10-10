package acceptancehelpers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An override under another file name (bazel's //cmd/gc:gc_testhooks) comes
// back as a gc in dir: gc's exec orders and pack scripts run bare `gc` through
// the PATH NewEnv builds from it.
func TestBuildGCUsesOverrideBinary(t *testing.T) {
	override := writeFakeGC(t, filepath.Join(t.TempDir(), "gc-external"))
	t.Setenv("GC_ACCEPTANCE_GC_BIN", override)

	dir := t.TempDir()
	got := BuildGC(dir)
	if want := filepath.Join(dir, "gc"); got != want {
		t.Fatalf("BuildGC() = %q, want %q", got, want)
	}
	if !sameFile(t, got, override) {
		t.Fatalf("BuildGC() = %q is not the override %q", got, override)
	}
}

func TestBuildGCReturnsAnOverrideNamedGCUnchanged(t *testing.T) {
	override := writeFakeGC(t, filepath.Join(t.TempDir(), "gc"))
	t.Setenv("GC_ACCEPTANCE_GC_BIN", override)

	if got := BuildGC(t.TempDir()); got != override {
		t.Fatalf("BuildGC() = %q, want %q", got, override)
	}
}

func writeFakeGC(t *testing.T, path string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake gc: %v", err)
	}
	return path
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	ai, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	bi, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(ai, bi)
}

func TestRunGCUsesExactBinaryOverridePath(t *testing.T) {
	t.Helper()

	tmpDir := t.TempDir()
	gcHome := filepath.Join(tmpDir, "home")
	runtimeDir := filepath.Join(tmpDir, "runtime")
	if err := os.MkdirAll(gcHome, 0o755); err != nil {
		t.Fatalf("mkdir gcHome: %v", err)
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("mkdir runtimeDir: %v", err)
	}

	override := filepath.Join(tmpDir, "custom-binary-name")
	script := "#!/bin/sh\necho override:$0 $*\n"
	if err := os.WriteFile(override, []byte(script), 0o755); err != nil {
		t.Fatalf("write override binary: %v", err)
	}

	env := NewEnv(override, gcHome, runtimeDir)
	out, err := RunGC(env, "", "version")
	if err != nil {
		t.Fatalf("RunGC: %v\n%s", err, out)
	}
	if !strings.Contains(out, "override:"+override+" version") {
		t.Fatalf("RunGC() output = %q, want override invocation for %q", out, override)
	}
}

func TestFindBDResolvesBazelRootpathOverride(t *testing.T) {
	srcdir := t.TempDir()
	want := filepath.Join(srcdir, "+http_archive+bd_pinned", "bd")
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_SRCDIR", srcdir)
	t.Setenv("TEST_WORKSPACE", "_main")
	// $(rootpath @bd_pinned//:bd) is relative to the main repository's
	// runfiles directory, not to the package directory the test runs in.
	t.Setenv("GC_ACCEPTANCE_BD_BIN", "../+http_archive+bd_pinned/bd")

	if got := FindBD(); got != want {
		t.Fatalf("FindBD() = %q, want %q", got, want)
	}
}

func TestFindBDNeverFallsBackFromABrokenOverride(t *testing.T) {
	pathDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pathDir, "bd"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)
	t.Setenv("TEST_SRCDIR", "")
	t.Setenv("GC_ACCEPTANCE_BD_BIN", filepath.Join(t.TempDir(), "missing-bd"))

	if got := FindBD(); got != "" {
		t.Fatalf("FindBD() = %q with a broken GC_ACCEPTANCE_BD_BIN, want \"\" (no fallback to PATH)", got)
	}
}
