package scripts_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// gastownhall/beads carries a byte-identical copy of the worker-env manifest
// and pin, and rbe-west's pool workers serve main's pin only, so the push
// that moves main's pin leaves beads with no worker until its own sync PR
// lands. Two workflows close that gap from this side:
//
//   - worker-env-notify-beads.yml, on that push, sends beads the
//     repository_dispatch event its worker-env-autosync.yml starts on,
//     instead of waiting for that workflow's 15-minute cron to notice (24
//     minutes late on 2026-10-21). It alone holds the App key that can.
//   - worker-env-notify-beads-pr.yml, on a same-repo PR that moves the pin,
//     keeps one comment saying what merging does to beads. It holds no
//     secret and checks nothing out.
//
// These tests pin the contract with beads, the security shape of each file,
// and run their scripts against fixtures.

const (
	notifyBeadsWorkflow   = ".github/workflows/worker-env-notify-beads.yml"
	notifyBeadsPRWorkflow = ".github/workflows/worker-env-notify-beads-pr.yml"
	// The contract with gastownhall/beads' worker-env-autosync.yml: it
	// starts on this repository_dispatch event type, whose client_payload is
	// {"sha": <the gascity main commit>, "pin": <its worker-env pin>}.
	notifyBeadsEvent    = "gascity-worker-env-repinned"
	notifyBeadsRepo     = "gastownhall/beads"
	notifyBeadsAutosync = "worker-env-autosync.yml"
	notifyBeadsFallback = "gh workflow run worker-env-autosync.yml -R gastownhall/beads"
	notifyBeadsMarker   = "<!-- worker-env-notify-beads -->"
	notifyBeadsToken    = "ghs_minted-for-beads"
)

// notifyBeadsPaths are the worker host's files: the pin lives in
// platforms/BUILD.bazel and is the manifest's sha256.
var notifyBeadsPaths = []string{rbeWorkerPlatformBuild, rbeWorkerEnvManifest}

type notifyBeadsFile struct {
	On          yaml.Node                 `yaml:"on"`
	Permissions map[string]string         `yaml:"permissions"`
	Env         map[string]string         `yaml:"env"`
	Concurrency any                       `yaml:"concurrency"`
	Jobs        map[string]notifyBeadsJob `yaml:"jobs"`
}

type notifyBeadsJob struct {
	Name        string            `yaml:"name"`
	If          string            `yaml:"if"`
	Needs       any               `yaml:"needs"`
	RunsOn      string            `yaml:"runs-on"`
	Environment any               `yaml:"environment"`
	Permissions map[string]string `yaml:"permissions"`
	Concurrency map[string]any    `yaml:"concurrency"`
	Outputs     map[string]string `yaml:"outputs"`
	Steps       []ghStep          `yaml:"steps"`
}

func loadNotifyBeads(t *testing.T, path string) notifyBeadsFile {
	t.Helper()
	var wf notifyBeadsFile
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), path)), &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return wf
}

