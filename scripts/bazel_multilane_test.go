package scripts_test

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// bazel.yml (rbe-west plan W1) runs beside bazel-test.yml as a preview. These
// tests keep the two workflows' remote cache keys equal, keep the preview off
// the required check names, pin its triggers, lanes, permissions and gate,
// and keep .github/actions/setup-bazel a byte copy of beads' composite.

const (
	bazelMultiLaneWorkflow = ".github/workflows/bazel.yml"
	setupBazelDir          = ".github/actions/setup-bazel"
)

// setupBazelBeadsDigests: sha256 of beads' .github/actions/setup-bazel files
// (beads main b8a9545c, #7174: R4's -Xmx4g client heap). A change here is a
// change in beads first: copy all three files from beads and update these
// digests in the same PR.
var setupBazelBeadsDigests = map[string]string{
	"action.yml":         "9dba170b11c0c2acbde181715e9a801d95972c5f1a74aa5ffaa12f3a12ab886d",
	"fork-credential.sh": "abd68bbacb42fa5c7a71d06aa652bcf0f2fbe870f00b13b51650fe95c3bef59e",
	"write-bazelrc.sh":   "ffd2f3ebca5a449e12db342c143b9d082cd1d3d5ab7abc8fac84475d5ed56550",
}

func TestSetupBazelIsBeadsByteCopy(t *testing.T) {
	root := repoRoot(t)
	for name, want := range setupBazelBeadsDigests {
		got := fmt.Sprintf("%x", sha256.Sum256([]byte(readFile(t, root, setupBazelDir+"/"+name))))
		if got != want {
			t.Errorf("%s/%s sha256 %s, want %s (beads' copy): change beads' composite first, then copy it here", setupBazelDir, name, got, want)
		}
	}
	// bazel-test.yml still runs tools/rbe/fork-credential.sh.
	if readFile(t, root, setupBazelDir+"/fork-credential.sh") != readFile(t, root, rbeForkCredential) {
		t.Errorf("%s/fork-credential.sh and %s differ; both are byte copies of beads'", setupBazelDir, rbeForkCredential)
	}
}

// The composite is beads', but .bazelversion is gascity's: setup-bazel must
// pin a sha256 for this repository's Bazel on both architectures, or every
// lane fails at "Install Bazelisk" (ported from beads'
// TestBazelRestoredCachesAreVerified).
func TestSetupBazelPinsThisBazelVersion(t *testing.T) {
	root := repoRoot(t)
	version := strings.TrimSpace(readFile(t, root, ".bazelversion"))
	var action struct {
		Runs struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, root, setupBazelDir+"/action.yml")), &action); err != nil {
		t.Fatalf("parse %s/action.yml: %v", setupBazelDir, err)
	}
	install := ""
	for _, step := range action.Runs.Steps {
		if step.Name == "Install Bazelisk" {
			install = step.Run
		}
	}
	if install == "" {
		t.Fatalf("%s/action.yml has no Install Bazelisk step", setupBazelDir)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		pin := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(version+"/"+arch) + `\) bazel_sha=[0-9a-f]{64} ;;$`)
		if !pin.MatchString(install) {
			t.Errorf("setup-bazel pins no sha256 for Bazel %s (.bazelversion) on %s; add it in beads' composite and copy it here", version, arch)
		}
	}
	for _, want := range []string{
		`echo "BAZELISK_HOME=$RUNNER_TEMP/bazelisk-home"`,
		`echo "BAZELISK_VERIFY_SHA256=$bazel_sha"`,
		`echo "BAZEL_CI_BAZEL_SHA256=$bazel_sha"`,
		`bazel_version="$(tr -d '[:space:]' < .bazelversion)"`,
	} {
		if !strings.Contains(install, want) {
			t.Errorf("setup-bazel Install Bazelisk lacks %q", want)
		}
	}
	if strings.Contains(install, "bazel-ci-cache") {
		t.Errorf("setup-bazel puts Bazelisk's home in the runner cache; the Bazel binary must never be restored from it")
	}
}

// bazelRCFlagValue returns the value of the last .bazelrc line "<prefix>=<value>"
// (e.g. prefix "test:ci --test_env=PATH"), or "".
func bazelRCFlagValue(rc, prefix string) string {
	m := regexp.MustCompile(`(?m)^`+regexp.QuoteMeta(prefix)+`=(\S+)\s*$`).FindAllStringSubmatch(rc, -1)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1][1]
}

