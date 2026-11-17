# Release gate: Go 1.27.4

**Verdict:** **PASS**

PASS authorizes PR opening. The actual PR-head Go module vulnerabilities check and every required check must be green before clearance and the merge-request. This record does not claim an unobserved CI result. Merge authority remains MPR.

- Bead: ga-alps1j3l; build ga-6ck4wbzz; review ga-463en5q4.
- reviewed_source: 67dd9e5a4d7f14360511a5e135b22f17a233cb89; original reviewed source: 80c306b016d5a657e61738d1f06da74c56c847d9.
- gate_base_kind: current-tip; gate_base: 6bd609f5bfb41f43f444843b1220d0cde4473446.
- merge_state: clean (origin/main@6bd609f5bfb41f43f444843b1220d0cde4473446).
- merge_head: 1fbad80d13ef2a85039893e98f9cc75b0f3d90b8; source/merge tree: 29de53da0ad45313536544056291902e8ec8912f.
- merge_validation: merge authority validates the final merge against its then-current base.
- DEPLOY_MODE: remote; PUSH_REMOTE: fork; isolated branch deploy/ga-alps1j3l-gate.

| # | Criterion | Verdict and evidence |
|---|---|---|
| 1 | Review PASS | PASS: original reviewer PASS plus independently verified exact whole-diff carryover; intermediate/final trees, base and authorship agree. Stable patch-id 53f1176dbb93deaf2eaf00f365422b9fb8cab0aa. Unchanged ancestry guard PASS using only this bead/build/review IDs. |
| 2 | Acceptance | PASS for PR-open: Go floor, SDK/lockfile, analyzer/container/worker pins and docs agree on 1.27.4; four owned tests PASS. Worker-env digest reproduces. Actual head security/required CI remains mandatory before merge. Repo-side pinning does not perform the upstream worker-host rollout. |
| 3 | Tests and required lanes | PASS for PR-open: exact-tree local suite 306/306 targets, 408 actions; suite PASS=89091 FAIL=0 SKIP=384; all four owned tests PASS by name. Fixed/environment native floor complete; union PASS=89107 FAIL=0 SKIP=395. Every remaining skip has source/site/reach proof. Leaf-job accounting below. |
| 4 | High review findings | PASS: no unresolved HIGH findings in the review/carryover of record. |
| 5 | Clean branch | PASS: clean frozen merge scratch; publication requires only the committed gate record beyond reviewed_source and a clean isolated deploy branch. |
| 6 | Merge/build/vet | PASS: clean frozen-base merge, identical source tree; bazel build //... and make vet (= whole-tree lint/nogo) pass; raw BEPs have successful terminals and no failed action/target. |
| 7 | One feature theme | PASS: one security/toolchain update across its 19 source files; no independent feature or internal prompt/doc contamination. |

## Test and policy evidence

