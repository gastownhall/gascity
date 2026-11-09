# Release gate: Nightly aggregate job

**Verdict:** **PASS**

Bead: ga-4nw07a8p; reviewer: ga-4wbahtu1; feature/rework lineage: ga-clmemp.3, ga-wsfovk, ga-799w0wq.
Recorded: 2026-11-09T00:59:46.907115+00:00. Gate round: adhoc-1794176682-ga-4nw07a8p-r3 (authorized third round; no fourth round).
deploy_mode: remote; target: deploy/ga-4nw07a8p-gate; source: `bb9a9780aea8d94cd864b9de3d476adbd0b75288` (resolved reviewed commit, never a branch tip).
merge_state: clean (origin/main@c7a45437fcf9bd35d8cda00928dcf3a7794059be).
gate_base_kind: current-tip; gate_base: `c7a45437fcf9bd35d8cda00928dcf3a7794059be` (pinned throughout).
canonical_merge: `67ac7d14cfdba01c00f9d2d23daa182bda92423b`; canonical_tree: `d8e4295f60e307b77275586e88d2e9200690c8d4`.
merge_validation: whole-tree build/nogo/vet and the full suite ran on that exact two-parent merge; current-head CI and merge authority's merged-result checks hold the merge-time guarantee.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Live reviewer notes explicitly name the resolved source and verdict pass; this is a fresh review after rebase, with no carryover substitution. |
| 2 | Acceptance criteria met | PASS | One always-run Nightly summary depends on all five jobs and rejects every non-success value. Both owned tests and all 15 cases PASS; real Nightly summary and all prerequisites succeed. Rebase fidelity, census reconciliation, generated ledger, pin and BUILD wiring checked below. |
| 3 | Full suite and required lanes pass | PASS | 533 fresh remote test-result events, 16 native floor steps, all 22 fast targets, exact-source real Nightly and all four CI leaf accounts complete; counts, skips and deferral below. |
| 4 | No unresolved HIGH findings | PASS | Live review has no security findings of any severity and no blocking style/spec finding; only two disclosed low observations below. |
| 5 | Final branch clean | PASS | Implementation and canonical scratch were clean; this record is the sole deploy edit. Publication requires its normal hook-checked commit and empty porcelain; no source-code edits. |
| 6 | Merge recorded; build and vet pass | PASS | Exact source/base parents and merge-tree equality independently verified. Canonical build: 1476 successful target completions, terminal SUCCESS; full nogo/vet and active hooks rc0. |
| 7 | Single feature theme | PASS | Six source-diff paths implement one Nightly aggregate: YAML, contract test and BUILD entry, execution-policy pin, subprocess census and prior gate record. No internal agent configuration or independent feature. |

Acceptance details:

- Fresh review satisfies the rebase-refresh contract; the source includes the routed-row repair. The clean current-base merge preserves the feature patch without conflict repair by the deployer.
- The resource ledger adds one subprocess call for the new test. Resource-census policy PASS verifies the reconciled ledger; TESTING.md, census.go and test-resources.toml need no hand edits.
- Existing Nightly job bodies/triggers/permissions are unchanged: only the 50-line aggregate is appended. Environment values carry each needs result to Python; any value other than success fails closed.
- The workflow execution pin and generated BUILD wiring pass their actual checks. The reviewed source was pushed and fresh reviewer PASS is recorded.
- Low review observations: tests spell cancellation `canceled`, while the script rejects both spellings through its success-only predicate; an older superseded FAIL gate remains historical. This current record supersedes those earlier verdicts.

test_cmd_scope: full-suite
test_exec_mode: remote-exec (derived from BEP execution strategy, not eligibility).
test_counts: 149402 PASS, 0 FAIL, 512 SKIP (top-level events over 549 retained farm/native logs; repeated names count per job).
diff_tests_executed: TestNightlyHasOneAlwaysRunAggregateDependingOnEveryJob PASS; TestNightlyAggregateFailsForEveryNonSuccessDependency PASS; its five dependencies x failure/canceled/skipped cases PASS (17/17 names; no FAIL/SKIP/MISSING).
waiver_ref: none. failure_attribution: none. heavy_mode: none (classifier verified).

