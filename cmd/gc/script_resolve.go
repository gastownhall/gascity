package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// ResolveScripts computes per-relative-path winners from layered script
// directories and creates symlinks in targetDir/scripts/.
//
// Layers are ordered lowest→highest priority. For each file found across
// all layers, the highest-priority layer wins. Winners are symlinked into
// targetDir/scripts/ preserving subdirectory structure.
//
// Idempotent: correct symlinks are left alone, stale ones are updated,
// and symlinks for scripts no longer in any layer are removed. Real files
// (non-symlinks) in the target directory are never overwritten.
func ResolveScripts(targetDir string, layers []string) error {
	if len(layers) == 0 {
		return nil
	}

	// Build winner map: relative path → absolute source path.
	// Later layers overwrite earlier ones (higher priority).
	winners := make(map[string]string)
	for _, layerDir := range layers {
		err := filepath.WalkDir(layerDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable entries
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(layerDir, path)
			if err != nil {
				return nil
			}
			abs, err := filepath.Abs(path)
			if err != nil {
				return nil
			}
			winners[rel] = abs
			return nil
		})
		if err != nil {
			continue // Layer dir doesn't exist — skip.
		}
	}

	symlinkDir := filepath.Join(targetDir, "scripts")

	if len(winners) == 0 {
		return cleanStaleScriptSymlinks(symlinkDir, winners)
	}

	// Create/update symlinks for winners.
	for rel, srcPath := range winners {
		linkPath := filepath.Join(symlinkDir, rel)

		// Ensure parent directory exists.
		if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
			return fmt.Errorf("creating script symlink parent dir: %w", err)
		}

		// Check if a real file (non-symlink) exists — don't overwrite.
		fi, err := os.Lstat(linkPath)
		if err == nil && fi.Mode()&os.ModeSymlink == 0 {
			continue // Real file — leave it alone.
		}

		// If symlink exists, check if it's correct.
		if err == nil && fi.Mode()&os.ModeSymlink != 0 {
			existing, readErr := os.Readlink(linkPath)
			if readErr == nil && existing == srcPath {
				continue // Already correct.
			}
			// Stale symlink — remove it.
			os.Remove(linkPath) //nolint:errcheck // will be recreated
		}

		if err := os.Symlink(srcPath, linkPath); err != nil {
			return fmt.Errorf("creating script symlink %q → %q: %w", rel, srcPath, err)
		}
	}

	return cleanStaleScriptSymlinks(symlinkDir, winners)
}

// cleanStaleScriptSymlinks removes symlinks in symlinkDir that are not in
// winners. Walks recursively and removes empty subdirectories afterward.
// Skips non-symlinks. No-op if symlinkDir doesn't exist.
func cleanStaleScriptSymlinks(symlinkDir string, winners map[string]string) error {
	if _, err := os.Stat(symlinkDir); os.IsNotExist(err) {
		return nil
	}

	// Collect stale symlinks.
	var stale []string
	err := filepath.WalkDir(symlinkDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		fi, lErr := os.Lstat(path)
		if lErr != nil {
			return nil
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			return nil // Real file — skip.
		}
		rel, rErr := filepath.Rel(symlinkDir, path)
		if rErr != nil {
			return nil
		}
		if _, isWinner := winners[rel]; !isWinner {
			stale = append(stale, path)
		}
		return nil
	})
	if err != nil {
		return nil
	}

	for _, path := range stale {
		os.Remove(path) //nolint:errcheck // best-effort cleanup
	}

	// Remove empty directories (bottom-up).
	removeEmptyDirs(symlinkDir)

	return nil
}

// removeEmptyDirs walks symlinkDir bottom-up and removes empty directories.
// The root symlinkDir itself is not removed.
func removeEmptyDirs(root string) {
	// Collect all directories.
	var dirs []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error { //nolint:errcheck // best-effort
		if err != nil {
			return nil
		}
		if d.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	})

	// Process deepest first.
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i]) //nolint:errcheck // fails silently if non-empty
	}
}
