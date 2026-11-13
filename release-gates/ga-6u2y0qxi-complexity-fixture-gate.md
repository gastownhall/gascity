# Minimal Git fixtures for complexity-script tests

**Verdict:** **PASS**

Bead: ga-6u2y0qxi; build: ga-zj6pxm; review: ga-6n96ma03.
Reviewed source: `19d21b38bf78e290f8d21f49b6c06e7dfd93ce2c`; frozen base: `6bd609f5bfb41f43f444843b1220d0cde4473446`.
Validation merge: `079e6d4552264317e225f2e333984258dbf41b7f`; tree: `dd7fcd52bb30da28d207b1218c73c5ff2db926ec`.
Evaluated: 2026-11-11T23:51:48.114187+00:00

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Closed review PASS names the resolved source SHA; no carryover or reviewer-added-test exception. |
| 2 | Acceptance criteria met | PASS | Six original test bodies and every assertion, fake tool, environment and runner helper are byte-identical after fixture setup replacement. New one-script/one-commit sentinel passes. All seven owned tests pass below one second. Required timing comparisons are carried into the PR. |
| 3 | Tests pass | PASS | Five fresh remote phases, native floor, latency lane, fast checks, package-bound by-name proof, skip audit and required-job accounting complete. |
| 4 | No high-severity findings open | PASS | Reviewer reports zero HIGH findings; one inherited Git environment hardening suggestion is informational. |
| 5 | Final branch clean | PASS | Source and validation tree clean before adding this intentional gate record; final committed cleanliness is checked before publication. |
| 6 | Merge state recorded; build/vet pass | PASS | Clean frozen merge passes full remote Bazel build and whole nogo/vet; terminal events, parents and tree verified. |
| 7 | Single feature theme | PASS | One test file replaces full-tree Git snapshots/archives with a minimal fixture; production script and BUILD files unchanged by this source range. |

merge_state: clean (origin/main@6bd609f5bfb41f43f444843b1220d0cde4473446)
merge_validation: merge result built, vetted and tested here; current-head merge checks remain required.
gate_base_kind: current-tip
gate_base: 6bd609f5bfb41f43f444843b1220d0cde4473446
Release policy source: required deployer protocols, TESTING.md, Makefile and engdocs/contributors/release-gate-criteria-conventions.md; docs/PROJECT_MANIFEST.md is absent.

## Test evidence

test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/merge --base 6bd609f5bfb41f43f444843b1220d0cde4473446 --bead-json /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/ga-6u2y0qxi.json --owned /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/owned.tsv --run-dir /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/c3-run/evidence; exit 0
test_cmd_scope: full-suite
test_counts: {"FAIL": 0, "PASS": 150699, "SKIP": 516}
canonical_full_suite_counts: {"FAIL": 0, "PASS": 150699, "SKIP": 515}; supplemental_native_counts: {"FAIL": 0, "PASS": 0, "SKIP": 1}
Counts are top-level Go result lines per job; subtest skip events and the non-Go configured skip are inventoried separately.
test_log_dir: /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/c3-run/evidence/remote/logs
test_bd: GASCITY_TEST_BD version=v1.3.4 bin=/home/jaword/.local/bd-versions/v1.3.4/bd reports='bd version 1.3.4 (7ef2fd05e: HEAD@7ef2fd05eb0c)' ref_check=match:7ef2fd05e origin=preexisting pin_source=/var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/merge/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
runtime_preflight: ICU loader and disk checks PASS; /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/runtime-preflight.txt; sha256=ac2ec9ad8b210e7e2af210d69a6d7d11aff54356f6cd9cedc73f06dd7b3e4522
test_exec_mode: remote-exec
test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 25, "results": 25, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 86, "results": 86, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 408, "results": 408, "retried": 0, "window_ok": true}}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
fallback_reason: null
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 76.22629928588867, "waited_seconds": 0.001}
farm_test_seconds: 13679.829
skip_inventory: /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/c3-run/evidence/remote/skip-inventory.json; sha256=9d1b0bf7f196b81140439deeefe8b6eb1312a18377e74852c9d8290e5e878352
owned_topup: /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/c3-run/evidence/remote/owned-topup.json; sha256=37517e5f3dc66819f61f5a7bb8ace1921282415f10551d2defa5c3eb0985b570
floor_topup: complete=true; 16/16 commands exit 0; /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/c3-run/evidence/remote/floor-topup.json; sha256=8213b91022b621256d8ecf12089425d57199f16ed627a34433e7c448f492f0a9
supplemental_floor_topup: actual remote upstream-storage absence independently observed natively on the gated tree; /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/supplemental-floor/verified.json; sha256=c2bd59d300aaea1667882ba2fd8f372aba73a7849fde48bfe969b89dca154040
latency_leg: measured, complete=true, rc=0; named tests PASS after a clear farm probe; /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/c3-run/evidence/remote/latency-leg.json; sha256=8f885a8816c6260c331bb47d5e210273af135c932cbb002b991069e7838d3885
heavy_mode: none (computed)
waiver_ref: none (gascity has no waiver path)
failure_attribution: none
ci_lane_run: n/a (no CI-config diff)

