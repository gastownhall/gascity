package scripts_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type ciCriticalPathWorkflow struct {
	Jobs map[string]ciCriticalPathJob `yaml:"jobs"`
}

type ciCriticalPathJob struct {
	Name            string                    `yaml:"name"`
	If              string                    `yaml:"if"`
	RunsOn          string                    `yaml:"runs-on"`
	TimeoutMinutes  string                    `yaml:"timeout-minutes"`
	Needs           ciCriticalPathNeeds       `yaml:"needs"`
	Outputs         map[string]string         `yaml:"outputs"`
	Steps           []ciCriticalPathStep      `yaml:"steps"`
	Strategy        ciCriticalPathJobStrategy `yaml:"strategy"`
	ContinueOnError bool                      `yaml:"continue-on-error"`
}

type ciCriticalPathJobStrategy struct {
	FailFast *bool                   `yaml:"fail-fast"`
	Matrix   ciCriticalPathJobMatrix `yaml:"matrix"`
}

type ciCriticalPathJobMatrix struct {
	Include []ciCriticalPathMatrixEntry `yaml:"include"`
	Shard   []int                       `yaml:"shard"`
	Keys    []string                    `yaml:"-"`
}

type ciCriticalPathMatrixEntry struct {
	ShardName string `yaml:"shard_name"`
	Command   string `yaml:"command"`
}

type ciCriticalPathNeeds []string

type ciCriticalPathStep struct {
	Name            string            `yaml:"name"`
	ID              string            `yaml:"id"`
	If              string            `yaml:"if"`
	Run             string            `yaml:"run"`
	Uses            string            `yaml:"uses"`
	ContinueOnError bool              `yaml:"continue-on-error"`
	Env             map[string]string `yaml:"env"`
	With            map[string]string `yaml:"with"`
}

const cmdGCProcessExtraTestEnv = `GO_TEST_TIMING_FILE="$${GO_TEST_TIMING_FILE}" GO_TEST_TIMING_NAME="$${GO_TEST_TIMING_NAME}" GO_TEST_TIMING_VARIANT="$${GO_TEST_TIMING_VARIANT}" GO_TEST_RUNNER_LABEL="$${GO_TEST_RUNNER_LABEL}" GC_TEST_FAILURE_ARTIFACT_DIR="$${GC_TEST_FAILURE_ARTIFACT_DIR}" GITHUB_SHA="$${GITHUB_SHA}" GITHUB_WORKFLOW="$${GITHUB_WORKFLOW}" GITHUB_RUN_ID="$${GITHUB_RUN_ID}" GITHUB_RUN_ATTEMPT="$${GITHUB_RUN_ATTEMPT}" GITHUB_JOB="$${GITHUB_JOB}" RUNNER_NAME="$${RUNNER_NAME}" RUNNER_OS="$${RUNNER_OS}" RUNNER_ARCH="$${RUNNER_ARCH}"`

func TestWorkerCorePhase2RunsUnderBazel(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}

	recipe := regexp.MustCompile(`(?m)^test-worker-core-phase2-all:\n((?:\t[^\n]+\n?)+)`).FindStringSubmatch(string(makefile))
	if len(recipe) != 2 {
		t.Fatal("Makefile has no test-worker-core-phase2-all target")
	}
	lines := strings.Split(strings.TrimSuffix(recipe[1], "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimPrefix(lines[i], "\t")
	}
	wantCommands := []string{
		`$(TEST_ENV) PROFILE="$${PROFILE-}" GC_WORKER_REPORT_DIR="$${GC_WORKER_REPORT_DIR-}" go test -count=1 ./internal/worker/workertest ./internal/runtime/tmux -run '^TestPhase2'`,
		`$(TEST_ENV) PROFILE="$${PROFILE-}" GC_WORKER_REPORT_DIR="$${GC_WORKER_REPORT_DIR-}" go test -count=1 -tags integration ./cmd/gc -run '^TestPhase2(StartupMaterialization|InitialInputDelivery|InputResultFailureClassification|WorkerCoreRealTransportProof)$$'`,
	}
	if !slices.Equal(lines, wantCommands) {
		t.Fatalf("test-worker-core-phase2-all commands:\n%q\nwant exactly:\n%q", lines, wantCommands)
	}

	// CI runs the conformance under Bazel only, with PROFILE unset so every
	// profile runs (see the note in ci.yml where the per-profile jobs were).
	wf := readCriticalPathWorkflow(t, "ci.yml")
	for name, job := range wf.Jobs {
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "make test-worker-core") {
				t.Errorf("ci.yml job %s runs %q; worker-core conformance runs under Bazel", name, strings.TrimSpace(step.Run))
			}
		}
		if strings.Contains(job.Name, "Worker core") {
			t.Errorf("ci.yml job %s (%q) is a per-profile worker-core job", name, job.Name)
		}
	}
}

