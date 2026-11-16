# Pool-template ready-work discovery release gate

**Verdict:** **PASS**

bead: ga-bpf0s1wa
evaluated_at: 2026-11-16T09:40:42.751929+00:00
deploy_mode: remote
reviewed_commit: 5ce9d163f47dc65bb16634c5941bcc8b4fa20a06
source_tree: f53dbadcb43a562ef9b1a610eb47fa0cb61115a7
tested_merge_commit: 5b8ba0c9d98fb13a7f6665c48fb24fe5c80b56a4
tested_merge_tree: 6e4be3a01092cfb8e197076311811448b403473b
merge_state: clean (origin/main@5e60a76af64cfbc702011218854d070f55c65149)
gate_base_kind: current-tip
gate_base_sha: 5e60a76af64cfbc702011218854d070f55c65149
merge_validation: the clean pinned merge was built and tested; MPR validates the actual merge result at merge time. The deploy branch preserves the exact reviewed source plus this evidence-only record.
reviewer_tests_verified: dea59cb53f3af0f35bce541192e84c84e0a26db5 -> 5ce9d163f47dc65bb16634c5941bcc8b4fa20a06 files=internal/config/workquery_pool_template_ready_test.go

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Reviewer ga-iboeaxe records PASS at the exact reviewed commit. The reviewer-added test commit was independently accepted by reviewer-added-tests.sh for ga-oe9huk.1 and ga-8vz95k.1. |
| 2 | PASS | Running configured ephemeral pool members discover template-assigned ready work after concrete identities and active workflow anchors; existing hold exclusions apply. Custom work queries remain verbatim. All five owned regression roots and their subtests PASS. |
| 3 | PASS | Five fresh remote phases, all 16 native-floor steps, by-name owned-test proof, every SKIP obligation, format/policy/drift/complexity and six-leaf required-CI accounting complete. Deferred native Windows jobs require PR CI before merge. |
| 4 | PASS | Zero unresolved HIGH style/security/spec findings. |
| 5 | PASS | Pristine canonical merged scratch verified before publication. The final isolated branch carries only reviewed source and this committed gate record; publication guards recheck clean status. |
| 6 | PASS | Clean pinned merge checked first. Normal hook materialization, full Bazel build and nogo/vet passed; raw build/vet BEPs end SUCCESS. |
| 7 | PASS | One ready-work discovery theme: internal/config implementation, regression tests and generated BUILD entry. No unrelated theme or internal documentation contamination. |

Delivery obligation: after the replacement PR exists at the verified head, close our predecessor #5627 as superseded and verify CLOSED. The predecessor hold is unchanged. The merge-request is withheld until this delivery step succeeds.

test_cmd_scope: full-suite
heavy_mode: none (canonical classifier)
waiver_ref: none
failure_attribution: none
ci_lane_run: n/a (no CI-config diff)
test_counts: 155070 PASS / 0 FAIL / 607 SKIP (column-zero result lines across original source logs, not unique tests).
skip_event_inventory: 593 remote + 72 native = 665 including indented subtests; all are explained. Including subtests: 155434 PASS / 0 FAIL / 665 SKIP.
test_log_dir: /var/tmp/gc-gate-ga-bpf0s1wa-fresh-bn03e2ga/criterion3-run/evidence/remote
log_deduplication: 541 original action logs + 16 original floor logs; canonical copied native aliases excluded. Rules_go transport marker 0x16 stripped only while parsing, raw artifacts retained.
policy_lane: make check-hooks fmt-check lint, four shell guards and ten static Bazel policy targets PASS; 3/10 executed, static cache credit disclosed.
generated_drift_lane: make bazel-sync produces no tracked/untracked drift; eight generated/docs targets PASS, 4/8 executed. Each static lane runs in its own fresh-tree view via pinned gate-base.sh.
complexity: form(ii) zero growth; pinned-base and merge global threshold reports are identical (both rc2), complexity-diff rc0. Tracker ga-q7p6is is closed; no new/regressed/deleted violations. This is static-policy evidence only.
known_red_precheck: first detached call lost GC_RIG and returned rig-unknown/allowed could-not-look continuation; corrected explicit GC_RIG=gascity check returned nothing-known-red and check-routed-test-rows green. Both raw logs retained; no verified precheck is claimed for the initial call.

