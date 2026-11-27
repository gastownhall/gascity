package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// TestWorkspaceImportTrustRootInBox: the root is resolved by the same git
// query run inside the box, so it is a box path, and any query that does not
// answer with a directory trusts nothing.
func TestWorkspaceImportTrustRootInBox(t *testing.T) {
	t.Parallel()

	const dir = "/workspace/rig/ga-1-fix"
	wantArgv := []string{"git", "-C", dir, "rev-parse", "--git-common-dir"}
	cases := []struct {
		name string
		out  string
		code int
		err  error
		want string
	}{
		{name: "linked worktree", out: "/workspace/rig/.git\n", want: "/workspace/rig"},
		{name: "main tree", out: ".git\n", want: dir},
		{name: "subdirectory of the main tree", out: "../.git\n", want: "/workspace/rig"},
		{name: "not a repository", code: 128},
		{name: "transport failure", err: errBoom},
		{name: "no output", out: "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn := &recordingExec{out: []byte(tc.out), code: tc.code, err: tc.err}
			if got := WorkspaceImportTrustRootInBox(context.Background(), conn, "s", dir); got != tc.want {
				t.Errorf("WorkspaceImportTrustRootInBox = %q, want %q", got, tc.want)
			}
			if len(conn.calls) != 1 || !slices.Equal(conn.calls[0], wantArgv) {
				t.Errorf("exec calls = %q, want one %q", conn.calls, wantArgv)
			}
		})
	}

	t.Run("empty dir", func(t *testing.T) {
		t.Parallel()
		conn := &recordingExec{out: []byte(".git\n")}
		if got := WorkspaceImportTrustRootInBox(context.Background(), conn, "s", " "); got != "" {
			t.Errorf("WorkspaceImportTrustRootInBox(empty dir) = %q, want empty", got)
		}
		if len(conn.calls) != 0 {
			t.Errorf("exec calls = %q, want none for an empty dir", conn.calls)
		}
	})
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
