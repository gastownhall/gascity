# Release gate: Deferred assigned-work diagnostics

**Verdict:** **PASS**

Bead ga-f4wpwo0; build ga-0o3uby; review ga-56fw2cq.

- DEPLOY_MODE: remote; PUSH_REMOTE: fork; base_branch: main.
- reviewed_commit: cac39787d391aefa5b4f49af719aa00798f42770.
- gate_base_kind: current-tip; gate_base: 6bd609f5bfb41f43f444843b1220d0cde4473446.
- merge_state: clean (origin/main@6bd609f5bfb41f43f444843b1220d0cde4473446).
- merge_head: 3cf75c0ba61c09a6c83e368e03e2c704de826c3b; merge_tree: 1ffcbb4a8750258648b8120b487cda627f569e1a.
- merge_validation: MPR and repository merge checks validate the final merge against its then-current base. The deployer performs no rebase or merge.
- No carryover or reviewer-added test substitution: the exact reviewed source is gated.
- docs/PROJECT_MANIFEST.md is absent; CI scope is derived from the gated Makefile, TESTING.md and workflows.

| # | Criterion | Evidence |
|---|---|---|
| 1 | PASS — Review | Review ga-56fw2cq explicitly records PASS for this source; the reviewed SHA resolves as a commit. |
| 2 | PASS — Acceptance | The opt-in assigned-work dump prints deferred=true/false from IndefinitelyDeferred. Both changed tests PASS by package/name: the unit checks both markers, and the desired-state regression proves that deferred work creates no demand while a normal control does. Existing debug-gating and pool-log tests also PASS. Status mapping, demand and reaper implementations are untouched. |
| 3 | PASS — Tests and CI accounting | All five forced fresh remote phases and all 16 native floor steps pass. No changed test failed, skipped or went missing. Unchanged source-site and shared-helper analysis accounts for all skips; CI leaves accounted for below. |
| 4 | PASS — Findings | No unresolved HIGH findings in the source review. |
| 5 | PASS — Clean | Frozen merge scratch is clean; exact source, base, merge parents and tree verified. Publication checks the isolated deploy for a sole gate-record addition and clean status after committing it. |
| 6 | PASS — Merge/build/vet | Clean merge against the recorded base. bazel build //... and whole-tree make vet/nogo have independently verified SUCCESS terminal BEPs. |
| 7 | PASS — Theme | One assigned-work diagnostic change, its two tests and Bazel source-list entry. No independent feature or internal prompt contamination. |

## Test evidence

- test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/merge --base 6bd609f5bfb41f43f444843b1220d0cde4473446 --bead-json /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/bead.json --owned /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/owned.tsv --run-dir /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/c3-run/evidence; exit 0.
- test_cmd_scope: full-suite.
- test_exec_mode: remote-exec; fallback_reason: none; waiver_ref: none; failure_attribution: none.
- test_counts: PASS=150702 FAIL=0 SKIP=515 (top-level result observations; repeated jobs count separately).
- test_exec_bep: all five predicates below; cached_locally=0, cached_remotely=0, retries=0, no local fallback. Raw predicates independently regenerated from phase windows.
- admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 78.38078689575195, "waited_seconds": 0.001}.
- rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}.
- farm_test_seconds: 13332.351.
- heavy_mode: none (computed by heavy-composite-gate.sh).
- test_log_dir: /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/c3-run/evidence/remote/logs; all 1068 retained raw CAS log/XML hashes and 534 normalized log copies independently verified.
- owned_topup: none; both changed roots ran in their owning unit package, also in integration-packages. Package binding prevents a same-named test elsewhere supplying evidence.
- floor_topup: complete, 16/16 exit 0; selected environment-absence packages, beadstest, Darwin compile, push-lock and concurrency self-tests. One supplemental native observation covers the actual upstream-storage-unavailable skip; canonical evidence is unchanged.
- latency_leg: measured on first attempt under clear farm probe; both latency-budget tests PASS. integration-full excludes those names; integration-latency runs only them.
- runtime: GATE_ICU74 verdict=OK and disk preflight PASS. The wrapper reports PASS-THROUGH for this rig; the frozen scratch and Bazel environment supply remote isolation, and native commands scrub live-store selectors and use pinned bd.
- policy_lane: whole-tree C6 make vet/nogo; run-pinned-lint.sh -- make fmt-check-changed; check-release-dist-ignore, check-routed-test-rows, check-split-topology-rows, check-residency-boundary, check-hooks; 13 policy Bazel targets, all exit 0 before the suite. Exact commands/flags: fast-lane.sh and fast-output.stdout/stderr.
- drift_lane: make bazel-sync, git diff --exit-code, empty status including untracked files; seven generated-artifact targets; make openapi-breaking-check with the pinned base. All exit 0 before the suite, each in an independent fresh view.
- ci_lane_run: n/a (no CI-config change).

