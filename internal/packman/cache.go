// Package packman resolves, caches, and pins remote pack imports.
package packman

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	gitutil "github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/remotesource"
)

var (
	runGit                   = defaultRunGit
	materializeSyntheticRepo = builtinpacks.MaterializeSyntheticRepo
)

// RepoCacheRoot returns the shared machine-local repo cache root.
func RepoCacheRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, ".gc", "cache", "repos"), nil
}

// RepoCacheKey returns the canonical source+commit cache key.
// Delegates to config.RepoCacheKey for canonical normalization so
// the loader and packman always agree on cache paths.
func RepoCacheKey(source, commit string) string {
	return config.RepoCacheKey(source, commit)
}

// RepoCachePath returns the cache path for a specific source+commit pair.
func RepoCachePath(source, commit string) (string, error) {
	root, err := RepoCacheRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, RepoCacheKey(source, commit)), nil
}

// EnsureRepoInCache clones and checks out the requested commit when absent,
// or repairs an existing cache whose checkout has drifted from the lock entry.
func EnsureRepoInCache(source, commit string) (string, error) {
	parsed := normalizeRemoteSource(source)
	cachePath, err := RepoCachePath(source, commit)
	if err != nil {
		return "", err
	}
	root, err := RepoCacheRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("creating repo cache root: %w", err)
	}
	return config.WithRepoCacheWriteLock(root, func() (string, error) {
		if builtinpacks.IsSource(source) {
			return ensureBundledRepoInCacheLocked(source, commit, cachePath)
		}
		return ensureRepoInCacheLocked(source, commit, parsed, cachePath)
	})
}

func ensureBundledRepoInCacheLocked(source, commit, cachePath string) (string, error) {
	validationErr := builtinpacks.ValidateSyntheticRepo(cachePath, commit)
	if validationErr == nil {
		if err := validateCachedPackRoot(source, cachePath); err != nil {
			return "", err
		}
		return cachePath, nil
	}

	recoveryCause := validationErr
	gitInfo, gitErr := os.Stat(filepath.Join(cachePath, ".git"))
	if gitErr == nil && !gitutil.MissingCheckoutMarker(gitInfo, gitErr) {
		if err := checkoutExistingCache(cachePath, commit); err == nil {
			if err := validateCachedPackRoot(source, cachePath); err != nil {
				recoveryCause = err
				if removeErr := os.RemoveAll(cachePath); removeErr != nil {
					return "", fmt.Errorf("removing invalid bundled repo cache %q after %w: %w", cachePath, err, removeErr)
				}
			} else {
				return cachePath, nil
			}
		} else {
			recoveryCause = err
			if removeErr := os.RemoveAll(cachePath); removeErr != nil {
				return "", fmt.Errorf("removing stale bundled repo cache %q after %w: %w", cachePath, err, removeErr)
			}
		}
	} else if gitErr != nil && !gitutil.MissingCheckoutMarker(gitInfo, gitErr) {
		return "", fmt.Errorf("checking bundled repo cache %q: %w", cachePath, gitErr)
	}
	if err := materializeBundledRepoInCacheLocked(source, commit, cachePath); err != nil {
		return "", fmt.Errorf("materializing bundled repo cache %q after %w: %w", cachePath, recoveryCause, err)
	}
	if err := validateCachedPackRoot(source, cachePath); err != nil {
		return "", fmt.Errorf("validating rematerialized bundled repo cache %q after %w: %w", cachePath, recoveryCause, err)
	}
	return cachePath, nil
}

func ensureRepoInCacheLocked(source, commit string, parsed remoteSource, cachePath string) (string, error) {
	if gitInfo, err := os.Stat(filepath.Join(cachePath, ".git")); err == nil && !gitutil.MissingCheckoutMarker(gitInfo, err) {
		if err := checkoutExistingCache(cachePath, commit); err == nil {
			if err := validateCachedPackRoot(source, cachePath); err != nil {
				if removeErr := os.RemoveAll(cachePath); removeErr != nil {
					return "", fmt.Errorf("removing invalid repo cache %q after %w: %w", cachePath, err, removeErr)
				}
			} else {
				return cachePath, nil
			}
		} else if err := os.RemoveAll(cachePath); err != nil {
			return "", fmt.Errorf("removing stale repo cache %q: %w", cachePath, err)
		}
	} else if gitutil.MissingCheckoutMarker(gitInfo, err) {
		if _, statErr := os.Stat(cachePath); statErr == nil {
			if removeErr := os.RemoveAll(cachePath); removeErr != nil {
				return "", fmt.Errorf("removing invalid repo cache %q: %w", cachePath, removeErr)
			}
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return "", fmt.Errorf("checking repo cache %q: %w", cachePath, statErr)
		}
	} else if err != nil {
		return "", fmt.Errorf("checking repo cache %q: %w", cachePath, err)
	}

	if _, err := runGit("", "clone", "--quiet", parsed.CloneURL, cachePath); err != nil {
		return "", fmt.Errorf("cloning %q: %w", source, err)
	}
	if _, err := runGit(cachePath, "checkout", "--quiet", commit); err != nil {
		return "", fmt.Errorf("checking out %q: %w", commit, err)
	}
	if err := validateCachedPackRoot(source, cachePath); err != nil {
		if removeErr := os.RemoveAll(cachePath); removeErr != nil {
			return "", fmt.Errorf("removing invalid repo cache %q after %w: %w", cachePath, err, removeErr)
		}
		return "", err
	}
	return cachePath, nil
}

