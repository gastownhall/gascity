**Verdict:** **PASS**

# Release gate: isolate the idle-boundary peek timeout

Deploy ga-40vigmy6; build ga-cg94kae0; review ga-8w813dgt.

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | PASS | Reviewer verdict/source agree; full SHA resolved in git and live review notes checked. |
| 2 | Acceptance criteria met | PASS | Exact one-file spec, name/order and commit-message checks. All 60 package tests pass in eight fresh shards; both timeout mutants fail and 100/100 PASS at a 3% CPU quota. Actual PR body delivery is verified before clearance. |
| 3 | Tests and required CI | PASS | Five fresh remote phases, complete native floor, all skips justified and six CI leaves accounted below. |
| 4 | No open HIGH finding | PASS | Complete review notes contain no open style/security/spec HIGH finding. |
| 5 | Final branch clean | PASS | Source and synthetic merge clean; publication verifies a clean record-only commit above the exact reviewed source. |
| 6 | Merge state/build/vet | PASS | Clean synthetic merge at the frozen current-main base; whole-tree build and explicit nogo/vet terminal SUCCESS, raw BEPs audited. |
| 7 | Single feature theme | PASS | One test-only idle-boundary timeout fix; no production code, timeout value, test count or build layout changed. |

The pinned merge has no docs/PROJECT_MANIFEST.md. TESTING.md, current CI workflows, bead acceptance and the release protocol define the checks.

deploy_mode: remote
push_remote: fork
gate_head: 308fa51dda30befe5789bf1b77c63e5d8ecb3a04
gate_base: f2bb5cdc01d827f2de1275acdb81e7890a010813
synthetic_merge: 7faf61c8b50eb7d0b92a868dd9e624e43165fb08
merge_tree: f82342ff52f4c2f67cf515783827f1b9c154cad2
merge_state: clean (origin/main@f2bb5cdc01d827f2de1275acdb81e7890a010813; files: none)
gate_base_kind: current-tip
merge_validation: MPR pins the cleared PR head and verifies required CI / merge-time checks
waiver_ref: none
failure_attribution: none

## Criterion 3 evidence

- test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-validate.B83rSu --base f2bb5cdc01d827f2de1275acdb81e7890a010813 --bead-json /var/tmp/gc-gate-ga-40vigmy6-qpygq9e8/bead-criterion3.json --owned /var/tmp/gc-gate-ga-40vigmy6-qpygq9e8/owned.tsv --run-dir /var/tmp/gc-gate-ga-40vigmy6-qpygq9e8/c3-run/evidence
- test_cmd_scope: full-suite
- phase_command_receipts: ["test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-40vigmy6-qpygq9e8/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-40vigmy6-qpygq9e8/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-40vigmy6-qpygq9e8/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-40vigmy6-qpygq9e8/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-40vigmy6-qpygq9e8/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"]
- test_counts: {"PASS":158178,"FAIL":0,"SKIP":617}
- diff_tests_executed: TestNonTmuxIdleBoundaryPeekBound PASS (unit-bc88cd0367f0a55aaefd.log)
- test_exec_mode: remote-exec
- heavy_mode: none
- test_exec_bep: {"audit":"/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/five-phase-raw-audit.json (SHA256 0065c699f0e86d680b390288c4dea2ca361cb47802f728c173984b5962abb5be)","raw":{"unit":"/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/c3-run/evidence/remote/unit.bep.json","acceptance":"/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/c3-run/evidence/remote/acceptance.bep.json","integration-packages":"/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/c3-run/evidence/remote/integration-packages.bep.json","integration-full":"/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/c3-run/evidence/remote/integration-full.bep.json","integration-latency":"/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/c3-run/evidence/remote/integration-latency.bep.json"}}
- rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}
- fallback_reason: none
- admission: {"class":"remote","floor_gib":25.0,"free_gib":70.0389633178711,"waited_seconds":0.001,"cap":2}
- farm_test_seconds: 13904.084
- delta_gate: {"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"f2bb5cdc01d827f2de1275acdb81e7890a010813","head":"308fa51dda30befe5789bf1b77c63e5d8ecb3a04","merge":"7faf61c8b50eb7d0b92a868dd9e624e43165fb08","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"f0716ecbc22dff67fccbb30be8e53c917e8a606aa6f285f696c020327e589259"}
- owned_topup: []
- floor_topup: 16 steps, all exit 0; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/c3-run/evidence/remote/floor-topup.json (SHA256 9a1c4cad8e1b1be3b45a21c8354f6948623a6f0848cd4ba417f95f136bbb5d53)
- skip_inventory: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/skip-independent-audit.json (SHA256 50a9365fc08ae76a10d7fa8de89e1ee2993e3192596b581742f3a47a07e10376)
- latency_leg: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/c3-run/evidence/remote/latency-leg.json (SHA256 7bfac96dbbd96f5428cea4eedcf6892b3c2354390f5ed75ce9663ce4764d201f)
- test_log_dir: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/c3-run/evidence/remote/logs
- policy_lane: Format, lint/nogo, policy guards and docsync PASS in independent fresh views; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/fast-independent-audit.json (SHA256 e6398788a1708e6f810611bf07c2c9aa3be263e13eb142812eb2d96ad490c282)
- drift_lane: make bazel-sync; git diff --exit-code; empty untracked status; generated/OpenAPI checks PASS
- complexity: Zero growth against pinned base, baseline untouched; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/complexity-zero-growth.json (SHA256 e4267511c8575e106d9914babb27567cb0c385235a87efa1b96ce01c288ee4ac)
- ci_lane_run: n/a (no CI job/matrix/timeout/required-list change)

