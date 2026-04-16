package main

import (
	"path/filepath"
	"strings"
)

func normalizePathForCompare(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	path = canonicalizeExistingPathPrefix(path)
	return filepath.Clean(path)
}

func canonicalizeExistingPathPrefix(path string) string {
	current := path
	var suffix []string
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func samePath(a, b string) bool {
	return normalizePathForCompare(a) == normalizePathForCompare(b)
}

func pathWithinRoot(path, root string) bool {
	path = normalizePathForCompare(path)
	root = normalizePathForCompare(root)
	if path == "" || root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || rel == "" || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
