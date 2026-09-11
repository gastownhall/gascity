package packman

import (
	"fmt"
	"os"
	"os/exec"
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
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
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
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
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
		if reflect.DeepEqual(args, []string{"status", "--porcelain"}) {
			return "", nil
		}
		return "", fmt.Errorf("unexpected git call: %v", args)
	}
	t.Cleanup(func() { runGit = prev })

	got, err := EnsureRepoInCache("", "https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}
	if got != path {
		t.Fatalf("EnsureRepoInCache path = %q, want %q", got, path)
	}
	want := [][]string{
		{"rev-parse", "HEAD"},
		{"status", "--porcelain"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("git calls = %#v, want %#v", calls, want)
	}
}

func TestEnsureRepoInCacheRepairsDirtyMatchingCheckout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
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

	got, err := EnsureRepoInCache("", "https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}
	if got != path {
		t.Fatalf("EnsureRepoInCache path = %q, want %q", got, path)
	}
	want := [][]string{
		{"rev-parse", "HEAD"},
		{"status", "--porcelain"},
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
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
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

	got, err := EnsureRepoInCache("", "https://github.com/example/repo", "abc123")
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
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
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
		case "checkout":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git call: %v", args)
		}
	}
	t.Cleanup(func() { runGit = prev })
	prevNet := runNetworkGit
	runNetworkGit = func(_, _, _ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "clone" {
			target := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(target, "pack.toml"), []byte("[pack]\nname = \"repo\"\nschema = 1\n"), 0o644); err != nil {
				return "", err
			}
			return "", nil
		}
		return "", fmt.Errorf("unexpected network git call: %v", args)
	}
	t.Cleanup(func() { runNetworkGit = prevNet })

	got, err := EnsureRepoInCache("", "https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}
	if got != path {
		t.Fatalf("EnsureRepoInCache path = %q, want %q", got, path)
	}
	want := [][]string{
		{"rev-parse", "HEAD"},
		{"status", "--porcelain"},
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
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	path, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}

	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		switch args[0] {
		case "checkout":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git call: %v", args)
		}
	}
	t.Cleanup(func() { runGit = prev })
	prevNet := runNetworkGit
	runNetworkGit = func(_, _, _ string, args ...string) (string, error) {
		if args[0] == "clone" {
			target := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
				return "", err
			}
			return "", nil
		}
		return "", fmt.Errorf("unexpected network git call: %v", args)
	}
	t.Cleanup(func() { runNetworkGit = prevNet })

	if _, err := EnsureRepoInCache("", "https://github.com/example/repo", "abc123"); err == nil {
		t.Fatal("EnsureRepoInCache succeeded, want pack validation error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cache path still exists after validation failure: %v", err)
	}
}

func TestEnsureRepoInCacheReclonesCacheDirWithoutGit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
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
		case "checkout":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git call: %v", args)
		}
	}
	t.Cleanup(func() { runGit = prev })
	prevNet := runNetworkGit
	runNetworkGit = func(_, _, _ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "clone" {
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
		}
		return "", fmt.Errorf("unexpected network git call: %v", args)
	}
	t.Cleanup(func() { runNetworkGit = prevNet })

	got, err := EnsureRepoInCache("", "https://github.com/example/repo", "abc123")
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
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
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
		case "checkout":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git call: %v", args)
		}
	}
	t.Cleanup(func() { runGit = prev })
	prevNet := runNetworkGit
	runNetworkGit = func(_, _, _ string, args ...string) (string, error) {
		if args[0] == "clone" {
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
		}
		return "", fmt.Errorf("unexpected network git call: %v", args)
	}
	t.Cleanup(func() { runNetworkGit = prevNet })

	got, err := EnsureRepoInCache("", "https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}
	if got != path {
		t.Fatalf("EnsureRepoInCache path = %q, want %q", got, path)
	}
}

// TestDefaultRunGitBlocksDisallowedTransport is the regression for the API
// pack-import SSRF hardening: defaultRunGit drives the attacker-influenced
// clone/ls-remote, so it must constrain git transports. An ext:: URL (which
// would otherwise execute an arbitrary command) must be refused by the
// protocol allowlist rather than run.
func TestDefaultRunGitBlocksDisallowedTransport(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// ext:: is not in the allowlist; git must refuse it before running the
	// command. Without the hardening, git would execute `true` and fail with a
	// different (protocol-parse) error instead.
	_, err := defaultRunGit("", "ls-remote", "ext::true")
	if err == nil {
		t.Fatal("defaultRunGit ran a disallowed ext:: transport; want a protocol block")
	}
	msg := err.Error()
	blocked := strings.Contains(msg, "ext") &&
		(strings.Contains(msg, "not allowed") || strings.Contains(msg, "protocol"))
	if !blocked {
		t.Fatalf("error = %q; want a git transport 'ext' not allowed / protocol block", msg)
	}
}
