// Package searchpath builds deterministic PATH search orders that include
// common user-managed install directories (nvm, fnm, asdf, cargo, etc.).
package searchpath

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Expand returns a deterministic PATH search order that preserves the caller's
// base PATH while adding common user-managed install locations.
func Expand(homeDir, goos, basePath string) []string {
	var dirs []string
	if homeDir = strings.TrimSpace(homeDir); homeDir != "" {
		dirs = append(dirs,
			filepath.Join(homeDir, ".local", "bin"),
			filepath.Join(homeDir, "bin"),
		)
	}
	dirs = append(dirs, splitPath(basePath)...)
	dirs = append(dirs, userManagedDirs(homeDir)...)
	switch goos {
	case "darwin":
		dirs = append(dirs,
			"/opt/homebrew/bin",
			"/opt/homebrew/sbin",
			"/opt/local/bin",
			"/opt/local/sbin",
		)
	case "linux":
		dirs = append(dirs,
			"/snap/bin",
			"/home/linuxbrew/.linuxbrew/bin",
			"/home/linuxbrew/.linuxbrew/sbin",
		)
	}
	return Dedupe(dirs)
}

// ExpandPath joins [Expand] using the platform PATH list separator.
func ExpandPath(homeDir, goos, basePath string) string {
	return strings.Join(Expand(homeDir, goos, basePath), string(os.PathListSeparator))
}

// Dedupe removes empty entries while preserving the first occurrence.
func Dedupe(dirs []string) []string {
	seen := make(map[string]struct{}, len(dirs))
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		out = append(out, dir)
	}
	return out
}

func splitPath(basePath string) []string {
	basePath = strings.TrimSpace(basePath)
	if basePath == "" {
		return nil
	}
	return strings.Split(basePath, string(os.PathListSeparator))
}

func userManagedDirs(homeDir string) []string {
	if homeDir == "" {
		return nil
	}
	dirs := existingDirs(
		filepath.Join(homeDir, "go", "bin"),
		filepath.Join(homeDir, ".cargo", "bin"),
		filepath.Join(homeDir, ".bun", "bin"),
		filepath.Join(homeDir, ".deno", "bin"),
		filepath.Join(homeDir, ".volta", "bin"),
		filepath.Join(homeDir, ".nvm", "current", "bin"),
		filepath.Join(homeDir, ".asdf", "shims"),
		filepath.Join(homeDir, ".nodenv", "shims"),
		filepath.Join(homeDir, ".local", "share", "mise", "shims"),
		filepath.Join(homeDir, ".local", "share", "rtx", "shims"),
		filepath.Join(homeDir, ".nodebrew", "current", "bin"),
	)
	dirs = append(dirs, globExistingDirs(
		filepath.Join(homeDir, ".nvm", "versions", "node", "*", "bin"),
		filepath.Join(homeDir, ".fnm", "node-versions", "*", "installation", "bin"),
		filepath.Join(homeDir, ".local", "share", "fnm", "node-versions", "*", "installation", "bin"),
		filepath.Join(homeDir, ".nodebrew", "node", "*", "bin"),
	)...)
	return dirs
}

func existingDirs(paths ...string) []string {
	out := make([]string, 0, len(paths))
	for _, dir := range paths {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		out = append(out, dir)
	}
	return out
}

// globExistingDirs expands each glob pattern, filters to directories that
// exist, and returns them in reverse-lexicographic order so that newer
// versions (e.g. v22.x) sort before older ones (e.g. v18.x). These entries
// are fallbacks — stable "current" or shim paths checked earlier in
// userManagedDirs take priority when they exist.
func globExistingDirs(patterns ...string) []string {
	var out []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		sort.Sort(sort.Reverse(sort.StringSlice(matches)))
		out = append(out, existingDirs(matches...)...)
	}
	return out
}
