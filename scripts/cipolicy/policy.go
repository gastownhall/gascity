// Package cipolicy validates the execution-affecting shape of required CI workflows.
package cipolicy

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

const (
	// These are SHA-256 digests of the display-free JSON projections below.
	// Whole-workflow execution hashes deliberately pin shell text instead of
	// approximating shell semantics: any execution change requires explicit
	// policy review, while workflow, job, step, and input descriptions remain
	// free to change. A failure prints the projection and candidate digest.
	expectedCITriggersHash = "d1a8bcd089019589658d8f154af9c26a70877285d84a384c2dcea299efc9554a"
	// Bumped for the beads-topology-acceptance job: the bd/dolt-backed topology
	// shapes had never executed in CI — every job lacked a bd with
	// --proxied-server, so each test skipped and a suite that ran nothing
	// reported green. The new job builds bd from BD_CURRENT_REF and sets
	// GC_REQUIRE_ACCEPTANCE_TOOLING so a runner without that bd fails instead.
	//
	// Bumped again to widen that job's beads_topology path filter. The curated
	// cmd/gc globs matched none of the files the proxied lifecycle actually lives
	// in — the ownership journal, the provider lifecycle, the bd env plumbing,
	// the `gc init` transport flags — so a change to the feature skipped its own
	// acceptance job and ci-required still went green on the allowed skip. The
	// filter is now cmd/gc/**, internal/beads/**, internal/doctor/**,
	// examples/bd/**, test/acceptance/** plus the pins and the workflow.
	//
	// Bumped again on the merge with main, which carried its own reviewed delta
	// (Beads v1.3.0-rc.2 -> v1.3.0): the merged workflow holds both changes, so
	// neither side's digest describes it.
	//
	// Bumped again to widen beads_topology's internal/ globs to internal/**.
	// The curated list repeated the same mistake one directory out: the Dolt
	// floor (internal/doltversion), the proxied provider's auth scope
	// (internal/doltauth), the pack state dir handed to the bd script
	// (internal/citylayout) and the pool/binding/health packages matched
	// neither beads_topology nor shared, so a change to any of them skipped the
	// only job that stands up the proxied shapes and ci-required accepted the
	// skip. `go list -deps ./test/acceptance/... ./cmd/gc` names 139 of 166
	// internal packages, so the filter is now the graph itself.
	//
	// Bumped again for one added step in the same job: "Proxied-native
	// lifecycle and safety" (council pr2 C-F1). TestProxiedNativeLifecycle and
	// TestProxiedNativeSafety carry //go:build acceptance_a and were selected
	// by no -run expression in any job, so the proxied-native lane's whole
	// evidence base — the per-crash-shape ping/recover budgets, foreign-root's
	// "0 pings, 0 dolt stop", the no-spawn positive control, both no-migrate
	// rows and the author-at-commit pin — was a local one-off no regression
	// could fail. Reviewed delta: one `go test` step, same job, same tooling,
	// no new trigger and no new permission.
	//
	// Bumped again to split that step into its own job (round3 D-F17). The
	// topology job's four step -timeouts summed to 115 minutes against its own
	// 90-minute cap, so a slow-but-live run was canceled by the job timeout
	// and lost its `--- FAIL` line and tee'd log. Reviewed delta: the topology
	// job's -timeouts become 30/15/30 (75 under 90); the lifecycle and safety
	// step moves to a new "Beads / proxied-native acceptance" job with the same
	// needs, the same beads_topology `if`, the same runner, env, bd build and
	// verify steps, one -timeout 45m test step under timeout-minutes 60, and a
	// skip summary; ci-required needs the new job and allows its skip exactly
	// as it does the topology job's. No new trigger and no new permission.
	// Merged with main's reviewed delta: cmd-gc-productmetrics-testhook
	// timeout-minutes 5 -> 12 (#6396: canceled at the 5-minute budget with no
	// failing test).
	//
	// Bumped again (#6385): the integration path filter also matches
	// internal/bootstrap/packs/core/assets/scripts/** so a reaper.sh-only
	// change runs the real-Dolt reaper tests. Reviewed delta: one filter path,
	// no new job, trigger or permission.
	//
	// Bumped again (F9): beads-topology-acceptance gains one step running
	// TestBeadsProxiedIgnoresUserLevelSharedServer (-timeout 15m) and its job
	// cap moves 90 -> 105 minutes to keep the step budget under it. Reviewed
	// delta: one test step and the cap, no new job, trigger or permission.
	//
	// Bumped again for the Beads v1.3.0 -> v1.3.1-rc.2 -> v1.3.1 pins: every job's
	// BD_VERSION env value moves to the new tag. Reviewed delta: that value
	// only, no new job, step, trigger or permission.
	//
	// Bumped again (ga-nr9epw, restoring ga-1037rg / ga-yoxtux regression
	// coverage without re-widening test-bd-cli-contract's own -run regex,
	// which TestAcceptanceTargetsSeparateTierAFromExternalBdContracts pins as
	// an exact literal substring): one new step, "bd CLI contract HOME
	// isolation (...)", added immediately after the existing "bd CLI contract
	// (...)" step in each of contract-acceptance-previous, contract-
	// acceptance-current and contract-radar-bd-head. Each new step runs `make
	// test-bd-cli-contract-home-isolation`, a separate Makefile target driving
	// only TestRunBDIsolatesHOMEFromSharedServerConfig under the same
	// acceptance_bd_contract tag and bd binary the preceding step already
	// resolved onto PATH. No new job, trigger or permission.
	//
	// Bumped again (rbe-west plan R2 step 1): the runner-policy job's own
	// runs-on drops its hard-coded login list for blacksmith-2vcpu-ubuntu-2404,
	// matching runner_policy.py, which now selects Blacksmith for every event
	// and author. Reviewed delta: that one runs-on value, no new job, step,
	// trigger or permission.
	//
	// Bumped again for the Dolt 2.1.7 -> 2.2.0 pin (the Dolt beads v1.3.1
	// qualifies): the job DOLT_VERSION env values only.
	//
	// Bumped again (keep managed Dolt logs from failed acceptance tests):
	// beads-topology-acceptance and beads-proxied-native-acceptance each gain
	// a step exporting GC_TEST_FAILURE_ARTIFACT_DIR to $GITHUB_ENV and an
	// `if: failure()` pinned upload-artifact step for that directory. No new
	// job, trigger, permission or secret.
	//
	// Bumped again (beads#7037): the topology job's shared-server step -run
	// also selects TestBlockedRepairOnProxiedCityAndRig, the proxied city+rig
	// proof of gc start's is_blocked repair (about 4 minutes, inside that
	// step's 15m -timeout). Reviewed delta: one -run alternative, no new job,
	// step, trigger or permission.
	//
	// Bumped again (Go module fetch resilience): every job that calls
	// actions/setup-go directly sets its `cache: false` and gains one step
	// right after it, `uses: ./.github/actions/go-mod-download` (restore the
	// go.sum-keyed module download cache, run a retried `go mod download`,
	// save on push/schedule only), so build and test steps never fetch from
	// proxy.golang.org; and the shared changes filter gains that action's
	// directory and its retry script. Reviewed delta: one setup-go input and
	// one local-action step per setup-go job, two filter paths. No new job,
	// trigger, permission or secret. Then the shared changes filter gains
	// .github/scripts/go-mod-verify-cache.sh, the action's go.sum
	// verification step. Reviewed delta: one filter path.
	//
	// Bumped again (gc 1.5.1 proxied idle timeout): the proxied-native job's
	// test step also selects TestProxiedIdleTimeoutReapAndTransparentRestart,
	// sets GC_ACCEPTANCE_TOPOLOGY_MATRIX=1 for that step (the row is gated off
	// Tier A by that switch), and the step is renamed to say so. No new job,
	// trigger or permission.
	//
	// Bumped again (gc 1.5.1 suspension quiescence): the same step also
	// selects TestProxiedSuspensionIsQuiescence (~8 minutes, inside the step's
	// 45m -timeout) and its name says so. No new job, trigger or permission.
	//
	// Bumped again (OpenAPI breaking-change gate): preflight-generated gains
	// an OPENAPI_BREAKING_BASE job env (PR base SHA, else github.sha) and a
	// "Fetch OpenAPI breaking-change base" step that shallow-fetches that
	// commit before `make spec-ci`, which now also runs the oasdiff gate; the
	// spec-ci step is renamed to say so. Reviewed delta: one env var, one
	// step, one step name; no new job, trigger or permission.
	//
	// Bumped again (ga-96smfk.8, checks moved to Bazel): preflight-static
	// drops the CI-policy, go.mod replace, native dependency surface,
	// event-export isolation, open-core boundary, format and docs steps, and
	// preflight-generated drops its GC_REQUIRE_OAPI_CODEGEN env, the spec-ci
	// and generated-docs drift steps and the patch upload. Each now runs as a
	// Bazel test in the required bazel.yml unit lane; the OpenAPI
	// breaking-change gate (needs the git base commit) stays as its own
	// `make openapi-breaking-check` step. Reviewed delta: removed steps, one
	// removed env and one renamed step command; no new job, trigger or
	// permission.
	//
	// Bumped again (ga-96smfk.26, acceptance shard floor): TestBeadsProxiedDefault
	// was split into independent TestBeadsProxiedDefault* top-level tests, so
	// the topology job's proxied-default step selects those eight names
	// instead of the one. Same tests' assertions, same step, -timeout and env.
	// No new job, trigger or permission.
	//
	// Bumped again (ga-96smfk.6, package integration shards moved to Bazel):
	// the eleven packages-* rows (packages-core-N-of-4,
	// packages-cmd-gc-integration, packages-runtime-tmux-N-of-6) leave
	// integration-shards for a new integration-packages-fork job with the
	// same runner, env and steps, run only for fork and Dependabot pull
	// requests (bazel.yml's gating integration-packages lane covers pushes
	// and same-repo PRs); ci-integration needs it and allows its skip.
	// Reviewed delta: one job split by condition; no new trigger, step
	// command or permission.
	//
	// Bumped again (ga-96smfk.5, lint and vet as nogo): preflight-static
	// drops the static-scope classifier, the golangci-lint version/cache
	// steps, `make lint-affected`, `make lint` and `make vet` (nogo now runs
	// them inside every Bazel Go compile, gated by bazel.yml), and its
	// checkout no longer needs fetch-depth 2. Reviewed delta: removed steps
	// and one removed checkout input; no new job, trigger or permission.
	//
	// Bumped again (ga-96smfk.50, //test/integration smoke subset moved to
	// Bazel): the bdstore and rest-smoke rows join the packages-* rows in one
	// fork/Dependabot-PR-only integration-shards job (bazel.yml's gating
	// integration-smoke and integration-packages lanes cover pushes and every
	// other PR), and integration-packages-fork goes; ci-integration drops it.
	// Reviewed delta: rows moved between two jobs of identical runner, env
	// and steps, one job removed; no new trigger, step command or permission.
	// Bumped again (ga-96smfk.7, cmd/gc process suite moved to Bazel): the
	// 12-shard cmd-gc-process job now runs for fork and Dependabot PRs only
	// (the integration-packages-fork condition) and its name says so. Pushes
	// and same-repo PRs run the same suite in bazel.yml's gating
	// integration-packages lane (//cmd/gc:gc_test, --config=integration,
	// GC_FAST_UNIT=0). Reviewed delta: one job's if and name; no new job,
	// trigger or permission.
	// Bumped again (ga-96smfk.7, worker-core conformance moved to Bazel): the
	// eight per-profile worker-core and worker-core-phase2 jobs and their two
	// summary jobs are removed, with the changes job's worker and
	// worker_phase2 filters and outputs that only they read, and ci-required
	// no longer needs the two summaries. Bazel runs the same tests with
	// PROFILE unset (every profile): workertest_test, tmux_test and gc_test
	// in the unit lane, gc_test's integration-tagged real-transport proof in
	// the integration-packages lane. Reviewed delta: removed jobs, filters,
	// outputs and needs; no new job, trigger or permission.
	//
	// Bumped again (ga-96smfk.7, tagged suites moved to Bazel): the
	// cmd-gc-productmetrics-testhook job and preflight-static's "Native
	// DoltLite beads tests" step are removed, with the job's two ci-required
	// references. bazel.yml's unit lane runs the same tests as
	// //cmd/gc:gc_productmetrics_testhook_test (-tags productmetrics_testhook)
	// and //internal/beads:beads_native_doltlite_test (-tags
	// gascity_native_beads). Reviewed delta: one removed job and one removed
	// step; no new job, trigger or permission.
	//
	// Bumped again (ga-96smfk.50, fork PRs on Bazel): integration-shards, the
	// last Go integration shards (packages-*, bdstore, rest-smoke, run only
	// for fork and Dependabot PRs since #7261), and cmd-gc-process (fork-only
	// since #7247) are deleted: bazel.yml's gating integration-packages and
	// integration-smoke lanes run those tests for every PR, fork ones
	// included (fork-ro run 37569466546, fork-rw run 37572755753).
	// ci-integration drops integration-shards, ci-required cmd-gc-process.
	// Reviewed delta: two jobs removed, two needs and two allowed skips
	// dropped; no new job, trigger or permission.
	//
	// Bumped again (ga-96smfk.10, Docker session suite moved to Bazel): the
	// docker-session job, its `docker` changes filter/output and its
	// ci-required need and allowed skip are removed. Every scenario of the
	// real-Docker harness it ran now runs as TestDockerSessionScript
	// (//test/containerhost, emulated container host) in bazel.yml's gating
	// integration-packages lane. Reviewed delta: one job and one filter
	// removed; no new job, trigger, step command or permission.
	//
	// Bumped again (ga-96smfk.9, dashboard SPA moved to Bazel): the npm
	// dashboard job and preflight-generated's `make dashboard-ci` step (and
	// its setup-node) are removed, and ci-preflight no longer needs the job.
	// Bazel's unit lane runs the same typecheck, Vitest, build, drift and
	// Playwright steps (//internal/api/dashboardspa/...). Reviewed delta:
	// removed job, steps and need; no new job, trigger or permission.
	// Bumped again (ga-96smfk.7, bd contract cells moved to Bazel): the
	// contract-acceptance-previous and contract-acceptance-current jobs are
	// removed, and Check and ci-preflight no longer need them (nor allow the
	// current cell's skip). bazel.yml's unit lane runs the same tests against
	// pinned bd release archives: //test/acceptance:bd_cli_contract_prev_test
	// and :bd_cli_contract_current_test, and
	// //internal/beads:bd_conditional_release_contract_test. Reviewed delta:
	// two removed jobs and their fan-in references; no new job, trigger or
	// permission.
	//
	// Bumped again (ga-96smfk.11, beads topology acceptance moved to Bazel):
	// the beads-topology-acceptance and beads-proxied-native-acceptance jobs
	// are removed, with the changes job's beads_topology filter and output
	// that only they read, and ci-required no longer needs them (nor allows
	// their skip). bazel.yml's required acceptance lane runs the same tests
	// against the pinned bd and dolt, under GC_REQUIRE_ACCEPTANCE_TOOLING
	// and GC_ACCEPTANCE_TOPOLOGY_MATRIX: //test/acceptance:acceptance_test
	// and :acceptance_solo_tests. Reviewed delta: two removed jobs, one
	// filter and output, and their fan-in references; no new job, trigger or
	// permission.
	//
	// Bumped again (ga-96smfk.10, K8s session suite moved to Bazel): the
	// k8s-session job, its `k8s` changes filter/output and its ci-required
	// need and allowed skip are removed. Its one step (TestK8sSessionConformance)
	// only ran with the GC_K8S_AVAILABLE secret, which was never set, so it
	// executed nothing; the suite now runs against the emulated pod API in
	// //test/containerhost in bazel.yml's gating integration-packages lane.
	// Reviewed delta: one job and one filter removed; no new job, trigger,
	// step command or permission.
	//
	// Bumped again (ga-96smfk.10, openclaw-bridge Node suite moved to
	// Bazel): the openclaw-bridge job, its openclaw_bridge changes
	// filter/output and its ci-required need and allowed skip are removed.
	// The same node --test files run as //contrib/openclaw-bridge's js_tests
	// under rules_js in bazel.yml's gating unit lane. Reviewed delta: one job
	// and one filter removed; no new job, trigger, step command or permission.
	//
	// Bumped again (ga-96smfk.38, OpenAPI breaking-change gate moved to
	// Bazel): the preflight-generated job and its need in Check and
	// ci-preflight are removed. //cmd/openapi-breaking:openapi-breaking_test
	// (in bazel.yml's gating unit lane) runs the gate with the pinned oasdiff
	// against the PR base commit's spec, which the lane writes with git show
	// and hands to @openapi_base_spec. Reviewed delta: one job removed; no
	// new job, trigger, step command or permission.
	//
	// Bumped again (ga-rqzwrh): integration-rest-full's job cap moves 15 -> 25
	// minutes. Its 16 shards split the suite by position, so #6821's three new
	// tests moved every later one and shard 6 went from 7-9m to 13-15m, canceled
	// at the old cap on 6 of 11 main pushes. Reviewed delta: that one
	// timeout-minutes value, no new job, step, trigger or permission.
	expectedCIExecutionHash     = "0960fd8cdd477fcce8b50ae86571ac2099d1dece2e9cb8b9323d185e25b0498a"
	expectedNightlyTriggersHash = "0a4400a09ac567e90adf8be1232eef1f14e36efd8dba3e143aa6e36f5b7a36f5"
	// Nightly: reviewed delta Beads v1.3.0-rc.2 -> v1.3.0, then (round3 review,
	// completeness) one new job, beads-proxied-perf: ubuntu-latest,
	// timeout-minutes 60, env GC_REQUIRE_ACCEPTANCE_TOOLING=1 and
	// GC_ACCEPTANCE_PERF=1, the setup action with dolt and no released bd, the
	// PR jobs' resolve-pin / build-bd-from-BD_CURRENT_REF / verify steps
	// verbatim, and one `go test -tags acceptance_a -timeout 45m -run
	// 'TestBeadsProxiedDefault$'` step. No new trigger, no new permission, no
	// provider selector. Then (v1.5.0 Tier C first-run drain) the tier-c job's
	// -run selector gained TestFreshInit_SlingSpawnsDefaultPoolWorker and
	// TestFreshInit_ClaudeUnrestricted, mirroring RC Gate's acceptance C shards;
	// same job, env, secrets and runner. Then the Beads v1.3.0 -> v1.3.1-rc.2
	// -> v1.3.1 pins: the workflow and job BD_VERSION env values only. Then
	// the Dolt 2.1.7 -> 2.2.0 pin (DOLT_VERSION env values) and one step in
	// the bundled-pack-pins job, `scripts/check-embedded-pins --skip-bundled`
	// with GITHUB_TOKEN, which fails when deps.env falls behind the latest
	// beads release or the Dolt it qualifies. No new job, trigger or
	// permission. Then beads-proxied-perf gains the same failure-diagnostics
	// routing step and `if: failure()` pinned upload-artifact step as the PR
	// acceptance jobs; no new job, trigger, permission or secret. Then the
	// tier B job fetches tag v1.5.0-rc1 (shallow, one ref) and requires the
	// split-storage rc1 upgrade scenario to run rather than skip; no new job,
	// trigger, permission or secret. Then (Go module fetch resilience)
	// bundled-pack-pins and waiver-clock each set setup-go `cache: false` and
	// gain one step right after it, `uses: ./.github/actions/go-mod-download`;
	// no new job, trigger, permission or secret. Then (ga-96smfk.26)
	// beads-proxied-perf's -run selects TestBeadsProxiedDefaultNativeLane, the
	// one test split out of TestBeadsProxiedDefault that reads
	// GC_ACCEPTANCE_PERF; no new job, trigger, permission or secret.
	expectedNightlyExecutionHash = "81df6767fb4a45226bfdf50a367a369c86f525510772a313d0ce3fa744295153"
	// Setup action: reviewed delta (Go module fetch resilience) is setup-go
	// `cache: false` and one step right after it,
	// `uses: ./.github/actions/go-mod-download`.
	expectedSetupActionHash = "910f005f48c629c9bf76c69a007b60c4132a76859183e59c0fe99d642bf6141f"
)

