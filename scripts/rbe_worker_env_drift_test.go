package scripts_test

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"gopkg.in/yaml.v3"
)

// rbe-west's oss and oss-fork schedulers match worker-env exactly, so a pin
// that no live worker advertises is a queue that never drains while the pool
// scaler keeps dispatching Blacksmith workers that cannot take it. These tests
// pin gascity's guards that make that loud and early: its client
// tools/rbe/worker-env-drift (pin, preflight) and the workflows that run the
// farm half (check, await, report, resolve) from the pinned
// gastownhall/rbe-worker, whose own tests cover that half:
//
//   - a worker whose host is not the pinned one still registers, advertising
//     what it measured: the pools are shared, and actions that send no
//     worker-env still run there, while those carrying the pin (gascity's
//     and beads') never match it;
//   - its run's measurement step may fail without stopping the worker, and a
//     job beside the worker opens or updates the pin's drift issue while the
//     worker serves (the farm caps the pools while that issue is open);
//   - while that issue is open, remote CI on that pin fails at once instead
//     of queueing;
//   - a scheduled canary measures the Blacksmith image against the pin;
//   - a change that moves the pin, or touches the worker host's definition,
//     is measured on the Blacksmith image inside the required bazel job, and
//     a moved pin skips the remote suite (no worker serves it before merge).
//   - a pull request run whose base commit predates a re-pin of the default
//     branch (a stale base, not a change that moves the pin) fails its remote
//     lane at the preflight with its cure (Re-run all jobs) instead of
//     skipping bazel and reporting success.

const (
	rbeWorkerEnvDrift     = "tools/rbe/worker-env-drift"
	rbeWorkerEnvCanary    = ".github/workflows/rbe-worker-env-canary.yml"
	rbeWorkerEnvDriftDir  = "${{ runner.temp }}/worker-env-drift"
	rbeWorkerEnvDriftName = "worker-env-drift"
	rbeWorkerEnvLabel     = "rbe-worker-env-drift"
	rbeWorkerEnvReportGrp = "rbe-worker-env-drift-report"
	ghPinnedActionRE      = `^[a-z0-9-]+/[a-z0-9-]+(/[a-z0-9-]+)?@[0-9a-f]{40}$`
)

// ghStub is a gh for worker-env-drift: issue list answers with
// $GH_ISSUES (a JSON array of {title,url}) through the --jq it is given.
// Every call is logged to $GH_LOG, one per line. GH_FAIL=1 fails every call.
// Like gh, it exits with its writer's status: a writer killed by SIGPIPE
// fails the call.
const ghStub = `#!/bin/sh
printf '%s\n' "$*" >>"$GH_LOG"
[ "${GH_FAIL:-}" != 1 ] || { echo "gh: HTTP 502" >&2; exit 1; }
jq=
prev=
for a; do
	[ "$prev" = --jq ] && jq=$a
	prev=$a
done
case "$1 $2" in
"issue list") jq -r "$jq" "$GH_ISSUES" ;;
esac
`

type driftEnv struct {
	t       *testing.T
	root    string // the repo: the script under test
	dir     string // cwd: platforms/BUILD.bazel and tools/rbe/worker-env.txt
	pin     string
	ghLog   string
	issues  string
	summary string
	output  string
	extra   []string
}

