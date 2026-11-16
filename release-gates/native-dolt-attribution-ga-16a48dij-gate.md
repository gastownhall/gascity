# native-dolt-attribution-ga-16a48dij — release gate

**Verdict:** **PASS**

Deploy bead: `ga-16a48dij`; mode: remote; base branch: main.
Reviewed source: `c785d4e449a02481392e98f66da3d4df69601386`.
Pinned base: `e9a628c5003026e465327af16fb36017ab84b38f`; gate_base_kind: current-tip.
Tested merge head: `b0927ef8449878dfb5b96bd42121bbe4aa081280`; tree: `7edcd1dec732cb840dbac1a6c9250e442fc039ae`.
merge_state: clean (origin/main@e9a628c5003026e465327af16fb36017ab84b38f; files: none).
merge_validation: MPR merge authority; Gas City main merge queue checks the merged result. A later head change requires a new gate; see deployer/docs/merge-time-guarantee.md, Gap 1.

| # | Result | Evidence |
|---|---|---|
| 1 — Review PASS | PASS | ga-7vsaa10l pass on deploy commit in notes; reviewer added test verified independently |
| 2 — Acceptance criteria | PASS | Code and by-name test evidence below; live observation explicitly deferred where named. |
| 3 — Full suite, policy, skips and CI | PASS | Five fresh uncached remote phases, native floor, owned tests by name, full SKIP baseline proofs, and all six CI leaf accounts verified. |
| 4 — Review severity | PASS | Zero unresolved HIGH findings. |
| 5 — Final branch | PASS | Isolated deploy/ga-16a48dij-gate is cut at the exact reviewed source; only this gate record is added in the commit. Clean working tree is checked after that commit and before push. |
| 6 — Merge build and vet | PASS | Canonical clean merge build and vet BEPs both overallSuccess=true; normal hooks active. |
| 7 — Single theme | PASS | Restore creator/sender attribution compatibility across native and CLI transports, all7 files under internal/beads |

Acceptance checks:

- six original attribution/sender regressions plus creator ambient-open concurrency PASS by name
- creator identity resolution and override; all create doors stamp it; ownership actor unchanged
- sender mirrored into metadata.from on write and sender/metadata fallback on native/BdStore reads
- full unit/acceptance/integration chain independently passed

Post-deployment observation remains deferred, not met or waived: {"criterion":"After deployment verify creator for fresh mail, convoy, formula-dispatched order and sender on new plus equivalent pre-switch mail","baseline":"Empty creator after2026-10-26 switch; old metadata.from mail printed empty From. Named gm-wisp-6r5u46u absent, so use an equivalent pre-switch message.","window_hours":24,"mint":"After PR exists, mint explicit-flags post-merge observation before deploy close. No live creates performed at this gate."}

test_cmd_scope: full-suite; heavy_mode: none; waiver_ref: none (no Gas City waiver path).
test_cmd: `env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-16a48dij /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-validate-ga-16a48dij-s8jofqve/merge --base e9a628c5003026e465327af16fb36017ab84b38f --bead-json /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/bead-criterion3.json --owned /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/owned.tsv --run-dir /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/criterion3-run/evidence -- python3 /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/c3-local-bazel.py`.
Five phases: make -k check; acceptance and solo tests; all integration packages; full integration minus gate-latency-tests.txt; only that latency inventory under a clear farm probe.
All phases force --nocache_test_results and --noremote_local_fallback, first attempts only. No cached test result is credited. test_exec_mode is derived from BEP: remote-exec.
test_log_dir: `/var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/criterion3-run/evidence/remote/logs`; full per-job logs retained.

```text
test_counts: PASS=154760 FAIL=0 SKIP=607
  (top-level result lines, column 0, summed over the 557 logs; a test that several jobs run counts once per job; sub-tests are not counted)
```

diff_tests_executed:

