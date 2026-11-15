# Lint composing-city selection release gate

**Verdict:** **PASS**

Recorded: 2026-11-10T20:02:56.566072+00:00
Bead: ga-xb7fkdr; build: ga-bwhpjq.1; review: ga-dvz83ow.
Reviewed source: `97b781d308d9004a934e7222def3212848103c94`.
Gated merge: `fb3e7b901b1784b5ada4dc0a8768617fed8e0a27`; tree: `70eb48c9f72395ca861286347fe0a70308646e63`.
deploy_mode: remote; push_remote: fork; provenance: own reviewed code.
merge_state: clean (origin/main@bad83762c64a670ec2c3626f15e498705e464ec7; frozen at criterion 6).
gate_base_kind: current-tip; gate_base_sha: bad83762c64a670ec2c3626f15e498705e464ec7
merge_validation: MPR validates the eventual merge against its current base; this record preserves the tested base and tree.
Evidence root: `/var/tmp/deploy-ga-xb7fkdr-r2.iiz4wctl`; raw logs retained.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Reviewer PASS records the exact source above. reviewer-added-tests.sh independently verifies the one additive test-only reviewer commit on the reviewed implementation; reviewer-tests-verified.txt retains the result. |
| 2 | PASS | Approved Increment1 scope: nested-city selection, W1 loader diagnostics, W2 fallback diagnostics, once-per-city loads, standalone silence and the three unmodified fragment-scoping controls pass by name (11 roots, four subcases) in both unit and integration-tagged full-suite output. The read-only production check observes one unique fragment finding; a verified landed pack fix explains the historical six-finding difference. No production binary installed or checkout changed. AC4-7 and AC8 remain in their accepted separate increments. See acceptance-scope-audit.json, lint-fixtures-verified.json and ac10-reviewed.json. |
| 3 | PASS | Canonical full suite, native floor, by-name audit, skip proofs and every required-CI LEAF account complete; details below. |
| 4 | PASS | Zero unresolved HIGH findings. The review records only informational residuals. |
| 5 | PASS | Exact reviewed source is checked out on isolated deploy/ga-xb7fkdr-gate, clean before this record; the suite scratch is also clean. Commit cleanliness is rechecked before push. |
| 6 | PASS | Clean synthetic merge onto the frozen base; bazel build //... and make vet (nogo) pass. Raw terminal BEPs, exact parents/tree and hashes verified in c6-verified.json. |
| 7 | PASS | One lint city-selection/diagnostic theme in cmd/gc with matching fixtures. RED/GREEN build commits and one additive reviewer test commit belong to the verified lineage; ancestry scope passes, no stack. The obsolete contributor-branch plan is superseded. |

## Tests and policy

test_cmd: gate-criterion3.sh --repo /var/tmp/deploy-ga-xb7fkdr-r2.iiz4wctl/merge --base bad83762c64a670ec2c3626f15e498705e464ec7 --bead-json /var/tmp/deploy-ga-xb7fkdr-r2.iiz4wctl/ga-xb7fkdr.json --owned /var/tmp/deploy-ga-xb7fkdr-r2.iiz4wctl/owned.tsv --run-dir /var/tmp/deploy-ga-xb7fkdr-r2.iiz4wctl/c3-run/evidence
test_cmd_scope: full-suite
test_counts: {"FAIL": 0, "PASS": 150487, "SKIP": 515} (canonical top-level Go result lines; inventories include subtest events).
non_go_skips: 1 Python check, LockfilesInSyncTest.test_rules_ts_typescript_matches_lock, configured non-applicable for openclaw-bridge; non-go-skip-reviewed.json retains the raw summary, unchanged source/data proof and the inference from the only skip site plus the false macro flag. The dashboard counterpart runs all three checks successfully.
test_log_dir: /var/tmp/deploy-ga-xb7fkdr-r2.iiz4wctl/c3-run/evidence/remote/logs
waiver_ref: none; failure_attribution: none.
policy_lane: PASS — pinned changed-format check plus full-tree //:go_format_test, full lint/nogo, shell guards, hook check and 13 Bazel policy targets.
drift_lane: PASS — make bazel-sync then git diff --exit-code and empty full porcelain; seven generated-artifact targets plus OpenAPI compatibility pass. Each of the three fast lanes used a separate fresh view before runtime setup and the suite.
storeclass_policy: PASS — merge-base trusted generator checks baseline and growth with no allow-raise flag; storeclass-proof/verified.json retains commands and raw output. This separate required policy was discovered in the live ruleset review after the original fast lanes and suite launch, and completed before PASS. No actual PR-CI result claimed.
ci_lane_run: n/a (no CI configuration changed).
heavy_mode: none (computed classifier).
runtime_preflight: ICU74 OK; pinned bd v1.3.4 and noninteractive credentials. The isolation wrapper reports pass-through for the scratch; canonical remote/native commands provide their prescribed environment isolation.

