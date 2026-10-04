package scripts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// rbe-west's oss and oss-fork schedulers match worker-env exactly, so a pin
// that no live worker advertises is a queue that never drains while the pool
// scaler keeps dispatching Blacksmith workers that cannot take it. These tests
// pin the guards (tools/rbe/worker-env-drift) that make that loud and early:
//
//   - a worker whose host is not the pinned one fails its run before
//     NativeLink starts (never registers), with the diff and the manifest and
//     pin to commit in its step summary, and opens or updates the pin's drift
//     issue;
//   - while that issue is open, the pool workflows boot no worker VM and
//     remote CI on that pin fails at once instead of queueing;
//   - a scheduled canary measures the Blacksmith image against the pin;
//   - a change that moves the pin, or touches the worker host's definition,
//     is measured on the Blacksmith image inside the required bazel job, and
//     a moved pin skips the remote suite (no worker serves it before merge).

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
// $GH_ISSUES (a JSON array of {title,url}) through the --jq it is given, api
// answers commits with $GH_COMMITS and contents?ref=SHA with $GH_FILES/SHA,
// issue view with $GH_VIEW. Every call is logged to $GH_LOG, one per line.
// GH_FAIL=1 fails every call.
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
"issue view") cat "$GH_VIEW" ;;
api*)
	for a; do
		case "$a" in
		*commits\?*) cat "$GH_COMMITS" ;;
		*contents/*ref=*) cat "$GH_FILES/${a##*ref=}" ;;
		esac
	done
	;;
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

// TestRBEWorkerEnvDriftCheck: the pinned host passes quietly; any other
// fails with the diff, the manifest and the exact pin line to commit, in the
// step summary and in the drift directory the report job reads.
func TestRBEWorkerEnvDriftCheck(t *testing.T) {
	pinned := "arch x86_64\npkg tmux 3.4-1\n"
	e := newDriftEnv(t, pinned)
	measured := filepath.Join(e.dir, "measured.txt")
	writeDriftFile(t, measured, pinned)
	out, err := e.run("check", measured)
	if err != nil || !strings.Contains(out, "this host is the pinned one ("+e.pin+")") {
		t.Fatalf("check of the pinned host: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "worker-env-drift")); !os.IsNotExist(err) {
		t.Errorf("check of the pinned host left a drift directory (%v)", err)
	}
	if s := e.read(e.summary); s != "" {
		t.Errorf("check of the pinned host wrote a step summary:\n%s", s)
	}

	drifted := "arch x86_64\npkg tmux 3.5-1\n"
	got := sha256Pin(drifted)
	writeDriftFile(t, measured, drifted)
	out, err = e.run("check", measured)
	if err == nil {
		t.Fatalf("check of a drifted host succeeded:\n%s", out)
	}
	if !strings.Contains(out, "::error title=rbe worker-env drift::this host measures worker-env="+got+", CI requests "+e.pin) {
		t.Errorf("check output has no ::error naming both hashes:\n%s", out)
	}
	summary := e.read(e.summary)
	for _, want := range []string{
		"### rbe worker-env drift",
		"-pkg tmux 3.4-1\n+pkg tmux 3.5-1\n",
		"        \"worker-env\": \"" + got + "\",\n",
		"```\n" + drifted + "```",
		"runner-1 in https://github.com/acme/repo/actions/runs/42",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("step summary missing %q:\n%s", want, summary)
		}
	}
	dir := filepath.Join(e.dir, "worker-env-drift")
	for name, want := range map[string]string{
		"worker-env.txt": drifted,
		"pinned-pin":     e.pin + "\n",
		"measured-pin":   got + "\n",
	} {
		if b := e.read(filepath.Join(dir, name)); b != want {
			t.Errorf("drift dir %s = %q, want %q", name, b, want)
		}
	}
}

// TestRBEWorkerEnvDriftGate: the pool boots no worker while its pin has an
// open drift issue, does for any other pin's issue, and fails open when
// GitHub cannot be asked (a GitHub outage must not stop the pools).
func TestRBEWorkerEnvDriftGate(t *testing.T) {
	e := newDriftEnv(t, "a\n")
	other := sha256Pin("b\n")
	e.setIssues(driftTitle(other), "https://x/issues/1")
	if out, err := e.run("gate"); err != nil {
		t.Errorf("gate with another pin's issue failed: %v\n%s", err, out)
	}
	e.setIssues(driftTitle(other), "https://x/issues/1", driftTitle(e.pin), "https://x/issues/2")
	out, err := e.run("gate")
	if err == nil || !strings.Contains(out, "::error title=rbe worker-env drift::https://x/issues/2") {
		t.Errorf("gate with this pin's issue: %v\n%s", err, out)
	}
	if s := e.read(e.summary); !strings.Contains(s, "https://x/issues/2") {
		t.Errorf("gate summary %q does not link the issue", s)
	}
	if log := e.read(e.ghLog); !strings.Contains(log, "issue list -R acme/repo --label "+rbeWorkerEnvLabel+" --state open") {
		t.Errorf("gate did not list the open %s issues:\n%s", rbeWorkerEnvLabel, log)
	}
	e.extra = []string{"GH_FAIL=1"}
	if out, err := e.run("gate"); err != nil || !strings.Contains(out, "::warning") {
		t.Errorf("gate with GitHub down: %v (want success with a warning)\n%s", err, out)
	}
}

// TestRBEWorkerEnvDriftPreflight: bazel-test learns whether the change moves
// the pin off the default branch's (no worker serves it before merge),
// whether to measure the host, and fails a remote run on a pin with an open
// drift issue instead of queueing it.
func TestRBEWorkerEnvDriftPreflight(t *testing.T) {
	e := newDriftEnv(t, "a\n")
	base := filepath.Join(e.dir, "base.bazel")
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
	for _, tc := range []struct {
		name, basePin, changed, remote string
		issue, fail                    bool
		moved, measure                 string
	}{
		{name: "same pin", basePin: e.pin, changed: "cmd/gc/main.go\n", moved: "false", measure: "false"},
		{name: "worker script changed", basePin: e.pin, changed: "README.md\ntools/rbe/blacksmith-worker.sh\n", moved: "false", measure: "true"},
		{name: "measurement changed", basePin: e.pin, changed: "tools/rbe/worker-env\n", moved: "false", measure: "true"},
		{name: "similar path", basePin: e.pin, changed: "tools/rbe/worker-env.txt.orig\n", moved: "false", measure: "false"},
		{name: "pin moved", basePin: sha256Pin("b\n"), changed: "tools/rbe/worker-env.txt\n", moved: "true", measure: "true"},
		{name: "default branch has no pin", basePin: "", changed: "platforms/BUILD.bazel\n", moved: "true", measure: "true"},
		{name: "moved pin ignores its issue", basePin: sha256Pin("b\n"), changed: "x\n", issue: true, remote: "true", moved: "true", measure: "true"},
		{name: "drift issue, local run", basePin: e.pin, changed: "x\n", issue: true, moved: "false", measure: "false"},
		{name: "drift issue, remote run", basePin: e.pin, changed: "x\n", issue: true, remote: "true", fail: true, moved: "false", measure: "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.basePin == "" {
				writeDriftFile(t, base, "")
			} else {
				writeDriftFile(t, base, buildWithPin(tc.basePin))
			}
			writeDriftFile(t, changed, tc.changed)
			e.setIssues()
			wantIssue := ""
			if tc.issue {
				wantIssue = "https://x/issues/7"
				e.setIssues(driftTitle(e.pin), wantIssue)
			}
			e.extra = []string{"WORKER_ENV_REMOTE=" + tc.remote}
			out, err := e.run("preflight", base, changed)
			if (err != nil) != tc.fail {
				t.Fatalf("preflight error %v, want failure %v\n%s", err, tc.fail, out)
			}
			if tc.fail && !strings.Contains(out, "::error title=rbe worker-env drift::"+wantIssue) {
				t.Errorf("failed preflight does not name the issue:\n%s", out)
			}
			got := outputs()
			want := map[string]string{"pin": e.pin, "pin-moved": tc.moved, "measure": tc.measure, "drift-issue": wantIssue}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("output %s = %q, want %q (outputs %v)", k, got[k], v, got)
				}
			}
		})
	}
}

// driftDir runs check on a drifted host and returns its drift directory and
// the measured pin.
func (e *driftEnv) driftDir(measured string) (string, string) {
	e.t.Helper()
	path := filepath.Join(e.dir, "measured.txt")
	writeDriftFile(e.t, path, measured)
	if out, err := e.run("check", path); err == nil {
		e.t.Fatalf("check of a drifted host succeeded:\n%s", out)
	}
	_ = os.Remove(e.summary)
	return filepath.Join(e.dir, "worker-env-drift"), sha256Pin(measured)
}

// TestRBEWorkerEnvDriftReport: the first drift of a pin opens its issue
// (labeled, titled with the pin, the manifest in the body); a new
// measurement is added once; a host whose measurement is a previous pin (a
// stale image after a re-pin) opens nothing, so it cannot block the new pin.
func TestRBEWorkerEnvDriftReport(t *testing.T) {
	e := newDriftEnv(t, "a\n")
	commits := filepath.Join(e.dir, "commits")
	files := filepath.Join(e.dir, "files")
	view := filepath.Join(e.dir, "view")
	writeDriftFile(t, commits, "c1\nc0\n")
	writeDriftFile(t, filepath.Join(files, "c1"), buildWithPin(e.pin))
	writeDriftFile(t, filepath.Join(files, "c0"), buildWithPin(sha256Pin("old\n")))
	writeDriftFile(t, view, "")
	e.extra = []string{"GH_COMMITS=" + commits, "GH_FILES=" + files, "GH_VIEW=" + view, "DEFAULT_BRANCH=main"}

	// Nothing recorded (the worker failed for another reason): no GitHub call.
	if out, err := e.run("report", filepath.Join(e.dir, "nothing")); err != nil || e.read(e.ghLog) != "" {
		t.Fatalf("report without drift: %v, gh calls %q\n%s", err, e.read(e.ghLog), out)
	}

	dir, got := e.driftDir("new\n")
	out, err := e.run("report", dir)
	if err != nil {
		t.Fatalf("report: %v\n%s", err, out)
	}
	log := e.read(e.ghLog)
	if !strings.Contains(log, "api repos/acme/repo/commits?path=platforms/BUILD.bazel&sha=main&per_page=20") {
		t.Errorf("report did not look up the previous pins:\n%s", log)
	}
	if !strings.Contains(log, "label create "+rbeWorkerEnvLabel+" -R acme/repo --force") {
		t.Errorf("report did not ensure the label:\n%s", log)
	}
	create := regexp.MustCompile(`(?m)^issue create -R acme/repo --title ` + regexp.QuoteMeta(driftTitle(e.pin)) + ` --label ` + rbeWorkerEnvLabel + ` --body-file (\S+)$`).FindStringSubmatch(log)
	if create == nil {
		t.Fatalf("report did not open the pin's issue:\n%s", log)
	}
	if body := e.read(create[1]); !strings.Contains(body, "```\nnew\n```") || !strings.Contains(body, `"worker-env": "`+got+`",`) {
		t.Errorf("issue body lacks the manifest or the pin line:\n%s", body)
	}

	// The issue is open: a new measurement is a comment, a known one nothing.
	e.setIssues(driftTitle(e.pin), "https://x/issues/9")
	_ = os.Remove(e.ghLog)
	if out, err := e.run("report", dir); err != nil || !strings.Contains(e.read(e.ghLog), "issue comment https://x/issues/9 -R acme/repo --body-file") {
		t.Errorf("report of a new measurement: %v, gh calls:\n%s\n%s", err, e.read(e.ghLog), out)
	}
	writeDriftFile(t, view, "earlier report\nworker-env="+got+"\n")
	_ = os.Remove(e.ghLog)
	if out, err := e.run("report", dir); err != nil || strings.Contains(e.read(e.ghLog), "issue comment") || strings.Contains(e.read(e.ghLog), "issue create") {
		t.Errorf("report of a known measurement: %v, gh calls:\n%s\n%s", err, e.read(e.ghLog), out)
	}

	// A stale host: it measures the pin of an earlier default-branch commit.
	e.setIssues()
	dir, _ = e.driftDir("old\n")
	_ = os.Remove(e.ghLog)
	out, err = e.run("report", dir)
	if err != nil || !strings.Contains(out, "::warning title=rbe worker-env stale host::") {
		t.Errorf("report of a stale host: %v\n%s", err, out)
	}
	if log := e.read(e.ghLog); strings.Contains(log, "issue create") || strings.Contains(log, "label create") {
		t.Errorf("report of a stale host touched issues:\n%s", log)
	}
}

// TestRBEWorkerEnvDriftResolve: once the default branch pins another host,
// the old pins' issues close; the current pin's stays, with a warning.
func TestRBEWorkerEnvDriftResolve(t *testing.T) {
	e := newDriftEnv(t, "a\n")
	e.setIssues(driftTitle(sha256Pin("old\n")), "https://x/issues/1", driftTitle(e.pin), "https://x/issues/2")
	out, err := e.run("resolve")
	if err != nil {
		t.Fatalf("resolve: %v\n%s", err, out)
	}
	log := e.read(e.ghLog)
	if !strings.Contains(log, "issue close https://x/issues/1 -R acme/repo --comment Superseded") {
		t.Errorf("resolve did not close the superseded issue:\n%s", log)
	}
	if strings.Contains(log, "issues/2 ") || !strings.Contains(out, "::warning title=rbe worker-env drift::this host matches the pin "+e.pin+", but https://x/issues/2 is open") {
		t.Errorf("resolve must leave the current pin's issue open and warn:\n%s\n%s", log, out)
	}
}

// TestRBEWorkerScriptRefusesDrift: the worker checks its measurement before
// NativeLink is even downloaded and exits on drift (it never registers a
// hash no client sends). measure mode provisions and checks without a
// certificate, then exits.
func TestRBEWorkerScriptRefusesDrift(t *testing.T) {
	script := readFile(t, repoRoot(t), rbeWorkerScript)
	at := 0
	for _, want := range []string{
		"WORKER_MODE=${WORKER_MODE:-run}\n",
		"[ \"$WORKER_MODE\" = measure ] || : \"${RBE_WORKER_TLS_CERT:?}\" \"${RBE_WORKER_TLS_KEY:?}\" \"${RBE_WEST_HOST:?}\" \"${WORKER_NAME:?}\"\n",
		"measure) ;;\n",
		`*) echo "WORKER_MODE must be run, pool or measure" >&2; exit 2 ;;`,
		"\ntools/rbe/worker-env \"${WORKER_TOOLSET[@]}\" >\"$RUNNER_TEMP/worker-env.txt\"\n",
		"\ntools/rbe/worker-env-drift check \"$RUNNER_TEMP/worker-env.txt\" || exit 3\n",
		"[ \"$WORKER_MODE\" != measure ] || exit 0\n",
		"/nativelink-${NL_VERSION}-x86_64-unknown-linux-musl.tar.gz",
	} {
		i := strings.Index(script[at:], want)
		if i < 0 {
			t.Fatalf("%s: %q missing or out of order", rbeWorkerScript, want)
		}
		at += i + len(want)
	}
	if strings.Contains(script, "::warning title=rbe worker-env drift") {
		t.Errorf("%s still only warns on drift", rbeWorkerScript)
	}
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
	ContinueOnError bool              `yaml:"continue-on-error"`
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
			if s.Uses != "" && !re.MatchString(s.Uses) {
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
	if dl == nil || dl.With["name"] != rbeWorkerEnvDriftName || !dl.ContinueOnError {
		t.Errorf("%s report job: download of %s missing or not continue-on-error: %+v", path, rbeWorkerEnvDriftName, dl)
	}
	_, rep := findStep(job, runs(rbeWorkerEnvDrift+" report "))
	if rep == nil || rep.Env["GH_TOKEN"] != "${{ github.token }}" {
		t.Errorf("%s report job: no %s report step with GH_TOKEN: %+v", path, rbeWorkerEnvDrift, rep)
	}
}

// checkDriftUpload: after the measuring step fails, its drift directory goes
// up as the artifact the report job reads.
func checkDriftUpload(t *testing.T, path string, job ghJob, measure int) {
	t.Helper()
	i, up := findStep(job, func(s ghStep) bool {
		return strings.HasPrefix(s.Uses, "actions/upload-artifact@") && s.With["name"] == rbeWorkerEnvDriftName
	})
	if up == nil || i < measure || up.If != "failure()" || up.With["name"] != rbeWorkerEnvDriftName ||
		up.With["path"] != rbeWorkerEnvDriftDir || up.With["if-no-files-found"] != "ignore" {
		t.Errorf("%s: no drift upload (if failure(), name %s, path %s, if-no-files-found ignore) after the measurement: %+v", path, rbeWorkerEnvDriftName, rbeWorkerEnvDriftDir, up)
	}
}

// TestRBEPoolWorkflowsGateAndReportDrift: both pool workflows ask the gate
// before a worker VM boots, and report a drifted worker as the pin's issue.
func TestRBEPoolWorkflowsGateAndReportDrift(t *testing.T) {
	const defaultBranch = "github.ref == format('refs/heads/{0}', github.event.repository.default_branch)"
	for _, path := range []string{rbeWorkerWorkflow, rbeForkPoolWorkflow} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			wf := parseWorkflow(t, path)
			checkPinnedActions(t, path, wf)
			if len(wf.Permissions) != 1 || wf.Permissions["contents"] != "read" {
				t.Errorf("permissions %v, want contents: read (jobs widen their own)", wf.Permissions)
			}
			if got := rbeSortedKeys(wf.Jobs); strings.Join(got, ",") != "gate,report-drift,worker" {
				t.Fatalf("jobs %v, want gate, worker, report-drift", got)
			}
			gate := wf.Jobs["gate"]
			if gate.If != defaultBranch || gate.RunsOn != "ubuntu-latest" {
				t.Errorf("gate if %q runs-on %q, want the default branch on ubuntu-latest", gate.If, gate.RunsOn)
			}
			if len(gate.Permissions) != 2 || gate.Permissions["contents"] != "read" || gate.Permissions["issues"] != "read" {
				t.Errorf("gate permissions %v, want contents: read, issues: read", gate.Permissions)
			}
			if _, s := findStep(gate, runs(rbeWorkerEnvDrift+" gate")); s == nil || s.Env["GH_TOKEN"] != "${{ github.token }}" {
				t.Errorf("gate has no %s gate step with GH_TOKEN", rbeWorkerEnvDrift)
			}
			worker := wf.Jobs["worker"]
			if n, _ := worker.Needs.(string); n != "gate" || worker.If != defaultBranch {
				t.Errorf("worker needs %v if %q, want gate and the default branch", worker.Needs, worker.If)
			}
			if len(worker.Permissions) != 0 {
				t.Errorf("worker permissions %v: the worker VM gets no token beyond contents: read", worker.Permissions)
			}
			i, _ := findStep(worker, runs(rbeWorkerScript))
			if i < 0 {
				t.Fatalf("worker job does not run %s", rbeWorkerScript)
			}
			checkDriftUpload(t, path, worker, i)
			report := wf.Jobs["report-drift"]
			if report.If != "failure() && needs.worker.result == 'failure'" {
				t.Errorf("report-drift if %q", report.If)
			}
			checkDriftReportJob(t, path, report, "worker")
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
	if strings.Join(paths, ",") != "platforms/BUILD.bazel,tools/rbe/**" {
		t.Errorf("push paths %v, want platforms/BUILD.bazel and tools/rbe/**", paths)
	}
	if len(wf.Permissions) != 1 || wf.Permissions["contents"] != "read" {
		t.Errorf("permissions %v, want contents: read", wf.Permissions)
	}
	measure := wf.Jobs["measure"]
	if measure.RunsOn != "blacksmith-2vcpu-ubuntu-2404" || len(measure.Permissions) != 0 {
		t.Errorf("measure runs-on %q permissions %v, want blacksmith-2vcpu-ubuntu-2404 and no extra permission", measure.RunsOn, measure.Permissions)
	}
	i, s := findStep(measure, runs(rbeWorkerScript))
	if s == nil || len(s.Env) != 1 || s.Env["WORKER_MODE"] != "measure" {
		t.Fatalf("measure job: no %s step with WORKER_MODE=measure alone: %+v", rbeWorkerScript, s)
	}
	checkDriftUpload(t, rbeWorkerEnvCanary, measure, i)
	report := wf.Jobs["report"]
	if report.If != "!cancelled() && needs.measure.result != 'skipped'" { //nolint:misspell // GitHub Actions spells it cancelled()
		t.Errorf("report if %q", report.If)
	}
	checkDriftReportJob(t, rbeWorkerEnvCanary, report, "measure")
	if _, s := findStep(report, runs(rbeWorkerEnvDrift+" resolve")); s == nil || s.If != "needs.measure.result == 'success'" {
		t.Errorf("report job: no resolve step for a matching host: %+v", s)
	}
}

// TestBazelTestWorkerEnvPreflight: the required bazel job asks the preflight
// before any remote bazel run, skips the remote suite when the change moves
// the pin, and measures its own Blacksmith host after every bazel run when
// the change touches the worker host (the toolset install would re-key the
// client's own actions if it came first).
func TestBazelTestWorkerEnvPreflight(t *testing.T) {
	wf := parseWorkflow(t, bazelTestWorkflow)
	job := wf.Jobs["bazel"]
	if job.Permissions["issues"] != "read" {
		t.Errorf("bazel job permissions %v, want issues: read (drift issues)", job.Permissions)
	}
	certAt, _ := findStep(job, func(s ghStep) bool { return s.ID == "fork-cert" })
	preAt, pre := findStep(job, func(s ghStep) bool { return s.ID == "worker-env" })
	if pre == nil || preAt < certAt {
		t.Fatalf("bazel job: no worker-env preflight step after fork-cert")
	}
	if !strings.Contains(pre.Run, rbeWorkerEnvDrift+` preflight "$RUNNER_TEMP/worker-env-base.bazel" "$RUNNER_TEMP/worker-env-changed"`) {
		t.Errorf("preflight step does not run %s preflight:\n%s", rbeWorkerEnvDrift, pre.Run)
	}
	for k, v := range map[string]string{
		"GH_TOKEN":       "${{ github.token }}",
		"DEFAULT_BRANCH": "${{ github.event.repository.default_branch }}",
		"RBE_FORK_CERT":  bazelRCForkCertEnv,
	} {
		if pre.Env[k] != v {
			t.Errorf("preflight env %s = %q, want %q", k, pre.Env[k], v)
		}
	}
	// Remote exactly when the bazel steps add --config=remote-exec.
	if !strings.Contains(pre.Run, `if [ -n "$BAZEL_REMOTE_EXECUTOR" ] || [ -n "$RBE_FORK_CERT" ]; then export WORKER_ENV_REMOTE=true; fi`) {
		t.Errorf("preflight step does not set WORKER_ENV_REMOTE as the remote-exec guard does:\n%s", pre.Run)
	}
	const skip = "steps.worker-env.outputs.pin-moved != 'true'"
	lastBazel := -1
	for i, s := range job.Steps {
		if !regexp.MustCompile(`(?m)^\s*bazel "\$\{ARGS\[@\]\}" (test|coverage)`).MatchString(s.Run) {
			continue
		}
		lastBazel = i
		if i < preAt {
			t.Errorf("step %q runs bazel before the worker-env preflight", s.Name)
		}
		if !strings.Contains(s.If, skip) && !strings.Contains(s.If, "github.ref == 'refs/heads/main'") {
			t.Errorf("step %q if %q: a moved pin must skip it (%s)", s.Name, s.If, skip)
		}
	}
	measAt, meas := findStep(job, runs(rbeWorkerScript))
	if meas == nil || meas.If != "always() && steps.worker-env.outputs.measure == 'true'" ||
		len(meas.Env) != 1 || meas.Env["WORKER_MODE"] != "measure" {
		t.Fatalf("bazel job: no host measurement step (if measure, WORKER_MODE=measure): %+v", meas)
	}
	if measAt < lastBazel {
		t.Errorf("the host measurement (step %d) runs before a bazel run (step %d): its toolset install would re-key the client's actions", measAt, lastBazel)
	}
}
