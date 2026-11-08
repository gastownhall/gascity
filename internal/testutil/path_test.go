package testutil

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestShortTempRootUsesShortConfiguredRoot(t *testing.T) {
	skipOnWindows(t)
	root := filepath.Join("/var", "tmp", "gc-root")
	if got := shortTempRoot(root, ""); got != root {
		t.Fatalf("shortTempRoot() = %q, want configured root %q", got, root)
	}
}

func TestShortTempRootUsesDiskBackedShortRoot(t *testing.T) {
	skipOnWindows(t)
	tempDir := filepath.Join("/var", "tmp", strings.Repeat("long-", 30))
	if got := shortTempRoot(tempDir, ""); got != "/var/tmp" {
		t.Fatalf("shortTempRoot() = %q, want /var/tmp root", got)
	}
}

func TestShortTempRootAvoidsSharedTmp(t *testing.T) {
	skipOnWindows(t)
	if got := shortTempRoot("/tmp", ""); got != "/var/tmp" {
		t.Fatalf("shortTempRoot() = %q, want /var/tmp root", got)
	}
}

func TestShortTempDirUsesBazelWritableRoot(t *testing.T) {
	skipOnWindows(t)
	tempDir := "/tmp/bt/bazel-test-sandbox/very/long/per-action/path"
	if got := shortTempRoot(tempDir, tempDir); got != "/tmp/bt" {
		t.Fatalf("shortTempRoot() = %q, want Bazel writable root /tmp/bt", got)
	}
}

func TestShortTempRootDoesNotAssumeBazelForArbitraryTestTempDir(t *testing.T) {
	skipOnWindows(t)
	tempDir := "/custom/test/tmp/with/a/very/long/per-action/path"
	if got := shortTempRoot(tempDir, tempDir); got != "/var/tmp" {
		t.Fatalf("shortTempRoot() = %q, want /var/tmp root", got)
	}
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shortTempRoot keeps the native temp root on Windows")
	}
}