var requiredFilterPaths = map[string][]string{
	"mail": {"internal/mail/**", "contrib/mail-scripts/**"},
	"beads": {
		"go.mod",
		"internal/beads/**",
		"test/acceptance/beads_cli_contract_test.go",
		"deps.env",
		".github/scripts/install-bd-archive.sh",
		"cmd/gc/init_provider_readiness.go",
	},
	"packs": {
		"examples/gastown/**",
		"internal/config/pack.go",
		"internal/config/compose.go",
		"cmd/gc/embed_builtin_packs.go",
		"scripts/update-bundled-gastown-pack",
	},
	"cmd_gc_process": {
		"go.mod",
		"go.sum",
		".github/workflows/**",
		"Makefile",
		"cmd/gc/**",
		"internal/**",
		"examples/gastown/**",
	},
	"credential_provider": {
		"go.mod",
		"go.sum",
		"internal/credentialprovider/**",
		"internal/testenv/**",
		"internal/testutil/**",
	},
	"integration": {
		"go.mod",
		"go.sum",
		".github/workflows/**",
		"Makefile",
		"**/*.go",
		"scripts/test-integration-shard",
		"scripts/test-go-test-shard",
		"scripts/runtime-tmux-tests.manifest",
		"scripts/go-test-observable",
		"examples/gastown/**",
	},
	"shared": {
		"go.mod",
		"go.sum",
		"Makefile",
		".github/workflows/**",
		".github/actions/setup-gascity-ubuntu/**",
		".github/actions/go-mod-download/**",
		".github/scripts/go-mod-download-retry.sh",
		".github/scripts/go-mod-verify-cache.sh",
		".github/scripts/install-dolt-archive.sh",
		".github/scripts/install-bd-archive.sh",
		".github/scripts/install-claude-native.sh",
		"internal/beads/**",
		"internal/events/**",
		"internal/config/**",
	},
}

