package packman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestSyncLockFromLockWalksTransitiveImports(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)

	lock := &Lockfile{
		Packs: map[string]LockedPack{
			"https://example.com/a.git": {Version: "1.2.0", Commit: "aaaa", Fetched: time.Unix(10, 0).UTC()},
			"https://example.com/b.git": {Version: "2.0.0", Commit: "bbbb", Fetched: time.Unix(20, 0).UTC()},
		},
	}
	if err := WriteLockfile(fsys.OSFS{}, city, lock); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}

	stageCachedPack(t, "https://example.com/a.git", "aaaa", `
[pack]
name = "a"
schema = 1

[imports.b]
source = "https://example.com/b.git"
version = "^2.0"
`)
	stageCachedPack(t, "https://example.com/b.git", "bbbb", `
[pack]
name = "b"
schema = 1
`)

	got, err := SyncLock(city, map[string]config.Import{
		"a": {Source: "https://example.com/a.git", Version: "^1.0"},
	}, InstallFromLock)
	if err != nil {
		t.Fatalf("SyncLock: %v", err)
	}
	if len(got.Packs) != 2 {
		t.Fatalf("len(Packs) = %d, want 2", len(got.Packs))
	}
}

func TestSyncLockHonorsTransitiveFalse(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)

	lock := &Lockfile{
		Packs: map[string]LockedPack{
			"https://example.com/a.git": {Version: "1.2.0", Commit: "aaaa", Fetched: time.Unix(10, 0).UTC()},
			"https://example.com/b.git": {Version: "2.0.0", Commit: "bbbb", Fetched: time.Unix(20, 0).UTC()},
		},
	}
	if err := WriteLockfile(fsys.OSFS{}, city, lock); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}

	stageCachedPack(t, "https://example.com/a.git", "aaaa", `
[pack]
name = "a"
schema = 1

[imports.b]
source = "https://example.com/b.git"
`)
	stageCachedPack(t, "https://example.com/b.git", "bbbb", `
[pack]
name = "b"
schema = 1
`)

	transitive := false
	got, err := SyncLock(city, map[string]config.Import{
		"a": {Source: "https://example.com/a.git", Transitive: &transitive},
	}, InstallFromLock)
	if err != nil {
		t.Fatalf("SyncLock: %v", err)
	}
	if len(got.Packs) != 1 {
		t.Fatalf("len(Packs) = %d, want 1", len(got.Packs))
	}
}

func TestSyncLockResolveIfNeededResolvesAndCaches(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)

	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		switch args[0] {
		case "ls-remote":
			return "aaaa\trefs/tags/v1.0.0\n", nil
		case "clone":
			target := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(target, "pack.toml"), []byte("[pack]\nname = \"a\"\nschema = 1\n"), 0o644); err != nil {
				return "", err
			}
			return "", nil
		case "checkout":
			return "", nil
		default:
			return "", nil
		}
	}
	t.Cleanup(func() { runGit = prev })

	got, err := SyncLock(city, map[string]config.Import{
		"a": {Source: "https://example.com/a.git", Version: "^1.0"},
	}, InstallResolveIfNeeded)
	if err != nil {
		t.Fatalf("SyncLock: %v", err)
	}
	pack, ok := got.Packs["https://example.com/a.git"]
	if !ok {
		t.Fatal("missing lock entry for a")
	}
	if pack.Version != "1.0.0" || pack.Commit != "aaaa" {
		t.Fatalf("pack = %#v", pack)
	}
}

func TestInstallLockedEnsuresEveryLockedRepo(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)

	if err := WriteLockfile(fsys.OSFS{}, city, &Lockfile{
		Schema: LockfileSchema,
		Packs: map[string]LockedPack{
			"https://example.com/a.git": {Version: "1.0.0", Commit: "aaaa"},
			"https://example.com/b.git": {Version: "2.0.0", Commit: "bbbb"},
		},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}

	var seen []string
	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		switch args[0] {
		case "clone":
			target := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
				return "", err
			}
			seen = append(seen, args[len(args)-2])
			return "", nil
		case "checkout":
			return "", nil
		default:
			return "", nil
		}
	}
	t.Cleanup(func() { runGit = prev })

	lock, err := InstallLocked(city)
	if err != nil {
		t.Fatalf("InstallLocked: %v", err)
	}
	if len(lock.Packs) != 2 {
		t.Fatalf("len(Packs) = %d, want 2", len(lock.Packs))
	}
	if len(seen) != 2 {
		t.Fatalf("cloned %d repos, want 2", len(seen))
	}
}

func TestReadCachedPackImportsUsesSubpath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	source := "file:///tmp/repo.git//packs/base"
	commit := "abc123"
	path, err := RepoCachePath(source, commit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, "packs", "base"), 0o755); err != nil {
		t.Fatalf("MkdirAll(subpath): %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "packs", "base", "pack.toml"), []byte(`
[pack]
name = "base"
schema = 1

[imports.inner]
source = "https://example.com/inner.git"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}

	imports, err := ReadCachedPackImports(source, commit)
	if err != nil {
		t.Fatalf("ReadCachedPackImports: %v", err)
	}
	if _, ok := imports["inner"]; !ok {
		t.Fatalf("missing nested import from subpath pack: %#v", imports)
	}
}

