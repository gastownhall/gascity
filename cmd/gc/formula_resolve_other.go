//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/formula"
)

type formulaResolutionState struct{}

func loadFormulaResolutionState(string) (*formulaResolutionState, error) {
	return &formulaResolutionState{}, nil
}

func (*formulaResolutionState) release() error {
	return nil
}

func ensureFormulaEntry(formulaDir, linkName, srcPath string, _ *formulaResolutionState) error {
	linkPath := filepath.Join(formulaDir, linkName)

	// Check if a real file (non-symlink) exists — don't overwrite.
	fi, err := os.Lstat(linkPath)
	if err == nil && fi.Mode()&os.ModeSymlink == 0 {
		return nil // Real file — leave it alone.
	}

	// If symlink exists, check if it's correct.
	if err == nil && fi.Mode()&os.ModeSymlink != 0 {
		existing, readErr := os.Readlink(linkPath)
		if readErr == nil && existing == srcPath {
			return nil // Already correct.
		}
		// Stale symlink — remove it.
		os.Remove(linkPath) //nolint:errcheck // will be recreated
	}

	if err := os.Symlink(srcPath, linkPath); err != nil {
		return fmt.Errorf("creating formula symlink %q → %q: %w", linkName, srcPath, err)
	}
	return nil
}

// cleanStaleFormulaEntries removes symlinks in formulaDir that are not in
// winners or whose targets no longer exist (broken symlinks from pack updates
// that removed formula files). Skips non-symlinks and non-formula files.
// No-op if formulaDir doesn't exist.
func cleanStaleFormulaEntries(formulaDir string, winners map[string]string, _ *formulaResolutionState) error {
	entries, err := os.ReadDir(formulaDir)
	if err != nil {
		return nil // Can't read — nothing to clean up.
	}
	for _, e := range entries {
		if e.IsDir() || !formula.IsTOMLFilename(e.Name()) {
			continue
		}
		linkPath := filepath.Join(formulaDir, e.Name())
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
