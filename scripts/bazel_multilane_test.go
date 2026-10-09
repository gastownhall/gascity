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

// bazel.yml (rbe-west plan W1) is the only Bazel workflow, and its gate and
// sync-check jobs are the required checks "bazel test (side-by-side)" and
// "BUILD files in sync". These tests pin its triggers, execution modes,
// lanes, permissions and gate, keep each required name on exactly one job
// across every workflow, and keep .github/actions/setup-bazel a byte copy of
// beads' composite.

const (
	bazelMultiLaneWorkflow = ".github/workflows/bazel.yml"
	setupBazelDir          = ".github/actions/setup-bazel"
)

// setupBazelBeadsDigests: sha256 of beads' .github/actions/setup-bazel files
// (beads main b8a9545c, #7174: R4's -Xmx4g client heap; action.yml from beads
// main 533f9529, #7379: MODULE.bazel in the runner cache key). A change here
// is a change in beads first: copy the files from beads and update these
// digests in the same PR.
var setupBazelBeadsDigests = map[string]string{
	"action.yml":         "e3e8b2904de5153a9f8006ecd7058c11569c3e5311d2381f0c192843b760f559",
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

// bazel.yml's lanes, pre-push and developers share remote cache entries
// only if their actions hash alike. The test PATH and every other key input
// are committed unconditionally in .bazelrc, and --config=ci carries no key
// input (bazel_key_parity_test.go checks both); this test pins the rest: the
// tagged-suite configs carry the suites' flags, and the remote modes give
// 2 vCPU clients minimal downloads and 64 actions in flight.
func TestBazelCIConfigSuiteConfigs(t *testing.T) {
	rc := readFile(t, repoRoot(t), ".bazelrc")

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
	for config, flags := range map[string]string{
		"acceptance":  "--define=gotags=acceptance_a --test_timeout=1100",
		"integration": "--define=gotags=integration --test_timeout=1100",
		// Its --test_filter: TestIntegrationSmokeLaneMatchesShardScript.
		"integration-smoke": "--config=integration",
	} {
		for _, f := range strings.Fields(flags) {
			if !strings.Contains(rc, "\ntest:"+config+" "+f+"\n") {
				t.Errorf(".bazelrc lacks test:%s %s", config, f)
			}
		}
	}
	for _, line := range []string{
		"test:ci --flaky_test_attempts=1",
		"test:ci --experimental_remote_cache_eviction_retries=0",
		// Tests run beside nogo, not after it (bazel_key_parity_test.go
		// classifies the flag non-key).
		"test:ci --experimental_use_validation_aspect",
	} {
		if !strings.Contains(rc, "\n"+line+"\n") {
			t.Errorf(".bazelrc lacks %q", line)
		}
	}
	// Every PR and push lane reuses cached results; only test:fresh (the
	// nightly fresh-test-results run, bazel-nightly.yml) may force
	// re-execution.
	sawFresh := false
	for _, o := range parseBazelRC(rc) {
		if o.flag != "--nocache_test_results" {
			continue
		}
		if o.command == "test" && o.config == "fresh" {
			sawFresh = true
			continue
		}
		t.Errorf(".bazelrc sets --nocache_test_results on %s:%s; only test:fresh may force re-execution, so every other lane reuses cached results", o.command, o.config)
	}
	if !sawFresh {
		t.Errorf(".bazelrc has no test:fresh --nocache_test_results (the nightly fresh run's config)")
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
	// A string: some steps set it from an expression.
	ContinueOnError string `yaml:"continue-on-error"`
	TimeoutMinutes  int    `yaml:"timeout-minutes"`
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
// check names (2026-10-06). Two jobs with one of these names would let
// either satisfy it.
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

// Each lane's exact bazel command. Every lane passes --config=ci and reuses
// cached test results; there is no --config=sole-run.
var multiLaneCommands = map[string]string{
	"unit":                 "test --config=ci --keep_going //...",
	"acceptance":           "test --config=ci --config=acceptance --keep_going //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests",
	"integration":          "test --config=ci --config=integration --keep_going //test/integration:integration_test",
	"integration-packages": "test --config=ci --config=integration --keep_going //test:integration_packages",
	"integration-smoke":    "test --config=ci --config=integration-smoke --keep_going //test/integration:integration_test",
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
	// the base repository's token or secrets), merge groups (the merge
	// queue, scripts/ci_merge_queue_test.go), pushes to main, dispatches
	// and calls.
	on := slices.Sorted(maps.Keys(wf.On))
	if want := []string{"merge_group", "pull_request", "push", "workflow_call", "workflow_dispatch"}; !reflect.DeepEqual(on, want) {
		t.Errorf("%s on: %v, want exactly %v", bazelMultiLaneWorkflow, on, want)
	}
	// A PR's runs share a group and cancel each other; a merge group keys on
	// its own queue ref and is never canceled; every other event has a
	// group of its own (the run id): a push to main is never canceled, nor
	// replaced while pending by the next push. The prefix is a literal
	// (under workflow_call github.workflow is the caller's name).
	wantConcurrency := map[string]string{
		"group":              multiLaneConcurrencyPrefix + "${{ github.event_name == 'pull_request' && github.event.pull_request.number || github.event_name == 'merge_group' && github.ref || github.run_id }}",
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
		"rbe": {"contents": "read", "actions": "write"}, // dispatches rbe-worker-pool.yml
		// The worker-env preflight lists drift issues (tools/rbe/worker-env-drift).
		"lane":        {"contents": "read", "issues": "read"},
		"coverage":    {"contents": "read", "issues": "read"},
		"sync-check":  readOnly,
		"bep-summary": readOnly, // downloads this run's artifacts with the job token
		// rbe-worker S3a: secret-free (rbe-worker-repo-design.md §5.4, §6.3);
		// issues: read for the same worker-env preflight the lane job runs.
		"worker-host": {"contents": "read", "issues": "read"},
		"gate":        nil, // the top-level contents: read
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

// multiLaneLanes runs the rbe job's Lanes step for an event, a mode and
// fresh-test-results ("true" or "false") and returns its raw lanes output
// (the string the lane job's if compares).
func multiLaneLanes(t *testing.T, script, event, mode, fresh string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	output := filepath.Join(dir, "output")
	out, err := runWorkflowStepScript(t, dir, script, map[string]string{
		"EVENT":               event,
		"MODE":                mode,
		"FRESH":               fresh,
		"GITHUB_OUTPUT":       output,
		"GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary"),
	})
	if err != nil {
		return out, err
	}
	lanes, ok := readStepOutput(t, output, "lanes")
	if !ok {
		t.Fatalf("Lanes step (event %s, mode %s, fresh %s) wrote no lanes output:\n%s", event, mode, fresh, out)
	}
	return lanes, nil
}

// multiLaneForkCertificates caps the rbe-fork certificates (one per lane) a
// fork run mints: well inside the mint's 12 per run and 160 per PR a day, so
// a run and a few re-runs never hit them.
const multiLaneForkCertificates = 4

var (
	multiLaneEvents = []string{"pull_request", "merge_group", "push", "workflow_dispatch", "workflow_call", "schedule"}
	multiLaneModes  = []string{"remote", "fork-ro", "fork-rw", "cache", "local"}
)

// wantMultiLanes: the lanes each (event, mode) starts, in order.
func wantMultiLanes(event, _ string) []string {
	// Every mode tests (the gate fails a run with no lane). Acceptance and
	// the gating integration lanes run in every mode, the fork pool's
	// (fork-ro: no network) included: gc init no longer clones gascity-packs
	// (#7005), and no integration_packages target needs the network.
	lanes := []string{"unit", "acceptance", "integration-packages", "integration-smoke"}
	// Nightly (schedule, via bazel-nightly.yml) and dispatches only, until
	// G3; off every push (rbe-ci-cost-latency-study.md recommendation 5).
	if event == "workflow_dispatch" || event == "schedule" {
		lanes = append(lanes, "integration")
	}
	return lanes
}

// TestBazelMultiLaneLaneList runs the rbe job's Lanes step for every event
// and mode and checks the lane job's matrix it yields: the lanes that start,
// each lane's exact command, evidence-only on integration alone, and at most
// multiLaneForkCertificates lanes (one rbe-fork certificate each) for a fork
// PR.
func TestBazelMultiLaneLaneList(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	step := multiLaneRBEStep(t, wf, "lanes")
	wantEnv := map[string]string{
		"EVENT": "${{ github.event_name }}",
		"MODE":  "${{ steps.decide.outputs.mode }}",
		"FRESH": "${{ inputs.fresh-test-results }}",
	}
	if step.If != "" || !reflect.DeepEqual(step.Env, wantEnv) {
		t.Errorf("Lanes step: if %q, env %v; want it unconditional with env %v", step.If, step.Env, wantEnv)
	}
	if got := wf.Jobs["rbe"].Outputs["lanes"]; got != "${{ steps.lanes.outputs.lanes }}" {
		t.Errorf("rbe job output lanes = %q, want the Lanes step's", got)
	}

	for _, event := range multiLaneEvents {
		for _, mode := range multiLaneModes {
			for _, fresh := range []string{"false", "true"} {
				raw, err := multiLaneLanes(t, step.Run, event, mode, fresh)
				if err != nil {
					t.Errorf("Lanes step (event %s, mode %s, fresh %s) failed: %v\n%s", event, mode, fresh, err, raw)
					continue
				}
				want := wantMultiLanes(event, mode)
				var entries []map[string]any
				if err := json.Unmarshal([]byte(raw), &entries); err != nil {
					t.Errorf("event %s, mode %s, fresh %s: lanes %q is not a JSON array of objects: %v", event, mode, fresh, raw, err)
					continue
				}
				got := []string{}
				for _, entry := range entries {
					name, _ := entry["lane"].(string)
					got = append(got, name)
					wantCmd := multiLaneCommands[name]
					if fresh == "true" {
						wantCmd += " --config=fresh"
					}
					if cmd, _ := entry["cmd"].(string); cmd != wantCmd {
						t.Errorf("event %s, mode %s, fresh %s: lane %s cmd %q, want %q", event, mode, fresh, name, cmd, wantCmd)
					}
					// Evidence-only (exit 0 on failure) is integration's alone, until G3.
					ev, set := entry["evidence-only"]
					if (name == "integration") != (set && ev == true) {
						t.Errorf("event %s, mode %s, fresh %s: lane %s evidence-only %v; want true on integration only", event, mode, fresh, name, ev)
					}
					for key := range entry {
						if key != "lane" && key != "cmd" && key != "evidence-only" {
							t.Errorf("event %s, mode %s, fresh %s: lane %s: unexpected matrix key %q", event, mode, fresh, name, key)
						}
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("event %s, mode %s, fresh %s: lanes %v, want %v", event, mode, fresh, got, want)
				}
				// The decide step reaches a fork mode on pull_request runs only.
				if event == "pull_request" && strings.HasPrefix(mode, "fork-") && len(got) > multiLaneForkCertificates {
					t.Errorf("event %s, mode %s, fresh %s: %d lanes; a fork run mints at most %d rbe-fork certificates", event, mode, fresh, len(got), multiLaneForkCertificates)
				}
			}
		}
	}
	// skip (rbe-west off) is gone: that is mode cache now, which tests.
	for _, mode := range []string{"bogus", "skip"} {
		if out, err := multiLaneLanes(t, step.Run, "pull_request", mode, "false"); err == nil {
			t.Errorf("Lanes step accepted mode %s: %s", mode, out)
		}
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

	if want := "bazel / ${{ matrix.lane }}"; lane.Name != want {
		t.Errorf("lane job name = %q, want %q", lane.Name, want)
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
	// Unit and acceptance run on 8 vCPU, every other lane on 4: a remote
	// lane's client-side loading and analysis is CPU-bound (ga-vnycm2.8:
	// 23-34 s on 4 vCPU, 15-19 s on 8), and every other mode executes here,
	// or may (a fork lane's fallback to the read-only cache).
	if want := "${{ (matrix.lane == 'unit' || matrix.lane == 'acceptance') && 'blacksmith-8vcpu-ubuntu-2404' || 'blacksmith-4vcpu-ubuntu-2404' }}"; lane.RunsOn != want {
		t.Errorf("lane runs-on = %q, want %q", lane.RunsOn, want)
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
// checking it runs always() over rbe, lane, sync-check and worker-host (S3a)
// with the env the cases below set.
func multiLaneGateEvaluate(t *testing.T, wf multiLaneWorkflow) string {
	t.Helper()
	gate := wf.Jobs["gate"]
	if gate.If != "always()" || !reflect.DeepEqual(gate.Needs, []any{"rbe", "lane", "sync-check", "worker-host"}) {
		t.Errorf("gate: if %q, needs %v; want always() over rbe, lane, sync-check, worker-host", gate.If, gate.Needs)
	}
	for _, step := range gate.Steps {
		if step.Name != "Evaluate" {
			continue
		}
		for k, v := range map[string]string{
			"LANE_LIST":   "${{ needs.rbe.outputs.lanes }}",
			"RBE":         "${{ needs.rbe.result }}",
			"LANES":       "${{ needs.lane.result }}",
			"SYNC":        "${{ needs.sync-check.result }}",
			"WORKER_HOST": "${{ needs.worker-host.result }}",
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

// multiLaneGatePasses runs the gate's Evaluate script for an event and mode
// with job results and the rbe job's lane list.
func multiLaneGatePasses(t *testing.T, script, event, mode, laneList, rbe, lanes, sync, workerHost string) bool {
	t.Helper()
	_, err := runWorkflowStepScript(t, t.TempDir(), script, map[string]string{
		"EVENT": event, "MODE": mode,
		"LANE_LIST": laneList, "RBE": rbe, "LANES": lanes, "SYNC": sync, "WORKER_HOST": workerHost,
	})
	return err == nil
}

// TestBazelMultiLaneGate runs the gate: rbe, sync-check and the lanes must
// all succeed; skipped lanes (an empty lane list) or a missing lane list fail
// it whatever the event.
func TestBazelMultiLaneGate(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	script := multiLaneGateEvaluate(t, wf)
	someLanes := `[{"lane":"unit","cmd":"test --config=ci --keep_going //..."}]`
	for _, c := range []struct {
		event, mode, laneList, rbe, lanes, sync, workerHost string
		pass                                                bool
	}{
		{"pull_request", "remote", someLanes, "success", "success", "success", "success", true},
		{"pull_request", "cache", someLanes, "success", "success", "success", "success", true},
		{"push", "remote", someLanes, "success", "success", "success", "success", true},
		{"pull_request", "remote", someLanes, "success", "failure", "success", "success", false},
		{"pull_request", "remote", someLanes, "success", "cancelled", "success", "success", false}, //nolint:misspell // GitHub Actions job result value
		{"pull_request", "remote", someLanes, "success", "skipped", "success", "success", false},
		{"pull_request", "remote", someLanes, "success", "success", "failure", "success", false},
		{"pull_request", "remote", someLanes, "success", "success", "skipped", "success", false},
		{"pull_request", "remote", someLanes, "failure", "success", "success", "success", false},
		{"pull_request", "remote", someLanes, "success", "success", "success", "failure", false},
		{"pull_request", "remote", someLanes, "success", "success", "success", "skipped", false},
		{"pull_request", "cache", "[]", "success", "skipped", "success", "success", false},
		{"push", "cache", "[]", "success", "skipped", "success", "success", false},
		{"workflow_dispatch", "cache", "[]", "success", "skipped", "success", "success", false},
		{"pull_request", "remote", "", "failure", "skipped", "success", "success", false},
		{"pull_request", "remote", "", "success", "skipped", "success", "success", false},
		{"push", "remote", "", "success", "success", "success", "success", false},
	} {
		if got := multiLaneGatePasses(t, script, c.event, c.mode, c.laneList, c.rbe, c.lanes, c.sync, c.workerHost); got != c.pass {
			t.Errorf("gate (event %s, mode %s) with lane list %q, rbe %s, lanes %s, sync %s, worker-host %s: pass %v, want %v",
				c.event, c.mode, c.laneList, c.rbe, c.lanes, c.sync, c.workerHost, got, c.pass)
		}
	}
}

// requiredCheckJobs maps each required check name to the jobs, across every
// workflow, whose name renders to it (matrix names expanded over the lanes).
func requiredCheckJobs(t *testing.T) map[string][]string {
	t.Helper()
	root := repoRoot(t)
	files, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.y*ml"))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string][]string{}
	for _, f := range files {
		var wf struct {
			Jobs map[string]struct {
				Name string `yaml:"name"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal([]byte(readFile(t, root, filepath.Join(".github", "workflows", filepath.Base(f)))), &wf); err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for id, job := range wf.Jobs {
			names := []string{job.Name}
			if job.Name == "" {
				names = []string{id} // GitHub names an unnamed job by its id
			}
			if strings.Contains(job.Name, "${{ matrix.lane }}") {
				names = nil
				for _, l := range multiLaneLaneNames() {
					names = append(names, strings.ReplaceAll(job.Name, "${{ matrix.lane }}", l))
				}
			}
			for _, name := range names {
				for _, required := range gascityRequiredChecks {
					if strings.EqualFold(strings.TrimSpace(name), required) {
						found[required] = append(found[required], filepath.Base(f)+":"+id)
					}
				}
			}
		}
	}
	return found
}

// TestBazelMultiLaneGateUnderRequiredNameRunsLanes: the required Bazel checks
// are bazel.yml's, each on exactly one job of all workflows (two jobs of one
// name would let either satisfy it): "bazel test (side-by-side)" is the gate,
// which fans in rbe, every lane and sync-check, and "BUILD files in sync" is
// sync-check. For every event and mode the rbe job starts unit and
// acceptance, and the gate passes only when those lanes ran and succeeded.
func TestBazelMultiLaneGateUnderRequiredNameRunsLanes(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	found := requiredCheckJobs(t)
	for name, want := range map[string]string{
		"bazel test (side-by-side)": "bazel.yml:gate",
		"BUILD files in sync":       "bazel.yml:sync-check",
	} {
		if !reflect.DeepEqual(found[name], []string{want}) {
			t.Errorf("required check %q is produced by %v, want exactly %s", name, found[name], want)
		}
	}
	if got := wf.Jobs["gate"].Name; got != "bazel test (side-by-side)" {
		t.Errorf("gate job name %q", got)
	}
	if got := wf.Jobs["sync-check"].Name; got != "BUILD files in sync" {
		t.Errorf("sync-check job name %q", got)
	}
	// Only the gate and sync-check of bazel.yml carry a required name.
	for id, job := range wf.Jobs {
		if id == "gate" || id == "sync-check" {
			continue
		}
		for _, required := range gascityRequiredChecks {
			if strings.EqualFold(strings.TrimSpace(job.Name), required) {
				t.Errorf("bazel.yml job %s is named %q, a required check", id, job.Name)
			}
		}
	}
	// sync-check runs whenever the gate does (it never sits behind a lane).
	if sync := wf.Jobs["sync-check"]; sync.If != "" || !reflect.DeepEqual(sync.Needs, "rbe") {
		t.Errorf("sync-check: if %q, needs %v; want unconditional, needing rbe alone (for base-sha)", sync.If, sync.Needs)
	}

	script := multiLaneGateEvaluate(t, wf)
	lanesStep := multiLaneRBEStep(t, wf, "lanes")
	for _, event := range multiLaneEvents {
		for _, mode := range multiLaneModes {
			raw, err := multiLaneLanes(t, lanesStep.Run, event, mode, "false")
			if err != nil {
				t.Fatalf("Lanes step (event %s, mode %s) failed: %v\n%s", event, mode, err, raw)
			}
			var entries []map[string]any
			if err := json.Unmarshal([]byte(raw), &entries); err != nil {
				t.Fatalf("event %s, mode %s: lanes %q: %v", event, mode, raw, err)
			}
			var lanes []string
			for _, e := range entries {
				lanes = append(lanes, fmt.Sprint(e["lane"]))
			}
			if !slices.Contains(lanes, "unit") || !slices.Contains(lanes, "acceptance") {
				t.Errorf("event %s, mode %s: lanes %v; the required gate must fan in unit and acceptance", event, mode, lanes)
			}
			if !multiLaneGatePasses(t, script, event, mode, raw, "success", "success", "success", "success") {
				t.Errorf("event %s, mode %s: the gate fails though every job succeeded", event, mode)
			}
			for _, bad := range []string{"failure", "skipped", "cancelled"} { //nolint:misspell // GitHub Actions job result value
				if multiLaneGatePasses(t, script, event, mode, raw, "success", bad, "success", "success") {
					t.Errorf("event %s, mode %s: the gate passes with lanes %s", event, mode, bad)
				}
			}
			if multiLaneGatePasses(t, script, event, mode, "[]", "success", "skipped", "success", "success") {
				t.Errorf("event %s, mode %s: the gate passes a run with no lane", event, mode)
			}
			if multiLaneGatePasses(t, script, event, mode, raw, "success", "success", "success", "failure") {
				t.Errorf("event %s, mode %s: the gate passes though worker-host failed", event, mode)
			}
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

// multiLaneDecide runs the rbe job's decide step with env (the mint stubbed
// by bazelTestCurlStub) and returns its mode and tier outputs.
func multiLaneDecide(t *testing.T, script string, env map[string]string) (mode, tier string, err error) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(bin, "curl"), bazelTestCurlStub)
	output := filepath.Join(dir, "output")
	full := map[string]string{
		"PATH":                bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GITHUB_OUTPUT":       output,
		"GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary"),
		"GITHUB_REPOSITORY":   "gastownhall/gascity",
		"GITHUB_RUN_ID":       "4242",
		"GITHUB_RUN_ATTEMPT":  "1",
		"BAZEL_TEST_CURL_LOG": filepath.Join(dir, "curl.log"),
		"RBE_VAR_ON":          "true",
		"RBE_INPUT_OFF":       "false",
		"RBE_INPUT_CACHE":     "false",
		"PULL_REQUEST":        "true",
		"FORK":                "false",
		"DEPENDABOT":          "false",
		"HAS_EXECUTOR":        "true",
		"PR_NUMBER":           "6969",
	}
	maps.Copy(full, env)
	out, err := runWorkflowStepScript(t, dir, script, full)
	if err != nil {
		return "", "", fmt.Errorf("%w: %s", err, out)
	}
	mode, _ = readStepOutput(t, output, "mode")
	tier, _ = readStepOutput(t, output, "tier")
	if env["BAZEL_TEST_MINT"] != "" || env["FORK"] == "true" || env["DEPENDABOT"] == "true" {
		if b, _ := os.ReadFile(full["BAZEL_TEST_CURL_LOG"]); len(b) > 0 {
			if !strings.Contains(string(b), rbeForkStatusURL+"gascity&run=4242&attempt=1&pr=6969") ||
				!strings.HasPrefix(string(b), "-sS --connect-timeout 5 --max-time 30 ") {
				t.Errorf("decide asked the mint %q; want the status URL with --connect-timeout 5 --max-time 30", b)
			}
		}
	}
	return mode, tier, nil
}

// TestBazelMultiLaneDecide runs the rbe job's decide step: fork and
// Dependabot pull requests ask rbe-west's mint, and only an open ro or rw
// answer yields a fork mode; anything else (closed, refused, unreachable,
// garbage, a tier that is neither) yields mode cache, whose lanes run here
// on the read-only cache, so such a PR is still tested. rbe-west off
// (vars.RBE_WEST_WORKERS) is mode cache too: no mode leaves a run untested.
func TestBazelMultiLaneDecide(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	step := multiLaneRBEStep(t, wf, "decide")
	for answer, want := range map[string]string{
		"ro": "fork-ro", "rw": "fork-rw", "closed": "cache", "rw-closed": "cache", "canary": "cache",
		"garbage": "cache", "evil": "cache", "unreachable": "cache",
	} {
		for _, who := range []string{"FORK", "DEPENDABOT"} {
			mode, tier, err := multiLaneDecide(t, step.Run, map[string]string{who: "true", "HAS_EXECUTOR": "false", "BAZEL_TEST_MINT": answer})
			if err != nil {
				t.Errorf("%s, mint %s: decide failed: %v", who, answer, err)
				continue
			}
			wantTier := strings.TrimPrefix(want, "fork-")
			if want == "cache" {
				wantTier = ""
			}
			if mode != want || tier != wantTier {
				t.Errorf("%s, mint %s: mode %q tier %q, want mode %q tier %q", who, answer, mode, tier, want, wantTier)
			}
		}
	}
	for name, c := range map[string]struct {
		env  map[string]string
		want string
	}{
		"same-repo PR":         {map[string]string{}, "remote"},
		"push":                 {map[string]string{"PULL_REQUEST": "false"}, "remote"},
		"rbe-west off":         {map[string]string{"RBE_VAR_ON": "false"}, "cache"},
		"rbe-west off, push":   {map[string]string{"RBE_VAR_ON": "false", "PULL_REQUEST": "false"}, "cache"},
		"no executor secret":   {map[string]string{"HAS_EXECUTOR": "false"}, "cache"},
		"rbe=cache dispatch":   {map[string]string{"PULL_REQUEST": "false", "RBE_INPUT_CACHE": "true"}, "cache"},
		"rbe=off dispatch":     {map[string]string{"PULL_REQUEST": "false", "RBE_INPUT_OFF": "true"}, "local"},
		"fork, rbe-west off":   {map[string]string{"FORK": "true", "RBE_VAR_ON": "false", "BAZEL_TEST_MINT": "ro"}, "fork-ro"},
		"fork, mint closed":    {map[string]string{"FORK": "true", "RBE_VAR_ON": "false", "BAZEL_TEST_MINT": "closed"}, "cache"},
		"rbe-west off, rbe=on": {map[string]string{"RBE_VAR_ON": "false", "HAS_EXECUTOR": "false"}, "cache"},
	} {
		mode, _, err := multiLaneDecide(t, step.Run, c.env)
		if err != nil {
			t.Errorf("%s: decide failed: %v", name, err)
			continue
		}
		if mode != c.want {
			t.Errorf("%s: mode %q, want %q", name, mode, c.want)
		}
		if !slices.Contains(multiLaneModes, mode) {
			t.Errorf("%s: mode %q is not one the Lanes step takes (%v)", name, mode, multiLaneModes)
		}
	}
	if _, _, err := multiLaneDecide(t, step.Run, map[string]string{"FORK": "true", "PR_NUMBER": "6969; rm -rf /"}); err == nil {
		t.Errorf("decide accepted a malformed PR number")
	}
}

// TestBazelMultiLaneForkFallback: a fork lane (mode fork-ro/fork-rw) whose
// rbe-fork certificate the mint refuses after the rbe job saw it open falls
// back to the read-only fork cache rather than failing the required gate:
// the first setup-bazel is continue-on-error in the fork modes alone, a
// second one with BAZEL_FORK_CACHE runs exactly when it failed, and the
// steps a locally executing lane needs (Go at /usr/local/go, the zstd
// probe) run for that fallback as for mode cache.
func TestBazelMultiLaneForkFallback(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	lane := wf.Jobs["lane"]
	const failed = "steps.bazel.outcome == 'failure' && startsWith(needs.rbe.outputs.mode, 'fork-')"
	var primary, fallback *multiLaneStep
	setups := 0
	for i, s := range lane.Steps {
		if s.Uses != setupBazelUses {
			continue
		}
		setups++
		switch s.ID {
		case "bazel":
			primary = &lane.Steps[i]
		case "bazel-fallback":
			fallback = &lane.Steps[i]
		}
	}
	if setups != 2 || primary == nil || fallback == nil {
		t.Fatalf("lane job: want setup-bazel steps bazel and bazel-fallback, got %d", setups)
	}
	if primary.ContinueOnError != "${{ startsWith(needs.rbe.outputs.mode, 'fork-') }}" {
		t.Errorf("lane setup-bazel continue-on-error %q; want it in the fork modes alone (a trusted or cache setup failure fails the lane)", primary.ContinueOnError)
	}
	if fallback.If != failed || !reflect.DeepEqual(fallback.Env, map[string]string{"BAZEL_FORK_CACHE": "true"}) || fallback.ContinueOnError != "" {
		t.Errorf("lane fallback setup-bazel: if %q, env %v, continue-on-error %q; want if %q, env BAZEL_FORK_CACHE=true only, failing the lane if it fails",
			fallback.If, fallback.Env, fallback.ContinueOnError, failed)
	}
	local := "needs.rbe.outputs.mode == 'cache' || needs.rbe.outputs.mode == 'local' || steps.bazel-fallback.outcome == 'success'"
	goSteps := 0
	for _, s := range lane.Steps {
		if strings.HasPrefix(s.Uses, "actions/setup-go@") || s.Name == "Provide /usr/local/go for locally run tests" {
			goSteps++
			if s.If != local {
				t.Errorf("lane step %q if %q, want %q", s.Name+s.Uses, s.If, local)
			}
		}
		// Push-to-main steps (the runner cache save) never see a fork mode.
		if strings.Contains(s.If, "steps.bazel.outcome == 'success'") && !strings.Contains(s.If, "steps.bazel-fallback.outcome == 'success'") &&
			!strings.Contains(s.If, "github.event_name == 'push'") {
			t.Errorf("lane step %q keys on the first setup-bazel alone (%q); a fallback lane set Bazel up too", s.Name, s.If)
		}
	}
	if goSteps != 2 {
		t.Errorf("lane job: %d Go steps for locally run tests, want setup-go and the /usr/local/go link", goSteps)
	}
}

// TestBazelMultiLaneCoverageUploadsToCodecov: nightly (schedule) or
// dispatch coverage reports the combined lcov to Codecov under flag
// bazel-unit, kept apart from the go-test arm's flags.
func TestBazelMultiLaneCoverageUploadsToCodecov(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	cov := wf.Jobs["coverage"]
	if !strings.HasPrefix(cov.If, "(github.event_name == 'schedule' || github.event_name == 'workflow_dispatch') && github.ref == 'refs/heads/main'") {
		t.Errorf("coverage if %q; want nightly schedule or dispatch, main only", cov.If)
	}
	n := 0
	for _, s := range cov.Steps {
		if !strings.HasPrefix(s.Uses, "codecov/codecov-action@") {
			continue
		}
		n++
		want := map[string]string{
			"files":   "bazel-out/_coverage/_coverage_report.dat",
			"flags":   "bazel-unit",
			"token":   "${{ secrets.CODECOV_TOKEN }}",
			"verbose": "true",
		}
		if !reflect.DeepEqual(s.With, want) {
			t.Errorf("Codecov upload with %v, want %v", s.With, want)
		}
	}
	if n != 1 {
		t.Errorf("coverage job has %d Codecov uploads, want 1", n)
	}
}

// ciAnalyticsRunTemplateRE finds a GitHub Actions expression (${{ ... }})
// inside a `run:` block: ci_analytics_extract.py's arguments must come from
// $LANE/$MODE/$CHECK_RUN_ID/$PR_HINT shell variables (set in `env:`, which
// GitHub Actions substitutes before the shell ever sees them), never a raw
// ${{ }} expression spliced into the command line itself.
var ciAnalyticsRunTemplateRE = regexp.MustCompile(`\$\{\{`)

// ciAnalyticsUploadPathForbidden rejects an upload `path` that would leak
// the raw BEP file, the uncompressed/compressed exec log, or the profile
// (as opposed to the redacted ci-analytics.json summary this step produces).
var ciAnalyticsUploadPathForbidden = regexp.MustCompile(`bazel-exec\.log|bazel-profile\.json|bazel-bep\.json`)

// TestCIAnalyticsStepsAreSafeAndBounded: the rbe-ci-bep-analytics-design.md
// "CI analytics summary" step (ci_analytics_extract.py) and its upload, in
// both the lane job and the coverage job, must stay strictly
// reporting-only: continue-on-error, a short timeout so a parser bug can
// never make the step (or the job) run long, no raw GitHub Actions
// expression spliced into the shell command (only validated $ENV_VARS,
// never a string this extractor would have to re-validate itself), and an
// upload name/path that can't be confused with the raw artifacts the
// extractor is specifically there to avoid uploading.
func TestCIAnalyticsStepsAreSafeAndBounded(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	const maxTimeoutMinutes = 3

	check := func(jobName string, job multiLaneJob, wantUploadName string) {
		summaryIdx := slices.IndexFunc(job.Steps, func(s multiLaneStep) bool { return s.Name == "CI analytics summary" })
		uploadIdx := slices.IndexFunc(job.Steps, func(s multiLaneStep) bool { return s.Name == "Upload CI analytics summary" })
		if summaryIdx < 0 || uploadIdx < 0 {
			t.Fatalf("%s job: missing CI analytics summary/upload steps", jobName)
		}
		summary := job.Steps[summaryIdx]
		if summary.ContinueOnError != "true" {
			t.Errorf("%s job %q: continue-on-error %q, want \"true\"", jobName, summary.Name, summary.ContinueOnError)
		}
		if summary.TimeoutMinutes <= 0 || summary.TimeoutMinutes > maxTimeoutMinutes {
			t.Errorf("%s job %q: timeout-minutes %d, want 1-%d", jobName, summary.Name, summary.TimeoutMinutes, maxTimeoutMinutes)
		}
		if ciAnalyticsRunTemplateRE.MatchString(summary.Run) {
			t.Errorf("%s job %q: run: contains a ${{ }} expression; use env: and a shell variable instead:\n%s", jobName, summary.Name, summary.Run)
		}
		//nolint:misspell // GitHub Actions spells it cancelled()
		if !strings.Contains(summary.If, "!cancelled()") {
			t.Errorf("%s job %q: if %q does not require !cancelled(); always() also runs after a workflow cancellation", jobName, summary.Name, summary.If)
		}

		upload := job.Steps[uploadIdx]
		if upload.ContinueOnError != "true" {
			t.Errorf("%s job %q: continue-on-error %q, want \"true\"", jobName, upload.Name, upload.ContinueOnError)
		}
		//nolint:misspell // GitHub Actions spells it cancelled()
		if !strings.Contains(upload.If, "!cancelled()") {
			t.Errorf("%s job %q: if %q does not require !cancelled()", jobName, upload.Name, upload.If)
		}
		if upload.With["name"] != wantUploadName {
			t.Errorf("%s job %q: upload name %q, want %q", jobName, upload.Name, upload.With["name"], wantUploadName)
		}
		if ciAnalyticsUploadPathForbidden.MatchString(upload.With["path"]) {
			t.Errorf("%s job %q: upload path %q names a raw artifact (exec log, profile, or BEP), not the redacted summary", jobName, upload.Name, upload.With["path"])
		}
	}

	check("lane", wf.Jobs["lane"], "ci-analytics-${{ matrix.lane }}-${{ github.run_attempt }}")
	check("coverage", wf.Jobs["coverage"], "ci-analytics-coverage-${{ github.run_attempt }}")

	// CHECK_RUN_ID must come from the real job.check_run_id context, not a
	// github.run_id substitution (the design's contract with S4; actionlint
	// recognized job.check_run_id starting at v1.7.8).
	for _, jobName := range []string{"lane", "coverage"} {
		job := wf.Jobs[jobName]
		idx := slices.IndexFunc(job.Steps, func(s multiLaneStep) bool { return s.Name == "CI analytics summary" })
		if idx < 0 {
			t.Fatalf("%s job: missing CI analytics summary step", jobName)
		}
		if got := job.Steps[idx].Env["CHECK_RUN_ID"]; got != "${{ job.check_run_id }}" {
			t.Errorf("%s job CI analytics summary: CHECK_RUN_ID %q, want %q", jobName, got, "${{ job.check_run_id }}")
		}
	}
}

// TestBazelWorkerHostFetchAndParityAlwaysRun: during S3-S5, the worker-host
// job fetches the PR's pinned rbe-worker and checks tree parity on every
// run, pin-moved or not (fix 3); only the H5 range check and the measure
// step stay gated. A permissive "accept either" if: would let this job
// silently stop fetching once nobody notices a tightened measure-only
// gate; pin the exact conditions instead.
func TestBazelWorkerHostFetchAndParityAlwaysRun(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	job, ok := wf.Jobs["worker-host"]
	if !ok {
		t.Fatalf("%s: no worker-host job", bazelMultiLaneWorkflow)
	}
	byName := map[string]multiLaneStep{}
	for _, s := range job.Steps {
		byName[s.Name] = s
	}

	fetch, ok := byName["Fetch the PR's pinned rbe-worker"]
	if !ok {
		t.Fatalf("worker-host: no %q step", "Fetch the PR's pinned rbe-worker")
	}
	if fetch.If != "" {
		t.Errorf("worker-host fetch step: if %q, want unconditional (runs on every worker-host run, S3-S5)", fetch.If)
	}

	parity, ok := byName["In-tree and pinned trees are identical (S3-S5 only)"]
	if !ok {
		t.Fatalf("worker-host: no %q step", "In-tree and pinned trees are identical (S3-S5 only)")
	}
	if parity.If != "" {
		t.Errorf("worker-host parity step: if %q, want unconditional (runs on every worker-host run, S3-S5)", parity.If)
	}

	measure, ok := byName["Measure this Blacksmith host against the PR's manifest"]
	if !ok {
		t.Fatalf("worker-host: no %q step", "Measure this Blacksmith host against the PR's manifest")
	}
	wantMeasureIf := "steps.host.outputs.measure == 'true' || steps.pins.outputs.pin-changed == 'true'"
	if measure.If != wantMeasureIf {
		t.Errorf("worker-host measure step: if %q, want %q", measure.If, wantMeasureIf)
	}
	if measure.Env["RBE_WORKER_REVISION"] != "${{ steps.rbe-worker.outputs.sha }}" {
		t.Errorf("worker-host measure step: RBE_WORKER_REVISION %q, want the fetched sha (pinned mode must log a real sha, not in-tree)", measure.Env["RBE_WORKER_REVISION"])
	}

	h5, ok := byName["Every commit in the bump range is signed, merged and verified (H5)"]
	if !ok {
		t.Fatalf("worker-host: no H5 step")
	}
	if h5.If != "steps.pins.outputs.pin-changed == 'true'" {
		t.Errorf("worker-host H5 step: if %q, want gated on pin-changed only", h5.If)
	}
	if !strings.Contains(h5.Run, ".github/actions/rbe-worker/h5.sh") {
		t.Errorf("worker-host H5 step: run %q, want it to call .github/actions/rbe-worker/h5.sh", h5.Run)
	}
	// S3 re-review: values pass through env:, not ${{ }} interpolated
	// straight into run: (an attacker-controlled pin-range value spliced
	// into the shell command line, rather than handed to it as an
	// environment variable, is a shell-injection risk h5.sh's own args
	// never needed to worry about until the workflow started doing this).
	if strings.Contains(h5.Run, "${{") {
		t.Errorf("worker-host H5 step: run %q contains ${{ }} interpolation; pass values through env: instead", h5.Run)
	}
	wantH5Env := map[string]string{
		"WORKER_REPO_DIR": "${{ steps.rbe-worker.outputs.dir }}",
		"OLD_SHA":         "${{ steps.pins.outputs.old }}",
		"NEW_SHA":         "${{ steps.rbe-worker.outputs.sha }}",
	}
	for k, want := range wantH5Env {
		if got := h5.Env[k]; got != want {
			t.Errorf("worker-host H5 step: env[%s] = %q, want %q", k, got, want)
		}
	}
}

// TestBazelLaneHasNoOwnMeasureStep: fix 10, now that worker-host (fix 3)
// measures this host on every pin bump: the lane job's own Blacksmith
// measure step (bazel.yml §5.4's old location) is gone, not duplicated.
func TestBazelLaneHasNoOwnMeasureStep(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	lane, ok := wf.Jobs["lane"]
	if !ok {
		t.Fatalf("%s: no lane job", bazelMultiLaneWorkflow)
	}
	for _, s := range lane.Steps {
		if strings.Contains(s.Run, "blacksmith-worker.sh") {
			t.Errorf("lane job step %q still runs blacksmith-worker.sh; worker-host (fix 3) now covers this measurement", s.Name)
		}
	}
}