Canonical execution (large arrays replaced with retained-evidence pointers):
```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-bpf0s1wa-fresh-bn03e2ga/criterion3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":65.54497146606445,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":415,"remote_executed":415,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":13068.315,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":74.426,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=70.79 load5=73.25 load15=38.96 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=30s load1=42.90 load5=66.25 load15=37.72 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 30.22 60.91 36.85 13/1013 54649"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":40206}},"log_dir":"/var/tmp/gc-gate-ga-bpf0s1wa-fresh-bn03e2ga/criterion3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":503,"environment-absence":90},"top_level_counts_by_reason_class":{"other":245,"environment-absence":43},"tests":{"elided_array":{"items":593,"bytes":221525}}},"test_cmd":{"elided_array":{"items":5,"bytes":2134}},"test_counts":{"PASS":155070,"FAIL":0,"SKIP":607},"diff_tests_executed":"diff_tests_executed:\n  TestPoolMemberAnchoredWorkflowPrecedesTemplateAssignedReadyWork PASS (unit-4bbf1267e0c4ccc54042.log)\n  TestPoolMemberConcreteReadyIdentityPrecedesTemplate PASS (unit-4bbf1267e0c4ccc54042.log)\n  TestPoolMemberCustomWorkQueryRemainsVerbatim PASS (unit-4bbf1267e0c4ccc54042.log)\n  TestPoolMemberDefaultQueriesDiscoverTemplateAssignedReadyWork PASS (unit-4bbf1267e0c4ccc54042.log)\n  TestTemplateAssignedReadyFallbackRequiresConfiguredEphemeralPoolMembership PASS (unit-4bbf1267e0c4ccc54042.log)\nGATE_TEST_EVIDENCE_RESULT ok: 5 diff-owned tests verified by name\n"}
```

All five BEPs independently prove remote strategy, no local/remote cache credit, attempt1, phase-window start and terminal verdict for each result: unit415, acceptance25, integration-packages86, integration-full14, integration-latency1; total541 actions.
fallback_reason: none; no local restart or stitched suite
floor_topup: all16 steps rc0, including every environment-absence package/parent selection, beadstest, Darwin compile and both runner self-tests.
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.49 run_max_load=10.52 run_mean_load=10.03 run_readings=6 wait_first_load=9.49 wait_max_load=9.49 wait_mean_load=9.49 wait_readings=1 read_errors=0
latency_leg: measured, first attempt; clear farm probe after30s (load1 42.90, threshold50). Both canonical latency roots PASS. Main integration excludes them.
environment: ICU74 loader check OK; canonical podman socket and pinned Dolt2.6.0 image verified before runtime; testcontainer-sweep recent. No per-action library path or test-environment workaround.

diff_tests_executed:
```text
internal/config	TestPoolMemberAnchoredWorkflowPrecedesTemplateAssignedReadyWork	PASS
internal/config	TestPoolMemberConcreteReadyIdentityPrecedesTemplate	PASS
internal/config	TestPoolMemberCustomWorkQueryRemainsVerbatim	PASS
internal/config	TestPoolMemberDefaultQueriesDiscoverTemplateAssignedReadyWork	PASS
internal/config	TestTemplateAssignedReadyFallbackRequiresConfiguredEphemeralPoolMembership	PASS
```

skip_justification:
All319 candidate printed sites are mechanically UNCHANGED; zero unresolved/touched sites. All30 shared conformance events resolve to the unchanged fencedStart helper and identical source blobs (t.Helper reports callers). Every raw reason is independently tied to its exact Go test context.
664/665 observations compare pre-existing by phase/package/name/reason with fresh pinned-base evidence, including the complete native floor. The sole SHARED-HUNKS entry is internal/config/workquery_pool_template_ready_test.go:1 (new test helpers); the non-owned TestLoadWithIncludes_PatchTargetingImplicitAgentErrorsToday skip in its package has the same pinned-base reason. No owned test/subtest skipped; no HELPER-HUNKS elsewhere.
The remaining unit event is internal/nudgepoller TestIsGoTestExecutableRecognizesThisBinaryWhateverItIsCalled. Both runs report an invalid cross-device hard link, with distinct action UUID/temp paths. The optional literal comparator exits1/reason-changed; it is retained unchanged and never called PASS. The whole test file, exact skip site and actual Linux/default-tag test-binary import closure are independently checked: no changed non-test input, SHARED-HUNKS or HELPER-HUNKS reaches this test, so the protocol does not require a baseline comparison. Exact raw reasons/source/import proof are retained in nudgepoller-skip-mechanism-proof.json.
Other reasons are unchanged platform/tool/privilege/lane/opt-in constraints, provider capability/fixture cases and characterization skips. Environment-absence selections receive canonical native-floor execution; remaining non-owned top-up SKIPs are inventoried. No failure, waiver or retry is excused.