func TestSyncLockConflictingPinnedVersionsError(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)

	_, err := SyncLock(city, map[string]config.Import{
		"a": {Source: "https://example.com/a.git", Version: "sha:aaaa"},
		"b": {Source: "https://example.com/a.git", Version: "sha:bbbb"},
	}, InstallResolveIfNeeded)
	if err == nil {
		t.Fatal("expected conflicting pinned versions error")
	}
	if !strings.Contains(err.Error(), "incompatible pinned versions") {
		t.Fatalf("error = %v", err)
	}
}

func TestSyncLockMergesCompatibleDirectConstraints(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)

	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		switch args[0] {
		case "ls-remote":
			return "aaaa\trefs/tags/v2.0.0\nbbbb\trefs/tags/v1.5.0\n", nil
		case "clone":
			target := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(target, "pack.toml"), []byte("[pack]\nname = \"a\"\nschema = 1\n"), 0o644); err != nil {
				return "", err
			}
			return "", nil
		case "checkout":
			return "", nil
		default:
			return "", nil
		}
	}
	t.Cleanup(func() { runGit = prev })

	lock, err := SyncLock(city, map[string]config.Import{
		"a": {Source: "https://example.com/a.git", Version: ">=1.0"},
		"b": {Source: "https://example.com/a.git", Version: "<2.0"},
	}, InstallResolveIfNeeded)
	if err != nil {
		t.Fatalf("SyncLock: %v", err)
	}
	pack := lock.Packs["https://example.com/a.git"]
	if pack.Version != "1.5.0" {
		t.Fatalf("Version = %q, want %q", pack.Version, "1.5.0")
	}
}

func TestSyncLockMergesDirectAndTransitiveConstraintsBeforeResolution(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)

	if err := WriteLockfile(fsys.OSFS{}, city, &Lockfile{
		Schema: LockfileSchema,
		Packs: map[string]LockedPack{
			"https://example.com/a.git": {Version: "1.0.0", Commit: "aaaa", Fetched: time.Unix(10, 0).UTC()},
			"https://example.com/c.git": {Version: "1.5.0", Commit: "bbbb", Fetched: time.Unix(20, 0).UTC()},
		},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}

	stageCachedPack(t, "https://example.com/a.git", "aaaa", `
[pack]
name = "a"
schema = 1

[imports.c]
source = "https://example.com/c.git"
version = ">=2.0"
`)
	stageCachedPack(t, "https://example.com/c.git", "bbbb", `
[pack]
name = "c"
schema = 1
`)

	_, err := SyncLock(city, map[string]config.Import{
		"a": {Source: "https://example.com/a.git", Version: "^1.0"},
		"c": {Source: "https://example.com/c.git", Version: "<2.0"},
	}, InstallFromLock)
	if err == nil {
		t.Fatal("expected direct/transitive conflict")
	}
	if !strings.Contains(err.Error(), `source "https://example.com/c.git" has conflicting constraints`) {
		t.Fatalf("error = %v", err)
	}
}

func TestSyncLockAllowsMultipleSubpathsFromSameRepoWithSharedClone(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)

	cloneCount := 0
	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		switch args[0] {
		case "ls-remote":
			return "aaaa\trefs/tags/v1.2.3\n", nil
		case "clone":
			cloneCount++
			target := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Join(target, "packs", "a"), 0o755); err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Join(target, "packs", "b"), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(target, "packs", "a", "pack.toml"), []byte("[pack]\nname = \"a\"\nschema = 1\n"), 0o644); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(target, "packs", "b", "pack.toml"), []byte("[pack]\nname = \"b\"\nschema = 1\n"), 0o644); err != nil {
				return "", err
			}
			return "", nil
		case "checkout":
			return "", nil
		default:
			return "", nil
		}
	}
	t.Cleanup(func() { runGit = prev })

	lock, err := SyncLock(city, map[string]config.Import{
		"a": {Source: "file:///tmp/repo.git//packs/a", Version: "^1.2"},
		"b": {Source: "file:///tmp/repo.git//packs/b", Version: "^1.2"},
	}, InstallResolveIfNeeded)
	if err != nil {
		t.Fatalf("SyncLock: %v", err)
	}
	if len(lock.Packs) != 2 {
		t.Fatalf("len(Packs) = %d, want 2", len(lock.Packs))
	}
	if cloneCount != 1 {
		t.Fatalf("cloneCount = %d, want 1 shared clone", cloneCount)
	}
	if lock.Packs["file:///tmp/repo.git//packs/a"].Commit != "aaaa" {
		t.Fatalf("subpath a commit = %q, want aaaa", lock.Packs["file:///tmp/repo.git//packs/a"].Commit)
	}
	if lock.Packs["file:///tmp/repo.git//packs/b"].Commit != "aaaa" {
		t.Fatalf("subpath b commit = %q, want aaaa", lock.Packs["file:///tmp/repo.git//packs/b"].Commit)
	}
}

func stageCachedPack(t *testing.T, source, commit, packToml string) {
	t.Helper()
	path, err := RepoCachePath(source, commit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
}