The five canonical phases cover the full unit/policy tree, acceptance, integration-tagged packages, full integration excluding the latency inventory, then the latency inventory under a clear farm probe. Every remote test result is uncached, remote-executed, attempt 1, within the phase window and terminal. No farm/local suite stitching or local fallback.

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
    "free_gib": 63.58469772338867,
    "waited_seconds": 0.001,
    "cap": 2
  },
  "farm_test_seconds": 14126.369,
  "owned_topup": []
}
```

skip_inventory: 501 remote events; 39 native events; full per-test reasons in remote/skip-inventory.json and floor-skip-inventory.json.
floor_topup: complete; 16 steps exit 0, including beadstest, Darwin compilation and both runner self-tests; remote/floor-topup.json.
latency_leg: measured under a clear probe; both inventory tests PASS; remote/latency-leg.json.

diff_tests_executed:
```text
TestLintCityLoadWarningCarriesTheLoadError PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-fd986cf5bab8709b8d45.log)
  TestLintLoadsEachCandidateCityOncePerInvocation PASS (integration-packages-e4245ddf6a37030111bc.log, unit-1ab526405ace21879f85.log)
  TestLintNestedCityDoesNotShadowComposingCity PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-423c6a0da1986b90e9a7.log)
  TestLintNoCandidateLoadsKeepsOwnDirs PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-abd97d2cb9b450225d57.log)
  TestLintNotComposedWarningNamesPackAndCity PASS (integration-packages-75af79934639abaae69c.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestLintStandaloneAndRootPackEmitNoCityWarnings PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-f93a0ca96ccc61d0cae9.log)
  TestLintWarnsOncePerCityThatFailsToLoad PASS (integration-packages-821a75e4a503601af052.log, unit-d9e667428f972d067bd3.log)
  TestLintWarnsWhenNoCityComposesPack PASS (integration-packages-074fc6c34a1fb2577127.log, unit-752a0bf25b15b465517a.log)