// notifyBeadsStep is the step with id in job.
func notifyBeadsStep(t *testing.T, wf notifyBeadsFile, job, id string) ghStep {
	t.Helper()
	for _, s := range wf.Jobs[job].Steps {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("job %s has no step with id %s", job, id)
	return ghStep{}
}

// notifyBeadsEnv is a step's environment as the runner builds it: the
// workflow's env, then the step's, each value's ${{ }} resolved under ctx
// (an output the step reads but ctx lacks resolves empty, as an unset one
// does on GitHub), plus the runner's own variables.
func notifyBeadsEnv(t *testing.T, wf notifyBeadsFile, step ghStep, ctx, runner map[string]string) map[string]string {
	t.Helper()
	env := map[string]string{}
	for k, v := range wf.Env {
		env[k] = interpolateGH(t, v, ctx)
	}
	for k, v := range step.Env {
		env[k] = interpolateGH(t, v, ctx)
	}
	for k, v := range runner {
		env[k] = v
	}
	return env
}

// notifyBeadsSleepStub stands in for sleep: it logs its argument to
// $SLEEP_LOG and returns at once.
const notifyBeadsSleepStub = "#!/bin/sh\necho \"$1\" >>\"$SLEEP_LOG\"\n"

// notifyBeadsGHStub stands in for gh in the workflows' steps. It logs every
// call (its arguments, one line) to $GH_LOG and the token it ran with to
// $GH_TOKENS, refuses any call the steps do not make (noting it in
// $GH_DIR/unexpected), and answers:
//
//   - POST repos/O/R/dispatches: the next status in $GH_STATUSES (one per
//     line; "none" is no answer at all), printed as gh --include prints it,
//     after saving --input as $GH_DIR/dispatch.N.json;
//   - GET .../pulls/N/files and .../issues/N/comments: $GH_FILES and
//     $GH_COMMENTS through the --jq filter given, as gh --jq does (one page);
//   - GET .../compare/BASE...HEAD: $GH_COMPARE through the --jq filter,
//     after logging BASE...HEAD to $GH_DIR/compare;
//   - GET .../contents/platforms/BUILD.bazel?ref=SHA with the raw media
//     type: $GH_BUILDS/SHA (404 without one), after logging SHA to
//     $GH_DIR/contents;
//   - POST .../issues/N/comments and PATCH .../issues/comments/ID: logs
//     "METHOD ENDPOINT" to $GH_DIR/writes and saves --input as
//     $GH_DIR/write.N.json.
const notifyBeadsGHStub = `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$GH_LOG"
printf '%s\n' "${GH_TOKEN:-}" >>"$GH_TOKENS"
n=$(($(wc -l <"$GH_LOG")))
unexpected() { echo "$*" >>"$GH_DIR/unexpected"; echo "gh stub: $*" >&2; exit 2; }
[ "${1:-}" = api ] || unexpected "not gh api: $*"
shift
method=GET include=false jq= input= accept= endpoint=
while [ $# -gt 0 ]; do
	case $1 in
	--method) method=$2; shift 2 ;;
	--include) include=true; shift ;;
	--paginate) shift ;;
	--jq) jq=$2; shift 2 ;;
	--input) input=$2; shift 2 ;;
	-H) accept=$2; shift 2 ;;
	-*) unexpected "flag $1" ;;
	*) endpoint=$1; shift ;;
	esac
done
case "$method $endpoint" in
"POST repos/"*/dispatches)
	cp "$input" "$GH_DIR/dispatch.$n.json"
	status=$(sed -n 1p "$GH_STATUSES")
	tail -n +2 "$GH_STATUSES" >"$GH_STATUSES.next" && mv -f "$GH_STATUSES.next" "$GH_STATUSES"
	if [ "$status" = none ]; then
		echo "error connecting to api.github.com" >&2
		exit 1
	fi
	if [ "$include" = true ]; then printf 'HTTP/2.0 %s Stub\nContent-Length: 0\r\n\r\n' "$status"; fi
	case $status in 2??) exit 0 ;; esac
	echo "gh: Stub (HTTP $status)" >&2
	exit 1
	;;
"GET repos/"*/pulls/*/files*) jq -r "$jq" "$GH_FILES" ;;
"GET repos/"*/issues/*/comments*) jq -r "$jq" "$GH_COMMENTS" ;;
"GET repos/"*/compare/*...*)
	echo "${endpoint##*/compare/}" >>"$GH_DIR/compare"
	jq -r "$jq" "$GH_COMPARE"
	;;
"GET repos/"*/contents/platforms/BUILD.bazel"?ref="*)
	[ "$accept" = "Accept: application/vnd.github.raw+json" ] || unexpected "contents without the raw media type: $accept"
	ref=${endpoint##*ref=}
	echo "$ref" >>"$GH_DIR/contents"
	if [ ! -f "$GH_BUILDS/$ref" ]; then
		echo "gh: Not Found (HTTP 404)" >&2
		exit 1
	fi
	cat "$GH_BUILDS/$ref"
	;;
"POST repos/"*/issues/*/comments | "PATCH repos/"*/issues/comments/*)
	echo "$method $endpoint" >>"$GH_DIR/writes"
	cp "$input" "$GH_DIR/write.$n.json"
	;;
*) unexpected "call $method $endpoint" ;;
esac
`

// notifyBeadsGH is a bin directory with the gh and sleep stubs, and the
// files they read and write.
type notifyBeadsGH struct {
	t                                                                         *testing.T
	bin, dir, builds, log, tokens, statuses, files, comments, compare, sleeps string
}

func newNotifyBeadsGH(t *testing.T) *notifyBeadsGH {
	t.Helper()
	g := &notifyBeadsGH{t: t, dir: t.TempDir()}
	g.bin = filepath.Join(g.dir, "bin")
	g.builds = filepath.Join(g.dir, "builds")
	for _, d := range []string{g.bin, g.builds} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{"gh": notifyBeadsGHStub, "sleep": notifyBeadsSleepStub} {
		if err := os.WriteFile(filepath.Join(g.bin, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []*string{&g.log, &g.tokens, &g.statuses, &g.files, &g.comments, &g.compare, &g.sleeps} {
		f, err := os.CreateTemp(g.dir, "gh-")
		if err != nil {
			t.Fatal(err)
		}
		*p = f.Name()
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

// env is the variables the stubs read, and a PATH that finds them first.
func (g *notifyBeadsGH) env() map[string]string {
	return map[string]string{
		"PATH":        g.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GH_DIR":      g.dir,
		"GH_BUILDS":   g.builds,
		"GH_LOG":      g.log,
		"GH_TOKENS":   g.tokens,
		"GH_STATUSES": g.statuses,
		"GH_FILES":    g.files,
		"GH_COMMENTS": g.comments,
		"GH_COMPARE":  g.compare,
		"SLEEP_LOG":   g.sleeps,
	}
}

func (g *notifyBeadsGH) write(path, content string) {
	g.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

// build serves content as platforms/BUILD.bazel at sha.
func (g *notifyBeadsGH) build(sha, content string) {
	g.t.Helper()
	g.write(filepath.Join(g.builds, sha), content)
}

// mergeBase answers the compare API with sha as the merge base, and serves
// build as platforms/BUILD.bazel there.
func (g *notifyBeadsGH) mergeBase(sha, build string) {
	g.t.Helper()
	g.write(g.compare, `{"merge_base_commit":{"sha":"`+sha+`"}}`)
	g.build(sha, build)
}

// lines are the non-empty lines of one of the stub's files.
func (g *notifyBeadsGH) lines(path string) []string {
	g.t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		g.t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// saved are the --input files the stub saved as PREFIX.N.json (dispatch or
// write), in call order.
func (g *notifyBeadsGH) saved(prefix string) []string {
	g.t.Helper()
	paths, err := filepath.Glob(filepath.Join(g.dir, prefix+".*.json"))
	if err != nil {
		g.t.Fatal(err)
	}
	byCall := map[int]string{}
	var calls []int
	for _, p := range paths {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), prefix+"."), ".json"))
		if err != nil {
			g.t.Fatalf("stub saved %s: %v", p, err)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			g.t.Fatal(err)
		}
		byCall[n] = string(b)
		calls = append(calls, n)
	}
	sort.Ints(calls)
	var out []string
	for _, n := range calls {
		out = append(out, byCall[n])
	}
	return out
}

func (g *notifyBeadsGH) checkNoUnexpected() {
	g.t.Helper()
	if calls := g.lines(filepath.Join(g.dir, "unexpected")); len(calls) > 0 {
		g.t.Errorf("the step made calls gh never gets from it: %v", calls)
	}
}

// TestWorkerEnvNotifyBeadsTriggers: the dispatch workflow runs on pushes to
// main that touch the worker host's files, and on nothing else; the comment
// workflow on same-repo PRs into main (the branch whose pin the pool workers
// serve) that touch them, and on nothing else.
func TestWorkerEnvNotifyBeadsTriggers(t *testing.T) {
	for _, tc := range []struct {
		path, event string
		want        map[string][]string
	}{
		{path: notifyBeadsWorkflow, event: "push", want: map[string][]string{
			"branches": {"main"}, "paths": notifyBeadsPaths,
		}},
		{path: notifyBeadsPRWorkflow, event: "pull_request_target", want: map[string][]string{
			"types": {"opened", "synchronize", "reopened"}, "branches": {"main"}, "paths": notifyBeadsPaths,
		}},
	} {
		t.Run(filepath.Base(tc.path), func(t *testing.T) {
			wf := loadNotifyBeads(t, tc.path)
			var on map[string]map[string][]string
			if err := wf.On.Decode(&on); err != nil {
				t.Fatalf("on: %v", err)
			}
			if got := rbeSortedKeys(on); strings.Join(got, ",") != tc.event {
				t.Fatalf("on %v, want %s alone", got, tc.event)
			}
			if !reflect.DeepEqual(on[tc.event], tc.want) {
				t.Errorf("on.%s %v, want %v", tc.event, on[tc.event], tc.want)
			}
		})
	}
}

// TestWorkerEnvNotifyBeadsJobsRunWhenTheyShould evaluates each job's `if`:
// repin runs on gastownhall/gascity alone (not on a fork's main), dispatch
// and grace (the rbe-west grace record, tools/rbe/worker-env-grace) only
// after repin found a moved pin, grace also after repin could not fetch
// main before the push (it compared with the parent, which can hide a
// re-pin), comment on same-repo PRs of gastownhall/gascity.
func TestWorkerEnvNotifyBeadsJobsRunWhenTheyShould(t *testing.T) {
	push := loadNotifyBeads(t, notifyBeadsWorkflow)
	pr := loadNotifyBeads(t, notifyBeadsPRWorkflow)
	if got := rbeSortedKeys(push.Jobs); strings.Join(got, ",") != "dispatch,grace,repin" {
		t.Fatalf("%s jobs %v, want repin, dispatch and grace", notifyBeadsWorkflow, got)
	}
	if got := rbeSortedKeys(pr.Jobs); strings.Join(got, ",") != "comment" {
		t.Fatalf("%s jobs %v, want comment", notifyBeadsPRWorkflow, got)
	}
	repin, dispatch, grace, comment := push.Jobs["repin"], push.Jobs["dispatch"], push.Jobs["grace"], pr.Jobs["comment"]
	if dispatch.Needs != "repin" || grace.Needs != "repin" || repin.Needs != nil || comment.Needs != nil {
		t.Errorf("needs: dispatch %v, grace %v, repin %v, comment %v; want dispatch and grace on repin alone", dispatch.Needs, grace.Needs, repin.Needs, comment.Needs)
	}
	// A status function (always(), failure() ...) would let dispatch or
	// grace run after a skipped or failed repin; without one, GitHub skips
	// them then.
	if strings.Contains(dispatch.If, "(") || strings.Contains(grace.If, "(") {
		t.Errorf("dispatch if %q, grace if %q call a function; they must run only after repin succeeded", dispatch.If, grace.If)
	}
	const (
		gascity = "gastownhall/gascity"
		fork    = "someone/gascity"
	)
	for _, tc := range []struct {
		name                   string
		ctx                    map[string]string
		repin, dispatch, grace bool
	}{
		{name: "push, pin moved", ctx: map[string]string{"github.repository": gascity, "needs.repin.outputs.moved": "true", "needs.repin.outputs.unfetched": "false"}, repin: true, dispatch: true, grace: true},
		{name: "push, pin unchanged", ctx: map[string]string{"github.repository": gascity, "needs.repin.outputs.moved": "false", "needs.repin.outputs.unfetched": "false"}, repin: true},
		{name: "push, pin moved from the parent's, before not fetched", ctx: map[string]string{"github.repository": gascity, "needs.repin.outputs.moved": "true", "needs.repin.outputs.unfetched": "true"}, repin: true, dispatch: true, grace: true},
		{name: "push, pin unchanged from the parent's, before not fetched", ctx: map[string]string{"github.repository": gascity, "needs.repin.outputs.moved": "false", "needs.repin.outputs.unfetched": "true"}, repin: true, grace: true},
		{name: "push to a fork's main", ctx: map[string]string{"github.repository": fork, "needs.repin.outputs.moved": "true", "needs.repin.outputs.unfetched": "true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := evalGHIf(t, repin.If, tc.ctx)
			// dispatch and grace need repin: a skipped repin skips them.
			d := r && evalGHIf(t, dispatch.If, tc.ctx)
			g := r && evalGHIf(t, grace.If, tc.ctx)
			if r != tc.repin || d != tc.dispatch || g != tc.grace {
				t.Errorf("runs repin %v, dispatch %v, grace %v; want %v, %v, %v", r, d, g, tc.repin, tc.dispatch, tc.grace)
			}
		})
	}
	for _, tc := range []struct {
		name    string
		ctx     map[string]string
		comment bool
	}{
		{name: "same-repo PR", ctx: map[string]string{"github.repository": gascity, "github.event.pull_request.head.repo.full_name": gascity}, comment: true},
		{name: "fork PR", ctx: map[string]string{"github.repository": gascity, "github.event.pull_request.head.repo.full_name": fork}},
		{name: "PR in a fork of gascity", ctx: map[string]string{"github.repository": fork, "github.event.pull_request.head.repo.full_name": fork}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if c := evalGHIf(t, comment.If, tc.ctx); c != tc.comment {
				t.Errorf("runs comment %v, want %v", c, tc.comment)
			}
		})
	}
}

// TestWorkerEnvNotifyBeadsAppKeyReach pins who can read the key. The
// gastownhall-autofix App's private key reaches one step of one file, the
// mint at the head of a push-only job that never checks out the repository,
// so nothing that runs after it is repository code; the token it mints can
// only write beads' contents, which POST /repos/O/R/dispatches needs, and
// only the dispatch step reads it. The comment workflow names no
// environment and reads no secret (also TestPullRequestTargetWorkflowsHoldNoSecrets).
func TestWorkerEnvNotifyBeadsAppKeyReach(t *testing.T) {
	root := repoRoot(t)
	for _, path := range []string{notifyBeadsWorkflow, notifyBeadsPRWorkflow} {
		wf := loadNotifyBeads(t, path)
		if wf.Env["BEADS_REPO"] != notifyBeadsRepo || wf.Env["BEADS_AUTOSYNC"] != notifyBeadsAutosync || wf.Env["BEADS_EVENT"] != notifyBeadsEvent {
			t.Errorf("%s env %v, want BEADS_REPO=%s, BEADS_AUTOSYNC=%s, BEADS_EVENT=%s", path, wf.Env, notifyBeadsRepo, notifyBeadsAutosync, notifyBeadsEvent)
		}
		for id, job := range wf.Jobs {
			if env := job.Environment; (id == "dispatch") != (env != nil) || (env != nil && env != "autofix") {
				t.Errorf("%s job %s environment %v: only the dispatch job names one, autofix", path, id, env)
			}
		}
		// The key: two lines, both inputs of the mint step.
		var secretLines []string
		for _, l := range strings.Split(readFile(t, root, path), "\n") {
			if strings.Contains(l, "secrets.") && !strings.HasPrefix(strings.TrimSpace(l), "#") {
				secretLines = append(secretLines, strings.TrimSpace(l))
			}
		}
		var want []string
		if path == notifyBeadsWorkflow {
			want = []string{"app-id: ${{ secrets.AUTOFIX_APP_ID }}", "private-key: ${{ secrets.AUTOFIX_APP_PRIVATE_KEY }}"}
		}
		if !reflect.DeepEqual(secretLines, want) {
			t.Errorf("%s: lines naming a secret %q, want %q", path, secretLines, want)
		}
	}
	// The comment links the workflow that dispatches on the merge.
	if pr := loadNotifyBeads(t, notifyBeadsPRWorkflow); pr.Env["NOTIFY_WORKFLOW"] != filepath.Base(notifyBeadsWorkflow) {
		t.Errorf("%s NOTIFY_WORKFLOW %q, want %s", notifyBeadsPRWorkflow, pr.Env["NOTIFY_WORKFLOW"], filepath.Base(notifyBeadsWorkflow))
	}

	text := readFile(t, root, notifyBeadsWorkflow)
	dispatch := loadNotifyBeads(t, notifyBeadsWorkflow).Jobs["dispatch"]
	if dispatch.Permissions == nil || len(dispatch.Permissions) != 0 {
		t.Errorf("dispatch permissions %v, want {} (its only token is the App's)", dispatch.Permissions)
	}
	var ids []string
	for _, s := range dispatch.Steps {
		ids = append(ids, s.ID)
		if strings.HasPrefix(s.Uses, "actions/checkout@") || strings.HasPrefix(s.Uses, "./") {
			t.Errorf("dispatch step %q uses %s: the job holding the App key never checks out the repository", s.Name, s.Uses)
		}
	}
	if strings.Join(ids, ",") != "token,dispatch,fallback" {
		t.Fatalf("dispatch steps %v, want token (the mint), dispatch, fallback", ids)
	}
	mint := dispatch.Steps[0]
	if !strings.HasPrefix(mint.Uses, "actions/create-github-app-token@") || mint.If != "" {
		t.Errorf("first dispatch step uses %q if %q, want actions/create-github-app-token, unconditional (the job's if gates it)", mint.Uses, mint.If)
	}
	owner, repo, _ := strings.Cut(notifyBeadsRepo, "/")
	wantWith := map[string]any{
		"app-id":              "${{ secrets.AUTOFIX_APP_ID }}",
		"private-key":         "${{ secrets.AUTOFIX_APP_PRIVATE_KEY }}",
		"owner":               owner,
		"repositories":        repo,
		"permission-contents": "write",
	}
	if !reflect.DeepEqual(mint.With, wantWith) {
		t.Errorf("mint inputs %v, want exactly %v", mint.With, wantWith)
	}
	// The minted token reaches the dispatch step alone.
	if n := strings.Count(text, "steps.token.outputs.token"); n != 1 || dispatch.Steps[1].Env["GH_TOKEN"] != "${{ steps.token.outputs.token }}" {
		t.Errorf("the minted token is read %d times, dispatch GH_TOKEN %q; want once, by the dispatch step", n, dispatch.Steps[1].Env["GH_TOKEN"])
	}
	for _, s := range dispatch.Steps[1:] {
		if s.Uses != "" {
			t.Errorf("dispatch step %q uses %s after the mint; only inline shell may follow it", s.Name, s.Uses)
		}
	}
}

// TestWorkerEnvNotifyBeadsGrantsAndCheckouts: every token grant is per job
// (neither workflow grants anything), repin checks out the pushed commit and
// its parent without keeping the token, the comment job checks nothing out
// and runs one inline script with pull-requests: write alone, every ${{ }}
// reaches a script through env:, no push run cancels another (a canceled one
// is a dispatch never sent), and every action is pinned to a commit with the
// version it is.
func TestWorkerEnvNotifyBeadsGrantsAndCheckouts(t *testing.T) {
	root := repoRoot(t)
	push := loadNotifyBeads(t, notifyBeadsWorkflow)
	pr := loadNotifyBeads(t, notifyBeadsPRWorkflow)
	usesLine := regexp.MustCompile(`^\s*(?:- )?uses: (\S+)(.*)$`)
	pinned := regexp.MustCompile(`^[a-z0-9-]+/[a-z0-9-]+@[0-9a-f]{40}$`)
	version := regexp.MustCompile(`^ # v\d+(\.\d+)*$`)
	for path, wantUses := range map[string]int{notifyBeadsWorkflow: 3, notifyBeadsPRWorkflow: 0} {
		wf := loadNotifyBeads(t, path)
		if wf.Permissions == nil || len(wf.Permissions) != 0 {
			t.Errorf("%s permissions %v, want {} (each job grants its own)", path, wf.Permissions)
		}
		for id, job := range wf.Jobs {
			if job.RunsOn != "ubuntu-latest" {
				t.Errorf("%s job %s runs-on %q, want ubuntu-latest (GitHub-hosted, no Blacksmith minutes)", path, id, job.RunsOn)
			}
			for _, s := range job.Steps {
				if strings.Contains(s.Run, "${{") {
					t.Errorf("%s job %s step %q interpolates ${{ }} into its script; pass it through env:", path, id, s.Name)
				}
			}
		}
		seen := 0
		for _, l := range strings.Split(readFile(t, root, path), "\n") {
			m := usesLine.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			seen++
			if !pinned.MatchString(m[1]) || !version.MatchString(m[2]) {
				t.Errorf("%s: %q, want owner/repo@<40-hex sha> # vN", path, strings.TrimSpace(l))
			}
		}
		if seen != wantUses {
			t.Errorf("%s: %d uses: lines, want %d", path, seen, wantUses)
		}
	}

	// grace publishes one record at a time without canceling one in
	// flight (TestWorkerEnvNotifyBeadsGraceJob).
	if push.Concurrency != nil || push.Jobs["repin"].Concurrency != nil || push.Jobs["dispatch"].Concurrency != nil {
		t.Errorf("concurrency on the push side (workflow %v, repin %v, dispatch %v): no push run may cancel another",
			push.Concurrency, push.Jobs["repin"].Concurrency, push.Jobs["dispatch"].Concurrency)
	}
	repin := push.Jobs["repin"]
	if want := map[string]string{"contents": "read"}; !reflect.DeepEqual(repin.Permissions, want) {
		t.Errorf("repin permissions %v, want %v", repin.Permissions, want)
	}
	for _, k := range []string{"moved", "pin", "sha", "before", "unfetched"} {
		if want := "${{ steps.pin.outputs." + k + " }}"; repin.Outputs[k] != want {
			t.Errorf("repin output %s = %q, want %q", k, repin.Outputs[k], want)
		}
	}
	if len(repin.Outputs) != 5 {
		t.Errorf("repin outputs %v, want moved, pin, sha, before, unfetched", repin.Outputs)
	}
	if len(repin.Steps) != 2 || !strings.HasPrefix(repin.Steps[0].Uses, "actions/checkout@") || repin.Steps[1].Uses != "" {
		t.Fatalf("repin steps %+v, want a checkout and one inline script", repin.Steps)
	}
	// The pushed commit's parent stands in for a push with no usable before.
	if want := map[string]any{"persist-credentials": false, "fetch-depth": 2}; !reflect.DeepEqual(repin.Steps[0].With, want) {
		t.Errorf("repin checkout with %v, want %v", repin.Steps[0].With, want)
	}

	comment := pr.Jobs["comment"]
	if want := map[string]string{"pull-requests": "write"}; !reflect.DeepEqual(comment.Permissions, want) {
		t.Errorf("comment permissions %v, want %v alone", comment.Permissions, want)
	}
	if len(comment.Steps) != 1 || comment.Steps[0].Uses != "" || comment.Steps[0].ID != "comment" {
		t.Errorf("comment steps %+v, want one inline script (id comment): it checks nothing out", comment.Steps)
	}
	if comment.Concurrency["group"] != "worker-env-notify-beads-pr-${{ github.event.pull_request.number }}" || comment.Concurrency["cancel-in-progress"] != true {
		t.Errorf("comment concurrency %v, want one group per PR, cancel-in-progress (a newer head supersedes)", comment.Concurrency)
	}
	if s := notifyBeadsStep(t, pr, "comment", "comment"); s.Env["GH_TOKEN"] != "${{ github.token }}" || s.Env["MAIN_SHA"] != "${{ github.workflow_sha }}" {
		t.Errorf("comment step GH_TOKEN %q, MAIN_SHA %q; want the workflow token and the commit this file was read from (main)", s.Env["GH_TOKEN"], s.Env["MAIN_SHA"])
	}
}

// notifyBeadsOrigin builds a main history in a local repository: one commit
// per NAME=BUILD argument, each carrying that platforms/BUILD.bazel and the
// real tools/rbe/worker-env-drift, and writes each commit's sha to
// ORIGIN.NAME.sha.
const notifyBeadsOrigin = `#!/usr/bin/env bash
set -euo pipefail
origin=$1 drift=$2
shift 2
git init -q -b main "$origin"
mkdir -p "$origin/tools/rbe" "$origin/platforms"
cp "$drift" "$origin/tools/rbe/worker-env-drift"
chmod +x "$origin/tools/rbe/worker-env-drift"
for spec in "$@"; do
	name=${spec%%=*}
	cp "${spec#*=}" "$origin/platforms/BUILD.bazel"
	echo "$name" >"$origin/commit.txt"
	git -C "$origin" add -A
	git -C "$origin" commit -q -m "$name"
	git -C "$origin" rev-parse HEAD >"$origin.$name.sha"
done
`

// notifyBeadsCheckout checks HEAD out of ORIGIN into DIR as the repin job's
// actions/checkout does: that commit and its parent (fetch-depth 2), from a
// remote named origin.
const notifyBeadsCheckout = `#!/usr/bin/env bash
set -euo pipefail
origin=$1 dir=$2 head=$3
git init -q "$dir"
git -C "$dir" remote add origin "$origin"
git -C "$dir" fetch -q --no-tags --depth=2 origin "+$head:refs/remotes/origin/main"
git -C "$dir" checkout -q --detach "$head"
`

// notifyBeadsGitEnv keeps the user's and the system's git configuration out
// of the fixture repositories.
func notifyBeadsGitEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_AUTHOR_NAME=worker-env-notify-beads test",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=worker-env-notify-beads test",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
	}
}

func runNotifyBeadsFixture(t *testing.T, dir, script string, args ...string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runRBEScript(dir, notifyBeadsGitEnv(dir), path, args...); err != nil {
		t.Fatalf("fixture %v: %v\n%s", args, err, stderr)
	}
}

// TestWorkerEnvNotifyBeadsDetectsTheRepin runs the repin job's script in
// checkouts of a fixture main. It compares the pushed commit's pin with
// main's before the push (github.event.before), so a re-pin anywhere in a
// push of several commits (a merge-queue group) counts, and one moved back
// within the push does not. A push with no usable before (a new branch's
// all-zero one, none at all, or one it cannot fetch) compares with the
// commit's parent; a first commit compares with no pin. Its before output
// is the push's own before, for the grace record, never that parent.
func TestWorkerEnvNotifyBeadsDetectsTheRepin(t *testing.T) {
	root := repoRoot(t)
	wf := loadNotifyBeads(t, notifyBeadsWorkflow)
	step := notifyBeadsStep(t, wf, "repin", "pin")
	if step.Env["BEFORE"] != "${{ github.event.before }}" {
		t.Fatalf("repin step env BEFORE %q, want the push's before", step.Env["BEFORE"])
	}

	work := t.TempDir()
	pinA, pinB := sha256Pin("a\n"), sha256Pin("b\n")
	builds := map[string]string{
		"a":       buildWithPin(pinA),
		"a-docs":  "# The worker host.\n" + buildWithPin(pinA),
		"b":       buildWithPin(pinB),
		"b-again": buildWithPin(pinB),
		"a-back":  buildWithPin(pinA),
		"none":    "platform(name = \"rbe_worker\")\n",
	}
	// main: c1 a, c2 a (comment edited), c3 b (the re-pin), c4 b (another
	// change), c5 a (moved back), c6 no pin.
	order := []string{"a", "a-docs", "b", "b-again", "a-back", "none"}
	var specs []string
	for _, name := range order {
		path := filepath.Join(work, name+".bazel")
		writeDriftFile(t, path, builds[name])
		specs = append(specs, name+"="+path)
	}
	origin := filepath.Join(work, "origin")
	runNotifyBeadsFixture(t, work, notifyBeadsOrigin, append([]string{origin, filepath.Join(root, rbeWorkerEnvDrift)}, specs...)...)
	sha := map[string]string{}
	for _, name := range order {
		sha[name] = strings.TrimSpace(readFile(t, work, "origin."+name+".sha"))
	}
	const zero = "0000000000000000000000000000000000000000"
	unknown := strings.Repeat("e", 40)

	// mainBefore: repin's before output, main before the push for the grace
	// record: the push's own before when it names a commit, fetched or not,
	// else none; never the parent the pin is compared with in its stead.
	// warn: repin could not fetch that before, so it compared with the
	// parent, warned and says unfetched=true (moved is a guess then).
	for _, tc := range []struct {
		name, before, head string
		moved              string
		pin                string
		mainBefore         string
		fail, warn         bool
	}{
		{name: "comment only", before: sha["a"], head: sha["a-docs"], moved: "false", pin: pinA, mainBefore: sha["a"]},
		{name: "re-pin", before: sha["a-docs"], head: sha["b"], moved: "true", pin: pinB, mainBefore: sha["a-docs"]},
		{name: "re-pin before the last commit of the push", before: sha["a-docs"], head: sha["b-again"], moved: "true", pin: pinB, mainBefore: sha["a-docs"]},
		{name: "moved and moved back within the push", before: sha["a-docs"], head: sha["a-back"], moved: "false", pin: pinA, mainBefore: sha["a-docs"]},
		{name: "new branch, parent pins another host", before: zero, head: sha["b"], moved: "true", pin: pinB},
		{name: "new branch, parent pins the same host", before: zero, head: sha["b-again"], moved: "false", pin: pinB},
		{name: "no before", before: "", head: sha["b"], moved: "true", pin: pinB},
		{name: "before not fetchable, parent pins the same host", before: unknown, head: sha["b-again"], moved: "false", pin: pinB, mainBefore: unknown, warn: true},
		{name: "before not fetchable, parent pins another host", before: unknown, head: sha["b"], moved: "true", pin: pinB, mainBefore: unknown, warn: true},
		{name: "first commit", before: zero, head: sha["a"], moved: "true", pin: pinA},
		{name: "pin removed", before: sha["a-back"], head: sha["none"], fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			co := filepath.Join(dir, "checkout")
			runNotifyBeadsFixture(t, dir, notifyBeadsCheckout, origin, co, tc.head)
			gh := newNotifyBeadsGH(t)
			output := filepath.Join(dir, "output")
			runner := gh.env()
			for _, kv := range notifyBeadsGitEnv(dir) {
				k, v, _ := strings.Cut(kv, "=")
				if k != "PATH" {
					runner[k] = v
				}
			}
			runner["RUNNER_TEMP"] = dir
			runner["GITHUB_OUTPUT"] = output
			runner["GITHUB_STEP_SUMMARY"] = filepath.Join(dir, "summary")
			env := notifyBeadsEnv(t, wf, step, map[string]string{"github.event.before": tc.before}, runner)
			out, err := runWorkflowStepScript(t, co, step.Run, env)
			if (err != nil) != tc.fail {
				t.Fatalf("exit %v, want failure %v\n%s", err, tc.fail, out)
			}
			moved, _ := readStepOutput(t, output, "moved")
			if tc.fail {
				if moved == "true" || !strings.Contains(out, "::error") {
					t.Errorf("a removed pin must fail with an ::error and no moved=true (moved %q)\n%s", moved, out)
				}
				return
			}
			pin, _ := readStepOutput(t, output, "pin")
			head, _ := readStepOutput(t, output, "sha")
			mainBefore, found := readStepOutput(t, output, "before")
			if moved != tc.moved || pin != tc.pin || head != tc.head || mainBefore != tc.mainBefore || !found {
				t.Errorf("moved %q pin %q sha %q before %q (written %v), want %q %q %q %q\n%s", moved, pin, head, mainBefore, found, tc.moved, tc.pin, tc.head, tc.mainBefore, out)
			}
			if unfetched, _ := readStepOutput(t, output, "unfetched"); unfetched != strconv.FormatBool(tc.warn) {
				t.Errorf("unfetched %q, want %v\n%s", unfetched, tc.warn, out)
			}
			if warned := strings.Contains(out, "::warning"); warned != tc.warn {
				t.Errorf("::warning printed %v, want %v\n%s", warned, tc.warn, out)
			}
			gh.checkNoUnexpected()
			if calls := gh.lines(gh.log); len(calls) > 0 {
				t.Errorf("repin calls gh %v; it reads git alone", calls)
			}
		})
	}
}

