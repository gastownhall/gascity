package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGoTestShardPreservesAcceptanceAuthEnv(t *testing.T) {
	repoRoot := filepath.Dir(t.TempDir())
	if wd, err := os.Getwd(); err == nil {
		repoRoot = filepath.Dir(wd)
	}

	cmd := exec.Command(
		filepath.Join(repoRoot, "scripts", "test-go-test-shard"),
		"./scripts/testdata/test-go-test-shard/env_required",
		"1",
		"1",
	)
	cmd.Dir = repoRoot
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GO_TEST_TIMEOUT=1m",
		"ANTHROPIC_AUTH_TOKEN=synthetic-token",
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test-go-test-shard failed: %v\n%s", err, out)
	}
}

func TestGoTestShardRunsWithoutPreservedProviderEnv(t *testing.T) {
	repoRoot := filepath.Dir(t.TempDir())
	if wd, err := os.Getwd(); err == nil {
		repoRoot = filepath.Dir(wd)
	}

	cmd := exec.Command(
		filepath.Join(repoRoot, "scripts", "test-go-test-shard"),
		"./scripts/testdata/test-go-test-shard/no_extra_env",
		"1",
		"1",
	)
	cmd.Dir = repoRoot
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GO_TEST_TIMEOUT=1m",
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test-go-test-shard failed without preserved provider env: %v\n%s", err, out)
	}
}
