# Managed Dolt startup: remove unreachable foreign-holder arm

**Verdict:** **PASS**

Deploy bead: ga-5ftfq569. Build: ga-9swd9p. Review: ga-a1p86ncu.
Reviewed source: `f3041e47b208574432e248a4cca614d036bcc5cb` (resolved against the object store).
Deploy mode: remote; base ref: origin/main; push remote: fork (quad341/gascity).
Ancestry scope: PASS for deploy/build IDs; no stacked PR. Preflight source-to-PR lookup: empty.

gate_base: `c377651cb318ed567997b5c2e81d029a55a56420`
gate_base_kind: current-tip
gate_head: `b1ed4f28b741c9b3a0fa8d948faab557cdf2196e` (tested merge)
merge_state: clean (origin/main@c377651cb318ed567997b5c2e81d029a55a56420)
merge_validation: merge result built and vetted here; later base changes are checked at merge time.
The tested merge differs from its pinned base by the reviewed four-line deletion only.

| # | Criterion | Verdict and evidence |
|---|---|---|
| 1 | Review PASS | PASS: closed review ga-a1p86ncu records verdict pass on the resolved source above; no carryover or added reviewer commit. |
| 2 | Acceptance criteria | PASS: only the unreachable foreign-holder else arm is deleted. Running-probe success, owned-server stop, and helperless foreign-holder handling remain. Five existing regression owners PASS by exact name in the process tier. |
| 3 | Tests and CI coverage | PASS: five fresh remote phases, all 16 native floor checks, policy/drift checks and the required cross-compile job completed successfully. Full counts and every ci-required leaf account appear below. |
| 4 | High-severity review findings | PASS: zero unresolved HIGH findings in style, spec and security review. |
| 5 | Final branch clean | PASS: gate record is the only addition above the reviewed source; final git status is checked after its commit and before push. |
| 6 | Merge/build/vet | PASS: clean merge on the pinned base; remote builds of gc and gc-osc7501 and full-tree nogo lint/vet exit 0. |
| 7 | Single feature theme | PASS: one managed-Dolt-startup cleanup in examples/bd/assets/scripts/gc-beads-bd.sh, +0/-4. |

## Acceptance and fast lane

A successful load_probe_managed_from_gc requires resolve_gc_helper_bin to be nonempty. op_start snapshots the same helper value, making the deleted empty-helper guard unreachable. The retained helperless shell-inspection path supplies foreign-holder protection.

Existing owners verified by name: TestGcBeadsBdStartLeavesForeignPortHolderAlone, TestGcBeadsBdStartUsesGCBinManagedConfigWriter, TestGcBeadsBdStartManagedHelperDoesNotInheritStartLockFD, TestDoltStateProbeManagedCmdReportsImposterHolder, TestDoltStateStopManagedCmdDoesNotKillImposterPortHolder. Default-tier skips for three of these are paired with process-tier PASS results. No new decision path or test is introduced.

Shell syntax (sh, dash, bash) and git diff --check: PASS.
policy_lane: make -k fmt-check check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary -> PASS.
GLT_RECORD: golangci-lint pin=2.14.0 resolved=2.14.0 mismatch=false.
drift_lane: make bazel-sync BAZEL_SYNC_FLAGS='--config=remote-exec --noremote_local_fallback', then git diff --exit-code -> PASS.
Each fast lane ran in its own fresh-tree-run.sh view, with policy's gate-base.sh pin set to the base above. The suite started after both passed.
known_red_precheck: proceed reason=nothing-known-red; no failure attribution.
GATE_ICU74 verdict=OK provenance=recorded; all three libraries verified.

C6 load: threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.87 run_max_load=26.26 run_mean_load=23.00 run_readings=2.

## Full-suite test evidence

test_cmd: gate-criterion3.sh --repo /var/tmp/deploy-ga-5ftfq569.VHpCqPq4/merge --base c377651cb318ed567997b5c2e81d029a55a56420 --bead-json /var/tmp/deploy-ga-5ftfq569.VHpCqPq4/bead.json --owned /var/tmp/deploy-ga-5ftfq569.VHpCqPq4/owned.tsv --run-dir /var/tmp/deploy-ga-5ftfq569.VHpCqPq4/suite/evidence (launched through gate-detached-run.sh).
test_cmd_scope: full-suite
test_exec_mode: remote-exec
test_counts: PASS=158887 FAIL=0 SKIP=618 (top-level results summed across retained per-job logs).
diff_tests_executed: none (no diff-owned tests).
waiver_ref: none.
heavy_mode: none.
ci_lane_run: n/a (no CI-config change).
failure_attribution: none.
fallback_reason: none.
test_log_dir: /var/tmp/deploy-ga-5ftfq569.VHpCqPq4/suite/evidence/remote/logs (570 retained logs; independently reread with gate-test-evidence.py).

