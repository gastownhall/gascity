package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// prtWorkflow is the subset of a workflow file the pull_request_target
// checkout policy inspects.
type prtWorkflow struct {
	On   yaml.Node `yaml:"on"`
	Jobs map[string]struct {
		Steps []struct {
			Uses string         `yaml:"uses"`
			With map[string]any `yaml:"with"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// triggersOn reports whether the workflow's `on:` block names event, in any
// of its three spellings (scalar, sequence, or mapping key).
func (wf prtWorkflow) triggersOn(event string) bool {
	switch wf.On.Kind {
	case yaml.ScalarNode:
		return wf.On.Value == event
	case yaml.SequenceNode:
		for _, n := range wf.On.Content {
			if n.Value == event {
				return true
			}
		}
	case yaml.MappingNode:
		for i := 0; i < len(wf.On.Content); i += 2 {
			if wf.On.Content[i].Value == event {
				return true
			}
		}
	}
	return false
}

// TestPullRequestTargetCheckoutsUseWorkflowCommit guards Scorecard's
// Dangerous-Workflow check (alert 65): a pull_request_target job runs with a
// privileged token, so every actions/checkout in it must check out the
// commit the workflow file was read from (github.workflow_sha, always a
// default-branch commit under pull_request_target) and never a ref taken
// from the github.event.pull_request payload, and must not persist the
// token into the checkout.
func TestPullRequestTargetCheckoutsUseWorkflowCommit(t *testing.T) {
	dir := filepath.Join(repoRoot(t), ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var wf prtWorkflow
		if err := yaml.Unmarshal(data, &wf); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		if !wf.triggersOn("pull_request_target") {
			continue
		}
		for jobName, job := range wf.Jobs {
			for _, step := range job.Steps {
				if !strings.HasPrefix(step.Uses, "actions/checkout@") {
					continue
				}
				checked++
				ref, _ := step.With["ref"].(string)
				if ref != "${{ github.workflow_sha }}" {
					t.Errorf("%s job %q: pull_request_target checkout must use ref: ${{ github.workflow_sha }}, got %q", name, jobName, ref)
				}
				if persist, ok := step.With["persist-credentials"].(bool); !ok || persist {
					t.Errorf("%s job %q: pull_request_target checkout must set persist-credentials: false, got %v", name, jobName, step.With["persist-credentials"])
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no actions/checkout step in any pull_request_target workflow; the parser is broken")
	}
}
