package scripts_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
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

// bazel.yml's lanes and bazel-test.yml share remote cache entries only if
// their actions hash alike. The test PATH and every other key input are
// committed unconditionally in .bazelrc, and --config=ci carries no key input
// (bazel_key_parity_test.go checks both); this test pins the rest: the
// tagged-suite configs carry bazel-test.yml's flags, and the remote modes
// both workflows select give 2 vCPU clients minimal downloads and 64 actions
// in flight.
func TestBazelCIConfigMatchesBazelTestRC(t *testing.T) {
	root := repoRoot(t)
	rc := readFile(t, root, ".bazelrc")
	legacy := readFile(t, root, bazelTestWorkflow)

	if bazelRCFlagValue(rc, "test --test_env=PATH") == "" {
		t.Errorf(".bazelrc pins no unconditional test PATH (test --test_env=PATH=...)")
	}
	for _, config := range []string{"remote-exec", "fork-cache"} {
		for _, flag := range []string{"--remote_download_minimal", "--jobs=64"} {
			if !strings.Contains(rc, "\nbuild:"+config+" "+flag+"\n") {
				t.Errorf(".bazelrc lacks build:%s %s", config, flag)
			}
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
	Outputs     map[string]string `yaml:"outputs"`
	Strategy    struct {
		FailFast *bool          `yaml:"fail-fast"`
		Matrix   map[string]any `yaml:"matrix"`
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
	// The lane job starts only for a non-empty lane list (an empty matrix is
	// an error) and takes its matrix from it whole.
	multiLaneIf      = "needs.rbe.outputs.lanes != '[]'"
	multiLaneInclude = "${{ fromJSON(needs.rbe.outputs.lanes) }}"
	// The concurrency group's literal prefix (never github.workflow, which
	// under workflow_call is the caller's name).
	multiLaneConcurrencyPrefix = "bazel-yml-"
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
	// a group of its own (the run id): a push to main is never canceled, nor
	// replaced while pending by the next push. The prefix is a literal, apart
	// from bazel-test.yml's (its github.workflow is its name).
	wantConcurrency := map[string]string{
		"group":              multiLaneConcurrencyPrefix + "${{ github.event_name == 'pull_request' && github.event.pull_request.number || github.run_id }}",
		"cancel-in-progress": "${{ github.event_name == 'pull_request' }}",
	}
	if !reflect.DeepEqual(wf.Concurrency, wantConcurrency) {
		t.Errorf("concurrency = %v, want %v", wf.Concurrency, wantConcurrency)
	}
	var legacy struct {
		Name        string            `yaml:"name"`
		Concurrency map[string]string `yaml:"concurrency"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), bazelTestWorkflow)), &legacy); err != nil {
		t.Fatalf("parse %s: %v", bazelTestWorkflow, err)
	}
	legacyGroup := strings.ReplaceAll(legacy.Concurrency["group"], "${{ github.workflow }}", legacy.Name)
	if legacyGroup == "" || strings.HasPrefix(legacyGroup, multiLaneConcurrencyPrefix) || strings.HasPrefix(multiLaneConcurrencyPrefix, strings.SplitN(legacyGroup, "${{", 2)[0]) {
		t.Errorf("%s concurrency group %q shares bazel.yml's prefix %q; the two workflows' runs must not cancel each other", bazelTestWorkflow, legacyGroup, multiLaneConcurrencyPrefix)
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

// multiLaneRBEStep returns the rbe job's step with id.
func multiLaneRBEStep(t *testing.T, wf multiLaneWorkflow, id string) multiLaneStep {
	t.Helper()
	for _, step := range wf.Jobs["rbe"].Steps {
		if step.ID == id {
			return step
		}
	}
	t.Fatalf("%s: the rbe job has no step %q", bazelMultiLaneWorkflow, id)
	return multiLaneStep{}
}

// readStepOutput returns the value of name in a $GITHUB_OUTPUT file, and
// whether it is there.
func readStepOutput(t *testing.T, path, name string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	value, found := "", false
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, name+"="); ok {
			value, found = v, true
		}
	}
	return value, found
}

// multiLaneLanes runs the rbe job's Lanes step for an event and a mode and
// returns its raw lanes output (the string the lane job's if compares).
func multiLaneLanes(t *testing.T, script, event, mode string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	output := filepath.Join(dir, "output")
	out, err := runWorkflowStepScript(t, dir, script, map[string]string{
		"EVENT":               event,
		"MODE":                mode,
		"GITHUB_OUTPUT":       output,
		"GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary"),
	})
	if err != nil {
		return out, err
	}
	lanes, ok := readStepOutput(t, output, "lanes")
	if !ok {
		t.Fatalf("Lanes step (event %s, mode %s) wrote no lanes output:\n%s", event, mode, out)
	}
	return lanes, nil
}

var (
	multiLaneEvents = []string{"pull_request", "push", "workflow_dispatch", "workflow_call", "schedule"}
	multiLaneModes  = []string{"remote", "fork-ro", "fork-rw", "cache", "local", "skip"}
)

// wantMultiLanes: the lanes each (event, mode) starts, in order.
func wantMultiLanes(event, mode string) []string {
	if mode == "skip" || (event == "pull_request" && mode == "cache") {
		// rbe-west off; a cache-mode PR until rbe-west's mint serves bazel.yml.
		return []string{}
	}
	lanes := []string{"unit"}
	if mode != "fork-ro" { // the fork pool has no network (ga-73eoo)
		lanes = append(lanes, "acceptance")
	}
	if event == "push" || event == "workflow_dispatch" { // until G3
		lanes = append(lanes, "integration")
	}
	return lanes
}

// TestBazelMultiLaneLaneList runs the rbe job's Lanes step for every event
// and mode and checks the lane job's matrix it yields: the lanes that start,
// each lane's exact command, evidence-only on integration alone, and at most
// 2 lanes (2 rbe-fork certificates) for a fork PR.
func TestBazelMultiLaneLaneList(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	step := multiLaneRBEStep(t, wf, "lanes")
	wantEnv := map[string]string{"EVENT": "${{ github.event_name }}", "MODE": "${{ steps.decide.outputs.mode }}"}
	if step.If != "" || !reflect.DeepEqual(step.Env, wantEnv) {
		t.Errorf("Lanes step: if %q, env %v; want it unconditional with env %v", step.If, step.Env, wantEnv)
	}
	if got := wf.Jobs["rbe"].Outputs["lanes"]; got != "${{ steps.lanes.outputs.lanes }}" {
		t.Errorf("rbe job output lanes = %q, want the Lanes step's", got)
	}

	for _, event := range multiLaneEvents {
		for _, mode := range multiLaneModes {
			raw, err := multiLaneLanes(t, step.Run, event, mode)
			if err != nil {
				t.Errorf("Lanes step (event %s, mode %s) failed: %v\n%s", event, mode, err, raw)
				continue
			}
			want := wantMultiLanes(event, mode)
			if len(want) == 0 && raw != "[]" {
				// The lane job's if compares the string itself.
				t.Errorf("event %s, mode %s: lanes %q, want exactly []", event, mode, raw)
				continue
			}
			var entries []map[string]any
			if err := json.Unmarshal([]byte(raw), &entries); err != nil {
				t.Errorf("event %s, mode %s: lanes %q is not a JSON array of objects: %v", event, mode, raw, err)
				continue
			}
			got := []string{}
			for _, entry := range entries {
				name, _ := entry["lane"].(string)
				got = append(got, name)
				if cmd, _ := entry["cmd"].(string); cmd != multiLaneCommands[name] {
					t.Errorf("event %s, mode %s: lane %s cmd %q, want %q", event, mode, name, cmd, multiLaneCommands[name])
				}
				// Evidence-only (exit 0 on failure) is integration's alone, until G3.
				ev, set := entry["evidence-only"]
				if (name == "integration") != (set && ev == true) {
					t.Errorf("event %s, mode %s: lane %s evidence-only %v; want true on integration only", event, mode, name, ev)
				}
				for key := range entry {
					if key != "lane" && key != "cmd" && key != "evidence-only" {
						t.Errorf("event %s, mode %s: lane %s: unexpected matrix key %q", event, mode, name, key)
					}
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("event %s, mode %s: lanes %v, want %v", event, mode, got, want)
			}
			// The decide step reaches a fork mode on pull_request runs only.
			if event == "pull_request" && strings.HasPrefix(mode, "fork-") && len(got) > 2 {
				t.Errorf("event %s, mode %s: %d lanes; a fork run mints at most 2 rbe-fork certificates", event, mode, len(got))
			}
		}
	}
	if out, err := multiLaneLanes(t, step.Run, "pull_request", "bogus"); err == nil {
		t.Errorf("Lanes step accepted mode bogus: %s", out)
	}
}

// multiLaneLaneNames: every lane any (event, mode) starts.
func multiLaneLaneNames() []string {
	var names []string
	for _, event := range multiLaneEvents {
		for _, mode := range multiLaneModes {
			for _, lane := range wantMultiLanes(event, mode) {
				if !slices.Contains(names, lane) {
					names = append(names, lane)
				}
			}
		}
	}
	return names
}

func TestBazelMultiLaneWorkflowShape(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	lane, ok := wf.Jobs["lane"]
	if !ok {
		t.Fatalf("%s has no lane job", bazelMultiLaneWorkflow)
	}

	// Two jobs with a required check's name would let either satisfy it.
	if want := "bazel / ${{ matrix.lane }}"; lane.Name != want {
		t.Errorf("lane job name = %q, want %q", lane.Name, want)
	}
	for id, job := range wf.Jobs {
		names := []string{job.Name}
		if strings.Contains(job.Name, "${{ matrix.lane }}") {
			names = nil
			for _, l := range multiLaneLaneNames() {
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

	// The matrix is the rbe job's lane list, whole (TestBazelMultiLaneLaneList
	// runs it); the job is skipped where the list is empty.
	if lane.If != multiLaneIf {
		t.Errorf("lane if = %q, want %q", lane.If, multiLaneIf)
	}
	if want := map[string]any{"include": multiLaneInclude}; !reflect.DeepEqual(lane.Strategy.Matrix, want) {
		t.Errorf("lane matrix = %v, want %v (an exclude cannot drop a lane an include entry names)", lane.Strategy.Matrix, want)
	}
	if lane.Strategy.FailFast == nil || *lane.Strategy.FailFast {
		t.Errorf("lane strategy: want fail-fast: false")
	}
	if want := "${{ (needs.rbe.outputs.mode == 'cache' || needs.rbe.outputs.mode == 'local') && 'blacksmith-4vcpu-ubuntu-2404' || 'blacksmith-2vcpu-ubuntu-2404' }}"; lane.RunsOn != want {
		t.Errorf("lane runs-on = %q, want %q (2 vCPU clients in remote modes)", lane.RunsOn, want)
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
}

// multiLaneGateEvaluate returns the gate job's Evaluate script, after
// checking it runs always() over rbe, lane and sync-check with the env the
// cases below set.
func multiLaneGateEvaluate(t *testing.T, wf multiLaneWorkflow) string {
	t.Helper()
	gate := wf.Jobs["gate"]
	if gate.If != "always()" || !reflect.DeepEqual(gate.Needs, []any{"rbe", "lane", "sync-check"}) {
		t.Errorf("gate: if %q, needs %v; want always() over rbe, lane, sync-check", gate.If, gate.Needs)
	}
	for _, step := range gate.Steps {
		if step.Name != "Evaluate" {
			continue
		}
		for k, v := range map[string]string{
			"LANE_LIST": "${{ needs.rbe.outputs.lanes }}",
			"RBE":       "${{ needs.rbe.result }}",
			"LANES":     "${{ needs.lane.result }}",
			"SYNC":      "${{ needs.sync-check.result }}",
		} {
			if step.Env[k] != v {
				t.Errorf("gate Evaluate env %s = %q, want %q", k, step.Env[k], v)
			}
		}
		return step.Run
	}
	t.Fatalf("%s: the gate job has no Evaluate step", bazelMultiLaneWorkflow)
	return ""
}

// multiLaneGatePasses runs the gate's Evaluate script with job results and
// the rbe job's lane list.
func multiLaneGatePasses(t *testing.T, script, laneList, rbe, lanes, sync string) bool {
	t.Helper()
	_, err := runWorkflowStepScript(t, t.TempDir(), script, map[string]string{
		"EVENT": "pull_request", "MODE": "remote",
		"LANE_LIST": laneList, "RBE": rbe, "LANES": lanes, "SYNC": sync,
	})
	return err == nil
}

// TestBazelMultiLaneGate runs the gate: rbe and sync-check must succeed;
// the lanes must succeed, or be skipped exactly where the lane list is empty.
func TestBazelMultiLaneGate(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	script := multiLaneGateEvaluate(t, wf)
	someLanes := `[{"lane":"unit","cmd":"test --config=ci --keep_going //..."}]`
	for _, c := range []struct {
		laneList, rbe, lanes, sync string
		pass                       bool
	}{
		{someLanes, "success", "success", "success", true},
		{someLanes, "success", "failure", "success", false},
		{someLanes, "success", "cancelled", "success", false}, //nolint:misspell // GitHub Actions job result value
		{someLanes, "success", "skipped", "success", false},
		{someLanes, "success", "success", "failure", false},
		{someLanes, "failure", "success", "success", false},
		{"[]", "success", "skipped", "success", true},
		{"[]", "success", "success", "success", false},
		{"[]", "success", "skipped", "failure", false},
		{"", "failure", "skipped", "success", false},
		{"", "success", "skipped", "success", false},
	} {
		if got := multiLaneGatePasses(t, script, c.laneList, c.rbe, c.lanes, c.sync); got != c.pass {
			t.Errorf("gate with lane list %q, rbe %s, lanes %s, sync %s: pass %v, want %v", c.laneList, c.rbe, c.lanes, c.sync, got, c.pass)
		}
	}
}

// TestBazelMultiLaneGateUnderRequiredNameRunsLanes: the cutover hazard. The
// gate passes when no lane ran (an empty lane list: mode skip, or a
// pull_request run in mode cache, i.e. fork and Dependabot PRs). Under a
// required check's name that merges untested PRs, so the cutover that
// renames the gate must first make a cache-mode PR run lanes or fail.
func TestBazelMultiLaneGateUnderRequiredNameRunsLanes(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	script := multiLaneGateEvaluate(t, wf)
	lanesStep := multiLaneRBEStep(t, wf, "lanes")
	var zeroLanes []string
	for _, event := range multiLaneEvents {
		for _, mode := range multiLaneModes {
			raw, err := multiLaneLanes(t, lanesStep.Run, event, mode)
			if err != nil {
				t.Fatalf("Lanes step (event %s, mode %s) failed: %v\n%s", event, mode, err, raw)
			}
			if raw == "[]" && multiLaneGatePasses(t, script, raw, "success", "skipped", "success") {
				zeroLanes = append(zeroLanes, event+"/"+mode)
			}
		}
	}
	if len(zeroLanes) == 0 {
		return
	}
	name := strings.TrimSpace(wf.Jobs["gate"].Name)
	for _, required := range gascityRequiredChecks {
		if strings.EqualFold(name, required) {
			t.Errorf("gate %q is a required check but passes with no lane run for %v: make a cache-mode PR run lanes (rbe-west's mint serves bazel.yml) or fail before the cutover", name, zeroLanes)
		}
	}
}

// TestBazelMultiLaneBaseSHA runs the rbe job's base-sha step against a stub
// git: exactly one head named refs/heads/<base_ref> (column 2, exact), three
// ls-remote attempts 5 s then 10 s apart, and no output otherwise.
func TestBazelMultiLaneBaseSHA(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	step := multiLaneRBEStep(t, wf, "base")
	if step.If != "github.event_name == 'pull_request'" || !reflect.DeepEqual(step.Env, map[string]string{"BASE_REF": "${{ github.base_ref }}"}) {
		t.Errorf("base step: if %q, env %v; want pull_request only, BASE_REF from github.base_ref", step.If, step.Env)
	}
	if got := wf.Jobs["rbe"].Outputs["base-sha"]; got != "${{ steps.base.outputs.base-sha }}" {
		t.Errorf("rbe job output base-sha = %q, want the base step's", got)
	}

	const (
		sha1 = "1111111111111111111111111111111111111111"
		sha2 = "2222222222222222222222222222222222222222"
	)
	for _, c := range []struct {
		name     string
		failures int    // ls-remote attempts that fail before it answers
		heads    string // its answer
		want     string // base-sha, "" for a failed step
		sleeps   string
	}{
		{"exact head", 0, sha1 + "\trefs/heads/main\n", sha1, ""},
		{"tail matches ignored", 0, sha2 + "\trefs/heads/x/refs/heads/main\n" + sha1 + "\trefs/heads/main\n", sha1, ""},
		{"third attempt", 2, sha1 + "\trefs/heads/main\n", sha1, "5\n10\n"},
		{"three failures", 3, sha1 + "\trefs/heads/main\n", "", "5\n10\n"},
		{"no head", 0, "", "", ""},
		{"tail match only", 0, sha2 + "\trefs/heads/x/refs/heads/main\n", "", ""},
		{"two heads", 0, sha1 + "\trefs/heads/main\n" + sha2 + "\trefs/heads/main\n", "", ""},
		{"not a sha", 0, "HEAD\trefs/heads/main\n", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			stubs := filepath.Join(dir, "stubs")
			if err := os.MkdirAll(stubs, 0o755); err != nil {
				t.Fatal(err)
			}
			heads := filepath.Join(dir, "heads")
			if err := os.WriteFile(heads, []byte(c.heads), 0o644); err != nil {
				t.Fatal(err)
			}
			calls, sleeps, output := filepath.Join(dir, "calls"), filepath.Join(dir, "sleeps"), filepath.Join(dir, "output")
			// git fails its first c.failures calls, then prints heads; every
			// call's arguments are logged.
			writeExecutable(t, filepath.Join(stubs, "git"), fmt.Sprintf(`#!/bin/sh
echo "$*" >> '%s'
n=$(wc -l < '%s')
[ "$n" -gt %d ] || exit 128
cat '%s'
`, calls, calls, c.failures, heads))
			writeExecutable(t, filepath.Join(stubs, "sleep"), "#!/bin/sh\necho \"$*\" >> '"+sleeps+"'\n")
			out, err := runWorkflowStepScript(t, dir, step.Run, map[string]string{
				"PATH":              stubs + ":" + os.Getenv("PATH"),
				"BASE_REF":          "main",
				"GITHUB_REPOSITORY": "gastownhall/gascity",
				"GITHUB_OUTPUT":     output,
			})
			got, written := readStepOutput(t, output, "base-sha")
			if c.want == "" {
				if err == nil || written {
					t.Errorf("step passed (base-sha %q, err %v); want it to fail with no output\n%s", got, err, out)
				}
			} else if err != nil || got != c.want {
				t.Errorf("base-sha %q (err %v), want %s\n%s", got, err, c.want, out)
			}
			if data, _ := os.ReadFile(sleeps); string(data) != c.sleeps {
				t.Errorf("slept %q, want %q", data, c.sleeps)
			}
			data, _ := os.ReadFile(calls)
			for _, call := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if want := "ls-remote --heads https://github.com/gastownhall/gascity refs/heads/main"; call != want {
					t.Errorf("git %q, want git %s", call, want)
				}
			}
		})
	}
}
