# Release gate: rig-root closed-bead worktrees (ga-49p2mjd6)

**Verdict:** **PASS**

Date: 2026-11-24T16:01:33.374913+00:00
Reviewed source: 5a534df41021ea5e69b1c4086585e42b9f31edf7
Pinned base: origin/main@48c8cd6313d9e3b8978eb60702afff61c921cc1b
Validated merge: ca1c2eba96ab103b955e5e5c367889e30b8b6cfc
Validated tree: a1e1742621fba0a952491adc1cc7f96c25b30b09
Pack: 56716f5eb435b3c9698047f4e79996d115193d2b
Deploy mode: remote; push remote: fork; review ga-4uaudr3r; build ga-4wem3l.

| # | Criterion | Verdict and evidence |
|---|---|---|
| 1 | Review PASS | PASS. Reviewer ga-4uaudr3r records PASS; reviewer-added test integrity independently verified. |
| 2 | Acceptance | PASS. Only the existing city root and rig/worktrees are accepted, each with its own containment boundary. Bead-ID extraction uses the matched root. Existing agent-home, closed-bead, quarantine, active-borrower, liveness and Git-state gates stay in the same path. Default remains off; dry-run preserves files. Seven new tests pass by name, including the symlink escape boundary. |
| 3 | Tests/policy | PASS. Full five-phase remote suite; all native floor steps; full fast lane and BUILD drift; required-CI and SKIP accounting below. No waiver or attributed failure. |
| 4 | HIGH findings | PASS. Zero unresolved HIGH findings. Doctor hint wording is a minor follow-up (ga-0vzruew6). |
| 5 | Clean branch | PASS. Reviewed source and validated tree are clean. Only this record is added to the isolated deploy branch; final clean status is checked before push. |
| 6 | Merge/build | PASS. Clean scratch merge; full Bazel build and full nogo succeed. |
| 7 | One theme | PASS. One worktree-reaper scope change, its tests and BUILD registration; three source files. |

**Reviewer-added tests:** `4155fa763cf4d2e36759fe811b4d77e25401027f` -> `5a534df41021ea5e69b1c4086585e42b9f31edf7` files=cmd/gc/bead_worktree_reaper_rig_worktrees_test.go — the reviewed commit plus the reviewer's own test files; the deployer re-verifies it (criterion 1)
reviewer_tests_verified: reviewer-added-tests.sh check with build bead ga-4wem3l returned reviewer-tests and the same file. Ancestry scope accepts ga-49p2mjd6 and ga-4wem3l; no stack.

merge_state: clean (origin/main@48c8cd6313d9e3b8978eb60702afff61c921cc1b)
gate_base_kind: current-tip
gate_base: 48c8cd6313d9e3b8978eb60702afff61c921cc1b
gate_head: 5a534df41021ea5e69b1c4086585e42b9f31edf7
merge_validation: the merge authority settles freshness/conflicts and tests the final merge result; this record covers the pinned merge above. Scratch commit was made with hooks off.

Criterion 6 command, exit 0 (1,500 targets):
```text
env ISOLATED_TEST_BEAD=ga-49p2mjd6 /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bazel build --config=remote-exec --noremote_local_fallback --repo_env=DO_NOT_TRACK=1 //...
```
Full c6.output retained: 150 lines; /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/c6.output; SHA256 56fe1dcb0f036722f1e505f0471209b0219b18b32f6c9a8d62f7fa66ce036cee.

Fast lane PASS: make check-hooks; //:go_format_test; make lint (full nogo); make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary; make bazel-sync then git diff --exit-code. Each writing/walking lane ran in its own fresh merge view. An initial nogo setup lacked the ignored local RBE rc and was stopped before a verdict; the successful continuation generated and checked that rc inside each fresh view. No failing test was retried.
Full fast-continuation.output retained: 163 lines; /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/fast-continuation.output; SHA256 2c6539d0fcfa1d6382fc9e9fd8109b77703e797b09ab578246a526e7291c0826.