var (
	jobExecutionFields = []string{
		"needs",
		"if",
		"uses",
		"with",
		"secrets",
		"runs-on",
		"timeout-minutes",
		"env",
		"strategy",
		"outputs",
		"continue-on-error",
		"defaults",
		"permissions",
		"environment",
		"concurrency",
		"container",
		"services",
		"steps",
	}
	workflowExecutionFields = []string{
		"permissions",
		"env",
		"defaults",
		"concurrency",
	}
	stepExecutionFields = []string{
		"id",
		"if",
		"uses",
		"run",
		"with",
		"shell",
		"env",
		"continue-on-error",
		"timeout-minutes",
		"working-directory",
	}
)

func validate(ci, nightly, action map[string]any) error {
	if err := assertSemanticHash("CI triggers", projectTriggers(ci), expectedCITriggersHash); err != nil {
		return err
	}
	if err := validateChangesJob(ci); err != nil {
		return err
	}
	if err := validatePRProviderOwnership(ci); err != nil {
		return err
	}
	if err := assertWorkflowExecution("CI", ci, expectedCIExecutionHash); err != nil {
		return err
	}
	if err := assertSemanticHash("setup action", projectAction(action), expectedSetupActionHash); err != nil {
		return err
	}
	if match, ok := findActionProviderSelector(projectAction(action), "setup-action"); ok {
		return fmt.Errorf(
			"setup action must not select a test provider: %s assigns %s",
			match.path,
			match.name,
		)
	}
	if err := assertSemanticHash(
		"nightly triggers",
		projectTriggers(nightly),
		expectedNightlyTriggersHash,
	); err != nil {
		return err
	}
	if err := validateNightlyProviderOwnership(nightly); err != nil {
		return err
	}
	return assertWorkflowExecution("nightly", nightly, expectedNightlyExecutionHash)
}