// cmd/gc's process suite runs under Bazel only, for every PR, fork ones
// included: //cmd/gc:gc_test in //test:integration_packages
// (tools/bazel/integration_suite.py lists it), which bazel.yml's gating
// integration-packages lane runs under --config=integration
// (GC_FAST_UNIT=0). No go-test cmd-gc-process job remains (ga-96smfk.50).
func TestCmdGCProcessSuiteRunsInTheBazelIntegrationLane(t *testing.T) {
	root := repoRoot(t)
	if _, ok := readCriticalPathWorkflow(t, "ci.yml").Jobs["cmd-gc-process"]; ok {
		t.Error("CI workflow has a cmd-gc-process go-test job again; bazel.yml's integration-packages lane runs that suite for every PR")
	}
	bazelrc, err := os.ReadFile(filepath.Join(root, ".bazelrc"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^test:integration --test_env=GC_FAST_UNIT=0$`).Match(bazelrc) {
		t.Error(".bazelrc test:integration does not set GC_FAST_UNIT=0; gc_test would skip the process suite")
	}
	bazelYML, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "bazel.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bazelYML), `"cmd":"test --config=ci --config=integration --keep_going //test:integration_packages"`) {
		t.Error("bazel.yml has no integration-packages lane running //test:integration_packages under --config=integration")
	}
}

func TestProductMetricsTesthookProfileIsFocusedAndRequired(t *testing.T) {
	root := repoRoot(t)
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	const owners = "TestProductMetricsTaggedBinaryProcessContracts|TestProductMetricsTesthookEndpointAcceptsOnlyLoopbackHTTPS|TestProductMetricsTaggedRunnerReadsInjectionOnlyAtInvocation|TestProductMetricsTesthookCAReadIsBounded|TestProductMetricsTaggedProcessFixtureIsEnabled|TestProductMetricsTestOnlyCensusEscapeIsNarrow"
	focusedRecipe := regexp.MustCompile(`(?m)^test-productmetrics-testhook:\n\t([^\n]+)$`).FindStringSubmatch(string(makefile))
	if len(focusedRecipe) != 2 {
		t.Fatal("Makefile has no single-command test-productmetrics-testhook target")
	}
	for _, marker := range []string{
		"scripts/go-test-observable test-productmetrics-testhook --",
		"-tags productmetrics_testhook",
		"-count=1",
		"-run '^(" + owners + ")$$'",
		"./cmd/gc",
	} {
		if !strings.Contains(focusedRecipe[1], marker) {
			t.Errorf("test-productmetrics-testhook recipe missing %q:\n%s", marker, focusedRecipe[1])
		}
	}
	serialRecipe := regexp.MustCompile(`(?m)^test-cmd-gc-process:\n((?:\t[^\n]+\n)+)`).FindStringSubmatch(string(makefile))
	if len(serialRecipe) != 2 || !strings.Contains(serialRecipe[1], "$(MAKE) test-productmetrics-testhook") {
		t.Errorf("serial test-cmd-gc-process must retain the focused tagged profile")
	}

	localRunner, err := os.ReadFile(filepath.Join(root, "scripts", "test-local-parallel"))
	if err != nil {
		t.Fatal(err)
	}
	localText := string(localRunner)
	if !strings.Contains(localText, `add_job "productmetrics-testhook" "make test-productmetrics-testhook"`) {
		t.Error("local parallel runner has no focused productmetrics-testhook jobspec")
	}
	if got := strings.Count(localText, "add_productmetrics_testhook_job"); got != 3 {
		t.Errorf("local productmetrics-testhook helper references = %d, want definition plus cmd-gc-process and full", got)
	}

	// CI runs the profile under Bazel only (bazel.yml's unit lane, fork PRs
	// included): gc_test built with the tag, selecting exactly the Makefile's
	// owners.
	wf := readCriticalPathWorkflow(t, "ci.yml")
	for name, job := range wf.Jobs {
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "test-productmetrics-testhook") {
				t.Errorf("ci.yml job %s runs %q; the tagged profile is //cmd/gc:gc_productmetrics_testhook_test", name, strings.TrimSpace(step.Run))
			}
		}
	}
	build, err := os.ReadFile(filepath.Join(root, "cmd", "gc", "BUILD.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	rule := regexp.MustCompile(`(?ms)^go_variant_test\(\n    name = "gc_productmetrics_testhook_test",\n.*?^\)`).FindString(string(build))
	for _, want := range []string{
		`test = ":gc_test",`,
		`gotags = ["productmetrics_testhook"],`,
		`args = ["-test.run=^(` + owners + `)$$"],`,
	} {
		if !strings.Contains(rule, want) {
			t.Errorf("gc_productmetrics_testhook_test lacks %s:\n%s", want, rule)
		}
	}
	if strings.Contains(rule, "manual") {
		t.Errorf("gc_productmetrics_testhook_test must run in //...:\n%s", rule)
	}

	macJob := readCriticalPathWorkflow(t, "mac-regression.yml").Jobs["mac-cmd-gc-process"]
	var macTaggedSteps []ciCriticalPathStep
	for _, step := range macJob.Steps {
		if strings.TrimSpace(step.Run) == "make test-productmetrics-testhook" {
			macTaggedSteps = append(macTaggedSteps, step)
		}
	}
	if len(macTaggedSteps) != 1 || macTaggedSteps[0].If != "${{ matrix.shard == 6 }}" {
		t.Errorf("Mac tagged profile steps = %v, want one execution on historical shard 6", macTaggedSteps)
	}
}

func TestCmdGCProcessTimingEnvCrossesMakeIsolation(t *testing.T) {
	fixture := newGoTestShardFixture(t)
	timingDir := filepath.Join(fixture.tmpDir, "timing artifacts")
	if err := os.Mkdir(timingDir, 0o755); err != nil {
		t.Fatalf("create timing directory: %v", err)
	}
	timingFile := filepath.Join(timingDir, "cmd gc process.json")

	cmd := makeCommand(
		"test-cmd-gc-process-shard",
		"CMD_GC_PROCESS_SHARD=1",
		"CMD_GC_PROCESS_TOTAL=2",
		"EXTRA_TEST_ENV="+cmdGCProcessExtraTestEnv,
	)
	cmd.Dir = fixture.repoRoot
	cmd.Env = []string{
		"PATH=" + fixture.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + fixture.homeDir,
		"SHELL=/bin/sh",
		"LANG=C.UTF-8",
		"TMPDIR=" + fixture.tmpDir,
		"GC_TEST_NO_SLICE=1",
		"SYS_USR_CGO_FALLBACK=0",
		"GO_TEST_TIMING_FILE=" + timingFile,
		"GO_TEST_TIMING_NAME=cmd-gc-process-1-of-2",
		"GO_TEST_TIMING_VARIANT=linux default",
		"GO_TEST_RUNNER_LABEL=blacksmith 32 vcpu",
		"GO_TEST_RUNNER_CPU_COUNT=99",
		"GITHUB_SHA=abc123",
		"GITHUB_WORKFLOW=CI workflow with spaces",
		"GITHUB_RUN_ID=77",
		"GITHUB_RUN_ATTEMPT=2",
		"GITHUB_JOB=cmd gc process",
		"RUNNER_NAME=runner name with spaces",
		"RUNNER_OS=Linux",
		"RUNNER_ARCH=X64",
	}
	status, output := runShardCommand(t, cmd)
	if status == 0 || !strings.Contains(string(output), "Error 23") {
		t.Fatalf("make status = %d, want product failure 23 to remain authoritative\n%s", status, output)
	}

	data, err := os.ReadFile(timingFile)
	if err != nil {
		t.Fatalf("read timing artifact after Make isolation: %v\n%s", err, output)
	}
	var artifact observableTimingArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("decode timing artifact after Make isolation: %v\n%s", err, data)
	}
	if artifact.ShardID != "cmd-gc-process-1-of-2" || artifact.Variant != "linux default" {
		t.Fatalf("timing identity after Make isolation = shard %q variant %q", artifact.ShardID, artifact.Variant)
	}
	if artifact.CommitSHA != "abc123" || artifact.Workflow != "CI workflow with spaces" || artifact.RunID != "77" || artifact.RunAttempt != "2" || artifact.Job != "cmd gc process" {
		t.Fatalf("timing run metadata after Make isolation = %+v", artifact)
	}
	wantRunner := observableTimingRunner{
		Label: "blacksmith 32 vcpu", Name: "runner name with spaces", OS: "Linux", Arch: "X64", CPUCount: 16,
	}
	if artifact.Runner != wantRunner {
		t.Fatalf("timing runner after Make isolation = %+v, want %+v", artifact.Runner, wantRunner)
	}
}

func TestPRTestJobsInstallOnlyRuntimeDependencies(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "ci.yml")

	for _, jobName := range []string{
		"preflight-acceptance",
		"contract-radar-bd-head",
	} {
		job := wf.Jobs[jobName]
		for _, step := range job.Steps {
			if !strings.Contains(step.Uses, "setup-gascity-ubuntu") {
				continue
			}
			if step.With["install-claude-cli"] != "false" {
				t.Errorf("%s installs a live Claude CLI even though PR tests use controlled providers", jobName)
			}
		}
	}
}

func TestAcceptanceJobsUseOnlyTheirHermeticProviderSetup(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "ci.yml")

	// The prev/current bd contract cells run under Bazel
	// (scripts/bd_contract_pins_test.go); the advisory bd main HEAD radar is
	// the one contract job left in ci.yml.
	providerSetupMarker := map[string]string{
		"contract-radar-bd-head": "go -C \"$src\" build",
	}
	for _, jobName := range []string{"contract-radar-bd-head"} {
		job := wf.Jobs[jobName]
		var hasSetupGo bool
		providerSetupIndex := -1
		acceptanceIndex := -1
		for i, step := range job.Steps {
			if strings.Contains(step.Uses, "setup-gascity-ubuntu") {
				t.Errorf("%s uses full-stack setup even though Tier A selects file, subprocess, and skipped-Dolt providers", jobName)
			}
			if strings.Contains(step.Uses, "actions/setup-go") {
				hasSetupGo = true
			}
			if strings.Contains(step.Run, providerSetupMarker[jobName]) {
				providerSetupIndex = i
			}
			if strings.Contains(step.Run, "make test-bd-cli-contract") {
				acceptanceIndex = i
			}
			if strings.TrimSpace(step.Run) == "make test-acceptance-go" {
				t.Errorf("%s step %q repeats broad Tier A instead of the focused bd contract", jobName, step.Name)
			}
		}
		if !hasSetupGo {
			t.Errorf("%s must install the pinned Go toolchain", jobName)
		}
		if providerSetupIndex < 0 {
			t.Errorf("%s does not prepare its bd contract provider", jobName)
		}
		if acceptanceIndex < 0 {
			t.Errorf("%s does not run the Tier A acceptance contract", jobName)
		} else if providerSetupIndex > acceptanceIndex {
			t.Errorf("%s prepares bd at step %d after acceptance at step %d, allowing contract tests to skip", jobName, providerSetupIndex, acceptanceIndex)
		}
	}

	var tierAHasSetupGo, tierARunsBroadSuite bool
	for _, step := range wf.Jobs["preflight-acceptance"].Steps {
		if strings.Contains(step.Uses, "actions/setup-go") {
			tierAHasSetupGo = true
		}
		if strings.TrimSpace(step.Run) == "make test-acceptance-go" {
			tierARunsBroadSuite = true
		}
		if strings.Contains(step.Uses, "setup-gascity-ubuntu") {
			t.Errorf("Tier A uses full-stack setup %q despite selecting controlled providers", step.Uses)
		}
		if strings.Contains(step.Run, "install-bd-archive.sh") {
			t.Errorf("Tier A step %q installs bd even though external CLI contracts have a focused parallel job", step.Name)
		}
		if strings.Contains(step.Run, "test-bd-cli-contract") {
			t.Errorf("Tier A step %q repeats the focused external bd contract", step.Name)
		}
	}
	if !tierAHasSetupGo {
		t.Error("Tier A must install the pinned Go toolchain")
	}
	if !tierARunsBroadSuite {
		t.Error("Tier A must run the broad hermetic acceptance suite")
	}

	check := wf.Jobs["check"]
	if slices.Contains(check.Needs, "contract-radar-bd-head") {
		t.Errorf("Check needs = %v: bd main HEAD radar must remain advisory", check.Needs)
	}
}

func TestAcceptanceTargetsSeparateTierAFromExternalBdContracts(t *testing.T) {
	root := repoRoot(t)
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	makeText := string(makefile)
	if !strings.Contains(makeText, "test-bd-cli-contract:") {
		t.Fatal("Makefile has no focused test-bd-cli-contract target")
	}
	wantTests := []string{"TestBdBasicCRUD", "TestBdDependencies", "TestBdDestructive", "TestBdWorkflow"}
	for _, testName := range wantTests {
		if !strings.Contains(makeText, testName) {
			t.Errorf("focused bd contract target does not name %s", testName)
		}
	}
	for _, marker := range []string{
		"command -v bd",
		"-tags acceptance_bd_contract",
		"-count=1",
		"-run '^(TestBdBasicCRUD|TestBdDependencies|TestBdDestructive|TestBdWorkflow)$$'",
		"./test/acceptance",
	} {
		if !strings.Contains(makeText, marker) {
			t.Errorf("focused bd contract target is missing %q", marker)
		}
	}

	contractTest, err := os.ReadFile(filepath.Join(root, "test", "acceptance", "beads_cli_contract_test.go"))
	if err != nil {
		t.Fatalf("read beads CLI contract test: %v", err)
	}
	firstLine, _, _ := strings.Cut(string(contractTest), "\n")
	if firstLine != "//go:build acceptance_bd_contract" {
		t.Fatalf("beads CLI contract build constraint = %q, want focused acceptance_bd_contract tag", firstLine)
	}
	matches := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(t \*testing\.T\)`).FindAllStringSubmatch(string(contractTest), -1)
	gotTests := make([]string, 0, len(matches))
	for _, match := range matches {
		gotTests = append(gotTests, match[1])
	}
	if !slices.Equal(gotTests, wantTests) {
		t.Fatalf("bd contract tests = %v, want focused manifest %v", gotTests, wantTests)
	}
}

func TestMacAcceptanceRetainsExternalBdContract(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "mac-regression.yml")
	job := wf.Jobs["mac-acceptance"]
	var runsTierA, runsBDContract bool
	for _, step := range job.Steps {
		runsTierA = runsTierA || strings.TrimSpace(step.Run) == "make test-acceptance-go"
		runsBDContract = runsBDContract || strings.TrimSpace(step.Run) == "make test-bd-cli-contract"
	}
	if !runsTierA {
		t.Error("Mac acceptance must retain hermetic Tier A")
	}
	if !runsBDContract {
		t.Error("Mac acceptance must retain the external bd CLI contract split from Tier A")
	}
}

