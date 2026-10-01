package testutil

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestShortTempDirUsesDiskBackedShortRoot(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), strings.Repeat("long-", 30)))
	dir := ShortTempDir(t, "gc-test-")
	if !strings.HasPrefix(dir, "/var/tmp/gc-test-") {
		t.Fatalf("ShortTempDir() = %q, want /var/tmp root", dir)
	}
}