func validateChangesJob(workflow map[string]any) error {
	changeJob, err := workflowJob(workflow, "changes")
	if err != nil {
		return err
	}
	steps, err := mappingSlice(changeJob["steps"], "changes steps")
	if err != nil || len(steps) != 3 {
		if err != nil {
			return err
		}
		return fmt.Errorf("changes must contain exactly three execution steps")
	}
	with, ok := steps[1]["with"].(map[string]any)
	if !ok {
		return fmt.Errorf("changes paths-filter step must have a with mapping")
	}
	filterSource, ok := with["filters"].(string)
	if !ok {
		return fmt.Errorf("changes paths-filter input must be static YAML")
	}
	var filters map[string]any
	if err := yaml.Unmarshal([]byte(filterSource), &filters); err != nil {
		return fmt.Errorf("parse changes filters: %w", err)
	}
	filterNames := make([]string, 0, len(requiredFilterPaths))
	for filter := range requiredFilterPaths {
		filterNames = append(filterNames, filter)
	}
	sort.Strings(filterNames)
	for _, filter := range filterNames {
		required := requiredFilterPaths[filter]
		paths, ok := filters[filter].([]any)
		if !ok {
			return fmt.Errorf("changes filter %q must be a static path list", filter)
		}
		present := make(map[string]bool, len(paths))
		for _, path := range paths {
			text, ok := path.(string)
			if !ok {
				return fmt.Errorf("changes filter %q contains a non-string path", filter)
			}
			present[text] = true
		}
		for _, path := range required {
			if !present[path] {
				return fmt.Errorf("changes filter %q is missing required path %q", filter, path)
			}
		}
	}

	return nil
}

