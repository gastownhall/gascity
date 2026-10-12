package scripts_test

import (
	"regexp"
	"testing"
)

// TestBazelWorkflowCreatesSandboxTmpDir guards bazel.yml's "Create Bazel test
// tmpdir" steps against the /tmp/bt sandbox failure. .bazelrc sets `test
// --test_tmpdir=/tmp/bt` and `test --sandbox_writable_path=/tmp/bt`
// unconditionally (since #6525, 26172ff4bd), but nothing creates that
// directory on the runner, so every sandboxed action fails with `I/O
// exception during sandboxed execution: [unix_jni.cc:382] /tmp/bt (No such
// file or directory)` before a single test can run. Runs with remote
// execution are unaffected; lanes that execute on the runner (fork-cache,
// local) take the hit on every invocation. Every job that runs `bazel test`
// or `bazel coverage` must create /tmp/bt first.
func TestBazelWorkflowCreatesSandboxTmpDir(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	bazelTestRun := regexp.MustCompile(`(?m)^\s*bazel\s+(coverage\s|"\$\{args\[@\]\}")`)
	jobs := 0
	for id, job := range wf.Jobs {
		mkdir := -1
		for i, step := range job.Steps {
			if step.Name == "Create Bazel test tmpdir" && regexp.MustCompile(`(?m)^\s*mkdir -p /tmp/bt\s*$`).MatchString(step.Run) && mkdir < 0 {
				mkdir = i
			}
			if !bazelTestRun.MatchString(step.Run) {
				continue
			}
			if i == 0 || mkdir < 0 {
				t.Errorf("job %s step %q runs bazel before a \"Create Bazel test tmpdir\" step (mkdir -p /tmp/bt)", id, step.Name)
			}
			jobs++
		}
	}
	if jobs == 0 {
		t.Fatalf("%s: found no bazel test or coverage step; the pattern no longer matches", bazelMultiLaneWorkflow)
	}
}
