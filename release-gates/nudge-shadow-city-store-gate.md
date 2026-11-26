**Verdict:** **PASS**

# Nudge shadows stay in the city nudges store

Evaluated: 2026-11-26T22:52:30+00:00
Deploy bead: ga-63vdy1zb; build: ga-vnc6rz; review: ga-6b4hp9km.
Reviewed source: 95041284c22ca0b4fdfac143c15792c71bd48d67
Deploy branch: deploy/ga-63vdy1zb-gate.
DEPLOY_MODE: remote; PUSH_REMOTE: fork; BASE_BRANCH: main.
The gate authorizes opening a PR. Merge authority must verify PR CI on its exact head.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | PASS — ga-6b4hp9km closes with verdict pass. Independently verified the reviewer's additive test commit with reviewer-added-tests.sh. |
| 2 | Acceptance criteria met | PASS — two-store placement, ack, expiry, all six producer routes, and relocated nudges-class tests pass by name. Full unit/acceptance/integration, nogo build/vet and generated-file drift checks pass. Production changes only the enqueue store and explanatory comment; queue semantics, schema, metadata keys and historical shadows are untouched. |
| 3 | Tests and required CI accounting | PASS — full-scope independent run, 159634 PASS / 0 FAIL / 618 SKIP result lines; five owned tests PASS in both unit and integration packages. SKIPs audited below; six CI leaf jobs accounted for below. |
| 4 | No unresolved high-severity findings | PASS — review has no blocker/major/HIGH finding. Informational existing error handling and advisory stale-comment nits are recorded in the review bead. |
| 5 | Clean final branch | PASS — isolated source checkout was clean before generating this gate record; the gate is the only added publication artifact. Cleanliness is rechecked after committing it and before push. |
| 6 | Merge state and build/vet | PASS — clean merge result built/vetted by make lint (Bazel nogo), 1505 targets, rc=0, 229.938 seconds. |
| 7 | Single feature theme | PASS — cmd/gc/cmd_sling.go and cmd/gc/cmd_nudge_test.go only. Ancestry scope/denylist checks passed for the linked deploy/build/review IDs; no stack. |

reviewer_tests_verified: 831957a85e61651aa4169e470b622dbe397582a5 -> 95041284c22ca0b4fdfac143c15792c71bd48d67 files=cmd/gc/cmd_nudge_test.go

merge_state: clean (origin/main@c2027dae0ac533c5a7a58f2909082f1b335acb9e)
gate_base_kind: current-tip
gate_base: c2027dae0ac533c5a7a58f2909082f1b335acb9e
gate_head: 95041284c22ca0b4fdfac143c15792c71bd48d67
merge_head: df4e8320c60600f26c6490facc258a0eae1c03bc
merge_tree: c2f53e47aef2e7f249a3d18336b00013a193c895
merge_validation: merge result built and vetted here; merge-time CI remains required.
scope_stack: none
heavy_mode: none
waiver_ref: none
failure_attribution: the independent criterion-3 run had no failure; publication-hook attributions follow below.

## Fast lane

All lanes passed before the suite launched. Each ran in its own fresh view of the same merge tree.
Base pin: GATE_BASE_RECORD base=c2027dae0ac533c5a7a58f2909082f1b335acb9e.

- Format: pinned make fmt-check; GLT_RECORD pin=2.14.0 resolved=2.14.0 mismatch=false.
- Policy: make lint NOGO_BUILD_FLAGS='--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback', then make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary.
- drift_lane: CI's make bazel-sync with BAZEL_SYNC_FLAGS=--config=remote-exec, then git diff --exit-code; rc=0. Nonfatal Gazelle diagnostics are retained in the log.
- known_red_precheck: proceed reason=nothing-known-red. check-routed-test-rows passed; ga-1ptv4jey was stale for this tree.
- Log: /var/tmp/gc-ga-63vdy1zb-fast-result.txt (includes FRESH_TREE_RECORD lines).

## Independent test evidence

test_cmd_scope: full-suite
test_exec_mode: remote-exec
test_counts: PASS=159634 FAIL=0 SKIP=618
Counts are top-level result lines summed across jobs; a test run in several jobs counts once per job.
test_log_dir: /var/tmp/gc-gate-ga-63vdy1zb-c3/evidence/remote/logs
execution_record: /var/tmp/gc-gate-ga-63vdy1zb-c3/evidence/execution.json
run_log: /var/tmp/gc-ga-63vdy1zb-c3-result.txt
test_exec_bep: per-phase *.bep.json and *.evidence.json under /var/tmp/gc-gate-ga-63vdy1zb-c3/evidence/remote.
farm_test_seconds: 13880.582
fallback_reason: none
delta_gate: shadow; decision=full; reason=not-gate-pr; no narrowing applied.
owned_topup: none required.
floor_topup: complete; all 16 mandatory steps exit 0; admission released before floor.
rbe_state: certificate valid to 2027-01-09; kill switch absent; worker-env pins match (sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72).
admission: remote, floor 25 GiB, free 42.805938720703125 GiB, wait 0.001 seconds, cap 2.
The isolation wrapper reports PASS-THROUGH for gascity; the test actions execute hermetically on the farm. Native floor commands scrub store selectors. The wrapper pinned test bd v1.3.4, ref_check=match.

