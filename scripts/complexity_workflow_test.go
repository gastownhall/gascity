package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestComplexityWorkflowIsAdvisoryAndPinned(t *testing.T) {
	root := repoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "complexity.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	var doc struct {
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			ContinueOnError *bool `yaml:"continue-on-error"`
			Steps           []struct {
				Uses            string            `yaml:"uses"`
				Run             string            `yaml:"run"`
				ContinueOnError bool              `yaml:"continue-on-error"`
				With            map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("workflow YAML: %v", err)
	}
	if doc.Permissions["contents"] != "read" {
		t.Fatalf("permissions = %#v, want contents: read", doc.Permissions)
	}
	if !strings.Contains(text, "pull_request:") || !strings.Contains(text, "types: [opened, reopened, synchronize, ready_for_review]") || !strings.Contains(text, "branches: [main]") {
		t.Fatalf("workflow must run on PR lifecycle and pushes to main")
	}
	job, ok := doc.Jobs["report"]
	if !ok || job.ContinueOnError != nil {
		t.Fatalf("report job missing or masks setup failures: %#v", doc.Jobs)
	}
	var checkout, setup, artifact, pathSetup, report, diff bool
	for _, step := range job.Steps {
		switch {
		case strings.HasPrefix(step.Uses, "actions/checkout@"):
			checkout = strings.Contains(step.Uses, "@de0fac2e4500dabe0009e67214ff5f5447ce83dd")
		case strings.HasPrefix(step.Uses, "actions/setup-go@"):
			setup = strings.Contains(step.Uses, "@4a3601121dd01d1626a1e23e37211e3254c1c06c")
		case strings.HasPrefix(step.Uses, "actions/upload-artifact@"):
			artifact = strings.Contains(step.Uses, "@ea165f8d65b6e75b540449e92b4886f43607fa02")
		}
		if strings.Contains(step.Run, `echo "$RUNNER_TEMP/bin" >> "$GITHUB_PATH"`) {
			pathSetup = true
		}
		if strings.Contains(step.Run, "make complexity") && step.ContinueOnError {
			report = true
		}
		if strings.Contains(step.Run, "make complexity-diff") && step.ContinueOnError {
			diff = true
		}
		if step.With["fetch-depth"] != "0" && strings.HasPrefix(step.Uses, "actions/checkout@") {
			checkout = false
		}
		if strings.HasPrefix(step.Uses, "actions/checkout@") && step.With["persist-credentials"] != "false" {
			checkout = false
		}
	}
	if !checkout || !setup || !artifact || !pathSetup || !report || !diff {
		t.Fatalf("workflow pins/advisory behavior: checkout=%t setup=%t artifact=%t path=%t report=%t diff=%t", checkout, setup, artifact, pathSetup, report, diff)
	}
	if strings.Contains(text, "needs: report") || strings.Contains(text, "required") {
		t.Fatalf("complexity workflow must not declare a required-gate dependency")
	}
}
