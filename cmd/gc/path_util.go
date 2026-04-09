package main

import "path/filepath"

func normalizePathForCompare(path string) string {
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

func samePath(a, b string) bool {
	return normalizePathForCompare(a) == normalizePathForCompare(b)
}
