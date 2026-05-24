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
	repoRoot := repoRoot(t)
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
	cmd.Env = goTestScriptEnv(t, tmpDir)

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

func goTestScriptEnv(t *testing.T, tmpDir string) []string {
	t.Helper()

	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + tmpDir,
	}
	for _, key := range []string{
		"GOPATH",
		"GOCACHE",
		"GOMODCACHE",
		"GOROOT",
		"GOENV",
		"GOFLAGS",
		"GO111MODULE",
		"GOEXPERIMENT",
		"GOPROXY",
		"GOPRIVATE",
		"GONOPROXY",
		"GONOSUMDB",
		"GOSUMDB",
		"GOINSECURE",
		"GOVCS",
		"GOWORK",
	} {
		value := os.Getenv(key)
		if value == "" {
			value = goEnvValue(t, key)
		}
		if value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func goEnvValue(t *testing.T, key string) string {
	t.Helper()
	out, err := exec.Command("go", "env", key).Output()
	if err != nil {
		t.Fatalf("go env %s: %v", key, err)
	}
	return strings.TrimSpace(string(out))
}
