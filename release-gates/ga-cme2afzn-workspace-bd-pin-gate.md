# Workspace bd pin release gate — ga-cme2afzn

**Verdict:** **PASS**

Evaluated 2026-11-12T03:45:31.678447+00:00. Reviewed source `7939cea6024f9f24a187889c436f8bf1031e5690`; review `ga-2fmn7fib`; build contract `ga-l6d0zf`.
Pinned base `6bd609f5bfb41f43f444843b1220d0cde4473446`; merge `5cfdd5b5112ce617bcad8e05b89ba2594f41c24d`; tested tree `3b63aa7a75d5add2a279285f7c3a8c3b360c35bf`. The reviewed source has the identical tree. `docs/PROJECT_MANIFEST.md` is absent; criteria follow the deployer release-gate protocol, AGENTS.md and TESTING.md.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS. Live review verdict names the resolved source; no carryover or reviewer-added commit. |
| 2 | Acceptance | PASS. All 14 contract criteria checked below; 24 acceptance roots PASS, 0 FAIL/SKIP. PR-text requirements are in the prepared body. |
| 3 | Tests | PASS. Five fresh remote phases, native floor, complete skip audit, fast policy/drift lanes and required-CI accounting below. |
| 4 | High findings | PASS. Live security/style review records none; zero unresolved HIGH findings. Two documented comment nits remain non-blocking. |
| 5 | Clean branch | PASS. Source and tested scratch are clean. Publication adds only this record; normal pre-commit and pre-push hooks and a clean committed-head check are mandatory before handoff. |
| 6 | Merge/build | PASS. Clean frozen merge, `bazel build //...` and whole nogo/vet complete; both terminal BEPs independently checked. |
| 7 | One theme | PASS. Five commits, seven source paths: root workspace pin resolution, regression tests, BUILD entries, one related comment and its listener census row. No internal-doc contamination. |

## Acceptance checks

| AC | Independent evidence |
|---|---|
| 1 | Reviewer assertion-level RED: four FAIL, five control PASS. Fresh full-unit GREEN: all nine owned roots PASS. Before/after output is carried into the PR body. |
| 2 | Held-lock fixture control, optional/strict pin resolution and managed-city steady-state build pass by name, including the recovery-context path. |
| 3 | New typed root TOML reader; normalized size/mtime raw memo; clone on hit/miss; before/after stat guard; no composed-load fallback, error or missing-file cache. Source inspected. |
| 4 | Root/fragment/no-env equivalence and unresolved-import tests PASS. |
| 5 | First read, warm memo, rewrite invalidation, syntax-error recovery, missing file and one-read strict pin tests PASS. |
| 6 | Ambient environment expansion test PASS; validation, lookup/PATH and ambient-warning suffix is byte-identical to base. |
| 7 | Only two resolver bodies change; existing error strings unchanged; thirteen keep-green tests PASS. Complexity 7→5 and 15→15. |
| 8 | Zero growth (ii): base and merge complexity checks exit 2 with byte-identical output; pinned-base complexity-diff prints no threshold changes; baseline untouched. |
| 9 | Exact seven-path source scope; no config/API/event/flag, lock-entry, hosted-probe, recovery-rung or status-file change. |
| 10 | No added sleeps/open polling; held-flock pattern retained; lock tests !windows, neutral tests untagged. |
| 11 | Documentation search repeated; no pin-resolution owner found. PR states that result. |
| 12 | All sixteen new functions have doc comments; normal hooks active; full nogo/vet, format and BUILD regeneration/drift checks pass. Review records two non-blocking comment nits. |
| 13 | Stop condition untriggered: managed steady-state held-lock test PASS. |
| 14 | Prepared PR says Refs #7170, names the base, includes RED/GREEN, requests no deploy; no upstream comment/issue or pack/host-config change. |

## Suite and evidence