Test environment: rootless Podman socket probe PASS; bd v1.3.4 pinned by the runner; Ryuk disabled with the paired unreaped-container acknowledgement. ICU loader evidence:
```text
GATE_ICU74 lib=libicui18n.so.74 file="/usr/local/lib64/icu74-compat/libicui18n.so.74.2" sha256=d90d62f0d7b6c6fbc279ae5e433cc7004aa47cdb106a71a02350099f8df9ae51 recorded=yes
GATE_ICU74 lib=libicuuc.so.74 file="/usr/local/lib64/icu74-compat/libicuuc.so.74.2" sha256=33499dd97a5393926b32e0b535f5340f7dae14b35e59025b572e6bd9ad48a2ae recorded=yes
GATE_ICU74 lib=libicudata.so.74 file="/usr/local/lib64/icu74-compat/libicudata.so.74.2" sha256=c891c5fdb45e3cf756b728abe256be7af1bfc97f00ca2f2fac450c96c09bd4b7 recorded=yes
GATE_ICU74 verdict=OK provenance=recorded sonames="libicui18n.so.74 libicuuc.so.74 libicudata.so.74"
```
heavy_mode: none. known-red precheck: proceed (nothing-known-red). Own-code provenance, remote enabled. Delta shadow computed full scope (no prior gate PR), without narrowing.

Criterion 3 command, exit 0:
```text
env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-49p2mjd6 GC_GATE_DELTA=shadow GC_GATE_LOAD_DEFER_MAX=3 DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-ga-49p2mjd6.WxcDnl --base 48c8cd6313d9e3b8978eb60702afff61c921cc1b --bead-json /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/bead-c3.json --owned /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/owned.tsv --run-dir /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/criterion3/evidence
```
test_exec_mode: remote-exec
test_cmd_scope: full-suite
BEP: 554/554 fresh remote results, all attempt 1, inside phase windows, neither cache flag set and no local fallback. Unit 309/309 targets; acceptance 8/8; integration-packages 38/38; full integration 15 shards. Latency phase passed on attempt 1 under a clear farm probe. All 16 mandatory native floor steps exit 0. owned_topup: none; all owned tests executed on the farm.

Retained-log reader output (top-level events, repeated tests counted per job; includes native floor logs):
```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/criterion3/evidence/remote/logs logs=570 logs_with_results=502
test_counts: PASS=159083 FAIL=0 SKIP=618
  (top-level result lines, column 0, summed over the 570 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0eded8e058b22af3c47d.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log (+56 more)
diff_tests_executed:
  TestReapClosedBeadWorktrees_OnlyRigWorktreesDirIsWidened PASS (integration-packages-074fc6c34a1fb2577127.log, unit-912ab643330b35bd0b6b.log)
  TestReapClosedBeadWorktrees_ReapsIdleRigWorktreesDirWorktree PASS (integration-packages-9326181a736eb30b71ca.log, unit-fd986cf5bab8709b8d45.log)
  TestReapClosedBeadWorktrees_RigWorktreesDirDryRunRemovesNothing PASS (integration-packages-821a75e4a503601af052.log, unit-abd97d2cb9b450225d57.log)
  TestReapClosedBeadWorktrees_RigWorktreesDirKeepsEverySafetyGate PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-752a0bf25b15b465517a.log)
  TestReapClosedBeadWorktrees_RigWorktreesDirResolvesParentNamedBead PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-f93a0ca96ccc61d0cae9.log)
  TestReapClosedBeadWorktrees_RigWorktreesDirSkipsOpenBead PASS (integration-packages-e4245ddf6a37030111bc.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestReapClosedBeadWorktrees_RigWorktreesDirSymlinkedOutsideRigIsNotScanned PASS (integration-packages-75af79934639abaae69c.log, unit-088cc38d26d094729229.log)
GATE_TEST_EVIDENCE_RESULT ok: 7 diff-owned tests verified by name
```
test_log_dir: /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/criterion3/evidence/remote/logs