func sha256Pin(b string) string {
	sum := sha256.Sum256([]byte(b))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func buildWithPin(pin string) string {
	return "platform(\n    name = \"rbe_worker\",\n    exec_properties = {\n        \"worker-env\": \"" + pin + "\",\n    },\n)\n"
}

func writeDriftFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newDriftEnv lays out a checkout whose pin is sha256(manifest), a stub gh
// and the GitHub Actions files.
func newDriftEnv(t *testing.T, manifest string) *driftEnv {
	t.Helper()
	e := &driftEnv{t: t, root: repoRoot(t), dir: t.TempDir(), pin: sha256Pin(manifest)}
	writeDriftFile(t, filepath.Join(e.dir, "tools/rbe/worker-env.txt"), manifest)
	writeDriftFile(t, filepath.Join(e.dir, "platforms/BUILD.bazel"), buildWithPin(e.pin))
	bin := filepath.Join(e.dir, "bin")
	writeDriftFile(t, filepath.Join(bin, "gh"), ghStub)
	if err := os.Chmod(filepath.Join(bin, "gh"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.ghLog = filepath.Join(e.dir, "gh.log")
	e.issues = filepath.Join(e.dir, "issues.json")
	e.summary = filepath.Join(e.dir, "summary.md")
	e.output = filepath.Join(e.dir, "output")
	e.setIssues()
	return e
}

// setIssues sets the open drift issues: title, url pairs.
func (e *driftEnv) setIssues(titleURL ...string) {
	var items []string
	for i := 0; i+1 < len(titleURL); i += 2 {
		items = append(items, `{"title":"`+titleURL[i]+`","url":"`+titleURL[i+1]+`"}`)
	}
	writeDriftFile(e.t, e.issues, "["+strings.Join(items, ",")+"]\n")
}

func (e *driftEnv) run(args ...string) (string, error) {
	e.t.Helper()
	env := append([]string{
		"PATH=" + filepath.Join(e.dir, "bin") + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin",
		"GITHUB_REPOSITORY=acme/repo", "GITHUB_RUN_ID=42", "RUNNER_NAME=runner-1", "RUNNER_TEMP=" + e.dir,
		"GITHUB_STEP_SUMMARY=" + e.summary, "GITHUB_OUTPUT=" + e.output,
		"GH_LOG=" + e.ghLog, "GH_ISSUES=" + e.issues,
	}, e.extra...)
	out, stderr, err := runRBEScript(e.dir, env, filepath.Join(e.root, rbeWorkerEnvDrift), args...)
	return out + stderr, err
}

func (e *driftEnv) read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		e.t.Fatal(err)
	}
	return string(b)
}

// ontoNone is a preflight case's third file with no pin in it.
const ontoNone = "<none>"

func driftTitle(pin string) string { return "rbe worker-env drift: " + pin }

// TestRBEWorkerEnvDriftPin: pin reads //platforms:rbe_worker's worker-env,
// the same value the platform test checks, "none" without one, and refuses
// two.
func TestRBEWorkerEnvDriftPin(t *testing.T) {
	root := repoRoot(t)
	out, stderr, err := runRBEScript(root, []string{"PATH=/usr/bin:/bin"}, filepath.Join(root, rbeWorkerEnvDrift), "pin")
	if err != nil {
		t.Fatalf("pin: %v\n%s", err, stderr)
	}
	want := rbeWorkerPlatformExecProperties(t, readFile(t, root, rbeWorkerPlatformBuild))[rbeWorkerEnvProperty]
	if strings.TrimSpace(out) != want {
		t.Errorf("pin = %q, want %q", out, want)
	}

	e := newDriftEnv(t, "a\n")
	none := filepath.Join(e.dir, "none.bazel")
	writeDriftFile(t, none, "platform(name = \"rbe_worker\")\n")
	if out, err := e.run("pin", none); err != nil || strings.TrimSpace(out) != "none" {
		t.Errorf("pin of a BUILD without worker-env: %q, %v; want none", out, err)
	}
	two := filepath.Join(e.dir, "two.bazel")
	writeDriftFile(t, two, buildWithPin(e.pin)+buildWithPin(sha256Pin("b\n")))
	if out, err := e.run("pin", two); err == nil {
		t.Errorf("pin of a BUILD with two pins succeeded: %q", out)
	}
}

// TestRBEWorkerEnvDriftPreflight: bazel.yml learns whether the change moves
// the pin off the default branch's (no worker serves it before merge),
// whether to measure the host, and fails a remote run on a pin with an open
// drift issue instead of queueing it.
func TestRBEWorkerEnvDriftPreflight(t *testing.T) {
	e := newDriftEnv(t, "a\n")
	base := filepath.Join(e.dir, "base.bazel")
	onto := filepath.Join(e.dir, "onto.bazel")
	changed := filepath.Join(e.dir, "changed")
	outputs := func() map[string]string {
		m := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(e.read(e.output)), "\n") {
			k, v, _ := strings.Cut(line, "=")
			m[k] = v
		}
		_ = os.Remove(e.output)
		return m
	}
	pinB, pinC := sha256Pin("b\n"), sha256Pin("c\n")
	for _, tc := range []struct {
		name, basePin, changed, remote string
		issue, fail                    bool
		moved, measure                 string
		// pin is the checkout's pin (e.pin if empty). onto is the pin of the
		// third argument, platforms/BUILD.bazel at the commit the checkout
		// was merged onto: empty is "not passed", ontoNone a file with no pin.
		pin, onto string
		// errOut is the failed preflight's annotation (the drift issue's if
		// empty); has and lacks are further substrings of the output.
		errOut     string
		has, lacks []string
	}{
		{name: "same pin", basePin: e.pin, changed: "cmd/gc/main.go\n", moved: "false", measure: "false"},
		{name: "rbe-worker pin moved", basePin: e.pin, changed: "README.md\n.github/actions/rbe-worker/pin\n", moved: "false", measure: "true"},
		{name: "rbe-worker fetch changed", basePin: e.pin, changed: ".github/actions/rbe-worker/fetch.sh\n", moved: "false", measure: "true"},
		{name: "rbe-worker H5 check changed", basePin: e.pin, changed: ".github/actions/rbe-worker/h5.sh\n", moved: "false", measure: "false"},
		{name: "a gascity copy of a farm script", basePin: e.pin, changed: "tools/rbe/blacksmith-worker.sh\n", moved: "false", measure: "false"},
		{name: "similar path", basePin: e.pin, changed: "tools/rbe/worker-env.txt.orig\n", moved: "false", measure: "false"},
		{name: "pin moved", basePin: sha256Pin("b\n"), changed: "tools/rbe/worker-env.txt\n", moved: "true", measure: "true"},
		{name: "default branch has no pin", basePin: "", changed: "platforms/BUILD.bazel\n", moved: "true", measure: "true"},
		{name: "moved pin ignores its issue", basePin: sha256Pin("b\n"), changed: "x\n", issue: true, remote: "true", moved: "true", measure: "true"},
		{name: "drift issue, local run", basePin: e.pin, changed: "x\n", issue: true, moved: "false", measure: "false"},
		{name: "drift issue, remote run", basePin: e.pin, changed: "x\n", issue: true, remote: "true", fail: true, moved: "false", measure: "false"},
		// Main re-pinned (tip b) after this run chose its base (onto a): the
		// checkout (pin a) is a stale base, not a change that moves the pin.
		{
			name: "stale base, remote run", basePin: pinB, onto: e.pin, changed: "x\n", remote: "true", fail: true, moved: "false", measure: "false",
			errOut: "::error title=rbe worker-env stale base::", has: []string{"Re-run all jobs", e.pin, pinB}, lacks: []string{"worker-env drift", "pin moved"},
		},
		{
			name: "stale base, local run", basePin: pinB, onto: e.pin, changed: "x\n", moved: "false", measure: "false",
			has: []string{"::notice title=rbe worker-env stale base::"}, lacks: []string{"pin moved", "::error"},
		},
		{
			name: "stale base outranks its drift issue", basePin: pinB, onto: e.pin, changed: "x\n", issue: true, remote: "true", fail: true, moved: "false", measure: "false",
			errOut: "::error title=rbe worker-env stale base::", lacks: []string{"worker-env drift"},
		},
		{name: "stale base, host definition changed", basePin: pinB, onto: e.pin, changed: "tools/rbe/worker-env.txt.orig\n.github/actions/rbe-worker/pin\n", moved: "false", measure: "true"},
		{
			name: "change moves the pin while main moved", pin: pinC, basePin: pinB, onto: e.pin, changed: "platforms/BUILD.bazel\n", remote: "true", moved: "true", measure: "true",
			has: []string{"::warning title=rbe worker-env pin moved::"}, lacks: []string{"stale base"},
		},
		{
			name: "change re-pins to main's current value", pin: pinB, basePin: pinB, onto: e.pin, changed: "platforms/BUILD.bazel\n", remote: "true", moved: "false", measure: "true",
			lacks: []string{"stale base", "pin moved"},
		},
		{
			name: "merged-onto commit has no pin", basePin: pinB, onto: ontoNone, changed: "platforms/BUILD.bazel\n", moved: "true", measure: "true",
			has: []string{"::warning title=rbe worker-env pin moved::"},
		},
		{
			name: "same pin, third file the same", basePin: e.pin, onto: e.pin, changed: "cmd/gc/main.go\n", remote: "true", moved: "false", measure: "false",
			lacks: []string{"stale base", "pin moved", "::error"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.basePin == "" {
				writeDriftFile(t, base, "")
			} else {
				writeDriftFile(t, base, buildWithPin(tc.basePin))
			}
			pin := cmp.Or(tc.pin, e.pin)
			writeDriftFile(t, filepath.Join(e.dir, "platforms/BUILD.bazel"), buildWithPin(pin))
			args := []string{"preflight", base, changed}
			switch tc.onto {
			case "":
			case ontoNone:
				writeDriftFile(t, onto, "")
				args = append(args, onto)
			default:
				writeDriftFile(t, onto, buildWithPin(tc.onto))
				args = append(args, onto)
			}
			writeDriftFile(t, changed, tc.changed)
			e.setIssues()
			wantIssue := ""
			if tc.issue {
				wantIssue = "https://x/issues/7"
				e.setIssues(driftTitle(pin), wantIssue)
			}
			e.extra = []string{"WORKER_ENV_REMOTE=" + tc.remote}
			out, err := e.run(args...)
			if (err != nil) != tc.fail {
				t.Fatalf("preflight error %v, want failure %v\n%s", err, tc.fail, out)
			}
			if tc.fail {
				errOut := cmp.Or(tc.errOut, "::error title=rbe worker-env drift::"+wantIssue)
				if !strings.Contains(out, errOut) {
					t.Errorf("failed preflight does not print %s:\n%s", errOut, out)
				}
			}
			for _, s := range tc.has {
				if !strings.Contains(out, s) {
					t.Errorf("preflight output lacks %q:\n%s", s, out)
				}
			}
			for _, s := range tc.lacks {
				if strings.Contains(out, s) {
					t.Errorf("preflight output has %q:\n%s", s, out)
				}
			}
			got := outputs()
			want := map[string]string{"pin": pin, "pin-moved": tc.moved, "measure": tc.measure, "drift-issue": wantIssue}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("output %s = %q, want %q (outputs %v)", k, got[k], v, got)
				}
			}
		})
	}
}

