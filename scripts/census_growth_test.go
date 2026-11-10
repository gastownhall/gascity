package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const censusGrowthWorkflow = "census-growth.yml"

func censusGrowthRead(t *testing.T, elem ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(append([]string{repoRoot(t)}, elem...)...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(elem...), err)
	}
	return string(data)
}

// TestCensusGrowthScriptIsAThinWrapper pins scripts/census-growth.go to the
// repo's runner shape: an ignored main that only calls the cli package.
func TestCensusGrowthScriptIsAThinWrapper(t *testing.T) {
	src := censusGrowthRead(t, "scripts", "census-growth.go")
	if !strings.HasPrefix(src, "//go:build ignore\n") {
		t.Error("scripts/census-growth.go must start with //go:build ignore")
	}
	if !strings.Contains(src, "censusgrowthcli.Run(os.Args[1:], os.Stdout, os.Stderr)") {
		t.Error("scripts/census-growth.go must delegate to censusgrowthcli.Run")
	}
	if lines := strings.Count(src, "\n"); lines > 20 {
		t.Errorf("scripts/census-growth.go has %d lines; the logic belongs in internal/testpolicy/censusgrowthcli", lines)
	}
}

type censusGrowthWorkflowFile struct {
	On   map[string]yaml.Node `yaml:"on"`
	Jobs map[string]struct {
		Permissions yaml.Node `yaml:"permissions"`
		Steps       []struct {
			Name string `yaml:"name"`
			If   string `yaml:"if"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func TestCensusGrowthWorkflowIsReportOnlyOnPullRequests(t *testing.T) {
	raw := censusGrowthRead(t, ".github", "workflows", censusGrowthWorkflow)
	var wf censusGrowthWorkflowFile
	if err := yaml.Unmarshal([]byte(raw), &wf); err != nil {
		t.Fatalf("parse %s: %v", censusGrowthWorkflow, err)
	}
	if _, ok := wf.On["pull_request"]; !ok {
		t.Errorf("%s must trigger on pull_request", censusGrowthWorkflow)
	}
	if _, ok := wf.On["pull_request_target"]; ok {
		t.Errorf("%s must not use pull_request_target: it would run with a write token on fork code", censusGrowthWorkflow)
	}

	// Comments are documentation, not behavior.
	var code []string
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			code = append(code, line)
		}
	}
	body := strings.Join(code, "\n")

	for _, want := range []string{
		"git show ${{ github.event.pull_request.base.sha }}:test/test-resource-counts.toml",
		"git show ${{ github.event.pull_request.base.sha }}:test/test-resources.toml",
		"$GITHUB_STEP_SUMMARY",
		"scripts/census-growth.go",
		"--base-counts", "--head-counts", "--base-policy", "--head-policy",
		"ledger-growth-approved",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s lacks %q", censusGrowthWorkflow, want)
		}
	}
	if strings.Contains(body, "--enforce") {
		t.Errorf("%s must leave --enforce off: report-only is the default posture", censusGrowthWorkflow)
	}
	if !strings.Contains(body, "--approved") {
		t.Errorf("%s must wire --approved from the maintainer label", censusGrowthWorkflow)
	}
	if strings.Contains(body, "github.event.pull_request.head.ref") || strings.Contains(body, "github.head_ref") {
		t.Errorf("%s must not interpolate the PR branch name into a shell command", censusGrowthWorkflow)
	}
}

func TestCensusGrowthStickyCommentIsSameRepoOnly(t *testing.T) {
	raw := censusGrowthRead(t, ".github", "workflows", censusGrowthWorkflow)
	var wf censusGrowthWorkflowFile
	if err := yaml.Unmarshal([]byte(raw), &wf); err != nil {
		t.Fatalf("parse %s: %v", censusGrowthWorkflow, err)
	}
	if !strings.Contains(raw, "<!-- census-growth-report -->") {
		t.Errorf("%s must key its sticky comment on the hidden marker the report opens with", censusGrowthWorkflow)
	}
	var commentSteps int
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			if !strings.Contains(step.Run, "issues/") && !strings.Contains(step.Run, "gh pr comment") && !strings.Contains(step.Run, "gh api") {
				continue
			}
			commentSteps++
			if !strings.Contains(step.If, "github.event.pull_request.head.repo.full_name == github.repository") {
				t.Errorf("comment step %q must be skipped for fork PRs (read-only token); if: %q", step.Name, step.If)
			}
		}
	}
	if commentSteps == 0 {
		t.Errorf("%s has no sticky-comment step", censusGrowthWorkflow)
	}
}

// TestWorkflowNeverNamesTheMakeTestEngine mirrors the repo rule for the new
// workflow only; the repo-wide guard is TestWorkflowsNameTheirTestEngine.
func TestCensusGrowthWorkflowRunsNoMakeTestTarget(t *testing.T) {
	raw := censusGrowthRead(t, ".github", "workflows", censusGrowthWorkflow)
	if regexp.MustCompile(`make (test|check|check-all|check-docs|test-acceptance|test-integration)\b`).MatchString(raw) {
		t.Errorf("%s must not run a make test target", censusGrowthWorkflow)
	}
}

func TestTestingMDDocumentsTheCensusGrowthReport(t *testing.T) {
	doc := censusGrowthRead(t, "TESTING.md")
	start := strings.Index(doc, "### Updating the resource census")
	if start < 0 {
		t.Fatal("TESTING.md lost its \"Updating the resource census\" section")
	}
	section := doc[start:]
	if next := strings.Index(section[4:], "\n### "); next >= 0 {
		section = section[:next+4]
	}
	for _, want := range []string{"GROWTH", "POLICY-ROW", "ledger-growth-approved", "scripts/census-growth.go", "report-only"} {
		if !strings.Contains(section, want) {
			t.Errorf("the census section of TESTING.md does not mention %q", want)
		}
	}
}
