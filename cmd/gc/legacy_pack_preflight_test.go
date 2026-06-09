package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/packman"
)

func TestEnsureBundledLockedRemoteImportsCachedSkipsNonBundledLockEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cityPath := t.TempDir()
	lockToml := `schema = 1

[packs."https://example.com/external.git//pack"]
version = "1.0.0"
commit = "abc123def456abc123def456abc123def456abc123de"
fetched = "2026-01-01T00:00:00Z"
`
	if err := os.WriteFile(filepath.Join(cityPath, packman.LockfileName), []byte(lockToml), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ensureBundledLockedRemoteImportsCached(cityPath); err != nil {
		t.Fatalf("ensureBundledLockedRemoteImportsCached returned error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".gc", "cache", "repos")); !os.IsNotExist(err) {
		t.Fatalf("non-bundled lock entry should not create shared repo cache, stat err = %v", err)
	}
}
