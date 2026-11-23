# Implicit-provider startup prompt selection — release gate

**Verdict:** **PASS**

Deploy bead: `ga-cwojep57`; build: `ga-uoss7e`; review: `ga-52urgj7p`.
Reviewed source: `e9b3e3e96ff4e24c919f582a0e4309861463eb9e`. Mode: remote; push remote: fork; base branch: main.
Pinned gate base: `a47ce14d1d187fc17fa8f56d12e5d5e59874f655`; gate_base_kind: current-tip.
Tested merge: `08e9c03e7f8e204f8f511616448d4ce7eb191909`; tree: `2ddd2347b5e6310f84fb6ed6864a2ea484bfeed6`.
merge_state: clean (origin/main@a47ce14d1d187fc17fa8f56d12e5d5e59874f655; files: none).
merge_validation: merge result built and vetted here at the pinned base. MPR owns merging; a later head requires a fresh gate.

| # | Result | Evidence |
|---|---|---|
| 1 — Review | PASS | Reviewer PASS on the exact resolved source; no carryover or reviewer-added commit. |
| 2 — Acceptance | PASS | Config stamps removed for city and rig agents; explicit template precedence, implicit-only selection and nil-city guard inspected. Launch uses the derived path at both probe and render; resume renders a copy with the same selected template. All six diff-owned tests and subtests PASS. Default graph/pool choice, chat boundaries and fragment compatibility documented. The ordinary PR destination is verified at PR open before closing. |
| 3 — Tests and policy | PASS | Complete five-phase uncached remote suite, native floor, policy/drift lanes, complete matching base skip proof and six-leaf required-CI accounting below. |
| 4 — Severity | PASS | Zero unresolved HIGH findings in the review. |
| 5 — Clean branch | PASS | Role checkout and test scratch pristine before writing this record. The isolated deploy head must contain the reviewed source plus this record only; publication verifies a clean worktree and exact record-only diff after commit. |
| 6 — Merge build/vet | PASS | Exact clean merge tree built and vetted/nogo; both BEPs overallSuccess=true. |
| 7 — Theme | PASS | One implicit-provider prompt selection theme: config deletion, selector and launch/resume consumers, tests, owning invariant and matching docs/changelog plus this record. |

Single prompt-name owner is primeBuiltinPromptFile. Explicit prompt_template wins; explicit prompt-less agents and client-chat suppression remain intact. formula_v2 selects graph-worker by default; formula_v2=false uses pool-worker where that builtin applies. Default graph-worker is plain markdown, so global/appended fragments no longer reach implicit workers; declare an explicit agent with prompt_template to retain those fragments. This accepted compatibility change is disclosed in the tutorial and changelog. No live-session validation is claimed.

