package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A city reached through a symlinked path (e.g. ~/gc -> /real/city) must resolve
// to the same store scope root as the real path, or the native-store identity
// gate ("database project_id could not be confirmed") rejects it and gc falls
// back to the bd subprocess path.
func TestResolveStoreScopeRootResolvesSymlinks(t *testing.T) {
	realDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "city-link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	got := resolveStoreScopeRoot(link, "")
	want := resolveStoreScopeRoot(realDir, "")
	if got != want {
		t.Fatalf("symlinked city path produced different scope root:\n  link: %s\n  real: %s", got, want)
	}
}