| Phase | Test actions | Predicate |
|---|---:|---|
| unit | 408 | PASS, fresh remote attempt 1 inside phase window |
| acceptance | 25 | PASS, fresh remote attempt 1 inside phase window |
| integration-packages | 86 | PASS, fresh remote attempt 1 inside phase window |
| integration-full | 14 | PASS, fresh remote attempt 1 inside phase window |
| integration-latency | 1 | PASS, fresh remote attempt 1 inside phase window |

The unit phase runs make -k check (whole-suite Bazel tests plus shell guards); acceptance runs acceptance_test and acceptance_solo_tests; integration-packages runs //test:integration_packages; full integration runs //test/integration:integration_test minus the dedicated latency names; the serialized latency phase runs those names under the farm-load probe. Expanded flags and BEP paths:

- test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec

diff_tests_executed:

- cmd/gc / TestAssignedWorkDumpMarksDeferredBeads PASS, no failed/skipped descendant
- cmd/gc / TestNamedSessionDemand_DeferredAssignedBead_NoDemandAndDumpMarksIt PASS, no failed/skipped descendant

## Skip/source audit

- skip_inventory: 501 remote entries and 39 native entries; zero owned skips. Every reported site is unchanged; individual reasons and site checks are retained.
- skip_justification: no SHARED-HUNKS or HELPER-HUNKS package exists in this diff, so no conditional base comparison is required (0 remote / 0 native comparisons). All 327 source sites are unchanged and file-blob-identical; all site checks require line_in_hunk=no and UNCHANGED.
- The two test additions change no shared helper or skip condition. Production only appends an existing boolean to opt-in debug output. Existing platform restrictions, lane opt-ins, missing fixture/tool/service cases and deliberately unsupported conformance rows do not bear on this diagnostic change.
- The diagnostic reports IndefinitelyDeferred as specified; a future defer_until on an otherwise open row remains outside this change's scope.
- The sole non-Go skip is configured non-applicability in the bridge lockfile check; source/config blobs equal the base, and the enabled dashboard counterpart runs all three checks.

## Required CI accounting

ci_required_closure: ci_required_triggers.py /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/merge 6bd609f5bfb41f43f444843b1220d0cde4473446 cac39787d391aefa5b4f49af719aa00798f42770 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs. Output: /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/ci-required/closure.stdout.

ci_required_accounting:

- changes | LOCAL-PASS | ci_required_triggers.py (exact command below), exit 0; /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/ci-required/closure.stdout
- runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<retained file> python3 .github/workflows/scripts/runner_policy.py, exit 0; /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/ci-required/runner-policy.stdout
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/ci-required/windows-native-reach.json
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- windows-lane | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/merge -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- python3 /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/run-ci-job.py windows-lane; exit 0; stdout /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/ci-required/windows-lane.stdout; stderr /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/ci-required/windows-lane.stderr
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'

