package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWorkspaceImportTrustRoot(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit",
		"--allow-empty", "-q", "-m", "init")

	// A linked worktree under the repo must resolve back to the main repo root,
	// so an import of the repo-root AGENTS.md (seen as external from the
	// worktree subdirectory) is recognized as first-party.
	wtParent := filepath.Join(repo, ".gc", "worktrees")
	if err := os.MkdirAll(wtParent, 0o755); err != nil {
		t.Fatalf("mkdir worktrees: %v", err)
	}
	wt := filepath.Join(wtParent, "wt")
	runGit(t, repo, "worktree", "add", "-q", "--detach", wt)

	wantRoot := evalSymlinks(t, repo)

	for _, dir := range []string{repo, wt} {
		if got := evalSymlinks(t, WorkspaceImportTrustRoot(context.Background(), dir)); got != wantRoot {
			t.Errorf("WorkspaceImportTrustRoot(%q) = %q, want repo root %q", dir, got, wantRoot)
		}
	}

	if got := WorkspaceImportTrustRoot(context.Background(), t.TempDir()); got != "" {
		t.Errorf("WorkspaceImportTrustRoot(non-git dir) = %q, want empty", got)
	}
	if got := WorkspaceImportTrustRoot(context.Background(), ""); got != "" {
		t.Errorf("WorkspaceImportTrustRoot(empty) = %q, want empty", got)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func evalSymlinks(t *testing.T, path string) string {
	t.Helper()
	if path == "" {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", path, err)
	}
	return resolved
}
