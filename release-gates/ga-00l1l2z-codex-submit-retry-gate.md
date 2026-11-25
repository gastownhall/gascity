# Release gate: Codex nudge submit retries

**Verdict:** **PASS**

Bead ga-00l1l2z; build ga-oqhooe.1; review ga-blcgn79.

- DEPLOY_MODE: remote; PUSH_REMOTE: fork; base_branch: main.
- reviewed_commit: 8b34af05a5897f278fa36d3db66031eea3aad0d5.
- gate_base_kind: current-tip; gate_base: 6bd609f5bfb41f43f444843b1220d0cde4473446.
- merge_state: clean (origin/main@6bd609f5bfb41f43f444843b1220d0cde4473446).
- merge_head: d710aad8f649c64ad4f9a9d8680e8239c459b9b4; merge_tree: 8cdd9e8fc38a1c2a59df819690de74323b11f0ad.
- merge_validation: MPR and repository merge checks validate the final merge against its then-current base. The deployer performs no rebase or merge.
- Reviewer test linkage independently verified: ab45f412cfe3f02c30c466ff45493c7e95ffd8b6 -> 8b34af05a5897f278fa36d3db66031eea3aad0d5, internal/runtime/tmux/codex_submit_turn_test.go.
- docs/PROJECT_MANIFEST.md is absent; CI scope is derived from the gated Makefile, TESTING.md and workflows.

| # | Criterion | Evidence |
|---|---|---|
| 1 | PASS — Review | Review verdict PASS; reviewed SHA resolved as a commit. reviewer-added-tests.sh independently accepts the exact parent/child and test-only file linkage. |
| 2 | PASS — Acceptance | Retry derivation preserves empty/single-key sequences and returns only the final key for multi-key retries. First actual submit retains its full sequence; confirmation counts actual sends. Session, staged-draft and pane retry paths use the reduction. All 26 changed roots pass by package/name, including the reviewer-added pane subtest. Both unchanged first-submit Codex tests and the manifest inventory test pass. |
| 3 | PASS — Tests and CI accounting | All five forced fresh remote phases and all 16 native floor steps pass. No changed test failed, skipped or went missing. Source/import-closure and same-reason pinned-base proofs account for all skips; CI leaves accounted for below. |
| 4 | PASS — Findings | No unresolved HIGH findings in the source review. |
| 5 | PASS — Clean | Frozen merge scratch is clean; exact source, base, merge parents and tree verified. Publication checks the isolated deploy for a sole gate-record addition and clean status after committing it. |
| 6 | PASS — Merge/build/vet | Clean merge against the recorded base. bazel build //... and whole-tree make vet/nogo have independently verified SUCCESS terminal BEPs. |
| 7 | PASS — Theme | One tmux submit-retry behavior, its tests and corresponding test-manifest entries. No independent feature or internal prompt contamination. |

## Test evidence

- test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/merge --base 6bd609f5bfb41f43f444843b1220d0cde4473446 --bead-json /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/bead.json --owned /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/owned.tsv --run-dir /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/c3-run/evidence; exit 0.
- test_cmd_scope: full-suite.
- test_exec_mode: remote-exec; fallback_reason: none; waiver_ref: none; failure_attribution: none.
- test_counts: PASS=150721 FAIL=0 SKIP=515 (top-level result observations; repeated jobs count separately).
- test_exec_bep: all five predicates below; cached_locally=0, cached_remotely=0, retries=0, no local fallback. Raw predicates independently regenerated from phase windows.
- admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 78.76907348632812, "waited_seconds": 0.001}.
- rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}.
- farm_test_seconds: 11693.02.
- heavy_mode: none (computed by heavy-composite-gate.sh).
- test_log_dir: /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/c3-run/evidence/remote/logs; all 1068 retained raw CAS log/XML hashes and 534 normalized log copies independently verified.
- owned_topup: none; all 26 changed roots ran in their owning unit package, also in integration-packages. Package binding prevents a same-named test elsewhere supplying evidence.
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

- test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec

diff_tests_executed:

- internal/runtime/tmux / TestCodexSubmitResendsWhileTheDraftWaits PASS, no failed/skipped descendant
- internal/runtime/tmux / TestNudgeSubmitRetryKeySequenceKeepsOnlyTheCommitKey PASS, no failed/skipped descendant
- internal/runtime/tmux / TestNudgeSubmitRetryKeySequenceNeverInterrupts PASS, no failed/skipped descendant
- internal/runtime/tmux / TestRecoverStagedDraftBusyAtEntrySendsNothing PASS, no failed/skipped descendant
- internal/runtime/tmux / TestRecoverStagedDraftSendsOnlyRetries PASS, no failed/skipped descendant
- internal/runtime/tmux / TestRecoverStagedDraftStopsOnProbeError PASS, no failed/skipped descendant
- internal/runtime/tmux / TestRecoverStagedDraftStopsOnSendError PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitAndConfirmCountsOnlySubmitsThatReachedThePane PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitAndConfirmHoldsTheFirstInterruptingSubmitWhileBusy PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitAndConfirmRecoversAFusedFirstSubmitWithTheCommitKey PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitAndConfirmSendsTheFullSequenceOnlyOnTheFirstSubmit PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitAndConfirmWithholdsAnInterruptingResend PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitEnterAndConfirmBestEffortWhenNeverBusy PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitEnterAndConfirmClearsStaleSendError PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitEnterAndConfirmFailedReSendIsUnconfirmed PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitEnterAndConfirmFailedReadIsNotIdle PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitEnterAndConfirmIgnoresPreexistingBusyOnFirstSend PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitEnterAndConfirmKeepsWatchingAfterSkippedReSend PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitEnterAndConfirmNeverCreditsPreexistingBusy PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitEnterAndConfirmNoDoubleSubmitOnFastTurn PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitEnterAndConfirmPreCheckConfirmsShortTurn PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitEnterAndConfirmReEntersWhileIdle PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitEnterAndConfirmReturnsSendError PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitEnterAndConfirmStopsAtPendingPrompt PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitEnterAndConfirmStopsWhenALookCannotReadThePane PASS, no failed/skipped descendant
- internal/runtime/tmux / TestSubmitEnterAndConfirmStopsWhenBusy PASS, no failed/skipped descendant

## Skip/source audit

- skip_inventory: 501 remote entries and 39 native entries; zero owned skips. Every reported site is unchanged; individual reasons and site checks are retained.
- skip_justification: 329 remote and 7 native events whose test closure reaches changed helper packages reproduce at the pinned base with the same reason. Other closures exclude the changed helper packages. All site checks require line_in_hunk=no and UNCHANGED.
- Shared hunks are the reviewer-added pane executor/helper and comments, attempt-aware fake send methods and a comment, and the slices import for pure derivation tests. All associated changed tests pass. No platform, opt-in, missing-fixture or daemon skip predicate changed.
- The sole non-Go skip is configured non-applicability in the bridge lockfile check; source/config blobs equal the base, and the enabled dashboard counterpart runs all three checks.

## Required CI accounting

ci_required_closure: ci_required_triggers.py /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/merge 6bd609f5bfb41f43f444843b1220d0cde4473446 8b34af05a5897f278fa36d3db66031eea3aad0d5 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs. Output: /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/ci-required/closure.stdout.

ci_required_accounting:

- changes | LOCAL-PASS | ci_required_triggers.py (exact command below), exit 0; /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/ci-required/closure.stdout
- runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<retained file> python3 .github/workflows/scripts/runner_policy.py, exit 0; /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/ci-required/runner-policy.stdout
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/ci-required/windows-native-reach.json
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- windows-lane | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/merge -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- python3 /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/run-ci-job.py windows-lane; exit 0; stdout /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/ci-required/windows-lane.stdout; stderr /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/ci-required/windows-lane.stderr
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'

- windows_reach: credential-provider-windows=none; windows-lane-test=none; each actual job closure computed separately.
- deferred_to_pr_ci: windows-lane-test (non-linux-runner).
- not_covered: none; host_gaps: none.
- Windows cross-compile steps ran exactly as written by the workflow, with their own environments, one job at a time in an independent fresh view. Commands/results: ci-required/windows-lane-steps.json.
- This PASS authorizes PR-open. Merge requires CI / required and each deferred job successful on the PR head (or skipped only by its own path filter). MPR owns that verification and the merge.

