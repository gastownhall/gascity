package packman

import (
	"fmt"
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
	stubCachedPackGit(t)

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
	stubCachedPackGit(t)

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

func TestSyncLockExpandsRepeatedSourceWhenAnyImportIsTransitive(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	stubCachedPackGit(t)

	lock := &Lockfile{
		Packs: map[string]LockedPack{
			"https://example.com/shared.git": {Version: "1.0.0", Commit: "aaaa", Fetched: time.Unix(10, 0).UTC()},
			"https://example.com/inner.git":  {Version: "1.0.0", Commit: "bbbb", Fetched: time.Unix(20, 0).UTC()},
		},
	}
	if err := WriteLockfile(fsys.OSFS{}, city, lock); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}
	stageCachedPack(t, "https://example.com/shared.git", "aaaa", `
[pack]
name = "shared"
schema = 1

[imports.inner]
source = "https://example.com/inner.git"
version = "^1.0"
`)
	stageCachedPack(t, "https://example.com/inner.git", "bbbb", `
[pack]
name = "inner"
schema = 1
`)

	transitiveFalse := false
	got, err := SyncLock(city, map[string]config.Import{
		"a": {Source: "https://example.com/shared.git", Version: "^1.0", Transitive: &transitiveFalse},
		"z": {Source: "https://example.com/shared.git", Version: "^1.0"},
	}, InstallFromLock)
	if err != nil {
		t.Fatalf("SyncLock: %v", err)
	}
	if len(got.Packs) != 2 {
		t.Fatalf("len(Packs) = %d, want 2", len(got.Packs))
	}
}

