package packman

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRepoCacheKeyDeterministic(t *testing.T) {
	a := RepoCacheKey("https://github.com/example/repo", "abc123")
	b := RepoCacheKey("https://github.com/example/repo", "abc123")
	c := RepoCacheKey("https://github.com/example/repo", "def456")
	if a != b {
		t.Fatalf("equal inputs produced different keys: %q != %q", a, b)
	}
	if a == c {
		t.Fatalf("different commits produced same key: %q", a)
	}
}

func TestRepoCachePathUsesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if !strings.HasPrefix(got, filepath.Join(home, ".gc", "cache", "repos")) {
		t.Fatalf("RepoCachePath = %q", got)
	}
}

func TestRepoCacheKeyNormalizesSubpathSources(t *testing.T) {
	plain := RepoCacheKey("file:///tmp/repo.git", "abc123")
	subpath := RepoCacheKey("file:///tmp/repo.git//packs/base", "abc123")
	if plain != subpath {
		t.Fatalf("RepoCacheKey should ignore subpath for cache identity: %q != %q", plain, subpath)
	}
}

func TestRepoCacheKeyNormalizesGitHubShortcut(t *testing.T) {
	shortcut := RepoCacheKey("github.com/example/repo", "abc123")
	https := RepoCacheKey("https://github.com/example/repo", "abc123")
	if shortcut != https {
		t.Fatalf("RepoCacheKey should normalize bare github shortcut: %q != %q", shortcut, https)
	}
}

func TestEnsureRepoInCacheUsesExistingCloneWhenCheckoutMatches(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "pack.toml"), []byte("[pack]\nname = \"repo\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}

	var calls [][]string
	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if reflect.DeepEqual(args, []string{"rev-parse", "HEAD"}) {
			return "abc123", nil
		}
		if reflect.DeepEqual(args, []string{"status", "--porcelain", "--ignored"}) {
			return "", nil
		}
		return "", fmt.Errorf("unexpected git call: %v", args)
	}
	t.Cleanup(func() { runGit = prev })

	got, err := EnsureRepoInCache("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}
	if got != path {
		t.Fatalf("EnsureRepoInCache path = %q, want %q", got, path)
	}
	want := [][]string{
		{"rev-parse", "HEAD"},
		{"status", "--porcelain", "--ignored"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("git calls = %#v, want %#v", calls, want)
	}
}

func TestEnsureRepoInCacheRepairsDirtyMatchingCheckout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "pack.toml"), []byte("not toml"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}

	var calls [][]string
	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "rev-parse":
			return "abc123", nil
		case "status":
			return " M pack.toml", nil
		case "reset":
			if err := os.WriteFile(filepath.Join(path, "pack.toml"), []byte("[pack]\nname = \"repo\"\nschema = 1\n"), 0o644); err != nil {
				return "", err
			}
			return "", nil
		case "clean":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git call: %v", args)
		}
	}
	t.Cleanup(func() { runGit = prev })

	got, err := EnsureRepoInCache("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}
	if got != path {
		t.Fatalf("EnsureRepoInCache path = %q, want %q", got, path)
	}
	want := [][]string{
		{"rev-parse", "HEAD"},
		{"status", "--porcelain", "--ignored"},
		{"reset", "--hard", "--quiet", "abc123"},
		{"clean", "-ffdx", "--quiet"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("git calls = %#v, want %#v", calls, want)
	}
}

func TestEnsureRepoInCacheRepairsExistingCloneCheckout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "pack.toml"), []byte("[pack]\nname = \"repo\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}

	var calls [][]string
	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "rev-parse":
			return "def456", nil
		case "checkout":
			return "", nil
		case "reset":
			return "", nil
		case "clean":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git call: %v", args)
		}
	}
	t.Cleanup(func() { runGit = prev })

	got, err := EnsureRepoInCache("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}
	if got != path {
		t.Fatalf("EnsureRepoInCache path = %q, want %q", got, path)
	}
	want := [][]string{
		{"rev-parse", "HEAD"},
		{"checkout", "--quiet", "abc123"},
		{"reset", "--hard", "--quiet", "abc123"},
		{"clean", "-ffdx", "--quiet"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("git calls = %#v, want %#v", calls, want)
	}
}

func TestEnsureRepoInCacheReclonesInvalidExistingCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	var calls [][]string
	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "rev-parse":
			return "abc123", nil
		case "status":
			return "", nil
		case "clone":
			target := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(target, "pack.toml"), []byte("[pack]\nname = \"repo\"\nschema = 1\n"), 0o644); err != nil {
				return "", err
			}
			return "", nil
		case "checkout":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git call: %v", args)
		}
	}
	t.Cleanup(func() { runGit = prev })

	got, err := EnsureRepoInCache("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}
	if got != path {
		t.Fatalf("EnsureRepoInCache path = %q, want %q", got, path)
	}
	want := [][]string{
		{"rev-parse", "HEAD"},
		{"status", "--porcelain", "--ignored"},
		{"clone", "--quiet", "https://github.com/example/repo", path},
		{"checkout", "--quiet", "abc123"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("git calls = %#v, want %#v", calls, want)
	}
}

func TestEnsureRepoInCacheCleansFreshCloneAfterPackValidationFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}

	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		switch args[0] {
		case "clone":
			target := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
				return "", err
			}
			return "", nil
		case "checkout":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git call: %v", args)
		}
	}
	t.Cleanup(func() { runGit = prev })

	if _, err := EnsureRepoInCache("https://github.com/example/repo", "abc123"); err == nil {
		t.Fatal("EnsureRepoInCache succeeded, want pack validation error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cache path still exists after validation failure: %v", err)
	}
}