func validatePRProviderOwnership(workflow map[string]any) error {
	execution, err := projectWorkflowExecution(workflow)
	if err != nil {
		return err
	}
	if match, ok := findWorkflowProviderSelector(execution, "ci"); ok {
		return fmt.Errorf(
			"PR workflow must not select nightly-only test providers: %s assigns %s",
			match.path,
			match.name,
		)
	}
	return nil
}

func validateNightlyProviderOwnership(workflow map[string]any) error {
	if match, ok := findEnvField(workflow, "nightly"); ok {
		return fmt.Errorf(
			"nightly provider selection must be owned only by integration-sqlite-coordstore: %s assigns %s",
			match.path,
			match.name,
		)
	}

	jobs, ok := workflow["jobs"].(map[string]any)
	if !ok {
		return fmt.Errorf("nightly jobs must be a mapping")
	}
	jobNames := make([]string, 0, len(jobs))
	for name := range jobs {
		jobNames = append(jobNames, name)
	}
	sort.Strings(jobNames)
	for _, name := range jobNames {
		raw := jobs[name]
		job, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("nightly job %q must be a mapping", name)
		}
		if name == "integration-sqlite-coordstore" {
			continue
		}
		path := "nightly.jobs." + name
		if match, found := findJobProviderSelector(projectJob(job), path); found {
			return fmt.Errorf(
				"nightly provider selection must be owned only by integration-sqlite-coordstore: %s assigns %s",
				match.path,
				match.name,
			)
		}
	}
	return nil
}

