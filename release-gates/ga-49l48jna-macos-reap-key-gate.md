# macOS workspace-service reap-key release gate

**Bead:** ga-49l48jna; build ga-b50d6ctf; review ga-ae19oqn1.
**Verdict:** **PASS**
**Linux gate:** **PASS**
**Recorded:** 2026-11-22T01:35:24.141933+00:00

Sequencing ruling gcg-11064321: Linux PASS preceded draft publication; the required actual Mac unit acceptance is now verified on draft head badd3f7a62795b3023a05049f9532c62aa29b1f8. Supplemental failures have explicit standard3a attribution below. Every moved PR head still requires a fresh exact-head gate before clearance.

Source: aebad195700b796afcdfe5bb76a4278fd379f944; original reviewed source: 20ac9e490fd587ce74d8f0ca1e209df341873de1.
review_carryover_verified: 20ac9e490fd587ce74d8f0ca1e209df341873de1 -> aebad195700b796afcdfe5bb76a4278fd379f944 mechanical-conflict files=internal/workspacesvc/BUILD.bazel; helper result mechanical-conflict patch-id c03f52c0b76a3dd2faf33fc1ad7c462aac70e3c8 files=internal/workspacesvc/BUILD.bazel. Production Go files byte-identical to the original reviewed fix.
Deploy mode remote; base ref origin/main; pinned gate_base dc87c978a3fde226b22ec20447a9de4a3e9718f8.
merge_state: clean (origin/main@dc87c978a3fde226b22ec20447a9de4a3e9718f8)
gate_base_kind: current-tip
merge_validation: clean two-parent merge 34a6a899eb77959f2ac384c86c5e04bd20d527b4, tree 0913d5e6c68d35ee4c6f20edd151b088309b7fc5; whole-tree build and nogo/vet passed. Merge authority settles subsequent base changes under docs/merge-time-guarantee.md; no self-rebase performed.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Review ga-ae19oqn1 passed; independently verified mechanical carryover and allowed generated BUILD context. |
| 2 | Acceptance criteria | PASS | Actual Mac / make test succeeded at https://github.com/gastownhall/gascity/actions/runs/44968647105/job/137043466613; exact checkout verified, named regression RUN with no SKIP/FAIL. Platform file partition and generated source entry verified. |
| 3 | Tests | PASS | Complete fresh Linux suite and required-CI accounting below; actual required Mac unit job succeeded. Supplemental core FAIL is attributed under clauses1-4, with original failure and partial-core coverage retained below. No waiver or retry. |
| 4 | No open HIGH review findings | PASS | Security findings none at any severity; style findings none blocking in original review. |
| 5 | Clean final branch | PASS | Reviewed source and canonical validation tree have no uncommitted source changes; this administrative gate record is the sole deploy addition. Final isolated-branch cleanliness is verified after its record commit. |
| 6 | Merge state, build and vet | PASS | Clean merge, exact parents/tree/base and terminal build/vet BEPs independently audited. |
| 7 | Single feature theme | PASS | Three paths, only internal/workspacesvc: Darwin reap-key case fold, complementary build tag and generated BUILD entry. |

Acceptance: Darwin arm64/amd64 select only reap_key_darwin.go; Linux/FreeBSD select only reap_key_other.go; Windows selects only reap_key_windows.go. On case-sensitive Mac volumes, two case-only roots may wait together; signalling still targets previously recorded processes only. Unicode normalization is outside this case-fold fix's scope.

## Linux test evidence

test_cmd_scope: full-suite
test_exec_mode: remote-exec; fallback_reason: none; criterion3_valid: true.
test_counts: PASS=158598 FAIL=0 SKIP=618 (top-level result lines across retained job logs; repeated jobs are counted separately).
diff_tests_executed: none (no diff-owned tests), source and canonical-merge ownership sets equal.

test_cmd:
- test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-49l48jna-0dzji_k8/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-49l48jna-0dzji_k8/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-49l48jna-0dzji_k8/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-49l48jna-0dzji_k8/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-49l48jna-0dzji_k8/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec

Five phases: unit 426 executions / 309 targets; acceptance 25 / 8; integration-packages 87 / 38; integration-full 15 / 1; integration-latency 1 / 1. All 554 executions were uncached remote attempt 1 in their measured windows. All 1,108 retained CAS test.log/test.xml artifacts were independently size/hash verified. The latency leg measured both budget tests under a clear probe. Native floor: all 16 planned steps completed; no source changes.

load_gate: native floor threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.66 run_max_load=16.61 run_mean_load=13.05; raw floor-topup.stdout retained. Initial remote admission class=remote; free_gib=61.873958587646484; waited_seconds=0.001. Additional per-job load reports are retained with CI receipts.