ci_required_closure: ["python3", "/home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py", "/var/tmp/gc-gate-ga-bpf0s1wa-fresh-bn03e2ga/merge", "5e60a76af64cfbc702011218854d070f55c65149", "5ce9d163f47dc65bb16634c5941bcc8b4fa20a06", "fork"]
workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612
leaf_count: 6 LEAF jobs need a status; the rest are rolled up through their needs.
ci_required_accounting:
- runner-policy | LOCAL-PASS | exit0; /var/tmp/gc-gate-ga-bpf0s1wa-fresh-bn03e2ga/ci-required/runner-policy.log; exact command, environment and wrapper argv retained in ci-required/accounting.json and ci-independent-audit.json.
- changes | LOCAL-PASS | exit0; /var/tmp/gc-gate-ga-bpf0s1wa-fresh-bn03e2ga/ci-required/triggers.txt; exact command, environment and wrapper argv retained in ci-required/accounting.json and ci-independent-audit.json.
- credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'; own native import closure reach=none
- pack-gate | LOCAL-PASS | exit0; /var/tmp/gc-gate-ga-bpf0s1wa-fresh-bn03e2ga/ci-required/pack-gate.log; exact command, environment and wrapper argv retained in ci-required/accounting.json and ci-independent-audit.json.
- windows-lane | LOCAL-PASS | exit0; /var/tmp/gc-gate-ga-bpf0s1wa-fresh-bn03e2ga/ci-required/windows-lane.log; exact command, environment and wrapper argv retained in ci-required/accounting.json and ci-independent-audit.json.
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner |   windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR; own native import closure reach=none
windows_reach: none independently for credential-provider-windows and windows-lane-test.
deferred_to_pr_ci: ["credential-provider-windows (non-linux-runner)", "windows-lane-test (non-linux-runner)"]
not_covered: none
host_gaps: none
Local CI jobs ran sequentially, detached through fresh-tree/load/isolation wrappers. windows-lane ran the workflow exact Windows/Darwin cross-build and compile-only steps, pack-gate both live steps, runner-policy the actual quad341 fork-PR inputs. Full commands/exit codes/log/load records retained.
Before merge: CI / required must be green on the exact PR head, and both deferred Windows jobs success (or skipped solely because their own path filter is false). Gate PASS authorizes PR opening; MPR owns the merge.