func assertWorkflowExecution(label string, workflow map[string]any, expectedHash string) error {
	execution, err := projectWorkflowExecution(workflow)
	if err != nil {
		return err
	}
	return assertSemanticHash(label+" workflow", execution, expectedHash)
}

func assertSemanticHash(label string, got any, expectedHash string) error {
	encoded, err := json.Marshal(got)
	if err != nil {
		return fmt.Errorf("encode %s semantic policy: %w", label, err)
	}
	actualHash := fmt.Sprintf("%x", sha256.Sum256(encoded))
	if actualHash == expectedHash {
		return nil
	}
	rendered, _ := json.MarshalIndent(got, "", "  ")
	return fmt.Errorf(
		"%s execution shape changed\nwant SHA-256: %s\ngot SHA-256:  %s\nsemantic projection:\n%s",
		label,
		expectedHash,
		actualHash,
		rendered,
	)
}

func workflowJob(workflow map[string]any, name string) (map[string]any, error) {
	jobs, ok := workflow["jobs"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("workflow jobs must be a mapping")
	}
	job, ok := jobs[name].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("workflow job %q must be a mapping", name)
	}
	return job, nil
}

func mappingSlice(value any, label string) ([]map[string]any, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list", label)
	}
	result := make([]map[string]any, 0, len(items))
	for index, item := range items {
		mapping, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s item %d must be a mapping", label, index)
		}
		result = append(result, mapping)
	}
	return result, nil
}

