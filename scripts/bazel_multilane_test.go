package scripts_test

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// bazel.yml (rbe-west plan W1) runs beside bazel-test.yml as a preview. These
// tests keep the two workflows' remote cache keys equal, keep the preview off
// the required check names, and keep .github/actions/setup-bazel a byte copy
// of beads' composite.

const (
	bazelMultiLaneWorkflow = ".github/workflows/bazel.yml"
	setupBazelDir          = ".github/actions/setup-bazel"
)

// setupBazelBeadsDigests: sha256 of beads' .github/actions/setup-bazel files
// (beads ci/r4-client-heap, the R4 -Xmx4g line on top of origin/main
// e8b024c8). A change here is a change in beads first: copy all three files
// from beads and update these digests in the same PR.
var setupBazelBeadsDigests = map[string]string{
	"action.yml":         "9dba170b11c0c2acbde181715e9a801d95972c5f1a74aa5ffaa12f3a12ab886d",
	"fork-credential.sh": "abd68bbacb42fa5c7a71d06aa652bcf0f2fbe870f00b13b51650fe95c3bef59e",
	"write-bazelrc.sh":   "ffd2f3ebca5a449e12db342c143b9d082cd1d3d5ab7abc8fac84475d5ed56550",
}

func TestSetupBazelIsBeadsByteCopy(t *testing.T) {
	root := repoRoot(t)
	for name, want := range setupBazelBeadsDigests {
		got := fmt.Sprintf("%x", sha256.Sum256([]byte(readFile(t, root, setupBazelDir+"/"+name))))
		if got != want {
			t.Errorf("%s/%s sha256 %s, want %s (beads' copy): change beads' composite first, then copy it here", setupBazelDir, name, got, want)
		}
	}
	// bazel-test.yml still runs tools/rbe/fork-credential.sh.
	if readFile(t, root, setupBazelDir+"/fork-credential.sh") != readFile(t, root, rbeForkCredential) {
		t.Errorf("%s/fork-credential.sh and %s differ; both are byte copies of beads'", setupBazelDir, rbeForkCredential)
	}
}

// --config=ci must carry bazel-test.yml's generated key-affecting lines with
// byte-identical values, and the tagged-suite configs bazel-test.yml's flags,
// or the two workflows stop sharing remote cache entries.
func TestBazelCIConfigMatchesBazelTestRC(t *testing.T) {
	root := repoRoot(t)
	rc := readFile(t, root, ".bazelrc")
	legacy := readFile(t, root, bazelTestWorkflow)
	for legacyLine, ciLine := range map[string]string{
		"echo 'build --remote_download_minimal'": "build:ci --remote_download_minimal",
		"echo 'build --jobs=64'":                 "build:ci --jobs=64",
	} {
		if !strings.Contains(legacy, legacyLine) {
			t.Errorf("%s no longer writes %q; update .bazelrc's %q to match", bazelTestWorkflow, legacyLine, ciLine)
		}
		if !strings.Contains(rc, "\n"+ciLine+"\n") {
			t.Errorf(".bazelrc lacks %q", ciLine)
		}
	}
	m := regexp.MustCompile(`echo 'test --test_env=PATH=([^']+)'`).FindStringSubmatch(legacy)
	if m == nil {
		t.Fatalf("%s writes no test --test_env=PATH=... line", bazelTestWorkflow)
	}
	if want := "\ntest:ci --test_env=PATH=" + m[1] + "\n"; !strings.Contains(rc, want) {
		t.Errorf(".bazelrc lacks %q (bazel-test.yml's value)", strings.TrimSpace(want))
	}
	for _, c := range []struct{ config, legacyFlags string }{
		{"acceptance", "--define=gotags=acceptance_a --test_timeout=1100"},
		{"integration", "--define=gotags=integration --test_timeout=1100"},
	} {
		if !strings.Contains(legacy, c.legacyFlags) {
			t.Errorf("%s no longer passes %q", bazelTestWorkflow, c.legacyFlags)
		}
		for _, f := range strings.Fields(c.legacyFlags) {
			if !strings.Contains(rc, "\ntest:"+c.config+" "+f+"\n") {
				t.Errorf(".bazelrc lacks test:%s %s", c.config, f)
			}
		}
	}
	for _, line := range []string{
		"test:ci --flaky_test_attempts=1",
		"test:sole-run --nocache_test_results",
		"test:sole-run --experimental_remote_cache_eviction_retries=0",
	} {
		if !strings.Contains(rc, "\n"+line+"\n") {
			t.Errorf(".bazelrc lacks %q", line)
		}
	}
}

