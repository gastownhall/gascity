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
	out, err := exec.CommandContext(ctx, "git", gitCommonDirArgs(dir)...).Output()
	if err != nil {
		return ""
	}
	return importTrustRootFromGitCommonDir(dir, string(out))
}

// WorkspaceImportTrustRootInBox is WorkspaceImportTrustRoot for a session
// whose agent runs inside a box that conn reaches (a pod, a remote host): it
// runs the same git query in the box, against dir as the box names it, so the
// root is a box path that the import paths the agent lists there can match. A
// host-side root would name a directory the box does not have.
//
// It returns "" when dir is empty or when the query fails to run, exits
// non-zero (dir is not inside a git repository, or the box has no git), or
// prints nothing, so the external-imports modal is left for a human.
func WorkspaceImportTrustRootInBox(ctx context.Context, conn ExecProvider, name, dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	out, code, err := conn.Exec(ctx, name, append([]string{"git"}, gitCommonDirArgs(dir)...))
	if err != nil || code != 0 {
		return ""
	}
	return importTrustRootFromGitCommonDir(dir, string(out))
}

// gitCommonDirArgs is the git argv (without "git") that prints the common git
// directory of the repository containing dir.
func gitCommonDirArgs(dir string) []string {
	return []string{"-C", dir, "rev-parse", "--git-common-dir"}
}

// importTrustRootFromGitCommonDir turns the output of `git -C dir rev-parse
// --git-common-dir` into the repository root, or "" for empty output.
func importTrustRootFromGitCommonDir(dir, out string) string {
	common := strings.TrimSpace(out)
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
