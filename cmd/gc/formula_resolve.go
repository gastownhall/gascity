package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/formula"
)

// ResolveFormulas computes per-formula-name winners from layered formula
// directories and creates formula entries in targetDir/.beads/formulas/.
//
// Layers are ordered lowest→highest priority. For each formula name (derived
// from either canonical or legacy filename form), the highest-priority layer
// wins. Winners are published into targetDir/.beads/formulas using both the
// canonical filename (<name>.toml) and a legacy compatibility alias
// (<name>.formula.toml). Unix publishes symlinks. Windows first tries the same
// symlink path and, only when the token lacks symlink privilege, falls back to
// a managed copy. Gas City's internal parser prefers the canonical filename;
// the legacy alias keeps older external bd binaries working while they still
// probe only the infixed TOML form.
//
// Idempotent: correct entries are left alone, stale ones are updated, and
// entries for formulas no longer in any layer are removed. Real files that gc
// did not create are never overwritten.
//
// Per-name precedence (last-wins across layers, canonical-beats-legacy
// within a layer) is delegated to formula.ResolveAll — the same source of
// truth used by parser.loadFormula so the symlink view and the in-process
// loader cannot disagree on which file wins.
func ResolveFormulas(targetDir string, layers []string) (returnErr error) {
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
		if _, err := os.Stat(symlinkDir); os.IsNotExist(err) {
			return nil
		}
	}
	state, err := loadFormulaResolutionState(symlinkDir)
	if err != nil {
		return err
	}
	defer func() {
		if err := state.release(); err != nil {
			returnErr = errors.Join(returnErr, err)
		}
	}()

	if len(winners) == 0 {
		return cleanStaleFormulaEntries(symlinkDir, linkTargets, state)
	}

	// Ensure target symlink directory exists.
	if err := os.MkdirAll(symlinkDir, 0o755); err != nil {
		return fmt.Errorf("creating formula symlink dir: %w", err)
	}

	// Create/update entries for winners. Both names always point to the
	// same winning source regardless of whether the source file on disk uses
	// the canonical or legacy extension.
	for linkName, srcPath := range linkTargets {
		if err := ensureFormulaEntry(symlinkDir, linkName, srcPath, state); err != nil {
			return err
		}
	}

	return cleanStaleFormulaEntries(symlinkDir, linkTargets, state)
}