func projectWorkflowExecution(workflow map[string]any) (map[string]any, error) {
	jobs, ok := workflow["jobs"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("workflow jobs must be a mapping")
	}
	projectedJobs := make(map[string]any, len(jobs))
	for name, raw := range jobs {
		job, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("workflow job %q must be a mapping", name)
		}
		projectedJobs[name] = projectJob(job)
	}
	result := selectFields(workflow, workflowExecutionFields)
	result["jobs"] = projectedJobs
	return result, nil
}

func projectJob(job map[string]any) map[string]any {
	result := selectFields(job, jobExecutionFields)
	rawSteps, exists := job["steps"]
	if !exists {
		return result
	}
	steps, ok := rawSteps.([]any)
	if !ok {
		result["steps"] = rawSteps
		return result
	}
	projected := make([]any, 0, len(steps))
	for _, raw := range steps {
		step, ok := raw.(map[string]any)
		if !ok {
			projected = append(projected, raw)
			continue
		}
		projected = append(projected, projectStep(step))
	}
	result["steps"] = projected
	return result
}

func projectStep(step map[string]any) map[string]any {
	return selectFields(step, stepExecutionFields)
}

func projectAction(action map[string]any) map[string]any {
	result := make(map[string]any)
	if inputs, ok := action["inputs"]; ok {
		result["inputs"] = projectInputs(inputs)
	}
	if outputs, ok := action["outputs"]; ok {
		result["outputs"] = projectInputs(outputs)
	}
	runs, ok := action["runs"].(map[string]any)
	if !ok {
		if raw, exists := action["runs"]; exists {
			result["runs"] = raw
		}
		return result
	}
	projectedRuns := selectFields(runs, []string{"using"})
	if rawSteps, exists := runs["steps"]; exists {
		if steps, ok := rawSteps.([]any); ok {
			projected := make([]any, 0, len(steps))
			for _, raw := range steps {
				if step, ok := raw.(map[string]any); ok {
					projected = append(projected, projectStep(step))
				} else {
					projected = append(projected, raw)
				}
			}
			projectedRuns["steps"] = projected
		} else {
			projectedRuns["steps"] = rawSteps
		}
	}
	result["runs"] = projectedRuns
	return result
}