// TestMacRegressionGateCentralizesTierRouting asserts the new centralized
// `gate` job (ga-hd99jq D1 / ga-n7ef4e): it always runs (no `if:`, so an
// all-skipped workflow run can no longer happen), computes one reason string
// plus the three tier booleans other jobs key off of, and mirrors
// review-formulas.yml's own paths-filter + decision-step shape.
func TestMacRegressionGateCentralizesTierRouting(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "mac-regression.yml")

	gate, ok := wf.Jobs["gate"]
	if !ok {
		t.Fatal("mac-regression workflow has no centralized gate job")
	}
	if !slices.Contains(gate.Needs, "runner-policy") {
		t.Errorf("gate needs = %v, want runner-policy", gate.Needs)
	}
	if strings.TrimSpace(gate.If) != "" {
		t.Errorf("gate if = %q, want no condition — the gate itself must always run so every tier job and the summary can read its outputs", strings.TrimSpace(gate.If))
	}
	const gateRunner = "${{ needs.runner-policy.outputs.runner_2vcpu }}"
	if gate.RunsOn != gateRunner {
		t.Errorf("gate runs-on = %q, want %q (routing decision is cheap, keep it off macOS runners)", gate.RunsOn, gateRunner)
	}

	wantOutputs := map[string]string{
		"run_smoke":           "${{ steps.gate.outputs.run_smoke }}",
		"run_full":            "${{ steps.gate.outputs.run_full }}",
		"run_review_formulas": "${{ steps.gate.outputs.run_review_formulas }}",
		"reason":              "${{ steps.gate.outputs.reason }}",
	}
	for name, want := range wantOutputs {
		if got := gate.Outputs[name]; got != want {
			t.Errorf("gate output %s = %q, want %q", name, got, want)
		}
	}

	var filterStep, decideStep, checkoutStep ciCriticalPathStep
	var hasFilter, hasDecide, hasCheckout bool
	for _, step := range gate.Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			checkoutStep, hasCheckout = step, true
		}
		switch step.ID {
		case "filter":
			filterStep, hasFilter = step, true
		case "gate":
			decideStep, hasDecide = step, true
		}
	}
	if !hasCheckout {
		t.Fatal("gate job has no checkout step")
	}
	wantCheckoutWith := map[string]string{
		"repository":          "${{ inputs.head_repo || github.repository }}",
		"ref":                 "${{ inputs.head_sha || github.sha }}",
		"persist-credentials": "false",
	}
	for name, want := range wantCheckoutWith {
		if got := checkoutStep.With[name]; got != want {
			t.Errorf("gate checkout with.%s = %q, want %q (every other mac-regression job pins the same head ref/repo and disables credential persistence; the gate job must not be the odd one out)", name, got, want)
		}
	}
	if !hasFilter {
		t.Fatal("gate job has no paths-filter step (id: filter)")
	}
	if !strings.Contains(filterStep.Uses, "dorny/paths-filter") {
		t.Errorf("gate filter step uses = %q, want dorny/paths-filter (same tool review-formulas.yml uses)", filterStep.Uses)
	}
	wantFilterEntries := []string{"cmd/gc/**", "internal/pathutil/**", "internal/fsys/**", ".github/actions/setup-gascity-macos/**"}
	filterValue := filterStep.With["filters"]
	for _, entry := range wantFilterEntries {
		if !strings.Contains(filterValue, "'"+entry+"'") {
			t.Errorf("gate filter mac_sensitive list missing %q; want exactly %v", entry, wantFilterEntries)
		}
	}
	if gotEntries := regexp.MustCompile(`(?m)^\s*-\s*'[^']*'\s*$`).FindAllString(filterValue, -1); len(gotEntries) != len(wantFilterEntries) {
		t.Errorf("gate filter mac_sensitive has %d path entries, want exactly %d (%v) — no broader glob than the paths that actually touch gc/cmd or fsys/pathutil behavior, or the macOS setup action every mac job runs", len(gotEntries), len(wantFilterEntries), wantFilterEntries)
	}
	if !hasDecide {
		t.Fatal("gate job has no routing-decision step (id: gate)")
	}

	wantDecideEnv := map[string]string{
		"EVENT_NAME":   "${{ github.event_name }}",
		"SUITE_INPUT":  "${{ inputs.suite }}",
		"PR_HEAD_REPO": "${{ github.event.pull_request.head.repo.full_name }}",
		"PR_DRAFT":     "${{ github.event.pull_request.draft }}",
		"NEEDS_LABEL":  "${{ contains(github.event.pull_request.labels.*.name, 'needs-mac') }}",
		"PATH_HIT":     "${{ steps.filter.outputs.mac_sensitive }}",
	}
	for name, want := range wantDecideEnv {
		if got := decideStep.Env[name]; got != want {
			t.Errorf("gate decision step env %s = %q, want %q", name, got, want)
		}
	}

	// Every trigger path the exit_contract enumerates must be handled so the
	// refactor preserves today's per-job run/skip outcome exactly.
	for _, marker := range []string{
		`"$EVENT_NAME" == "schedule"`,
		`run_smoke=true; run_full=true; run_review_formulas=true`,
		`"$EVENT_NAME" == "workflow_dispatch"`,
		`case "$SUITE_INPUT" in`,
		`needs-mac)`,
		`"$EVENT_NAME" == "pull_request"`,
		`"$PR_HEAD_REPO" != "${{ github.repository }}"`,
		`"$PR_DRAFT" == "true"`,
		`"$NEEDS_LABEL" == "true"`,
		`echo "run_smoke=$run_smoke"`,
		`echo "run_full=$run_full"`,
		`echo "run_review_formulas=$run_review_formulas"`,
		`echo "reason=$reason"`,
	} {
		if !strings.Contains(decideStep.Run, marker) {
			t.Errorf("gate decision step run script missing %q", marker)
		}
	}

	// An unrecognized (or default) $SUITE_INPUT on a manual dispatch must
	// still run the smoke tier, not silently run nothing — run_smoke must
	// be set unconditionally before the case statement, not only inside
	// specific case branches, so the catch-all `*)` arm inherits it too.
	const dispatchMarker = `"$EVENT_NAME" == "workflow_dispatch" ]]; then`
	dispatchIdx := strings.Index(decideStep.Run, dispatchMarker)
	if dispatchIdx < 0 {
		t.Fatal("gate decision step run script missing workflow_dispatch branch")
	}
	afterDispatch := decideStep.Run[dispatchIdx+len(dispatchMarker):]
	caseIdx := strings.Index(afterDispatch, `case "$SUITE_INPUT" in`)
	if caseIdx < 0 {
		t.Fatal("gate decision step run script missing case statement in workflow_dispatch branch")
	}
	if preCase := afterDispatch[:caseIdx]; !strings.Contains(preCase, "run_smoke=true") {
		t.Errorf("gate decision step workflow_dispatch branch does not set run_smoke=true before the case statement (preamble %q) — an unrecognized suite input must still default to the smoke tier", preCase)
	}
}

