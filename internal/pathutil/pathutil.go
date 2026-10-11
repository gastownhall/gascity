// Package pathutil provides path normalization and comparison utilities.
package pathutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// NormalizePathForCompare resolves symlinks and makes a path absolute
// for reliable comparison.
func NormalizePathForCompare(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else if resolved, resolveErr := ResolveNearestExistingAncestor(path); resolveErr == nil {
		path = resolved
	}
	return canonicalizePlatformPathAlias(path)
}

// ResolveNearestExistingAncestor canonicalizes path by resolving symlinks on
// its nearest existing ancestor and rejoining the remaining, not-yet-existing
// tail segments onto that resolved ancestor. A plain filepath.EvalSymlinks
// fails outright on any path with a missing leaf, so this is what lets a
// not-yet-created path — a rig or git-clone destination, for example — be
// compared or contained correctly even when it's reached through a
// symlinked ancestor.
//
// path is expected to already be absolute; it is only filepath.Clean'd here,
// not resolved against the working directory.
//
// It returns an error only when resolution fails for a reason other than an
// ancestor simply not existing yet — e.g. a permission error or a symlink
// loop — since walking further up the tree cannot recover from those.
func ResolveNearestExistingAncestor(path string) (string, error) {
	cur := filepath.Clean(path)
	tail := ""
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, tail), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Clean(path), nil
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

func canonicalizePlatformPathAlias(path string) string {
	path = filepath.Clean(path)
	// On macOS, /tmp and /var commonly appear to callers without /private
	// while EvalSymlinks and lsof report the same location under /private.
	// Collapse those host aliases so path equality stays stable across APIs.
	if runtime.GOOS != "darwin" {
		return path
	}
	if path == "/private/tmp" {
		return "/tmp"
	}
	if strings.HasPrefix(path, "/private/tmp/") {
		return "/tmp/" + strings.TrimPrefix(path, "/private/tmp/")
	}
	if path == "/private/var" {
		return "/var"
	}
	if strings.HasPrefix(path, "/private/var/") {
		return "/var/" + strings.TrimPrefix(path, "/private/var/")
	}
	return path
}

// SamePath reports whether two paths refer to the same location after
// symlink resolution and normalization.
func SamePath(a, b string) bool {
	return NormalizePathForCompare(a) == NormalizePathForCompare(b)
}

// IsOutsideDir reports whether a relative path (as returned by
// filepath.Rel) escapes its base directory. Use after filepath.Rel to
// check containment without re-resolving the base.
func IsOutsideDir(rel string) bool {
	return rel == ".." || (len(rel) > 2 && rel[:3] == ".."+string(filepath.Separator))
}

// PathWithin reports whether candidate is the same path as root or a path
// lexically contained beneath root after normalization and symlink resolution.
func PathWithin(root, candidate string) bool {
	root = NormalizePathForCompare(root)
	candidate = NormalizePathForCompare(candidate)
	if root == "" || candidate == "" {
		return false
	}
	if root == candidate {
		return true
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
