package packman

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// stubCachedPackGitFake returns a runGit fake that materializes a cached pack on
// clone and answers checkout/rev-parse/status, but FAILS the test if the
// tag-listing path (`ls-remote`) is consulted. Tests use it to prove that the
// injected registry resolver short-circuits git-tag resolution.
func stubCachedPackGitNoTags(t *testing.T) {
	t.Helper()
	prev := runGit
	runGit = func(dir string, args ...string) (string, error) {
		switch args[0] {
		case "ls-remote":
			t.Fatalf("ls-remote consulted; registry resolver should bypass tag discovery (args=%v)", args)
			return "", nil
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
}

func TestSyncLockUsesRegistryResolverOverTags(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	stubCachedPackGitNoTags(t)

	const source = "https://example.com/contributing.git"
	SetVersionResolver(func(src, constraint string) (ResolvedVersion, bool, error) {
		if src != source || constraint != ">=0.2.1" {
			return ResolvedVersion{}, false, nil
		}
		return ResolvedVersion{Version: "0.2.1", Commit: "regcommit"}, true, nil
	})
	t.Cleanup(func() { SetVersionResolver(nil) })

	got, err := SyncLock(city, map[string]config.Import{
		"contributing": {Source: source, Version: ">=0.2.1"},
	}, InstallResolveIfNeeded)
	if err != nil {
		t.Fatalf("SyncLock: %v", err)
	}
	pack, ok := got.Packs[source]
	if !ok {
		t.Fatalf("missing lock entry for %q: %#v", source, got.Packs)
	}
	if pack.Version != "0.2.1" || pack.Commit != "regcommit" {
		t.Fatalf("pack = %#v, want registry release {0.2.1 regcommit}", pack)
	}
}

func TestSyncLockFallsBackToTagsWhenResolverUnknown(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))

	prev := runGit
	runGit = func(dir string, args ...string) (string, error) {
		switch args[0] {
		case "ls-remote":
			return "tagcommit\trefs/tags/v1.0.0\n", nil
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
		default:
			return "", nil
		}
	}
	t.Cleanup(func() { runGit = prev })

	SetVersionResolver(func(string, string) (ResolvedVersion, bool, error) {
		return ResolvedVersion{}, false, nil // unknown to any registry
	})
	t.Cleanup(func() { SetVersionResolver(nil) })

	got, err := SyncLock(city, map[string]config.Import{
		"a": {Source: "https://example.com/a.git", Version: "^1.0"},
	}, InstallResolveIfNeeded)
	if err != nil {
		t.Fatalf("SyncLock: %v", err)
	}
	pack := got.Packs["https://example.com/a.git"]
	if pack.Version != "1.0.0" || pack.Commit != "tagcommit" {
		t.Fatalf("pack = %#v, want tag-resolved {1.0.0 tagcommit}", pack)
	}
}

func TestSyncLockResolverErrorPropagates(t *testing.T) {
	home := t.TempDir()
	city := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	stubCachedPackGitNoTags(t)

	sentinel := errors.New("no release satisfies >=9.9.9")
	SetVersionResolver(func(string, string) (ResolvedVersion, bool, error) {
		return ResolvedVersion{}, false, sentinel
	})
	t.Cleanup(func() { SetVersionResolver(nil) })

	_, err := SyncLock(city, map[string]config.Import{
		"contributing": {Source: "https://example.com/packs//contributing", Version: ">=9.9.9"},
	}, InstallResolveIfNeeded)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel propagated (fail-closed)", err)
	}
}