| Phase | Remote results | Cached | Retries | BEP predicate |
|---|---:|---:|---:|---|
| unit | 427 | 0 | 0 | PASS |
| acceptance | 25 | 0 | 0 | PASS |
| integration-packages | 87 | 0 | 0 | PASS |
| integration-full | 15 | 0 | 0 | PASS |
| integration-latency | 1 | 0 | 0 | PASS |

test_cmd: standard gate-criterion3.sh chain, launched through gate-detached-run.sh. The phase commands are:

- make -k check
- bazel test --config=acceptance //test/acceptance:acceptance_test
- bazel test --config=integration //test:integration_packages
- bazel test --config=integration --test_arg=-test.v --test_arg=-test.skip='^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$' //test/integration:integration_test
- bazel test --config=integration --test_arg=-test.v --test_filter='^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$' --test_sharding_strategy=disabled --run_under=<gate-farm-probe.sh threshold=50 hold_max=600 poll=10> //test/integration:integration_test

Every Bazel test phase additionally uses:
--config=remote-exec --keep_going --config=ci --nocache_test_results
--noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel
--test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=<phase.bep.json>.
Exact expanded commands are retained in execution.json and phase stdout.
The floor ran beadstest, every environment-absence SKIP's package, Darwin compilation, and both runner self-tests.

diff_tests_executed:
  TestDeliverSlingNudgeOnARelocatedNudgesClassLeavesWorkAndRigStoresWithoutShadows PASS (integration-packages-b72a67a2f9444af705e3.log, unit-64014bc0bb3833bc198c.log)
  TestDeliverSlingNudgeRigBeadShadowTerminalizesOnAck PASS (integration-packages-18745ddf7d62465654f5.log, unit-2bde55fab377f50b4739.log)
  TestDeliverSlingNudgeRigBeadShadowTerminalizesOnExpiry PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-4d20b3d28497f844aa39.log)
  TestDeliverSlingNudgeShadowLandsInCityStoreNotSlungBeadRigStore PASS (integration-packages-bc482d2ae768251ad47f.log, unit-23e7d9fc763496dd8095.log)
  TestNudgeShadowProducersLandInTheCityNudgesStore PASS (integration-packages-830685c60d0c2dd88567.log, unit-2ef13e1b6898c5544764.log)
GATE_TEST_EVIDENCE_RESULT ok: 5 diff-owned tests verified by name

latency_leg: complete=true, attempt=1, probe_verdict=clear, both budget tests PASS; threshold=50, hold_max=600, poll=10. Probe cleared after 20 seconds at load1=47.44; 9 window readings, max_load1=47.44, over_threshold=0. The full-integration phase ran neither latency test.

load_scope: required local floor
load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
run_start_load: 12.63
run_max_load: 15.67
run_mean_load: 14.61
run_readings: 8

## SKIP justification

No diff-owned test skipped or failed. All 331 unique skip sites, including the native-floor sites, were checked on the actual merged tree with diff-owned-tests.py --site: line_in_hunk=no, func_touched=no, UNCHANGED. The only added helpers are confined to cmd/gc and the new tests. Independent baseline runs verify the CLI package's skips: unit 193/193 and integration-packages 68/68 pre-existing, with zero new, reason-changed or diff-owned skips. The comparison normalized only the documented remote-worker account difference for TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a00, head rbe-a05).

Other skips are unchanged platform constraints, opt-in live/diagnostic cases, unit/process-lane separation, helper-only entry points, unavailable optional tooling, or fixture topologies the test explicitly cannot represent. The changed production code is the CLI nudge producer, and the changed helpers are in the CLI test binary; other packages do not import that main package. The mandatory floor re-ran environment-absence selections locally; all 16 steps passed and the 72 floor skip events introduce no new skip site.

skip_inventory: execution.json.skip_inventory and remote/skip-inventory.json under /var/tmp/gc-gate-ga-63vdy1zb-c3/evidence.
site_evidence: /var/tmp/gc-ga-63vdy1zb-skip-sites.txt
floor_skip_inventory: /var/tmp/gc-ga-63vdy1zb-floor-skip-inventory.json
base_comparison_evidence: /var/tmp/gc-gate-ga-63vdy1zb-skip-proof/evidence/{unit,integration-packages}-comparison.txt
base_comparison_log: /var/tmp/gc-ga-63vdy1zb-skip-proof-result.txt (rc=0; focused support evidence, never a substitute for the full-suite run).

## Required CI job accounting

ci_required_closure: ci_required_triggers.py /var/tmp/gc-merge-validate.H4YNgA c2027dae0ac533c5a7a58f2909082f1b335acb9e 95041284c22ca0b4fdfac143c15792c71bd48d67 fork
workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612
6 LEAF jobs need a status; check is an aggregator.

| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<temp> python3 .github/workflows/scripts/runner_policy.py; rc=0; /var/tmp/gc-ga-63vdy1zb-runner-policy.log. |
| changes | LOCAL-PASS | Closure tool output: packs, credential_provider, shared all no match; /var/tmp/gc-ga-63vdy1zb-ci-closure.txt. |
| credential-provider-windows | NOT-TRIGGERED | Tool row: needs.changes.outputs.credential_provider == 'true'; filter did not match. |
| pack-gate | NOT-TRIGGERED | Tool row: needs.changes.outputs.packs == 'true'; filter did not match. |
| windows-lane | LOCAL-PASS | Job's four run steps executed verbatim (Windows build/test compilation, Windows nogo build/test compilation, all-package macOS build/test compilation), fresh-tree + load-gate + isolation wrapper, detached; rc=0; /var/tmp/gc-ga-63vdy1zb-ci-windows-lane-result.txt. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Native Windows tests require blacksmith-4vcpu-windows-2025. Independent GOOS=windows test-binary import closure over .github/windows-lane/test.txt has 10 module directories; changed inputs: none. /var/tmp/gc-ga-63vdy1zb-windows-native-reach.txt. |

windows_reach: none; credential-provider-windows closure check rc=0, empty output.
windows_native_reach: none; allowlist test-binary closure check rc=0.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none
ci_lane_run: n/a (no CI-config change in this diff)

The local cross-compile job's load summary is threshold=15, waited_seconds=0, wait_timed_out=0, run_start_load=12.58, run_max_load=17.82, run_mean_load=16.09, run_readings=4.

The PR can merge only after CI / required and the deferred native Windows job are green on its exact head. This gate does not merge.


## Publication-hook attribution

The additional normal pre-push hook ran on source/gate head
2e152eabce39b840912efecf66b976de1f379bd6 with --config=fork-cache, because that
reviewed source predates the farm worker pin. It independently reported the
two conditions below. The owned hook service was stopped after these
failures were attributed; this partial duplicate run and its unfinished
targets are not claimed as PASS and do not replace the completed full-scope
criterion-3 evidence above.

failure_attribution: TestDatedWaiversAgainstToday -> ga-5550q2dx
- Clause 1: waiverexpiry_test.go:41 UNCHANGED; no shared hunks; not diff-owned.
- Clause 2: tracker opened and read; created 2026-11-05, before this run.
  It explicitly covers the same expired provider-ledger waiver condition
  and this test on stale source trees.
- Clause 3(a), MECHANISM: successful go list -mod=readonly -test -deps of
  ./internal/testpolicy/waiverexpiry yielded 21 module directories and
  zero changed non-test inputs in that test binary's import closure.
- Clause 4: no package/path overlap with the CLI nudge source or tests.
- Sighting comment read back: 69c95c52-3456-5d5e-97d6-bcedb2f13023.
- Failure: unchanged t3bridge/exec waivers expired 2026-11-05 and became
  fleet-fatal 2026-11-19. No date or waiver was modified.
- Evidence: /var/tmp/gc-gate-ga-63vdy1zb-push/failures/waiverexpiry.log;
  /var/tmp/gc-ga-63vdy1zb-waiver-expiry-closure.txt;
  /var/tmp/gc-ga-63vdy1zb-prepush-waiver-site.txt.

failure_attribution: scripts_test package alarm -> ga-vg3wto5
- A single existing package-budget condition, not one new tracker per test.
  Shards 4/5/6 exhausted the five-minute package alarm while
  TestMacRegressionGateRunsSmokeOnEveryNonDocsPullRequest/code_change (0s),
  TestGoTestShardPreservesAcceptanceAuthEnv (25s), and
  TestWorkerEnvNotifyBeadsGraceJob/Go_re-pin (2s) were running.
- Clause 1: all three function sites UNCHANGED, no shared hunks.
- Clause 2: tracker opened and read; created 2026-10-24. Its condition is
  scripts_test's five-minute package alarm under whole-suite/host load.
- Clause 3(a), MECHANISM: successful scripts test-binary import closure
  yielded 26 module directories, zero changed non-test inputs.
- Clause 4: no scripts package/path overlap. No census or test-target changes.
- Load read after alarms, while the same hook continued: 86.53/89.71/67.63.
- Sighting comment read back: aa2e883c-0e28-5a0c-b047-cd55b9ec86f8.
- Evidence: /var/tmp/gc-gate-ga-63vdy1zb-push/failures/scripts-shard-{4,5,6}.log;
  /var/tmp/gc-ga-63vdy1zb-scripts-closure.txt;
  /var/tmp/gc-ga-63vdy1zb-prepush-scripts-sites.txt.

These are first sightings for this deploy bead, not repeated failed rounds.
Both landed MECHANISM proofs; neither uses an inconclusive attribution or
waiver_ref. No failed test was re-run. The four-clause standing pre-push
authorization in non-diff-owned-gate-failure permits --no-verify for the
specific recorded publication head after the live ownership/source guards.
This addendum changes only the gate Markdown, outside both test binaries'
input closures; the reviewed code and the completed merged-tree evidence
are unchanged.