skip_justification: 337 names remain SKIP-only after farm + native checks, at 168 resolved sites; every site is outside changed hunks, no shared/helper test hunk changed, and no changed skip file requires a base control. Literal reasons, resolved sites, closures and raw events are retained. Linux selects the unchanged non-Darwin key implementation. The named case-insensitive-path regression skips at proxy_process_reap_test.go:377 because its two spellings name different directories on this Linux filesystem; this is not evidence that the Mac fix works. Its actual successful Mac run is recorded below.

failure_attribution: Linux suite none; supplemental Mac failures are explicitly mapped below.
attribution_evidence: full Mac clause1/2/3(a)/4 proof retained in the pointers below.
waiver_ref: none.
uncovered_criteria: none waived; required Mac unit acceptance verified. Supplemental core commands2-4 did not run after shard1 failed, recorded below.
descoped_criteria: none.
deploy_stage_criteria: satisfied at badd3f7a62795b3023a05049f9532c62aa29b1f8 by successful Mac / make test and named RUN/no SKIP/no FAIL. Any resulting new PR head is re-gated before clearance.
post_merge_criteria: none.

fast_lane: format, policy/shell guards, generated BUILD/docs/spec drift, OpenAPI compatibility, complexity zero-growth and strict trusted-base storeclass policy all passed before runtime probes or the suite. Complexity baseline untouched, delta changes empty. No API/spec/dashboard changes require a manual dashboard preview.

## Required CI accounting

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.tqrArO dc87c978a3fde226b22ec20447a9de4a3e9718f8 aebad195700b796afcdfe5bb76a4278fd379f944 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs.
ci_required_accounting:
- changes | LOCAL-PASS | command python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.tqrArO dc87c978a3fde226b22ec20447a9de4a3e9718f8 aebad195700b796afcdfe5bb76a4278fd379f944 fork; exit 0; log /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/ci-required/closure.stdout
- runner-policy | LOCAL-PASS | command python3 .github/workflows/scripts/runner_policy.py; exit 0; log /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/ci-required/runner-policy.stdout
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- windows-lane | LOCAL-PASS | command /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.tqrArO -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class local-heavy --bead ga-49l48jna --label ci-windows-lane --record /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/ci-required/windows-lane-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/ci-required/windows-lane.sh; exit 0; log /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/ci-required/windows-lane-run-output.txt
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner |   windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR

windows_reach: none.
windows_lane_test_reach: {"packages":["./internal/credentialprovider","./internal/genshell/...","./internal/processgroup"],"closure":["internal/bazeltest","internal/credentialprovider","internal/genshell","internal/genshell/genshelltest","internal/loopbackport","internal/pathutil","internal/processgroup","internal/shellquote","internal/testenv","internal/testutil"],"changed_reachable":[],"scope":"Own GOOS=windows test-binary import closures, plain and integration; cross-build is windows-lane, actual Windows execution remains deferred."}.
deferred_to_pr_ci: windows-lane-test (non-linux-runner).
not_covered: none.
CI / required and the deferred native Windows job must still pass on the actual PR head before merge. Mac acceptance is separately mandatory under the mayor ruling.

## Retained evidence

Full execution.json retained: 6059 lines; /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/c3-run/evidence/execution.json; SHA256 3db3463096c08d704e46d944a0af3740e7d788b20411d127c2d5d12a6d8707c4.

Full remote independent audit retained: 109 lines; /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/remote-independent-audit.json; SHA256 dcc7b5ee64994bdc55ab45a22a5b4ce7bbee3d1c18fdaa7d626d8fee2ce8c85c.

Full raw by-name evidence retained: 6 lines; /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/test-evidence.stdout; SHA256 1269dab23782579e731af67e214f51f5449687ef68352e051c18fafbed6843e7.

Full skip-site inventory and literal reasons retained: 15772 lines; /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/skip-audit/plan.json; SHA256 f7eb770751c005282262310db08d69637fc8c69d5e8a6c4a6c5218d4d42d81fb.

Full site/hunk proof retained: 168 lines; /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/skip-audit/sites.stdout; SHA256 5d2ea5b108a69c9078af96a38f0419de1dc355a318f49f549314c80d47dfa116.

Full required CI receipts retained: 327 lines; /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/ci-required/summary.json; SHA256 f105b3fb86660582a36ec107524aac385f9bd84b39b286656743cabf05005ceb.

Full C6 audit retained: 64 lines; /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/c6-audit.json; SHA256 2b8204f93200ed62c1f306bf8001b3193280e7a11ba506f179daa8b925d43cfa.

Full fast audit retained: 589 lines; /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/fast-independent-audit.json; SHA256 32040c17668bacf7cb1f8c6073948781e5524210e0b5c40b74c580928f803c5d.

Full storeclass policy retained: 59 lines; /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/storeclass-proof/verified.json; SHA256 3c36dfd626d4d667a39e34c27725fd0fe052a80e2a4e9e2965a71062171955b6.

