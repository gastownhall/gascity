# Pool-session close and actor fallback order release gate

**Verdict:** **PASS**

Deploy bead: ga-fhy1tvm; build: ga-3r34xjw; review: ga-ugtjj8u.
Reviewed source: `170e089e6bb06ed08c94b8aa9876984a9adcff7f`. Provenance only: builder/ga-3r34xjw.
Deploy mode: remote; push remote: fork; isolated branch: deploy/ga-fhy1tvm-gate.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Closed review ga-ugtjj8u explicitly passes the exact reviewed source; no carryover or reviewer-added-test substitution. |
| 2 | Acceptance criteria met | PASS | Five refreshed-source exit clauses verified below against source and fresh canonical test results. |
| 3 | Tests pass | PASS | Full five-phase uncached Bazel gate, mandatory native floor, fast checks and required-job accounting passed; three changed tests PASS by name. No failure attribution or waiver. |
| 4 | No unresolved HIGH findings | PASS | Review security findings: none; no uncovered or descoped criteria. Latent helper observations accepted by reviewer, non-blocking. |
| 5 | Final branch clean | PASS | Reviewed source worktree clean; sole deploy addition is this gate record. Clean final status checked before publication. |
| 6 | Merge state recorded; builds and vets | PASS | Canonical current-main merge independently reconstructed; whole-tree bazel build //... with nogo/vet passed. |
| 7 | Single feature theme | PASS | Two test files cover pool-session close identity and the corresponding opt-in actor fallback guard; no production or CI-config change. |

merge_state: clean (origin/main@d5261665dcd8f12e8a5a7ed4fdf9343f849f8c37)
gate_base_kind: current-tip
gate_base_sha: d5261665dcd8f12e8a5a7ed4fdf9343f849f8c37
gate_merge_head: d2de28734eb60053d401cc27f180cbb4b5f6500e
gate_merge_tree: 9ab0d583b9fcae3c145075eaf3bb3088c352610c
merge_validation: canonical merged result built, vetted and tested; current-head CI and MPR merge-time checks remain required. No deployer rebase.
preflight: authenticated quad341; reviewed commit/pulls returned []; no already-merged target or existing PR.
ancestry_scope: PASS, accepted ga-fhy1tvm / ga-3r34xjw / original work ga-gskond.1; no denylisted paths or unrelated commits; no stack.
manifest: docs/PROJECT_MANIFEST.md absent; canonical commands resolved from AGENTS.md, TESTING.md, Makefile and CI.

Acceptance evidence:

- Refreshed source based on main at build time; fresh merge with pinned current main independently verified by exact parents and merge-tree output.
- Main's identity-independent close behavior preserved. Distinct session-id/name pool row checks the same successful close; actor-order opt-in helper and positive/negative tests carried. Existing rows retained.
- No new exec.Command or resource allocation introduced, so the prior census-only commit is unnecessary. Resource-census policy target passed in the full gate without ledger edits.
- Affected tests ran through Bazel in the full canonical gate, rather than borrowing builder/reviewer or narrowed evidence.
- Conflict-resolution port explicitly accepted in independent review ga-ugtjj8u; successor deploy supersedes already-closed ga-tvvobi.

criterion6_cmd: isolated-test-run.sh --rbe -- bazel build --config=remote-exec --noremote_local_fallback //...; rc=0; /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/c6.log
policy_lane: fresh-tree-run.sh + gate-base.sh run + isolated-test-run.sh --rbe; make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary; make lint NOGO_BUILD_FLAGS='--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback'; rc=0; /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/policy.log
format_lane: fresh-tree-run.sh + isolated-test-run.sh --rbe; bazel test --config=remote-exec --noremote_local_fallback //:go_format_test; rc=0; /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/format.log
drift_lane: fresh-tree-run.sh + isolated-test-run.sh --rbe; make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec then git diff --exit-code; rc=0; /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/drift.log
fast_lane: PASS before runtime setup and suite; canonical scratch unchanged.
hooks: make check-hooks PASS; active pre-commit required on gate commit; pre-push remains active.