// TestWorkerEnvNotifyBeadsDispatch runs the dispatch step against a stub
// gh: it posts beads exactly the contract's event and payload with the
// minted token, retries a 5xx or no answer at all a few times with a pause,
// fails at once on a 4xx (the same answer every time), and refuses a sha or
// pin that is not one before calling anything.
func TestWorkerEnvNotifyBeadsDispatch(t *testing.T) {
	wf := loadNotifyBeads(t, notifyBeadsWorkflow)
	step := notifyBeadsStep(t, wf, "dispatch", "dispatch")
	sha, pin := strings.Repeat("0123456789", 4), sha256Pin("b\n")
	for _, tc := range []struct {
		name, sha, pin string
		statuses       []string
		ok             bool
		calls, sleeps  int
	}{
		{name: "accepted", sha: sha, pin: pin, statuses: []string{"204"}, ok: true, calls: 1},
		{name: "5xx, then accepted", sha: sha, pin: pin, statuses: []string{"502", "503", "204"}, ok: true, calls: 3, sleeps: 2},
		{name: "no answer, then accepted", sha: sha, pin: pin, statuses: []string{"none", "204"}, ok: true, calls: 2, sleeps: 1},
		{name: "5xx every time", sha: sha, pin: pin, statuses: []string{"500", "502", "503", "504", "500", "204"}, calls: 5, sleeps: 4},
		{name: "App not installed on beads", sha: sha, pin: pin, statuses: []string{"404", "204"}, calls: 1},
		{name: "token lacks contents:write", sha: sha, pin: pin, statuses: []string{"403", "204"}, calls: 1},
		{name: "payload refused", sha: sha, pin: pin, statuses: []string{"422", "204"}, calls: 1},
		{name: "no pin", sha: sha, pin: "none"},
		{name: "no sha", sha: "", pin: pin},
		{name: "a sha that is not one", sha: "-x" + sha[2:], pin: pin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := newNotifyBeadsGH(t)
			gh.write(gh.statuses, strings.Join(tc.statuses, "\n")+"\n")
			dir := t.TempDir()
			runner := gh.env()
			runner["RUNNER_TEMP"] = dir
			runner["GITHUB_STEP_SUMMARY"] = filepath.Join(dir, "summary")
			env := notifyBeadsEnv(t, wf, step, map[string]string{
				"steps.token.outputs.token": notifyBeadsToken,
				"needs.repin.outputs.sha":   tc.sha,
				"needs.repin.outputs.pin":   tc.pin,
			}, runner)
			out, err := runWorkflowStepScript(t, dir, step.Run, env)
			if (err == nil) != tc.ok {
				t.Fatalf("exit %v, want success %v\n%s", err, tc.ok, out)
			}
			gh.checkNoUnexpected()
			calls := gh.lines(gh.log)
			if len(calls) != tc.calls {
				t.Fatalf("%d gh calls %q, want %d\n%s", len(calls), calls, tc.calls, out)
			}
			if sleeps := gh.lines(gh.sleeps); len(sleeps) != tc.sleeps {
				t.Errorf("slept %v, want %d pauses", sleeps, tc.sleeps)
			}
			for _, tok := range gh.lines(gh.tokens) {
				if tok != notifyBeadsToken {
					t.Errorf("gh ran with GH_TOKEN %q, want the minted token", tok)
				}
			}
			for _, c := range calls {
				if !strings.HasPrefix(c, "api --include --method POST repos/"+notifyBeadsRepo+"/dispatches --input ") {
					t.Errorf("gh call %q, want api --include --method POST repos/%s/dispatches --input FILE", c, notifyBeadsRepo)
				}
			}
			want := map[string]any{"event_type": notifyBeadsEvent, "client_payload": map[string]any{"sha": tc.sha, "pin": tc.pin}}
			for _, body := range gh.saved("dispatch") {
				var got map[string]any
				if err := json.Unmarshal([]byte(body), &got); err != nil || !reflect.DeepEqual(got, want) {
					t.Errorf("dispatch payload %s (%v), want %v", body, err, want)
				}
			}
		})
	}
}

