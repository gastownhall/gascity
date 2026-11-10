# Release gate: Kimi lexical discovery paths

**Verdict:** **PASS**

Date: 2026-11-06T22:48:15.029102+00:00. Bead: `ga-azsskb2k`; build `ga-4pl001`; review `ga-co6ax3rx`.
DEPLOY_MODE: remote; PUSH_REMOTE: fork; repository: gastownhall/gascity.
Reviewed source: `0107cfaf45107dd8ec0669f110b993306e8aa1c0`. Branch: `deploy/ga-azsskb2k-gate`.
Gate base: `0bd038188e7947cd283868b8640091fd14f90857`. Tested merge: `fe1ebdfd1498cb4c3deb15d5b9cd8cd8fc3c7284`; tree `e7c45aa9a1897ed0b439b726a38fa4dc0c967b8b`.
merge_state: clean (origin/main@0bd038188e7947cd283868b8640091fd14f90857)
gate_base_kind: current-tip (at evaluation)
merge_validation: mpr checks the exact PR head and its actual required CI at merge time. PR preparation refreshed origin/main to `70583dada11ea4c5393b35c2ce0bf15dc42b7528`; the recorded gate base stays pinned. No rebase.

This PASS authorizes PR opening. Merge requires `CI / required` green on the PR head and `Mac / make test` success if that tier runs. The macOS acceptance criterion is explicitly a deployment-stage condition, not a Linux test result.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Reviewer ga-co6ax3rx PASS names the exact resolved source above. No carryover or reviewer-added source. |
| 2 | Acceptance criteria met | PASS | Independent RED/GREEN checks, three discovery APIs plus metadata extraction, broken-link guard and cycle test; both orders of PR4933 compatibility independently verified. Superseding wording is prepared verbatim for PR open. Conditional Mac coverage must succeed when run. |
| 3 | Tests pass | PASS | Full-scope five-phase fresh remote suite plus all16 native checks and four-leaf CI accounting. One unchanged mail-hook latency failure is attributed under standing3a clauses1,2,3(b),4, below. No waiver or failed-test retry. |
| 4 | No open high-severity review findings | PASS | Review has zero HIGH findings; security/style review PASS. |
| 5 | Final source clean | PASS | Assigned and merged worktrees clean, tested HEAD unchanged. The isolated deploy adds only this record; its post-commit clean state is verified before push. |
| 6 | Merge state and build/vet | PASS | Clean pinned-base merge; full Bazel build/nogo succeeds across1456 targets, BEP overallSuccess=true, failed targets0; hooks active. |
| 7 | Single feature theme | PASS | Only internal/sessionlog/kimi_reader.go and kimi_reader_test.go. Ancestry citation/denylist check PASS for this deploy/build; no stack, agent configuration or independent feature. |

## Acceptance evidence

Mayor clarification `gcg-2505729` corrects the drafting slip: the two new lexical/cycle reproducers are RED without the fix; the verbatim ported broken-link guard is GREEN on both trees. This is a spec clarification, not a waiver.
Pinned-main production plus reviewed tests: lexical-path and cycle tests FAIL at their exact-path assertions; broken-link guard PASS. Fresh full-suite unit output on the fixed merged tree: all three roots PASS, including both lexical-root subtests. No owned SKIP or FAIL.

- TestFindKimiSessionFileSkipsBrokenSymlinkedRoot: PASS.
- TestFindKimiSessionFileTerminatesOnSymlinkCycles: PASS.
- TestKimiDiscoveredPathsStayUnderLexicalSearchRoot: PASS (symlinked search root and nested symlinked account root).

Full unit coverage includes sessionlog, worker/transcript and worker, with no GC_FAST_UNIT guards in those packages; Bazel/nogo supplies the repository-authoritative vet/lint evidence. Review's broken-link mutant additionally confirms the ported guard is live.
`merge-compatibility-replay.json` independently simulates PR4933 squash then RED/GREEN replay, clean at every step and final tree319efb8cd96ccbc86b5d1b6220ffbfd90e0fce3e. Direct merges both orders also yield that exact source tree. PR4933 and its branch were not changed.

## Criterion3 command and results

test_cmd_scope: full-suite
test_counts: PASS=147700 FAIL=1 SKIP=510
test_log_dir: /var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/criterion3/evidence/remote/logs (537 retained logs; results are top-level Go lines per job, not distinct-test counts).
test_exec_mode: remote-exec (all Bazel results remote; mandatory native floor recorded separately).
waiver_ref: none.

Canonical launch (actual saved argv):
```sh
env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-azsskb2k /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/merge --base 0bd038188e7947cd283868b8640091fd14f90857 --bead-json /var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/bead.json --owned /var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/owned-tests.tsv --run-dir /var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/criterion3/evidence
```

Actual phase flag records, including forced fresh test results:
```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/criterion3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/criterion3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/criterion3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/criterion3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/criterion3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

Phase scopes: unit=`make -k check` → `bazel test //...`; acceptance=`bazel test $BAZEL_FLAGS --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests`; integration-packages=`bazel test $BAZEL_FLAGS --config=integration //test:integration_packages`; integration-full uses the whole integration target with only the shipped latency owners excluded; integration-latency runs exactly those owners with the canonical farm probe, disabled sharding and unchanged fastest-of7/1s budget. Filters are derived from gate-latency-tests.txt, not a selected exit-contract list.

