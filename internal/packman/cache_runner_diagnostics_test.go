package packman

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestDefaultRunGitFailureKeepsDiagnostics pins the failure half of the
// shared repo-cache runner contract: a git command that exits non-zero or
// cannot start yields an empty result and an error that names `git <args>`,
// carries git's own stderr, and wraps the process error, so callers can
// classify *exec.ExitError versus exec.ErrNotFound.
func TestDefaultRunGitFailureKeepsDiagnostics(t *testing.T) {
	t.Run("nonzero exit", func(t *testing.T) {
		useNeutralGitEnv(t)
		cacheDir := t.TempDir()
		seedPackmanCacheRepo(t, cacheDir)

		out, err := defaultRunGit(cacheDir, "rev-parse", "missing-revision")
		if err == nil {
			t.Fatal("rev-parse of a missing revision succeeded")
		}
		if out != "" {
			t.Errorf("output = %q, want empty on failure", out)
		}
		for _, want := range []string{"git rev-parse missing-revision", "fatal:"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err, want)
			}
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Errorf("error = %v, want it to wrap *exec.ExitError", err)
		}
	})

	t.Run("process start failure", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		out, err := defaultRunGit(t.TempDir(), "rev-parse", "HEAD")
		if out != "" {
			t.Errorf("output = %q, want empty on failure", out)
		}
		if !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("error = %v, want it to wrap exec.ErrNotFound", err)
		}
		if !strings.Contains(err.Error(), "git rev-parse HEAD") {
			t.Errorf("error = %q, want it to name the git command", err)
		}
	})
}
