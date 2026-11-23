# Supervisor uninstall HOME guard — release gate

**Verdict:** **PASS**

Deploy bead: `ga-9ve6o39q`; build: `ga-26jr48.1`; review: `ga-dvfd7tcy`.
Reviewed source: `6eaf9194041392857d97ffd0f7bad169d617c347`. Mode: remote; push remote: fork; base branch: main.
Pinned gate base: `a47ce14d1d187fc17fa8f56d12e5d5e59874f655`; gate_base_kind: current-tip.
Tested merge: `04302aa945e34e2d5e19c571d1fa39c18427cb2c`; tree: `e2ee83c6acf6ca0030f574f009882457024ecb26`.
merge_state: clean (origin/main@a47ce14d1d187fc17fa8f56d12e5d5e59874f655; files: none).
merge_validation: MPR owns merging; current-head CI and merge-queue checks validate the result at merge time. A later head requires a new gate. No deployer merge or rebase.

| # | Result | Evidence |
|---|---|---|
| 1 — Review | PASS | Reviewer PASS on the exact resolved source; no carryover or reviewer-added commit. |
| 2 — Acceptance | PASS | HOME mismatch refuses before unit removal with exit 1 and GC_HOME guidance; delegated warning precedes refusal; ordinary delegated uninstall remains covered. Three owned regressions passed by name. |
| 3 — Tests and policy | PASS | Complete five-phase uncached remote suite, native floor, policy/drift lanes, all skip comparisons and six-leaf required-CI accounting below. |
| 4 — Severity | PASS | Zero unresolved HIGH findings in the review. |
| 5 — Clean branch | PASS | Test scratch pristine; deploy branch contains the reviewed source plus this record only. Verified clean after the record commit, before publication. |
| 6 — Merge build/vet | PASS | Exact clean merge tree built and vetted/nogo; both BEPs overallSuccess=true. |
| 7 — Theme | PASS | One supervisor uninstall HOME-override refusal theme, three Go files plus this record. |

The guard shares install/start HOME comparison and runs after the delegation warning and before buildSupervisorServiceData or either platform uninstall. The test-only guard seam keeps normal uninstall tests in disposable HOME directories. GC_SUPERVISOR_ISOLATED_HOME=1 does not exempt uninstall. No API/schema/dashboard/CI/config/dependency change.

