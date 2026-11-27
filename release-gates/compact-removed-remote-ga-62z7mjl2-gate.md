# Release gate: compact pending push to a removed remote

**Verdict:** **PASS**

Evaluated 2026-11-16T03:44:46.828790+00:00; bead `ga-62z7mjl2`, build `ga-zvgiw5`, review `ga-c0wf7ic9`.
Reviewed source: `77c64735786da7edbee86e631042eaf8b67afac8`. Isolated branch: `deploy/ga-62z7mjl2-gate`.
DEPLOY_MODE: remote; PUSH_REMOTE: fork; base: `origin/main@294079d53f5736601a12d8e9961c393fadce6105`.
merge_state: clean (origin/main@294079d53f5736601a12d8e9961c393fadce6105)
gate_base_kind: current-tip
Tested merge HEAD: `2f0355216fe6f098b11eeb4ef4e8c7011360396d`; tree: `a8514dc2237a98a8755819cc099bc0195ec6076b`.
merge_validation: clean pinned merge built, vetted and tested; MPR owns current-head CI and merge-time currency.
Preflight: own-author source-branch PR lookup and reviewed-commit pulls lookup returned no PR; source not landed. No self-rebase.

| # | Criterion | Verdict and evidence |
|---|---|---|
| 1 | Review PASS | PASS. Closed review ga-c0wf7ic9 says pass and pins the exact source above; no carryover or reviewer-added commits. |
| 2 | Acceptance | PASS. Guard and retry paths classify a removed remote only after a successful zero-count probe; terminal mail uses sidecar bookkeeping, leaves the evidence marker byte-identical, and keeps the database blocked. Existing/unanswered remotes retain prior handling. New markers retain explicit unverified-head metadata. Seven regression roots PASS; no production marker action. |
| 3 | Tests | PASS. Complete five-phase uncached remote suite, canonical native floor, by-name ownership, SKIP proofs, fast lanes and CI accounting below. |
| 4 | Findings | PASS. Zero unresolved HIGH findings. Optional invalid-remote guard coverage remains follow-up ga-0iv96wbo. |
| 5 | Clean branch | PASS. Isolated branch was pristine at the reviewed source; only this gate record is added. Post-commit cleanliness is required before push. |
| 6 | Build and merge state | PASS. Normal-hook merge materialization, bazel build //... and make vet returned 0; build/vet BEPs overallSuccess=true on the exact merge tree. |
| 7 | One theme | PASS. Five paths cover compact removed-remote classification, its fixture/tests/BUILD entry and recovery guidance. All five source commits cite ga-zvgiw5; ancestry denylist/citation checks pass. |

## Test evidence