```text
merge_state: clean (origin/main@6bd609f5bfb41f43f444843b1220d0cde4473446)
gate_base_kind: current-tip
merge_validation: MPR validates the merge result at merge time; deploy clearance never authorizes an ungated head.
criterion-6-merge-validated: build and whole nogo/vet PASS
test_cmd: gate-criterion3.sh --repo /var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/merge --base 6bd609f5bfb41f43f444843b1220d0cde4473446 --bead-json /var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/ga-cme2afzn.json --owned /var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/owned.tsv --run-dir /var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/c3-run/evidence -- bash /var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/local-recipe-refusal.sh
test_cmd_scope: full-suite
test_counts: PASS=150723 FAIL=0 SKIP=515 (top-level result lines, repeated jobs counted separately)
test_exec_mode: remote-exec
test_exec_bep: per-phase table below; raw predicates and windows independently recomputed
tests_green: true
load_threshold: 15
load_waited_seconds: 720
load_wait_timed_out: 0
run_start_load: 14.61
run_max_load: 14.41
run_mean_load: 13.48
run_readings: 7
farm_test_seconds: 12303.403
fallback_reason: none
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 73.54010391235352, "waited_seconds": 0.001}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
owned_topup: none
floor_topup: 16/16 steps completed, each exit 0; includes beadstest, Darwin compile, runner self-tests and all environment-absence selections
heavy_mode: none
latency_leg: complete, measured on attempt 1 after farm probe clear; both named latency tests PASS
skip_inventory: 501 remote rows + 39 native rows, all justified; 327 unchanged source sites; 189 same-reason cmd/gc comparisons at base
waiver_ref: none (no gascity waiver path)
failure_attribution: none
test_log_dir: /var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/c3-run/evidence/remote/logs
ci_lane_run: n/a (no CI-config diff)
```

| Phase | Fresh remote results | Cached / retried / other strategy |
|---|---:|---|
| unit | 408 PASS | 0 / 0 / 0 |
| acceptance | 25 PASS | 0 / 0 / 0 |
| integration-packages | 86 PASS | 0 / 0 / 0 |
| integration-full | 14 PASS | 0 / 0 / 0 |
| integration-latency | 1 PASS | 0 / 0 / 0 |

diff_tests_executed:
  TestBdRuntimeEnvSteadyStateDoesNotWaitOnHeldRepoCacheLock PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-752a0bf25b15b465517a.log)
  TestWorkspaceEnvTableDoesNotCacheErrorsOrMissingCities PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-088cc38d26d094729229.log)
  TestWorkspaceEnvTableIsMemoizedByCityTOMLStat PASS (integration-packages-9326181a736eb30b71ca.log, unit-912ab643330b35bd0b6b.log)
  TestWorkspaceEnvTableMatchesTheComposedLoad PASS (integration-packages-e5330b8c981927473156.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestWorkspacePinDoesNotWaitOnHeldRepoCacheLock PASS (integration-packages-b72a67a2f9444af705e3.log, unit-fd986cf5bab8709b8d45.log)
  TestWorkspacePinFixtureComposedLoadWaitsOnHeldLock PASS (integration-packages-830685c60d0c2dd88567.log, unit-d9e667428f972d067bd3.log)
  TestWorkspacePinObservesTheAmbientEnvironmentOnEveryCall PASS (integration-packages-e4245ddf6a37030111bc.log, unit-cd6930bb0d0da8646500.log)
  TestWorkspacePinResolvesWhenAnImportCannotBeResolved PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-abd97d2cb9b450225d57.log)
  TestWorkspaceStrictPinReportsPathWithoutBdFromTheMemo PASS (integration-packages-12697e2ea0d32c278115.log, unit-f93a0ca96ccc61d0cae9.log)
GATE_TEST_EVIDENCE_RESULT ok: 9 diff-owned tests verified by name