test_cmd: canonical gate-criterion3.sh --repo /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/merge-canonical --base c7a45437fcf9bd35d8cda00928dcf3a7794059be --bead-json /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/bead-array.json --owned /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/owned-tests.tsv --run-dir /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/c3-run/evidence.
The command ran through the detached canonical runner with runtime preflight, five remote phases and the local native floor. Unit is make -k check; acceptance and both integration tiers match Bazel CI; latency runs separately as prescribed.
Every phase forces --config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 and its own --build_event_json_file; nogo uses --keep_going --output_groups=nogo_fix --config=remote-exec.

| BEP phase | Fresh remote terminal PASS results |
|---|---:|
| unit | 407 |
| acceptance | 25 |
| integration-packages | 86 |
| integration-full | 14 |
| integration-latency | 1 |
| Total | 533 |

test_exec_bep: every result has strategy remote, neither cached flag, attempt 1, start within its phase window and terminal PASSED. Independently recomputed from all five raw BEPs; 1066 original log/XML SHA256 values verified.
rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
admission: {"class":"remote","floor_gib":25.0,"free_gib":87.54984283447266,"waited_seconds":0.001,"cap":2}
farm_test_seconds: 11477.903; fallback_reason: none; owned_topup: none.
floor_topup: complete, all 16 steps rc0: environment-absence packages, pinned beadstest, Darwin compile and both runner self-tests. Native checks do not change the BEP-derived remote execution mode.
latency_leg: measured, clear farm probe on attempt 1; both budget tests PASS; no latency result from a loaded whole-suite run is credited.
skip_inventory: 537 farm/native rows, 289 sites, 131 byte-identical base/head files. All 39 skipping test-binary import closures exclude changed scripts/cipolicy and test/workflows helpers. Native environment-absence topups ran; residual non-owned platform/backend/fixture skips retain per-test reasons. No diff-owned skip is justified away.
skip_justification: every observed skip site is unchanged and outside changed package/helper reach; complete per-site, shared-hunk, file-blob, closure and native-topup evidence retained at the pointers below.
test_log_dir: /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/c3-run/evidence/remote/logs and adjacent floor-logs (retained, never deleted).

policy_lane: PASS; fresh-tree-run.sh + gate-base.sh run, run-pinned-lint.sh -- make fmt-check-changed, make lint, shell boundary/routed-row/release guards and hooks, plus 14 policy/format Bazel targets. fast-lane.sh retains exact targets/commands.
drift_lane: PASS; a separate fresh view runs make bazel-sync, git diff --exit-code and empty porcelain, followed by seven generated-artifact Bazel targets. A third view runs the OpenAPI breaking check, one target. All 22 fast targets PASS before the suite; no lane wrote the canonical suite scratch.

load_threshold: 15; load_waited_seconds: 0; load_wait_timed_out: false; run_start_load: 33.31; run_max_load: 49.15; run_mean_load: 22.42; run_readings: 73.
These are orchestration-host readings, not farm executor load. Remote admission has no local-heavy wait; native floor retains the canonical local bounded load wait. The latency farm probe measured load5=14.93 on 32 CPUs.
Runtime: ICU74 OK; rootless Podman socket/API and both pinned Dolt image tags verified. Disabled Ryuk paired with unreaped-container opt-out and enabled/recent testcontainer sweep. Full runtime-proof.json retained.

ci_lane_run: https://github.com/gastownhall/gascity/actions/runs/42537831180 — DISPATCHED at exact reviewed source, completed success; all six jobs including Nightly summary succeed. This is the job's first actual run, separate from contract tests. Only the approved pre-PASS exact-source push and one dispatch occurred.

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/merge-canonical c7a45437fcf9bd35d8cda00928dcf3a7794059be bb9a9780aea8d94cd864b9de3d476adbd0b75288 origin.
workflow_blob: e5b11c348505dab28dc223e874bc2c61cbbb33c5; tool result: 4 LEAF jobs need a status; check/ci-required are aggregators with no invented leaf status.