| Phase | Result | Fresh remote test-result actions | Cache credits / retries | Window valid |
|---|---|---:|---|---|
| unit | PASS | 426 | 0 / 0 | yes |
| acceptance | PASS | 25 | 0 / 0 | yes |
| integration-packages | PASS | 87 | 0 / 0 | yes |
| integration-full | PASS | 15 | 0 / 0 | yes |
| integration-latency | PASS | 1 | 0 / 0 | yes |

554 fresh remote test-result actions total; no evidence errors.
farm_test_seconds: 14806.273.
admission: remote cap2, floor25 GiB, free43.0416 GiB, waited0.001 seconds; local floor retains40 GiB policy.
rbe_state: rc marker present, kill switch absent, certificate valid through2027-01-09; worker-env tree and main pins matched.
delta_gate: shadow/full, reason=not-gate-pr; no narrowing.
owned_topup: none needed.
floor_topup: complete,16/16 steps exit0, including environment-absence top-ups, beadstest, Darwin compile and runner self-tests.
latency_leg: measured under a clear farm probe; TestGcBdLatencyBudget and TestGcSeatHookLatencyBudget PASS in their separate lane.

Native-floor load: threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=67.05 run_max_load=94.61 run_mean_load=81.03 run_readings=11; wait_first_load=66.17 wait_max_load=101.12 wait_mean_load=70.52 wait_readings=61 read_errors=0. This ordinary native floor proceeded after its bounded wait; all steps passed.

