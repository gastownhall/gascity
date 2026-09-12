package scripts_test

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestWorkflowWritePermissionsAreJobScoped guards against Scorecard
// TokenPermissions regressions (alerts 67, 68, 248): each of these
// workflows previously granted a write permission at workflow level, where
// every job inherits it, when only one job actually needs it.
func TestWorkflowWritePermissionsAreJobScoped(t *testing.T) {
	type workflowFile struct {
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			Permissions map[string]string `yaml:"permissions"`
		} `yaml:"jobs"`
	}

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
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			path := filepath.Join(repoRoot(t), ".github", "workflows", tc.file)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", tc.file, err)
			}
			var wf workflowFile
			if err := yaml.Unmarshal(data, &wf); err != nil {
				t.Fatalf("parse %s: %v", tc.file, err)
			}

			for scope, level := range wf.Permissions {
				if level == "write" {
					t.Errorf("%s: workflow-level permissions must be read-only, got %s: write", tc.file, scope)
				}
			}

			job, ok := wf.Jobs[tc.writeJob]
			if !ok {
				t.Fatalf("%s: expected job %q not found", tc.file, tc.writeJob)
			}
			for _, scope := range tc.wantWrites {
				if got := job.Permissions[scope]; got != "write" {
					t.Errorf("%s: job %q must declare %s: write, got %q", tc.file, tc.writeJob, scope, got)
				}
			}
		})
	}
}