func TestSyncLockResolveIfNeededResolvesAndCaches(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)

	prev := runGit
	runGit = func(dir string, args ...string) (string, error) {
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
			writeCachedPackCommit(t, dir, args[len(args)-1])
			return "", nil
		case "rev-parse":
			data, err := os.ReadFile(filepath.Join(dir, ".packman-test-commit"))
			if err != nil {
				return "", err
			}
			return string(data), nil
		case "status":
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
			if err := os.WriteFile(filepath.Join(target, "pack.toml"), []byte("[pack]\nname = \"cached\"\nschema = 1\n"), 0o644); err != nil {
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
	stubCachedPackGit(t)

	source := "file:///tmp/repo.git//packs/base"
	commit := "abc123"
	path, err := RepoCachePath(source, commit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.git): %v", err)
	}
	writeCachedPackCommit(t, path, commit)
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

func TestReadCachedPackImportsRejectsMissingGitHead(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(path, "packs", "base", "pack.toml"), []byte("[pack]\nname = \"base\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}

	_, err = ReadCachedPackImports(source, commit)
	if err == nil {
		t.Fatal("ReadCachedPackImports succeeded for cache with missing .git/HEAD")
	}
	if !strings.Contains(err.Error(), "reading cached repo HEAD") {
		t.Fatalf("error = %v, want cached repo HEAD failure", err)
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
	runGit = func(dir string, args ...string) (string, error) {
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
			writeCachedPackCommit(t, dir, args[len(args)-1])
			return "", nil
		case "rev-parse":
			data, err := os.ReadFile(filepath.Join(dir, ".packman-test-commit"))
			if err != nil {
				return "", err
			}
			return string(data), nil
		case "status":
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

func TestSyncLockSelectiveUpgradeMergesSameSourceConstraints(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)

	prev := runGit
	runGit = func(dir string, args ...string) (string, error) {
		switch args[0] {
		case "ls-remote":
			return "cccc\trefs/tags/v2.0.0\nbbbb\trefs/tags/v1.5.0\n", nil
		case "clone":
			target := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(target, "pack.toml"), []byte("[pack]\nname = \"shared\"\nschema = 1\n"), 0o644); err != nil {
				return "", err
			}
			return "", nil
		case "checkout":
			writeCachedPackCommit(t, dir, args[len(args)-1])
			return "", nil
		case "rev-parse":
			data, err := os.ReadFile(filepath.Join(dir, ".packman-test-commit"))
			if err != nil {
				return "", err
			}
			return string(data), nil
		case "status":
			return "", nil
		default:
			return "", nil
		}
	}
	t.Cleanup(func() { runGit = prev })

	lock, err := SyncLockSelectiveUpgrade(city, map[string]config.Import{
		"pack:shared":         {Source: "https://example.com/shared.git", Version: ">=1.0"},
		"rig:frontend:shared": {Source: "https://example.com/shared.git", Version: "<2.0"},
	}, map[string]struct{}{
		"https://example.com/shared.git": {},
	})
	if err != nil {
		t.Fatalf("SyncLockSelectiveUpgrade: %v", err)
	}
	pack := lock.Packs["https://example.com/shared.git"]
	if pack.Version != "1.5.0" {
		t.Fatalf("Version = %q, want %q", pack.Version, "1.5.0")
	}
}

func TestSyncLockMergesDirectAndTransitiveConstraintsBeforeResolution(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	stubCachedPackGit(t)

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

func TestSyncLockInstallUpgradeReconcilesCompatibleConstraintsAcrossScopes(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)

	if err := WriteLockfile(fsys.OSFS{}, city, &Lockfile{
		Schema: LockfileSchema,
		Packs: map[string]LockedPack{
			"https://example.com/shared.git":   {Version: "1.0.0", Commit: "aaaa", Fetched: time.Unix(10, 0).UTC()},
			"https://example.com/consumer.git": {Version: "1.0.0", Commit: "dddd", Fetched: time.Unix(20, 0).UTC()},
		},
	}); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}

	stageCachedPack(t, "https://example.com/shared.git", "aaaa", `
[pack]
name = "shared"
schema = 1
`)
	stageCachedPack(t, "https://example.com/shared.git", "bbbb", `
[pack]
name = "shared"
schema = 1
`)
	stageCachedPack(t, "https://example.com/shared.git", "cccc", `
[pack]
name = "shared"
schema = 1
`)
	stageCachedPack(t, "https://example.com/consumer.git", "dddd", `
[pack]
name = "consumer"
schema = 1

[imports.shared]
source = "https://example.com/shared.git"
version = "<2.0"
`)

	stubCachedPackGit(t)
	prev := runGit
	runGit = func(dir string, args ...string) (string, error) {
		switch args[0] {
		case "ls-remote":
			switch args[len(args)-1] {
			case "https://example.com/shared.git":
				return "cccc\trefs/tags/v2.0.0\nbbbb\trefs/tags/v1.5.0\naaaa\trefs/tags/v1.0.0\n", nil
			case "https://example.com/consumer.git":
				return "dddd\trefs/tags/v1.0.0\n", nil
			default:
				return "", nil
			}
		default:
			return prev(dir, args...)
		}
	}
	t.Cleanup(func() { runGit = prev })

	lock, err := SyncLock(city, map[string]config.Import{
		"a_shared":   {Source: "https://example.com/shared.git", Version: ">=1.0"},
		"z_consumer": {Source: "https://example.com/consumer.git", Version: "^1.0"},
	}, InstallUpgrade)
	if err != nil {
		t.Fatalf("SyncLock: %v", err)
	}

	pack := lock.Packs["https://example.com/shared.git"]
	if pack.Version != "1.5.0" {
		t.Fatalf("Version = %q, want %q", pack.Version, "1.5.0")
	}
}

func TestSyncLockConvergesForDeepTransitiveChains(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	stubCachedPackGit(t)

	lock := &Lockfile{
		Schema: LockfileSchema,
		Packs:  make(map[string]LockedPack),
	}
	for i := 0; i <= 10; i++ {
		source := fmt.Sprintf("https://example.com/p%d.git", i)
		commit := fmt.Sprintf("c%d", i)
		lock.Packs[source] = LockedPack{
			Version: "1.0.0",
			Commit:  commit,
			Fetched: time.Unix(int64(i+1), 0).UTC(),
		}

		packToml := "[pack]\nname = \"p\"\nschema = 1\n"
		if i < 10 {
			packToml += fmt.Sprintf("\n[imports.next]\nsource = %q\nversion = \"^1.0\"\n", fmt.Sprintf("https://example.com/p%d.git", i+1))
		}
		stageCachedPack(t, source, commit, packToml)
	}
	if err := WriteLockfile(fsys.OSFS{}, city, lock); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}

	got, err := SyncLock(city, map[string]config.Import{
		"root": {Source: "https://example.com/p0.git", Version: "^1.0"},
	}, InstallFromLock)
	if err != nil {
		t.Fatalf("SyncLock: %v", err)
	}
	if len(got.Packs) != 11 {
		t.Fatalf("len(Packs) = %d, want 11", len(got.Packs))
	}
}

func TestSyncLockAllowsMultipleSubpathsFromSameRepoWithSharedClone(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)

	cloneCount := 0
	prev := runGit
	runGit = func(dir string, args ...string) (string, error) {
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
			writeCachedPackCommit(t, dir, args[len(args)-1])
			return "", nil
		case "rev-parse":
			data, err := os.ReadFile(filepath.Join(dir, ".packman-test-commit"))
			if err != nil {
				return "", err
			}
			return string(data), nil
		case "status":
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
	writeCachedPackCommit(t, path, commit)
	if err := os.WriteFile(filepath.Join(path, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
}
