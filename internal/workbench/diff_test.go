package workbench

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/testutil"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir, _ := testutil.InitGitRepo(t)
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeDiff_MissingWorktree(t *testing.T) {
	if got := WorktreeDiff(context.Background(), "", 0); got.State != DiffMissingWorktree {
		t.Fatalf("empty dir state = %q, want %q", got.State, DiffMissingWorktree)
	}
	if got := WorktreeDiff(context.Background(), filepath.Join(t.TempDir(), "nope"), 0); got.State != DiffMissingWorktree {
		t.Fatalf("absent dir state = %q, want %q", got.State, DiffMissingWorktree)
	}
}

func TestWorktreeDiff_Empty(t *testing.T) {
	dir := gitRepo(t)
	if got := WorktreeDiff(context.Background(), dir, 0); got.State != DiffEmpty {
		t.Fatalf("clean repo state = %q, want %q", got.State, DiffEmpty)
	}
}

func TestWorktreeDiff_OK(t *testing.T) {
	dir := gitRepo(t)
	write(t, filepath.Join(dir, "a.txt"), "one\n")
	testutil.RunGit(t, dir, "add", "a.txt")
	testutil.RunGit(t, dir, "commit", "-m", "init")
	write(t, filepath.Join(dir, "a.txt"), "two\n")

	got := WorktreeDiff(context.Background(), dir, 0)
	if got.State != DiffOK {
		t.Fatalf("state = %q, want %q", got.State, DiffOK)
	}
	if !strings.Contains(got.Text, "-one") || !strings.Contains(got.Text, "+two") {
		t.Fatalf("diff missing change:\n%s", got.Text)
	}
}

func TestWorktreeDiff_Truncated(t *testing.T) {
	dir := gitRepo(t)
	write(t, filepath.Join(dir, "a.txt"), strings.Repeat("x\n", 100))
	testutil.RunGit(t, dir, "add", "a.txt")
	testutil.RunGit(t, dir, "commit", "-m", "init")
	write(t, filepath.Join(dir, "a.txt"), strings.Repeat("y\n", 100))

	got := WorktreeDiff(context.Background(), dir, 32)
	if got.State != DiffOK || !got.Truncated {
		t.Fatalf("state=%q truncated=%v, want ok/truncated", got.State, got.Truncated)
	}
	if len(got.Text) > 32 {
		t.Fatalf("text len %d exceeds bound 32", len(got.Text))
	}
}