- TestBdIssueToBeadReadsSenderColumn PASS
- TestBeadFromNativeIssueSenderFallsBackToMetadataFrom PASS
- TestMetadataWithFrom PASS
- TestNativeApplyGraphPlanOverCapStampsCreatorAndSender PASS
- TestNativeApplyGraphPlanStampsCreatorAndSenderOnEveryCreate PASS
- TestNativeCreatorIdentityGitUserNameComesBeforeUser PASS
- TestNativeCreatorIdentityResolutionOrder PASS
- TestNativeDoltStoreCreateMirrorsFromIntoMetadata PASS
- TestNativeDoltStoreCreateStampsCreatedByFromBeadsActor PASS
- TestNativeDoltStoreCreatorResolvesAmbientActorAfterOpenWindow PASS
- TestNativeDoltStoreGraphApplyStampsCreatedByFromBeadsActor PASS
- TestNativeDoltStoreReadsSenderFromBdStoreWrittenMetadata PASS
- TestNativeDoltStoreTxCreateStampsCreatedByFromBeadsActor PASS
- TestNativeDoltStoreWithNativeCreatorOverridesAmbient PASS
- TestNativeGuardedItemsStampCreatorAndSenderOnCreates PASS
- TestNativeStampCreatorFillsOnlyAnEmptyCreatedBy PASS

FAST lane: policy/lint, formatting, BUILD drift and generated-file drift all passed before the suite, each in a fresh view. Complexity zero-growth form (ii): base_rc=2, merge_rc=2, byte-identical diagnostics, diff_rc=0; no threshold or policy relaxation.
ci_lane_run: n/a (no CI-config change in this diff).

skip_justification: Every skip is non-owned by test; every printed site is outside a diff hunk. Test-binary import closures were computed for unit/integration/acceptance_a, naming every shared/helper hunk. Every reached original key matches a pinned-base same-name/same-reason comparison in its actual farm tier; native herdr additionally matches the canonical native floor. Unreached keys have no changed helper/package in those closures. Exact original reasons remain in all_sites rows. No owned SKIP is accepted.
All 394 shared-reached original keys have farm-tier baseline proof; all310 printed sites unchanged. Native herdr same-command proof is complete; native SimCoverage same-command proof is complete. Supplemental comparisons are focused attribution evidence, never a substitute for the full suite.

Load observations:

- full-chain: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.95 run_max_load=96.16 run_mean_load=51.16 run_readings=115 wait_first_load=9.95 wait_max_load=9.95 wait_mean_load=9.95 wait_readings=1 read_errors=0`.
- native floor: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=34.14 run_max_load=63.81 run_mean_load=44.57 run_readings=10 wait_first_load=42.90 wait_max_load=95.91 wait_mean_load=66.33 wait_readings=61 read_errors=0`.

ci_required_closure: `python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate-ga-16a48dij-s8jofqve/merge e9a628c5003026e465327af16fb36017ab84b38f c785d4e449a02481392e98f66da3d4df69601386 fork`; .github/workflows/ci.yml blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs need a status; the rest are rolled up through their needs.

ci_required_accounting:

- changes | LOCAL-PASS | {"command":["python3","/home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py","/var/tmp/gc-merge-validate-ga-16a48dij-s8jofqve/merge","e9a628c5003026e465327af16fb36017ab84b38f","c785d4e449a02481392e98f66da3d4df69601386","fork"],"rc":0,"log":"/var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/ci-required/closure-final.txt"}
- runner-policy | LOCAL-PASS | {"command":["python3",".github/workflows/scripts/runner_policy.py"],"env":{"EVENT_NAME":"pull_request","PR_AUTHOR":"quad341","FORCE_BLACKSMITH":"","GITHUB_OUTPUT":"/var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/ci-required/runner-policy-final.out"},"rc":0,"log":"/var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/ci-required/runner-policy-final.log"}
- pack-gate | LOCAL-PASS | {"command":["bash","/var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/ci-required/pack-gate.sh"],"rc":0,"log":"/var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/ci-required/pack-gate/units/gate.w1.stdout.log","wrapper_log":"/var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/ci-required/pack-gate/units/gate.w1.stderr.log","load":"LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=31.55 run_max_load=34.92 run_mean_load=32.91 run_readings=4 wait_first_load=86.74 wait_max_load=86.74 wait_mean_load=44.73 wait_readings=62 read_errors=0 prewait_s=1801"}
- windows-lane | LOCAL-PASS | {"command":["bash","/var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/ci-required/windows-lane-corrected.sh"],"rc":0,"log":"/var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/ci-required/windows-lane-corrected/units/gate.w1.stdout.log","wrapper_log":"/var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/ci-required/windows-lane-corrected/units/gate.w1.stderr.log","load":"LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=71.10 run_max_load=68.96 run_mean_load=65.93 run_readings=4 wait_first_load=19.28 wait_max_load=74.89 wait_mean_load=45.63 wait_readings=62 read_errors=0 prewait_s=1801"}
- credential-provider-windows | DEFERRED-TO-PR-CI | {"reason":"non-linux-runner","windows_reach":"none","own_reach_record":"/var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/ci-required/credential-provider-windows-reach.json"}
- windows-lane-test | DEFERRED-TO-PR-CI | {"reason":"non-linux-runner","windows_reach":"none","own_reach_record":"/var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/ci-required/windows-lane-test-reach.json"}