test_cmd_scope: full-suite
heavy_mode: none
waiver_ref: none
failure_attribution: none (zero failed tests)
ci_lane_run: n/a (no CI configuration change)
policy_lane: PASS; make check-hooks fmt-check lint; release-dist/routed-row/split-topology/residency guards; ten Bazel policy/format targets.
drift_lane: PASS; make bazel-sync plus git diff --exit-code and empty porcelain; eight generated/docsync targets. Each lane ran in its own fresh view through gate-base.sh at the pinned base, before the suite.
complexity_zero_growth: PASS (ii); base/merge complexity-check both rc2 with identical diagnostics, pinned complexity-diff rc0 prints no threshold changes; no new/regressed/deleted rows.
test_cmd: canonical gate-criterion3.sh from `/var/tmp/gc-merge-validate-ga-62z7mjl2-5bn_gtje/merge`, pinned base above; unit make -k check (bazel test //...), Tier A acceptance including solo tests, full integration packages, full integration excluding the declared latency tests, then only those tests under the latency probe. All five phases force --config=remote-exec --config=ci --nocache_test_results --noremote_local_fallback; verbose per-job output retained.
test_counts: PASS=154800 FAIL=0 SKIP=607 (top-level result lines summed by job; repeated tier executions remain separate).
test_exec_mode: remote-exec
test_log_dir: `/var/tmp/gc-gate-ga-62z7mjl2-c7v01294/criterion3-run-2/evidence/remote/logs`
test_exec_bep: five phase files under `/var/tmp/gc-gate-ga-62z7mjl2-c7v01294/criterion3-run-2/evidence/remote`; independent raw predicate and 1082 retained CAS artifact hashes verified.

| Phase | Results | Remote / fresh first attempt | Cached / other / retry |
|---|---:|---:|---:|
| unit | 415 | 415, start inside phase window | 0 / 0 / 0 |
| acceptance | 25 | 25, start inside phase window | 0 / 0 / 0 |
| integration-packages | 86 | 86, start inside phase window | 0 / 0 / 0 |
| integration-full | 14 | 14, start inside phase window | 0 / 0 / 0 |
| integration-latency | 1 | 1, start inside phase window | 0 / 0 / 0 |

rbe_state: eligible; certificate expires 2027-01-09; worker environment pin matches; sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72.
fallback_reason: none. admission: remote cap2, floor25GiB, free69.57GiB, wait0s; prior zero-test admission deferral retained separately.
farm_test_seconds: 11671.544
latency_leg: measured; clear probe on first attempt, both declared latency roots PASS. Whole-suite latency results were excluded from verdict.
owned_topup: none needed; all seven owned roots PASS in the complete suite.
floor_topup: complete; all16 canonical native steps rc0 (environment-absence packages, beadstest, Darwin compile and runner self-tests).
GATE_ICU74: verdict=OK provenance=recorded (all three pinned sonames). Pinned test bd v1.3.4 and test-owned runtime used; wrapper reported pass-through, with live-store selector removal where stated, rather than a claim that all ambient variables were stripped.

diff_tests_executed:
- TestCompactScriptDryRunDoesNotReportUnsatisfiablePendingPush PASS (integration-packages-17007f3ad20cb33b8d6a.log, unit-6b47d0615f42f4ae2b8d.log)
- TestCompactScriptFetchFailureMarkerRecordsUnverifiedRemoteHead PASS (integration-packages-aa557e5bc0c7045b52d7.log, unit-89d44f035a0dc6a9b6c9.log)
- TestCompactScriptPendingPushRemoteProbeFailureKeepsRecurringAlert PASS (integration-packages-0708b0d0c2f548d0a722.log, unit-98fce3c243b1e6971368.log)
- TestCompactScriptPendingPushToExistingRemoteIsNotUnsatisfiable PASS (integration-packages-a9e12e81511731ef69dd.log, unit-9d67ff2f074bc8f49779.log)
- TestCompactScriptPendingPushToRemovedRemoteReportsUnsatisfiableOnce PASS (integration-packages-aa557e5bc0c7045b52d7.log, unit-89d44f035a0dc6a9b6c9.log)
- TestCompactScriptSharedHistoryGuardKeepsPendingPushHeldWhileRemoteResolves PASS (integration-packages-2c974992a3e988636c94.log, unit-da81bca7c80446c5af28.log)
- TestCompactScriptSharedHistoryGuardReportsPendingPushToRemovedRemoteUnsatisfiable PASS (integration-packages-130aa78402132a9cb052.log, unit-ff6413fa61a58d5e02ec.log)

skip_inventory: full original inventory retained; all310 printed sites resolve with line_in_hunk=no. Both SHARED-HUNKS entries (new test file header and fake-Dolt helper) were named in all-tag import closures. All383 reachable original package/test keys have same-pinned-base, same-tier, same-name/reason proof across27 farm comparisons. Remaining99 keys are unreachable. All72 native SKIPs separately match the same native command at the pinned base. No diff-owned SKIP or missing result.
skip_justification: unchanged platform/helper/opt-in skips and environment-absence skips inventoried by name/reason; canonical native top-ups supply environment-dependent execution. Shared-hunk reach is closed by the base comparisons above, rather than inferred from a passing count.
Native comparison driver rc1 was an audit-only refusal on examples/gastown with zero HEAD SKIPs; all16 test steps had already returned0. Final retained-log comparison checks real PASS/RUN output for all7 SKIP-free packages and ALL PRE-EXISTING for each nonempty comparison. No tests were retried.

## Required CI accounting

ci_required_closure: ci_required_triggers.py `/var/tmp/gc-merge-validate-ga-62z7mjl2-5bn_gtje/merge` `294079d53f5736601a12d8e9961c393fadce6105` `77c64735786da7edbee86e631042eaf8b67afac8` fork; workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs. check is an aggregator.

| Job | Status | Evidence |
|---|---|---|
| changes | LOCAL-PASS | Trigger tool rc0; filtered suite, packs/credential_provider/shared no match. |
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=temp python3 .github/workflows/scripts/runner_policy.py; rc0. |
| credential-provider-windows | NOT-TRIGGERED | Tool: credential_provider filter no match. |
| pack-gate | NOT-TRIGGERED | Tool: packs filter no match. |
| windows-lane | LOCAL-PASS | Actual four YAML run blocks, including tools/nogo working directory, ran sequentially in fresh view; CI_COMMAND steps4-7 PASS, detached rc0. GOOS=windows build/test-compile allowlists and analyzers; GOOS=darwin build/test-compile all packages. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Windows credentialprovider/genshell test import closure computed; no changed non-test input reachable. |

windows_reach: none (credential-provider and windows-lane-test closures).
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
Merge only after CI / required is green on the PR head and the deferred job succeeds (or skips because its own filter is false). This PASS authorizes PR-open, not merge.

## Load and retained records

load_threshold: 15; load_waited_seconds: 0; load_wait_timed_out: 0; run_start_load: 25.64; run_max_load: 74.71; run_mean_load: 46.80; run_readings: 99 (canonical full chain summary; native reference and CI waits below).

- criterion3-run-2/units/gate.w1.stderr.log: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=25.64 run_max_load=74.71 run_mean_load=46.80 run_readings=99 wait_first_load=25.64 wait_max_load=25.64 wait_mean_load=25.64 wait_readings=1 read_errors=0`
- native-original-skip-base-run/units/gate.w1.stderr.log: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=50.48 run_max_load=57.96 run_mean_load=52.86 run_readings=9 wait_first_load=84.25 wait_max_load=89.66 wait_mean_load=67.44 wait_readings=62 read_errors=0 prewait_s=1801`
- ci-required/windows-lane/units/gate.w1.stderr.log: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=960 wait_timed_out=0 run_start_load=14.91 run_max_load=27.25 run_mean_load=22.90 run_readings=5 wait_first_load=38.38 wait_max_load=41.08 wait_mean_load=26.42 wait_readings=34 read_errors=0 prewait_s=960`

Remote phases use bounded remote admission without a local-heavy wait; native baseline and cross-build use the ordinary local-heavy cap and load wait. Fast-lane wait/run readings remain in fast-independent-audit.json.

Detailed evidence is retained outside the PR to keep the record reviewable:
- `/var/tmp/gc-gate-ga-62z7mjl2-c7v01294/final-independent-evidence-audit.json` (4402 bytes; SHA256 `11ca52ee05b381b89052515bb422260ba068c368656e313079dc7b5a547ac73b`)
- `/var/tmp/gc-gate-ga-62z7mjl2-c7v01294/criterion3-run-2/evidence/execution.json` (323310 bytes; SHA256 `57eca843854fbc50f2ac9deae83b31e9df9486cc3018bbb4392b2e30849da523`)
- `/var/tmp/gc-gate-ga-62z7mjl2-c7v01294/fast-independent-audit.json` (4293 bytes; SHA256 `ddf43f7bffa235815fc7c31422754be414f9afffe75e1d8d882180d70e33d7eb`)
- `/var/tmp/gc-gate-ga-62z7mjl2-c7v01294/c6-result.json` (721 bytes; SHA256 `fcf5925244c2c8c317e464bd417c6d1cf03778896dafe29ca8d47a9b8b97fd24`)
- `/var/tmp/gc-gate-ga-62z7mjl2-c7v01294/all-skip-site-audit.json` (293308 bytes; SHA256 `af1cc4d83a76286687529aff6409784c7d53cc6018674e79ac0955f2b8fefff8`)
- `/var/tmp/gc-gate-ga-62z7mjl2-c7v01294/all-skip-comparison-coverage.json` (272127 bytes; SHA256 `c0e2c574e941b6078e98a76caaca5bdcf6f71354349f7405325017f05db3b850`)
- `/var/tmp/gc-gate-ga-62z7mjl2-c7v01294/all-skip-base-result.json` (103020 bytes; SHA256 `c5ad3214faf071cf4ca4dafaeda13b20c44deb1631d040f709143a990bd34458`)
- `/var/tmp/gc-gate-ga-62z7mjl2-c7v01294/native-original-skip-comparison-final.json` (15570 bytes; SHA256 `bf6329b3fea08312a1dbf219a6a80f6229dc5d4ad6f0f7ffdd20245ae9e14a8c`)
- `/var/tmp/gc-gate-ga-62z7mjl2-c7v01294/ci-required/closure.txt` (1279 bytes; SHA256 `3d70beda08303c204b3182cd157dc6f4ded35e6cb86be9616674b8a6a4260b73`)
- `/var/tmp/gc-gate-ga-62z7mjl2-c7v01294/ci-required/windows-lane-workflow-spec.json` (893 bytes; SHA256 `3606fcdcaefb4037ba1f0dd4b7676aed2d061245f77be77b44f0d5609d71b433`)