// pipeOverflow is more than any pipe buffer holds (64 KiB on Linux, 16-64
// KiB on macOS): a writer of this much into a reader that stops early is
// killed by SIGPIPE every time, never only when the race goes that way.
const pipeOverflow = 1 << 20

// defaultSIGPIPE gives the scripts this test runs the default SIGPIPE, as
// on GitHub runners, even when the test inherited it ignored (some agent
// sandboxes do): a signal ignored at exec stays ignored, and an ignored
// SIGPIPE turns the kill into an EPIPE that some writers (jq) shrug off.
// A handled signal is reset to its default at exec.
func defaultSIGPIPE(t *testing.T) {
	t.Helper()
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	t.Cleanup(func() { signal.Reset(syscall.SIGPIPE) })
}

// TestRBEWorkerEnvDriftSummaryDrainsStdin: summary always reads its input,
// with or without a step summary file. A summary that returned unread let
// `echo ... | summary` die of SIGPIPE under pipefail, so preflight exited 141
// instead of 1, now and then (the hook passes /dev/null to keep it away).
func TestRBEWorkerEnvDriftSummaryDrainsStdin(t *testing.T) {
	defaultSIGPIPE(t)
	root := repoRoot(t)
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{name: "unset"},
		{name: "empty", env: []string{"GITHUB_STEP_SUMMARY="}},
		{name: "file", env: []string{"GITHUB_STEP_SUMMARY=" + filepath.Join(t.TempDir(), "summary.md")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := `. "$1" && head -c ` + strconv.Itoa(pipeOverflow) + ` /dev/zero | summary`
			// bash -c SCRIPT $0 $1: SCRIPT sources $1, the script under test.
			stdout, stderr, err := runRBEScript(t.TempDir(), append([]string{"PATH=/usr/bin:/bin"}, tc.env...),
				"-c", script, "bash", filepath.Join(root, rbeWorkerEnvDrift))
			if err != nil {
				t.Fatalf("writer | summary: exit %d (%v), want 0 (141 is SIGPIPE: summary left its input unread)\n%s%s", exitCode(err), err, stdout, stderr)
			}
			for _, kv := range tc.env {
				if path, ok := strings.CutPrefix(kv, "GITHUB_STEP_SUMMARY="); ok && path != "" {
					if fi, err := os.Stat(path); err != nil || fi.Size() != pipeOverflow {
						t.Errorf("step summary %s: %v, want %d bytes", path, err, pipeOverflow)
					}
				}
			}
		})
	}
}

// TestRBEWorkerEnvDriftReadsWholePipes: no pipe in worker-env-drift ends in a
// reader that stops early (grep -q, head -n 1). Under pipefail the writer's
// SIGPIPE fails the pipeline, so a found match reads as no match: preflight
// let a remote run on a drifted pin queue. (rbe-worker's farm half keeps the
// same test for report and await.)
func TestRBEWorkerEnvDriftReadsWholePipes(t *testing.T) {
	defaultSIGPIPE(t)
	e := newDriftEnv(t, "a\n")

	// A pin with many open issues of its title: their list overflows the pipe.
	var issues []string
	for i := 0; i < 10000; i++ {
		issues = append(issues, driftTitle(e.pin), "https://x/issues/"+strconv.Itoa(7+i))
	}
	e.setIssues(issues...)
	base := filepath.Join(e.dir, "base.bazel")
	changed := filepath.Join(e.dir, "changed")
	writeDriftFile(t, base, buildWithPin(e.pin))
	writeDriftFile(t, changed, "x\n")
	e.extra = []string{"WORKER_ENV_REMOTE=true"}
	out, err := e.run("preflight", base, changed)
	if code := exitCode(err); code != 1 || !strings.Contains(out, "::error title=rbe worker-env drift::https://x/issues/7:") {
		t.Errorf("preflight on a pin with many drift issues: exit %d, want 1 naming the first issue\n%s", code, out)
	}
}