// TestMacRegressionTierJobsGateOnCentralizedOutputs asserts every tier job
// collapses its duplicated multi-line if: into a single check against the
// gate job's own output (ga-hd99jq D1) — a refactor of how the decision is
// computed, not a change to which jobs run when.
func TestMacRegressionTierJobsGateOnCentralizedOutputs(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "mac-regression.yml")

	tests := []struct {
		job    string
		wantIf string
	}{
		{"mac-quality", "needs.gate.outputs.run_smoke == 'true'"},
		{"mac-unit", "needs.gate.outputs.run_smoke == 'true'"},
		{"mac-cmd-gc-process", "needs.gate.outputs.run_smoke == 'true'"},
		{"mac-acceptance", "needs.gate.outputs.run_smoke == 'true'"},
		{"mac-cover", "needs.gate.outputs.run_full == 'true'"},
		{"mac-integration-packages", "needs.gate.outputs.run_full == 'true'"},
		{"mac-integration-bdstore", "needs.gate.outputs.run_full == 'true'"},
		{"mac-integration-rest", "needs.gate.outputs.run_full == 'true'"},
		{"mac-integration-review-formulas", "needs.gate.outputs.run_review_formulas == 'true'"},
	}
	for _, tt := range tests {
		t.Run(tt.job, func(t *testing.T) {
			job, ok := wf.Jobs[tt.job]
			if !ok {
				t.Fatalf("mac-regression workflow has no %s job", tt.job)
			}
			if !slices.Contains(job.Needs, "gate") {
				t.Errorf("%s needs = %v, want gate", tt.job, job.Needs)
			}
			if got := strings.TrimSpace(job.If); got != tt.wantIf {
				t.Errorf("%s if = %q, want exactly %q (single gate-output check, not a duplicated inline expression)", tt.job, got, tt.wantIf)
			}
		})
	}
}

