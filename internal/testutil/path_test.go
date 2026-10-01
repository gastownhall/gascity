package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShortTempDirUsesShortConfiguredRoot(t *testing.T) {
	root, err := os.MkdirTemp("/var/tmp", "gc-root-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("TMPDIR", root)
	dir := ShortTempDir(t, "gc-test-")
	if !strings.HasPrefix(dir, root+string(filepath.Separator)) {
		t.Fatalf("ShortTempDir() = %q, want configured root %q", dir, root)
	}
}

func TestShortTempDirUsesDiskBackedShortRoot(t *testing.T) {
	t.Setenv("TEST_TMPDIR", "")
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), strings.Repeat("long-", 30)))
	dir := ShortTempDir(t, "gc-test-")
	if !strings.HasPrefix(dir, "/var/tmp/gc-test-") {
		t.Fatalf("ShortTempDir() = %q, want /var/tmp root", dir)
	}
}

func TestShortTempDirAvoidsSharedTmpWhenTMPDIRUnset(t *testing.T) {
	t.Setenv("TEST_TMPDIR", "")
	t.Setenv("TMPDIR", "")
	dir := ShortTempDir(t, "gc-test-")
	if strings.HasPrefix(dir, "/tmp/") {
		t.Fatalf("ShortTempDir() = %q, must not use shared /tmp", dir)
	}
}

func TestShortTempDirUsesBazelWritableRoot(t *testing.T) {
	tempDir := "/tmp/bt/bazel-test-sandbox/very/long/per-action/path"
	if got := shortTempRoot(tempDir, tempDir); got != "/tmp/bt" {
		t.Fatalf("shortTempRoot() = %q, want Bazel writable root /tmp/bt", got)
	}
}
