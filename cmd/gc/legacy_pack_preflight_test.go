package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/packman"
)

func TestEnsureBundledLockedRemoteImportsCachedHydratesBundledLockEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cityPath := t.TempDir()
	source := config.PublicGastownPackSource
	commit := strings.TrimPrefix(config.PublicGastownPackVersion, "sha:")
	writePreflightImportLock(t, cityPath, source, commit)

	if err := ensureBundledLockedRemoteImportsCached(cityPath); err != nil {
		t.Fatalf("ensureBundledLockedRemoteImportsCached returned error: %v", err)
	}

	cacheDir := filepath.Join(home, ".gc", "cache", "repos", packman.RepoCacheKey(source, commit))
	if _, err := os.Stat(filepath.Join(cacheDir, ".gc-bundled-pack-cache.toml")); err != nil {
		t.Fatalf("bundled cache marker stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "gastown", "pack.toml")); err != nil {
		t.Fatalf("bundled pack root stat error: %v", err)
	}
}

func TestEnsureBundledLockedRemoteImportsCachedValidatesWarmCacheWithoutWriteLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cityPath := t.TempDir()
	source := config.PublicGastownPackSource
	commit := strings.TrimPrefix(config.PublicGastownPackVersion, "sha:")
	writePreflightImportLock(t, cityPath, source, commit)

	if err := ensureBundledLockedRemoteImportsCached(cityPath); err != nil {
		t.Fatalf("cold hydration returned error: %v", err)
	}

	// Hold the exclusive repo-cache lock while rerunning the preflight. A warm
	// cache must validate lock-free; blocking here means the preflight took the
	// write-locked repair path even though the cache already validates.
	root := filepath.Join(home, ".gc", "cache", "repos")
	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		_, err := config.WithRepoCacheWriteLock(root, func() (string, error) {
			close(locked)
			<-release
			return "", nil
		})
		lockDone <- err
	}()
	<-locked
	defer func() {
		close(release)
		if err := <-lockDone; err != nil {
			t.Errorf("releasing repo cache write lock: %v", err)
		}
	}()

	warm := make(chan error, 1)
	go func() { warm <- ensureBundledLockedRemoteImportsCached(cityPath) }()
	select {
	case err := <-warm:
		if err != nil {
			t.Fatalf("warm hydration returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("warm hydration blocked on the repo-cache write lock; want lock-free validation when the cache already validates")
	}
}

func TestEnsureBundledLockedRemoteImportsCachedRejectsBundledLockEntryWithoutCommit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cityPath := t.TempDir()
	source := config.PublicGastownPackSource
	writePreflightImportLock(t, cityPath, source, "")

	err := ensureBundledLockedRemoteImportsCached(cityPath)
	if err == nil {
		t.Fatal("ensureBundledLockedRemoteImportsCached succeeded, want missing commit error")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("lock entry %q is missing commit", source)) {
		t.Fatalf("error = %v, want missing commit detail", err)
	}
}

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

func writePreflightImportLock(t *testing.T, cityPath, source, commit string) {
	t.Helper()
	lockToml := fmt.Sprintf(`schema = 1

[packs.%q]
version = "1.0.0"
commit = %q
fetched = "2026-01-01T00:00:00Z"
`, source, commit)
	if err := os.WriteFile(filepath.Join(cityPath, packman.LockfileName), []byte(lockToml), 0o644); err != nil {
		t.Fatal(err)
	}
}