| Phase | Fresh remote results | Terminal verdict |
|---|---:|---|
| unit | 395 | PASS |
| acceptance | 25 | PASS |
| integration-packages | 86 | PASS |
| integration-full | 14 | PASS |
| integration-latency | 1 | FAILED1, attributed below |

All521 results: remote strategy, no cached-local/remote flags, no retries, attempt1, start within the phase window;1042 raw log/XML artifacts independently digest-verified. farm_test_seconds: 12406.267; fallback_reason: none; no farm/local fallback or suite stitching.
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}.
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 41.04973220825195, "waited_seconds": 0.0}. Separate native continuation uses remote25GiB/cap2 admission plus the unchanged local soft load wait.
heavy_mode: none (classifier); heavy_units: n/a; coverage_check: full canonical scope, no heavy-package composite required.
owned_topup: none; all three owners already PASS in full unit output.
floor_topup: complete=true,16/16 exit0; /var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/criterion3/evidence/remote/floor-topup.json retains every exact command and log. This is the unrun canonical floor after attribution, never a latency retry. Systemd restart5.7s/15s, test PASS54.33s; Darwin compile and both runner self-tests PASS.

LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=107.06 run_max_load=108.37 run_mean_load=99.44 run_readings=13 wait_first_load=36.99 wait_max_load=110.13 wait_mean_load=67.50 wait_readings=61 read_errors=0
load_threshold: 15; load_waited_seconds: 1800; load_wait_timed_out: 1; run_start_load: 107.06; run_max_load: 108.37; run_mean_load: 99.44; run_readings: 13.

## Standing3a latency attribution

failure_attribution: TestGcSeatHookLatencyBudget → ga-y39d8tx; clause3(b) CROSS-PR.
The current canonical latency lane reports clear probe waited0s/load1=4.36/load5=1.27/load15=0.44/cpus32/threshold50. Bd latency PASS; mail-hook minimum table1000ms, assertion fastest-of7>=1s, FAIL. Prime662ms/drain463ms PASS. This is a real recorded failure, not a busy deferral; no rerun or altered budget/probe/statistic.
Clauses1/2/4: site183/function UNCHANGED, shared0, predating matching tracker opened, no integration-package overlap. Independent retained canonical clear-probe failure on our own PR8314 was re-read and hash-verified; live author quad341/head and comparison source/base/merge resolved. Its five cmd/gc/gate files share no file with the current two Kimi files; latency fixture blob identical. Witness SHA2566f5065c4d1312604db2696c6e5be50a8a1394d496eb82d34fc8e24aab581d74b. Evidence: /var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/latency-attribution/proof.json; tracker sighting read-back verified. This is the first matching sighting on this carrier.
The original execution.json remains exit1/criterion3_valid=false and latency result=failed/complete=false. Standing3a attribution plus the separately completed unrun native floor supports this assessed PASS; the raw result is never rewritten. No waiver.

## Skips, fast lane and CI

skip_inventory:496 remote rows and39 native rows retained. All289 remote source sites and36 native sites UNCHANGED, no shared/helper hunks, no unresolved references or extra environment absence missed by CONT-aware audit. Required environment-absence parents/packages all ran in the canonical native floor; non-owned native SKIPs are inventoried. Same-package skipped Claude/Codex/Gemini path-alias tests are unchanged early Darwin guards; changed Kimi tests all ran PASS. Other SKIPs are unchanged platform, live-provider, helper-process or lane-selection preconditions; the changed Kimi discovery functions do not control their guards. No owned skip is excused.
policy_lane: four make-check guards; whole-tree nogo lint;14 policy/format/generated/docs/security targets PASS. GLT_RECORD pin2.14.0 resolved2.14.0 mismatch=false. Dedicated changed-format view pins LINT_CHANGED_REF and tracked scope; actual diff format PASS.
generated_drift: make bazel-sync then git diff --exit-code PASS in its own fresh view. No fast lane changed the suite scratch. These lanes completed before runtime setup/suite.
ci_lane_run: n/a (no CI-config diff).
ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/merge 0bd038188e7947cd283868b8640091fd14f90857 0107cfaf45107dd8ec0669f110b993306e8aa1c0 fork; workflow blob e5b11c348505dab28dc223e874bc2c61cbbb33c5;4 LEAF jobs need a status.
ci_required_accounting: runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<out> python3 .github/workflows/scripts/runner_policy.py; exit=0; log=/var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/ci-accounting/runner-policy.log
ci_required_accounting: changes | LOCAL-PASS | ci_required_triggers.py from committed workflow, exact merge-base diff; exit=0; log=/var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/ci-accounting/closure.txt
ci_required_accounting: credential-provider-windows | NOT-TRIGGERED | closure tool no credential/shared match; runs-on resolves through actual runner-policy runner_windows=blacksmith-4vcpu-windows-2025; proof=/var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/ci-accounting/closure.txt
ci_required_accounting: pack-gate | NOT-TRIGGERED | closure tool no packs/shared match; proof=/var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/ci-accounting/closure.txt
windows_reach: none; helper exit0/empty output.
deferred_to_pr_ci: none.
not_covered: none.
Mac acceptance remains conditional on actual PR CI, explicitly distinct from the Darwin compile floor check.