// --config=ci must give tests the same PATH bazel-test.yml's runs do (the
// key input), and the tagged-suite configs bazel-test.yml's flags, or the two
// workflows stop sharing remote cache entries. Either source may hold a
// value: today bazel-test.yml writes its lines to .bazelrc.local; once
// gascity #6993 lands they are committed (an unconditional pinned test PATH,
// build:remote-exec transport flags, which setup-bazel's --config=remote-exec
// picks up), and --config=ci's copies become redundant but must still agree.
func TestBazelCIConfigMatchesBazelTestRC(t *testing.T) {
	root := repoRoot(t)
	rc := readFile(t, root, ".bazelrc")
	legacy := readFile(t, root, bazelTestWorkflow)

	legacyPath := ""
	if m := regexp.MustCompile(`echo 'test --test_env=PATH=([^']+)'`).FindStringSubmatch(legacy); m != nil {
		legacyPath = m[1]
	} else {
		legacyPath = bazelRCFlagValue(rc, "test --test_env=PATH")
	}
	if legacyPath == "" {
		t.Fatalf("neither %s's .bazelrc.local lines nor .bazelrc pin a test PATH (test --test_env=PATH=...)", bazelTestWorkflow)
	}
	ciPath := bazelRCFlagValue(rc, "test:ci --test_env=PATH")
	if ciPath == "" {
		ciPath = bazelRCFlagValue(rc, "test --test_env=PATH")
	}
	if ciPath != legacyPath {
		t.Errorf("--config=ci tests see PATH %q, bazel-test.yml's %q; the PATH is a key input, keep them equal", ciPath, legacyPath)
	}

	// 2 vCPU clients: minimal downloads and 64 actions in flight, from
	// --config=ci or from the remote-exec config setup-bazel enables.
	for _, flag := range []string{"--remote_download_minimal", "--jobs=64"} {
		if !strings.Contains(rc, "\nbuild:ci "+flag+"\n") && !strings.Contains(rc, "\nbuild:remote-exec "+flag+"\n") {
			t.Errorf(".bazelrc sets %s in neither build:ci nor build:remote-exec", flag)
		}
	}

	for _, c := range []struct{ config, legacyFlags string }{
		{"acceptance", "--define=gotags=acceptance_a --test_timeout=1100"},
		{"integration", "--define=gotags=integration --test_timeout=1100"},
	} {
		if !strings.Contains(legacy, c.legacyFlags) {
			t.Errorf("%s no longer passes %q", bazelTestWorkflow, c.legacyFlags)
		}
		for _, f := range strings.Fields(c.legacyFlags) {
			if !strings.Contains(rc, "\ntest:"+c.config+" "+f+"\n") {
				t.Errorf(".bazelrc lacks test:%s %s", c.config, f)
			}
		}
	}
	for _, line := range []string{
		"test:ci --flaky_test_attempts=1",
		"test:sole-run --nocache_test_results",
		"test:sole-run --experimental_remote_cache_eviction_retries=0",
	} {
		if !strings.Contains(rc, "\n"+line+"\n") {
			t.Errorf(".bazelrc lacks %q", line)
		}
	}
}

type multiLaneStep struct {
	ID   string            `yaml:"id"`
	Name string            `yaml:"name"`
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
	With map[string]string `yaml:"with"`
}

