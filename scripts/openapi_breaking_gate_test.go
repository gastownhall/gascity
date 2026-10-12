package scripts_test

import (
	"regexp"
	"strings"
	"testing"
)

// The OpenAPI breaking-change gate runs under Bazel only
// (//cmd/openapi-breaking:openapi-breaking_test, in the unit lane's //...):
// on pull requests and merge groups bazel.yml's unit lane writes the base
// commit's spec and names it in GC_OPENAPI_BREAKING_BASE_SPEC, the client
// environment variable @openapi_base_spec reads. Without that step the gate compares the spec
// with itself and passes every PR; with a second, go-test copy in ci.yml
// the gate would run twice.
func TestOpenAPIBreakingGateRunsInTheBazelUnitLane(t *testing.T) {
	root := repoRoot(t)

	for name, job := range readCriticalPathWorkflow(t, "ci.yml").Jobs {
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "openapi-breaking") {
				t.Errorf("ci.yml job %s runs %q; the gate is //cmd/openapi-breaking:openapi-breaking_test in bazel.yml's unit lane", name, strings.TrimSpace(step.Run))
			}
		}
	}

	var base *multiLaneStep
	steps := readMultiLaneWorkflow(t).Jobs["lane"].Steps
	testIndex, baseIndex := -1, -1
	for i := range steps {
		switch {
		case steps[i].ID == "test":
			testIndex = i
		case strings.Contains(steps[i].Run, "GC_OPENAPI_BREAKING_BASE_SPEC="):
			base, baseIndex = &steps[i], i
		}
	}
	if base == nil {
		t.Fatalf("%s: no lane step exports GC_OPENAPI_BREAKING_BASE_SPEC; the gate would compare the spec with itself", bazelMultiLaneWorkflow)
	}
	if testIndex < baseIndex {
		t.Errorf("%s: the base spec step (#%d) must run before the bazel step (#%d)", bazelMultiLaneWorkflow, baseIndex, testIndex)
	}
	// Pull requests and merge groups (the merge queue) have a base; a merge
	// group's is merge_group.base_sha, the commit the entry is queued onto.
	if want := "(github.event_name == 'pull_request' || github.event_name == 'merge_group') && matrix.lane == 'unit'"; base.If != want {
		t.Errorf("%s base spec step if = %q, want %q", bazelMultiLaneWorkflow, base.If, want)
	}
	if got, want := base.Env["BASE_SHA"], "${{ github.event_name == 'merge_group' && github.event.merge_group.base_sha || needs.rbe.outputs.base-sha }}"; got != want {
		t.Errorf("%s base spec step BASE_SHA = %q, want %q (merge_group.base_sha on a merge group, else fresh-merge's base-sha, the commit the PR is merged onto)", bazelMultiLaneWorkflow, got, want)
	}
	if !strings.Contains(base.Run, `git show "$BASE_SHA:internal/api/openapi.json"`) || !strings.Contains(base.Run, `>> "$GITHUB_ENV"`) {
		t.Errorf("%s base spec step must write git show $BASE_SHA:internal/api/openapi.json and export its path through $GITHUB_ENV:\n%s", bazelMultiLaneWorkflow, base.Run)
	}

	module := readFile(t, root, "MODULE.bazel")
	if !regexp.MustCompile(`(?m)^openapi_base_spec\(\n    name = "openapi_base_spec",`).MatchString(module) {
		t.Error("MODULE.bazel declares no @openapi_base_spec repository")
	}
	rule := readFile(t, root, "tools/bazel/rules/openapi_base_spec.bzl")
	if !strings.Contains(rule, `_ENV = "GC_OPENAPI_BREAKING_BASE_SPEC"`) || !strings.Contains(rule, "rctx.getenv(_ENV)") {
		t.Error("tools/bazel/rules/openapi_base_spec.bzl no longer reads GC_OPENAPI_BREAKING_BASE_SPEC from the client environment")
	}
}