// TestMacRegressionSummaryAlwaysRunsAndReadsGateResult is the core
// signal-integrity fix (ga-wecoe1, ga-hd99jq F1/F2): the summary job must
// never itself be skipped, or an all-skipped run reports green. Its if:
// becomes bare always(), and it must read the gate job's own result/outputs
// rather than re-evaluating the trigger — the fleet's D5 rule.
func TestMacRegressionSummaryAlwaysRunsAndReadsGateResult(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "mac-regression.yml")
	job, ok := wf.Jobs["mac-regression-summary"]
	if !ok {
		t.Fatal("mac-regression workflow has no mac-regression-summary job")
	}

	if got := strings.TrimSpace(job.If); got != "always()" {
		t.Errorf("mac-regression-summary if = %q, want bare always() so the summary itself is never skipped (D5: an all-skipped run must not report green)", got)
	}
	if !slices.Contains(job.Needs, "gate") {
		t.Errorf("mac-regression-summary needs = %v, want gate", job.Needs)
	}

	var summarize ciCriticalPathStep
	var found bool
	for _, step := range job.Steps {
		if step.Name == "Summarize" {
			summarize, found = step, true
		}
	}
	if !found {
		t.Fatal("mac-regression-summary has no Summarize step")
	}

	wantEnv := map[string]string{
		"GATE_RESULT": "${{ needs.gate.result }}",
		"RUN_SMOKE":   "${{ needs.gate.outputs.run_smoke }}",
		"REASON":      "${{ needs.gate.outputs.reason }}",
	}
	for name, want := range wantEnv {
		if got := summarize.Env[name]; got != want {
			t.Errorf("Summarize env %s = %q, want %q", name, got, want)
		}
	}

	for _, marker := range []string{
		`"${GATE_RESULT}" != "success"`,
		`"${RUN_SMOKE}" != "true"`,
		`Mac Regression: not requested`,
	} {
		if !strings.Contains(summarize.Run, marker) {
			t.Errorf("Summarize run script missing %q (gate-failed / not-requested branch from the exit_contract)", marker)
		}
	}
}