Retained evidence root: /var/tmp/gc-gate-ga-bpf0s1wa-fresh-bn03e2ga
Evidence hash manifest: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/retained-evidence.sha256
Full context.json retained: 25 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/context.json; SHA256 93ecf717ad163fe594d5a27d83dce5fbe47f1fb6004bf04362b5a465f0304858.
Full c6-independent-audit.json retained: 33 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/c6-independent-audit.json; SHA256 83ad9d45b1b94f07b0d9e177356624b31894e9a57f33d13eb700f8360d21d212.
Full c6-result.json retained: 25 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/c6-result.json; SHA256 9b45c4db16f815792a2f40b9adbb032a33ab4999c1029415a54d690d48727bb4.
Full review-acceptance-static-audit.json retained: 30 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/review-acceptance-static-audit.json; SHA256 d13f06034e4e16b1df3d547e3fe9a8ac8d97b306c8f81a20121b2874eeb88246.
Full fast-independent-audit.json retained: 45 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/fast-independent-audit.json; SHA256 2e55195fc8ebdc68f6aebc114dffd8aacb8598960e276033c89a39063d64fc53.
Full complexity-results.json retained: 5 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/complexity-results.json; SHA256 f95c6eb46b4b4bd90c31e95ff8bb296f71c8f8f0febf64fc73c40c5d577299e8.
Full complexity-scoring.json retained: 16 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/complexity-scoring.json; SHA256 7d6f7278af6d92d94814e0942413f2f56cb4d3b2dbd69654110de4912eac2408.
Full remote-phases-independent-audit.json retained: 243 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/remote-phases-independent-audit.json; SHA256 5afb68c059acf0f0d2ca12e94879e8e18fee420134bc1219e4a750042e202167.
Full runtime-skip-independent-audit.json retained: 96 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/runtime-skip-independent-audit.json; SHA256 a5ba47878d831d290d70e81c203fc0f0da302e8b8f0c8c298c14acbd250725bc.
Full independent-skip-reason-audit.json retained: 8655 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/independent-skip-reason-audit.json; SHA256 6351611895e617c66f93f47c515bdd8399928afd0c56ecef1429cf88c1c63015.
Full source-log-manifest.json retained: 573 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/source-log-manifest.json; SHA256 5b9bf10860dd1d728d2000d9b89dd0ca468ca73f782e9ec201576046dca0fbfb.
Full ci-independent-audit.json retained: 58 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/ci-independent-audit.json; SHA256 886dcdf23e0613ee2b48a8e1caec6d5c7b163f67e0448e15ae5a91d6a95551be.
Full ci-required/accounting.json retained: 150 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/ci-required/accounting.json; SHA256 0e87e6bd94a4c56a2bf3999323699096f3228b4c55393a16782387f52807488d.
Full complete-skip-audit-plan.json retained: 6637 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/complete-skip-audit-plan.json; SHA256 74a44df89251259d8f354336e19bd69f5532412a2e9873455f93e742ae092431.
Full complete-skip-sites.txt retained: 319 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/complete-skip-sites.txt; SHA256 d955dbb529a9626de07213ab1a27b012adf713d5ca26d7db5b2f4f16a134f5bb.
Full skip-shared-source-resolution.json retained: 397 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/skip-shared-source-resolution.json; SHA256 776888f674e09b2c0cff0da9f8dc21d91767e237d1867ab4a19d9155ce853d94.
Full skip-helper-site.txt retained: 1 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/skip-helper-site.txt; SHA256 9841bf82e4c7ae0056b91b5e30033ac22f2ffa302639a2263b6901e1a6efdb17.
Full skip-comparison-audit.json retained: 2524 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/skip-comparison-audit.json; SHA256 481b5301bdca3736c782c7174704dd386c7d6930e4ec2b737e5310aee0464b26.
Full nudgepoller-skip-mechanism-proof.json retained: 45 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/nudgepoller-skip-mechanism-proof.json; SHA256 545fc46c7d5218bf19d0fcf893400b270c386a85d51f21382c00394ab83254d2.
Full nudgepoller-skip-import-closure.txt retained: 6 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/nudgepoller-skip-import-closure.txt; SHA256 1f16fa08b1f522e7b0d2fa7293da70cbd33a84fe1ebc07e5b49e5b02c8adc226.
Full owned.tsv retained: 5 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/owned.tsv; SHA256 789148c1f0ccefe7205cc37532be6a84b1c2937ec695e2e6b968b6d1579a862f.
Full owned-explain.txt retained: 21 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/owned-explain.txt; SHA256 efc37551114e95b320f1053386f87f1bd0131770e31769de38712e6343517cbd.
Full merged-owned.tsv retained: 5 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/merged-owned.tsv; SHA256 789148c1f0ccefe7205cc37532be6a84b1c2937ec695e2e6b968b6d1579a862f.
Full skip-base-result.json retained: 568 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/skip-base-result.json; SHA256 58b111ed6b222fbb7ae139f662e024f7da9ffc959c1909166e3690abe35feaad.
Full runtime-tooling-manifest.json retained: 16 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/runtime-tooling-manifest.json; SHA256 6a03d5bd8f286e454e78c510be099eb5fbe2a36cf3be7cb2aab612f9707cdd6b.
Full known-red-precheck.log retained: 1 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/known-red-precheck.log; SHA256 aafbc31e7a03b64c7db3dba530dd41616afb53aa17317d6349327202a46d3c7b.
Full known-red-precheck-corrected.log retained: 3 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bpf0s1wa/gate-pass-20261116T094042Z/proofs/known-red-precheck-corrected.log; SHA256 48bca42e947c38bee3209d91ce78f7a1a93ddda70a2e77e66768e62fb5cd7aa2.
Full full execution.json retained: 5952 lines; /var/tmp/gc-gate-ga-bpf0s1wa-fresh-bn03e2ga/criterion3-run/evidence/execution.json; SHA256 5c7ade3359e96106e526b611a7de5455a507756a5ac96b170f5318b262c941ac.
