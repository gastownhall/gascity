package testutil

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// InitGitRepo creates a test-owned repository with one commit and returns its
// path and initial branch.
func InitGitRepo(t testing.TB) (string, string) {
	t.Helper()
	dir := t.TempDir()
	RunGit(t, dir, "init")
	RunGit(t, dir, "config", "user.email", "test@test.com")
	RunGit(t, dir, "config", "user.name", "Test")
	RunGit(t, dir, "commit", "--allow-empty", "-m", "init")
	return dir, RunGit(t, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// RunGit runs git with repository-locating environment variables removed so
// ambient Git state cannot redirect a test command.
func RunGit(t testing.TB, dir string, args ...string) string {
	t.Helper()
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR":
			continue
		}
		env = append(env, entry)
	}
	out, err := RunGitWithEnv(dir, env, args...)
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), out, err)
	}
	return strings.TrimSpace(string(out))
}

// RunGitWithEnv runs git in dir with exactly env and returns its combined
// output and process error. The caller owns environment isolation and error
// assertions, including commands whose failure is part of the contract.
func RunGitWithEnv(dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.CombinedOutput()
}
