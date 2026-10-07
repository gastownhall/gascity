//go:build integration

package prwatchdog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
	"gopkg.in/yaml.v3"
)

// This fixture exercises real Git tree selection and local-action discovery,
// then the production Watch evaluator with its existing in-process API/clock
// seams. It does not execute the networked module-download action or replace
// the final PR's required hosted workflow bootstrap/observation evidence.
func TestWatchdogBootstrapObservesHeadFromCurrentTrustedBase(t *testing.T) {
	for _, tc := range []struct {
		name, branch, evidence string
		fresh, mac, formulas   bool
		wantPass               bool
	}{
		{name: "stale_main", branch: "main", evidence: "complete", wantPass: true},
		{name: "fresh_main", branch: "main", fresh: true, evidence: "complete", wantPass: true},
		{name: "stale_non_default_base", branch: "release/2026", evidence: "complete", wantPass: true},
		{name: "fresh_non_default_base", branch: "release/2026", fresh: true, evidence: "complete", wantPass: true},
		{name: "bootstrap_does_not_pass_missing_evidence", branch: "main", fresh: true, evidence: "missing"},
		{name: "bootstrap_does_not_pass_failed_evidence", branch: "main", fresh: true, evidence: "failed"},
		{name: "base_commit_evidence_does_not_satisfy_pr_head", branch: "main", fresh: true, evidence: "wrong_head"},
		{name: "both_labels_require_both_suites", branch: "main", fresh: true, mac: true, formulas: true, evidence: "complete", wantPass: true},
		{name: "mac_label_remains_required", branch: "main", fresh: true, mac: true, evidence: "core_only"},
		{name: "formulas_label_remains_required", branch: "main", fresh: true, formulas: true, evidence: "core_only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWatchdogBootstrapFixture(t)
			baseSHA := f.staleSHA
			if tc.fresh {
				baseSHA = f.tips[tc.branch]
			}
			ref, _ := watchdogCheckoutOptions(t)["ref"].(string)
			selected := strings.NewReplacer(
				"${{ github.event.pull_request.base.ref }}", tc.branch,
				"${{ github.event.pull_request.base.sha }}", baseSHA,
				"${{ github.event.pull_request.head.sha }}", f.headSHA,
				"${{ github.event.pull_request.head.ref }}", "untrusted-head",
			).Replace(ref)
			f.git(t, "checkout", "--detach", selected)
			gotSHA := f.git(t, "rev-parse", "HEAD")

			// Resolve the workflow's actual required local action before
			// observation, reproducing the runner's bootstrap failure at an
			// old base tree that has the program but no action metadata.
			if err := f.loadLocalActions(t); err != nil {
				t.Fatalf("bootstrap failed before observation: checkout=%s event.base.sha=%s current base=%s: %v", gotSHA, baseSHA, f.tips[tc.branch], err)
			}
			if gotSHA != f.tips[tc.branch] {
				t.Fatalf("loaded %s, want current trusted %s tip %s", gotSHA, tc.branch, f.tips[tc.branch])
			}
			marker, err := os.ReadFile(filepath.Join(f.root, "trusted-base.txt"))
			if err != nil || string(marker) != tc.branch {
				t.Fatalf("loaded wrong base branch: marker=%q err=%v", marker, err)
			}
			program, err := os.ReadFile(filepath.Join(f.root, "scripts/prwatchdog/cmd/watchdog/main.go"))
			if err != nil || string(program) != f.trustedProgram {
				t.Fatalf("observation must load the trusted watchdog, not PR-head program: err=%v", err)
			}

			env := watchdogObservationEnv(t)
			if env["PR_HEAD_SHA"] != "${{ github.event.pull_request.head.sha }}" {
				t.Fatalf("observation no longer uses the explicit event PR head: %v", env["PR_HEAD_SHA"])
			}
			if env["NEEDS_MAC_LABEL"] != "${{ contains(github.event.pull_request.labels.*.name, 'needs-mac') }}" ||
				env["NEEDS_REVIEW_FORMULAS_LABEL"] != "${{ contains(github.event.pull_request.labels.*.name, 'needs-review-formulas') }}" {
				t.Fatalf("observation changed opt-in label selection: %v", env)
			}
			clock := &fakeClock{now: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)}
			runs := []CheckRun{
				{Name: CheckName, HeadSHA: f.headSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess},
				{Name: CIRequiredName, HeadSHA: f.headSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess},
			}
			switch tc.evidence {
			case "complete":
				if tc.mac {
					runs = append(runs, CheckRun{Name: MacCheckName, HeadSHA: f.headSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess})
				}
				if tc.formulas {
					runs = append(runs, CheckRun{Name: ReviewFormulasCheckName, HeadSHA: f.headSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess})
				}
			case "missing":
				runs = nil
			case "failed":
				runs[1].Conclusion = ConclusionFailure
			case "wrong_head":
				for i := range runs {
					runs[i].HeadSHA = f.tips[tc.branch]
				}
			}
			fetcher := &scriptedFetcher{responses: []fetchResponse{{runs: runs}}}
			eval := Watch(context.Background(), fetcher, clock, &fakeSleeper{clock: clock}, PollOptions{
				HeadSHA: f.headSHA, NeedsMacLabel: tc.mac, NeedsReviewFormulasLabel: tc.formulas,
				Deadline: ObservationDeadline, Interval: ObservationDeadline,
			})
			if len(fetcher.calls) == 0 {
				t.Fatal("bootstrap never reached observation")
			}
			for _, head := range fetcher.calls {
				if head != f.headSHA {
					t.Fatalf("observed %s, want explicit PR-head commit %s", head, f.headSHA)
				}
			}
			if !eval.Terminal || eval.Pass != tc.wantPass {
				t.Fatalf("bootstrap success changed evidence verdict: got %+v, want pass=%v", eval, tc.wantPass)
			}
		})
	}
}