func exitCode(err error) int {
	var ee interface{ ExitCode() int }
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

type ghWorkflow struct {
	On          map[string]any    `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]ghJob  `yaml:"jobs"`
}

type ghJob struct {
	Name        string            `yaml:"name"`
	If          string            `yaml:"if"`
	Needs       any               `yaml:"needs"`
	RunsOn      string            `yaml:"runs-on"`
	Permissions map[string]string `yaml:"permissions"`
	Concurrency map[string]any    `yaml:"concurrency"`
	Outputs     map[string]string `yaml:"outputs"`
	Steps       []ghStep          `yaml:"steps"`
}

type ghStep struct {
	ID              string            `yaml:"id"`
	Name            string            `yaml:"name"`
	If              string            `yaml:"if"`
	Uses            string            `yaml:"uses"`
	With            map[string]any    `yaml:"with"`
	Env             map[string]string `yaml:"env"`
	Run             string            `yaml:"run"`
	ContinueOnError string            `yaml:"continue-on-error"` // a string: may be an expression
}

func parseWorkflow(t *testing.T, path string) ghWorkflow {
	t.Helper()
	var wf ghWorkflow
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), path)), &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return wf
}

// checkPinnedActions: every action is pinned to a commit SHA.
func checkPinnedActions(t *testing.T, path string, wf ghWorkflow) {
	t.Helper()
	re := regexp.MustCompile(ghPinnedActionRE)
	for name, job := range wf.Jobs {
		for _, s := range job.Steps {
			// A local composite action (./.github/actions/...) is this same
			// commit: nothing to pin, as ci.yml and bazel.yml's local actions.
			if s.Uses != "" && !strings.HasPrefix(s.Uses, "./") && !re.MatchString(s.Uses) {
				t.Errorf("%s job %s uses %q, not pinned to a commit SHA", path, name, s.Uses)
			}
		}
	}
}

func findStep(job ghJob, pred func(ghStep) bool) (int, *ghStep) {
	for i := range job.Steps {
		if pred(job.Steps[i]) {
			return i, &job.Steps[i]
		}
	}
	return -1, nil
}

func runs(cmd string) func(ghStep) bool {
	return func(s ghStep) bool { return strings.Contains(s.Run, cmd) }
}

func uses(action string) func(ghStep) bool {
	return func(s ghStep) bool { return strings.HasPrefix(s.Uses, action+"@") }
}

// checkDriftReportJob: a job that turns a failed measurement's drift
// directory into the pin's drift issue: GitHub-hosted, the only job with
// issues: write, one at a time across workflows (no duplicate issues).
func checkDriftReportJob(t *testing.T, path string, job ghJob, needs string) {
	t.Helper()
	if job.RunsOn != "ubuntu-latest" {
		t.Errorf("%s report job runs-on %q, want ubuntu-latest (no Blacksmith minutes)", path, job.RunsOn)
	}
	if len(job.Permissions) != 2 || job.Permissions["contents"] != "read" || job.Permissions["issues"] != "write" {
		t.Errorf("%s report job permissions %v, want contents: read, issues: write", path, job.Permissions)
	}
	if n, _ := job.Needs.(string); n != needs {
		t.Errorf("%s report job needs %v, want %s", path, job.Needs, needs)
	}
	if job.Concurrency["group"] != rbeWorkerEnvReportGrp || job.Concurrency["cancel-in-progress"] != false {
		t.Errorf("%s report job concurrency %v, want group %s without cancel-in-progress", path, job.Concurrency, rbeWorkerEnvReportGrp)
	}
	_, dl := findStep(job, uses("actions/download-artifact"))
	if dl == nil || dl.With["name"] != rbeWorkerEnvDriftName || dl.ContinueOnError != "true" {
		t.Errorf("%s report job: download of %s missing or not continue-on-error: %+v", path, rbeWorkerEnvDriftName, dl)
	}
	_, rep := findStep(job, runs(`worker-env-drift" report `))
	if rep == nil || rep.Env["GH_TOKEN"] != "${{ github.token }}" {
		t.Fatalf("%s report job: no worker-env-drift report step with GH_TOKEN: %+v", path, rep)
	}
	// The farm half of worker-env-drift is the pinned rbe-worker's: the
	// fetch step sits before the report command, which runs it from
	// $RBE_WORKER_DIR.
	fetchAt, fetch := findStep(job, func(s ghStep) bool { return s.Uses == "./.github/actions/rbe-worker" })
	repAt, _ := findStep(job, runs(`worker-env-drift" report `))
	if fetch == nil || fetchAt > repAt {
		t.Errorf("%s report job: no fetch-the-pinned-rbe-worker step before the report command: %+v", path, fetch)
	}
	if rep.Env["RBE_WORKER_DIR"] != "${{ steps.rbe-worker.outputs.dir }}" {
		t.Errorf("%s report job: report step RBE_WORKER_DIR = %q, want the fetch step's dir output", path, rep.Env["RBE_WORKER_DIR"])
	}
}

// checkDriftUpload: after the measuring step fails, its drift directory goes
// up as the artifact the report job reads.
func checkDriftUpload(t *testing.T, path string, job ghJob, measure int, ifExpr string) int {
	t.Helper()
	i, up := findStep(job, func(s ghStep) bool {
		return strings.HasPrefix(s.Uses, "actions/upload-artifact@") && s.With["name"] == rbeWorkerEnvDriftName
	})
	if up == nil || i < measure || up.If != ifExpr || up.With["name"] != rbeWorkerEnvDriftName ||
		up.With["path"] != rbeWorkerEnvDriftDir || up.With["if-no-files-found"] != "ignore" {
		t.Errorf("%s: no drift upload (if %s, name %s, path %s, if-no-files-found ignore) after the measurement: %+v", path, ifExpr, rbeWorkerEnvDriftName, rbeWorkerEnvDriftDir, up)
	}
	return i
}

// isMeasureStep: a blacksmith-worker.sh step in measure mode, run from
// $RBE_WORKER_DIR (the pinned rbe-worker the fetch step checked out).
func isMeasureStep(s ghStep) bool {
	run := strings.TrimSpace(s.Run)
	return run == `"$RBE_WORKER_DIR/blacksmith-worker.sh"` && s.Env["WORKER_MODE"] == "measure"
}

// TestRBEPoolWorkflowsReportDriftWhileServing: both pool workflows boot their
// worker unconditionally (beads shares the pools, and a skipped worker job
// would conclude success and have the scaler re-dispatch at once). The worker
// job measures its host in a step of its own that may fail without stopping
// the job, uploads the drift, and then serves; a job beside it waits for that
// upload and reports the pin's drift issue while the worker serves.
func TestRBEPoolWorkflowsReportDriftWhileServing(t *testing.T) {
	const defaultBranch = "github.ref == format('refs/heads/{0}', github.event.repository.default_branch)"
	for _, path := range []string{rbeWorkerWorkflow, rbeForkPoolWorkflow} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			wf := parseWorkflow(t, path)
			checkPinnedActions(t, path, wf)
			if len(wf.Permissions) != 1 || wf.Permissions["contents"] != "read" {
				t.Errorf("permissions %v, want contents: read (jobs widen their own)", wf.Permissions)
			}
			if got := rbeSortedKeys(wf.Jobs); strings.Join(got, ",") != "await-drift,report-drift,worker" {
				t.Fatalf("jobs %v, want worker, await-drift, report-drift (no gate)", got)
			}
			worker := wf.Jobs["worker"]
			if worker.Needs != nil || worker.If != defaultBranch {
				t.Errorf("worker needs %v if %q, want nothing but the default branch", worker.Needs, worker.If)
			}
			if len(worker.Permissions) != 0 {
				t.Errorf("worker permissions %v: the worker VM gets no token beyond contents: read", worker.Permissions)
			}
			fetchAt, fetch := findStep(worker, func(s ghStep) bool { return s.Uses == "./.github/actions/rbe-worker" })
			if fetch == nil {
				t.Fatalf("worker job: no fetch-the-pinned-rbe-worker step")
			}
			measureAt, measure := findStep(worker, isMeasureStep)
			if measure == nil || measure.ID != "worker-env" || measure.ContinueOnError != "true" || len(measure.Env) != 3 ||
				measure.Env["RBE_WORKER_DIR"] != "${{ steps.rbe-worker.outputs.dir }}" ||
				measure.Env["RBE_WORKER_REVISION"] != "${{ steps.rbe-worker.outputs.sha }}" {
				t.Fatalf("worker job: no continue-on-error measure step (id worker-env, WORKER_MODE=measure, RBE_WORKER_DIR and RBE_WORKER_REVISION): %+v", measure)
			}
			if fetchAt > measureAt {
				t.Errorf("worker job: the fetch step (%d) must come before the measure step (%d)", fetchAt, measureAt)
			}
			upAt := checkDriftUpload(t, path, worker, measureAt, "steps.worker-env.outcome == 'failure'")
			serveAt, serveStep := findStep(worker, func(s ghStep) bool {
				run := strings.TrimSpace(s.Run)
				return run == `"$RBE_WORKER_DIR/blacksmith-worker.sh"` && s.Env["WORKER_MODE"] == "pool"
			})
			if serveStep != nil && serveStep.Env["RBE_WORKER_DIR"] != "${{ steps.rbe-worker.outputs.dir }}" {
				t.Errorf("the pool worker step RBE_WORKER_DIR = %q, want the fetch step's dir output", serveStep.Env["RBE_WORKER_DIR"])
			}
			if serveAt < upAt {
				t.Errorf("the pool worker step (%d) must come after the drift upload (%d)", serveAt, upAt)
			}
			if serve := worker.Steps[serveAt]; serve.If != "" || serve.ContinueOnError == "true" {
				t.Errorf("the pool worker step runs if %q, continue-on-error %q; it must run whatever the measurement said", serve.If, serve.ContinueOnError)
			}
			upload := worker.Steps[upAt].Name

			await := wf.Jobs["await-drift"]
			if await.Needs != nil || await.If != defaultBranch || await.RunsOn != "ubuntu-latest" {
				t.Errorf("await-drift needs %v if %q runs-on %q, want beside the worker on ubuntu-latest", await.Needs, await.If, await.RunsOn)
			}
			if len(await.Permissions) != 2 || await.Permissions["contents"] != "read" || await.Permissions["actions"] != "read" {
				t.Errorf("await-drift permissions %v, want contents: read, actions: read", await.Permissions)
			}
			awaitFetchAt, awaitFetch := findStep(await, func(s ghStep) bool { return s.Uses == "./.github/actions/rbe-worker" })
			waitAt, wait := findStep(await, runs(`worker-env-drift" await `))
			if wait == nil || wait.ID != "await" || wait.Env["GH_TOKEN"] != "${{ github.token }}" ||
				wait.Env["RBE_WORKER_DIR"] != "${{ steps.rbe-worker.outputs.dir }}" ||
				!strings.Contains(wait.Run, ` await "`+worker.Name+`" "`+upload+`"`) {
				t.Errorf("await-drift does not wait for %q / %q: %+v", worker.Name, upload, wait)
			}
			if awaitFetch == nil || awaitFetchAt > waitAt {
				t.Errorf("await-drift: no fetch-the-pinned-rbe-worker step before the await command")
			}
			if await.Outputs["drift"] != "${{ steps.await.outputs.drift }}" {
				t.Errorf("await-drift outputs %v, want drift from the await step", await.Outputs)
			}
			report := wf.Jobs["report-drift"]
			if report.If != "needs.await-drift.outputs.drift == 'true'" {
				t.Errorf("report-drift if %q", report.If)
			}
			checkDriftReportJob(t, path, report, "await-drift")
		})
	}
}