diff_tests_executed:
```text
TestComplexityDiffDuplicateKeysRespectThreshold PASS (unit-9f907e0fea7ca680f21c.log)
  TestComplexityDiffIgnoresBaselineContents PASS (unit-81d8a9af827e251f6b12.log)
  TestComplexityDiffRejectsMissingBaseRef PASS (unit-271bc77ad6715453dfe1.log)
  TestComplexityDiffReportsHighToLowAsImproved PASS (unit-b0c13e82fd961ff4f637.log)
  TestComplexityFixtureRootIsAOneCommitScriptOnlyRepo PASS (unit-b0c13e82fd961ff4f637.log)
  TestComplexityReportRejectsInvalidMode PASS (unit-db0feb4ad70da6248282.log)
  TestComplexityReportUpdateAndDiffUseStableJSONKeys PASS (unit-ee115c844954da53d34d.log)
```
owned_test_seconds: {"TestComplexityDiffDuplicateKeysRespectThreshold": 0.09, "TestComplexityDiffIgnoresBaselineContents": 0.03, "TestComplexityDiffRejectsMissingBaseRef": 0.02, "TestComplexityDiffReportsHighToLowAsImproved": 0.13, "TestComplexityFixtureRootIsAOneCommitScriptOnlyRepo": 0.16, "TestComplexityReportRejectsInvalidMode": 0.01, "TestComplexityReportUpdateAndDiffUseStableJSONKeys": 0.07}

## Acceptance and timing comparisons

PASS; six original tests and all assertions, fake tools, env and runners byte-identical after prescribed setup replacement; fixture-shape sentinel added; only one test file changed.
Authoritative Bazel coverage exercises the same tests as the legacy go-test acceptance wording; no ordinary go-test substitute is claimed.
Earlier investigator back-to-back comparison at approximately 28 load: seven filtered tests 7.73 s before, 0.38 s after the fixture prototype. Raw historical logs are no longer present at their original temporary path; these figures are attributed to the build record.
Builder comparison: RED at approximately 60 load; GREEN load not recorded. Stable JSON keys 3.12 to 0.12 s; invalid mode 0.00 to 0.02 s; missing base ref 0.01 to 0.04 s; baseline ignored 0.79 to 0.07 s; duplicate thresholds 2.98 to 0.21 s; high-to-low improvement 0.71 to 0.08 s; whole filtered target 0.8 s. Reviewer remote observations 0.00 to 0.07 s per test. These are distinct from this run’s verified unit timings above.

## Fast lanes

policy_lane: independent fresh-tree view through gate-base.sh run; pinned changed-format check, whole nogo/lint, required shell guards and 13 explicit policy targets; exit 0.
drift_lane: independent fresh-tree view; make bazel-sync, git diff --exit-code, empty status including untracked files, seven generated/client/lockfile targets; exit 0.
openapi_lane: independent fresh-tree view; make openapi-breaking-check against the frozen base; exit 0.
storeclass_policy: independent fresh-tree view, trusted-base generator and strict baseline/growth predicates; exit 0.
known_red_precheck: known-red-precheck: proceed reason=fix-carrying (its build bead ga-zj6pxm is stamped gc.fixes_tracker=ga-vkhfnj); exit 0; no gate waiver.

## Skip justification

All seven changed test roots executed and passed. The remote skip inventory and native floor skips are in unchanged source files, outside the changed complexity helper and its test-binary import reach. Reasons cover existing unit/process-lane routing, platform/privilege limitations, provider capability and deliberate helper-process fixtures, optional replay/DSN/registry/manual inputs, tracked existing debt, and tool/service absence. The canonical native floor covers environment-absence packages; its remaining skips are unchanged opt-in/provider fixtures, optional PostgreSQL/br prerequisites and tmux bindings requiring a surrounding tmux session. The additional native upstream-storage observation has its own retained result and complete printed reason. No skipped test is diff-owned, no shared/helper hunk reaches a skipped test, no failure is attributed and no waiver is used. Per-site evidence and import closures are retained with the automated proof.
Per-site, source/blob, import closure and any required same-reason base comparisons: /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/skip-proof-automated.json; sha256=60c8f27e3490457a22af2f20e7e40b5f1bfd7d5d69358409f6d601f259382bbb
Non-Go configured skip: Existing bridge lockfile unittest defaults to no --rules-ts-typescript; sole configured non-applicable skip. Source, macro and BUILD input blobs match pinned base; dashboard counterpart opts in.
Parallel-output reason recovery, when needed, uses only the same-action hash-verified XML, matching raw log and literal source. Original suite logs remain untouched; /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/skip-reason-recovery.json; sha256=37517e5f3dc66819f61f5a7bb8ace1921282415f10551d2defa5c3eb0985b570