| Phase | Fresh remote actions | Cached/local/retried |
| --- | ---: | --- |
| unit | 422 | 0/0/0 |
| acceptance | 25 | 0/0/0 |
| integration-packages | 87 | 0/0/0 |
| integration-full | 14 | 0/0/0 |
| integration-latency | 1 | 0/0/0 |

549 fresh remote first-attempt actions and 1098 raw log/XML artifacts verified by digest and length. Every result started inside its phase window. Counts sum top-level results over jobs, not distinct names. Latency budgets were measured only under a clear probe.
Native floor includes environment-absence cases, beadstest, Darwin compilation and runner self-tests. Podman socket, pinned image, active sweep, ICU74 and disk were checked before launch. /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/c3-run/evidence/execution.json (SHA256 b64e9a8ffff581e9f294e076b546f776586c0c8366ed171470cb7c03a3743902).

- load_threshold: 15
- load_waited_seconds: 0
- load_wait_timed_out: 0
- run_start_load: 6.68
- run_max_load: 9.32
- run_mean_load: 8.71
- run_readings: 6
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=6.68 run_max_load=9.32 run_mean_load=8.71 run_readings=6 wait_first_load=6.68 wait_max_load=6.68 wait_mean_load=6.68 wait_readings=1 read_errors=0

## Skip and supplemental-lane evidence

675 all-depth SKIP events at 317 sites; owned SKIP 0 and unresolved 0. Every guard-file blob is identical at BASE and the merge. No SKIP belongs to runtimecontract.
The tool reports SHARED-HUNKS internal/runtime/runtimecontract/nontmux_test.go:1 for the added seven-line comment. Fresh per-phase remote BASE comparisons and native same-command BASE comparisons show identical reasons by name for every skipped case. No prior test result is credited.
base_comparison_required: true — the comment-only SHARED-HUNKS entry is answered by /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/base-skip-comparisons-complete.json (SHA256 88cfa4d3990a87e026087052ea07a2be6bec4dd2af945a89000daedbf816b0de). Detailed receipts: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/base-skip-comparisons.
skip_guard_matrix_sha256: f75d6f554e9d4b8a49a2b8c2adcc2f9f54314ae3e1cb882bd9ce00c7df4a7153
manual_source_review_pending: false
compare_notes: ["COMPARE-NOTE a generated path name differs between the runs in 1 SKIP(s), read as the same reason because it is the only difference and the same file and line were printed (a generated name is the digits Go appends to a temp directory's name, the process id in the name of gascity's temp root gct<pid>-<digits>, or a UUID right after a /work/ directory, which is how a Bazel remote worker names its work directory; the logs are not changed): TestIsGoTestExecutableRecognizesThisBinaryWhateverItIsCalled (base 7779c7fd-497a-4432-8e96-19c54864047b and 2814545110, head 86c67715-c43e-471d-ad50-6654d30b59c6 and 3362949560)", "COMPARE-NOTE a generated path name differs between the runs in 1 SKIP(s), read as the same reason because it is the only difference and the same file and line were printed (a generated name is the digits Go appends to a temp directory's name, the process id in the name of gascity's temp root gct<pid>-<digits>, or a UUID right after a /work/ directory, which is how a Bazel remote worker names its work directory; the logs are not changed): TestProxyProcessReapRegistryMatchesAnEquivalentSpelling (base 2870857464, head 4114284731)", "COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a04, head rbe-a08)"]
The actual bazel.yml split-strict path case matches neither the source path nor this gate record; no supplemental manual lane is required. /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/supplemental-lane-scope.json (SHA256 16d54964b11c5458a67c9102c8926e10ce2e66bb5ffdd6d7fa314c52dcce4700).

## Required CI accounting

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.B83rSu f2bb5cdc01d827f2de1275acdb81e7890a010813 308fa51dda30befe5789bf1b77c63e5d8ecb3a04 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status.

ci_required_accounting:

- runner-policy | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.B83rSu -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-40vigmy6-qpygq9e8/ci-runner-policy-v3.sh; exit0; /var/tmp/gc-gate-ga-40vigmy6-qpygq9e8/ci-required-v3/runner-policy-output.txt
  LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=8.95 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=8.95 wait_max_load=8.95 wait_mean_load=8.95 wait_readings=1 read_errors=0
