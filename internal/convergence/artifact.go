package convergence

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// EnsureArtifactDir creates the artifact directory for a given iteration
// if it doesn't exist. Returns the absolute path to the created directory.
func EnsureArtifactDir(fs fsys.FS, cityPath, beadID string, iteration int) (string, error) {
	dir := ArtifactDirFor(cityPath, beadID, iteration)
	if err := fs.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating artifact directory: %w", err)
	}
	return dir, nil
}

// ValidateArtifactDir checks that an artifact directory is safe for gate
// execution:
//   - No symlinks pointing outside the artifact root
//   - No non-regular files (FIFOs, device files, sockets)
//
// Returns nil if safe, error describing the violation otherwise.
func ValidateArtifactDir(dir string) error {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolving artifact directory: %w", err)
	}
	// Canonicalize with EvalSymlinks so comparisons are consistent
	// when the artifact root itself contains symlinked components.
	// canonical-path-exception: existence/resolvability only, not comparison
	// preparation. absDir is already absolute (filepath.Abs above), so this
	// call cannot diverge into a relative/absolute mismatch; its error path
	// is the deliberate "artifact directory must exist" check for this
	// function, which pathutil.NormalizePathForCompare's never-errors
	// contract would silently swallow.
	absDir, err = filepath.EvalSymlinks(absDir)
	if err != nil {
		return fmt.Errorf("resolving artifact directory: %w", err)
	}

	return filepath.WalkDir(absDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		typ := d.Type()

		// Check for symlinks using EvalSymlinks for full resolution
		// (handles multi-hop chains), consistent with ResolveConditionPath.
		// canonical-path-exception: existence/resolvability only, not
		// comparison preparation. path comes from WalkDir(absDir, ...) and
		// is already absolute, so this cannot diverge into a
		// relative/absolute mismatch; a broken or unresolvable symlink
		// target must fail directory validation here, which
		// pathutil.NormalizePathForCompare's never-errors contract would
		// silently paper over.
		if typ&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("resolving symlink %q: %w", path, err)
			}
			rel, err := filepath.Rel(absDir, resolved)
			if err != nil || pathutil.IsOutsideDir(rel) {
				return fmt.Errorf("symlink %q points outside artifact directory: resolves to %q", path, resolved)
			}
			return nil
		}

		// Allow regular files and directories.
		if typ.IsRegular() || typ.IsDir() {
			return nil
		}

		// Reject everything else (FIFOs, device files, sockets).
		return fmt.Errorf("unsafe file type in artifact directory: %q (mode %s)", path, typ)
	})
}
