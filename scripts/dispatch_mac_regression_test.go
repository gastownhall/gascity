package scripts_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

// The tests above read the workflow's shape; none of them runs the dispatcher.
// dispatchMacDriver runs the dispatch step's Python body with urllib's urlopen
// replaced by a recorder. Its arguments are the body file, the file the recorded
// requests are written to, and the HTTP status the stubbed response carries (400
// and above raise HTTPError, as a rejected request does). The body's own exit is
// left to propagate, so the process succeeds exactly when the step would.
const dispatchMacDriver = `import json
import sys
import urllib.error
import urllib.request

body_path, calls_path, status = sys.argv[1], sys.argv[2], int(sys.argv[3])
calls = []


class Response:
    def __init__(self, status):
        self.status = status

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


def urlopen(request, timeout=None):
    calls.append({
        "url": request.full_url,
        "method": request.get_method(),
        "headers": {key.lower(): value for key, value in request.header_items()},
        "body": request.data.decode("utf-8"),
        "timeout": timeout,
    })
    if status >= 400:
        raise urllib.error.HTTPError(request.full_url, status, "stub", {}, None)
    return Response(status)


urllib.request.urlopen = urlopen
try:
    with open(body_path, encoding="utf-8") as body:
        exec(compile(body.read(), body_path, "exec"), {"__name__": "__main__"})
finally:
    with open(calls_path, "w", encoding="utf-8") as out:
        json.dump(calls, out)
`

type dispatchMacRequest struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Timeout float64           `json:"timeout"`
}

// dispatchMacPythonBody returns the Python that the dispatch step hands to python3.
func dispatchMacPythonBody(t *testing.T, run string) string {
	t.Helper()
	_, rest, ok := strings.Cut(run, "python3 - <<'PY'\n")
	if !ok {
		t.Fatalf("dispatch step does not run a quoted python3 heredoc:\n%s", run)
	}
	body, tail, ok := strings.Cut(rest, "\nPY")
	if !ok || strings.TrimSpace(tail) != "" {
		t.Fatalf("dispatch step's heredoc is not the end of its script:\n%s", run)
	}
	return body + "\n"
}

// runDispatchMacScript runs body in a directory whose .github/blacksmith-allowlist.txt
// holds allowlist, with env as the step's environment and the stubbed API answering
// status. It returns the requests the script sent, its output, and whether the
// process succeeded.
func runDispatchMacScript(t *testing.T, body, allowlist string, env map[string]string, status int) ([]dispatchMacRequest, string, bool) {
	t.Helper()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, ".github", "blacksmith-allowlist.txt"), allowlist)
	writeTestFile(t, filepath.Join(dir, "body.py"), body)
	writeTestFile(t, filepath.Join(dir, "driver.py"), dispatchMacDriver)
	callsPath := filepath.Join(dir, "calls.json")
	cmd := testCommand("python3", filepath.Join(dir, "driver.py"), filepath.Join(dir, "body.py"), callsPath, strconv.Itoa(status))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	output, runErr := cmd.CombinedOutput()
	// The driver records the requests even when the body exits or raises, so a
	// missing file means the driver itself did not run, not that the step failed.
	raw, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatalf("driver recorded no requests (%v); python3 run: %v\n%s", err, runErr, output)
	}
	var calls []dispatchMacRequest
	if err := json.Unmarshal(raw, &calls); err != nil {
		t.Fatalf("parse recorded requests %q: %v", raw, err)
	}
	return calls, string(output), runErr == nil
}

