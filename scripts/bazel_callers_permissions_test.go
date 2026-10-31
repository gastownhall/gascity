package scripts_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// permissionRank orders GitHub token permission levels so a grant can be
// compared with a request: write covers read, read covers none.
var permissionRank = map[string]int{"": 0, "none": 0, "read": 1, "write": 2}

type permWorkflow struct {
	Jobs map[string]struct {
		Uses        string            `yaml:"uses"`
		Permissions map[string]string `yaml:"permissions"`
	} `yaml:"jobs"`
}

func loadPermWorkflow(t *testing.T, path string) permWorkflow {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf permWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return wf
}

// TestBazelWorkflowCallersGrantEveryRequestedPermission guards against
// startup_failure: GitHub validates a reusable workflow's job permissions
// when the caller starts, including jobs whose `if:` would skip, so every
// workflow that calls bazel.yml must grant at least the highest level any
// bazel.yml job requests for each scope.
func TestBazelWorkflowCallersGrantEveryRequestedPermission(t *testing.T) {
	dir := filepath.Join(repoRoot(t), ".github", "workflows")
	callee := loadPermWorkflow(t, filepath.Join(dir, "bazel.yml"))
	need := map[string]string{}
	for _, job := range callee.Jobs {
		for scope, level := range job.Permissions {
			if permissionRank[level] > permissionRank[need[scope]] {
				need[scope] = level
			}
		}
	}
	if len(need) == 0 {
		t.Fatal("bazel.yml requests no job permissions; the parser is broken")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	callers := 0
	for _, e := range entries {
		name := e.Name()
		if name == "bazel.yml" || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		wf := loadPermWorkflow(t, filepath.Join(dir, name))
		for jobName, job := range wf.Jobs {
			if job.Uses != "./.github/workflows/bazel.yml" {
				continue
			}
			callers++
			var missing []string
			for scope, level := range need {
				if permissionRank[job.Permissions[scope]] < permissionRank[level] {
					missing = append(missing, scope+": "+level)
				}
			}
			sort.Strings(missing)
			if len(missing) > 0 {
				t.Errorf("%s job %q calls bazel.yml without granting %s (startup_failure)", name, jobName, strings.Join(missing, ", "))
			}
		}
	}
	if callers == 0 {
		t.Fatal("found no workflow calling bazel.yml; the scan is broken")
	}
}
