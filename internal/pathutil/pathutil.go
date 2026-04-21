// Package pathutil provides path normalization and comparison utilities.
package pathutil

import (
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
	} else if resolved, ok := normalizeMissingPath(path); ok {
		path = resolved
	}
	return canonicalizePlatformPathAlias(path)
}

func normalizeMissingPath(path string) (string, bool) {
	var missing []string
	for current := path; ; current = filepath.Dir(current) {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", false
		}
		missing = append(missing, filepath.Base(current))
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