Full platform file selection retained: 97 lines; /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/platform-selection.json; SHA256 282d3658d624d5bed0b1fca9e33192200077e4b072daa198d661371d82ce866b.

test_log_dir: /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/c3-run/evidence/remote/logs; all raw evidence is retained. Re-read live bead state and service status on resume; no historical result authorizes a changed head.

## Actual Mac acceptance and supplemental failure attribution

mac_tested_head: badd3f7a62795b3023a05049f9532c62aa29b1f8 (fork quad341/gascity); actual unit-job git log output proves the checkout, independent of the base-main workflow-definition SHA.
mac_unit: SUCCESS — https://github.com/gastownhall/gascity/actions/runs/44968647105/job/137043466613; make test-mac step SUCCESS.
named_regression: TestProxyProcessReapRegistryMatchesAnEquivalentSpelling RUN, no SKIP or FAIL; raw event: 2026-11-22T01:10:03.8036070Z 2026-11-22T01:04:35.758502Z run TestProxyProcessReapRegistryMatchesAnEquivalentSpelling.
mac_workflow: failure — https://github.com/gastownhall/gascity/actions/runs/44968647105. Every job is terminal. The core job failed with the two recorded tests; the summary aggregates that core failure. Other selected jobs, including quality and cmd/gc integration, succeeded; review-formulas was not selected by needs-mac.
mac_evidence_scope: complete required unit acceptance. Supplemental integration-core stopped during packages-core-1-of-4 (61 packages); commands packages-core-2-of-4 through packages-core-4-of-4 did not run. Linux full-suite evidence above remains complete. No workflow-wide Mac PASS or completed later-core coverage is asserted.

failure_attribution: TestCredentialProviderParentCancellationCleanup -> ga-2inf4mec | clause3(a): unreachable changed production package in Darwin/arm64 CGO1 Go1.27.4 integration test-binary imports; clauses1/4 clear.
  clause2: tracker newly filed and opened in this observing run after open/closed lookup NONE; explicit gm-sf3238 discovering-run exception, no predating sighting claimed.
  clause1: unchanged body and exact failure site; no shared/helper hunk. clause4: separate package, no added test load/census/target. Original result remains FAIL, never a test PASS.
failure_attribution: TestStartSQLServerRetriesWhenAClientSocketHoldsTheProbedPort -> ga-2tucpj0k | clause3(a): unreachable changed production package in Darwin/arm64 CGO1 Go1.27.4 integration test-binary imports; clauses1/4 clear.
  clause2: tracker newly filed and opened in this observing run after open/closed lookup NONE; explicit gm-sf3238 discovering-run exception, no predating sighting claimed.
  clause1: unchanged body and exact failure site; no shared/helper hunk. clause4: separate package, no added test load/census/target. Original result remains FAIL, never a test PASS.

waiver_ref: none. No test or workflow rerun was used to reach this first-head verdict.

resolved_pr_ci_at_first_head: Windows lane / native tests SUCCESS https://github.com/gastownhall/gascity/actions/runs/44968594543/job/137043209649; CI / required SUCCESS https://github.com/gastownhall/gascity/actions/runs/44968594543/job/137043635553. The initial Linux gate native-Windows deferral above is resolved for this head.

Full Mac unit verification retained: 21 lines; /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/mac-first/unit-verification.json; SHA256 df47fa7bc191016433241ea235e475057526b0ea5a924dc4cc1babe0c0f96f79.

Full Mac raw unit log retained: 51393 lines; /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/mac-first/job-137043466613.raw.log; SHA256 b7421d9b32f7e7ebbcb686fcec40407bafae9064c862cc9a3ab6b29d76cd27ff.

Full Mac supplemental attribution retained: 39 lines; /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/mac-first/failure-attribution.json; SHA256 78c82fe36af638108fc735a838b8cc71f4f7b3bc8c733b854e4f3724747f1153.

Full Mac failure import closures retained: 58 lines; /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/mac-first/failure-import-proof.json; SHA256 32ce8b42a3d91d6c7681f7a7df82723e389575c2d7e680a919cc953f57e14d8c.

Full Mac unchanged failure sites retained: 6 lines; /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/mac-first/failure-sites.stdout; SHA256 e5cb97e6c7faa74c743735b8c536b5c611ea6e5ef0bd27d87c8cfb2c16126a65.

Full Mac final job results and raw-log hashes retained: 3276 lines; /var/tmp/gc-gate-ga-49l48jna-0dzji_k8/mac-first/remainder-terminal.json; SHA256 1e2f476371d1ef601c9e3f40023e63786aecd03c14e7a58001812a06238f9c30.

publication_guard: committing this final record advances the draft head. The new head receives a fresh gate and actual Mac verification before ready, deploy-clearance or merge-request. Those additional head-specific receipts stay in bead notes and durable artifacts so publication does not change the cleared head again.
