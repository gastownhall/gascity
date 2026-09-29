package scripts_test

import (
	"strings"
	"testing"
)

// TestBazelTestWorkflowCreatesSandboxTmpDir guards the "Writable test HOME"
// step in .github/workflows/bazel-test.yml against the /tmp/bt sandbox
// failure. .bazelrc sets `test --test_tmpdir=/tmp/bt` and
// `test --sandbox_writable_path=/tmp/bt` unconditionally (since #6525,
// 26172ff4bd), but nothing creates that directory on the runner, so every
// sandboxed action fails with `I/O exception during sandboxed execution:
// [unix_jni.cc:382] /tmp/bt (No such file or directory)` before a single
// test can run. Runs with remote execution (same-repo PRs, main pushes) are
// unaffected; fork PRs and our own deploy/*-gate PRs — which run locally,
// no RBE secret — take the hit on every invocation. The step must create
// /tmp/bt before any `bazel test` invocation.
func TestBazelTestWorkflowCreatesSandboxTmpDir(t *testing.T) {
	root := repoRoot(t)
	workflow := readFile(t, root, ".github/workflows/bazel-test.yml")

	step := extractWritableTestHomeStep(t, workflow)

	if !strings.Contains(step, "mkdir -p /tmp/bt") {
		t.Error(`Writable test HOME step must create /tmp/bt (mkdir -p /tmp/bt) so the .bazelrc test_tmpdir/sandbox_writable_path mount does not fail on a cold runner`)
	}
}

// extractWritableTestHomeStep returns the raw YAML text of the "Writable
// test HOME" step, from its "- name:" line up to (but not including) the
// next step's "- name:" line.
func extractWritableTestHomeStep(t *testing.T, workflow string) string {
	t.Helper()
	start := strings.Index(workflow, "- name: Writable test HOME")
	if start < 0 {
		t.Fatal(`bazel-test.yml missing the "Writable test HOME" step`)
	}
	rest := workflow[start:]
	end := strings.Index(rest[1:], "\n      - name:")
	if end < 0 {
		return rest
	}
	return rest[:end+1]
}