## Host load

Remote admission has no local-heavy/load wait. The native floor, local CI jobs and native base observations retain the soft threshold-15 wait. Each LOAD_GATE_SUMMARY records load_threshold, load_waited_seconds, load_wait_timed_out, run_start_load, run_max_load, run_mean_load and run_readings; timed-out waits are not themselves test results.

- c3: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=23.18 run_max_load=22.92 run_mean_load=16.42 run_readings=49 wait_first_load=23.18 wait_max_load=23.18 wait_mean_load=23.18 wait_readings=1 read_errors=0
- local-audit: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=86.95 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=23.67 wait_max_load=97.10 wait_mean_load=48.81 wait_readings=61 read_errors=0
- floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=10.82 run_max_load=14.33 run_mean_load=13.51 run_readings=7 wait_first_load=10.82 wait_max_load=10.82 wait_mean_load=10.82 wait_readings=1 read_errors=0
- windows-lane.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.08 run_max_load=29.15 run_mean_load=21.41 run_readings=4 wait_first_load=12.08 wait_max_load=12.08 wait_mean_load=12.08 wait_readings=1 read_errors=0
- native.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=270 wait_timed_out=0 run_start_load=14.45 run_max_load=21.01 run_mean_load=18.56 run_readings=3 wait_first_load=27.06 wait_max_load=27.06 wait_mean_load=20.50 wait_readings=10 read_errors=0
- owned-skip-integration-packages.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=31.06 run_max_load=29.89 run_mean_load=28.66 run_readings=2 wait_first_load=31.06 wait_max_load=31.06 wait_mean_load=31.06 wait_readings=1 read_errors=0
- owned-skip-unit.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=30.12 run_max_load=33.09 run_mean_load=32.46 run_readings=3 wait_first_load=30.12 wait_max_load=30.12 wait_mean_load=30.12 wait_readings=1 read_errors=0

## Retained evidence

- /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/context.json; SHA256 1d6a8799fe915aecd3054616c0e33b22a7ba62b15f7569a7ea002d5a5164e45f.
- /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/c6-verified.json; SHA256 efa980c66d8ad17b95ec62c558cfe2668bc89f75e10ac60086e7b1a08cef5c0e.
- /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/fast-verified.json; SHA256 044feb97ecad8aa798d49317c8f1bb56224604ce0c30f0d808a5f5333d2f1273.
- /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/acceptance-precheck.json; SHA256 7dbb502179cb3a02ec21a0bc47d400136f9fafc7cb57b44bbc7b84a68bcc936d.
- /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/c3-run/evidence/execution.json; SHA256 519e894685966fe80af295138b1b844f0c72b19c7a69b95018fb10ef06e009ba.
- /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/resume-c3-audit.json; SHA256 6c9f956aeb1a743cfbf494ffdb07e96a3f4a6a425caf3987984d864d933094b3.
- /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/owned-package-verified.json; SHA256 b8a53292ceb90c9119c2a50498b34e58c2479da6918efcdae01a88234ce82759.
- /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/acceptance-context-verified.json; SHA256 4eab1eb58fc77b6a06dc2e7563d720fc9797cb4d8da9c3eb29f0ea23158cbfbd.
- /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/skip-proof-automated.json; SHA256 ec9de892c012723d5e7dc5aa727762362d69c74ebb677da49031676eccc71c0e.
- /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/manual-skip-review.json; SHA256 aa8cc08540eedc98bdce01609c3971ab4ce60ca78152235b939f1edf44ce01bd.
- /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/supplemental-floor/verified.json; SHA256 1c1698493ed8aff56ba6ed3700dadc95f754632cbc9af6e438a7a10ee110f689.
- /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/non-go-skip-reviewed.json; SHA256 ae91f29012da2602b7b632be0c9352312578b3e37b3f1a869d3f14ed4bb130f1.
- /var/tmp/gc-gate-ga-00l1l2z-fresh.3leghoag/ci-required/accounting-verified.json; SHA256 d0d8ec3c6eb39e20f6b57525dba4e043fcdbecf2ba4e51e3591d473106f41282.