| ci_required_accounting job | Status | Evidence |
|---|---|---|
| changes | LOCAL-PASS | Closure tool above rc0; ci-required/closure-final.txt. |
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<retained temp file> python3 .github/workflows/scripts/runner_policy.py, rc0 in canonical scratch; ci-required/runner-policy.log. |
| credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | windows_reach.sh rc0 with empty output, no changed Windows-closure input; ci-required/windows-reach.log. |
| pack-gate | LOCAL-PASS | Job's exact two run blocks and env, including GC_TEST_GASCITY_PACKS_REGISTRY=main, rc0: bundled pin check plus make test-pack-registry-live; ci-required/pack-step-results.json and pack-output.txt. |

windows_reach: none.
deferred_to_pr_ci: credential-provider-windows (non-linux-runner).
not_covered: none.
host_gaps: none; live read-only registry probe passed, so the actual pack job ran locally.
pack_gate_execution: detached fresh view + pinned gate-base + local-heavy admission + bounded load wait + isolation wrapper; both steps rc0, terminal GATE_RUN_EXIT rc=0 state=complete. The wrapper reports PASS-THROUGH on this fresh scratch path; the make target itself scrubs its test environment. No operator checkout was modified.
pack_gate_load: threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=102.24 run_max_load=99.37 run_mean_load=89.94 run_readings=11; admission prewait also exhausted 1800s before normal admission. The ordinary bounded wait then proceeded, as policy permits; this is no host-gap deferral or waiver.

The PASS authorizes PR-open. Merge requires current-head CI / required and each deferred job success (or its actual false-path skip). Merge authority holds the exact-head guarantee; this deployer does not merge.

Recovery history: old quiescence and caller-environment HOLD records remain in git. Mayor decisions ga-dgfbs5k7 and ga-aq81v1ys authorize this same third-round continuation. The missing GC_CITY_ROOT caller was corrected only under that grant; every ordinary guard and normal origin dry-run then passed. No source rewrite, reset, rebase, force push, waiver or fourth suite round.

Evidence root: /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c. Original logs/BEPs and resolved historical records remain retained.

Full c6-canonical-validation.json retained: 15 lines; /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/c6-canonical-validation.json; SHA256 97cb8d6020fa6820f0054378a9efa1d4c5f1233b22c254a2645455270dfe799c.

Full fast-validation.json retained: 24 lines; /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/fast-validation.json; SHA256 08c0b2ea356b54b6aefd9f0186c32d74eb3006b28ee79585a034230e97ef4cd5.

Full suite-audit.json retained: 103 lines; /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/suite-audit.json; SHA256 49ebdf3c6ab8c03e7d34578d182ccec784ed4f38e770ba134dcf4809179d8ea1.

Full skip-audit.json retained: 11 lines; /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/skip-audit.json; SHA256 c2f37d5ffa4b4e01d564573a32a1ada92b656792dfe5e138738864c2cbadc305.

Full skip-reach-proof.json retained: 97 lines; /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/skip-reach-proof.json; SHA256 024016009d46fc3783acd04e3db40d16713ec2aeb2fd4d8f77d71bbcc81d729b.

Full owned-by-name-subtests.json retained: 53 lines; /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/owned-by-name-subtests.json; SHA256 b5db74e4481c72dd69edbba0ed8083f4acbc9b5f2a54e7f792580441d98c313d.

Full runtime-proof.json retained: 22 lines; /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/runtime-proof.json; SHA256 c7ba89d40e106baedec6e31a3c316ea838fad28d2e276060821155617ab9c164.

Full ci-required/accounting.json retained: 76 lines; /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/ci-required/accounting.json; SHA256 61b369d29297d26d93eae69e516156688646bbadd69bf8a1ec847d381b2420c8.

Full final-independent-audit.json retained: 20 lines; /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/final-independent-audit.json; SHA256 84125eb64d47f6a1845470647440e3bad8f6e36c806e2ef62113baff15ad6b48.

Full c3-run/evidence/execution.json retained: 5044 lines; /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/c3-run/evidence/execution.json; SHA256 0ff668e25bffe27a932257f3072e0dcae584d65214fab8d1fa411b62fbdeff6a.