// TestRBEWorkerEnvCanaryWorkflow: every six hours (and when the default
// branch moves the pin) a small Blacksmith runner provisions and measures
// itself as a worker would; drift opens the pin's issue, a match closes
// superseded ones.
func TestRBEWorkerEnvCanaryWorkflow(t *testing.T) {
	wf := parseWorkflow(t, rbeWorkerEnvCanary)
	checkPinnedActions(t, rbeWorkerEnvCanary, wf)
	if got := rbeSortedKeys(wf.On); strings.Join(got, ",") != "push,schedule,workflow_dispatch" {
		t.Errorf("on %v, want push, schedule, workflow_dispatch", got)
	}
	sched, _ := wf.On["schedule"].([]any)
	if len(sched) != 1 || sched[0].(map[string]any)["cron"] != "17 */6 * * *" {
		t.Errorf("schedule %v, want every six hours", sched)
	}
	push, _ := wf.On["push"].(map[string]any)
	var paths []string
	for _, p := range push["paths"].([]any) {
		paths = append(paths, p.(string))
	}
	if strings.Join(paths, ",") != "platforms/BUILD.bazel,tools/rbe/worker-env.txt,.github/actions/rbe-worker/**,.github/workflows/rbe-worker-env-canary.yml" {
		t.Errorf("push paths %v, want platforms/BUILD.bazel, tools/rbe/worker-env.txt, .github/actions/rbe-worker/** and the canary's own path", paths)
	}
	if len(wf.Permissions) != 1 || wf.Permissions["contents"] != "read" {
		t.Errorf("permissions %v, want contents: read", wf.Permissions)
	}
	measure := wf.Jobs["measure"]
	if measure.RunsOn != "blacksmith-2vcpu-ubuntu-2404" || len(measure.Permissions) != 0 {
		t.Errorf("measure runs-on %q permissions %v, want blacksmith-2vcpu-ubuntu-2404 and no extra permission", measure.RunsOn, measure.Permissions)
	}
	fetchAt, fetch := findStep(measure, func(s ghStep) bool { return s.Uses == "./.github/actions/rbe-worker" })
	if fetch == nil {
		t.Fatalf("measure job: no fetch-the-pinned-rbe-worker step")
	}
	i, s := findStep(measure, isMeasureStep)
	if s == nil || len(s.Env) != 3 || s.Env["WORKER_MODE"] != "measure" || s.Env["RBE_WORKER_DIR"] != "${{ steps.rbe-worker.outputs.dir }}" ||
		s.Env["RBE_WORKER_REVISION"] != "${{ steps.rbe-worker.outputs.sha }}" {
		t.Fatalf("measure job: no blacksmith-worker.sh step with WORKER_MODE=measure, RBE_WORKER_DIR and RBE_WORKER_REVISION: %+v", s)
	}
	if fetchAt > i {
		t.Errorf("measure job: the fetch step (%d) must come before the measure step (%d)", fetchAt, i)
	}
	checkDriftUpload(t, rbeWorkerEnvCanary, measure, i, "failure()")
	report := wf.Jobs["report"]
	if report.If != "!cancelled() && needs.measure.result != 'skipped'" { //nolint:misspell // GitHub Actions spells it cancelled()
		t.Errorf("report if %q", report.If)
	}
	checkDriftReportJob(t, rbeWorkerEnvCanary, report, "measure")
	reportFetchAt, reportFetch := findStep(report, func(s ghStep) bool { return s.Uses == "./.github/actions/rbe-worker" })
	resolveAt, s := findStep(report, runs(`worker-env-drift" resolve`))
	if s == nil || s.If != "needs.measure.result == 'success'" || s.Env["RBE_WORKER_DIR"] != "${{ steps.rbe-worker.outputs.dir }}" {
		t.Errorf("report job: no resolve step for a matching host: %+v", s)
	}
	if reportFetch == nil || reportFetchAt > resolveAt {
		t.Errorf("report job: no fetch-the-pinned-rbe-worker step before the resolve command")
	}
}