skip_justification: All skip sites/functions are unchanged. The three SHARED-HUNKS paths are confined to cmd/gc (two new test-helper files and one comment-only edit); its 186 remote skip rows and 3 native skip rows reproduce at the pinned base with the same reasons, individually bound to their originating Bazel target/config or exact native command. Other skipped packages have computed module test import closures excluding cmd/gc and all shared/helper changes; each retained reason is in the inventories. All environment-absence skips received the canonical native floor; its remaining skips are inventoried. One non-Go Python skip is non-applicable macro configuration, unchanged at base, with its opted-in dashboard counterpart fully passing. No owned test skips and no failure is attributed.
Exact inventories, source/closure decisions, per-target baseline commands and comparison summaries: `/var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/skip-audit-prepared.json`, `/var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/native-skip-audit-prepared.json`, `/var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/skip-comparison/verified.json`. One initial baseline setup was refused for directory permissions before any BEP/test execution; its untouched refusal is retained separately, and fresh 0700 evidence was used. No failed test was rerun.

Native floor load (mandatory local wait):
```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=720 wait_timed_out=0 run_start_load=14.61 run_max_load=14.41 run_mean_load=13.48 run_readings=7 wait_first_load=72.51 wait_max_load=72.51 wait_mean_load=40.99 wait_readings=25 read_errors=0
```

## Fast lanes and required CI

policy_lane: PASS — pinned-base fmt-check-changed, whole lint/nogo, four make-check shell guards, check-hooks and thirteen policy targets.
drift_lane: PASS — make bazel-sync + empty git diff/status, seven generated-artifact checks; OpenAPI breaking check PASS. Each lane ran in a separate fresh committed view before the suite. Trusted-base storeclass build/baseline/growth all exit 0.
Exact commands and fresh-view identities: `/var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/fast-lane.sh`, `/var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/fast-verified.json`, `/var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/storeclass-proof/verified.json`. Policy load wait:
```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=41.13 run_max_load=51.82 run_mean_load=48.86 run_readings=5 wait_first_load=31.61 wait_max_load=75.27 wait_mean_load=47.84 wait_readings=61 read_errors=0
```

ci_required_closure: python3 ci_required_triggers.py /var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/merge 6bd609f5bfb41f43f444843b1220d0cde4473446 7939cea6024f9f24a187889c436f8bf1031e5690 fork; workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs. Runner-policy resolves Linux expressions to Ubuntu.

ci_required_accounting:

| Job | Accounting | Command/result/evidence |
|---|---|---|
| changes | LOCAL-PASS | `/var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/ci-required/closure.stdout` |
| runner-policy | LOCAL-PASS | `/var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/ci-required/runner-policy.stdout` |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | `/var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/ci-required/windows-native-reach.json` |
| credential-provider-windows | NOT-TRIGGERED | `  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'` |
| windows-lane | LOCAL-PASS | `/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/merge -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- python3 /var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/run-ci-job.py windows-lane`; exit 0; `/var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/ci-required/windows-lane.stdout`, `/var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/ci-required/windows-lane.stderr` |
| pack-gate | NOT-TRIGGERED | `  pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'` |

windows_reach: credential-provider = none; Windows-native-test allowlist = none.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none; host_gaps: none.
Job steps were matched to the frozen workflow verbatim, including Windows and Darwin build/test-compiles. Proof: `/var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/ci-required/accounting-verified.json`.

## Audit pointers

Root `/var/tmp/deploy-ga-cme2afzn-fresh._d694bu9`. Source acceptance `/var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/acceptance-precheck.json`; runtime names `/var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/acceptance-runtime-verified.json`; raw suite/CAS SHA-256 verification `/var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/resume-c3-audit.json`; C6 `/var/tmp/deploy-ga-cme2afzn-fresh._d694bu9/c6-verified.json`. All raw BEPs, per-job logs and baseline refusals are retained. No install/deploy is requested. Merge authority is MPR; after landing, notify mayor and architect and record the landing SHA for the deferred regression-test follow-up.