func materializeBundledRepoInCacheLocked(source, commit, cachePath string) error {
	expected, err := RepoCachePath(source, commit)
	if err != nil {
		return err
	}
	if cachePath != expected {
		return fmt.Errorf("refusing to materialize bundled repo cache at non-canonical path %q, expected %q", cachePath, expected)
	}
	return materializeSyntheticRepo(cachePath, commit)
}

func withRepoCacheReadLock(fn func() error) error {
	root, err := RepoCacheRoot()
	if err != nil {
		return err
	}
	return config.WithRepoCacheReadLock(root, fn)
}

func checkoutExistingCache(cachePath, commit string) error {
	head, headErr := runGit(cachePath, "rev-parse", "HEAD")
	if headErr == nil && gitutil.SameCommit(head, commit) {
		dirty, err := cachedRepoDirty(cachePath)
		if err != nil {
			return err
		}
		if !dirty {
			return nil
		}
		return resetCachedRepo(cachePath, commit)
	}
	if _, err := runGit(cachePath, "checkout", "--quiet", commit); err != nil {
		if headErr != nil {
			return fmt.Errorf("reading cached repo HEAD: %w; checking out %q: %w", headErr, commit, err)
		}
		return fmt.Errorf("checking out %q in cached repo: %w", commit, err)
	}
	return resetCachedRepo(cachePath, commit)
}

func cachedRepoDirty(cachePath string) (bool, error) {
	// Intentionally NOT --ignored: gitignored build artifacts written into
	// the cache in place (e.g. Python __pycache__/*.pyc from running a cached
	// pack's scripts, or a stray .DS_Store) are not local modifications to the
	// pack's tracked content and must not count as "dirty" — they recur and
	// would otherwise wedge the city behind a perpetual "run gc import install"
	// gate (vp-gny3). A reinstall's `git clean -ffdx` still clears them.
	status, err := runGit(cachePath, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("checking cached repo worktree status: %w", err)
	}
	return strings.TrimSpace(status) != "", nil
}

func validateCachedRepoCheckout(cachePath, commit string) error {
	head, err := runGit(cachePath, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("reading cached repo HEAD: %w", err)
	}
	if !gitutil.SameCommit(head, commit) {
		return fmt.Errorf("cached repository is checked out at %s, expected %s", strings.TrimSpace(head), commit)
	}
	dirty, err := cachedRepoDirty(cachePath)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("cached repository has local worktree changes")
	}
	return nil
}

func resetCachedRepo(cachePath, commit string) error {
	if _, err := runGit(cachePath, "reset", "--hard", "--quiet", commit); err != nil {
		return fmt.Errorf("resetting cached repo to %q: %w", commit, err)
	}
	if _, err := runGit(cachePath, "clean", "-ffdx", "--quiet"); err != nil {
		return fmt.Errorf("cleaning cached repo: %w", err)
	}
	return nil
}

func validateCachedPackRoot(source, cachePath string) error {
	packPath := filepath.Join(cachedPackDir(source, cachePath), "pack.toml")
	st, err := os.Stat(packPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("cached pack %q is missing pack.toml at %s", source, packPath)
		}
		return fmt.Errorf("checking cached pack %q at %s: %w", source, packPath, err)
	}
	if st.IsDir() {
		return fmt.Errorf("cached pack %q has directory where pack.toml is expected at %s", source, packPath)
	}
	return nil
}

type remoteSource struct {
	CloneURL string
	Subpath  string
}

func normalizeRemoteSource(source string) remoteSource {
	parsed := remotesource.Parse(source)
	return remoteSource{CloneURL: parsed.CloneURL, Subpath: parsed.Subpath}
}

func defaultRunGit(dir string, args ...string) (string, error) {
	cmdArgs := append([]string{
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.untrackedCache=false",
	}, args...)
	cmd := exec.Command("git", cmdArgs...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = gitutil.HermeticEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}