SKIP justification: 604 farm SKIP rows, 319 resolved reported sites, all unchanged against the actual validated merge; zero unresolved sites. Ninety environment-absence rows were selected for native top-up. The native floor has 72 SKIP events across 45 unchanged sites. Reasons are unchanged OS/service, topology or optional provider/infrastructure preconditions. No diff-owned test skipped. The two new test helpers are private to the added file and only its seven tests call them. Required same-package baseline checks prove 261 farm cmd/gc SKIPs and 36 native cmd/gc SKIPs pre-existing with the same reasons (one official farm account-only normalization). Supplemental helper/interleaving sites are retained.
```text
COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a13, head rbe-a06)
COMPARE-SUMMARY head_skips=68 judged=68 pre-existing=68 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=4 judged=4 pre-existing=4 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=189 judged=189 pre-existing=189 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=36 judged=36 pre-existing=36 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
```

Load telemetry, retained verbatim (ordinary bounded timeout proceeds):
```text
criterion3/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=80.44 run_max_load=79.15 run_mean_load=67.85 run_readings=10 wait_first_load=24.84 wait_max_load=102.80 wait_mean_load=72.50 wait_readings=61 read_errors=0
native-skip-proof.output: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=91.00 run_max_load=91.62 run_mean_load=89.48 run_readings=4 wait_first_load=25.96 wait_max_load=91.00 wait_mean_load=55.73 wait_readings=61 read_errors=0
```

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-ga-49p2mjd6.WxcDnl 48c8cd6313d9e3b8978eb60702afff61c921cc1b 5a534df41021ea5e69b1c4086585e42b9f31edf7 fork; merged ci.yml blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status.
Full ci-required-closure.txt retained: 17 lines; /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/ci-required-closure.txt; SHA256 a5416f11f63ed4cbc984e81b4dc467980347d9fa27492d5636b1b0bd31a5f98e.

ci_required_accounting:

| LEAF job | Status | Evidence |
|---|---|---|
| changes | LOCAL-PASS | ci_required_triggers.py exit 0; /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/ci-required-closure.txt |
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<retained> python3 .github/workflows/scripts/runner_policy.py from merged scratch; exit 0; /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/runner-policy.log |
| credential-provider-windows | NOT-TRIGGERED | Tool: NON-LINUX; NOT-TRIGGERED; needs.changes.outputs.credential_provider == true |
| pack-gate | NOT-TRIGGERED | Tool: linux; NOT-TRIGGERED; needs.changes.outputs.packs == true |
| windows-lane | CI-PASS | https://github.com/gastownhall/gascity/actions/runs/45431464323/job/138577911302; exact reviewed head 5a534df41021ea5e69b1c4086585e42b9f31edf7; all four CI command steps success; per-bead draft order gcg-16090113 |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Own Windows import closure: none, exit 0; /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/native-windows-reach.json. Native Windows success now observed at https://github.com/gastownhall/gascity/actions/runs/45431464323/job/138577911887; remains required on the final PR head. |

windows_reach: none. credential-provider-windows helper exit 0 without output; native windows-lane-test has its own GOOS=windows, GOARCH=amd64 test-import closure, exit 0, ten module directories, no changed path. go.mod/go.sum unchanged.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
This PASS authorizes PR opening. Merge requires CI / required green on the current PR head and deferred native Windows success (or a legitimate path-filter skip). No host-precondition deferral.