- test_cmd: gascity-bazel-legs.sh test //... --config=fork-cache --keep_going --test_env=GO_TEST_WRAP_TESTV=1 (with BEP output paths).
- test_cmd_scope: full-suite per explicit per-bead mayor rulings 1/2 (gcg-6668289): ONE local //... suite substitutes for the RBE phases. No claim those remote phases ran.
- test_exec_mode: local; BEP strategies: 407 linux-sandbox, 1 processwrapper-sandbox; cached_locally=0; cached_remotely=0; farm_test_seconds=0.
- test_exec_bep: /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/local-evidence/unit.bep.json and unit.bep.leg2.json; hashes/verdict predicates in local-suite-verified.json.
- rbe_state: ineligible; fallback_reason: pin-mismatch. Reviewed source intentionally moves the pin; no hybrid tree, pin revert or stitched farm/local suite.
- load_threshold: 15; load_waited_seconds: 0; load_wait_timed_out: 0; run_start_load: 6.52; run_max_load: 48.60; run_mean_load: 39.05; run_readings: 15 (full suite).
- admission: heavy-run-lock host.lock, shared local-heavy slot 0; host cap 3; 71.89 GiB free at suite start; no cap/threshold override.
- isolation_wrapper: invoked, reports PASS-THROUGH for this rig. Suite uses frozen merge scratch plus Bazel's pinned environment; independent fast/native/CI lanes use fresh views. Native commands scrub store selectors and use pinned bd v1.3.4.
- runtime: GATE_ICU74 verdict=OK provenance=recorded; no direct testcontainers tests in this tree. Reaper retention 876000h checked in live city config before build and during the native-floor wait.
- temporary instrumentation: GO_TEST_WRAP_TESTV=1 also set only in the scratch's imported, untracked .bazelrc.local for per-test evidence and normal warm pre-push, per ruling 2; restore after publication. No committed rc/source/pin changed for testing. The gate record changes root_extras inputs; normal pre-push may rerun affected tests and must not be bypassed.
- diff_tests_executed: TestGoModGoDirectiveIsAtPatchedFloor PASS; TestGoToolchainPinsAgreeWithGoMod PASS; TestTrivyIgnoreDropsStdlibWaiversForRebuiltTools PASS; TestNolintNeverSuppressesStoreclass PASS.
- waiver_ref: none (no gascity waiver path); attribution: none; heavy_mode: none, computed.
- test_log_dir: /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/local-evidence/logs; retained raw log/XML hashes bind to exact BEP action identities.
- skip_inventory: 420 suite/native skip events, 383 distinct names, 0 owned; /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/skip-audit/inventory.json records each reason/site/reach. Site proofs require line_in_hunk=no. Both changed scripts shared hunks are test-only and absent from these binaries' go list -test -deps integration closure; scripts has no skips.
- owned_topup: none required. floor_topup: complete, /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/local-evidence/floor-topup.json: beadstest, Darwin compile, runner self-tests and every selected environment-absence parent. Non-owned floor skips remain inventoried.
- latency_leg: not included in the explicitly substituted //... suite; no latency-lane PASS claimed. Whole-target timing results are context only outside their dedicated clear-probe lane.
- policy_lane: whole-tree make vet (= lint/nogo), pinned fmt-check-changed, check-release-dist-ignore, check-routed-test-rows, check-split-topology-rows, check-residency-boundary, check-hooks and 13 policy Bazel targets PASS, before the suite.
- drift_lane: make bazel-sync with empty diff/status; seven generated artifact targets and OpenAPI breaking check PASS, separate fresh views before the suite.
- ci_config_diff: none. docs/PROJECT_MANIFEST.md is absent; criteria/commands come from the current pack protocol and actual repo Makefile/Bazel workflows.

## Required CI accounting

ci_required_closure: ci_required_triggers.py /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/merge 6bd609f5bfb41f43f444843b1220d0cde4473446 67dd9e5a4d7f14360511a5e135b22f17a233cb89 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs. Exact output: /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/ci-required/closure.stdout.

ci_required_accounting:

- changes | LOCAL-PASS | /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/ci-required/closure.stdout; closure command above, exit 0
- runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<retained temp> python3 .github/workflows/scripts/runner_policy.py; exit 0; /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/ci-required/runner-policy.stdout
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/ci-required/windows-native-reach.json
- credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/ci-required/credential-windows-reach.stdout
- windows-lane | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/merge -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- python3 /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/run-ci-job.py windows-lane; exit 0; stdout /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/ci-required/windows-lane.stdout; stderr /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/ci-required/windows-lane.stderr; exact workflow run-step bodies/env/exit/logs in /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/ci-required/windows-lane-steps.json
- pack-gate | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/merge -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- python3 /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/run-ci-job.py pack-gate; exit 0; stdout /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/ci-required/pack-gate.stdout; stderr /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/ci-required/pack-gate.stderr; exact workflow run-step bodies/env/exit/logs in /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/ci-required/pack-gate-steps.json

