package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gazelleExcludeFixture is a scratch repository holding a copy of
// tools/bazel/gazelle_exclude_guard.py, a Makefile, a workflow and the given
// BUILD.bazel files (keyed by package directory).
func gazelleExcludeFixture(t *testing.T, builds map[string]string) string {
	t.Helper()
	root := t.TempDir()
	script, err := os.ReadFile(filepath.Join(repoRoot(t), "tools", "bazel", "gazelle_exclude_guard.py"))
	if err != nil {
		t.Fatalf("read gazelle_exclude_guard.py: %v", err)
	}
	writeTestFile(t, filepath.Join(root, "tools", "bazel", "gazelle_exclude_guard.py"), string(script))
	writeTestFile(t, filepath.Join(root, "Makefile"), "test-mac:\n\ttrue\n\ntest-go:\n\ttrue\n")
	writeTestFile(t, filepath.Join(root, ".github", "workflows", "mac.yml"), "jobs:\n  mac:\n    steps:\n      - run: make test-mac\n")
	for pkg, content := range builds {
		writeTestFile(t, filepath.Join(root, pkg, "BUILD.bazel"), content)
	}
	return root
}

func runGazelleExcludeGuard(t *testing.T, root string) (string, error) {
	t.Helper()
	cmd := testCommand("python3", filepath.Join(root, "tools", "bazel", "gazelle_exclude_guard.py"))
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func TestGazelleExcludeGuard(t *testing.T) {
	cases := []struct {
		name  string
		build string
		// wantFail is a substring the failure output must contain; empty
		// means the guard must pass.
		wantFail string
	}{
		{
			name: "excluded file still run by a go_test in the same BUILD file",
			build: "# gazelle:exclude a_test.go\n\n" +
				"go_test(\n    name = \"unsharded_test\",\n    srcs = [\"a_test.go\"],\n)\n",
		},
		{
			name: "excluded file with a named gate step that a workflow runs",
			build: "# gazelle:exclude a_test.go\n" +
				"# excluded-test-gate: a_test.go test-mac\n",
		},
		{
			name: "excluded file with a reviewed reason",
			build: "# gazelle:exclude a_test.go\n" +
				"# excluded-test-reason: a_test.go needs a live GPU, no gate has one\n",
		},
		{
			name:     "excluded file that nothing runs",
			build:    "# gazelle:exclude a_test.go\n",
			wantFail: "a_test.go",
		},
		{
			name: "gate step that is not a Makefile target",
			build: "# gazelle:exclude a_test.go\n" +
				"# excluded-test-gate: a_test.go no-such-target\n",
			wantFail: "no-such-target",
		},
		{
			name: "gate step no workflow runs",
			build: "# gazelle:exclude a_test.go\n" +
				"# excluded-test-gate: a_test.go test-go\n",
			wantFail: "test-go",
		},
		{
			name: "reason with no text",
			build: "# gazelle:exclude a_test.go\n" +
				"# excluded-test-reason: a_test.go\n",
			wantFail: "a_test.go",
		},
		{
			name: "marker for a file that is no longer excluded",
			build: "# excluded-test-gate: gone_test.go test-mac\n" +
				"go_test(\n    name = \"x_test\",\n    srcs = [\"x_test.go\"],\n)\n",
			wantFail: "gone_test.go",
		},
		{
			name: "a quoted mention in a comment is not a go_test src",
			build: "# gazelle:exclude a_test.go\n" +
				"# see \"a_test.go\" for why\n",
			wantFail: "a_test.go",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := gazelleExcludeFixture(t, map[string]string{"pkg": tc.build})
			output, err := runGazelleExcludeGuard(t, root)
			if tc.wantFail == "" {
				if err != nil {
					t.Fatalf("guard failed on an accounted-for exclude: %v\n%s", err, output)
				}
				return
			}
			if err == nil {
				t.Fatalf("guard passed, want a failure naming %q:\n%s", tc.wantFail, output)
			}
			if !strings.Contains(output, tc.wantFail) || !strings.Contains(output, "pkg/BUILD.bazel") {
				t.Errorf("failure does not name %q in pkg/BUILD.bazel:\n%s", tc.wantFail, output)
			}
		})
	}
}

// TestBazelSyncRunsGazelleExcludeGuard pins the wiring: the guard walks every
// BUILD.bazel, which a bazel test sandbox cannot see, so `make bazel-sync`
// (and with it CI's "BUILD files in sync" job) is where it runs.
func TestBazelSyncRunsGazelleExcludeGuard(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	recipe := string(data)
	start := strings.Index(recipe, "\nbazel-sync:")
	if start < 0 {
		t.Fatal("Makefile has no bazel-sync target")
	}
	body := recipe[start:]
	if end := strings.Index(body[1:], "\n\n"); end >= 0 {
		body = body[:end+1]
	}
	if !strings.Contains(body, "python3 tools/bazel/gazelle_exclude_guard.py") {
		t.Errorf("bazel-sync does not run tools/bazel/gazelle_exclude_guard.py:\n%s", body)
	}
}
