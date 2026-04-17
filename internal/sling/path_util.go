package sling

import "github.com/gastownhall/gascity/internal/pathutil"

// NormalizePathForCompare delegates to pathutil.NormalizePathForCompare.
func NormalizePathForCompare(path string) string {
	return pathutil.NormalizePathForCompare(path)
}

// SamePath delegates to pathutil.SamePath.
func SamePath(a, b string) bool {
	return pathutil.SamePath(a, b)
}
