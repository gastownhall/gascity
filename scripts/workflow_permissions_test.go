package scripts_test

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// workflowPermissionsFile is the subset of a GitHub Actions workflow that
// carries GITHUB_TOKEN grants. permissions may be a scope map or a shorthand
// string ("read-all", "write-all"), so it stays a yaml.Node until inspected.
type workflowPermissionsFile struct {
	Permissions yaml.Node `yaml:"permissions"`
	Jobs        map[string]struct {
		Permissions yaml.Node `yaml:"permissions"`
	} `yaml:"jobs"`
}

func parseWorkflowPermissions(t *testing.T, path string) workflowPermissionsFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf workflowPermissionsFile
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return wf
}

// writeGrants returns the scopes a permissions node grants write on, with
// "*" standing for the write-all shorthand.
func writeGrants(t *testing.T, file string, node yaml.Node) []string {
	t.Helper()
	switch node.Kind {
	case 0:
		return nil
	case yaml.ScalarNode:
		if node.Value == "write-all" {
			return []string{"*"}
		}
		return nil
	case yaml.MappingNode:
		var grants []string
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i+1].Value == "write" {
				grants = append(grants, node.Content[i].Value)
			}
		}
		return grants
	default:
		t.Fatalf("%s: unexpected permissions node kind %v", file, node.Kind)
		return nil
	}
}

// scopeGrants decodes an explicit permissions scope map into its access
// level per scope. A missing block or a shorthand string fails the test:
// every block pinned here is an explicit scope map.
func scopeGrants(t *testing.T, where string, node yaml.Node) map[string]string {
	t.Helper()
	if node.Kind == 0 {
		t.Fatalf("%s: no permissions block", where)
	}
	if node.Kind != yaml.MappingNode {
		t.Fatalf("%s: permissions must be an explicit scope map, got node kind %v", where, node.Kind)
	}
	var grants map[string]string
	if err := node.Decode(&grants); err != nil {
		t.Fatalf("%s: decode permissions: %v", where, err)
	}
	return grants
}

// TestWorkflowLevelPermissionsAreReadOnly guards against Scorecard
// TokenPermissions regressions (alerts 67, 248, 442, 375): a write grant at
// workflow level is inherited by every job, so writes must be declared on
// the individual jobs that need them.
func TestWorkflowLevelPermissionsAreReadOnly(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(repoRoot(t), ".github", "workflows", "*.y*ml"))
	if err != nil {
		t.Fatalf("glob workflows: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no workflow files found")
	}
	for _, path := range paths {
		file := filepath.Base(path)
		wf := parseWorkflowPermissions(t, path)
		if wf.Permissions.Kind == 0 {
			t.Errorf("%s: missing workflow-level permissions block; the default token grant is not read-only", file)
			continue
		}
		if grants := writeGrants(t, file, wf.Permissions); len(grants) > 0 {
			t.Errorf("%s: workflow-level permissions must be read-only, got write on %s; move the grant to the job that needs it", file, strings.Join(grants, ", "))
		}
	}
}

// TestWorkflowWritePermissionsAreJobScoped pins the exact permissions block
// of each job that took a write grant off the workflow level. A job-level
// block replaces the workflow default rather than merging with it, so a
// dropped read or a widened grant fails here, not at runtime or in a
// Scorecard alert.
func TestWorkflowWritePermissionsAreJobScoped(t *testing.T) {
	cases := []struct {
		file     string
		writeJob string
		want     map[string]string
	}{
		{
			file:     "docs-autofix.yml",
			writeJob: "autofix",
			want:     map[string]string{"contents": "write", "pull-requests": "write", "actions": "read"},
		},
		{
			file:     "codeql.yml",
			writeJob: "analyze",
			want:     map[string]string{"actions": "read", "contents": "read", "security-events": "write"},
		},
		{
			file:     "dispatch-labeled-pr-suite.yml",
			writeJob: "dispatch-suite",
			want:     map[string]string{"actions": "write", "contents": "read", "pull-requests": "read"},
		},
		{
			file:     "ci-verdict-watchdog.yml",
			writeJob: "flag-cancelled-while-queued", //nolint:misspell // workflow job ID
			want:     map[string]string{"checks": "write", "actions": "read"},
		},
		{
			file:     "worker-env-notify-beads-pr.yml",
			writeJob: "comment",
			want:     map[string]string{"pull-requests": "write"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			wf := parseWorkflowPermissions(t, filepath.Join(repoRoot(t), ".github", "workflows", tc.file))
			job, ok := wf.Jobs[tc.writeJob]
			if !ok {
				t.Fatalf("%s: expected job %q not found", tc.file, tc.writeJob)
			}
			got := scopeGrants(t, fmt.Sprintf("%s job %q", tc.file, tc.writeJob), job.Permissions)
			if !maps.Equal(got, tc.want) {
				t.Errorf("%s: job %q permissions = %v, want exactly %v", tc.file, tc.writeJob, got, tc.want)
			}
		})
	}
}

// TestCodeQLRunnerPolicyRunsOnTheWorkflowGrant pins the other half of the
// codeql.yml split: runner-policy declares no permissions block of its own,
// so it runs on the workflow-level grant, which must stay exactly
// actions: read and contents: read.
func TestCodeQLRunnerPolicyRunsOnTheWorkflowGrant(t *testing.T) {
	const file, jobName = "codeql.yml", "runner-policy"
	wf := parseWorkflowPermissions(t, filepath.Join(repoRoot(t), ".github", "workflows", file))
	job, ok := wf.Jobs[jobName]
	if !ok {
		t.Fatalf("%s: expected job %q not found", file, jobName)
	}
	if job.Permissions.Kind != 0 {
		t.Errorf("%s: job %q must run on the workflow-level grant, not its own permissions block", file, jobName)
	}
	want := map[string]string{"actions": "read", "contents": "read"}
	if got := scopeGrants(t, file+" workflow level", wf.Permissions); !maps.Equal(got, want) {
		t.Errorf("%s: workflow-level permissions = %v, want exactly %v", file, got, want)
	}
}
