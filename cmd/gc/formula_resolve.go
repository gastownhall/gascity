package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/formula"
)

// ResolveFormulas computes per-formula-name winners from layered formula
// directories and creates formula symlinks in targetDir/.beads/formulas/.
//
// Layers are ordered lowest→highest priority. For each formula name (derived
// from either canonical or legacy filename form), the highest-priority layer
// wins. Winners are symlinked into targetDir/.beads/formulas using both the
// canonical filename (<name>.toml) and a legacy compatibility alias
// (<name>.formula.toml). Gas City's internal parser prefers the canonical
// filename; the legacy alias keeps older external bd binaries working while
// they still probe only the infixed TOML form.
//
// Idempotent: correct symlinks are left alone, stale ones are updated,
// and symlinks for formulas no longer in any layer are removed. Real files
// (non-symlinks) in the target directory are never overwritten.
//
// Per-name precedence (last-wins across layers, canonical-beats-legacy
// within a layer) is delegated to formula.ResolveAll — the same source of
// truth used by parser.loadFormula so the symlink view and the in-process
// loader cannot disagree on which file wins.
func ResolveFormulas(targetDir string, layers []string) error {
	if len(layers) == 0 {
		return nil
	}

	winners := formula.ResolveAll(layers)

	// Build the set of formula link names we will emit. Each winning formula
	// gets a canonical link plus a legacy compatibility alias.
	linkTargets := make(map[string]string, len(winners)*2)
	for name, src := range winners {
		linkTargets[name+formula.CanonicalTOMLExt] = src
		linkTargets[name+formula.LegacyTOMLExt] = src
	}

	symlinkDir := filepath.Join(targetDir, ".beads", "formulas")

	if len(winners) == 0 {
		return cleanStaleFormulaSymlinks(symlinkDir, linkTargets)
	}

	// Ensure target symlink directory exists.
	if err := os.MkdirAll(symlinkDir, 0o755); err != nil {
		return fmt.Errorf("creating formula symlink dir: %w", err)
	}

	// Create/update symlinks for winners. Both link names always point to the
	// same winning source regardless of whether the source file on disk uses
	// the canonical or legacy extension.
	for linkName, srcPath := range linkTargets {
		linkPath := filepath.Join(symlinkDir, linkName)

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
			return fmt.Errorf("creating formula symlink %q → %q: %w", linkName, srcPath, err)
		}
	}

	return cleanStaleFormulaSymlinks(symlinkDir, linkTargets)
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