// The dispatch step is what keeps a fork PR's author from reaching the secrets
// that Mac Regression's workflow_dispatch run holds, so this test runs its
// script. A trusted author's non-draft PR is dispatched exactly once, at the
// base ref, with the exact fork head and the full suite; an author who is not
// trusted, or a draft, is never dispatched; a dispatch GitHub does not accept
// fails the step; and nothing the PR controls reaches the URL, a header, or the
// log.
func TestDispatchMacRegressionScriptDispatchesOnlyTrustedNonDraftAuthors(t *testing.T) {
	step, _ := dispatchMacStepAndJob(t, readDispatchMacRegression(t))
	body := dispatchMacPythonBody(t, step.Run)
	base := map[string]string{
		"GH_TOKEN":       "test-token",
		"REPOSITORY":     "example/repo",
		"BASE_REF":       "main",
		"PR_NUMBER":      "123",
		"PR_DRAFT":       "false",
		"PR_AUTHOR":      "fork-author",
		"PR_ASSOCIATION": "NONE",
		"PR_HEAD_REPO":   "fork-owner/repo",
		"PR_HEAD_REF":    "feature/x",
		"PR_HEAD_SHA":    strings.Repeat("a", 40),
	}
	if got, want := slices.Sorted(maps.Keys(step.Env)), slices.Sorted(maps.Keys(base)); !slices.Equal(got, want) {
		t.Fatalf("dispatch step env = %q, want the variables this test supplies, %q", got, want)
	}
	const allowlist = "# Logins allowed to run automatically.\n" +
		"# One login per line. Blank lines and # comments are ignored.\n" +
		"#\n" +
		"# Sponsored is a word in a comment, not a login.\n" +
		"Trusted-One\n" +
		"\n" +
		"trusted-two\n"
	hostileRef := "x\"; $(touch pwned) `id` \\ ${{ github.token }}\n::error::boom"

	const (
		dispatches = iota // one request GitHub accepts, and the step succeeds
		skips             // no request, and the step succeeds
		fails             // one request GitHub does not accept, and the step fails
	)
	assoc := func(association string) map[string]string {
		return map[string]string{"PR_ASSOCIATION": association}
	}
	author := func(login string) map[string]string {
		return map[string]string{"PR_AUTHOR": login}
	}
	cases := []struct {
		name   string
		env    map[string]string // overrides of base
		status int               // the API's answer; 0 is 204
		want   int
	}{
		{name: "owner", env: assoc("OWNER"), want: dispatches},
		{name: "member", env: assoc("MEMBER"), want: dispatches},
		{name: "collaborator", env: assoc("COLLABORATOR"), want: dispatches},
		{name: "allowlisted login in other case", env: author("TRUSTED-ONE"), want: dispatches},
		{name: "allowlisted login after a blank line", env: author("trusted-two"), want: dispatches},
		{name: "hostile head ref", env: map[string]string{"PR_ASSOCIATION": "OWNER", "PR_HEAD_REF": hostileRef}, want: dispatches},

		{name: "no association", env: assoc("NONE"), want: skips},
		{name: "contributor", env: assoc("CONTRIBUTOR"), want: skips},
		{name: "first time contributor", env: assoc("FIRST_TIME_CONTRIBUTOR"), want: skips},
		{name: "first timer", env: assoc("FIRST_TIMER"), want: skips},
		{name: "mannequin", env: assoc("MANNEQUIN"), want: skips},
		{name: "empty association", env: assoc(""), want: skips},
		{name: "unknown author", env: map[string]string{"PR_AUTHOR": "", "PR_ASSOCIATION": ""}, want: skips},
		{name: "login that extends an allowlisted one", env: author("trusted-one-evil"), want: skips},
		{name: "word from an allowlist comment", env: author("Sponsored"), want: skips},
		{name: "draft from an owner", env: map[string]string{"PR_ASSOCIATION": "OWNER", "PR_DRAFT": "true"}, want: skips},
		{name: "draft from an allowlisted login", env: map[string]string{"PR_AUTHOR": "trusted-one", "PR_DRAFT": "true"}, want: skips},

		{name: "dispatch rejected", env: assoc("OWNER"), status: 422, want: fails},
		{name: "unexpected success status", env: assoc("OWNER"), status: 202, want: fails},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := maps.Clone(base)
			maps.Copy(env, tc.env)
			status := tc.status
			if status == 0 {
				status = 204
			}
			calls, output, ok := runDispatchMacScript(t, body, allowlist, env, status)
			if want := tc.want != fails; ok != want {
				t.Errorf("step succeeded = %v, want %v\n%s", ok, want, output)
			}
			wantCalls := 1
			if tc.want == skips {
				wantCalls = 0
			}
			if len(calls) != wantCalls {
				t.Fatalf("script sent %d dispatch requests, want %d\n%s", len(calls), wantCalls, output)
			}
			if strings.Contains(output, env["PR_HEAD_REF"]) {
				t.Errorf("script printed the PR head ref %q:\n%s", env["PR_HEAD_REF"], output)
			}
			if len(calls) == 0 {
				return
			}
			call := calls[0]
			if want := "https://api.github.com/repos/example/repo/actions/workflows/mac-regression.yml/dispatches"; call.Method != "POST" || call.URL != want {
				t.Errorf("request = %s %s, want POST %s", call.Method, call.URL, want)
			}
			if got, want := call.Headers["authorization"], "Bearer test-token"; got != want {
				t.Errorf("Authorization header = %q, want %q", got, want)
			}
			if call.Timeout <= 0 {
				t.Errorf("request timeout = %v, want a positive timeout", call.Timeout)
			}
			if strings.Contains(call.URL, env["PR_HEAD_REF"]) {
				t.Errorf("request URL carries the PR head ref: %s", call.URL)
			}
			for name, value := range call.Headers {
				if strings.Contains(value, env["PR_HEAD_REF"]) {
					t.Errorf("request header %s carries the PR head ref: %q", name, value)
				}
			}
			var payload struct {
				Ref    string            `json:"ref"`
				Inputs map[string]string `json:"inputs"`
			}
			decoder := json.NewDecoder(strings.NewReader(call.Body))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&payload); err != nil {
				t.Fatalf("request body %q: %v", call.Body, err)
			}
			wantInputs := map[string]string{
				"suite":     "full",
				"pr_number": env["PR_NUMBER"],
				"head_repo": env["PR_HEAD_REPO"],
				"head_ref":  env["PR_HEAD_REF"],
				"head_sha":  env["PR_HEAD_SHA"],
			}
			if payload.Ref != env["BASE_REF"] || !maps.Equal(payload.Inputs, wantInputs) {
				t.Errorf("request body = ref %q inputs %q, want ref %q inputs %q", payload.Ref, payload.Inputs, env["BASE_REF"], wantInputs)
			}
		})
	}
}