test_cmd_scope: full-suite; heavy_mode: none; waiver_ref: none (Gas City has no waiver path).
test_cmd: `env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-cwojep57 GC_GATE_DELTA=shadow GC_GATE_LOAD_DEFER_MAX=3 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-validate.HB657c --base a47ce14d1d187fc17fa8f56d12e5d5e59874f655 --bead-json /var/tmp/gc-gate-ga-cwojep57-20j9mt96/c3-bead.json --owned /var/tmp/gc-gate-ga-cwojep57-20j9mt96/owned.tsv --run-dir /var/tmp/gc-gate-ga-cwojep57-20j9mt96/c3-run/evidence`.
Unit command: make -k check (whole-tree bazel test //... plus shell guards); acceptance: bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests; integration packages: bazel test --config=integration //test:integration_packages; full integration and its separated latency leg: //test/integration:integration_test. Exact per-phase flags, commands and BEPs are retained in the execution proof.
Phases: whole-tree unit/check; acceptance; every integration-tagged package; full integration excluding latency inventory; inventory-only latency under a clear farm probe. All force --nocache_test_results and --noremote_local_fallback, first attempts, remote strategy and phase-window start proven from BEP.
diff_owned_evidence: /var/tmp/gc-gate-ga-cwojep57-20j9mt96/merge-owned-explain.txt (owned set identical at source and tested merge).
not_owned_skip_evidence: /var/tmp/gc-gate-ga-cwojep57-20j9mt96/skip-source-audit/evidence.json; per-name unchanged sites, explicit shared/helper import reach and same-command/by-name base comparisons; the supplemental simulator baseline timeout is disclosed below.
test_bd: isolated-test-run:   pinned test bd (gm-i3csn7): GASCITY_TEST_BD version=v1.3.4 bin=/home/jaword/.local/bd-versions/v1.3.4/bd reports='bd version 1.3.4 (7ef2fd05e: HEAD@7ef2fd05eb0c)' ref_check=match:7ef2fd05e origin=preexisting pin_source=/var/tmp/gc-merge-validate.HB657c/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
gate_base: a47ce14d1d187fc17fa8f56d12e5d5e59874f655; gate_head: e9b3e3e96ff4e24c919f582a0e4309861463eb9e; delta_gate: {"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"a47ce14d1d187fc17fa8f56d12e5d5e59874f655","head":"e9b3e3e96ff4e24c919f582a0e4309861463eb9e","merge":"08e9c03e7f8e204f8f511616448d4ce7eb191909","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"336ccab1695140ac5b6b7468543beb224dace6e085f0cfc4166446a662739856"}.
failure_attribution: none; policy_attribution: none; post_merge_criteria: none.
rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}.
admission: {"class":"remote","floor_gib":25.0,"free_gib":58.81378936767578,"waited_seconds":20.004,"cap":2}.
test_exec_bep: {"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}}.
owned_topup: [].
test_exec_mode: remote-exec; fallback_reason: none; farm_test_seconds: 12622.211.
test_log_dir: `/var/tmp/gc-gate-ga-cwojep57-20j9mt96/c3-run/evidence/remote/logs` (retained).

```text
test_counts: PASS=158787 FAIL=0 SKIP=618
  (top-level result lines, column 0, summed over the 570 logs; a test that several jobs run counts once per job; sub-tests are not counted)
diff_tests_executed:
  TestBuildDesiredState_ClientProviderChatResumedForRoutedWorkLaunchesWithoutWorkerPersona PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-088cc38d26d094729229.log)
  TestDoPrimeImplicitProviderAgentMatchesPromptlessPoolAgent PASS (integration-packages-bc482d2ae768251ad47f.log, unit-d9e667428f972d067bd3.log)
  TestResolveDiscoveredSessionTemplate_ClientProviderChatLaunchesWithoutWorkerPersona PASS (integration-packages-821a75e4a503601af052.log, unit-912ab643330b35bd0b6b.log)
  TestResolveTemplateImplicitAgentStartsWithSelectedWorkerPrompt PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-fd986cf5bab8709b8d45.log)
  TestStartupPromptContractImplicitWorkerResumeGetsSelectedWorkerPrompt PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-3d6d3036a2f5902472b5.log)
  TestInjectImplicitAgents_NoPromptTemplateWhenCoreComposed PASS (unit-4bbf1267e0c4ccc54042.log)
```

policy_lane: PASS — make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary check-hooks; explicit nogo/lint; thirteen policy/docsync targets.
drift_lane: PASS — make bazel-sync followed by git diff --exit-code and empty porcelain; genspec/schema/client/SPA/lockfile sync targets; openapi-breaking-check against the pinned gate base.
fast_lane: PASS 2026-11-22T17:11:46.722071+00:00 merge_head=08e9c03e7f8e204f8f511616448d4ce7eb191909.
known_red_precheck: known-red-precheck: target=check-routed-test-rows verdict=green merge=0 base=- (it passes on the merge tree); known-red-precheck: stale: tracker=ga-1ptv4jey `make check-routed-test-rows` passes on this merge tree; the tracker may be ready to close; known-red-precheck: proceed reason=nothing-known-red (check-routed-test-rows=green).
format_lane: PASS — //:go_format_test. All lanes ran in separate fresh views before the suite. Strict storeclass predicates used trusted base tools and passed.
complexity: zero-growth form (ii); base and merge both exit 2, no merge row worse than its base row, no new/regressed/deleted threshold changes, baseline untouched. Raw outputs retained; this is the documented scoring rule, no waived failure.
ci_lane_run: n/a (no CI-config change).

skip_justification: all 319 printed skip sites are UNCHANGED with line_in_hunk=no; all six owned tests and their subtests PASS. SHARED-HUNKS: prime_manual_provider_session_test.go has two doc comments and the graph heading constant used only by the two owned tests; startup_prompt_contract_test.go adds io. Actual test-binary import closures require base measurements for cmd/gc. Production prompt derivation feeds none of the platform/lane/missing-fixture/provider skip predicates. Every selected base skip reason matches, including the four simulator names described below.

Supplemental baseline limitation: the base-only //cmd/gc:gc_sim_exec_test invocation exits 3 with TIMEOUT in an unselected simulation case; it was never rerun or called passing. All four selected required names reached terminal SKIP with unchanged printed reasons before that timeout, and the comparator re-read them as 4/4 pre-existing. This by-name measurement uses the retained --test_output=all console with only C0 framing removed; raw/normalized hashes and original BEP are bound in 1-unit-selected-skip-proof.json. CAS materialization was unavailable for that supplemental baseline, so no CAS verification is claimed for it. The other two remote comparison groups and native group exit 0. The canonical gated-head full suite is independent, complete and PASS; no failure attribution or waiver is used.

```text
COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a11, head rbe-a02)
COMPARE-SUMMARY head_skips=68 judged=68 pre-existing=68 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=4 judged=4 pre-existing=4 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=189 judged=189 pre-existing=189 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=36 judged=36 pre-existing=36 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
```

Native environment: rootless Podman/Dolt image and sweep preflight passed; ICU 74 recorded libraries verdict=OK.

Load observations (wait_* and run_* are separate; timed-out ordinary waits are recorded):

```text
load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
run_start_load: 25.74
run_max_load: 34.69
run_mean_load: 22.63
run_readings: 102
storeclass-proof/wrapper.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=661 wait_timed_out=0 run_start_load=14.74 run_max_load=25.94 run_mean_load=20.43 run_readings=5 wait_first_load=16.13 wait_max_load=16.39 wait_mean_load=15.65 wait_readings=23 read_errors=0
c3-run/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=25.74 run_max_load=34.69 run_mean_load=22.63 run_readings=102 wait_first_load=25.74 wait_max_load=25.74 wait_mean_load=25.74 wait_readings=1 read_errors=0
c3-run/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=15.33 run_max_load=18.51 run_mean_load=17.23 run_readings=8 wait_first_load=18.92 wait_max_load=26.51 wait_mean_load=20.43 wait_readings=61 read_errors=0
```

ci_required_closure: `python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.HB657c a47ce14d1d187fc17fa8f56d12e5d5e59874f655 e9b3e3e96ff4e24c919f582a0e4309861463eb9e fork`; workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs need a status; the rest are rolled up through their needs..

ci_required_accounting:

- runner-policy | LOCAL-PASS | command /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.HB657c -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 --poll-interval 30 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- timeout --signal=TERM --kill-after=10s 1800 python3 /var/tmp/gc-deployer-required-ci-20261122.py job /var/tmp/gc-gate-ga-cwojep57-20j9mt96 runner-policy; exit 0; log /var/tmp/gc-gate-ga-cwojep57-20j9mt96/required-ci/runner-policy.stdout
- changes | LOCAL-PASS | command python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.HB657c a47ce14d1d187fc17fa8f56d12e5d5e59874f655 e9b3e3e96ff4e24c919f582a0e4309861463eb9e fork; exit 0; log /var/tmp/gc-gate-ga-cwojep57-20j9mt96/required-ci/closure.stdout
- credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | LOCAL-PASS | command /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.HB657c -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 --poll-interval 30 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- timeout --signal=TERM --kill-after=10s 1800 python3 /var/tmp/gc-deployer-required-ci-20261122.py job /var/tmp/gc-gate-ga-cwojep57-20j9mt96 pack-gate; exit 0; log /var/tmp/gc-gate-ga-cwojep57-20j9mt96/required-ci/pack-gate.stdout
- windows-lane | LOCAL-PASS | command /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.HB657c -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 --poll-interval 30 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- timeout --signal=TERM --kill-after=10s 1800 python3 /var/tmp/gc-deployer-required-ci-20261122.py job /var/tmp/gc-gate-ga-cwojep57-20j9mt96 windows-lane; exit 0; log /var/tmp/gc-gate-ga-cwojep57-20j9mt96/required-ci/windows-lane.stdout
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner |   windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR

windows_reach: none for each native Windows job, each checked by its own test import closure.
deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none. PR-open PASS does not replace green CI / required or successful deferred jobs on this exact head.

Commands and exit statuses (native floor commands supplement the full suite):

```text
bazel build --keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback --build_event_json_file=/var/tmp/gc-gate-ga-cwojep57-20j9mt96/c6-vet.bep.json //... ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.HB657c -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-cwojep57 --label fast-format --record /var/tmp/gc-gate-ga-cwojep57-20j9mt96/fast-format-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-cwojep57-20j9mt96/fast-lane.sh format ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.HB657c -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-cwojep57 --label fast-policy --record /var/tmp/gc-gate-ga-cwojep57-20j9mt96/fast-policy-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-cwojep57-20j9mt96/fast-lane.sh policy ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.HB657c -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-cwojep57 --label fast-drift --record /var/tmp/gc-gate-ga-cwojep57-20j9mt96/fast-drift-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-cwojep57-20j9mt96/fast-lane.sh drift ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.HB657c -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-cwojep57 --label fast-openapi --record /var/tmp/gc-gate-ga-cwojep57-20j9mt96/fast-openapi-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-cwojep57-20j9mt96/fast-lane.sh openapi ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.HB657c -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-cwojep57 --label fast-complexity --record /var/tmp/gc-gate-ga-cwojep57-20j9mt96/fast-complexity-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-cwojep57-20j9mt96/fast-lane.sh complexity ; exit=0
go build -o /var/tmp/gc-storeclass-trusted-6ezi0b7u/storeclass-baseline-trusted ./analyzers/storeclass/cmd/storeclass-baseline ; exit=0
/var/tmp/gc-storeclass-trusted-6ezi0b7u/storeclass-baseline-trusted -repo /var/tmp/gc-fresh-tree.OZb3sU ; exit=0
/var/tmp/gc-storeclass-trusted-6ezi0b7u/storeclass-baseline-trusted -repo /var/tmp/gc-fresh-tree.OZb3sU -growth-since a47ce14d1d187fc17fa8f56d12e5d5e59874f655 -base-tip a47ce14d1d187fc17fa8f56d12e5d5e59874f655 -gate-only ; exit=0
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-cwojep57-20j9mt96/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-cwojep57-20j9mt96/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-cwojep57-20j9mt96/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-cwojep57-20j9mt96/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-cwojep57-20j9mt96/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
GATE_PHASE phase=unit test_rc=0 predicate_rc=0 start_ms=1795367551896 end_ms=1795367712642
GATE_PHASE phase=acceptance test_rc=0 predicate_rc=0 start_ms=1795367713466 end_ms=1795367864679
GATE_PHASE phase=integration-packages test_rc=0 predicate_rc=0 start_ms=1795367865527 end_ms=1795368118872
GATE_PHASE phase=integration-full test_rc=0 predicate_rc=0 start_ms=1795368120386 end_ms=1795368481140
GATE_PHASE phase=integration-latency test_rc=0 predicate_rc=0 start_ms=1795368482091 end_ms=1795368560298
go test -tags integration -count=1 -v -run '^(TestCollectInputConvoyRootsAgreesWithTheFrozenReference|TestConvoyAutocloseAgreesWithTheFrozenReference|TestMoleculeAutocloseAgreesWithTheFrozenReference|TestReplay|TestSessionEventPumpLiveHerdr|TestSimCoverageGate|TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace|TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace)$' ./cmd/gc ; exit=0
go test -tags integration -count=1 -v -run '^(TestAttachedRigScopeShellToken|TestJsonlExportPushRetryDelayIsLocaleIndependent)$' ./examples/gastown ; exit=0
go test -tags integration -count=1 -v -run '^(TestMetadataEmptyStringClearContract|TestProbeVersionsAgainstHostBinaries)$' ./internal/beads ; exit=0
go test -tags integration -count=1 -v -run '^(TestPinnedBeadsModuleDirResolvesThisBuildsOwnCache)$' ./internal/beads/beadstest ; exit=0
go test -tags integration -count=1 -v -run '^(TestMigrateJournalFileMatchesPinnedBeads)$' ./internal/beads/contract ; exit=0
go test -tags integration -count=1 -v -run '^(TestBrProviderConformance)$' ./internal/beads/exec ; exit=0
go test -tags integration -count=1 -v -run '^(TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint|TestCustomTypesCheck_TableDrift|TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext)$' ./internal/doctor ; exit=0
go test -tags integration -count=1 -v -run '^(TestMCPMailBridgeSourceable|TestMCPMailConformance|TestMCPMailCrossPodNameResolution|TestMCPMailProjectKeyIsolation|TestMCPMailReplyMissingOriginalIsNotFound)$' ./internal/mail/exec ; exit=0
go test -tags integration -count=1 -v -run '^(TestActivityLive|TestHerdrConformance|TestProviderLive|TestProviderLiveClaudeKindPath|TestProviderLiveOccupantSwapKeepsLiveness|TestSessionEventsLive)$' ./internal/runtime/herdr ; exit=0
go test -tags integration -count=1 -v -run '^(TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors)$' ./internal/runtime/runtimetest ; exit=0
go test -tags integration -count=1 -v -run '^(TestGetKeyBinding_CapturesUserBinding|TestGetKeyBinding_SkipsGasTownBindings|TestIsGTBinding_DetectsGasTownBindings|TestSetBindings_PreserveFallbackOnRepeatedCalls)$' ./internal/runtime/tmux ; exit=0
go test -tags integration -count=1 -v -run '^(TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig|TestBdLatestSchemaVersionLeavesNothingInItsTempRoot|TestRealBdProvisionLeavesTheHostHomeAlone)$' ./test/acceptance/helpers ; exit=0
go test -tags integration -count=1 -v -run '^(TestStartDrift_SystemdManaged_RestartsToNewBuildID)$' ./test/integration ; exit=0
make test-fsys-darwin-compile ; exit=0
bash scripts/test-push-gate-lock.sh ; exit=0
bash scripts/test-local-concurrency.sh ; exit=0
python3 .github/workflows/scripts/runner_policy.py
 ; exit=0
for attempt in 1 2 3; do
  if scripts/update-bundled-gastown-pack --check; then
    exit 0
  fi
  if [ "$attempt" = "3" ]; then
    exit 1
  fi
  sleep $((attempt * 10))
done
 ; exit=0
for attempt in 1 2 3; do
  if make test-pack-registry-live; then
    exit 0
  fi
  if [ "$attempt" = "3" ]; then
    exit 1
  fi
  sleep $((attempt * 10))
done
 ; exit=0
GOOS=windows go build $(cat .github/windows-lane/build.txt) ; exit=0
GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt) ; exit=0
GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/... ; exit=0
GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./... ; exit=0
```

Execution metadata and inventories are retained by hash, not pasted into this record:

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-cwojep57-20j9mt96/c3-run/evidence/remote","gate_base":"a47ce14d1d187fc17fa8f56d12e5d5e59874f655","gate_head":"e9b3e3e96ff4e24c919f582a0e4309861463eb9e","admission":{"class":"remote","floor_gib":25.0,"free_gib":58.81378936767578,"waited_seconds":20.004,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"a47ce14d1d187fc17fa8f56d12e5d5e59874f655","head":"e9b3e3e96ff4e24c919f582a0e4309861463eb9e","merge":"08e9c03e7f8e204f8f511616448d4ce7eb191909","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"336ccab1695140ac5b6b7468543beb224dace6e085f0cfc4166446a662739856"},"test_exec_bep":{"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":12622.211,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":75.527,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=64.61 load5=68.91 load15=56.40 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=20s load1=46.27 load5=64.45 load15=55.20 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 36.57 59.82 53.98 15/1034 55888"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38722}},"log_dir":"/var/tmp/gc-gate-ga-cwojep57-20j9mt96/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":217314}}},"test_cmd":{"elided_array":{"items":5,"bytes":2064}},"test_counts":{"PASS":158787,"FAIL":0,"SKIP":618},"diff_tests_executed":"diff_tests_executed:\n  TestBuildDesiredState_ClientProviderChatResumedForRoutedWorkLaunchesWithoutWorkerPersona PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-088cc38d26d094729229.log)\n  TestDoPrimeImplicitProviderAgentMatchesPromptlessPoolAgent PASS (integration-packages-bc482d2ae768251ad47f.log, unit-d9e667428f972d067bd3.log)\n  TestResolveDiscoveredSessionTemplate_ClientProviderChatLaunchesWithoutWorkerPersona PASS (integration-packages-821a75e4a503601af052.log, unit-912ab643330b35bd0b6b.log)\n  TestResolveTemplateImplicitAgentStartsWithSelectedWorkerPrompt PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-fd986cf5bab8709b8d45.log)\n  TestStartupPromptContractImplicitWorkerResumeGetsSelectedWorkerPrompt PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-3d6d3036a2f5902472b5.log)\n  TestInjectImplicitAgents_NoPromptTemplateWhenCoreComposed PASS (unit-4bbf1267e0c4ccc54042.log)\nGATE_TEST_EVIDENCE_RESULT ok: 6 diff-owned tests verified by name\n"}
```

Full execution retained: 6059 lines; /var/tmp/gc-gate-ga-cwojep57-20j9mt96/c3-run/evidence/execution.json; SHA256 fedb22d51aae6371e39d4914d521dfa7c6e2467876790257a7e8940f606f6fe9.
Full skip sites retained: 5429 lines; /var/tmp/gc-gate-ga-cwojep57-20j9mt96/skip-source-audit/evidence.json; SHA256 00a78891bc43cd645319f78b976c3cac8709e361c02c86beb4aa4a9a232f451c.
Full base skip comparisons retained: 244 lines; /var/tmp/gc-gate-ga-cwojep57-20j9mt96/base-skip-evidence/report.json; SHA256 36bc562a41ccd69471cdc0d7f1d393479963cecc219b39b781f6b3e88b72bbf5.
Full selected base simulator skips retained: 50 lines; /var/tmp/gc-gate-ga-cwojep57-20j9mt96/base-skip-evidence/1-unit-selected-skip-proof.json; SHA256 631fe9d34a7b53713db1a35623c3e7f666e99f400528adf0f6a47b3fac873948.
Full skip import reach retained: 920 lines; /var/tmp/gc-gate-ga-cwojep57-20j9mt96/skip-import-closures.json; SHA256 821d078109441fb4bb2442a13010c4934db12e672a17223081e5b024cb677d1a.
Full CI accounting retained: 223 lines; /var/tmp/gc-gate-ga-cwojep57-20j9mt96/required-ci/accounting.json; SHA256 f9884ce6eef74f4d5b1b00b21c45533921b27f1de7c45deb9dd261946c106178.
Full known-red precheck retained: 5 lines; /var/tmp/gc-gate-ga-cwojep57-20j9mt96/known-red-precheck.json; SHA256 bab9e01655a9cd66afa59c6dab6e0e37be9aa1b708b95f4648cb82c51430c4af.
Full runtime preflight retained: 113 lines; /var/tmp/gc-gate-ga-cwojep57-20j9mt96/runtime-preflight.json; SHA256 9fb77cfd4c0f74e64a5f70736f628258f8c7cedfbc3ada9926a17f2668cea593.
Full diff-owned explanation retained: 747 lines; /var/tmp/gc-gate-ga-cwojep57-20j9mt96/merge-owned-explain.txt; SHA256 f3a190670689a94476b3402bc5bca967ed2021598964bdd086144021fedb145a.
Full fast lanes retained: 173 lines; /var/tmp/gc-gate-ga-cwojep57-20j9mt96/fast-result.json; SHA256 53646955a6fe5d476c65d58a60d79fa82898cc8dec2b1b022621c95290c15086.
Full merge build/vet retained: 59 lines; /var/tmp/gc-gate-ga-cwojep57-20j9mt96/c6-audit.json; SHA256 08d2a60fca12c329507f5eb2dbee73ec32e26de8f6d016e13db520ce45e7381b.
Full retained evidence manifest retained: 1705 lines; /var/tmp/gc-gate-ga-cwojep57-20j9mt96/gate-evidence-manifest.sha256; SHA256 23f8f9618ff418011d944d4b2b09fc606d75520a088c3be0e3fc047af1d82c5f.