test_cmd_scope: full-suite; heavy_mode: none; waiver_ref: none (Gas City has no waiver path).
test_cmd: `env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-9ve6o39q GC_GATE_DELTA=shadow GC_GATE_LOAD_DEFER_MAX=3 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-validate.drNxVE --base a47ce14d1d187fc17fa8f56d12e5d5e59874f655 --bead-json /var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp/c3-bead.json --owned /var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp/owned.tsv --run-dir /var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp/c3-run/evidence`.
Unit command: make -k check (whole-tree bazel test //... plus shell guards); acceptance: bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests; integration packages: bazel test --config=integration //test:integration_packages; full integration and its separated latency leg: //test/integration:integration_test. Exact per-phase flags, commands and BEPs are retained in the execution proof.
Phases: whole-tree unit/check; acceptance; every integration-tagged package; full integration excluding latency inventory; inventory-only latency under a clear farm probe. All force --nocache_test_results and --noremote_local_fallback, first attempts, remote strategy and phase-window start proven from BEP.
test_exec_mode: remote-exec; fallback_reason: none; farm_test_seconds: 13752.974.
test_log_dir: `/var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp/c3-run/evidence/remote/logs` (retained).

```text
test_counts: PASS=158772 FAIL=0 SKIP=618
  (top-level result lines, column 0, summed over the 570 logs; a test that several jobs run counts once per job; sub-tests are not counted)
diff_tests_executed:
  TestDoSupervisorUninstallHomeOverrideKeepsDelegationWarning PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestDoSupervisorUninstallRejectsHomeOverride PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-752a0bf25b15b465517a.log)
  TestSupervisorUninstallWarnsAndSkipsDelegatedUnit PASS (integration-packages-18745ddf7d62465654f5.log, unit-912ab643330b35bd0b6b.log)
```

policy_lane: PASS — make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary check-hooks; explicit nogo/lint; thirteen policy/docsync targets.
drift_lane: PASS — make bazel-sync followed by git diff --exit-code and empty porcelain; genspec/schema/client/SPA/lockfile sync targets; openapi-breaking-check against the pinned gate base.
format_lane: PASS — //:go_format_test. All lanes ran in separate fresh views before the suite. Strict storeclass predicates used trusted base tools and passed.
complexity: zero-growth form (ii); base and merge both exit 2, no merge row worse than its base row, no new/regressed/deleted threshold changes, baseline untouched. Raw outputs retained; this is the documented scoring rule, no waived failure.
ci_lane_run: n/a (no CI-config change).

skip_justification: all 319 printed skip sites are unchanged, including platform/lane, helper-only, missing opt-in fixture/provider and environment-absence conditions. The new uninstall guard runs after the skip predicates; seedSupervisorServiceUnit serves only the new tests. The SHARED-HUNKS entry in cmd/gc/cmd_supervisor_test.go was inspected. Actual test-binary import closures select cmd/gc for same-command base measurement; every selected remote/native skip matches its reason at the pinned base. All environment-absence top-ups and the native floor completed (16 steps, exit 0); none of the owned tests skipped. Focused baseline runs are supplemental skip evidence, never the full-suite substitute.

```text
COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a13, head rbe-a11)
COMPARE-SUMMARY head_skips=68 judged=68 pre-existing=68 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=4 judged=4 pre-existing=4 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=189 judged=189 pre-existing=189 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=36 judged=36 pre-existing=36 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
```

Native environment: rootless Podman/Dolt image and sweep preflight passed; ICU 74 recorded libraries verdict=OK.

Load observations (wait_* and run_* are separate; timed-out ordinary waits are recorded):

```text
c3-run/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=17.69 run_max_load=30.97 run_mean_load=17.62 run_readings=67 wait_first_load=17.69 wait_max_load=17.69 wait_mean_load=17.69 wait_readings=1 read_errors=0
c3-run/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=540 wait_timed_out=0 run_start_load=14.62 run_max_load=30.97 run_mean_load=24.89 run_readings=10 wait_first_load=15.56 wait_max_load=16.80 wait_mean_load=15.53 wait_readings=19 read_errors=0
base-skip-evidence/native-0-cmd-gc.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=15.45 run_max_load=18.20 run_mean_load=16.36 run_readings=3 wait_first_load=20.50 wait_max_load=21.29 wait_mean_load=17.90 wait_readings=61 read_errors=0
```

ci_required_closure: `python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.drNxVE a47ce14d1d187fc17fa8f56d12e5d5e59874f655 6eaf9194041392857d97ffd0f7bad169d617c347 fork`; workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs need a status; the rest are rolled up through their needs..

ci_required_accounting:

- runner-policy | LOCAL-PASS | command /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.drNxVE -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 --poll-interval 30 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- timeout --signal=TERM --kill-after=10s 1800 python3 /var/tmp/gc-deployer-required-ci-20261122.py job /var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp runner-policy; exit 0; log /var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp/required-ci/runner-policy.stdout
- changes | LOCAL-PASS | command python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.drNxVE a47ce14d1d187fc17fa8f56d12e5d5e59874f655 6eaf9194041392857d97ffd0f7bad169d617c347 fork; exit 0; log /var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp/required-ci/closure.stdout
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- windows-lane | LOCAL-PASS | command /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.drNxVE -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 --poll-interval 30 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- timeout --signal=TERM --kill-after=10s 1800 python3 /var/tmp/gc-deployer-required-ci-20261122.py job /var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp windows-lane; exit 0; log /var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp/required-ci/windows-lane.stdout
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner |   windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR

windows_reach: none for each native Windows job, each checked by its own test import closure.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none. PR-open PASS does not replace green CI / required or successful deferred jobs on this exact head.

Execution metadata and inventories are retained by hash, not pasted into this record:

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp/c3-run/evidence/remote","gate_base":"a47ce14d1d187fc17fa8f56d12e5d5e59874f655","gate_head":"6eaf9194041392857d97ffd0f7bad169d617c347","admission":{"class":"remote","floor_gib":25.0,"free_gib":58.7578125,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"a47ce14d1d187fc17fa8f56d12e5d5e59874f655","head":"6eaf9194041392857d97ffd0f7bad169d617c347","merge":"04302aa945e34e2d5e19c571d1fa39c18427cb2c","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"136c80740b483a20b3ebacb2bcd94545dad77fae1e81e661f28b62a86c6e58d3"},"test_exec_bep":{"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":13752.974,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":126.501,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=91.53 load5=103.42 load15=58.77 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=40s load1=49.06 load5=90.98 load15=56.46 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 38.22 82.81 55.16 29/1216 56148"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38722}},"log_dir":"/var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":217314}}},"test_cmd":{"elided_array":{"items":5,"bytes":2064}},"test_counts":{"PASS":158772,"FAIL":0,"SKIP":618},"diff_tests_executed":"diff_tests_executed:\n  TestDoSupervisorUninstallHomeOverrideKeepsDelegationWarning PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-1cc8c8ab55e59a9e03a9.log)\n  TestDoSupervisorUninstallRejectsHomeOverride PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-752a0bf25b15b465517a.log)\n  TestSupervisorUninstallWarnsAndSkipsDelegatedUnit PASS (integration-packages-18745ddf7d62465654f5.log, unit-912ab643330b35bd0b6b.log)\nGATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name\n"}
```

Full execution retained: 6059 lines; /var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp/c3-run/evidence/execution.json; SHA256 e30ece96ecfb5252b0c33b1b5fd3be050b049f61ad53824bbb9599f9e7d303d4.
Full skip sites retained: 5428 lines; /var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp/skip-source-audit/evidence.json; SHA256 3dbaeddb44f295c93abe721fb15cdf9675c5ecf14e31988a480bf0c5647e1c09.
Full skip import reach retained: 920 lines; /var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp/skip-import-closures.json; SHA256 742c0f7a71fd252e128fe3787b7dc9befe1de68dfa1523adae75bf56c4ee5208.
Full base comparisons retained: 241 lines; /var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp/base-skip-evidence/report.json; SHA256 040c396bd69c6e929cc911cf265c44f47fcaae1442402f1320e10d088723168b.
Full CI accounting retained: 173 lines; /var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp/required-ci/accounting.json; SHA256 beb49dc8c5e9762c19d3d0a80da4ab6ff35e26d2585dc4a321d7b6b85b7e74cb.
Full fast lanes retained: 173 lines; /var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp/fast-result.json; SHA256 6a7d5735309d61a35fc7aa11e0c52f21a8abeccda6eaf8647f0c36849fdc90d7.
Full merge build/vet retained: 59 lines; /var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp/c6-audit.json; SHA256 e8705faebc6e53ad7fb3814af7417ad75156951c57dd55906f200b6cda75b7ba.
Full retained evidence manifest retained: 1701 lines; /var/tmp/gc-gate-ga-9ve6o39q-uzhhxddp/gate-evidence-manifest.sha256; SHA256 2e0ae43fdfeb6d30fcb54b3686f5aed2ee0043c587b3ac781f0fd98f5d352072.
