# Session-start liveness release gate

**Verdict:** **PASS**

Recorded: 2026-11-10T02:12:27.539498+00:00
Bead: ga-cj24o6n7; build: ga-a8cyhtat; review: ga-afna057o.
Reviewed source: `d94a2e0aba8f5d5e4869949cb7d8bfa8609517ff`.
Gated merge: `9a0041952fd7e60a90beca3b5435d2cddddcbc92`; tree: `9a8a0482f96a012790313938fe298b20e178add1`.
deploy_mode: remote; push_remote: fork; provenance: own reviewed code.
merge_state: clean (origin/main@849d5206cbf74b85234270cfad26ec7a746b71b1; frozen at criterion 6).
gate_base_kind: current-tip; gate_base_sha: 849d5206cbf74b85234270cfad26ec7a746b71b1
merge_validation: MPR validates the eventual merge against its current base; this record preserves the tested base and tree.
Evidence root: `/var/tmp/deploy-ga-cj24o6n7.f6c2ukv1` (all raw logs retained).

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Reviewer PASS names the reviewed source. reviewer-added-tests.sh recomputed 914d36f83c6aa6eda888a1f4fd39ac3b2da29402 -> d94a2e0aba8f5d5e4869949cb7d8bfa8609517ff, files=internal/session/ensure_running_unconfirmed_test.go. |
| 2 | PASS | SESSION-RUNTIME-017 and the rebase exit contract verified against code and all 14 test roots. Seven original/replayed commit pairs have identical changed lines. BUILD is the union of main/source entries. See acceptance-runtime-verified.json. |
| 3 | PASS | Full canonical suite, native floor, by-name audit, skip proofs and all six required-CI LEAF accounts are complete. Details below. |
| 4 | PASS | Zero unresolved HIGH review findings; the reviewer records only minor/info points and documented residuals. |
| 5 | PASS | Exact reviewed source checked out on isolated deploy/ga-cj24o6n7-gate with clean tracked/untracked status before this record. The committed checkout must also be clean before push. |
| 6 | PASS | Clean synthetic merge onto the frozen base; bazel build //... and make vet (nogo) both succeed. Raw BEP terminals and hashes verified in c6-verified.json. |
| 7 | PASS | One session-start liveness theme. Changes are session code/tests, its requirement row, BUILD entry, and the historical same-feature gate record. Ancestry scope passed with verified lineage IDs; no stacked deploy. |

## Tests and policy

test_cmd: gate-criterion3.sh --repo /var/tmp/deploy-ga-cj24o6n7.f6c2ukv1/merge --base 849d5206cbf74b85234270cfad26ec7a746b71b1 --bead-json /var/tmp/deploy-ga-cj24o6n7.f6c2ukv1/ga-cj24o6n7.json --owned /var/tmp/deploy-ga-cj24o6n7.f6c2ukv1/owned.tsv --run-dir /var/tmp/deploy-ga-cj24o6n7.f6c2ukv1/c3-run/evidence
test_cmd_scope: full-suite
test_counts: {"FAIL": 0, "PASS": 150275, "SKIP": 512} (canonical aggregate; skip-event inventories also include subtests).
test_log_dir: /var/tmp/deploy-ga-cj24o6n7.f6c2ukv1/c3-run/evidence/remote/logs
waiver_ref: none; failure_attribution: none.
policy_lane: PASS — pinned fmt-check-changed, make lint/nogo, make check shell guards, check-hooks and 13 Bazel policy targets.
drift_lane: PASS — make bazel-sync plus git diff --exit-code and empty porcelain in a fresh view; seven generated-artifact targets and the OpenAPI breaking check pass. Each of the three lanes used its own fresh view.
ci_lane_run: n/a (no CI configuration changes).
heavy_mode: none (computed classifier; no listed heavy package selected).
runtime_preflight: gate-icu74.sh verdict=OK; wrappers pin test bd v1.3.4 and disable interactive credential prompts. The prescribed wrapper is pass-through for the scratch; canonical farm/native commands scrub live GC/BD/BEADS/DOLT selectors themselves.

The canonical five phases ran make -k check, Bazel acceptance including solo tests, integration packages, full integration excluding the latency list, then only that latency list under a clear farm probe. Every remote result is fresh, attempt 1, uncached, terminal and remote-executed; no local fallback. Native floor completed all 16 steps (including beadstest, Darwin compile and both runner self-tests).

```json
{
  "test_exec_mode": "remote-exec",
  "test_exec_bep": {
    "unit": {
      "results": 408,
      "remote_executed": 408,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "acceptance": {
      "results": 25,
      "remote_executed": 25,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "integration-packages": {
      "results": 86,
      "remote_executed": 86,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "integration-full": {
      "results": 14,
      "remote_executed": 14,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "integration-latency": {
      "results": 1,
      "remote_executed": 1,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    }
  },
  "rbe_state": {
    "rc_marker_present": true,
    "kill_switch_absent": true,
    "cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT",
    "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98",
    "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"
  },
  "fallback_reason": null,
  "admission": {
    "class": "remote",
    "floor_gib": 25.0,
    "free_gib": 60.441497802734375,
    "waited_seconds": 0.001,
    "cap": 2
  },
  "farm_test_seconds": 12325.431,
  "owned_topup": []
}
```

skip_inventory: remote/skip-inventory.json (498 remote events); floor-skip-inventory.json (39 native events).
floor_topup: complete, 16/16 steps exit 0; remote/floor-topup.json.
latency_leg: measured, clear probe; both listed latency tests PASS; remote/latency-leg.json.