func TestEnsureRepoInCacheReclonesCacheDirWithoutGit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "leftover.txt"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("WriteFile(leftover): %v", err)
	}

	var calls [][]string
	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "clone":
			target := args[len(args)-1]
			if _, err := os.Stat(filepath.Join(target, "leftover.txt")); !os.IsNotExist(err) {
				return "", fmt.Errorf("stale cache directory was not removed before clone")
			}
			if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(target, "pack.toml"), []byte("[pack]\nname = \"repo\"\nschema = 1\n"), 0o644); err != nil {
				return "", err
			}
			return "", nil
		case "checkout":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git call: %v", args)
		}
	}
	t.Cleanup(func() { runGit = prev })

	got, err := EnsureRepoInCache("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}
	if got != path {
		t.Fatalf("EnsureRepoInCache path = %q, want %q", got, path)
	}
	want := [][]string{
		{"clone", "--quiet", "https://github.com/example/repo", path},
		{"checkout", "--quiet", "abc123"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("git calls = %#v, want %#v", calls, want)
	}
}

func TestEnsureRepoInCacheReclonesCacheFileWithoutGit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatalf("WriteFile(cachePath): %v", err)
	}

	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		switch args[0] {
		case "clone":
			target := args[len(args)-1]
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				return "", fmt.Errorf("stale cache file was not removed before clone")
			}
			if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(target, "pack.toml"), []byte("[pack]\nname = \"repo\"\nschema = 1\n"), 0o644); err != nil {
				return "", err
			}
			return "", nil
		case "checkout":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git call: %v", args)
		}
	}
	t.Cleanup(func() { runGit = prev })

	got, err := EnsureRepoInCache("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}
	if got != path {
		t.Fatalf("EnsureRepoInCache path = %q, want %q", got, path)
	}
}

func TestNeedsCredentialsCoversNetworkSubcommands(t *testing.T) {
	for _, sub := range []string{"clone", "fetch", "ls-remote", "push"} {
		if !needsCredentials(sub) {
			t.Errorf("needsCredentials(%q) = false, want true", sub)
		}
	}
	for _, sub := range []string{"checkout", "reset", "clean", "status", "rev-parse", "merge", "rebase"} {
		if needsCredentials(sub) {
			t.Errorf("needsCredentials(%q) = true, want false", sub)
		}
	}
}

func TestHermeticGitEnvSilencesGlobalConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	env := hermeticGitEnv()
	if !containsEnv(env, "GIT_CONFIG_NOSYSTEM=1") {
		t.Errorf("hermeticGitEnv() missing GIT_CONFIG_NOSYSTEM=1; env=%v", env)
	}
	if !containsEnv(env, "GIT_CONFIG_GLOBAL=/dev/null") {
		t.Errorf("hermeticGitEnv() missing GIT_CONFIG_GLOBAL=/dev/null; env=%v", env)
	}
}

func TestCredentialAwareGitEnvAllowsGlobalConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	env := credentialAwareGitEnv()
	for _, banned := range []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"} {
		if containsEnv(env, banned) {
			t.Errorf("credentialAwareGitEnv() set %q; credential helpers would not apply", banned)
		}
	}
}

func TestSanitizedGitEnvDropsDangerousOverrides(t *testing.T) {
	t.Setenv("GIT_DIR", "/tmp/should-not-leak")
	t.Setenv("GIT_CONFIG_GLOBAL", "/tmp/poisoned-config")
	env := sanitizedGitEnv()
	for _, banned := range []string{"GIT_DIR=", "GIT_CONFIG_GLOBAL="} {
		for _, e := range env {
			if strings.HasPrefix(e, banned) {
				t.Errorf("sanitizedGitEnv() inherited %q from caller environment", e)
			}
		}
	}
}

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}
