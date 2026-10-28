package scripts_test

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// dispatch-mac-regression.yml runs Mac Regression for fork pull requests. A
// fork PR's pull_request run gets no secrets, and a pull_request_target job
// may hold none but GITHUB_TOKEN (TestPullRequestTargetWorkflowsHoldNoSecrets),
// so the dispatcher only vets the author and dispatches mac-regression.yml,
// whose workflow_dispatch run holds the secrets, as
// dispatch-labeled-pr-suite.yml does. These tests pin that shape.

type dispatchMacWorkflow struct {
	On          map[string]yaml.Node      `yaml:"on"`
	Permissions map[string]string         `yaml:"permissions"`
	Jobs        map[string]dispatchMacJob `yaml:"jobs"`
}

type dispatchMacJob struct {
	If          string            `yaml:"if"`
	Permissions map[string]string `yaml:"permissions"`
	Uses        string            `yaml:"uses"`
	Secrets     any               `yaml:"secrets"`
	Environment any               `yaml:"environment"`
	Steps       []dispatchMacStep `yaml:"steps"`
}

type dispatchMacStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
	With map[string]any    `yaml:"with"`
}

func readDispatchMacRegression(t *testing.T) dispatchMacWorkflow {
	t.Helper()
	path := filepath.Join(repoRoot(t), ".github", "workflows", "dispatch-mac-regression.yml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf dispatchMacWorkflow
	if err := yaml.Unmarshal(body, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return wf
}

// dispatchMacStepAndJob returns the one step that POSTs the workflow dispatch, and the
// job it belongs to.
func dispatchMacStepAndJob(t *testing.T, wf dispatchMacWorkflow) (dispatchMacStep, dispatchMacJob) {
	t.Helper()
	var found []dispatchMacStep
	var job dispatchMacJob
	for _, j := range wf.Jobs {
		for _, s := range j.Steps {
			if strings.Contains(s.Run, "/dispatches") {
				found = append(found, s)
				job = j
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("dispatch-mac-regression.yml has %d steps that POST a workflow dispatch, want 1", len(found))
	}
	return found[0], job
}

func TestDispatchMacRegressionTriggersOnForkPullRequestEvents(t *testing.T) {
	wf := readDispatchMacRegression(t)
	node, ok := wf.On["pull_request_target"]
	if !ok || len(wf.On) != 1 {
		t.Fatalf("on: = %v, want pull_request_target only", slices.Collect(maps.Keys(wf.On)))
	}
	var trigger struct {
		Branches []string `yaml:"branches"`
		Types    []string `yaml:"types"`
	}
	if err := node.Decode(&trigger); err != nil {
		t.Fatal(err)
	}
	if want := []string{"main"}; !slices.Equal(trigger.Branches, want) {
		t.Errorf("branches = %q, want %q", trigger.Branches, want)
	}
	want := []string{"opened", "reopened", "synchronize", "ready_for_review"}
	if got := slices.Clone(trigger.Types); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		t.Errorf("types = %q, want %q", trigger.Types, want)
	}
}

// The dispatcher holds GITHUB_TOKEN and nothing else: no reusable-workflow
// call (which would inherit secrets), no secrets: block, and write access only
// to dispatch a workflow.
func TestDispatchMacRegressionHoldsOnlyAScopedGithubToken(t *testing.T) {
	wf := readDispatchMacRegression(t)
	for name, perms := range wf.Permissions {
		if perms == "write" {
			t.Errorf("workflow permissions grant %s: write; scope write access to the dispatch job", name)
		}
	}
	for name, job := range wf.Jobs {
		if job.Uses != "" {
			t.Errorf("job %s calls reusable workflow %q; a pull_request_target job must dispatch it instead so the secrets live in the dispatched run", name, job.Uses)
		}
		if job.Secrets != nil {
			t.Errorf("job %s passes secrets (%v); the dispatcher holds none but GITHUB_TOKEN", name, job.Secrets)
		}
		if job.Environment != nil {
			t.Errorf("job %s names an environment; the dispatcher holds no environment secrets", name)
		}
		for _, step := range job.Steps {
			for key, value := range step.Env {
				if strings.Contains(value, "secrets.") && !strings.Contains(value, "secrets.GITHUB_TOKEN") {
					t.Errorf("job %s step %q reads %s from %q", name, step.Name, key, value)
				}
			}
		}
	}
	_, job := dispatchMacStepAndJob(t, wf)
	want := map[string]string{"actions": "write", "contents": "read"}
	for name, level := range job.Permissions {
		if want[name] != level {
			t.Errorf("dispatch job permission %s: %s, want only actions: write and contents: read", name, level)
		}
	}
	for name, level := range want {
		if job.Permissions[name] != level {
			t.Errorf("dispatch job permission %s = %q, want %q", name, job.Permissions[name], level)
		}
	}
}

func TestDispatchMacRegressionChecksOutOnlyTheWorkflowCommit(t *testing.T) {
	wf := readDispatchMacRegression(t)
	checkouts := 0
	for name, job := range wf.Jobs {
		for _, step := range job.Steps {
			if !strings.HasPrefix(step.Uses, "actions/checkout@") {
				continue
			}
			checkouts++
			if got := step.With["ref"]; got != "${{ github.workflow_sha }}" {
				t.Errorf("job %s checkout ref = %v, want ${{ github.workflow_sha }} (the trusted allowlist comes from the workflow's own commit)", name, got)
			}
			if got := step.With["persist-credentials"]; got != false {
				t.Errorf("job %s checkout persist-credentials = %v, want false", name, got)
			}
			if _, ok := step.With["repository"]; ok {
				t.Errorf("job %s checkout sets repository; it must not fetch the fork", name)
			}
		}
	}
	if checkouts != 1 {
		t.Errorf("dispatcher has %d checkouts, want 1 (the allowlist read)", checkouts)
	}
}

// Only a fork PR needs the dispatcher; a same-repo PR already runs
// mac-regression.yml on its own pull_request event.
func TestDispatchMacRegressionOnlyHandlesNonDraftForkPullRequests(t *testing.T) {
	wf := readDispatchMacRegression(t)
	_, job := dispatchMacStepAndJob(t, wf)
	for _, want := range []string{
		"github.event.pull_request.head.repo.full_name != github.repository",
		"github.event.pull_request.draft == false",
	} {
		if !strings.Contains(job.If, want) {
			t.Errorf("dispatch job if: = %q, want it to contain %q", job.If, want)
		}
	}
}

func TestDispatchMacRegressionVetsTheAuthorBeforeDispatching(t *testing.T) {
	step, _ := dispatchMacStepAndJob(t, readDispatchMacRegression(t))
	script := step.Run

	trust := strings.Index(script, `association in {"OWNER", "MEMBER", "COLLABORATOR"}`)
	post := strings.Index(script, "urllib.request.urlopen")
	if trust < 0 || post < 0 || trust > post {
		t.Fatalf("the author_association check (at %d) must run before the dispatch POST (at %d) in the same step", trust, post)
	}
	for _, want := range []string{
		".github/blacksmith-allowlist.txt",
		"author.lower() in allowlist",
		"if not trusted:",
		"raise SystemExit(0)",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("dispatch script lacks %q", want)
		}
	}
	// Both refusals must leave before the POST: an untrusted author, and a
	// draft head.
	if !strings.Contains(script, "draft") {
		t.Error("dispatch script does not skip draft pull requests")
	}
	for name, env := range map[string]string{
		"PR_ASSOCIATION": "${{ github.event.pull_request.author_association }}",
		"PR_AUTHOR":      "${{ github.event.pull_request.user.login }}",
	} {
		if got := step.Env[name]; got != env {
			t.Errorf("dispatch step env %s = %q, want %q", name, got, env)
		}
	}
}

// The dispatched run must see the exact fork head, the full suite, and a ref
// that is the base branch: never a branch name standing in for the head.
func TestDispatchMacRegressionDispatchesTheExactForkHeadAndFullSuite(t *testing.T) {
	step, _ := dispatchMacStepAndJob(t, readDispatchMacRegression(t))
	script := step.Run

	for _, want := range []string{
		`"mac-regression.yml"`,
		`"suite": "full"`,
		`"pr_number": os.environ["PR_NUMBER"]`,
		`"head_repo": os.environ["PR_HEAD_REPO"]`,
		`"head_sha": os.environ["PR_HEAD_SHA"]`,
		`"head_ref": os.environ["PR_HEAD_REF"]`,
		`"ref": os.environ["BASE_REF"]`,
		"actions/workflows/",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("dispatch script lacks %q", want)
		}
	}
	for name, env := range map[string]string{
		"PR_NUMBER":    "${{ github.event.pull_request.number }}",
		"PR_HEAD_REPO": "${{ github.event.pull_request.head.repo.full_name }}",
		"PR_HEAD_SHA":  "${{ github.event.pull_request.head.sha }}",
		"PR_HEAD_REF":  "${{ github.event.pull_request.head.ref }}",
		"BASE_REF":     "${{ github.event.pull_request.base.ref }}",
		"GH_TOKEN":     "${{ github.token }}",
	} {
		if got := step.Env[name]; got != env {
			t.Errorf("dispatch step env %s = %q, want %q", name, got, env)
		}
	}
	if strings.Contains(script, "${{") {
		t.Errorf("dispatch script has an inline expression; pass it through env:\n%s", script)
	}
}

// What the dispatcher sends has to be what mac-regression.yml accepts, or
// the POST is rejected at run time with a 422.
func TestDispatchMacRegressionInputsAreAcceptedByMacRegression(t *testing.T) {
	path := filepath.Join(repoRoot(t), ".github", "workflows", "mac-regression.yml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		On struct {
			WorkflowDispatch struct {
				Inputs map[string]struct {
					Options []string `yaml:"options"`
				} `yaml:"inputs"`
			} `yaml:"workflow_dispatch"`
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal(body, &wf); err != nil {
		t.Fatal(err)
	}
	inputs := wf.On.WorkflowDispatch.Inputs
	for _, name := range []string{"suite", "pr_number", "head_repo", "head_sha", "head_ref"} {
		if _, ok := inputs[name]; !ok {
			t.Errorf("mac-regression.yml workflow_dispatch has no %s input", name)
		}
	}
	if !slices.Contains(inputs["suite"].Options, "full") {
		t.Errorf("mac-regression.yml suite options = %q, want full among them", inputs["suite"].Options)
	}
}