// TestMacRegressionHeaderCommentDescribesCentralizedGate asserts the file
// header (originally documenting "each job copies the expression; keep them
// in sync") is updated to describe the centralized-gate shape that removes
// that exact duplication.
func TestMacRegressionHeaderCommentDescribesCentralizedGate(t *testing.T) {
	path := filepath.Join(repoRoot(t), ".github", "workflows", "mac-regression.yml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	content := string(body)
	if strings.Contains(content, "each job copies the") {
		t.Error("mac-regression.yml header comment still describes the old per-job duplicated if: expression — update it to describe the centralized gate job")
	}
	if !strings.Contains(content, "gate") {
		t.Error("mac-regression.yml header comment should describe the centralized gate job")
	}
}

func TestStaticChecksUseOnlyTheGoToolchain(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "ci.yml")
	job := wf.Jobs["preflight-static"]
	var hasSetupGo bool
	for _, step := range job.Steps {
		if strings.Contains(step.Uses, "actions/setup-go") {
			hasSetupGo = true
			if step.With["go-version-file"] != "go.mod" {
				t.Errorf("static checks setup-go version file = %q, want go.mod", step.With["go-version-file"])
			}
		}
		if strings.Contains(step.Uses, "setup-gascity-ubuntu") || strings.Contains(step.Uses, "actions/setup-node") {
			t.Errorf("static checks use unnecessary full-stack dependency setup %q", step.Uses)
		}
		if strings.Contains(step.Run, "make install-tools") {
			t.Errorf("static checks step %q installs oapi-codegen even though generated-artifact CI owns it", step.Name)
		}
	}
	if !hasSetupGo {
		t.Error("static checks must install the pinned Go toolchain")
	}
}

// TestLintAndVetRunAsNogoInBazel pins where lint and vet gate: nogo
// (//tools/nogo) validates every Go compile in bazel.yml's lanes, so no ci.yml
// job runs golangci-lint or go vet a second time.
func TestLintAndVetRunAsNogoInBazel(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "ci.yml")
	for jobName, job := range wf.Jobs {
		for _, step := range job.Steps {
			run := step.Run
			for _, legacy := range []string{"make lint", "make vet", "golangci-lint", "go vet"} {
				if strings.Contains(run, legacy) {
					t.Errorf("ci.yml %s step %q runs %q; lint and vet are nogo in the Bazel build", jobName, step.Name, legacy)
				}
			}
		}
	}

	module, err := os.ReadFile(filepath.Join(repoRoot(t), "MODULE.bazel"))
	if err != nil {
		t.Fatalf("read MODULE.bazel: %v", err)
	}
	if !strings.Contains(string(module), "go_sdk.nogo(") || !strings.Contains(string(module), `nogo = "//tools/nogo"`) {
		t.Error(`MODULE.bazel must register //tools/nogo with go_sdk.nogo; without it lint and vet gate nowhere`)
	}
}