type multiLaneJob struct {
	Name        string            `yaml:"name"`
	If          string            `yaml:"if"`
	Needs       any               `yaml:"needs"`
	RunsOn      string            `yaml:"runs-on"`
	Permissions map[string]string `yaml:"permissions"`
	Strategy    struct {
		Matrix struct {
			Lane    []string            `yaml:"lane"`
			Include []map[string]any    `yaml:"include"`
			Exclude []map[string]string `yaml:"exclude"`
		} `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []multiLaneStep `yaml:"steps"`
}

type multiLaneWorkflow struct {
	On          map[string]yaml.Node `yaml:"on"`
	Concurrency map[string]string    `yaml:"concurrency"`
	Permissions map[string]string    `yaml:"permissions"`
	Jobs        map[string]multiLaneJob
}

// gascityRequiredChecks: the main ruleset's and branch protection's required
// check names (2026-10-04). A preview job with one of these names would let
// either workflow satisfy it.
var gascityRequiredChecks = []string{
	"Check",
	"Analyze (actions)",
	"Analyze (go)",
	"Analyze (javascript-typescript)",
	"Analyze (python)",
	"CI / required",
	"bazel test (side-by-side)",
	"BUILD files in sync",
}

// Each lane's exact bazel command. Every lane passes --config=ci; no lane
// passes --config=sole-run before G0.
var multiLaneCommands = map[string]string{
	"unit":        "test --config=ci --keep_going //...",
	"acceptance":  "test --config=ci --config=acceptance --keep_going //test/acceptance:acceptance_test",
	"integration": "test --config=ci --config=integration --keep_going //test/integration:integration_test",
}

const (
	multiLaneIf = "needs.rbe.outputs.mode != 'skip' && !(github.event_name == 'pull_request' && needs.rbe.outputs.mode == 'cache')"
	// The heap report warns above 3.5 GB of the 4 GB client heap (R4).
	multiLaneHeapWarn = `if [ -n "$peak" ] && [ "$peak" -gt 3584 ]; then`
)

func readMultiLaneWorkflow(t *testing.T) multiLaneWorkflow {
	t.Helper()
	var wf multiLaneWorkflow
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), bazelMultiLaneWorkflow)), &wf); err != nil {
		t.Fatalf("parse %s: %v", bazelMultiLaneWorkflow, err)
	}
	return wf
}

func TestBazelMultiLaneWorkflowTriggersAndPermissions(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	// pull_request (never pull_request_target: fork code must not run with
	// the base repository's token or secrets), pushes to main, dispatches
	// and calls.
	on := slices.Sorted(maps.Keys(wf.On))
	if want := []string{"pull_request", "push", "workflow_call", "workflow_dispatch"}; !reflect.DeepEqual(on, want) {
		t.Errorf("%s on: %v, want exactly %v", bazelMultiLaneWorkflow, on, want)
	}
	// A PR's runs share a group and cancel each other; every other event has
	// a group of its own (the run id): a push to main is never cancelled, nor
	// replaced while pending by the next push.
	wantConcurrency := map[string]string{
		"group":              "${{ github.workflow }}-${{ github.event_name == 'pull_request' && github.event.pull_request.number || github.run_id }}",
		"cancel-in-progress": "${{ github.event_name == 'pull_request' }}",
	}
	if !reflect.DeepEqual(wf.Concurrency, wantConcurrency) {
		t.Errorf("concurrency = %v, want %v", wf.Concurrency, wantConcurrency)
	}
	readOnly := map[string]string{"contents": "read"}
	if !reflect.DeepEqual(wf.Permissions, readOnly) {
		t.Errorf("top-level permissions = %v, want %v", wf.Permissions, readOnly)
	}
	wantJobs := map[string]map[string]string{
		"rbe":        {"contents": "read", "actions": "write"}, // dispatches rbe-worker-pool.yml
		"lane":       readOnly,
		"coverage":   readOnly,
		"sync-check": readOnly,
		"gate":       nil, // the top-level contents: read
	}
	if len(wf.Jobs) != len(wantJobs) {
		t.Errorf("%s has %d jobs, want %d (%v)", bazelMultiLaneWorkflow, len(wf.Jobs), len(wantJobs), wantJobs)
	}
	for id, want := range wantJobs {
		job, ok := wf.Jobs[id]
		if !ok {
			t.Errorf("%s has no %s job", bazelMultiLaneWorkflow, id)
			continue
		}
		if !reflect.DeepEqual(job.Permissions, want) {
			t.Errorf("job %s permissions = %v, want %v", id, job.Permissions, want)
		}
	}
}

func TestBazelMultiLaneWorkflowShape(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	lane, ok := wf.Jobs["lane"]
	if !ok {
		t.Fatalf("%s has no lane job", bazelMultiLaneWorkflow)
	}
	matrix := lane.Strategy.Matrix

	// Two jobs with a required check's name would let either satisfy it.
	for id, job := range wf.Jobs {
		names := []string{job.Name}
		if strings.Contains(job.Name, "${{ matrix.lane }}") {
			names = nil
			for _, l := range matrix.Lane {
				names = append(names, strings.ReplaceAll(job.Name, "${{ matrix.lane }}", l))
			}
		}
		for _, name := range names {
			for _, required := range gascityRequiredChecks {
				if strings.EqualFold(strings.TrimSpace(name), required) {
					t.Errorf("job %s is named %q, a required check; the preview must not report it (the cutover renames the gate when bazel-test.yml is deleted)", id, name)
				}
			}
		}
	}

	// B1: no lanes for a pull_request run in mode cache, until rbe-west's
	// mint serves bazel.yml; the gate accepts exactly that skip.
	if lane.If != multiLaneIf {
		t.Errorf("lane if = %q, want %q", lane.If, multiLaneIf)
	}
	if want := "${{ (needs.rbe.outputs.mode == 'cache' || needs.rbe.outputs.mode == 'local') && 'blacksmith-4vcpu-ubuntu-2404' || 'blacksmith-2vcpu-ubuntu-2404' }}"; lane.RunsOn != want {
		t.Errorf("lane runs-on = %q, want %q (2 vCPU clients in remote modes)", lane.RunsOn, want)
	}

	// The lanes and their exact commands.
	if want := []string{"unit", "acceptance", "integration"}; !reflect.DeepEqual(matrix.Lane, want) {
		t.Errorf("lane matrix lanes = %v, want %v", matrix.Lane, want)
	}
	got := map[string]string{}
	for _, entry := range matrix.Include {
		name, _ := entry["lane"].(string)
		cmd, _ := entry["cmd"].(string)
		got[name] = cmd
		// Evidence-only (exit 0 on failure) is integration's alone, until G3.
		ev, set := entry["evidence-only"]
		if (name == "integration") != (set && ev == true) {
			t.Errorf("lane %s: evidence-only %v; want true on integration only", name, ev)
		}
		for key := range entry {
			if key != "lane" && key != "cmd" && key != "evidence-only" {
				t.Errorf("lane %s: unexpected matrix key %q", name, key)
			}
		}
	}
	if !reflect.DeepEqual(got, multiLaneCommands) {
		t.Errorf("lane commands = %v, want %v", got, multiLaneCommands)
	}
	// Lanes left out start no runner and mint no certificate: integration on
	// pull requests (until G3), acceptance on the network-less fork pool.
	wantExclude := []map[string]string{
		{"lane": "${{ github.event_name == 'pull_request' && 'integration' || 'none' }}"},
		{"lane": "${{ needs.rbe.outputs.mode == 'fork-ro' && 'acceptance' || 'none' }}"},
	}
	if !reflect.DeepEqual(matrix.Exclude, wantExclude) {
		t.Errorf("lane matrix exclude = %v, want %v", matrix.Exclude, wantExclude)
	}

	// Every checkout is full blobless history, then fresh-merge onto the rbe
	// job's base-sha, so every lane tests one tree.
	for _, id := range []string{"lane", "sync-check"} {
		steps := wf.Jobs[id].Steps
		if len(steps) < 2 || !strings.HasPrefix(steps[0].Uses, "actions/checkout@") ||
			steps[0].With["fetch-depth"] != "0" || steps[0].With["filter"] != "blob:none" ||
			steps[1].Uses != freshMergeUses ||
			!reflect.DeepEqual(steps[1].With, map[string]string{"base-sha": "${{ needs.rbe.outputs.base-sha }}"}) {
			t.Errorf("job %s: first steps must be a full-history blobless checkout, then %s with the rbe job's base-sha", id, freshMergeUses)
		}
	}

	var tmpdir bool
	var testStep *multiLaneStep
	for i, step := range lane.Steps {
		tmpdir = tmpdir || strings.Contains(step.Run, "mkdir -p /tmp/bt")
		if step.ID == "test" {
			testStep = &lane.Steps[i]
		}
	}
	if !tmpdir {
		t.Error("lane job: no /tmp/bt step")
	}
	if testStep == nil || testStep.Env["CMD"] != "${{ matrix.cmd }}" || testStep.Env["EVIDENCE_ONLY"] != "${{ matrix.evidence-only == true }}" ||
		!strings.Contains(testStep.Run, `read -r -a args <<<"$CMD"`) {
		t.Errorf("lane job: the test step must run matrix.cmd as words and read matrix.evidence-only")
	}
	for _, id := range []string{"lane", "coverage"} {
		heap := false
		for _, step := range wf.Jobs[id].Steps {
			if step.Name == "Bazel client heap" && strings.Contains(step.Run, "bazel info peak-heap-size") &&
				strings.Contains(step.Run, multiLaneHeapWarn) && strings.Contains(step.Run, "::warning ") {
				heap = true
			}
		}
		if !heap {
			t.Errorf("job %s: no Bazel client heap step that warns above 3.5 GB (%s)", id, multiLaneHeapWarn)
		}
	}

	gate := wf.Jobs["gate"]
	if gate.If != "always()" || !reflect.DeepEqual(gate.Needs, []any{"rbe", "lane", "sync-check"}) {
		t.Errorf("gate: if %q, needs %v; want always() over rbe, lane, sync-check", gate.If, gate.Needs)
	}
	evaluate := ""
	for _, step := range gate.Steps {
		if step.Name == "Evaluate" {
			evaluate = step.Run
		}
	}
	for _, want := range []string{
		`[ "$RBE" = success ] || exit 1`,
		`[ "$SYNC" = success ] || exit 1`,
		`[ "$MODE" = skip ] && exit 0`,
		"if [ \"$EVENT\" = pull_request ] && [ \"$MODE\" = cache ]; then\n  [ \"$LANES\" = skipped ]\n  exit $?\nfi",
	} {
		if !strings.Contains(evaluate, want) {
			t.Errorf("gate Evaluate lacks %q:\n%s", want, evaluate)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(evaluate), `[ "$LANES" = success ]`) {
		t.Errorf("gate Evaluate must end by requiring the lanes' success:\n%s", evaluate)
	}
}