// TestNotifyBeadsGHStubRunsUnderBSDTools: the gh stub runs on the macOS
// runners too (make test-mac runs scripts/), where wc pads its count to eight
// columns and sed -i takes its next argument as the backup suffix. Shims with
// that BSD behavior run first on PATH here, so Linux pins the portability.
func TestNotifyBeadsGHStubRunsUnderBSDTools(t *testing.T) {
	realWC, err := exec.LookPath("wc")
	if err != nil {
		t.Skipf("no wc on PATH: %v", err)
	}
	realSed, err := exec.LookPath("sed")
	if err != nil {
		t.Skipf("no sed on PATH: %v", err)
	}
	bsd := t.TempDir()
	for name, content := range map[string]string{
		"wc":  "#!/bin/sh\n'" + realWC + `' "$@" | awk '{ printf "%8d", $1; for (i = 2; i <= NF; i++) printf " %s", $i; printf "\n" }'` + "\n",
		"sed": "#!/bin/sh\nif [ \"$1\" = -i ]; then echo \"sed: 1: \\\"$3\\\": invalid command code\" >&2; exit 1; fi\nexec '" + realSed + "' \"$@\"\n",
	} {
		if err := os.WriteFile(filepath.Join(bsd, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gh := newNotifyBeadsGH(t)
	gh.write(gh.statuses, "204\n500\n")
	input := filepath.Join(t.TempDir(), "payload.json")
	gh.write(input, `{"event_type":"x"}`)
	env := gh.env()
	env["PATH"] = bsd + string(os.PathListSeparator) + env["PATH"]
	for i, wantOK := range []bool{true, false} {
		out, err := runWorkflowStepScript(t, t.TempDir(), "gh api --include --method POST repos/o/r/dispatches --input '"+input+"'\n", env)
		if (err == nil) != wantOK {
			t.Fatalf("call %d: exit %v, want success %v\n%s", i+1, err, wantOK, out)
		}
	}
	gh.checkNoUnexpected()
	if saved := gh.saved("dispatch"); len(saved) != 2 {
		t.Errorf("stub saved %d dispatch payloads, want 2", len(saved))
	}
	if left := gh.lines(gh.statuses); len(left) != 0 {
		t.Errorf("statuses left unconsumed: %q", left)
	}
}

// TestWorkerEnvNotifyBeadsFallback: when the mint or the dispatch fails, the
// job fails and says how to start beads' autosync by hand.
func TestWorkerEnvNotifyBeadsFallback(t *testing.T) {
	wf := loadNotifyBeads(t, notifyBeadsWorkflow)
	step := notifyBeadsStep(t, wf, "dispatch", "fallback")
	if step.If != "failure()" {
		t.Errorf("fallback if %q, want failure()", step.If)
	}
	if _, ok := step.Env["GH_TOKEN"]; ok {
		t.Errorf("fallback step reads GH_TOKEN; it only prints")
	}
	dir := t.TempDir()
	gh := newNotifyBeadsGH(t)
	runner := gh.env()
	runner["RUNNER_TEMP"] = dir
	runner["GITHUB_STEP_SUMMARY"] = filepath.Join(dir, "summary")
	pin := sha256Pin("b\n")
	env := notifyBeadsEnv(t, wf, step, map[string]string{
		"needs.repin.outputs.sha": strings.Repeat("ab", 20),
		"needs.repin.outputs.pin": pin,
	}, runner)
	out, err := runWorkflowStepScript(t, dir, step.Run, env)
	if err != nil {
		t.Fatalf("fallback step failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "::error") || !strings.Contains(out, notifyBeadsFallback) || !strings.Contains(out, pin) {
		t.Errorf("fallback output lacks an ::error naming %s and the pin:\n%s", notifyBeadsFallback, out)
	}
	if s := readFile(t, dir, "summary"); !strings.Contains(s, notifyBeadsFallback) {
		t.Errorf("step summary lacks %s:\n%s", notifyBeadsFallback, s)
	}
	if calls := gh.lines(gh.log); len(calls) > 0 {
		t.Errorf("fallback calls gh %v", calls)
	}
}

// notifyBeadsComment is a comments-API entry for the comment tests.
type notifyBeadsComment struct {
	ID   int `json:"id"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Body string `json:"body"`
}

func notifyBeadsComments(t *testing.T, c ...notifyBeadsComment) string {
	t.Helper()
	if c == nil {
		c = []notifyBeadsComment{}
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func notifyBeadsCommentBy(id int, login, body string) notifyBeadsComment {
	c := notifyBeadsComment{ID: id, Body: body}
	c.User.Login = login
	return c
}

// notifyBeadsFiles is a pulls-files-API answer: filename, status pairs.
func notifyBeadsFiles(t *testing.T, fs ...string) string {
	t.Helper()
	entries := []map[string]string{}
	for i := 0; i+1 < len(fs); i += 2 {
		entries = append(entries, map[string]string{"filename": fs[i], "status": fs[i+1]})
	}
	b, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const (
	notifyBeadsPR      = "17"
	notifyBeadsHeadSHA = "c0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ff"
	notifyBeadsMainSHA = "0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d"
	notifyBeadsBaseSHA = "ba5eba5eba5eba5eba5eba5eba5eba5eba5eba5e"
)

// runNotifyBeadsComment runs the comment step for PR 17 of
// gastownhall/gascity against gh, which serves its file list, comments and
// BUILD files, and returns its output and error.
func runNotifyBeadsComment(t *testing.T, gh *notifyBeadsGH) (string, error) {
	t.Helper()
	wf := loadNotifyBeads(t, notifyBeadsPRWorkflow)
	step := notifyBeadsStep(t, wf, "comment", "comment")
	runner := gh.env()
	runner["RUNNER_TEMP"] = t.TempDir()
	runner["GITHUB_REPOSITORY"] = "gastownhall/gascity"
	runner["GITHUB_SERVER_URL"] = "https://github.com"
	runner["GITHUB_WORKFLOW"] = "rbe worker-env re-pin notice"
	runner["GITHUB_WORKFLOW_REF"] = "gastownhall/gascity/" + notifyBeadsPRWorkflow + "@refs/heads/main"
	env := notifyBeadsEnv(t, wf, step, map[string]string{
		"github.token":                       "ghs_workflow",
		"github.event.pull_request.number":   notifyBeadsPR,
		"github.event.pull_request.head.sha": notifyBeadsHeadSHA,
		"github.workflow_sha":                notifyBeadsMainSHA,
	}, runner)
	// No checkout: the step runs in an empty directory.
	return runWorkflowStepScript(t, t.TempDir(), step.Run, env)
}

// TestWorkerEnvNotifyBeadsPRComment runs the comment step against a stub gh.
// The PR is data: its file list says whether it changes
// platforms/BUILD.bazel at all (a PR that does not leaves main's pin as it
// is, whatever an old head pins), and only then is its head's BUILD read,
// raw, at the event's head sha; main's is read at the commit the workflow
// file came from. A head that pins another host than main is compared with
// its merge base too: a PR that edits BUILD on a branch from before main's
// last re-pin pins its merge base's host, which merging does not bring
// back. A PR that moves the pin gets one comment, posted once and updated in
// place; one that no longer does has that comment say so; no other PR gets
// one. Only github-actions[bot]'s marker comment is edited.
func TestWorkerEnvNotifyBeadsPRComment(t *testing.T) {
	pinMain, pinNew, pinOld := sha256Pin("a\n"), sha256Pin("b\n"), sha256Pin("o\n")
	const (
		bot   = "github-actions[bot]"
		human = "someone"
	)
	repin := notifyBeadsFiles(t, "go.mod", "modified", rbeWorkerEnvManifest, "modified", rbeWorkerPlatformBuild, "modified")
	buildOnly := notifyBeadsFiles(t, rbeWorkerPlatformBuild, "modified")
	manifestOnly := notifyBeadsFiles(t, rbeWorkerEnvManifest, "modified")
	ours := notifyBeadsCommentBy(42, bot, notifyBeadsMarker+"\nan earlier version")
	for _, tc := range []struct {
		name, files, comments string
		// head and base: the BUILD at the PR head and at its merge base
		// ("" serves none).
		head, base string
		// write is "" (none), "POST" (a new comment) or "PATCH 42".
		write string
		// moves: the comment says merging re-pins, from main's pin to
		// pinNew; else it says the PR no longer moves the pin.
		moves bool
		// fetchedHead: the head's BUILD was read (main's always is);
		// compared: the merge base was asked for and its BUILD read.
		fetchedHead, compared bool
	}{
		{name: "manifest only, BUILD untouched", files: manifestOnly, comments: notifyBeadsComments(t)},
		{name: "old branch: head pins another host, PR leaves BUILD alone", files: manifestOnly, head: buildWithPin(pinNew), comments: notifyBeadsComments(t)},
		{name: "re-pin, first push", files: repin, head: buildWithPin(pinNew), base: buildWithPin(pinMain), comments: notifyBeadsComments(t, notifyBeadsCommentBy(7, human, "LGTM")), write: "POST", moves: true, fetchedHead: true, compared: true},
		{name: "re-pin, comment already there", files: repin, head: buildWithPin(pinNew), base: buildWithPin(pinMain), comments: notifyBeadsComments(t, notifyBeadsCommentBy(7, human, "LGTM"), ours), write: "PATCH 42", moves: true, fetchedHead: true, compared: true},
		{name: "re-pin, only a human's comment carries the marker", files: repin, head: buildWithPin(pinNew), base: buildWithPin(pinMain), comments: notifyBeadsComments(t, notifyBeadsCommentBy(43, human, notifyBeadsMarker+"\nquoted")), write: "POST", moves: true, fetchedHead: true, compared: true},
		{name: "re-pin on a branch from before main's last re-pin", files: repin, head: buildWithPin(pinNew), base: buildWithPin(pinOld), comments: notifyBeadsComments(t), write: "POST", moves: true, fetchedHead: true, compared: true},
		{name: "BUILD edited on a branch from before main's last re-pin", files: buildOnly, head: "# docs\n" + buildWithPin(pinOld), base: buildWithPin(pinOld), comments: notifyBeadsComments(t), fetchedHead: true, compared: true},
		{name: "re-pin dropped, BUILD still edited, on an old branch", files: buildOnly, head: "# docs\n" + buildWithPin(pinOld), base: buildWithPin(pinOld), comments: notifyBeadsComments(t, ours), write: "PATCH 42", fetchedHead: true, compared: true},
		{name: "BUILD edited, pin unchanged", files: buildOnly, head: "# docs\n" + buildWithPin(pinMain), comments: notifyBeadsComments(t), fetchedHead: true},
		{name: "re-pin reverted within the PR", files: buildOnly, head: buildWithPin(pinMain), comments: notifyBeadsComments(t, ours), write: "PATCH 42", fetchedHead: true},
		{name: "BUILD change dropped, manifest still touched", files: manifestOnly, comments: notifyBeadsComments(t, ours), write: "PATCH 42"},
		{name: "BUILD removed", files: notifyBeadsFiles(t, rbeWorkerPlatformBuild, "removed"), comments: notifyBeadsComments(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh := newNotifyBeadsGH(t)
			gh.write(gh.files, tc.files)
			gh.write(gh.comments, tc.comments)
			gh.build(notifyBeadsMainSHA, buildWithPin(pinMain))
			if tc.head != "" {
				gh.build(notifyBeadsHeadSHA, tc.head)
			}
			if tc.base != "" {
				gh.mergeBase(notifyBeadsBaseSHA, tc.base)
			}
			out, err := runNotifyBeadsComment(t, gh)
			if err != nil {
				t.Fatalf("comment step failed: %v\n%s", err, out)
			}
			gh.checkNoUnexpected()
			for _, tok := range gh.lines(gh.tokens) {
				if tok != "ghs_workflow" {
					t.Errorf("gh ran with GH_TOKEN %q, want the workflow token", tok)
				}
			}
			for _, c := range gh.lines(gh.log) {
				if !strings.Contains(c, "repos/gastownhall/gascity/") ||
					strings.Contains(c, "/pulls/") && !strings.Contains(c, "/pulls/"+notifyBeadsPR+"/files") {
					t.Errorf("gh call %q is not about PR %s of gastownhall/gascity", c, notifyBeadsPR)
				}
			}
			wantRefs := []string{notifyBeadsMainSHA}
			var wantCompare []string
			if tc.fetchedHead {
				wantRefs = append(wantRefs, notifyBeadsHeadSHA)
			}
			if tc.compared {
				wantRefs = append(wantRefs, notifyBeadsBaseSHA)
				wantCompare = []string{notifyBeadsMainSHA + "..." + notifyBeadsHeadSHA}
			}
			if refs := gh.lines(filepath.Join(gh.dir, "contents")); !reflect.DeepEqual(refs, wantRefs) {
				t.Errorf("BUILD read at %v, want %v (main's; the head's only when the PR changes it; the merge base's only when the head pins another host than main)", refs, wantRefs)
			}
			if compares := gh.lines(filepath.Join(gh.dir, "compare")); !reflect.DeepEqual(compares, wantCompare) {
				t.Errorf("compared %v, want %v", compares, wantCompare)
			}
			writes := gh.lines(filepath.Join(gh.dir, "writes"))
			var want []string
			switch tc.write {
			case "POST":
				want = []string{"POST repos/gastownhall/gascity/issues/" + notifyBeadsPR + "/comments"}
			case "PATCH 42":
				want = []string{"PATCH repos/gastownhall/gascity/issues/comments/42"}
			}
			if !reflect.DeepEqual(writes, want) {
				t.Fatalf("comment writes %v, want %v\n%s", writes, want, out)
			}
			if tc.write == "" {
				return
			}
			var body struct {
				Body string `json:"body"`
			}
			saved := gh.saved("write")
			if len(saved) != 1 {
				t.Fatalf("%d comment bodies, want 1", len(saved))
			}
			if err := json.Unmarshal([]byte(saved[0]), &body); err != nil {
				t.Fatalf("comment input %s: %v", saved[0], err)
			}
			if !strings.HasPrefix(body.Body, notifyBeadsMarker+"\n") || strings.Contains(body.Body, "${") {
				t.Errorf("comment body does not start with the marker, or has an unexpanded variable:\n%s", body.Body)
			}
			selfURL := "https://github.com/gastownhall/gascity/actions/workflows/" + filepath.Base(notifyBeadsPRWorkflow)
			if !strings.Contains(body.Body, selfURL) {
				t.Errorf("comment does not link %s:\n%s", selfURL, body.Body)
			}
			if tc.moves {
				for _, w := range []string{
					"re-pins rbe-west's OSS pool for " + notifyBeadsRepo,
					"from `" + pinMain + "` (main) to `" + pinNew + "`",
					notifyBeadsEvent,
					"https://github.com/gastownhall/gascity/actions/workflows/" + filepath.Base(notifyBeadsWorkflow),
					"https://github.com/" + notifyBeadsRepo + "/actions/workflows/" + notifyBeadsAutosync,
					// True whether or not beads auto-merges its sync PR yet.
					"within a minute or two of the merge", "as soon as its CI passes (auto-merge) or after beads' automated review",
					"`Bazel / execution mode`", "fully uncached", "about an hour, sometimes more",
				} {
					if !strings.Contains(body.Body, w) {
						t.Errorf("re-pin comment lacks %q:\n%s", w, body.Body)
					}
				}
				if strings.Contains(body.Body, "auto-merges") {
					t.Errorf("re-pin comment promises beads auto-merges its sync PR, which needs beads' opt-in:\n%s", body.Body)
				}
			} else if !strings.Contains(body.Body, "no longer moves the worker-env pin") || strings.Contains(body.Body, pinNew) {
				t.Errorf("retracted comment does not say the PR no longer moves the pin:\n%s", body.Body)
			}
		})
	}
}

// TestWorkerEnvNotifyBeadsPRReadsPinsAsWorkerEnvDrift: the comment step,
// which checks nothing out, reads a BUILD's pin with its own copy of
// tools/rbe/worker-env-drift's sed. For each head BUILD, the step must reach
// worker-env-drift pin's answer: a pin other than main's is commented on
// with exactly that pin, main's pin or none is not, and a BUILD
// worker-env-drift refuses fails the step.
func TestWorkerEnvNotifyBeadsPRReadsPinsAsWorkerEnvDrift(t *testing.T) {
	root := repoRoot(t)
	pinMain, pinNew, pinOther := sha256Pin("a\n"), sha256Pin("b\n"), sha256Pin("c\n")
	for name, head := range map[string]string{
		"pin":                    buildWithPin(pinNew),
		"main's pin":             buildWithPin(pinMain),
		"other indentation":      "platform(\n  name = \"rbe_worker\",\n  exec_properties = {\n    \"worker-env\": \"" + pinNew + "\",\n  },\n)\n",
		"no pin":                 "platform(name = \"rbe_worker\")\n",
		"two pins":               buildWithPin(pinNew) + buildWithPin(pinOther),
		"upper-case hex":         buildWithPin(strings.ToUpper(pinNew)),
		"no trailing comma":      strings.Replace(buildWithPin(pinNew), pinNew+"\",", pinNew+"\"", 1),
		"short digest":           buildWithPin(pinNew[:len(pinNew)-1]),
		"pin in a comment":       "# \"worker-env\": \"" + pinNew + "\",\n" + buildWithPin(pinMain),
		"pin after other fields": "platform(\n    name = \"rbe_worker\",\n    exec_properties = {\n        \"OSFamily\": \"Linux\",\n        \"worker-env\": \"" + pinNew + "\",\n    },\n)\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "BUILD.bazel")
			writeDriftFile(t, path, head)
			drift, _, driftErr := runRBEScript(t.TempDir(), []string{"PATH=/usr/bin:/bin"}, filepath.Join(root, rbeWorkerEnvDrift), "pin", path)
			drift = strings.TrimSpace(drift)

			gh := newNotifyBeadsGH(t)
			gh.write(gh.files, notifyBeadsFiles(t, rbeWorkerPlatformBuild, "modified"))
			gh.write(gh.comments, notifyBeadsComments(t))
			gh.build(notifyBeadsMainSHA, buildWithPin(pinMain))
			gh.build(notifyBeadsHeadSHA, head)
			gh.mergeBase(notifyBeadsBaseSHA, buildWithPin(pinMain))
			out, err := runNotifyBeadsComment(t, gh)
			gh.checkNoUnexpected()
			if (err != nil) != (driftErr != nil) {
				t.Fatalf("step error %v, worker-env-drift pin error %v: they must agree\n%s", err, driftErr, out)
			}
			if driftErr != nil {
				return
			}
			saved := gh.saved("write")
			if drift == "none" || drift == pinMain {
				if len(saved) != 0 {
					t.Errorf("worker-env-drift reads %s, which moves nothing, but the step commented:\n%v", drift, saved)
				}
				return
			}
			if len(saved) != 1 || !strings.Contains(saved[0], "to `"+drift+"`") {
				t.Errorf("worker-env-drift reads %s; want one comment naming it, got %v\n%s", drift, saved, out)
			}
		})
	}
}

// TestAutofixEnvironmentJobsAreListed: the autofix environment holds the
// gastownhall-autofix App's private key. Its deployment-branch policy admits
// main only, which also admits a pull_request_target run (its ref is the
// base branch), so each job that names it is listed and reviewed here, and
// none of them is in a pull_request_target workflow
// (TestPullRequestTargetWorkflowsHoldNoSecrets).
func TestAutofixEnvironmentJobsAreListed(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(repoRoot(t), ".github", "workflows", "*.y*ml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob workflows: %v (%d files)", err, len(paths))
	}
	var got []string
	for _, path := range paths {
		var wf struct {
			Jobs map[string]struct {
				Environment any `yaml:"environment"`
			} `yaml:"jobs"`
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(data, &wf); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for id, job := range wf.Jobs {
			name := job.Environment
			if m, ok := name.(map[string]any); ok {
				name = m["name"]
			}
			if name == "autofix" {
				got = append(got, filepath.Base(path)+":"+id)
			}
		}
	}
	sort.Strings(got)
	if want := []string{"docs-autofix.yml:autofix", "worker-env-notify-beads.yml:dispatch"}; !reflect.DeepEqual(got, want) {
		t.Errorf("jobs naming the autofix environment %v, want %v", got, want)
	}
}