Compact execution record:
```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/criterion3/evidence/remote","gate_base":"48c8cd6313d9e3b8978eb60702afff61c921cc1b","gate_head":"5a534df41021ea5e69b1c4086585e42b9f31edf7","admission":{"class":"remote","floor_gib":25.0,"free_gib":51.351627349853516,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"48c8cd6313d9e3b8978eb60702afff61c921cc1b","head":"5a534df41021ea5e69b1c4086585e42b9f31edf7","merge":"ca1c2eba96ab103b955e5e5c367889e30b8b6cfc","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"a0afc8149a7a8287ea0e3c580cb5c220e2ddbdc0f08fe9d4aeeb9fc0047a339d"},"test_exec_bep":{"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":15557.025,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":74.326,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=18.32 load5=16.33 load15=17.66 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 22.87 17.70 18.05 9/1087 56938"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":40100}},"log_dir":"/var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/criterion3/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":225165}}},"test_cmd":{"elided_array":{"items":5,"bytes":2129}},"test_counts":{"PASS":159083,"FAIL":0,"SKIP":618},"diff_tests_executed":"diff_tests_executed:\n  TestReapClosedBeadWorktrees_OnlyRigWorktreesDirIsWidened PASS (integration-packages-074fc6c34a1fb2577127.log, unit-912ab643330b35bd0b6b.log)\n  TestReapClosedBeadWorktrees_ReapsIdleRigWorktreesDirWorktree PASS (integration-packages-9326181a736eb30b71ca.log, unit-fd986cf5bab8709b8d45.log)\n  TestReapClosedBeadWorktrees_RigWorktreesDirDryRunRemovesNothing PASS (integration-packages-821a75e4a503601af052.log, unit-abd97d2cb9b450225d57.log)\n  TestReapClosedBeadWorktrees_RigWorktreesDirKeepsEverySafetyGate PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-752a0bf25b15b465517a.log)\n  TestReapClosedBeadWorktrees_RigWorktreesDirResolvesParentNamedBead PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-f93a0ca96ccc61d0cae9.log)\n  TestReapClosedBeadWorktrees_RigWorktreesDirSkipsOpenBead PASS (integration-packages-e4245ddf6a37030111bc.log, unit-1cc8c8ab55e59a9e03a9.log)\n  TestReapClosedBeadWorktrees_RigWorktreesDirSymlinkedOutsideRigIsNotScanned PASS (integration-packages-75af79934639abaae69c.log, unit-088cc38d26d094729229.log)\nGATE_TEST_EVIDENCE_RESULT ok: 7 diff-owned tests verified by name\n"}
```
Full criterion3/evidence/execution.json retained: 6058 lines; /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/criterion3/evidence/execution.json; SHA256 7bd568c6accae03793ad6683f9eff19e692d1aea48caa0f859c9405a3b4c5c16.

Evidence and raw per-job logs remain under /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo. No raw test log was removed.
Full farm-skip-site-index.json retained: 6646 lines; /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/farm-skip-site-index.json; SHA256 8bf6a173fd8e4a5726c9167373648b4771a70f2fa018687f7cf41a7fc6a11120.
Full skip-site-proofs-merged.jsonl retained: 275 lines; /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/skip-site-proofs-merged.jsonl; SHA256 6d7a7a37646c5a5d8a5ee0fbeacc1986d4d5fc0241161ea392f6c2f212edea9b.
Full skip-site-supplements.jsonl retained: 5 lines; /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/skip-site-supplements.jsonl; SHA256 775484af28e98d6f808c20d09bffe7d9ca70e2670585703bb4583990b7d9f37d.
Full farm-extra-site-proofs.jsonl retained: 43 lines; /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/farm-extra-site-proofs.jsonl; SHA256 71b894fb7a41f89e9ab7996b4e6498a2cc39b45e61496d8b4a29536c0c314c31.
Full native-skip-site-index.json retained: 794 lines; /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/native-skip-site-index.json; SHA256 a18dfbff45ffeec56aa30528884f1c803f34c3f5c39ac74298a55fcba91e8627.
Full native-skip-site-proofs.jsonl retained: 45 lines; /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/native-skip-site-proofs.jsonl; SHA256 a4ecd974412a96c7e716322a89c874e7bd55b84fd8f516fba1ce49c593a6506b.
Full native-windows-reach.json retained: 41 lines; /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/native-windows-reach.json; SHA256 7da5726eecc5c1305924bb2e7f8cc1a428484150faa211fc356913d150c7c6a0.
Full native-skip-proof-evidence/result.json retained: 19 lines; /var/tmp/gc-gate.ga-49p2mjd6-20261124.o9f_i5mo/native-skip-proof-evidence/result.json; SHA256 c0bdf1af8a6be405f6cb99e9906263bfa8dc4e8c4884fe1f3e98d7415d554d3f.

Ship coordination: parent ga-twed0v asks for companion formula migration ga-b7i4cn alongside this reactive backstop. It is not bundled here or represented as landed. The merge handoff names this coordination requirement.

Shipping disposition: mayor gcg-16108545 confirms KEEP DRAFT until companion ga-b7i4cn has a gated PR; publish exact-head clearance now, then mark both ready together (or stacked) and notify mayor. Companion refresh ga-db7dcdeq remains open. This gate PASS does not release that ship-together condition.
