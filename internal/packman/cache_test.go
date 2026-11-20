package packman

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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

// useNeutralGitTestEnv isolates real-git cache tests from the host's user
// config and locale so seeding and diagnostics are deterministic.
func useNeutralGitTestEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("LC_ALL", "C")
}

// seedCacheRepo writes a minimal pack into dir, commits it into a fresh
// repository and returns the HEAD commit.
func seedCacheRepo(t *testing.T, dir string) string {
	t.Helper()
	writeFixtureFile(t, dir, "pack.toml", "[pack]\nname = \"tools\"\nschema = 1\n", 0o644)
	writeFixtureFile(t, dir, "formulas/build.toml", "formula = \"build\"\n", 0o644)
	writeFixtureFile(t, dir, ".gitignore", "__pycache__/\n", 0o644)
	fixtureGit(t, dir, "init", "-q")
	fixtureGit(t, dir, "add", ".")
	fixtureGit(t, dir, "commit", "-q", "-m", "seed")
	return fixtureGit(t, dir, "rev-parse", "HEAD")
}

func TestValidateCachedRepoCheckoutIgnoresGitTrace(t *testing.T) {
	useNeutralGitTestEnv(t)
	dir := t.TempDir()
	head := seedCacheRepo(t, dir)

	t.Setenv("GIT_TRACE", "1")
	t.Setenv("GIT_TRACE2_PERF", "1")
	if err := validateCachedRepoCheckout(dir, head); err != nil {
		t.Fatalf("validateCachedRepoCheckout with GIT_TRACE set: %v", err)
	}
	dirty, err := cachedRepoDirty(dir)
	if err != nil {
		t.Fatalf("cachedRepoDirty with GIT_TRACE set: %v", err)
	}
	if dirty {
		t.Fatal("cachedRepoDirty reported a clean cache as dirty with GIT_TRACE set")
	}
}

func TestDefaultRunGitFailureKeepsDiagnostics(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Run("non-zero exit", func(t *testing.T) {
		useNeutralGitTestEnv(t)
		dir := t.TempDir()
		seedCacheRepo(t, dir)
		out, err := defaultRunGit(dir, "rev-parse", "--verify", "refs/heads/missing-revision")
		if out != "" {
			t.Errorf("output = %q, want empty on failure", out)
		}
		if err == nil {
			t.Fatal("defaultRunGit succeeded for a missing revision")
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Errorf("error = %v, want it to wrap *exec.ExitError", err)
		}
		for _, want := range []string{"git rev-parse --verify refs/heads/missing-revision", "fatal:"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err, want)
			}
		}
	})
	t.Run("cannot start", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("PATH", "")
		out, err := defaultRunGit(dir, "rev-parse", "HEAD")
		if out != "" {
			t.Errorf("output = %q, want empty on failure", out)
		}
		if err == nil {
			t.Fatal("defaultRunGit succeeded with git missing from PATH")
		}
		if !errors.Is(err, exec.ErrNotFound) {
			t.Errorf("error = %v, want it to wrap exec.ErrNotFound", err)
		}
		if !strings.Contains(err.Error(), "git rev-parse HEAD") {
			t.Errorf("error = %q, want it to name git rev-parse HEAD", err)
		}
	})
}

// TestCachedRepoDirtyIgnoresStatOnlyRewrite covers the stat-cache claim of
// PR #8723: a tracked file re-rendered with byte-identical content but a new
// inode and mtime reads clean through a single status call, with no index
// refresh, while real content changes still read dirty.
func TestCachedRepoDirtyIgnoresStatOnlyRewrite(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	rewriteFormula := func(t *testing.T, dir string) {
		t.Helper()
		formula := filepath.Join(dir, "formulas", "build.toml")
		tmp := formula + ".tmp"
		if err := os.WriteFile(tmp, []byte("formula = \"build\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, formula); err != nil {
			t.Fatal(err)
		}
		future := time.Now().Add(2 * time.Hour)
		if err := os.Chtimes(formula, future, future); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name      string
		setup     func(t *testing.T, dir string)
		wantDirty bool
	}{
		{name: "writable index", setup: rewriteFormula},
		{name: "held index lock", setup: func(t *testing.T, dir string) {
			rewriteFormula(t, dir)
			writeFixtureFile(t, dir, ".git/index.lock", "", 0o644)
		}},
		{name: "optional locks disabled", setup: func(t *testing.T, dir string) {
			rewriteFormula(t, dir)
			t.Setenv("GIT_OPTIONAL_LOCKS", "0")
		}},
		{name: "modified tracked file", wantDirty: true, setup: func(t *testing.T, dir string) {
			writeFixtureFile(t, dir, "formulas/build.toml", "formula = \"changed\"\n", 0o644)
		}},
		{name: "untracked file", wantDirty: true, setup: func(t *testing.T, dir string) {
			writeFixtureFile(t, dir, "extra.txt", "extra\n", 0o644)
		}},
		{name: "ignored artifact", setup: func(t *testing.T, dir string) {
			writeFixtureFile(t, dir, "__pycache__/x.pyc", "bytecode", 0o644)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useNeutralGitTestEnv(t)
			dir := t.TempDir()
			seedCacheRepo(t, dir)
			tc.setup(t, dir)

			var calls []string
			prev := runGit
			runGit = func(dir string, args ...string) (string, error) {
				calls = append(calls, strings.Join(args, " "))
				return defaultRunGit(dir, args...)
			}
			t.Cleanup(func() { runGit = prev })

			dirty, err := cachedRepoDirty(dir)
			if err != nil {
				t.Fatalf("cachedRepoDirty: %v", err)
			}
			if dirty != tc.wantDirty {
				t.Errorf("cachedRepoDirty = %v, want %v", dirty, tc.wantDirty)
			}
			if want := []string{"status --porcelain"}; !reflect.DeepEqual(calls, want) {
				t.Errorf("git calls = %q, want %q", calls, want)
			}
		})
	}
}
