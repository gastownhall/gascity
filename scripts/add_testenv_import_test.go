package scripts_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestAddTestenvImportSkipsNestedGitWorktrees guards against a regression
// where the generator's unbounded directory walk descended into linked git
// worktrees nested under the repo root (e.g. a pool worktree's own
// worktrees/<bead-id> subdirectory) and wrote generated
// testenv_import_test.go files into them, corrupting whatever branch a
// sibling session had checked out there. See ga-t00ejy.
func TestAddTestenvImportSkipsNestedGitWorktrees(t *testing.T) {
	root := repoRoot(t)
	scriptPath := filepath.Join(root, "scripts", "add-testenv-import.go")

	fixture := t.TempDir()
	writeTestFile(t, filepath.Join(fixture, "go.mod"), "module fixture\n\ngo 1.26.6\n")
	writeTestFile(t, filepath.Join(fixture, "pkg1", "pkg1_test.go"),
		"package pkg1\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {}\n")

	// A linked worktree nested under the fixture root, exactly like
	// builder-1/worktrees/<bead-id> in the real repo: a .git FILE (not a
	// directory) is what marks a directory as a worktree root, regardless of
	// its name.
	nestedWorktree := filepath.Join(fixture, "worktrees", "ga-5vzfgb-fixture")
	writeTestFile(t, filepath.Join(nestedWorktree, ".git"),
		"gitdir: /elsewhere/.git/worktrees/ga-5vzfgb-fixture\n")
	writeTestFile(t, filepath.Join(nestedWorktree, "pkg2", "pkg2_test.go"),
		"package pkg2\n\nimport \"testing\"\n\nfunc TestBar(t *testing.T) {}\n")

	cmd := exec.Command("go", "run", scriptPath)
	cmd.Dir = fixture
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("run add-testenv-import: %v\nstdout: %s\nstderr: %s", err, out.String(), errBuf.String())
	}

	if _, err := os.Stat(filepath.Join(fixture, "pkg1", "testenv_import_test.go")); err != nil {
		t.Errorf("expected canonical import file written for pkg1 outside any worktree: %v", err)
	}

	if _, err := os.Stat(filepath.Join(nestedWorktree, "pkg2", "testenv_import_test.go")); err == nil {
		t.Errorf("generator wrote into nested git worktree %s — it must skip linked worktrees entirely", nestedWorktree)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat nested worktree output: %v", err)
	}
}