skip_inventory:676 events including nested floor events (the reader's618 counts top-level results). Ownership checked against the tested merge, whose net diff is exactly the reviewed deletion.318 source sites classify UNCHANGED. The remaining31 rows are30 provider-capability fencing skips plus one existing crash-scenario skip whose reason was lost by the inventory parser under parallel output interleaving; each is resolved from source and raw logs. No Go test, helper or import hunk, no added test load, no new unresolved skip. The retained per-test reasons concern platform, environment, capability or explicitly skipped existing scenarios; the deletion cannot change those conditions. Script-driven acceptance owners pass in the process tier.

## Required CI-job accounting

ci_required_closure: python3 ci_required_triggers.py /var/tmp/deploy-ga-5ftfq569.VHpCqPq4/merge c377651cb318ed567997b5c2e81d029a55a56420 f3041e47b208574432e248a4cca614d036bcc5cb fork -> exit0; workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`;6 LEAF jobs. The check aggregator receives no status.

| Leaf job | Status | Command/result/evidence |
|---|---|---|
| changes | LOCAL-PASS | Closure tool exit0; retained closure.txt contains path-filter results. |
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=runner-policy.out python3 .github/workflows/scripts/runner_policy.py from merged tree; exit0; runner-policy.log and runner-policy.out. |
| credential-provider-windows | NOT-TRIGGERED | Tool: NON-LINUX; credential_provider filter false. |
| pack-gate | NOT-TRIGGERED | Tool: packs filter false. |
| windows-lane | LOCAL-PASS | Every workflow run command, including tools/nogo working-directory, in windows-lane-corrected.sh; detached/fresh-tree/load-gate/isolation wrappers; GATE_RUN_EXIT rc=0 state=complete; stdout/stderr retained under windows-lane-corrected/units. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Runner-policy maps runner_windows to blacksmith-4vcpu-windows-2025. Windows-tagged native-package test import closure computed; changed shell asset is outside it. |

The cross-compile job ran the Windows build allowlist, compiled its tests, built and compiled the Windows analyzer tests from tools/nogo, then built every package and compiled its tests for Darwin. Compile-only output is evidence for that job, not a claim that Windows or Darwin tests executed. fresh-tree-run.sh reported the pinned merge/tree and an unchanged tracked tree; isolated-test-run.sh selected PASS-THROUGH for this rig and pinned bd1.3.4. No clean-environment claim is made.

Compile load (verbatim): LOAD_GATE_SUMMARY threshold=15 waited_seconds=991 wait_timed_out=0 run_start_load=14.07 run_max_load=19.25 run_mean_load=16.79 run_readings=3 wait_first_load=23.90 wait_max_load=47.01 wait_mean_load=30.38 wait_readings=34 read_errors=0

windows_reach: none (windows_reach.sh exit0; native windows-lane-test's own GOOS=windows GOARCH=amd64 go list -mod=readonly -test -deps closure also has no changed input).
deferred_to_pr_ci: windows-lane-test (non-linux-runner).
not_covered: none.
ci_required_accounting: all6 leaf jobs accounted above; PR-open authorization only. Merge requires CI / required green on the PR head and success for the deferred native Windows job.

The original cross-compile attempt omitted the workflow's tools/nogo working-directory and is invalid invocation evidence. Its logs are retained; the corrected complete job above supplies the account. Earlier disk-floor refusals ran no command and supply no test verdict. No source failure is attributed or waived.

## Retained evidence

Evidence root: /var/tmp/deploy-ga-5ftfq569.VHpCqPq4. CI-job root: /var/tmp/gc-heavy-gate/runs/ga-5ftfq569.ci-required. Raw logs are retained locally; large inventories are referenced rather than pasted.
- Full execution.json: `/var/tmp/deploy-ga-5ftfq569.VHpCqPq4/suite/evidence/execution.json`; SHA256 `aefbf3a6486a7325cfed93c1d2ada18e618c50b941c0a8bc647f4f8f216888af`.
- Full by-name test results: `/var/tmp/deploy-ga-5ftfq569.VHpCqPq4/resume-by-name-evidence.txt`; SHA256 `ac441aab39ec5ef130791b7975ec7890b7882b5a5e97aed7df9e4b478720af87`.
- Full acceptance owners: `/var/tmp/deploy-ga-5ftfq569.VHpCqPq4/acceptance-regression-evidence.txt`; SHA256 `25126bfc70251f8ce516acef2265cb6ed5dcbef5ec2c7b6ce64745dd2fc03004`.
- Full C6 build/nogo: `/var/tmp/deploy-ga-5ftfq569.VHpCqPq4/c6-result.txt`; SHA256 `a01894e6b3039c645113da1318076293a59d6c750f8986628e148f389da08062`.
- Full fast lanes: `/var/tmp/deploy-ga-5ftfq569.VHpCqPq4/fast-result.txt`; SHA256 `2edd9b70de0082783984d1cb0ec71194bcb1a885c836053e79a66dfb55a9b8c4`.
- Full skip inventory: `/var/tmp/deploy-ga-5ftfq569.VHpCqPq4/all-skip-inventory.json`; SHA256 `6402e06ec67cc0db9da54e68108a54d814dd7100c29451efc822ff78991537d2`.
- Full skip source proofs: `/var/tmp/deploy-ga-5ftfq569.VHpCqPq4/skip-source-evidence.txt`; SHA256 `2f806ea69b296d42c75fccfd0c9b74aa4a54e9074fe44a5f6f0cd723ddc4e6df`.
- Full skip special resolutions: `/var/tmp/deploy-ga-5ftfq569.VHpCqPq4/skip-special-resolutions.json`; SHA256 `81435c39658d3408b5b35933dfdf6818d8052f0121550a1710031d4c4e4ccc2f`.
- Full CI closure: `/var/tmp/gc-heavy-gate/runs/ga-5ftfq569.ci-required/closure.txt`; SHA256 `fb8d46afc43b560f699e94f2654003cebd6bea020a1e3272d481b68e04711729`.
- Full CI corrected stdout: `/var/tmp/gc-heavy-gate/runs/ga-5ftfq569.ci-required/windows-lane-corrected/units/gate.w1.stdout.log`; SHA256 `29972c8a74a13c32f294ec97847323db41a8efca22187fc43471d945e7e6fcd4`.
- Full CI corrected stderr: `/var/tmp/gc-heavy-gate/runs/ga-5ftfq569.ci-required/windows-lane-corrected/units/gate.w1.stderr.log`; SHA256 `a9e94db7e2521733afbddcf7a2058828782a54efcd30d41d84c525f5f7092dbe`.
- Full native Windows reach: `/var/tmp/gc-heavy-gate/runs/ga-5ftfq569.ci-required/windows-native-reach.txt`; SHA256 `3872fd0b67603659a4f49ab7dff2158826416aa39685d3d067f01fae3bb73371`.