// TestBazelMultiLaneWorkerEnvPreflight: bazel.yml's remote jobs ask the
// worker-env preflight before any remote bazel run. The lane job asks
// before setup-bazel (no rbe-fork certificate for a run that would only
// queue), counts as remote in exactly the modes setup-bazel attaches a
// remote executor, and skips Bazel when the change moves the pin. Neither it
// nor the coverage job (remote only, which asks too) measures its own host:
// the worker-host job does (TestBazelWorkerHostFetchAlwaysRuns).
func TestBazelMultiLaneWorkerEnvPreflight(t *testing.T) {
	const (
		skip      = "steps.worker-env.outputs.pin-moved != 'true'"
		preflight = rbeWorkerEnvDrift + ` preflight "$RUNNER_TEMP/worker-env-base.bazel" "$RUNNER_TEMP/worker-env-changed"`
	)
	bazelRun := regexp.MustCompile(`(?m)^\s*bazel ("\$\{args\[@\]\}"|coverage )`)
	wf := parseWorkflow(t, bazelMultiLaneWorkflow)
	for _, id := range []string{"lane", "coverage"} {
		t.Run(id, func(t *testing.T) {
			job := wf.Jobs[id]
			if job.Permissions["issues"] != "read" {
				t.Errorf("%s job permissions %v, want issues: read (drift issues)", id, job.Permissions)
			}
			preAt, pre := findStep(job, func(s ghStep) bool { return s.ID == "worker-env" })
			setupAt, setup := findStep(job, func(s ghStep) bool { return s.Uses == "./.github/actions/setup-bazel" })
			if pre == nil || setup == nil || preAt > setupAt {
				t.Fatalf("%s job: no worker-env preflight step before setup-bazel", id)
			}
			if !strings.Contains(pre.Run, preflight) {
				t.Errorf("%s preflight step does not run %s:\n%s", id, preflight, pre.Run)
			}
			for k, v := range map[string]string{
				"GH_TOKEN":       "${{ github.token }}",
				"DEFAULT_BRANCH": "${{ github.event.repository.default_branch }}",
			} {
				if pre.Env[k] != v {
					t.Errorf("%s preflight env %s = %q, want %q", id, k, pre.Env[k], v)
				}
			}
			if setup.If != skip {
				t.Errorf("%s setup-bazel if %q, want %q (no certificate or cache restore for a moved pin)", id, setup.If, skip)
			}
			bazelSteps := 0
			for i, s := range job.Steps {
				if !bazelRun.MatchString(s.Run) {
					continue
				}
				bazelSteps++
				if i < preAt {
					t.Errorf("%s step %q runs bazel before the worker-env preflight", id, s.Name)
				}
				if s.If != skip {
					t.Errorf("%s step %q if %q: a moved pin must skip it (%s)", id, s.Name, s.If, skip)
				}
			}
			if bazelSteps == 0 {
				t.Fatalf("%s job: no bazel test or coverage step", id)
			}
		})
	}

	lane := wf.Jobs["lane"]
	_, pre := findStep(lane, func(s ghStep) bool { return s.ID == "worker-env" })
	if pre == nil {
		t.Fatal("lane job: no worker-env preflight step")
	}
	// setup-bazel attaches an executor in mode remote and fork-* (the lane
	// job's Set up Bazel env), and in no other mode.
	if pre.Env["MODE"] != "${{ needs.rbe.outputs.mode }}" ||
		!strings.Contains(pre.Run, `case "$MODE" in remote|fork-ro|fork-rw) export WORKER_ENV_REMOTE=true ;; esac`) {
		t.Errorf("lane preflight does not set WORKER_ENV_REMOTE in modes remote, fork-ro and fork-rw:\n%s", pre.Run)
	}
	// The base the checkout was merged onto is the lane's third argument:
	// a stale base is told from a change that moves the pin by it. The
	// coverage job runs on the default branch with no fresh-merge and keeps
	// the two-argument call.
	if pre.Env["BASE_SHA"] != "${{ needs.rbe.outputs.base-sha }}" ||
		!strings.Contains(pre.Run, preflight+` "$RUNNER_TEMP/worker-env-onto.bazel"`) {
		t.Errorf("lane preflight does not pass the merged-onto commit's BUILD file (BASE_SHA %q) as the third argument:\n%s", pre.Env["BASE_SHA"], pre.Run)
	}
	// The checkout is full blobless history for fresh-merge; a --depth fetch
	// would make it shallow.
	if strings.Contains(pre.Run, "--depth") {
		t.Errorf("lane preflight fetches with --depth into a full-history checkout:\n%s", pre.Run)
	}
	// The lane job does not measure its own Blacksmith host; the worker-host
	// job does, once per run instead of once per lane.
	if _, meas := findStep(lane, runs("blacksmith-worker.sh")); meas != nil {
		t.Errorf("lane job still measures the host (%+v); worker-host covers this now (fix 10), once per run instead of once per lane", meas)
	}

	coverage := wf.Jobs["coverage"]
	_, cpre := findStep(coverage, func(s ghStep) bool { return s.ID == "worker-env" })
	if cpre == nil || cpre.Env["WORKER_ENV_REMOTE"] != "true" {
		t.Errorf("coverage preflight must set WORKER_ENV_REMOTE=true (the job runs in mode remote only): %+v", cpre)
	}
	if _, three := cpre.Env["BASE_SHA"]; three || strings.Contains(cpre.Run, "worker-env-onto") {
		t.Errorf("coverage preflight passes a merged-onto commit; it has no fresh-merge and keeps the two-argument call: %+v", cpre)
	}
	if _, m := findStep(coverage, runs("blacksmith-worker.sh")); m != nil {
		t.Errorf("coverage job measures the host; the worker-host job does")
	}
}