func selectFields(source map[string]any, fields []string) map[string]any {
	result := make(map[string]any)
	for _, field := range fields {
		if value, ok := source[field]; ok {
			result[field] = copyValue(value)
		}
	}
	return result
}

func projectTriggers(workflow map[string]any) any {
	triggers := copyValue(workflow["on"])
	triggerMap, ok := triggers.(map[string]any)
	if !ok {
		return triggers
	}
	for _, triggerName := range []string{"workflow_call", "workflow_dispatch"} {
		trigger, ok := triggerMap[triggerName].(map[string]any)
		if !ok {
			continue
		}
		trigger["inputs"] = projectInputs(trigger["inputs"])
	}
	return triggerMap
}

func projectInputs(value any) any {
	inputs := copyValue(value)
	inputMap, ok := inputs.(map[string]any)
	if !ok {
		return inputs
	}
	for _, value := range inputMap {
		if input, ok := value.(map[string]any); ok {
			delete(input, "description")
		}
	}
	return inputMap
}

func copyValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, child := range value {
			result[key] = copyValue(child)
		}
		return result
	case []any:
		result := make([]any, len(value))
		for index, child := range value {
			result[index] = copyValue(child)
		}
		return result
	default:
		return value
	}
}

type providerSelectorMatch struct {
	name string
	path string
}

var providerSelectorNames = []string{
	"GC_BEADS",
	"GC_ACCEPTANCE_BEADS_PROVIDER",
}

func findWorkflowProviderSelector(workflow map[string]any, path string) (providerSelectorMatch, bool) {
	if match, ok := findEnvField(workflow, path); ok {
		return match, true
	}
	jobs, ok := workflow["jobs"].(map[string]any)
	if !ok {
		return providerSelectorMatch{}, false
	}
	names := sortedKeys(jobs)
	for _, name := range names {
		job, ok := jobs[name].(map[string]any)
		if !ok {
			continue
		}
		if match, found := findJobProviderSelector(job, joinFieldPath(path, "jobs."+name)); found {
			return match, true
		}
	}
	return providerSelectorMatch{}, false
}

func findJobProviderSelector(job map[string]any, path string) (providerSelectorMatch, bool) {
	if match, ok := findEnvField(job, path); ok {
		return match, true
	}
	if container, ok := job["container"].(map[string]any); ok {
		if match, found := findEnvField(container, joinFieldPath(path, "container")); found {
			return match, true
		}
	}
	if services, ok := job["services"].(map[string]any); ok {
		for _, name := range sortedKeys(services) {
			service, ok := services[name].(map[string]any)
			if !ok {
				continue
			}
			servicePath := joinFieldPath(path, "services."+name)
			if match, found := findEnvField(service, servicePath); found {
				return match, true
			}
		}
	}
	return findStepsProviderSelector(job["steps"], joinFieldPath(path, "steps"))
}

func findActionProviderSelector(action map[string]any, path string) (providerSelectorMatch, bool) {
	runs, ok := action["runs"].(map[string]any)
	if !ok {
		return providerSelectorMatch{}, false
	}
	return findStepsProviderSelector(runs["steps"], joinFieldPath(path, "runs.steps"))
}

func findStepsProviderSelector(value any, path string) (providerSelectorMatch, bool) {
	steps, ok := value.([]any)
	if !ok {
		return providerSelectorMatch{}, false
	}
	for index, raw := range steps {
		step, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		stepPath := fmt.Sprintf("%s[%d]", path, index)
		if match, found := findEnvField(step, stepPath); found {
			return match, true
		}
	}
	return providerSelectorMatch{}, false
}

func findEnvField(value map[string]any, path string) (providerSelectorMatch, bool) {
	env, exists := value["env"]
	if !exists {
		return providerSelectorMatch{}, false
	}
	return findProviderEnvKey(env, joinFieldPath(path, "env"))
}

func findProviderEnvKey(value any, path string) (providerSelectorMatch, bool) {
	env, ok := value.(map[string]any)
	if !ok {
		return providerSelectorMatch{}, false
	}
	for _, name := range providerSelectorNames {
		if _, exists := env[name]; exists {
			return providerSelectorMatch{name: name, path: joinFieldPath(path, name)}, true
		}
	}
	return providerSelectorMatch{}, false
}

func sortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func joinFieldPath(path, field string) string {
	if path == "" {
		return field
	}
	return path + "." + field
}
