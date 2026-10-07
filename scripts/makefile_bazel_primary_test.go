package scripts_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// The primary make targets run the bazel commands CI gates on
// (.github/workflows/bazel.yml's lanes, minus the lane-only --config=ci), and
// each keeps a plain-go twin under an explicit -go name. GitHub Actions jobs
// default to the go twins while the Go-tier jobs that call these names by
// hand still exist; TEST_ENGINE overrides either default.

const fakeMakeBazel = "/fake/bazel"

func makeDryRun(t *testing.T, target string, env ...string) string {
	t.Helper()
	cmd := makeCommand("--no-print-directory", "-n",
		"-f", filepath.Join(repoRoot(t), "Makefile"),
		"BAZEL="+fakeMakeBazel,
		"SYS_USR_CGO_FALLBACK=0",
		target)
	cmd.Dir = repoRoot(t)
	for _, entry := range filteredMakefileCGOTestEnv() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "GITHUB_ACTIONS" || name == "TEST_ENGINE" || name == "BAZEL_FLAGS" {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n %s: %v\n%s", target, err, out)
	}
	return string(out)
}

func TestMakePrimaryTargetsRunBazel(t *testing.T) {
	for target, want := range map[string]string{
		"test":             fakeMakeBazel + " test  --keep_going //...",
		"check":            fakeMakeBazel + " test  --keep_going //...",
		"check-docs":       fakeMakeBazel + " test  --keep_going //test/docsync:docsync_test",
		"test-acceptance":  fakeMakeBazel + " test  --keep_going --config=acceptance //test/acceptance:acceptance_test",
		"test-integration": fakeMakeBazel + " test  --keep_going --config=integration //test:integration_packages //test/integration:integration_test",
	} {
		t.Run(target, func(t *testing.T) {
			out := makeDryRun(t, target)
			if !strings.Contains(out, want) {
				t.Errorf("make %s does not run %q:\n%s", target, want, out)
			}
			if strings.Contains(out, "go test") || strings.Contains(out, "go-test-observable") {
				t.Errorf("make %s runs go test under the bazel engine:\n%s", target, out)
			}
		})
	}
}

func TestMakeCheckKeepsShellGuardsBesideBazel(t *testing.T) {
	out := makeDryRun(t, "check")
	for _, want := range []string{
		"./scripts/check-routed-test-rows.sh",
		"./scripts/check-split-topology-rows.sh",
		"./scripts/check-residency-boundary.sh",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("make check does not run %s:\n%s", want, out)
		}
	}
}

func TestMakeGoTwinsRunGoTest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		env    []string
		want   string
	}{
		{name: "test-go", target: "test-go", want: "scripts/go-test-observable test"},
		{name: "check-docs-go", target: "check-docs-go", want: "go test ./test/docsync"},
		{name: "test-acceptance-go", target: "test-acceptance-go", want: "go test -tags acceptance_a"},
		{name: "test-integration-go", target: "test-integration-go", want: "go test -tags integration"},
		{name: "explicit go engine", target: "test", env: []string{"TEST_ENGINE=go"}, want: "scripts/go-test-observable test"},
		{name: "github actions defaults to go", target: "test", env: []string{"GITHUB_ACTIONS=true"}, want: "scripts/go-test-observable test"},
		{name: "github actions acceptance defaults to go", target: "test-acceptance", env: []string{"GITHUB_ACTIONS=true"}, want: "go test -tags acceptance_a"},
		{name: "github actions check-docs defaults to go", target: "check-docs", env: []string{"GITHUB_ACTIONS=true"}, want: "go test ./test/docsync"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := makeDryRun(t, tc.target, tc.env...)
			if !strings.Contains(out, tc.want) {
				t.Errorf("make %s does not run %q:\n%s", tc.target, tc.want, out)
			}
			if strings.Contains(out, fakeMakeBazel) {
				t.Errorf("make %s runs bazel under the go engine:\n%s", tc.target, out)
			}
		})
	}
}

func TestMakeBazelEngineOverridesGitHubActionsDefault(t *testing.T) {
	out := makeDryRun(t, "test", "GITHUB_ACTIONS=true", "TEST_ENGINE=bazel")
	if !strings.Contains(out, fakeMakeBazel+" test  --keep_going //...") {
		t.Errorf("TEST_ENGINE=bazel does not run bazel under GitHub Actions:\n%s", out)
	}
}

func TestMakeRejectsUnknownTestEngine(t *testing.T) {
	cmd := makeCommand("--no-print-directory", "-n",
		"-f", filepath.Join(repoRoot(t), "Makefile"),
		"SYS_USR_CGO_FALLBACK=0", "TEST_ENGINE=gradle", "test")
	cmd.Dir = repoRoot(t)
	cmd.Env = filteredMakefileCGOTestEnv()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("make TEST_ENGINE=gradle succeeded:\n%s", out)
	}
	if !strings.Contains(string(out), "TEST_ENGINE=gradle is not one of bazel, go") {
		t.Errorf("unknown engine error does not name the choices:\n%s", out)
	}
}
