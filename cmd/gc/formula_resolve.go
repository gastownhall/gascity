package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/formula"
)

// ResolveFormulas computes per-filename winners from layered formula
// directories and creates symlinks in targetDir/.beads/formulas/.
//
// Layers are ordered lowest→highest priority. For each canonical or legacy
// TOML formula file found across all layers, the highest-priority layer wins. Winners are
// symlinked into targetDir/.beads/formulas/ so bd finds them natively.
//
// Idempotent: correct symlinks are left alone, stale ones are updated,
// and symlinks for formulas no longer in any layer are removed. Real files
// (non-symlinks) in the target directory are never overwritten.
func ResolveFormulas(targetDir string, layers []string) error {
	if len(layers) == 0 {
		return nil
	}

	// Build winner map: filename → absolute source path.
	// Later layers overwrite earlier ones (higher priority).
	winners := make(map[string]string)
	for _, layerDir := range layers {
		entries, err := os.ReadDir(layerDir)
		if err != nil {
			continue // Layer dir doesn't exist — skip (not an error).
		}
		for _, e := range entries {
			if e.IsDir() || !formula.IsTOMLFilename(e.Name()) {
				continue
			}
			abs, err := filepath.Abs(filepath.Join(layerDir, e.Name()))
			if err != nil {
				continue
			}
			winners[e.Name()] = abs
		}
	}

	symlinkDir := filepath.Join(targetDir, ".beads", "formulas")

	if len(winners) == 0 {
		return cleanStaleFormulaSymlinks(symlinkDir, winners)
	}

	// Ensure target symlink directory exists.
	if err := os.MkdirAll(symlinkDir, 0o755); err != nil {
		return fmt.Errorf("creating formula symlink dir: %w", err)
	}

	// Create/update symlinks for winners.
	for name, srcPath := range winners {
		linkPath := filepath.Join(symlinkDir, name)

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
			return fmt.Errorf("creating formula symlink %q → %q: %w", name, srcPath, err)
		}
	}

	return cleanStaleFormulaSymlinks(symlinkDir, winners)
}

// cleanStaleFormulaSymlinks removes symlinks in symlinkDir that are not in
// winners or whose targets no longer exist (broken symlinks from pack updates
// that removed formula files). Skips non-symlinks and non-formula files.
// No-op if symlinkDir doesn't exist.
func cleanStaleFormulaSymlinks(symlinkDir string, winners map[string]string) error {
	entries, err := os.ReadDir(symlinkDir)
	if err != nil {
		return nil // Can't read — nothing to clean up.
	}
	for _, e := range entries {
		if e.IsDir() || !formula.IsTOMLFilename(e.Name()) {
			continue
		}
		linkPath := filepath.Join(symlinkDir, e.Name())
		fi, err := os.Lstat(linkPath)
		if err != nil {
			continue
		}
		// Only consider symlinks (never real files).
		if fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		// Remove if not a winner.
		if _, isWinner := winners[e.Name()]; !isWinner {
			os.Remove(linkPath) //nolint:errcheck // best-effort cleanup
			continue
		}
		// Winner but target may have been deleted (pack removed the file
		// after initial fetch). os.Stat follows the symlink — if the
		// target is gone, remove the dangling link.
		if _, statErr := os.Stat(linkPath); statErr != nil {
			os.Remove(linkPath) //nolint:errcheck // best-effort cleanup
		}
	}

	return nil
}
