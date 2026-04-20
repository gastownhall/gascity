package config

import (
	"os"
	"path/filepath"
	"strings"
)

const repoCacheLockName = ".packman-cache.lock"

// WithRepoCacheReadLock runs fn while holding the shared repo-cache lock if
// the cache root exists. It does not create cache files or directories.
func WithRepoCacheReadLock(root string, fn func() error) error {
	return withRepoCacheLock(root, repoCacheLockShared, false, fn)
}

// WithRepoCacheWriteLock runs fn while holding the exclusive repo-cache lock.
func WithRepoCacheWriteLock(root string, fn func() (string, error)) (string, error) {
	var result string
	err := withRepoCacheLock(root, repoCacheLockExclusive, true, func() error {
		var fnErr error
		result, fnErr = fn()
		return fnErr
	})
	return result, err
}

func withRepoCacheReadLockForPath(path string, fn func() error) error {
	root, ok := repoCacheRootForPath(path)
	if !ok {
		return fn()
	}
	return WithRepoCacheReadLock(root, fn)
}

func repoCacheRootForPath(path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	for _, root := range repoCacheRootCandidates() {
		if pathWithinDir(abs, root) {
			return root, true
		}
	}
	return "", false
}

func repoCacheRootCandidates() []string {
	var roots []string
	add := func(root string) {
		if root == "" {
			return
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			abs = root
		}
		for _, existing := range roots {
			if existing == abs {
				return
			}
		}
		roots = append(roots, abs)
	}
	if gcHome := ImplicitGCHome(); gcHome != "" {
		add(filepath.Join(gcHome, "cache", "repos"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(filepath.Join(home, ".gc", "cache", "repos"))
	}
	return roots
}

func pathWithinDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (!filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}
