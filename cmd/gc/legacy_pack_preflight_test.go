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
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	cityPath := t.TempDir()
	source := config.PublicGastownPackSource
	commit := strings.TrimPrefix(config.PublicGastownPackVersion, "sha:")
	writePreflightImportLock(t, cityPath, commit)

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
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	cityPath := t.TempDir()
	commit := strings.TrimPrefix(config.PublicGastownPackVersion, "sha:")
	writePreflightImportLock(t, cityPath, commit)

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
	writePreflightImportLock(t, cityPath, "")

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
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
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

// writePreflightImportLock pins the public gastown source at commit in a
// fresh packs.lock (every preflight scenario uses that bundled source).
func writePreflightImportLock(t *testing.T, cityPath, commit string) {
	source := config.PublicGastownPackSource
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

// TestEnsureBundledLockedRemoteImportsCachedSkipsNonCanonicalBundledPin
// pins the canonical-pin gate at the preflight: a bundled source locked at
// a non-canonical commit is an ordinary remote import — the preflight must
// neither materialize embedded content for it nor error (gc import install
// owns fetching it).
func TestEnsureBundledLockedRemoteImportsCachedSkipsNonCanonicalBundledPin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	cityPath := t.TempDir()
	writePreflightImportLock(t, cityPath, "0123456789abcdef0123456789abcdef01234567")

	if err := ensureBundledLockedRemoteImportsCached(cityPath); err != nil {
		t.Fatalf("ensureBundledLockedRemoteImportsCached returned error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".gc", "cache", "repos")); !os.IsNotExist(err) {
		t.Fatalf("non-canonical bundled lock entry should not create shared repo cache, stat err = %v", err)
	}
}