## Retained evidence

ICU74 provenance (no library-path overlay or test-action key workaround):
```text
GATE_ICU74 lib=libicui18n.so.74 file="/usr/local/lib64/icu74-compat/libicui18n.so.74.2" sha256=d90d62f0d7b6c6fbc279ae5e433cc7004aa47cdb106a71a02350099f8df9ae51 recorded=yes
GATE_ICU74 lib=libicuuc.so.74 file="/usr/local/lib64/icu74-compat/libicuuc.so.74.2" sha256=33499dd97a5393926b32e0b535f5340f7dae14b35e59025b572e6bd9ad48a2ae recorded=yes
GATE_ICU74 lib=libicudata.so.74 file="/usr/local/lib64/icu74-compat/libicudata.so.74.2" sha256=c891c5fdb45e3cf756b728abe256be7af1bfc97f00ca2f2fac450c96c09bd4b7 recorded=yes
GATE_ICU74 verdict=OK provenance=recorded sonames="libicui18n.so.74 libicuuc.so.74 libicudata.so.74"
```
Fresh-view/base-pin evidence:
```text
GLT_RECORD: {"golangci_lint":{"pin":"2.14.0","resolved":"2.14.0","mismatch":false}}
FRESH_TREE_RECORD from=/var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/merge head=fe1ebdfd1498 tree=e7c45aa9a189 dir=/var/tmp/gc-fresh-tree.HATr6n modified=0 untracked=0 ignored=4 ignored_names=bazel-bin,bazel-merge,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=0bd038188e7947cd283868b8640091fd14f90857 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.HATr6n
GLT_RECORD: {"golangci_lint":{"pin":"2.14.0","resolved":"2.14.0","mismatch":false}}
FRESH_TREE_RECORD from=/var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/merge head=fe1ebdfd1498 tree=e7c45aa9a189 dir=/var/tmp/gc-fresh-tree.MyVzNz modified=0 untracked=0 ignored=4 ignored_names=bazel-bin,bazel-merge,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=0bd038188e7947cd283868b8640091fd14f90857 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE,LINT_CHANGED_REF dir=/var/tmp/gc-fresh-tree.MyVzNz
FRESH_TREE_RECORD from=/var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/merge head=fe1ebdfd1498 tree=e7c45aa9a189 dir=/var/tmp/gc-fresh-tree.3ukEXL modified=0 untracked=0 ignored=4 ignored_names=bazel-bin,bazel-merge,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=0bd038188e7947cd283868b8640091fd14f90857 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.3ukEXL
```

Compact original execution (raw failure preserved):
```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":1,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/criterion3/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":41.04973220825195,"waited_seconds":0.0,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"},"test_exec_bep":{"unit":{"results":395,"remote_executed":395,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":12406.267,"criterion3_valid":false,"latency_leg":{"complete":false,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":{"elided_array":{"items":1,"bytes":1918}},"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"FAIL":1}},"verdict":"failed","message":"a latency test failed after a probe that said clear (TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=FAIL): it ran on a worker whose load average was under the threshold, so this is a real verdict, not a deferral, and it is never retried into a pass. The load it ran under is in latency-leg.json.","rc":1},"test_cmd":{"elided_array":{"items":5,"bytes":2084}}}
```

Full original execution retained: 150 lines; /var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/criterion3/evidence/execution.json; SHA256 1b2143693af3c4077a6effa0aa7d32ea608ed633ebb9f9ce2a032bd1d03efee7.

Full final evidence assessment retained: 218 lines; /var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/final-evidence-assessment.json; SHA256 51b2a7c8227a745668dee2a2b89982586c849d2b9c0a4ba11a45448c26d2eeb5.

Full raw evidence digest manifest retained: 1646 lines; /var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/RAW-EVIDENCE-SHA256SUMS.txt; SHA256 107ba7824eab5567947b1e4c197107ff6ed0b52c8e111185577d7b6b7049195d.

Full remote skip inventory retained: 3970 lines; /var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/criterion3/evidence/remote/skip-inventory.json; SHA256 5538eba2bd9577b501a14581f67512ec0c2024ac6f2f4456062f82fd4f5dfedd.

Full native skip inventory retained: 314 lines; /var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/native-skip-inventory.json; SHA256 31a9cb7f4908fee2d3de626cdacbdacfacbbdc914088fd232fe0332ce3fdd11a.

Full native floor plan/results retained: 909 lines; /var/tmp/gc-gate.ga-azsskb2k.yOhoV9UH/criterion3/evidence/remote/floor-topup.json; SHA256 73905d9461df353e999ee0513a6abbbf95e3f0060c62d3de67375f08507e348f.

