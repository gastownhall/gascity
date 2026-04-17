// Package pathutil provides path normalization and comparison utilities.
package pathutil

import "path/filepath"

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
	}
	return filepath.Clean(path)
}

// SamePath reports whether two paths refer to the same location after
// symlink resolution and normalization.
func SamePath(a, b string) bool {
	return NormalizePathForCompare(a) == NormalizePathForCompare(b)
}