GATE_TEST_EVIDENCE_RESULT ok: 8 diff-owned tests verified by name
```

## Skip justification

No owned test skipped or failed. All skip sites are outside the diff and their complete source-file blobs match the frozen base. New fixture helpers and the loader-count seam are local to lint tests with cleanup restoration; no unrelated skip guard was changed. Import-closure-reachable remote skip events (186) reproduce at the pinned base with the same commands and reasons; 315 other remote events have no shared-hunk reach. All native skips have same-reason base comparisons. Site, helper, closure and reason evidence is retained in skip-proof-verified.json, skip-comparisons/ and both skip-audit-prepared files.
Skip names and reason multiset are identical to the reviewed herdr gate inventory. Every skip source file is unchanged against this pinned base (independent verifier required). The only shared/helper hunks are cmd/gc lint fixtures, pure warning-text filtering and the loader-count seam; rg confirms the new fixture/filter helpers are called only by owned lint tests. The seam is restored by t.Cleanup; tests are sequential. Imports add fmt and the other shared hunk is a comment. Unrelated process-lane, platform, helper, optional-fixture, pending/golden/replay and live-tier skip guards are untouched. All reachable remote events and every native event now reproduce at this pinned base with matching reasons. No new, changed, owned or unjudged skip remains; the other remote events have unchanged source and tag-aware closure proof. The original controller failed before a baseline test; its corrected continuation and original error are retained.

## Required CI job accounting

ci_required_closure: ci_required_triggers.py /var/tmp/deploy-ga-xb7fkdr-r2.iiz4wctl/merge bad83762c64a670ec2c3626f15e498705e464ec7 97b781d308d9004a934e7222def3212848103c94 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs.
ci_required_accounting:

- changes | LOCAL-PASS | /var/tmp/deploy-ga-xb7fkdr-r2.iiz4wctl/ci-required/closure.stdout
- runner-policy | LOCAL-PASS | /var/tmp/deploy-ga-xb7fkdr-r2.iiz4wctl/ci-required/runner-policy.stdout
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | /var/tmp/deploy-ga-xb7fkdr-r2.iiz4wctl/ci-required/windows-native-reach.json
- windows-lane | LOCAL-PASS | /var/tmp/deploy-ga-xb7fkdr-r2.iiz4wctl/ci-required/windows-lane.stdout; exit=0
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'

Locally run job steps match the frozen workflow run commands, environment and working directories. Full commands and streams are in ci-required/*-steps.json. Runner policy used EVENT_NAME=pull_request, PR_AUTHOR=quad341 and empty FORCE_BLACKSMITH. Every locally run build job used a fresh view and the official load/isolation wrappers, serially. Trigger rows are copied from the derived closure.
windows_reach: none (credential-provider and native Windows-test closures checked).
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
Merge readiness requires CI / required green on the PR head and every deferred job successful, or skipped only because its own path filter is false.

## Load and retained proof

Remote admission uses its prescribed exemption from the host load wait. Every native leg retains the official measurements below; a bounded-wait timeout is recorded and is not a waiver.

- c3-run/evidence/remote/floor-topup.stdout: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1470 wait_timed_out=0 run_start_load=14.75 run_max_load=17.13 run_mean_load=16.63 run_readings=7 wait_first_load=73.29 wait_max_load=84.86 wait_mean_load=54.76 wait_readings=50 read_errors=0`
- ci-required/windows-lane.stderr: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=21.43 run_max_load=51.14 run_mean_load=40.96 run_readings=4 wait_first_load=17.64 wait_max_load=79.41 wait_mean_load=45.46 wait_readings=61 read_errors=0`
- skip-base-evidence/native.stderr: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=61.47 run_max_load=69.70 run_mean_load=63.23 run_readings=4 wait_first_load=79.20 wait_max_load=99.70 wait_mean_load=75.12 wait_readings=61 read_errors=0`
- storeclass-proof/wrapper.stderr: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=20.61 run_max_load=27.79 run_mean_load=22.98 run_readings=4 wait_first_load=16.62 wait_max_load=79.59 wait_mean_load=45.08 wait_readings=61 read_errors=0`

Independent audits: c6-verified.json; fast-independent-audit.json; reviewer-tests-verified.txt; lint-fixtures-verified.json; ac10-reviewed.json; resume-c3-audit.json (1068 artifact digests); acceptance-scope-audit.json; ci-required/accounting-verified.json; skip-proof-verified.json.
Skip-audit continuation: skip-controller-recovery.json records the deployer-owned early-context setup error; original failed service retained, no baseline test started there. The continuation reloads the finalized context and completes the pending proof. No test failure was attributed or waived.
All raw evidence remains at the root above. The deployer performs no merge.

## Recursive city lint comparison

The installed binary reports 93 fragment findings across 49 packs. The retained scoped-build observation reports one across the same 49 pack paths. The city commits differ only in test fixtures; pack configuration and prompt/template inputs are unchanged. ac10-current-comparison.json retains the two raw output hashes, city revisions and input-path checks. Other city lint diagnostics remain outside this increment. The scoped binary was not rebuilt or installed for this comparison.