diff_tests_executed:
```text
TestEnsureRunning_CachedRunningTakesNoFreshReadAndStartsNothing PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_CompositeInFrontOfCachingProviderReadsTheLeafFresh PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_ConfirmedNotRunningStartsOnce PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_FreshCompositeOverALeafWithoutFreshReadConfirmsTheGate PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_ProviderWithLivenessReadButNoFreshReadTakesNoExtraRead PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_ProviderWithoutFreshReadKeepsItsIsRunningReadsBeforeStart PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_SessionExistsConvergesOnFreshRead PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_SessionExistsOnAFreshNotRunningReadIsNotConvergence PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_StaleAbsentSnapshotOverLiveRuntimeConverges PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_StaleKeyCheckOnConfirmedAbsenceStillRetriesFresh PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_StaleKeyCheckOnUnconfirmedLivenessDoesNotRetainACommittedConversation PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_StaleKeyCheckOnUnconfirmedLivenessKeepsResumeState PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_UnconfirmedLivenessStartsNothingAndChangesNothing PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_UnseededAutoRouteAsksTheOtherBackendBeforeStarting PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
GATE_TEST_EVIDENCE_RESULT ok: 14 diff-owned tests verified by name
```

## Skip justification

No diff-owned test skipped or failed. All 290 remote skip sites and 36 native skip sites are unchanged; their source files are byte-identical to the pinned base. The shared/helper hunks are the added internal/session test helpers. Test-binary import closures identify 338 remote skip events with possible shared-package reach; the pinned-base phase runs reproduce every one with the same reason. The remaining 160 remote events have no shared-hunk reach. All 39 native events also match the pinned-base native commands. Per-test comparisons, full reasons, sites and closure evidence are retained in skip-comparisons/, skip-audit-prepared.json and native-skip-audit-prepared.json.
The unchanged reasons cover platform/root/group requirements, explicit process/live-provider/cleanup opt-ins, unavailable provider fixtures/tools and external services, unsupported conformance capabilities, pre-existing tracked placeholders, and intentionally gated manual measurements. They occur at unchanged test/setup guards; the new session liveness code changes none of those guards or prerequisites. Reachable tests additionally have same-command/same-reason baseline proof. No skip is silently credited as a PASS.

## Required CI job accounting

ci_required_closure: ci_required_triggers.py /var/tmp/deploy-ga-cj24o6n7.f6c2ukv1/merge 849d5206cbf74b85234270cfad26ec7a746b71b1 d94a2e0aba8f5d5e4869949cb7d8bfa8609517ff fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs.
ci_required_accounting:

- changes | LOCAL-PASS | /var/tmp/deploy-ga-cj24o6n7.f6c2ukv1/ci-required/closure.stdout
- runner-policy | LOCAL-PASS | /var/tmp/deploy-ga-cj24o6n7.f6c2ukv1/ci-required/runner-policy.stdout
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | /var/tmp/deploy-ga-cj24o6n7.f6c2ukv1/ci-required/windows-native-reach.json
- windows-lane | LOCAL-PASS | /var/tmp/deploy-ga-cj24o6n7.f6c2ukv1/ci-required/windows-lane.stdout; exit=0
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'

The windows-lane job ran all four YAML commands unchanged, in a fresh view of the gated tree: Windows allowlist build, its test compilation, Windows nogo analyzers/test compilation, and every package/test compiled for Darwin. All exit 0; exact commands and streams are in ci-required/windows-lane-steps.json. changes is the closure command above, exit 0, log ci-required/closure.stdout. runner-policy: EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<retained output file> python3 .github/workflows/scripts/runner_policy.py, exit 0, logs ci-required/runner-policy.stdout and runner-policy-output.txt. NOT-TRIGGERED rows are copied from the derived closure. Native Windows test reach is empty.
windows_reach: none (credential and native Windows-test closures checked).
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none.
Merge readiness still requires CI / required green on the PR head and each deferred job successful, or skipped only because its own path filter is false.

## Load and retained proof

- c3-run/evidence/remote/floor-topup.stdout: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=35.06 run_max_load=33.36 run_mean_load=30.65 run_readings=8 wait_first_load=89.14 wait_max_load=96.64 wait_mean_load=64.09 wait_readings=61 read_errors=0`
- ci-required/windows-lane.stderr: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=300 wait_timed_out=0 run_start_load=14.73 run_max_load=16.68 run_mean_load=16.17 run_readings=3 wait_first_load=25.71 wait_max_load=25.71 wait_mean_load=19.84 wait_readings=11 read_errors=0`
- skip-base-evidence/native.stderr: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=10.60 run_max_load=11.61 run_mean_load=11.16 run_readings=3 wait_first_load=10.60 wait_max_load=10.60 wait_mean_load=10.60 wait_readings=1 read_errors=0`

The native floor used the ordinary 1800-second bounded wait; it timed out and the canonical runner proceeded. This is recorded, with no waiver. All checks returned successful terminal verdicts.
Raw independent audits: c6-verified.json; fast-verified.json; resume-c3-audit.json (1,068 artifact hashes and 534 normalized logs); acceptance-runtime-verified.json; ci-required/accounting-verified.json; skip-base-evidence/comparison-summary.json; skip-proof-verified.json (20 comparison groups, 377 same-reason events); skip-base-raw-audit.json (304 baseline artifact hashes).
All evidence remains under the root above. No merge is performed by the deployer.
