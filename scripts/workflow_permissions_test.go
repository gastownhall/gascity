package scripts_test

import (
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

// TestWorkflowLevelPermissionsAreReadOnly guards against Scorecard
// TokenPermissions regressions (alerts 67, 68, 248): a write grant at
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

// TestWorkflowWritePermissionsAreJobScoped pins the job-level grants that
// were moved off the workflow level, so dropping one breaks here instead of
// at runtime.
func TestWorkflowWritePermissionsAreJobScoped(t *testing.T) {
	cases := []struct {
		file       string
		writeJob   string
		wantWrites []string
	}{
		{
			file:       "docs-autofix.yml",
			writeJob:   "autofix",
			wantWrites: []string{"contents", "pull-requests"},
		},
		{
			file:       "codeql.yml",
			writeJob:   "analyze",
			wantWrites: []string{"security-events"},
		},
		{
			file:       "dispatch-labeled-pr-suite.yml",
			writeJob:   "dispatch-suite",
			wantWrites: []string{"actions"},
		},
		{
			file:       "ci-verdict-watchdog.yml",
			writeJob:   "flag-cancelled-while-queued", //nolint:misspell // workflow job ID
			wantWrites: []string{"checks"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			wf := parseWorkflowPermissions(t, filepath.Join(repoRoot(t), ".github", "workflows", tc.file))
			job, ok := wf.Jobs[tc.writeJob]
			if !ok {
				t.Fatalf("%s: expected job %q not found", tc.file, tc.writeJob)
			}
			got := map[string]bool{}
			for _, scope := range writeGrants(t, tc.file, job.Permissions) {
				got[scope] = true
			}
			for _, scope := range tc.wantWrites {
				if !got[scope] {
					t.Errorf("%s: job %q must declare %s: write", tc.file, tc.writeJob, scope)
				}
			}
		})
	}
}