## Required-job accounting

ci_required_closure: ci_required_triggers.py /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/merge 6bd609f5bfb41f43f444843b1220d0cde4473446 19d21b38bf78e290f8d21f49b6c06e7dfd93ce2c fork; workflow_blob=fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs; exit 0.
```text
diff 6bd609f5bfb41f43f444843b1220d0cde4473446...19d21b38bf78e290f8d21f49b6c06e7dfd93ce2c: 1 changed files
  filter packs                no match
  filter credential_provider  no match
  filter shared               no match
suite_mode: filtered  (shared filter did not match)

ci-required needs 7 jobs; transitive closure 7 jobs
job key                                  kind       runs-on                    trigger
runner-policy                            LEAF       linux                      EVERY-PR
changes                                  LEAF       linux                      EVERY-PR
check                                    AGGREGATOR linux                      EVERY-PR
  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
  pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
  windows-lane                           LEAF       linux                      EVERY-PR
  windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR

6 LEAF jobs need a status; the rest are rolled up through their needs.
```

ci_required_accounting:

- changes | LOCAL-PASS | {"evidence": "/var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/ci-required/closure.stdout", "status": "LOCAL-PASS"}
- runner-policy | LOCAL-PASS | {"evidence": "/var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/ci-required/runner-policy.stdout", "status": "LOCAL-PASS"}
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | {"evidence": "/var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/ci-required/windows-native-reach.json", "reason": "non-linux-runner", "status": "DEFERRED-TO-PR-CI", "windows_reach": "none"}
- credential-provider-windows | NOT-TRIGGERED | {"evidence": "  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'", "status": "NOT-TRIGGERED", "windows_reach": "none"}
- windows-lane | LOCAL-PASS | {"command": ["/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh", "--from", "/var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/merge", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh", "--threshold", "15", "--max-wait", "1800", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh", "--", "python3", "/var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/run-ci-job.py", "windows-lane"], "rc": 0, "status": "LOCAL-PASS", "stderr": "/var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/ci-required/windows-lane.stderr", "stdout": "/var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/ci-required/windows-lane.stdout"}
- pack-gate | NOT-TRIGGERED | {"evidence": "  pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'", "status": "NOT-TRIGGERED"}

windows_reach: {"credential": [], "native": []}
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none
Merge requires current-head CI / required and every deferred job to pass. This gate authorizes PR-open only.

## Observed load

c3-run/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=58.74 run_max_load=74.24 run_mean_load=69.76 run_readings=13 wait_first_load=119.65 wait_max_load=119.65 wait_mean_load=88.32 wait_readings=61 read_errors=0
c3-run/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=45.72 run_max_load=119.96 run_mean_load=76.80 run_readings=113 wait_first_load=45.72 wait_max_load=45.72 wait_mean_load=45.72 wait_readings=1 read_errors=0
ci-required/windows-lane.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=22.82 run_max_load=24.93 run_mean_load=23.82 run_readings=3 wait_first_load=76.42 wait_max_load=91.43 wait_mean_load=57.25 wait_readings=61 read_errors=0
skip-base-evidence/native.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=360 wait_timed_out=0 run_start_load=14.56 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=21.75 wait_max_load=21.75 wait_mean_load=18.13 wait_readings=13 read_errors=0
load_threshold: 15
load_waited_seconds: 1800
load_wait_timed_out: 1
run_start_load: 58.74
run_max_load: 74.24
run_mean_load: 69.76
run_readings: 13

## Retained evidence

- /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/c6-verified.json; sha256=8d8f8872ffbfb31a72ba2372e4fde4cc337d82ceeacb5638324cafe754e1512c
- /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/fast-verified.json; sha256=06d20d0f8a45e75bcd6d1e7624365e92c851e6177505dfa9e4316e347401b93b
- /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/resume-c3-audit.json; sha256=f54a8664959d9bc7919cd2ac01092b22f0b1646c14570442285fa872d4104686
- /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/owned-package-verified.json; sha256=4a161724cbfbedd55051faca09f75ff51ca021ecf7c1ce0568b8824445ff24ea
- /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/owned-timings-verified.json; sha256=6b72860898063b84668f96ff97d0acceaeebdf4e17f93099ad514f52d99937ea
- /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/ci-required/accounting-verified.json; sha256=4292493008f54bda2dbd23d8462ede6db5904be34fa56f37dfbaf11c3e5e598d
- /var/tmp/deploy-ga-6u2y0qxi-fresh.g8y6ktdj/final-manual-review.json; sha256=1c8688de2b6aeb1a508632e49b8c577f8c9787803cc06cbdc31b1cff9d2b2dce
