// Package packman resolves, caches, and pins remote pack imports.
package packman

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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

// EnsureRepoInCache clones and checks out the requested commit when absent.
func EnsureRepoInCache(source, commit string) (string, error) {
	parsed := normalizeRemoteSource(source)
	cachePath, err := RepoCachePath(source, commit)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(cachePath, ".git")); err == nil {
		return cachePath, nil
	}

	root, err := RepoCacheRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("creating repo cache root: %w", err)
	}

	if _, err := runGit("", "clone", "--quiet", parsed.CloneURL, cachePath); err != nil {
		return "", fmt.Errorf("cloning %q: %w", source, err)
	}
	if _, err := runGit(cachePath, "checkout", "--quiet", commit); err != nil {
		return "", fmt.Errorf("checking out %q: %w", commit, err)
	}
	return cachePath, nil
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
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	for _, e := range os.Environ() {
		if k, _, ok := strings.Cut(e, "="); ok && fetchGitEnvBlacklist[k] {
			continue
		}
		cmd.Env = append(cmd.Env, e)
	}
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
}
