package runtime

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
)

// WorkspaceImportTrustRoot returns the root of the git repository that contains
// dir, to be used as the trusted boundary for external CLAUDE.md imports with
// WithTrustedImportRoot. It resolves the common git directory (so a linked
// worktree under `<repo>/.gc/worktrees/<id>` maps back to `<repo>`, the main
// working tree that holds the repository's own AGENTS.md), then returns that
// tree's root.
//
// It returns "" when dir is empty or is not inside a git repository. Callers
// pass the result straight to WithTrustedImportRoot, so an empty result simply
// leaves the external-imports modal for a human instead of auto-accepting.
func WorkspaceImportTrustRoot(ctx context.Context, dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(string(out))
	if common == "" {
		return ""
	}
	// --git-common-dir is absolute for linked worktrees and may be relative
	// (e.g. ".git") for the main tree; resolve it against dir before taking the
	// parent so the repository root is absolute.
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	return filepath.Dir(filepath.Clean(common))
}