func TestCIPreflightFansInDirectlyWithoutWaitingForHistoricalCheck(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "ci.yml")
	if got := wf.Jobs["check"].Name; got != "Check" {
		t.Errorf("historical branch-protection job name = %q, want Check", got)
	}
	job := wf.Jobs["ci-preflight"]
	if slices.Contains(job.Needs, "check") {
		t.Errorf("ci-preflight needs = %v: historical Check fan-in adds a serialized job", job.Needs)
	}
	for _, need := range []string{
		"runner-policy",
		"changes",
		"preflight-static",
		"preflight-acceptance",
		"release-config",
	} {
		if !slices.Contains(job.Needs, need) {
			t.Errorf("ci-preflight needs = %v, want direct dependency %q", job.Needs, need)
		}
	}
	if !slices.Contains(wf.Jobs["ci-required"].Needs, "ci-preflight") {
		t.Errorf("ci-required needs = %v, want ci-preflight aggregate", wf.Jobs["ci-required"].Needs)
	}
}

func TestPRIntegrationMatrixKeepsHeavyRestCoverageInReleaseGates(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "ci.yml")
	// bazel.yml's gating integration-packages and integration-smoke lanes run
	// the former packages-*, bdstore and rest-smoke Go shards for every PR,
	// fork ones included (ga-96smfk.50); no Go integration-shards job remains.
	if _, ok := wf.Jobs["integration-shards"]; ok {
		t.Error("CI workflow has an integration-shards job again; bazel.yml's integration-packages and integration-smoke lanes cover it")
	}

	full, ok := wf.Jobs["integration-rest-full"]
	if !ok {
		t.Fatal("CI workflow must retain rest-full as a post-merge safety net")
	}
	if !strings.Contains(full.If, "github.event_name == 'push'") {
		t.Errorf("integration-rest-full condition = %q, want push-only coverage", full.If)
	}
	if want := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}; !slices.Equal(full.Strategy.Matrix.Shard, want) {
		t.Errorf("integration-rest-full shards = %v, want %v", full.Strategy.Matrix.Shard, want)
	}
	var runsFullREST bool
	for _, step := range full.Steps {
		if strings.Contains(step.Run, "test-integration-shard rest-full-") {
			runsFullREST = true
		}
	}
	if !runsFullREST {
		t.Error("integration-rest-full must execute the sharded full REST suite")
	}

	aggregator := wf.Jobs["ci-integration"]
	if !slices.Contains(aggregator.Needs, "integration-rest-full") {
		t.Errorf("ci-integration needs = %v, want post-merge REST coverage included in the aggregate", aggregator.Needs)
	}
	var permitsPRSkip bool
	for _, step := range aggregator.Steps {
		if strings.Contains(step.Run, "allow_skipped") && strings.Contains(step.Run, `"integration-rest-full"`) {
			permitsPRSkip = true
		}
	}
	if !permitsPRSkip {
		t.Error("ci-integration must treat the push-only REST job as an expected skip on pull requests")
	}
}

// restFullMinTimeoutMinutes is the floor for the integration-rest-full job cap.
// The 16 shards split the suite by position, so #6821's three new tests moved
// every later one: shard 6 went from 7-9m to 13-15m and 6 of 11 main pushes were
// canceled at the old 15m cap with no regression behind them (ga-rqzwrh). Lower
// the cap only with per-test timings from the shard logs in hand.
const restFullMinTimeoutMinutes = 25

func TestIntegrationRestFullJobCapFitsMeasuredShardRuntime(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "ci.yml")
	full, ok := wf.Jobs["integration-rest-full"]
	if !ok {
		t.Fatal("CI workflow must retain rest-full as a post-merge safety net")
	}
	got, err := strconv.Atoi(full.TimeoutMinutes)
	if err != nil {
		t.Fatalf("integration-rest-full timeout-minutes = %q, want a literal number of minutes: %v", full.TimeoutMinutes, err)
	}
	if got < restFullMinTimeoutMinutes {
		t.Errorf("integration-rest-full timeout-minutes = %d, want at least %d: the slowest passing shard ran 13-15m, so a lower cap cancels main on a redistribution", got, restFullMinTimeoutMinutes)
	}
}

func (n *ciCriticalPathNeeds) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		*n = []string{node.Value}
		return nil
	}
	var values []string
	if err := node.Decode(&values); err != nil {
		return err
	}
	*n = values
	return nil
}

func (m *ciCriticalPathJobMatrix) UnmarshalYAML(node *yaml.Node) error {
	type plainMatrix ciCriticalPathJobMatrix
	var decoded plainMatrix
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*m = ciCriticalPathJobMatrix(decoded)
	for i := 0; i+1 < len(node.Content); i += 2 {
		m.Keys = append(m.Keys, node.Content[i].Value)
	}
	return nil
}

func TestForkVerifyRunsOnlyInForks(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "fork-verify.yml")
	job, ok := wf.Jobs["verify"]
	if !ok {
		t.Fatal("fork-verify workflow has no verify job")
	}

	const want = "${{ github.repository != 'gastownhall/gascity' }}"
	if strings.TrimSpace(job.If) != want {
		t.Fatalf("fork verify job condition = %q, want %q so canonical PRs do not duplicate CI", job.If, want)
	}
}