windows_reach: none for every deferred non-Linux job; each uses its own import-closure proof.
deferred_to_pr_ci: credential-provider-windows (non-linux-runner); windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none. PR-open clearance never replaces green CI / required or the deferred jobs on the PR head.

Execution metadata (large inventories elided; retained pointers below):

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/criterion3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":71.22991180419922,"waited_seconds":0.002,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":415,"remote_executed":415,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":14353.377,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":102.826,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=72.03 load5=114.98 load15=69.26 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=30s load1=43.80 load5=104.03 load15=67.07 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 31.04 96.19 65.45 20/1163 54885"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":39570}},"log_dir":"/var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/criterion3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":503,"environment-absence":90},"top_level_counts_by_reason_class":{"other":245,"environment-absence":43},"tests":{"elided_array":{"items":593,"bytes":218067}}},"test_cmd":{"elided_array":{"items":5,"bytes":2104}},"test_counts":{"PASS":154760,"FAIL":0,"SKIP":607},"diff_tests_executed":"diff_tests_executed:\n  TestBdIssueToBeadReadsSenderColumn PASS (integration-packages-b6a233fef6b5b347dcfa.log, unit-0b74cd4d9bd61207e1fd.log)\n  TestBeadFromNativeIssueSenderFallsBackToMetadataFrom PASS (integration-packages-8272c8197ebafcf5385f.log, unit-ea4511196531bb4b8c55.log)\n  TestMetadataWithFrom PASS (integration-packages-999a89ecc5b3d09abdd4.log, unit-b398920224b769833cd5.log)\n  TestNativeApplyGraphPlanOverCapStampsCreatorAndSender PASS (integration-packages-ceb32a3c147a95796597.log, unit-363a36f2662d541bc75e.log)\n  TestNativeApplyGraphPlanStampsCreatorAndSenderOnEveryCreate PASS (integration-packages-5a4a1a23acc13a6dd98f.log, unit-3ee1ae2eaa26dfd63a88.log)\n  TestNativeCreatorIdentityGitUserNameComesBeforeUser PASS (integration-packages-a1f48694841e1859d61a.log, unit-364160de6f2f808aa619.log)\n  TestNativeCreatorIdentityResolutionOrder PASS (integration-packages-b6a233fef6b5b347dcfa.log, unit-7f2c404ca64cd2da0395.log)\n  TestNativeDoltStoreCreateMirrorsFromIntoMetadata PASS (integration-packages-ad823e3d7eb71467b6af.log)\n  TestNativeDoltStoreCreateStampsCreatedByFromBeadsActor PASS (integration-packages-20cfdbabba8f52b16e0b.log)\n  TestNativeDoltStoreCreatorResolvesAmbientActorAfterOpenWindow PASS (integration-packages-8d820cd4c52192de043e.log, unit-8c9c804909a3bb579837.log)\n  TestNativeDoltStoreGraphApplyStampsCreatedByFromBeadsActor PASS (integration-packages-8d820cd4c52192de043e.log)\n  TestNativeDoltStoreReadsSenderFromBdStoreWrittenMetadata PASS (integration-packages-b6a233fef6b5b347dcfa.log)\n  TestNativeDoltStoreTxCreateStampsCreatedByFromBeadsActor PASS (integration-packages-a9e2a6fbbc2774327994.log)\n  TestNativeDoltStoreWithNativeCreatorOverridesAmbient PASS (integration-packages-ad823e3d7eb71467b6af.log, unit-27371dea3b130a4c231f.log)\n  TestNativeGuardedItemsStampCreatorAndSenderOnCreates PASS (integration-packages-20cfdbabba8f52b16e0b.log, unit-0b74cd4d9bd61207e1fd.log)\n  TestNativeStampCreatorFillsOnlyAnEmptyCreatedBy PASS (integration-packages-86f8e1ae5bca4d4a5e22.log, unit-9a6a09bab414814cab53.log)\nGATE_TEST_EVIDENCE_RESULT ok: 16 diff-owned tests verified by name\n"}
```

Full execution retained: 5952 lines; /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/criterion3-run/evidence/execution.json; SHA256 e3842036aa55a5ae37eb76e2a2b68a1630e6454284017e54aae4457c9175bb86.
Full skip inventory retained: 4746 lines; /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/criterion3-run/evidence/remote/skip-inventory.json; SHA256 12e2fa8654cc8d62308237d6c9bf044ed98a465672391eef53ce443d434d8677.
Full native skip inventory retained: 578 lines; /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/criterion3-native-floor-skip-inventory.json; SHA256 1ede9c42c067868a0ac5e7d6e5d299a2c90910eeb93717d026550cced33b96ef.
Full skip proof closure retained: 121 lines; /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/skip-closure-audit.json; SHA256 b4f800eaa6b334bf4da0df7376779590d5fa9820cfff574b0341fd549d68c82e.
Full printed skip sites retained: 5303 lines; /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/all-skip-site-audit.json; SHA256 1333bb4b1bc3b04a386cf3c94d8961566a7523d432430bc3ee01018c450dc10f.
Full import reach retained: 5988 lines; /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/all-skip-comparison-coverage.json; SHA256 2321d01c61fbf09c8d00ce08c3f9b78684fe8a190d504ea7e54ec7d9943cf86e.
Full CI job accounting retained: 96 lines; /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/ci-required/accounting.json; SHA256 602a03d447aec658d71bdf14f09cb1d8bfd3e0279ba61af2fc1b7da6cd51ec46.
Full FAST lanes retained: 167 lines; /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/fast-result.json; SHA256 4fadacc79a181811ee5dbe08c7f5e31056c56b9fe2ae9d896ff50dde1472121b.
Full merge build and vet retained: 25 lines; /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/c6-result.json; SHA256 7e660cf486e72e1b06d54e373f292614099ee288f57d0b0ffd8bb620ccb194c7.
Full evidence hash manifest retained: 1698 lines; /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/publication-evidence-manifest.sha256; SHA256 034c70bc5474db5cd71d446474e71f017dbc98dd05325b77dd34b1f92c674093.

Publication verification 2026-11-16T02:57:52.206795+00:00:

policy_lane: bash /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/fast-policy.sh — PASS; drift_lane: bash /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/fast-drift.sh and bash /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/fast-generated.sh — PASS. Actual fresh-tree, gate-base, admission, load and isolation argv are retained in fast-result.json.
failure_attribution: none for runtime tests. No owned SKIP/FAIL, no retry/cache credit, no waiver.
reviewer_tests_verified: 5943b93f75842a2858cae221301874253d037d02 -> c785d4e449a02481392e98f66da3d4df69601386 files=internal/beads/native_dolt_store_creator_test.go.
Native SimCoverage baseline comparison: same pinned base, unmodified canonical floor command, same name/reason; compare_rc=0, ALL PRE-EXISTING. Full native baseline proof retained: 45 lines; /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/native-skip-sim-base-result.json; SHA256 3058b0768f6872645a2a40817bfba575b05d0bffc51ee21dc75fcfc9c446ea1a.
Full publication audit retained: 113 lines; /var/tmp/gc-gate-ga-16a48dij-yoq5bjyd/publication-evidence-audit.json; SHA256 e587a426737a02e4dedd0b4cde9bae45370807c851e1c880a0ffaea4a16507f3.
The earlier manifest was captured while the native baseline was pending; its only changed entry was skip-closure-audit.json when that proof completed. All 1657 earlier immutable artifact hashes matched; the final manifest includes the completed native baseline logs and current summary. Earlier manifest remains retained.