type watchdogBootstrapFixture struct {
	root, staleSHA, headSHA, trustedProgram string
	tips                                    map[string]string
}

func newWatchdogBootstrapFixture(t *testing.T) watchdogBootstrapFixture {
	t.Helper()
	f := watchdogBootstrapFixture{root: t.TempDir(), tips: make(map[string]string)}
	f.git(t, "init", "--initial-branch=main")
	f.git(t, "config", "user.name", "Watchdog fixture")
	f.git(t, "config", "user.email", "watchdog@example.invalid")
	program, err := os.ReadFile("cmd/watchdog/main.go")
	if err != nil {
		t.Fatal(err)
	}
	f.trustedProgram = string(program)
	f.write(t, "scripts/prwatchdog/cmd/watchdog/main.go", f.trustedProgram)
	f.write(t, "go.mod", "module example.invalid/trusted-base\n")
	f.commit(t, "recorded PR base before local action")
	f.staleSHA = f.git(t, "rev-parse", "HEAD")
	action, err := os.ReadFile(filepath.Join("..", "..", ".github", "actions", "go-mod-download", "action.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{"main", "release/2026"} {
		if branch != "main" {
			f.git(t, "checkout", "-b", branch, f.staleSHA)
		}
		f.write(t, ".github/actions/go-mod-download/action.yml", string(action))
		f.write(t, "trusted-base.txt", branch)
		f.commit(t, "trusted base adds required local action")
		f.tips[branch] = f.git(t, "rev-parse", "HEAD")
	}
	f.git(t, "checkout", "-b", "untrusted-head", f.tips["main"])
	f.write(t, "scripts/prwatchdog/cmd/watchdog/main.go", "package main // UNTRUSTED PR-HEAD PROGRAM\n")
	f.write(t, "trusted-base.txt", "UNTRUSTED")
	f.commit(t, "untrusted contributor program")
	f.headSHA = f.git(t, "rev-parse", "HEAD")
	return f
}

func (f watchdogBootstrapFixture) git(t *testing.T, args ...string) string {
	t.Helper()
	// Test-owned repositories only; no ambient commit hook or signing program.
	return testutil.RunGit(t, f.root, append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false"}, args...)...)
}

func (f watchdogBootstrapFixture) write(t *testing.T, path, body string) {
	t.Helper()
	full := filepath.Join(f.root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f watchdogBootstrapFixture) commit(t *testing.T, message string) {
	t.Helper()
	f.git(t, "add", ".")
	f.git(t, "commit", "-m", message)
}

func (f watchdogBootstrapFixture) loadLocalActions(t *testing.T) error {
	t.Helper()
	doc := loadWatchdogWorkflow(t)
	jobs, _ := doc["jobs"].(map[string]any)
	job, _ := jobs["evidence"].(map[string]any)
	steps, _ := job["steps"].([]any)
	found := false
	for _, raw := range steps {
		step, _ := raw.(map[string]any)
		uses, _ := step["uses"].(string)
		if !strings.HasPrefix(uses, "./") {
			continue
		}
		found = true
		body, err := os.ReadFile(filepath.Join(f.root, uses, "action.yml"))
		if err != nil {
			return fmt.Errorf("loading local action %s: %w", uses, err)
		}
		var action map[string]any
		if err := yaml.Unmarshal(body, &action); err != nil {
			return err
		}
		runs, _ := action["runs"].(map[string]any)
		if runs["using"] != "composite" {
			return fmt.Errorf("local action %s has no composite bootstrap", uses)
		}
	}
	if !found {
		return fmt.Errorf("workflow no longer loads the required local bootstrap action")
	}
	return nil
}

func watchdogObservationEnv(t *testing.T) map[string]any {
	t.Helper()
	doc := loadWatchdogWorkflow(t)
	jobs, _ := doc["jobs"].(map[string]any)
	job, _ := jobs["evidence"].(map[string]any)
	steps, _ := job["steps"].([]any)
	for _, raw := range steps {
		step, _ := raw.(map[string]any)
		if run, _ := step["run"].(string); strings.TrimSpace(run) == "go run ./scripts/prwatchdog/cmd/watchdog" {
			env, _ := step["env"].(map[string]any)
			return env
		}
	}
	t.Fatal("workflow no longer invokes trusted watchdog observation")
	return nil
}