type multiLaneWorkflow struct {
	Concurrency struct {
		CancelInProgress string `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
	Jobs map[string]struct {
		Name     string `yaml:"name"`
		RunsOn   string `yaml:"runs-on"`
		Strategy struct {
			Matrix struct {
				Include []map[string]any `yaml:"include"`
			} `yaml:"matrix"`
		} `yaml:"strategy"`
		Steps []struct {
			Name string            `yaml:"name"`
			Uses string            `yaml:"uses"`
			Run  string            `yaml:"run"`
			With map[string]string `yaml:"with"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func TestBazelMultiLaneWorkflowShape(t *testing.T) {
	root := repoRoot(t)
	var wf multiLaneWorkflow
	if err := yaml.Unmarshal([]byte(readFile(t, root, bazelMultiLaneWorkflow)), &wf); err != nil {
		t.Fatalf("parse %s: %v", bazelMultiLaneWorkflow, err)
	}
	// Two jobs with a required check's name would let either satisfy it.
	for id, job := range wf.Jobs {
		for _, required := range []string{"bazel test (side-by-side)", "BUILD files in sync"} {
			if job.Name == required {
				t.Errorf("job %s is named %q, a required check bazel-test.yml still reports; rename it at the cutover, when bazel-test.yml is deleted", id, required)
			}
		}
	}
	// A push never cancels the previous push's run.
	if got := wf.Concurrency.CancelInProgress; got != "${{ github.event_name == 'pull_request' }}" {
		t.Errorf("concurrency cancel-in-progress = %q; only pull_request runs may be cancelled", got)
	}
	lane, ok := wf.Jobs["lane"]
	if !ok {
		t.Fatalf("%s has no lane job", bazelMultiLaneWorkflow)
	}
	if want := "${{ (needs.rbe.outputs.mode == 'cache' || needs.rbe.outputs.mode == 'local') && 'blacksmith-4vcpu-ubuntu-2404' || 'blacksmith-2vcpu-ubuntu-2404' }}"; lane.RunsOn != want {
		t.Errorf("lane runs-on = %q, want %q (2 vCPU clients in remote modes)", lane.RunsOn, want)
	}
	if len(lane.Strategy.Matrix.Include) == 0 {
		t.Fatal("lane matrix has no include entries")
	}
	for _, entry := range lane.Strategy.Matrix.Include {
		cmd, _ := entry["cmd"].(string)
		if !strings.Contains(" "+cmd+" ", " --config=ci ") {
			t.Errorf("lane %v: cmd %q lacks --config=ci", entry["lane"], cmd)
		}
		// G0 makes sole-run a per-lane choice; until then no lane opts in.
		if strings.Contains(cmd, "sole-run") {
			t.Errorf("lane %v: cmd %q runs sole-run before G0", entry["lane"], cmd)
		}
	}
	for _, id := range []string{"lane", "sync-check"} {
		steps := wf.Jobs[id].Steps
		if len(steps) < 2 || !strings.HasPrefix(steps[0].Uses, "actions/checkout@") ||
			steps[0].With["fetch-depth"] != "0" || steps[0].With["filter"] != "blob:none" ||
			steps[1].Uses != "./.github/actions/fresh-merge" ||
			steps[1].With["base-sha"] != "${{ needs.rbe.outputs.base-sha }}" {
			t.Errorf("job %s: first steps must be a full-history blobless checkout, then fresh-merge with the rbe job's base-sha", id)
		}
	}
	var tmpdir, heap bool
	for _, step := range lane.Steps {
		tmpdir = tmpdir || strings.Contains(step.Run, "mkdir -p /tmp/bt")
		if step.Name == "Bazel client heap" && strings.Contains(step.Run, "bazel info peak-heap-size") {
			heap = true
		}
	}
	if !tmpdir || !heap {
		t.Errorf("lane job: /tmp/bt step %v, heap report step %v; want both", tmpdir, heap)
	}
}