- windows_reach: credential-provider-windows=none; windows-lane-test=none; each actual job closure computed separately.
- deferred_to_pr_ci: windows-lane-test (non-linux-runner).
- not_covered: none; host_gaps: none.
- Windows cross-compile steps ran exactly as written by the workflow, with their own environments, one job at a time in an independent fresh view. Commands/results: ci-required/windows-lane-steps.json.
- This PASS authorizes PR-open. Merge requires CI / required and each deferred job successful on the PR head (or skipped only by its own path filter). MPR owns that verification and the merge.

## Host load

Remote admission has no local-heavy/load wait. The native floor, local CI jobs and native base observations retain the soft threshold-15 wait. Each LOAD_GATE_SUMMARY records load_threshold, load_waited_seconds, load_wait_timed_out, run_start_load, run_max_load, run_mean_load and run_readings; timed-out waits are not themselves test results.

- c3: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=30.82 run_max_load=32.94 run_mean_load=17.27 run_readings=56 wait_first_load=30.82 wait_max_load=30.82 wait_mean_load=30.82 wait_readings=1 read_errors=0
- local-audit: LOAD_GATE_SUMMARY threshold=15 waited_seconds=90 wait_timed_out=0 run_start_load=14.97 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=18.35 wait_max_load=18.35 wait_mean_load=16.63 wait_readings=4 read_errors=0
- floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=150 wait_timed_out=0 run_start_load=14.93 run_max_load=16.11 run_mean_load=14.32 run_readings=6 wait_first_load=17.47 wait_max_load=17.99 wait_mean_load=16.91 wait_readings=6 read_errors=0
- windows-lane.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.88 run_max_load=17.00 run_mean_load=15.42 run_readings=3 wait_first_load=12.88 wait_max_load=12.88 wait_mean_load=12.88 wait_readings=1 read_errors=0

## Retained evidence

- /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/context.json; SHA256 43fccc9bbdd0044cf6b9885f431764d488ff5045759e7550cad91f149835fe11.
- /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/resume-prerequisites-verified.json; SHA256 fcf6ed7c1b04d6e9bfa07cc6856a6cc3a644f38824fd29a3cd963bd130adc97f.
- /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/fast-verified.json; SHA256 7ec71bc4ed6733fc0e31f7cb0d0ed0a6fa5a2ca74bafad656e43e88d0663db5b.
- /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/acceptance-precheck.json; SHA256 572622bbf8a58835ff83b8218fbc375645cd5d685051d1dbf32331679ed199a5.
- /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/c3-run/evidence/execution.json; SHA256 123bbb54c104a73c0adc51a93bdfa5a42820eef8b259332ac0e71bebcbb12c4c.
- /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/resume-c3-audit.json; SHA256 ce4512bb644b274c8f9a4ecb720fbe4c2cbc0260c472bd152e12bb2f5c6dfff7.
- /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/owned-package-verified.json; SHA256 fe20fa1df2ce2d2c1c3084cf8d2d7e397f48cbd25d7ac9f34bc5b4f11921c84b.
- /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/acceptance-context-verified.json; SHA256 a51d124bdc241366a60359f56387d97fc03ee3bb107eaea2535b165db2a735d4.
- /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/skip-proof-automated.json; SHA256 b1e66b2a7b9bacd635d27514c8513c89fd36857cac6961746b81d5ac584b5f1c.
- /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/manual-skip-review.json; SHA256 5b1e4677beb081eca77d56ac62d45c96711f4d49e60f7ac12555516e79eeb363.
- /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/supplemental-floor/verified.json; SHA256 71dff483be46957b53dbb793e30878f8fe52fa46cc50e455f1078872a6e0042a.
- /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/non-go-skip-reviewed.json; SHA256 3518960ba939ad352dd27567f6fc90df1ab283fd39e7b2274d5b93b4b3047a79.
- /var/tmp/gc-gate-ga-f4wpwo0-fresh.63369xa6/ci-required/accounting-verified.json; SHA256 408be880d9e531621d604dc69bfee6e81c511a4643aec9a158eb277e8e99874e.
