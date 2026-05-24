package molecule

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// Molecule-scoped working storage lives at:
//
//	<cityPath>/.gc/molecules/<rootID>/          — molecule root (Dir)
//	<cityPath>/.gc/molecules/<rootID>/artifacts/<beadID>/ — per-step (ArtifactDirFor)
//
// These paths outlive any single worker (polecat, pool agent). Anchoring
// artifact lifetime to the work (molecule root ID) rather than the worker
// (session/worktree) lets ralph-loop and multi-round fanout steps survive
// worker teardown and resume from prior-round state on re-sling.

// Dir returns the molecule root directory.
func Dir(cityPath, rootID string) string {
	return filepath.Join(cityPath, ".gc", "molecules", rootID)
}

// ArtifactDirFor returns the per-step-bead artifact directory.
func ArtifactDirFor(cityPath, rootID, beadID string) string {
	return filepath.Join(Dir(cityPath, rootID), "artifacts", beadID)
}

// EnsureArtifactDir creates the per-step artifact directory if it does
// not exist and returns its path (joined against cityPath as-given; not
// canonicalized). Rejects empty or path-traversing rootID/beadID values
// so a malformed bead ID cannot write outside the molecule root.
func EnsureArtifactDir(fs fsys.FS, cityPath, rootID, beadID string) (string, error) {
	if err := validateIDSegment("root bead ID", rootID); err != nil {
		return "", err
	}
	if err := validateIDSegment("bead ID", beadID); err != nil {
		return "", err
	}
	dir := ArtifactDirFor(cityPath, rootID, beadID)
	if err := fs.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating molecule artifact directory: %w", err)
	}
	return dir, nil
}

// RemoveDir deletes the molecule-scoped working storage rooted at
// Dir(cityPath, rootID). Intended for molecule finalize cleanup.
//
// Safety guarantees:
//   - Empty cityPath is rejected — otherwise paths would resolve against
//     the caller's cwd and a mis-configured controller could purge
//     molecule dirs under an unrelated tree.
//   - Empty rootID is rejected (would otherwise resolve to
//     cityPath/.gc/molecules/ and wipe every molecule).
//   - Path-traversing rootID values (containing "/", "..", or absolute
//     paths) are rejected.
//   - A missing directory is treated as success (idempotent; important
//     because processWorkflowFinalize retries on controller crash).
func RemoveDir(cityPath, rootID string) error {
	if strings.TrimSpace(cityPath) == "" {
		return fmt.Errorf("cityPath is empty")
	}
	if err := validateIDSegment("root bead ID", rootID); err != nil {
		return err
	}
	dir := Dir(cityPath, rootID)

	// Containment check: the resolved directory must live under
	// cityPath/.gc/molecules/. Use relative-path analysis so symlinks in
	// cityPath itself do not trip us.
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolving molecule directory: %w", err)
	}
	absRoot, err := filepath.Abs(filepath.Join(cityPath, ".gc", "molecules"))
	if err != nil {
		return fmt.Errorf("resolving molecules root: %w", err)
	}
	rel, err := filepath.Rel(absRoot, absDir)
	if err != nil || pathutil.IsOutsideDir(rel) || rel == "." || rel == "" {
		return fmt.Errorf("molecule directory %q escapes molecules root %q", absDir, absRoot)
	}

	if err := os.RemoveAll(absDir); err != nil {
		return fmt.Errorf("removing molecule directory %q: %w", absDir, err)
	}
	return nil
}

// validateIDSegment rejects empty or path-unsafe ID segments. A valid ID
// must be non-empty, not contain path separators, not be an absolute
// path, and not be the literal ".." (parent directory reference).
//
// Note: "root..1" and similar IDs with ".." embedded in them are allowed
// once separators are already banned — without a separator, ".." is just
// two literal dots and cannot escape the directory.
func validateIDSegment(label, id string) error {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return fmt.Errorf("%s is empty", label)
	}
	if filepath.IsAbs(trimmed) {
		return fmt.Errorf("%s %q must not be an absolute path", label, trimmed)
	}
	if strings.ContainsAny(trimmed, `/\`) {
		return fmt.Errorf("%s %q must not contain path separators", label, trimmed)
	}
	if trimmed == ".." {
		return fmt.Errorf("%s %q must not be a parent-directory reference", label, trimmed)
	}
	return nil
}
