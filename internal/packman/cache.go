// Package packman resolves, caches, and pins remote pack imports.
package packman

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gastownhall/gascity/internal/config"
)

var runGit = defaultRunGit

// RepoCacheRoot returns the shared machine-local cache root for URL+commit clones.
func RepoCacheRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, ".gc", "cache", "repos"), nil
}

// RepoCacheKey returns the sha256(url+commit) cache key.
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
		return ensureRepoInCacheLocked(source, commit, parsed, cachePath)
	})
}

func ensureRepoInCacheLocked(source, commit string, parsed remoteSource, cachePath string) (string, error) {
	if _, err := os.Stat(filepath.Join(cachePath, ".git")); err == nil {
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
	} else if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
		if _, statErr := os.Stat(cachePath); statErr == nil {
			if removeErr := os.RemoveAll(cachePath); removeErr != nil {
				return "", fmt.Errorf("removing invalid repo cache %q: %w", cachePath, removeErr)
			}
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return "", fmt.Errorf("checking repo cache %q: %w", cachePath, statErr)
		}
	} else if err != nil && !os.IsNotExist(err) {
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

func withRepoCacheReadLock(fn func() error) error {
	root, err := RepoCacheRoot()
	if err != nil {
		return err
	}
	return config.WithRepoCacheReadLock(root, fn)
}

func checkoutExistingCache(cachePath, commit string) error {
	head, headErr := runGit(cachePath, "rev-parse", "HEAD")
	if headErr == nil && sameCommit(head, commit) {
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
	status, err := runGit(cachePath, "status", "--porcelain", "--ignored")
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
	if !sameCommit(head, commit) {
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
	if strings.Contains(source, "github.com/") && strings.Contains(source, "/tree/") {
		return parseGitHubTreeSource(source)
	}
	if strings.HasPrefix(source, "github.com/") {
		return remoteSource{CloneURL: "https://" + source}
	}
	return parsePackmanRemoteSource(source)
}

func parsePackmanRemoteSource(source string) remoteSource {
	withoutRef := source
	if i := strings.LastIndex(withoutRef, "#"); i >= 0 {
		withoutRef = withoutRef[:i]
	}

	searchFrom := 0
	if idx := strings.Index(withoutRef, "://"); idx >= 0 {
		searchFrom = idx + 3
	}
	if i := strings.Index(withoutRef[searchFrom:], "//"); i >= 0 {
		pos := searchFrom + i
		return remoteSource{
			CloneURL: withoutRef[:pos],
			Subpath:  withoutRef[pos+2:],
		}
	}
	return remoteSource{CloneURL: withoutRef}
}

func parseGitHubTreeSource(source string) remoteSource {
	u := source
	scheme := ""
	if idx := strings.Index(u, "://"); idx >= 0 {
		scheme = u[:idx+3]
		u = u[idx+3:]
	}
	parts := strings.SplitN(u, "/", 6)
	if len(parts) < 5 {
		return remoteSource{CloneURL: source}
	}
	cloneURL := scheme + parts[0] + "/" + parts[1] + "/" + parts[2] + ".git"
	if len(parts) > 5 {
		return remoteSource{CloneURL: cloneURL, Subpath: parts[5]}
	}
	return remoteSource{CloneURL: cloneURL}
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
	for _, e := range os.Environ() {
		if k, _, ok := strings.Cut(e, "="); ok && fetchGitEnvBlacklist[k] {
			continue
		}
		cmd.Env = append(cmd.Env, e)
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

var fetchGitEnvBlacklist = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_COMMON_DIR":                   true,
	"GIT_CEILING_DIRECTORIES":          true,
	"GIT_DISCOVERY_ACROSS_FILESYSTEM":  true,
	"GIT_NAMESPACE":                    true,
	"GIT_CONFIG":                       true,
	"GIT_CONFIG_GLOBAL":                true,
	"GIT_CONFIG_SYSTEM":                true,
	"GIT_CONFIG_NOSYSTEM":              true,
	"GIT_CONFIG_COUNT":                 true,
	"GIT_EXEC_PATH":                    true,
	"GIT_PAGER":                        true,
}
