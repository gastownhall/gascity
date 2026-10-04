package scripts_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// A pull_request run checks out refs/pull/N/merge as GitHub computed it when
// the event fired, and reopens, approval-delayed first-time runs and re-runs
// reuse it, so a run can test a merge onto a main that predates fixes (plan
// F11g-2). Every bazel-test.yml job follows its blobless full-history
// checkout with .github/actions/fresh-merge, which merges the PR head onto
// the base branch's current tip.

const (
	freshMergeAction     = ".github/actions/fresh-merge/action.yml"
	freshMergeUses       = "./.github/actions/fresh-merge"
	freshMergeStepName   = "Merge the PR head onto the base branch's current tip"
	bazelEvidenceStep    = "bazel test //test/integration (evidence-only)"
	bazelEvidenceStepIf  = "always() && steps.fresh-merge.outcome != 'failure'"
	bazelTestConcurrency = "${{ github.workflow }}-${{ github.event_name == 'pull_request' && github.event.pull_request.number || github.run_id }}"
)

// TestBazelTestConcurrency: a PR's runs share a group and cancel each other;
// every other event gets a group of its own (the run id), so a push to main
// is never canceled, nor replaced while pending by the next push.
func TestBazelTestConcurrency(t *testing.T) {
	var wf struct {
		Concurrency map[string]string `yaml:"concurrency"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), bazelTestWorkflow)), &wf); err != nil {
		t.Fatalf("parse %s: %v", bazelTestWorkflow, err)
	}
	want := map[string]string{
		"group":              bazelTestConcurrency,
		"cancel-in-progress": "${{ github.event_name == 'pull_request' }}",
	}
	if !reflect.DeepEqual(wf.Concurrency, want) {
		t.Errorf("%s concurrency = %v, want %v", bazelTestWorkflow, wf.Concurrency, want)
	}
}

// TestBazelTestFreshMergeSteps pins every job's checkout (full blobless
// history) and the fresh-merge step right after it, and keeps the
// always() evidence step off a failed merge's half-merged tree.
func TestBazelTestFreshMergeSteps(t *testing.T) {
	var wf bazelTestWorkflowFile
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), bazelTestWorkflow)), &wf); err != nil {
		t.Fatalf("parse %s: %v", bazelTestWorkflow, err)
	}
	if len(wf.Jobs) != 2 {
		t.Errorf("%s has %d jobs, want bazel and sync-check (each needs the fresh merge)", bazelTestWorkflow, len(wf.Jobs))
	}
	for _, name := range []string{"bazel", "sync-check"} {
		job, ok := wf.Jobs[name]
		if !ok {
			t.Errorf("%s has no %s job", bazelTestWorkflow, name)
			continue
		}
		checkouts := 0
		for i, step := range job.Steps {
			if !strings.HasPrefix(step.Uses, "actions/checkout@") {
				continue
			}
			checkouts++
			want := map[string]string{"fetch-depth": "0", "filter": "blob:none"}
			if !reflect.DeepEqual(step.With, want) {
				t.Errorf("%s checkout with = %v, want %v", name, step.With, want)
			}
			if i+1 >= len(job.Steps) {
				t.Errorf("%s: checkout is the last step; want fresh-merge after it", name)
				continue
			}
			next := job.Steps[i+1]
			if next.Uses != freshMergeUses || next.ID != "fresh-merge" || next.Name != freshMergeStepName || next.If != "" || next.With != nil || next.Run != "" {
				t.Errorf("%s: step after checkout = %+v; want %q (id fresh-merge, no if, no inputs)", name, next, freshMergeUses)
			}
		}
		if checkouts != 1 {
			t.Errorf("%s has %d checkouts, want 1", name, checkouts)
		}
	}
	found := false
	for _, step := range wf.Jobs["bazel"].Steps {
		if step.Name == bazelEvidenceStep {
			found = true
			if step.If != bazelEvidenceStepIf {
				t.Errorf("%q if = %q, want %q", bazelEvidenceStep, step.If, bazelEvidenceStepIf)
			}
		}
	}
	if !found {
		t.Errorf("bazel job has no %q step", bazelEvidenceStep)
	}
}

type freshMergeActionFile struct {
	Inputs map[string]struct {
		Required bool   `yaml:"required"`
		Default  string `yaml:"default"`
	} `yaml:"inputs"`
	Runs struct {
		Using string `yaml:"using"`
		Steps []struct {
			If    string            `yaml:"if"`
			Shell string            `yaml:"shell"`
			Env   map[string]string `yaml:"env"`
			Run   string            `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"runs"`
}

// freshMergeActionRun returns the action's one step script after pinning
// what the behavior test cannot see: pull_request only, bash, env, and no
// required input (bazel-test.yml passes none; bazel.yml passes base-sha).
func freshMergeActionRun(t *testing.T) string {
	t.Helper()
	var action freshMergeActionFile
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), freshMergeAction)), &action); err != nil {
		t.Fatalf("parse %s: %v", freshMergeAction, err)
	}
	if action.Runs.Using != "composite" || len(action.Runs.Steps) != 1 {
		t.Fatalf("%s: using %q, %d steps; want composite, one step", freshMergeAction, action.Runs.Using, len(action.Runs.Steps))
	}
	for name, in := range action.Inputs {
		if in.Required || in.Default != "" {
			t.Errorf("%s input %s: required %v, default %q; want optional with an empty default (the base branch's tip)", freshMergeAction, name, in.Required, in.Default)
		}
	}
	if _, ok := action.Inputs["base-sha"]; !ok || len(action.Inputs) != 1 {
		t.Errorf("%s inputs = %v, want base-sha only", freshMergeAction, action.Inputs)
	}
	step := action.Runs.Steps[0]
	wantEnv := map[string]string{
		"BASE_REF": "${{ github.base_ref }}",
		"BASE_SHA": "${{ inputs.base-sha }}",
		"HEAD_SHA": "${{ github.event.pull_request.head.sha }}",
	}
	if step.If != "github.event_name == 'pull_request'" || step.Shell != "bash" || !reflect.DeepEqual(step.Env, wantEnv) {
		t.Errorf("%s step: if %q, shell %q, env %v; want pull_request only, bash, env %v", freshMergeAction, step.If, step.Shell, step.Env, wantEnv)
	}
	if strings.Contains(step.Run, "${{") {
		t.Fatalf("%s run interpolates an expression; pass it through env:\n%s", freshMergeAction, step.Run)
	}
	return step.Run
}

// TestFreshMergeActionBehaviour runs the action's script as Actions does
// (bash --noprofile --norc -eo pipefail) in a blobless clone of a scratch
// origin whose main moved after the event: a clean merge (deterministic, on
// the fetched tip, with the workflow-skew warning when main changed
// .github/), a conflict, a head that already contains the tip, a base
// branch that cannot be fetched (three attempts with backoff), and with
// base-sha: a merge onto that commit though main has moved past it, a
// malformed base-sha, and one that cannot be fetched.
func TestFreshMergeActionBehaviour(t *testing.T) {
	script := freshMergeActionRun(t)
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	stubs := filepath.Join(tmp, "stubs")
	for _, d := range []string{home, stubs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sleepLog := filepath.Join(tmp, "sleep.log")
	// The retry backoff is real sleep on a runner; here it is recorded.
	writeExecutable(t, filepath.Join(stubs, "sleep"), "#!/bin/sh\necho \"$*\" >> '"+sleepLog+"'\n")
	stepPath := filepath.Join(tmp, "step.sh")
	if err := os.WriteFile(stepPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	baseEnv := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		"LC_ALL=C",
	}
	// run runs a command in dir with the isolated git environment.
	run := func(dir string, env []string, name string, args ...string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Env = append(append([]string{}, baseEnv...), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	// Fixture commits get fixed identities and dates (tick restarts per
	// setup), so the same fixture gives the same commits in every origin.
	tick := 0
	git := func(dir string, args ...string) string {
		t.Helper()
		tick += 60
		date := "GIT_AUTHOR_DATE=" + time.Unix(int64(tick), 0).UTC().Format(time.RFC3339)
		out, err := run(dir, []string{
			"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@invalid", date,
			"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@invalid", "GIT_COMMITTER_" + strings.TrimPrefix(date, "GIT_AUTHOR_"),
		}, "git", args...)
		if err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
		return strings.TrimSpace(out)
	}
	write := func(dir, rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(dir, msg string, files map[string]string) string {
		t.Helper()
		for rel, content := range files {
			write(dir, rel, content)
		}
		git(dir, "add", "-A")
		git(dir, "commit", "--quiet", "-m", msg)
		return git(dir, "rev-parse", "HEAD")
	}

	type fixture struct {
		work, origin, head, event, tip, summary string
	}
	// setup: main at base0; the PR adds pr.txt (prFiles) onto base0; the
	// event merge ref merges the head onto base0; the clone happens; then
	// main moves on with mainFiles (after mergeTipIntoPR, the PR merges that
	// tip itself).
	setup := func(name string, prFiles, mainFiles map[string]string, mergeTipIntoPR bool) fixture {
		t.Helper()
		tick = 1700000000
		f := fixture{origin: filepath.Join(tmp, name, "origin"), work: filepath.Join(tmp, name, "work"), summary: filepath.Join(tmp, name, "summary.md")}
		if err := os.MkdirAll(f.origin, 0o755); err != nil {
			t.Fatal(err)
		}
		git(f.origin, "init", "--quiet", "-b", "main")
		git(f.origin, "config", "uploadpack.allowFilter", "true")
		base0 := commit(f.origin, "base", map[string]string{"a.txt": "a\n", "b.txt": "1\n", ".github/workflows/w.yml": "v1\n"})
		git(f.origin, "checkout", "--quiet", "-b", "pr")
		f.head = commit(f.origin, "pr", prFiles)
		git(f.origin, "checkout", "--quiet", "--detach", base0)
		git(f.origin, "merge", "--quiet", "--no-ff", "--no-edit", f.head)
		f.event = git(f.origin, "rev-parse", "HEAD")
		git(f.origin, "update-ref", "refs/pull/1/merge", f.event)
		git(f.origin, "checkout", "--quiet", "main")
		git(filepath.Dir(f.work), "clone", "--quiet", "--filter=blob:none", "--no-checkout", "file://"+f.origin, f.work)
		git(f.work, "fetch", "--quiet", "origin", "refs/pull/1/merge")
		git(f.work, "checkout", "--quiet", "--detach", f.event)
		f.tip = commit(f.origin, "main moves", mainFiles)
		if mergeTipIntoPR {
			git(f.origin, "checkout", "--quiet", "pr")
			git(f.origin, "merge", "--quiet", "--no-edit", "main")
			f.head = git(f.origin, "rev-parse", "HEAD")
			git(f.origin, "checkout", "--quiet", "main")
			git(f.work, "fetch", "--quiet", "origin", "pr")
		}
		return f
	}
	step := func(f fixture, baseRef, baseSHA string) (string, error) {
		t.Helper()
		if err := os.Remove(sleepLog); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return run(f.work, []string{
			"PATH=" + stubs + ":" + os.Getenv("PATH"),
			"BASE_REF=" + baseRef,
			"BASE_SHA=" + baseSHA,
			"HEAD_SHA=" + f.head,
			"GITHUB_SHA=" + f.event,
			"GITHUB_STEP_SUMMARY=" + f.summary,
		}, "bash", "--noprofile", "--norc", "-eo", "pipefail", stepPath)
	}

	t.Run("clean merge onto the fetched tip", func(t *testing.T) {
		prFiles := map[string]string{"pr.txt": "pr\n"}
		mainFiles := map[string]string{"b.txt": "2\n"}
		f := setup("clean", prFiles, mainFiles, false)
		out, err := step(f, "main", "")
		if err != nil {
			t.Fatalf("step failed: %v\n%s", err, out)
		}
		if got := git(f.work, "rev-parse", "HEAD^1", "HEAD^2"); got != f.head+"\n"+f.tip {
			t.Errorf("HEAD parents = %q, want head %s then tip %s", got, f.head, f.tip)
		}
		if got := git(f.work, "show", "HEAD:b.txt") + "|" + git(f.work, "show", "HEAD:pr.txt"); got != "2|pr" {
			t.Errorf("merged tree b.txt|pr.txt = %q, want main's 2 and the PR's pr", got)
		}
		if got, want := git(f.work, "log", "-1", "--format=%ad|%cd|%an|%cn", "--date=raw", "HEAD"), git(f.work, "log", "-1", "--format=%cd|%cd", "--date=raw", f.tip)+"|ci|ci"; got != want {
			t.Errorf("merge commit dates and identity = %q, want the tip's committer date and ci: %q", got, want)
		}
		if strings.Contains(out, "::warning") {
			t.Errorf("main did not touch .github/, yet a warning:\n%s", out)
		}
		summary := readFile(t, filepath.Dir(f.summary), filepath.Base(f.summary))
		if !strings.Contains(summary, f.head) || !strings.Contains(summary, f.tip) || !strings.Contains(summary, f.event) {
			t.Errorf("step summary lacks head, tip or event merge ref:\n%s", summary)
		}
		// Same head and base: the same merge commit, in another clone too.
		first := git(f.work, "rev-parse", "HEAD")
		again := setup("clean-again", prFiles, mainFiles, false)
		if again.head != f.head || again.tip != f.tip {
			t.Fatalf("fixture is not reproducible: head %s/%s, tip %s/%s", f.head, again.head, f.tip, again.tip)
		}
		if out, err := step(again, "main", ""); err != nil {
			t.Fatalf("second run failed: %v\n%s", err, out)
		}
		if second := git(again.work, "rev-parse", "HEAD"); second != first {
			t.Errorf("merge commit %s, then %s for the same head and base; want one commit", first, second)
		}
	})

	t.Run("workflow changed on main", func(t *testing.T) {
		f := setup("skew", map[string]string{"pr.txt": "pr\n"}, map[string]string{".github/workflows/w.yml": "v2\n"}, false)
		out, err := step(f, "main", "")
		if err != nil {
			t.Fatalf("step failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "::warning title=Workflow changed on main since this event::") || !strings.Contains(out, "A push to the PR refreshes it.") {
			t.Errorf("no workflow-skew warning:\n%s", out)
		}
	})

	t.Run("conflict", func(t *testing.T) {
		f := setup("conflict", map[string]string{"b.txt": "pr\n"}, map[string]string{"b.txt": "main\n"}, false)
		out, err := step(f, "main", "")
		if err == nil {
			t.Fatalf("step passed on a conflict:\n%s", out)
		}
		want := "::error title=PR does not merge onto main::the PR head " + f.head + " conflicts with main at " + f.tip + "; rebase or merge main"
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
		if _, err := os.Stat(f.summary); err == nil {
			t.Errorf("a failed merge wrote the step summary")
		}
	})

	t.Run("head already contains the tip", func(t *testing.T) {
		f := setup("merged", map[string]string{"pr.txt": "pr\n"}, map[string]string{"b.txt": "2\n"}, true)
		out, err := step(f, "main", "")
		if err != nil {
			t.Fatalf("step failed: %v\n%s", err, out)
		}
		if got := git(f.work, "rev-parse", "HEAD"); got != f.head {
			t.Errorf("HEAD = %s, want the PR head %s unchanged (it already contains %s)", got, f.head, f.tip)
		}
	})

	t.Run("base branch cannot be fetched", func(t *testing.T) {
		f := setup("nobase", map[string]string{"pr.txt": "pr\n"}, map[string]string{"b.txt": "2\n"}, false)
		out, err := step(f, "gone", "")
		if err == nil {
			t.Fatalf("step passed without a base branch:\n%s", out)
		}
		if !strings.Contains(out, "::error title=fresh-merge cannot fetch gone::git fetch of gone failed 3 times") {
			t.Errorf("output lacks the fetch error:\n%s", out)
		}
		slept, readErr := os.ReadFile(sleepLog)
		if readErr != nil || string(slept) != "5\n10\n" {
			t.Errorf("backoff sleeps = %q (%v), want 5 then 10", slept, readErr)
		}
		if got := git(f.work, "rev-parse", "HEAD"); got != f.event {
			t.Errorf("HEAD moved to %s; want the event merge ref %s untouched", got, f.event)
		}
	})

	t.Run("base-sha: onto that commit, not the moved tip", func(t *testing.T) {
		f := setup("basesha", map[string]string{"pr.txt": "pr\n"}, map[string]string{"b.txt": "2\n"}, false)
		// main moves again after the rbe job resolved f.tip
		later := commit(f.origin, "main moves again", map[string]string{"b.txt": "3\n"})
		out, err := step(f, "main", f.tip)
		if err != nil {
			t.Fatalf("step failed: %v\n%s", err, out)
		}
		if got := git(f.work, "rev-parse", "HEAD^1", "HEAD^2"); got != f.head+"\n"+f.tip {
			t.Errorf("HEAD parents = %q, want head %s then base-sha %s (not the later tip %s)", got, f.head, f.tip, later)
		}
		if got := git(f.work, "show", "HEAD:b.txt"); got != "2" {
			t.Errorf("merged b.txt = %q, want base-sha's 2", got)
		}
		summary := readFile(t, filepath.Dir(f.summary), filepath.Base(f.summary))
		if !strings.Contains(summary, f.tip) || strings.Contains(summary, later) {
			t.Errorf("step summary names the wrong base:\n%s", summary)
		}
	})

	t.Run("base-sha malformed", func(t *testing.T) {
		f := setup("badsha", map[string]string{"pr.txt": "pr\n"}, map[string]string{"b.txt": "2\n"}, false)
		out, err := step(f, "main", "main")
		if err == nil {
			t.Fatalf("step passed with base-sha main:\n%s", out)
		}
		if !strings.Contains(out, "::error title=fresh-merge base-sha::base-sha is 'main', not a 40-digit commit id") {
			t.Errorf("output lacks the base-sha error:\n%s", out)
		}
		if got := git(f.work, "rev-parse", "HEAD"); got != f.event {
			t.Errorf("HEAD moved to %s; want the event merge ref %s untouched", got, f.event)
		}
	})

	t.Run("base-sha cannot be fetched", func(t *testing.T) {
		f := setup("nosha", map[string]string{"pr.txt": "pr\n"}, map[string]string{"b.txt": "2\n"}, false)
		missing := strings.Repeat("ab", 20)
		out, err := step(f, "main", missing)
		if err == nil {
			t.Fatalf("step passed with an unknown base-sha:\n%s", out)
		}
		if want := "::error title=fresh-merge cannot fetch main commit " + missing + "::git fetch of main commit " + missing + " failed 3 times"; !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
		slept, readErr := os.ReadFile(sleepLog)
		if readErr != nil || string(slept) != "5\n10\n" {
			t.Errorf("backoff sleeps = %q (%v), want 5 then 10", slept, readErr)
		}
	})
}
