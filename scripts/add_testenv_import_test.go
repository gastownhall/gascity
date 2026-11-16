package scripts_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// Pure Go: never the host's C compiler, which the worker-env measurement
	// (rbe-worker's worker/worker-env) does not cover.
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
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

// TestAddTestenvImportSelectsOnlyLintRequiredDirs pins the generator's
// directory selection to internal/testenv's skipRepoLintDir, the lint whose
// failure message tells the reader to run this generator. A directory the lint
// prunes must never receive a generated file: a nested Go module (tools/nogo)
// cannot import internal/testenv, so a file written there breaks that module's
// tests. See ga-yuu6ag.
func TestAddTestenvImportSelectsOnlyLintRequiredDirs(t *testing.T) {
	root := repoRoot(t)
	scriptPath := filepath.Join(root, "scripts", "add-testenv-import.go")

	const testSrc = "package %s\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n"
	fixture := t.TempDir()
	writeTestFile(t, filepath.Join(fixture, "go.mod"), "module fixture\n\ngo 1.26.6\n")

	// Directories the lint requires a canonical import file in.
	required := []string{"real", filepath.Join("internal", "deep")}
	// Directories skipRepoLintDir prunes, one per rule, including a package
	// nested below a pruned directory.
	pruned := []string{
		filepath.Join("tools", "nogo"),                        // nested Go module root
		filepath.Join("tools", "nogo", "analyzers", "unused"), // below a nested module
		".hidden",      // "." prefix
		"_scratch",     // "_" prefix
		"worktrees",    // name == worktrees
		"worktree-abc", // "worktree-" prefix
		"pkg",          // public tree
		"vendor",
		"node_modules",
	}
	writeTestFile(t, filepath.Join(fixture, "tools", "nogo", "go.mod"), "module nogo\n\ngo 1.26.6\n")
	for _, dir := range append(append([]string{}, required...), pruned...) {
		writeTestFile(t, filepath.Join(fixture, dir, "x_test.go"), fmt.Sprintf(testSrc, "x"))
	}

	run := func() string {
		cmd := exec.Command("go", "run", scriptPath)
		cmd.Dir = fixture
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		var out, errBuf bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			t.Fatalf("run add-testenv-import: %v\nstdout: %s\nstderr: %s", err, out.String(), errBuf.String())
		}
		return out.String()
	}

	run()
	for _, dir := range required {
		if _, err := os.Stat(filepath.Join(fixture, dir, "testenv_import_test.go")); err != nil {
			t.Errorf("lint requires an import file in %s, generator did not write one: %v", dir, err)
		}
	}
	for _, dir := range pruned {
		if _, err := os.Stat(filepath.Join(fixture, dir, "testenv_import_test.go")); err == nil {
			t.Errorf("generator wrote into %s, which the lint's skipRepoLintDir prunes", dir)
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", dir, err)
		}
	}

	if second := run(); !strings.Contains(second, "0 file(s) written") {
		t.Errorf("second run is not idempotent, want 0 file(s) written, got:\n%s", second)
	}
}
