package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const repoCacheLockName = ".packman-cache.lock"

// ErrRepoCacheBusy reports that another process holds a conflicting repo-cache
// lock. Only the Try* helpers return it; the blocking helpers wait instead.
var ErrRepoCacheBusy = errors.New("repo cache is busy")

// repoCacheLockOptions selects which repo-cache lock an acquisition takes and
// how it behaves under contention.
type repoCacheLockOptions struct {
	mode        int
	createRoot  bool
	nonBlocking bool
}

// WithRepoCacheReadLock runs fn while holding the shared repo-cache lock if
// the cache root exists. It does not create cache files or directories.
func WithRepoCacheReadLock(root string, fn func() error) error {
	return withRepoCacheLock(root, repoCacheLockOptions{mode: repoCacheLockShared}, fn)
}

// TryWithRepoCacheReadLock runs fn while holding the shared repo-cache lock,
// returning ErrRepoCacheBusy immediately instead of waiting when another
// process holds the lock exclusively.
//
// Use this only where a missed result is acceptable and a stall is not, such
// as command discovery or shell completion: a single cache write holds the
// exclusive lock for as long as its network clone takes, and every blocking
// reader on the machine queues behind it. Callers whose answer must be
// correct — installers, prune, bootstrap, skills materialization — keep using
// WithRepoCacheReadLock and wait.
func TryWithRepoCacheReadLock(root string, fn func() error) error {
	return withRepoCacheLock(root, repoCacheLockOptions{mode: repoCacheLockShared, nonBlocking: true}, fn)
}

// WithRepoCacheWriteLock runs fn while holding the exclusive repo-cache lock.
func WithRepoCacheWriteLock(root string, fn func() (string, error)) (string, error) {
	return writeLock(root, false, fn)
}

// TryWithRepoCacheWriteLock is WithRepoCacheWriteLock for advisory callers: it
// returns ErrRepoCacheBusy immediately rather than waiting for a writer that
// may be doing a network clone. Cache repair reached from an advisory load is
// better skipped than waited on; the next authoritative command repairs it.
func TryWithRepoCacheWriteLock(root string, fn func() (string, error)) (string, error) {
	return writeLock(root, true, fn)
}

// repoCacheWriteLock picks the exclusive-lock helper matching a load's
// blocking policy, so call sites read the same either way.
func repoCacheWriteLock(nonBlocking bool) func(string, func() (string, error)) (string, error) {
	if nonBlocking {
		return TryWithRepoCacheWriteLock
	}
	return WithRepoCacheWriteLock
}

func writeLock(root string, nonBlocking bool, fn func() (string, error)) (string, error) {
	var result string
	err := withRepoCacheLock(root, repoCacheLockOptions{
		mode:        repoCacheLockExclusive,
		createRoot:  true,
		nonBlocking: nonBlocking,
	}, func() error {
		var fnErr error
		result, fnErr = fn()
		return fnErr
	})
	return result, err
}

func withRepoCacheReadLockForPath(path string, nonBlocking bool, fn func() error) error {
	root, ok := repoCacheRootForPath(path)
	if !ok {
		return fn()
	}
	if nonBlocking {
		return TryWithRepoCacheReadLock(root, fn)
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
