package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type ciCriticalPathWorkflow struct {
	Jobs map[string]ciCriticalPathJob `yaml:"jobs"`
}

type ciCriticalPathJob struct {
	Name     string                    `yaml:"name"`
	If       string                    `yaml:"if"`
	Needs    ciCriticalPathNeeds       `yaml:"needs"`
	Steps    []ciCriticalPathStep      `yaml:"steps"`
	Strategy ciCriticalPathJobStrategy `yaml:"strategy"`
}

type ciCriticalPathJobStrategy struct {
	Matrix ciCriticalPathJobMatrix `yaml:"matrix"`
}

type ciCriticalPathJobMatrix struct {
	Include []ciCriticalPathMatrixEntry `yaml:"include"`
	Shard   []int                       `yaml:"shard"`
}

type ciCriticalPathMatrixEntry struct {
	ShardName string `yaml:"shard_name"`
	Command   string `yaml:"command"`
}

type ciCriticalPathNeeds []string

type ciCriticalPathStep struct {
	Name string            `yaml:"name"`
	Run  string            `yaml:"run"`
	Uses string            `yaml:"uses"`
	With map[string]string `yaml:"with"`
}

func TestPRTestJobsInstallOnlyRuntimeDependencies(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "ci.yml")

	for _, jobName := range []string{"cmd-gc-process", "integration-shards", "docker-session"} {
		job, ok := wf.Jobs[jobName]
		if !ok {
			t.Errorf("CI workflow has no %s job", jobName)
			continue
		}
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "make install-tools") {
				t.Errorf("%s step %q installs lint/codegen tools already owned by preflight", jobName, step.Name)
			}
		}
	}

	for _, jobName := range []string{
		"preflight-acceptance",
		"contract-acceptance-current",
		"contract-radar-bd-head",
		"cmd-gc-process",
		"integration-shards",
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

	providerSetupMarker := map[string]string{
		"contract-acceptance-previous": "install-bd-archive.sh",
		"contract-acceptance-current":  "go -C \"$src\" build",
		"contract-radar-bd-head":       "go -C \"$src\" build",
	}
	for _, jobName := range []string{"contract-acceptance-previous", "contract-acceptance-current", "contract-radar-bd-head"} {
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
			if strings.TrimSpace(step.Run) == "make test-acceptance" {
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

	var previousBDInstalled bool
	for _, step := range wf.Jobs["contract-acceptance-previous"].Steps {
		if strings.Contains(step.Run, "install-bd-archive.sh") && strings.Contains(step.Run, "BD_PREV_VERSION") {
			previousBDInstalled = true
		}
	}
	if !previousBDInstalled {
		t.Error("previous-bd contract job must install the deps.env minimum-supported bd so CLI contract tests cannot silently skip")
	}

	var tierAHasSetupGo, tierARunsBroadSuite bool
	for _, step := range wf.Jobs["preflight-acceptance"].Steps {
		if strings.Contains(step.Uses, "actions/setup-go") {
			tierAHasSetupGo = true
		}
		if strings.TrimSpace(step.Run) == "make test-acceptance" {
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
	for _, need := range []string{"contract-acceptance-previous", "contract-acceptance-current"} {
		if !slices.Contains(check.Needs, need) {
			t.Errorf("Check needs = %v, want required bd contract %q", check.Needs, need)
		}
	}
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
		runsTierA = runsTierA || strings.TrimSpace(step.Run) == "make test-acceptance"
		runsBDContract = runsBDContract || strings.TrimSpace(step.Run) == "make test-bd-cli-contract"
	}
	if !runsTierA {
		t.Error("Mac acceptance must retain hermetic Tier A")
	}
	if !runsBDContract {
		t.Error("Mac acceptance must retain the external bd CLI contract split from Tier A")
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
		"preflight-generated",
		"contract-acceptance-previous",
		"contract-acceptance-current",
		"release-config",
		"dashboard",
	} {
		if !slices.Contains(job.Needs, need) {
			t.Errorf("ci-preflight needs = %v, want direct dependency %q", job.Needs, need)
		}
	}
	var permitsCurrentContractSkip bool
	for _, step := range job.Steps {
		if strings.Contains(step.Run, "allow_skipped") && strings.Contains(step.Run, `"contract-acceptance-current"`) {
			permitsCurrentContractSkip = true
		}
	}
	if !permitsCurrentContractSkip {
		t.Error("ci-preflight must allow the path-gated current-bd contract to skip")
	}
	if !slices.Contains(wf.Jobs["ci-required"].Needs, "ci-preflight") {
		t.Errorf("ci-required needs = %v, want ci-preflight aggregate", wf.Jobs["ci-required"].Needs)
	}
}

func TestPRIntegrationMatrixKeepsHeavyRestCoverageInReleaseGates(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "ci.yml")
	var cmdGCRows, restSmokeRows []string
	for _, entry := range wf.Jobs["integration-shards"].Strategy.Matrix.Include {
		if strings.Contains(entry.Command, "rest-full") {
			t.Errorf("PR integration shard %q runs rest-full; Makefile assigns that suite to nightly/RC and targeted validation", entry.ShardName)
		}
		if strings.Contains(entry.Command, "packages-cmd-gc-") {
			cmdGCRows = append(cmdGCRows, entry.Command)
		}
		if strings.Contains(entry.Command, "rest-smoke-") {
			restSmokeRows = append(restSmokeRows, entry.Command)
		}
	}
	if want := []string{"./scripts/test-integration-shard packages-cmd-gc-integration"}; !slices.Equal(cmdGCRows, want) {
		t.Errorf("PR cmd/gc integration rows = %v, want one focused integration-only row %v", cmdGCRows, want)
	}
	if want := []string{
		"./scripts/test-integration-shard rest-smoke-1-of-2",
		"./scripts/test-integration-shard rest-smoke-2-of-2",
	}; !slices.Equal(restSmokeRows, want) {
		t.Errorf("PR REST smoke rows = %v, want %v", restSmokeRows, want)
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
		if strings.Contains(step.Run, "make test-acceptance") {
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