criterion3_evidence_regime: city commit 54de499878d5ddb0152e6fff91911e327d0b6e93 (resolved before launch).
test_cmd: gate-criterion3.sh --repo /var/tmp/gc-merge-validate.ga-fhy1tvm.l6jj_yx6 --base d5261665dcd8f12e8a5a7ed4fdf9343f849f8c37 --bead-json /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/bead.json --owned /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/owned.txt --run-dir /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/c3/evidence; detached via gate-detached-run.sh, rc=0. Five phases invoke bazel test --config=remote-exec --noremote_local_fallback --nocache_test_results --config=ci with verbose per-job logs: //...; --config=acceptance //test/acceptance:acceptance_test; --config=integration //test:integration_packages; --config=integration //test/integration:integration_test with latency roots excluded; separate latency phase for those roots. Mandatory native floor runs in addition.
test_cmd_scope: full-suite
test_counts: PASS=140113 FAIL=0 SKIP=481 (top-level result lines across 530 retained logs; repeated tests count once per job, not unique tests).
test_log_dir: /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/c3/evidence/remote/logs
tests_green: true
heavy_mode: none
waiver_ref: none
failure_attribution: none
ci_lane_run: n/a (no CI-config change)
setup: pinned bd v1.3.2 verified; rootless runtime/pinned image probe passed; gate-icu74.sh check verdict=OK with recorded provenance, no overlay.
test_exec_mode: remote-exec
test_exec_bep: unit 389/389, acceptance 24/24, integration-packages 86/86, integration-full 14/14, integration-latency 1/1; total 514/514 remote uncached attempt 1, start-window valid and terminal verdicts; cached locally/remotely=0, retries=0, other strategy=0.
rbe_state: worker env matches pinned main; marker present; kill switch absent; cert valid to 2027-01-09 (execution.json below).
fallback_reason: none
admission: remote floor=25 GiB, free=50.224 GiB, cap=2, waited=0.001s; no remote heavy slot/load wait.
farm_test_seconds: 10330.728
owned_topup: none (all changed tests ran in full phases).
floor_topup: complete, 16/16 steps rc=0; beadstest, Darwin compile, runner self-tests and environment-absence top-ups completed. Non-owned residual skips inventoried.
latency_leg: both latency roots PASS in phase 5 attempt 1; GATE_FARM_PROBE verdict=clear, waited=0s, load1=30.04, load5=64.03, load15=36.46, threshold=50; absent from full integration leg as required.
load_threshold: 15
load_waited_seconds: native floor 1800; required-job accounting 0
load_wait_timed_out: native floor 1; required-job accounting 0
run_start_load: native floor 25.29; required-job accounting 4.49
run_max_load: native floor 24.15; required-job accounting NA (finished before sample)
run_mean_load: native floor 21.73; required-job accounting NA
run_readings: native floor 6; required-job accounting 0
load_note: bounded native wait timed out and proceeded per ordinary local-floor policy; wait_first_load=107.66, wait_max_load=116.16, wait_mean_load=92.11, wait_readings=61. Remote outer observer is context only (run_start_load=63.12, run_max_load=116.14, run_mean_load=79.63, run_readings=110); farm phases have remote admission rather than host load wait.

Diff-owned results (gate-test-evidence.py, not aggregate inference):

- TestStaleDBFormulaCloseIsIndependentOfIdentityEnv: PASS, including the distinct session-id/name pool row.
- TestRawBdMutationRequiresActorOrForce: PASS.
- TestBdActorGuardExemptOrderCheck: PASS.

skip_inventory: 467 farm inventory rows + 39 native rows; exact reasons/sites retained by pointers below (counts include subtests and repeated jobs).
skip_justification: remaining skips are unchanged platform, optional live-provider/tool, integration-mode or fixture prerequisites; 506 rows audited at 281 unchanged sites, zero unresolved/affected packages. No skipped test's import closure reaches test/packlint shared hunks; this test-only patch adds no non-test package input. No changed test skipped. Native floor retried every environment-absence skip as required; no failure waived.

ci_required_closure: python3 ci_required_triggers.py /var/tmp/gc-merge-validate.ga-fhy1tvm.l6jj_yx6 d5261665dcd8f12e8a5a7ed4fdf9343f849f8c37 170e089e6bb06ed08c94b8aa9876984a9adcff7f fork; rc=0; workflow blob e5b11c348505dab28dc223e874bc2c61cbbb33c5; 4 LEAF jobs need a status; /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/ci/closure.txt.
ci_required_accounting:

- runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<retained file> python3 .github/workflows/scripts/runner_policy.py; rc=0; /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/ci/runner-policy.log and runner-policy.out. Detached/fresh-tree/isolation/load wrappers; accounting log records command environment.
- changes | LOCAL-PASS | ci_required_triggers.py above; rc=0; /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/ci/closure.txt; suite_mode filtered, no changed-path filter matched.
- credential-provider-windows | NOT-TRIGGERED | credential_provider no match; if: needs.changes.outputs.credential_provider == 'true'; workflow runner resolved to blacksmith-4vcpu-windows-2025.
- pack-gate | NOT-TRIGGERED | packs no match; if: needs.changes.outputs.packs == 'true'.

windows_reach: none, windows_reach.sh rc=0; /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/ci/windows-reach.txt.
deferred_to_pr_ci: none
not_covered: none
accounting_cmd: fresh-tree-run.sh -- isolated-test-run.sh -- bash /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/ci-accounting.sh (self-wraps load-gate-run.sh --class local-heavy --threshold 15 --max-wait 1800); detached post-c3-v2 rc=0; /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/ci-accounting.log. Earlier rc98 disk refusal ran no job; original setup log retained. Resumed after DISK_FLOOR_OK floor_gb=40 free_gb=45.1; completed tests never relaunched.

Compact execution record:

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/c3/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":50.22367858886719,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"},"test_exec_bep":{"unit":{"results":389,"remote_executed":389,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":24,"remote_executed":24,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":10330.728,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":47.174,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=30.04 load5=64.03 load15=36.46 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 21.97 59.40 35.66 13/1008 52375"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":30535}},"log_dir":"/var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/c3/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":396,"environment-absence":71},"top_level_counts_by_reason_class":{"other":223,"environment-absence":42},"tests":{"elided_array":{"items":467,"bytes":166184}}},"test_cmd":{"elided_array":{"items":5,"bytes":2049}},"test_counts":{"PASS":140113,"FAIL":0,"SKIP":481},"diff_tests_executed":"diff_tests_executed:\n  TestStaleDBFormulaCloseIsIndependentOfIdentityEnv PASS (integration-packages-aa557e5bc0c7045b52d7.log, unit-da81bca7c80446c5af28.log)\n  TestBdActorGuardExemptOrderCheck PASS (unit-8efdfc704ae600336f3a.log)\n  TestRawBdMutationRequiresActorOrForce PASS (unit-8efdfc704ae600336f3a.log)\nGATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name\n"}
```

Full execution.json retained: 4788 lines; /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/c3/evidence/execution.json; SHA256 69df02147575c5362b2e18001406270e3506cd8146a35a91608f4f60b7b41385.

Full farm skip inventory retained: 3738 lines; /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/c3/evidence/remote/skip-inventory.json; SHA256 a1b6687bd8ef6107ba66adf4a3953501fe0ed341baf176d3a84872bfd7894b12.

Full native skip inventory retained: 314 lines; /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/native-skip-inventory.json; SHA256 6ee2ff28945ed47f05658f4479f4aa7aea9c074c3d1ef398d4317ccc4a96afec.

Full skip-site proofs retained: 5568 lines; /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/skip-site-rows.json; SHA256 9e18bbb6bcd639bea93a4e299fbb8ed69504b16dc98741693d3366952b7009d0.

Full skip import reach retained: 4643 lines; /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/final-skip-reach.json; SHA256 b6226f493ec522f976d14c223133cd7efb0e708b5a2ada09fb0795b7e007fb1c.

Full floor top-up steps retained: 901 lines; /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/c3/evidence/remote/floor-topup.json; SHA256 0cf4b75ee68b71b40477478d5ef53825698c2bfcffe8739ae32b814c79c3adc4.

Full retained evidence manifest retained: 1615 lines; /var/tmp/gc-deploy-ga-fhy1tvm.3ry7vanz/completed-evidence-sha256.json; SHA256 63eb1391ecc330795df2187700ad7cc9d6f07ff06faa320e34e8ac81ebd78ab3.