// laneWorkerEnvStep is the lane job's "worker-env preflight" step.
func laneWorkerEnvStep(t *testing.T) ghStep {
	t.Helper()
	_, step := findStep(parseWorkflow(t, bazelMultiLaneWorkflow).Jobs["lane"], func(s ghStep) bool { return s.ID == "worker-env" })
	if step == nil {
		t.Fatal("lane job: no worker-env preflight step")
	}
	return *step
}

// TestBazelLaneWorkerEnvStepStaleBase runs the lane's real worker-env
// preflight step (bash -e, as Actions runs a step with no shell) against a
// scratch origin whose main re-pinned (S1) after the run chose its base (S0).
// The checkout is the PR head merged onto BASE_SHA, as fresh-merge leaves it.
// A stale base must fail a remote lane with its cure, not skip bazel green:
// run 37579257822 (PR #7268, attempt 2) merged with zero tests run that way.
func TestBazelLaneWorkerEnvStepStaleBase(t *testing.T) {
	step := laneWorkerEnvStep(t)
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	env := []string{
		"HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"), "LC_ALL=C",
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@invalid", "GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@invalid",
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(dir, msg string, files map[string]string) string {
		t.Helper()
		for rel, content := range files {
			writeDriftFile(t, filepath.Join(dir, rel), content)
		}
		git(dir, "add", "-A")
		git(dir, "commit", "--quiet", "-m", msg)
		return git(dir, "rev-parse", "HEAD")
	}

	p0, p1, p2 := sha256Pin("p0\n"), sha256Pin("p1\n"), sha256Pin("p2\n")
	origin := filepath.Join(tmp, "origin.git")
	seed := filepath.Join(tmp, "seed")
	for _, d := range []string{origin, seed} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git(origin, "init", "--quiet", "--bare", "-b", "main")
	git(seed, "init", "--quiet", "-b", "main")
	git(seed, "remote", "add", "origin", origin)
	tool, err := os.ReadFile(filepath.Join(repoRoot(t), rbeWorkerEnvDrift))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(seed, "tools/rbe"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(seed, rbeWorkerEnvDrift), string(tool))
	s0 := commit(seed, "S0", map[string]string{
		"platforms/BUILD.bazel":    buildWithPin(p0),
		"tools/rbe/worker-env.txt": "p0\n",
		"README.md":                "s0\n",
	})
	// The PR: one non-host file, and a second PR that re-pins to p2.
	git(seed, "checkout", "--quiet", "-b", "pr", s0)
	h := commit(seed, "H", map[string]string{"pr.txt": "pr\n"})
	git(seed, "checkout", "--quiet", "-b", "repin", s0)
	h2 := commit(seed, "H2", map[string]string{"platforms/BUILD.bazel": buildWithPin(p2), "tools/rbe/worker-env.txt": "p2\n"})
	// Main re-pins after the run chose its base.
	git(seed, "checkout", "--quiet", "main")
	s1 := commit(seed, "S1", map[string]string{"platforms/BUILD.bazel": buildWithPin(p1), "tools/rbe/worker-env.txt": "p1\n"})
	git(seed, "push", "--quiet", "origin", "main", "pr:pr", "repin:repin")
	// A commit with no platforms/BUILD.bazel at all, and a PR on it that adds
	// the file: the merged-onto file is absent at an existing commit, which
	// reads as an empty file and not as a read error.
	git(seed, "checkout", "--quiet", "-b", "nofile", s0)
	git(seed, "rm", "--quiet", "platforms/BUILD.bazel")
	sNoFile := commit(seed, "S-nofile", map[string]string{"README.md": "no platforms file\n"})
	git(seed, "checkout", "--quiet", "-b", "addfile")
	hAddFile := commit(seed, "H-addfile", map[string]string{"platforms/BUILD.bazel": buildWithPin(p0)})
	git(seed, "push", "--quiet", "origin", "nofile:nofile", "addfile:addfile")

	// checkout makes a fresh clone whose tree is head merged onto onto (or
	// onto alone), as fresh-merge (or a push event's checkout) leaves it.
	n := 0
	checkout := func(onto, head string) string {
		t.Helper()
		n++
		dir := filepath.Join(tmp, "run"+strconv.Itoa(n))
		git(tmp, "clone", "--quiet", "--no-checkout", origin, dir)
		git(dir, "checkout", "--quiet", "--detach", onto)
		if head != "" {
			git(dir, "merge", "--quiet", "--no-ff", "-m", "merge", head)
		}
		return dir
	}

	e := newDriftEnv(t, "lane step\n") // only its gh stub and issue list are used
	for _, tc := range []struct {
		name, onto, head, baseSHA, mode string
		fail                            bool
		moved, measure, wantOut         string
	}{
		{name: "stale base, remote", onto: s0, head: h, baseSHA: s0, mode: "remote", fail: true, moved: "false", measure: "false", wantOut: "::error title=rbe worker-env stale base::"},
		{name: "stale base, cache", onto: s0, head: h, baseSHA: s0, mode: "cache", moved: "false", measure: "false", wantOut: "::notice title=rbe worker-env stale base::"},
		{name: "fresh base after the re-pin", onto: s1, head: h, baseSHA: s1, mode: "remote", moved: "false", measure: "false"},
		{name: "push event, no base-sha", onto: s0, mode: "remote", moved: "true", measure: "true", wantOut: "::warning title=rbe worker-env pin moved::"},
		{name: "a PR that re-pins", onto: s0, head: h2, baseSHA: s0, mode: "remote", moved: "true", measure: "true", wantOut: "::warning title=rbe worker-env pin moved::"},
		{name: "base-sha is not a commit", onto: s0, head: h, baseSHA: strings.Repeat("ab", 20), mode: "remote", fail: true},
		{name: "file absent at base-sha is empty", onto: sNoFile, head: hAddFile, baseSHA: sNoFile, mode: "remote", moved: "true", measure: "true", wantOut: "::warning title=rbe worker-env pin moved::"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := checkout(tc.onto, tc.head)
			e.output = filepath.Join(dir, "..", "output"+strconv.Itoa(n))
			stepEnv := map[string]string{
				"PATH":              filepath.Join(e.dir, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
				"GITHUB_REPOSITORY": "acme/repo", "GITHUB_OUTPUT": e.output, "GITHUB_STEP_SUMMARY": filepath.Join(dir, "..", "summary"+strconv.Itoa(n)),
				"RUNNER_TEMP": dir + "/.tmp", "GH_TOKEN": "t", "GH_LOG": e.ghLog, "GH_ISSUES": e.issues,
				"DEFAULT_BRANCH": "main", "MODE": tc.mode, "BASE_SHA": tc.baseSHA,
			}
			if err := os.MkdirAll(stepEnv["RUNNER_TEMP"], 0o755); err != nil {
				t.Fatal(err)
			}
			for _, kv := range env {
				k, v, _ := strings.Cut(kv, "=")
				stepEnv[k] = v
			}
			path := filepath.Join(dir, ".tmp", "step.sh")
			if err := os.WriteFile(path, []byte(step.Run), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-e", path)
			cmd.Dir = dir
			for k, v := range stepEnv {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("step error %v, want failure %v\n%s", err, tc.fail, out)
			}
			if !strings.Contains(string(out), tc.wantOut) {
				t.Errorf("step output lacks %q:\n%s", tc.wantOut, out)
			}
			if tc.moved == "" {
				return // a step that failed before the preflight wrote nothing to assert
			}
			for k, v := range map[string]string{"pin-moved": tc.moved, "measure": tc.measure} {
				if got, _ := readStepOutput(t, e.output, k); got != v {
					t.Errorf("output %s = %q, want %q\n%s", k, got, v, out)
				}
			}
		})
	}
}
