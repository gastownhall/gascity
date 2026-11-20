package scripts_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Fresh on drift (rbe-reexecution-design.md S3c): bazel-nightly.yml
// re-executes every test only when the canary's host fingerprint moved or
// the ISO week did, and on every dispatch. The decision fails safe (fresh)
// whenever it cannot read the fingerprint, and stays in shadow mode (always
// fresh, logging the decision) until RBE_FRESH_ON_DRIFT is "on".

const bazelNightlyWorkflow = ".github/workflows/bazel-nightly.yml"

// TestBazelNightlyFreshOnDriftWiring: the decide job feeds bazel.yml's
// fresh-test-results, and a green fresh run leaves the marker cache entry.
func TestBazelNightlyFreshOnDriftWiring(t *testing.T) {
	wf := parseWorkflow(t, bazelNightlyWorkflow)
	checkPinnedActions(t, bazelNightlyWorkflow, wf)
	decide := wf.Jobs["decide"]
	if decide.Permissions["actions"] != "read" || decide.Permissions["contents"] != "read" || len(decide.Permissions) != 2 {
		t.Errorf("decide permissions %v, want actions: read and contents: read only", decide.Permissions)
	}
	if decide.Outputs["fresh"] != "${{ steps.d.outputs.fresh }}" || decide.Outputs["key"] != "${{ steps.d.outputs.key }}" {
		t.Errorf("decide outputs %v", decide.Outputs)
	}
	if len(decide.Steps) != 1 || decide.Steps[0].Env["SHADOW"] != "${{ vars.RBE_FRESH_ON_DRIFT != 'on' }}" {
		t.Errorf("decide must have one step with SHADOW from vars.RBE_FRESH_ON_DRIFT: %+v", decide.Steps)
	}

	var raw struct {
		Jobs map[string]struct {
			Needs any            `yaml:"needs"`
			If    string         `yaml:"if"`
			Uses  string         `yaml:"uses"`
			With  map[string]any `yaml:"with"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), bazelNightlyWorkflow)), &raw); err != nil {
		t.Fatal(err)
	}
	bazel := raw.Jobs["bazel"]
	//nolint:misspell // GitHub Actions spells it cancelled()
	if bazel.Uses != "./.github/workflows/bazel.yml" || bazel.Needs != "decide" || bazel.If != "${{ !cancelled() }}" {
		t.Errorf("bazel job uses %q needs %v if %q, want bazel.yml after decide, run unless the workflow is canceled", bazel.Uses, bazel.Needs, bazel.If)
	}
	// Fail open: a failed decide (empty output) is fresh, never a skipped nightly.
	if got := bazel.With["fresh-test-results"]; got != "${{ needs.decide.outputs.fresh != 'false' }}" {
		t.Errorf("bazel fresh-test-results %v, want fresh unless decide said false", got)
	}

	remember := wf.Jobs["remember"]
	if remember.If != "needs.decide.outputs.key != '' && needs.decide.outputs.fresh == 'true' && needs.bazel.result == 'success'" {
		t.Errorf("remember if %q: only a green fresh run with a key may be remembered", remember.If)
	}
	_, save := findStep(remember, uses("actions/cache/save"))
	if save == nil || save.With["key"] != "${{ needs.decide.outputs.key }}" {
		t.Errorf("remember: no actions/cache/save of the decide key: %+v", save)
	}
}

// TestRBEWorkerEnvCanaryRecordsTheHostFingerprint: every measurement, drift
// or not, leaves a host-fingerprint artifact for the nightly decision.
func TestRBEWorkerEnvCanaryRecordsTheHostFingerprint(t *testing.T) {
	wf := parseWorkflow(t, rbeWorkerEnvCanary)
	measure := wf.Jobs["measure"]
	mAt, m := findStep(measure, isMeasureStep)
	if m == nil || m.ID != "measure" {
		t.Fatalf("measure step needs id: measure: %+v", m)
	}
	const cond = "always() && steps.measure.outcome != 'skipped'"
	fAt, f := findStep(measure, runs(`host-fingerprint.txt`))
	if f == nil || f.If != cond || f.Env["RBE_WORKER_REVISION"] != "${{ steps.rbe-worker.outputs.sha }}" ||
		!strings.Contains(f.Run, `worker-env.raw.txt`) || !strings.Contains(f.Run, "uname -r") || !strings.Contains(f.Run, "${ImageVersion:-}") {
		t.Fatalf("no fingerprint step over the raw listing, the kernel and the rbe-worker revision: %+v", f)
	}
	uAt, u := findStep(measure, func(s ghStep) bool { return s.With["name"] == "host-fingerprint" })
	if u == nil || u.If != cond || !strings.HasPrefix(u.Uses, "actions/upload-artifact@") {
		t.Fatalf("no host-fingerprint upload: %+v", u)
	}
	if mAt >= fAt || fAt >= uAt {
		t.Errorf("order measure %d, fingerprint %d, upload %d", mAt, fAt, uAt)
	}
}

// TestBazelNightlyFreshDecision runs the decide step's script against a
// stub gh: shadow mode, a covered week, a new fingerprint, a dispatch, and
// every way the fingerprint can be unreadable.
func TestBazelNightlyFreshDecision(t *testing.T) {
	wf := parseWorkflow(t, bazelNightlyWorkflow)
	script := wf.Jobs["decide"].Steps[0].Run
	const fp = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// date -u +%G-W%V, as the script computes it.
	year, wk := time.Now().UTC().ISOWeek()
	week := fmt.Sprintf("%d-W%02d", year, wk)
	key := "fresh-" + fp + "-" + week

	cases := []struct {
		name, event, shadow, runID, fingerprint, cacheKeys string
		wantFresh, wantKey, wantLog                        string
	}{
		{"shadow, covered week", "schedule", "true", "41", fp, key, "true", key, "would-run-fresh=false shadow=true"},
		{"on, covered week", "schedule", "false", "41", fp, key, "false", key, "would-run-fresh=false shadow=false"},
		{"on, new fingerprint or week", "schedule", "false", "41", fp, "fresh-other-" + week, "true", key, "a new host fingerprint or a new week"},
		{"on, a prefix is not the key", "schedule", "false", "41", fp, key + "-x", "true", key, "would-run-fresh=true"},
		{"on, dispatch", "workflow_dispatch", "false", "41", fp, key, "true", "", "a dispatch is always fresh"},
		{"on, no canary run", "schedule", "false", "", fp, key, "true", "", "fail safe"},
		{"on, malformed fingerprint", "schedule", "false", "41", "not-a-fingerprint", key, "true", "", "fail safe"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			// gh run list | run download -D DIR | cache list, as the script calls them.
			stub := `#!/bin/bash
case "$1 $2" in
"run list") [ -n "$STUB_RUN" ] || exit 1; echo "$STUB_RUN" ;;
"run download") d=""; while [ $# -gt 0 ]; do [ "$1" = -D ] && d=$2; shift; done
  mkdir -p "$d" && printf '%s\n' "$STUB_FP" >"$d/host-fingerprint.txt" ;;
"cache list") echo "$*" >>"$STUB_LOG"; case " $* " in *" --ref refs/heads/main "*) printf '%s\n' $STUB_CACHE ;; *) printf '%s\n' $STUB_PR_CACHE ;; esac ;;
*) exit 2 ;;
esac
`
			if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			out, summary := filepath.Join(dir, "out"), filepath.Join(dir, "summary")
			cmd := exec.Command("bash", "-e", "-c", script)
			cmd.Env = []string{
				"PATH=" + bin + ":" + os.Getenv("PATH"), "RUNNER_TEMP=" + dir, "GITHUB_OUTPUT=" + out,
				"GITHUB_STEP_SUMMARY=" + summary, "GITHUB_REPOSITORY=gastownhall/gascity",
				"GITHUB_EVENT_NAME=" + c.event, "SHADOW=" + c.shadow, "GH_TOKEN=x",
				"STUB_RUN=" + c.runID, "STUB_FP=" + c.fingerprint, "STUB_CACHE=" + c.cacheKeys,
				// Without --ref the stub would also list a PR's entry for the key.
				"STUB_PR_CACHE=" + key, "STUB_LOG=" + filepath.Join(dir, "gh.log"),
			}
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("decide script failed: %v\n%s", err, b)
			}
			got := readFile(t, dir, "out")
			if !strings.Contains(got, "fresh="+c.wantFresh+"\n") || !strings.Contains(got, "key="+c.wantKey+"\n") {
				t.Errorf("outputs %q, want fresh=%s key=%s", got, c.wantFresh, c.wantKey)
			}
			if calls, err := os.ReadFile(filepath.Join(dir, "gh.log")); err == nil && !strings.Contains(string(calls), "--ref refs/heads/main") {
				t.Errorf("gh cache list without --ref refs/heads/main: %s", calls)
			}
			if log := readFile(t, dir, "summary"); !strings.Contains(log, c.wantLog) {
				t.Errorf("summary %q, want %q", log, c.wantLog)
			}
		})
	}
}