- windows_reach: credential-provider-windows=go.mod; windows-lane-test=go.mod (each computed from its own actual job/import closure). GOOS=windows GOARCH=amd64 go vet ./internal/credentialprovider and the same with -tags=integration both exit 0; exact native-test closure matches the default vet and reuses it. Results/logs: /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/ci-required/windows-credential-vet-results.json. This is compile/vet evidence, not native Windows execution.
- deferred_to_pr_ci: windows-lane-test (non-linux-runner), credential-provider-windows (non-linux-runner).
- not_covered: none.
- Serial commands, exact step environments and cross-vet results: /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp/ci-required/accounting.json and named logs. Each deferred job must succeed on the actual PR head before merge.

## Host load

- ci pack-gate: LOAD_GATE_SUMMARY threshold=15 waited_seconds=60 wait_timed_out=0 run_start_load=14.72 run_max_load=15.27 run_mean_load=15.06 run_readings=4 wait_first_load=15.94 wait_max_load=15.94 wait_mean_load=15.40 wait_readings=3 read_errors=0
- ci windows-credential-vet: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.37 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=14.37 wait_max_load=14.37 wait_mean_load=14.37 wait_readings=1 read_errors=0
- ci windows-lane: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.43 run_max_load=15.86 run_mean_load=14.58 run_readings=4 wait_first_load=13.43 wait_max_load=13.43 wait_mean_load=13.43 wait_readings=1 read_errors=0
- c6: LOAD_GATE_SUMMARY threshold=15 waited_seconds=270 wait_timed_out=0 run_start_load=14.94 run_max_load=59.12 run_mean_load=44.76 run_readings=19 wait_first_load=24.58 wait_max_load=24.58 wait_mean_load=19.19 wait_readings=10 read_errors=0
- fast: LOAD_GATE_SUMMARY threshold=15 waited_seconds=871 wait_timed_out=0 run_start_load=14.76 run_max_load=29.82 run_mean_load=24.41 run_readings=5 wait_first_load=49.90 wait_max_load=49.90 wait_mean_load=30.81 wait_readings=30 read_errors=0
- fast: LOAD_GATE_SUMMARY threshold=15 waited_seconds=360 wait_timed_out=0 run_start_load=14.66 run_max_load=22.33 run_mean_load=20.08 run_readings=4 wait_first_load=29.20 wait_max_load=29.20 wait_mean_load=20.88 wait_readings=13 read_errors=0
- fast: LOAD_GATE_SUMMARY threshold=15 waited_seconds=210 wait_timed_out=0 run_start_load=14.90 run_max_load=14.29 run_mean_load=14.29 run_readings=1 wait_first_load=21.21 wait_max_load=21.21 wait_mean_load=17.39 wait_readings=8 read_errors=0
- c3: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=6.52 run_max_load=48.60 run_mean_load=39.05 run_readings=15 wait_first_load=6.52 wait_max_load=6.52 wait_mean_load=6.52 wait_readings=1 read_errors=0
- local-audit: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1320 wait_timed_out=0 run_start_load=14.66 run_max_load=15.99 run_mean_load=15.59 run_readings=4 wait_first_load=40.25 wait_max_load=65.70 wait_mean_load=42.13 wait_readings=45 read_errors=0

## Retained evidence

Full source/carryover, raw BEPs, action log/XML manifests, native floor, skip-site/reach and CI evidence: /var/tmp/deploy-ga-alps1j3l-carryover.WsUU7TLp. Evidence index SHA256 54f11af0a43db8595f0b85984ccf19d368198087774a25c5bac01d65375f74b7 (evidence-index.json). Large inventories/logs are retained there, not pasted here. Suite start/end mail gcg-6708225/gcg-6711297 was recipient/subject/body peek-verified. Two preliminary launch compositions stopped before any test ran; setup outputs retained. Exactly one manual full local suite ran.