- windows-lane | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.B83rSu -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-40vigmy6-qpygq9e8/ci-windows-lane.sh; exit0; /var/tmp/gc-gate-ga-40vigmy6-qpygq9e8/ci-required-v3/windows-lane-output.txt
  LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.57 run_max_load=10.14 run_mean_load=10.06 run_readings=3 wait_first_load=9.57 wait_max_load=9.57 wait_mean_load=9.57 wait_readings=1 read_errors=0
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner |   windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR
- changes | LOCAL-PASS | python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.B83rSu f2bb5cdc01d827f2de1275acdb81e7890a010813 308fa51dda30befe5789bf1b77c63e5d8ecb3a04 fork; exit0; /var/tmp/gc-gate-ga-40vigmy6-qpygq9e8/ci-required-v3/closure.txt

windows_reach: credential-provider-windows none; windows-lane-test none (its own allowlist and ordinary/integration Windows import closures).
deferred_to_pr_ci: [{"job": "windows-lane-test", "reason": "non-linux-runner"}]
not_covered: []
host_gaps: []
PASS authorizes PR-open only. MPR verifies CI / required and each deferred native job on the current PR head before merging.

## Acceptance evidence

Exact spec snippet, test name/order and commit-message checks: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/acceptance-code-audit.json (SHA256 6798ffe10bf36d8f8a83565ad2ce6f83ab99f54c6bf8274c4b31f9b91fae4de1).
All 60 original top-level runtimecontract tests passed once across eight fresh unit shards. Maximum shard 54.869s. Owned test PASS in 0.1s, shard8. /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/runtimecontract-independent-audit.json (SHA256 b98ef897efe18c93afdee98d706108a7d595a06a12a07805b1ffbf20c1473678).

| Check | Result | Evidence |
| --- | --- | --- |
| p1 | PASS: Expected mutant test FAIL, Bazel exit3, fresh remote uncached; raw test.log digest/length verified | /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/acceptance-p1-verified.json (SHA256 97e38202f745e6e7e399e575c964e433df90d50ce28aa3f40ddc6ecdd49706e6) |
| p2 | PASS: Expected mutant test FAIL, Bazel exit3, fresh remote uncached; raw test.log digest/length verified | /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/acceptance-p2-verified.json (SHA256 1e6cab719dc63acef425ba442a4ba711c731f645f8092f4727fda30a9da17a9a) |
| quota | PASS: 100 PASS,0FAIL,0SKIP; CPUQuota3%; max0.3s | /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/acceptance-quota-verified.json (SHA256 9187c6947494a8729ff44b37d4684b143d0ef329c41004460bfde6f5e4370a34) |

P1 sets the read window to 3s and fails both timeout-detail and elapsed checks. P2 uses a background context and one-minute operation timeout, and fails the elapsed check at about 30s. Each mutation runs in its own disposable view; the canonical merge remains clean. The quota limits only the actual test process; no synthetic host load is generated.
Historical controlled A/B, retained in the complete review record: original test 95/100 failures at 3% CPU quota (69 handshake-related SKIP mismatches, 26 elapsed-bound failures), fixed 100/100 PASS. This gate independently repeats the fixed quota check and both mutants above; historical results are not credited as this run.
Prepared PR-body SHA256 ff1875db6c0f9facccc8c1f914437aca2f3efc348484b108c7f5ad928915b911; actual GitHub body delivery is verified before clearance/handoff.
Native comparison context correction: the initial BASE native checks used umask 0077; the canonical workload used 0022. Both sets passed, but the final skip audit credits only the additional matching-0022 comparisons. The original receipts remain retained. /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/native-umask-correction.json (SHA256 afcb9dcaeb252983d7d2a7aa08e0552882ea8ca0eaa23051256f09ae1e395bab).
Tracker ga-8xk44z8s stays open until this fix lands. Landing is the exit of PR #9186 deploy hold; PR-open alone does not clear it. Mayor quiet-window release covers only ga-40vigmy6 before 04:00 Pacific.

## Retained evidence

Verified archive /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf; manifest SHA256 593cc7f2b97a3262bf5f8c6858ab0803044aab3dd1a904007bef95469e4a8f0a; tested merge bundle SHA256 865583ac89aaabe59c4feeba96ac0fad0e2c7f42bbd49a7713a7e05489a2f80d. Publication archive separately records the final record commit, pushed PR head, clearance and verified handoff.
Preparation corrections: the first static watcher could not open its not-yet-created script; no lane or test ran. The private spec extractor was corrected for an indented code fence, source equivalence verified, and actual static checks ran once under after-c6-v2-run. /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/setup-correction.json (SHA256 199860ff364eae9de77ad03562fd4c1a3d8438e192149b965177c02de08a5923). Later follow-up watchers stopped on a heartbeat before starting any acceptance/base test. Explicit identity and captured bounded heartbeat retries were added; subsequent services use v2 run directories. /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-40vigmy6/gate-20261121-6dxtnvrf/evidence/continuation-setup-correction.json (SHA256 2bef315c47bcad653f67482ecea3593dbe3acc6a72e11181d5f4a61e707e607e). Original errors are retained, no test result was retried into a pass, and no criterion failure was attributed or waived.
