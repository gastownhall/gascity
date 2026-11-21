package scripts_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestBranchFilteredPRWorkflowsCoverHotfixAndReleaseBases pins that every
// workflow whose pull_request trigger is filtered by base branch also runs
// for PRs into hotfix/** and release/** branches. Those PRs carry backports
// that ship in patch releases; a filter of [main] alone silently skips
// these scans on exactly the branches a release is cut from.
func TestBranchFilteredPRWorkflowsCoverHotfixAndReleaseBases(t *testing.T) {
	root := repoRoot(t)
	paths, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var filtered int
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			On map[string]yaml.Node `yaml:"on"`
		}
		if err := yaml.Unmarshal(body, &doc); err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
		node, ok := doc.On["pull_request"]
		if !ok || node.Kind != yaml.MappingNode {
			continue
		}
		var trigger struct {
			Branches []string `yaml:"branches"`
		}
		if err := node.Decode(&trigger); err != nil {
			t.Fatalf("%s: pull_request: %v", filepath.Base(path), err)
		}
		if len(trigger.Branches) == 0 {
			continue
		}
		filtered++
		for _, want := range []string{"main", "hotfix/**", "release/**"} {
			if !slices.Contains(trigger.Branches, want) {
				t.Errorf("%s: pull_request.branches = %v, missing %q", filepath.Base(path), trigger.Branches, want)
			}
		}
	}
	if filtered == 0 {
		t.Fatal("no branch-filtered pull_request workflows found; the walk is broken")
	}
}
