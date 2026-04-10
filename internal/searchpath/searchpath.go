// Package searchpath builds deterministic PATH search orders that include
// common user-managed install directories (nvm, fnm, asdf, cargo, etc.).
package searchpath

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// compareNatural compares two strings using a natural ordering that treats
// runs of decimal digits as numbers. This lets version-like strings such as
// "v8.17.0" and "v22.14.0" sort numerically (v8 < v22) instead of
// lexicographically (v22 < v8 because '2' < '8'). Returns -1 if a < b,
// 0 if a == b, and 1 if a > b.
func compareNatural(a, b string) int {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		ai, bi := a[i], b[j]
		aDigit := ai >= '0' && ai <= '9'
		bDigit := bi >= '0' && bi <= '9'
		if aDigit && bDigit {
			// Skip leading zeros so "007" and "7" compare equal on value.
			aStart := i
			for aStart < len(a) && a[aStart] == '0' {
				aStart++
			}
			bStart := j
			for bStart < len(b) && b[bStart] == '0' {
				bStart++
			}
			// Advance i/j to the end of each digit run.
			aEnd := i
			for aEnd < len(a) && a[aEnd] >= '0' && a[aEnd] <= '9' {
				aEnd++
			}
			bEnd := j
			for bEnd < len(b) && b[bEnd] >= '0' && b[bEnd] <= '9' {
				bEnd++
			}
			aNum := a[aStart:aEnd]
			bNum := b[bStart:bEnd]
			switch {
			case len(aNum) < len(bNum):
				return -1
			case len(aNum) > len(bNum):
				return 1
			case aNum != bNum:
				if aNum < bNum {
					return -1
				}
				return 1
			}
			// Numbers equal; tie-break on leading-zero count so "07" > "7".
			aZeros := aStart - i
			bZeros := bStart - j
			if aZeros != bZeros {
				if aZeros < bZeros {
					return -1
				}
				return 1
			}
			i, j = aEnd, bEnd
			continue
		}
		if ai != bi {
			if ai < bi {
				return -1
			}
			return 1
		}
		i++
		j++
	}
	switch {
	case i < len(a):
		return 1
	case j < len(b):
		return -1
	default:
		return 0
	}
}

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
// exist, and returns them in descending natural (version-aware) order so
// that newer versions (e.g. v22.x) sort before older ones (e.g. v8.x).
// Plain lexicographic sort is wrong here — "v8" > "v22" lex because '8' > '2'
// — so we treat digit runs numerically. These entries are fallbacks —
// stable "current" or shim paths checked earlier in userManagedDirs take
// priority when they exist.
func globExistingDirs(patterns ...string) []string {
	var out []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		sort.SliceStable(matches, func(i, j int) bool {
			return compareNatural(matches[i], matches[j]) > 0
		})
		out = append(out, existingDirs(matches...)...)
	}
	return out
}
