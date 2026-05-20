package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestGoTestObservableDefaultLogPathIsUnique(t *testing.T) {
	repoRoot := filepath.Dir(t.TempDir())
	if wd, err := os.Getwd(); err == nil {
		repoRoot = filepath.Dir(wd)
	}
	tmpDir := t.TempDir()

	first := runObservableTestLogPath(t, repoRoot, tmpDir)
	second := runObservableTestLogPath(t, repoRoot, tmpDir)
	t.Cleanup(func() {
		_ = os.Remove(first)
		_ = os.Remove(second)
	})

	if first == second {
		t.Fatalf("default log paths should be unique, got %q twice", first)
	}
	for _, path := range []string{first, second} {
		if !strings.HasPrefix(path, tmpDir+string(os.PathSeparator)) {
			t.Fatalf("default log path %q should be under TMPDIR %q", path, tmpDir)
		}
		if filepath.Base(path) == "gascity-observable-log-test.jsonl" {
			t.Fatalf("default log path %q should not be a shared deterministic file", path)
		}
	}
}

func runObservableTestLogPath(t *testing.T, repoRoot, tmpDir string) string {
	t.Helper()

	cmd := exec.Command(
		filepath.Join(repoRoot, "scripts", "go-test-observable"),
		"observable-log-test",
		"--",
		"./internal/shellquote",
		"-run",
		"^$",
		"-count=1",
	)
	cmd.Dir = repoRoot
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + tmpDir,
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go-test-observable failed: %v\n%s", err, out)
	}

	match := regexp.MustCompile(`(?m)^observable go test: log=(.+)$`).FindSubmatch(out)
	if match == nil {
		t.Fatalf("go-test-observable output did not include log path:\n%s", out)
	}
	return strings.TrimSpace(string(match[1]))
}