func TestPackGateAddsOnlyParallelPackCoverage(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "ci.yml")
	job, ok := wf.Jobs["pack-gate"]
	if !ok {
		t.Fatal("CI workflow has no pack-gate job")
	}

	for _, need := range []string{"runner-policy", "changes"} {
		if !slices.Contains(job.Needs, need) {
			t.Errorf("pack-gate needs = %v, want routing dependency %q", job.Needs, need)
		}
	}
	if slices.Contains(job.Needs, "check") {
		t.Errorf("pack-gate needs = %v: pack checks must run alongside preflight, not after it", job.Needs)
	}

	var checksBundledPin, smokesLiveRegistry bool
	for _, step := range job.Steps {
		if strings.Contains(step.Uses, "setup-gascity-ubuntu") {
			t.Errorf("pack-gate uses full-stack setup %q for Go-only focused checks", step.Uses)
		}
		if strings.Contains(step.Run, "make test-acceptance-go") {
			t.Errorf("pack-gate step %q repeats the required preflight acceptance suite", step.Name)
		}
		if strings.Contains(step.Run, "make install-tools") {
			t.Errorf("pack-gate step %q installs tools unused by its focused checks", step.Name)
		}
		if strings.Contains(step.Run, "update-bundled-gastown-pack --check") {
			checksBundledPin = true
		}
		if strings.Contains(step.Run, "make test-pack-registry-live") {
			smokesLiveRegistry = true
		}
	}
	if !checksBundledPin {
		t.Error("pack-gate must retain the bundled-pack provenance check")
	}
	if !smokesLiveRegistry {
		t.Error("pack-gate must retain the live registry/materialization smoke test")
	}
}

func TestGoReleaserOutputCannotDirtyReleaseBuilds(t *testing.T) {
	gitignorePath := filepath.Join(repoRoot(t), ".gitignore")
	body, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf("read %s: %v", gitignorePath, err)
	}

	var ignoresRootDist bool
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "/dist/" {
			ignoresRootDist = true
			break
		}
	}
	if !ignoresRootDist {
		t.Fatal("root .gitignore must contain anchored /dist/ so GoReleaser metadata cannot set vcs.modified=true")
	}
}

func TestReleasePipelinesVerifyExactBinaryMetadata(t *testing.T) {
	tests := []struct {
		name               string
		workflow           string
		job                string
		wantCommitResolver string
		wantVersionArg     string
	}{
		{
			name:               "release",
			workflow:           "release.yml",
			job:                "release",
			wantCommitResolver: `git rev-parse "${GITHUB_REF_NAME}^{commit}"`,
			wantVersionArg:     `"${GITHUB_REF_NAME#v}"`,
		},
		{
			name:               "rc gate snapshot",
			workflow:           "rc-gate.yml",
			job:                "ubuntu_goreleaser_snapshot",
			wantCommitResolver: "git rev-parse HEAD",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf := readCriticalPathWorkflow(t, tt.workflow)
			job, ok := wf.Jobs[tt.job]
			if !ok {
				t.Fatalf("workflow %s has no %s job", tt.workflow, tt.job)
			}

			goreleaserIndex := -1
			ignoreCheckIndex := -1
			metadataCheckIndex := -1
			var metadataCheck ciCriticalPathStep
			for i, step := range job.Steps {
				if strings.HasPrefix(step.Uses, "goreleaser/goreleaser-action@") {
					goreleaserIndex = i
				}
				if strings.Contains(step.Run, "make check-release-dist-ignore") {
					ignoreCheckIndex = i
				}
				if step.Name == "Verify release binary metadata" {
					metadataCheckIndex = i
					metadataCheck = step
				}
			}

			if goreleaserIndex < 0 {
				t.Fatal("GoReleaser step not found")
			}
			if ignoreCheckIndex < 0 || ignoreCheckIndex >= goreleaserIndex {
				t.Fatalf("release-output ignore check index = %d, want before GoReleaser index %d", ignoreCheckIndex, goreleaserIndex)
			}
			if metadataCheckIndex <= goreleaserIndex {
				t.Fatalf("binary metadata check index = %d, want after GoReleaser index %d", metadataCheckIndex, goreleaserIndex)
			}
			if metadataCheck.ContinueOnError {
				t.Fatal("binary metadata verification must block release progression")
			}
			if !strings.Contains(metadataCheck.Run, "scripts/verify-release-binary-metadata.sh") {
				t.Fatal("binary metadata verification must use the shared checker")
			}
			if !strings.Contains(metadataCheck.Run, tt.wantCommitResolver) {
				t.Errorf("binary metadata verification does not resolve the expected commit with %q", tt.wantCommitResolver)
			}
			if tt.wantVersionArg != "" && !strings.Contains(metadataCheck.Run, tt.wantVersionArg) {
				t.Errorf("binary metadata verification does not require release version %s", tt.wantVersionArg)
			}
		})
	}
}

func readCriticalPathWorkflow(t *testing.T, name string) ciCriticalPathWorkflow {
	t.Helper()

	path := filepath.Join(repoRoot(t), ".github", "workflows", name)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var wf ciCriticalPathWorkflow
	if err := yaml.Unmarshal(body, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return wf
}

// Fork main branches receive every upstream fast-forward, but they do not and
// should not carry the canonical repository's cross-repository dispatch token.
// Keep that expected fork run green while preserving a hard failure if the
// canonical publisher loses its secret or dispatch permission.
func TestNotifyImageRebuildsRunsOnlyInCanonicalRepository(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "notify-image-build.yaml")
	notify, ok := wf.Jobs["notify"]
	if !ok {
		t.Fatal("notify-image-build workflow has no notify job")
	}
	if got, want := notify.If, "github.repository == 'gastownhall/gascity'"; got != want {
		t.Fatalf("notify job if = %q, want %q", got, want)
	}
}
