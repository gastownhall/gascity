**Verdict:** **PASS**

# Managed Dolt unresponsive-recovery release gate

- Deploy bead: ga-f4hzfz5; build ga-dhbgba0 / rework ga-e46ibb4; passing review ga-6q3y4i1.
- Reviewed source: `94107ef508c94636857b84bd9ff7d414b642185b` (resolved from the reviewed Commit field); source branch is provenance only.
- Public bug: https://github.com/gastownhall/gascity/issues/8669.
- Date: 2026-11-01. `DEPLOY_MODE=remote`; `BASE_REF=origin/main`; push target `fork`.
- `merge_state: clean (origin/main@d5261665dcd8f12e8a5a7ed4fdf9343f849f8c37)`
- `gate_base_kind: current-tip`; `gate_base: d5261665dcd8f12e8a5a7ed4fdf9343f849f8c37`; source merge base `f3c4ca5bbe3cf5e130aad564aab80f5a7d81acbe`.
- `merge_head: 656d578dcb4ab3df4eb01dc49c258f467a5f9e54`; canonical merge tree `282c3a2c91bd73efcbff4b6dff3f3a6eb220b1df`. Normal hooks ran and the resulting tree exactly matches Git merge-tree.
- `merge_validation: merge result built and vetted here`.
- Evidence root: `/var/tmp/gc-gate-ga-f4hzfz5.DFRt15`. No runtime installation or service replacement is part of this deploy.

## Criteria

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Review ga-6q3y4i1 records PASS on the reviewed source. reviewer-added-tests.sh independently returned reviewer-tests files=cmd/gc/dolt_recover_managed_test.go for the direct 2e74da4f41b71f2de9ad491ab099801784d15f56 → 94107ef508c94636857b84bd9ff7d414b642185b addition. |
| 2 | PASS | Code and fresh tests cover live-server deferral, elapsed/disabled windows, healthy-streak clearing, cross-process cooldown and decision logging. Both original frozen-server entry-point reproductions pass on a newly built canonical candidate; details below. |
| 3 | PASS | Full-scope fresh remote unit, acceptance, integration-packages, full integration and separate latency phases passed; all required local floor and CI-leaf checks completed. All 17 owned tests PASS by name; no owned SKIP/FAIL/MISSING. |
| 4 | PASS | Latest passing review has no unresolved HIGH findings. Its low-severity observations and optional coverage follow-up do not block it. |
| 5 | PASS | Checkout was clean before this sole record was added. Final deploy status is checked after committing it and before publication. |
| 6 | PASS | Whole-tree bazel build --config=remote-exec --noremote_local_fallback --keep_going //... passed, including nogo/vet: 1448 targets, 13515 actions, 379.864 seconds. Its actual summary was 9765 action-cache hits, 2033 internal and 11482 linux-sandbox processes; this is build evidence, separate from fresh remote test evidence. |
| 7 | PASS | One feature theme: safely recover a managed Dolt server after a probe failure. Config, generated reference/schema, logging, provider cooldown and test census changes support that behavior. Ancestry was checked with the confirmed build/rework ids; no unrelated feature or .claude path. |

## Acceptance evidence

- The default continuous-unresponsiveness window is 180 seconds; the configured zero window keeps immediate recovery. Negative durations are rejected. Fresh config/default/parse/validation tests PASS.
- A failed SQL probe of an owned, live, listening server records the first-unresponsive time and returns an error before stop/start. Dead/refusing, read-only, elapsed-window and zero-window paths retain recovery, covered by the fresh full-suite results.
- Successful probes, healthy provider observations, observed-rebind and lock-wait returns clear the persisted streak. All four paths have passing tests, including the two reviewer-added witnesses and the two-incident regression.
- Provider recovery cooldown survives separate processes; a future timestamp is not counted as a recent recovery. Both fresh tests PASS.
- Deferrals and stops append decision, PID, port, probe error, elapsed duration and quoted caller to dolt.log. The provider helper preserves stderr. Fresh log/caller tests PASS.
- Independent real-server checks ran both original investigation scripts against a Bazel-built binary from the canonical merge. Each private server was query-ready before SIGSTOP and still accepted TCP connections while frozen. Both commands returned 1 with the deferral error and dolt.log decision, left the same server alive in state T, and produced no traced SIGTERM/SIGKILL to that PID during the command. Both owned servers were confirmed absent after cleanup.
- Harness-only adaptations pin the candidate gc and test bd on the private env-i PATH, trace the tested command, replace the old post-command wait with assertions, and resume the private process before cleanup. Original script hashes and exact diffs are retained; no product code or test assertion was changed.

## Fast lane

- `policy_lane: PASS` — fresh-tree-run.sh → gate-base.sh run → run-pinned-lint.sh → make fmt-check; make lint (remote nogo); make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary check-hooks. Pin and resolved golangci-lint version 2.14.0, mismatch=false.
- `drift_lane: PASS` — a separate fresh view ran make bazel-sync with remote flags, then git diff --exit-code. The existing internal/git Gazelle expression warning was printed; no generated drift remained.
- Fast lane completed at 2026-11-01T16:28:18Z before the full suite launch. The first unfinished policy attempt was cancelled to install the missing per-view remote definitions; its log is retained, and no test failure was retried.

## Full test execution

- `test_cmd: gate-criterion3.sh --repo /var/tmp/gc-gate-ga-f4hzfz5.DFRt15/merge --base d5261665dcd8f12e8a5a7ed4fdf9343f849f8c37 --bead-json /var/tmp/gc-gate-ga-f4hzfz5.DFRt15/bead.json --owned /var/tmp/gc-gate-ga-f4hzfz5.DFRt15/owned.tsv --run-dir /var/tmp/gc-gate-ga-f4hzfz5.DFRt15/suite/evidence`, launched through gate-detached-run.sh, with GC_GATE_REMOTE_ENABLED=1 and the prescribed admission/isolation wrappers.
- `test_cmd_scope: full-suite`; `test_counts: PASS=140143 FAIL=0 SKIP=481`. These are top-level result lines across 530 retained logs, including 16 local floor logs; repeated jobs count separately. They are not distinct test names.
- `test_exec_mode: remote-exec`; `criterion3_valid: true`; `fallback_reason: none`.
- `test_log_dir: /var/tmp/gc-gate-ga-f4hzfz5.DFRt15/suite/evidence/remote/logs`.
- `heavy_mode: none`; `waiver_ref: none`; `failure_attribution: none (no failed criterion or test result)`.
- `rbe_state`: kill switch absent, certificate valid to 2027-01-09; tree and main worker-env pins both sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98.
- `admission`: remote, 25-GiB floor, free_gib=44.117881774902344, waited_seconds=0.001, cap=2.
- `farm_test_seconds: 10443.972`.
- `owned_topup: none needed`; `floor_topup: complete, 16/16 exit 0`; native compile, beadstest/cache witness, runner self-tests and every environment-absence skip top-up are retained.
- `latency_leg: measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS`; attempt 1, probe clear after 20 seconds at load1=44.24, threshold 50. No latency result from the main integration leg is credited.
- ICU74 check returned OK with recorded hashes. Test bd was pinned to v1.3.2. The wrapper explicitly reported PASS-THROUGH / NOT ISOLATED; no clean ambient-environment claim is made. The frozen-server reproductions independently used private env-i homes and cities.

| Phase | BEP results | Remote executed | Cached remote/local | Retries | Window valid |
|---|---:|---:|---|---:|---|
| unit | 389 | 389 | 0 / 0 | 0 | true |
| acceptance | 24 | 24 | 0 / 0 | 0 | true |
| integration-packages | 86 | 86 | 0 / 0 | 0 | true |
| integration-full | 14 | 14 | 0 / 0 | 0 | true |
| integration-latency | 1 | 1 | 0 / 0 | 0 | true |

Exact recorded phase flags:

- `test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/suite/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec`
- `test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/suite/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec`
- `test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/suite/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec`
- `test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/suite/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec`
- `test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/suite/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec`

ICU loader evidence:

```text
GATE_ICU74 lib=libicui18n.so.74 file="/usr/local/lib64/icu74-compat/libicui18n.so.74.2" sha256=d90d62f0d7b6c6fbc279ae5e433cc7004aa47cdb106a71a02350099f8df9ae51 recorded=yes
GATE_ICU74 lib=libicuuc.so.74 file="/usr/local/lib64/icu74-compat/libicuuc.so.74.2" sha256=33499dd97a5393926b32e0b535f5340f7dae14b35e59025b572e6bd9ad48a2ae recorded=yes
GATE_ICU74 lib=libicudata.so.74 file="/usr/local/lib64/icu74-compat/libicudata.so.74.2" sha256=c891c5fdb45e3cf756b728abe256be7af1bfc97f00ca2f2fac450c96c09bd4b7 recorded=yes
GATE_ICU74 verdict=OK provenance=recorded sonames="libicui18n.so.74 libicuuc.so.74 libicudata.so.74"
```

## Skips and owned tests

`skip_inventory`: 467 inventory rows, classified as {"environment-absence": 71, "other": 396}. Printed reasons remain in the inventory. Mode/platform gates, optional live providers, helper-only invocations and unsupported optional conformance capabilities account for the non-owned skips. The environment-absence rows received their required native top-ups; remaining non-owned skips are inventoried rather than counted as execution of this change.

All inventory skip sites were resolved to the canonical source: 281 unchanged sites in 125 files, no touched site, no unresolved location. Exact file analysis and site evidence are retained. The only changed shared test helpers are in cmd/gc; their 147 unit skip rows across 142 parent tests were independently measured by name on untouched pinned base d5261665dcd8f12e8a5a7ed4fdf9343f849f8c37: 147 pre-existing, zero new, changed-reason, owned, reasonless or unjudged rows. This focused comparison supplements the full-suite evidence and does not replace any phase.

```text
diff_tests_executed:
  TestHealthBeadsProviderBacksOffSecondRecoverAcrossProcesses PASS (integration-packages-509ef0a6cd81866833cc.log, unit-cd6930bb0d0da8646500.log)
  TestHealthBeadsProviderClearsUnresponsiveStateWhenHealthy PASS (integration-packages-074fc6c34a1fb2577127.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestLogManagedDoltRecoverDecisionQuotesCaller PASS (integration-packages-18745ddf7d62465654f5.log, unit-23e7d9fc763496dd8095.log)
  TestManagedDoltRecoverCallerFallsBackToProcess PASS (integration-packages-830685c60d0c2dd88567.log, unit-4d20b3d28497f844aa39.log)
  TestRecentManagedDoltRecoverIgnoresFutureTimestamp PASS (integration-packages-bc482d2ae768251ad47f.log, unit-f679cd2551fcc64525aa.log)
  TestRecoverManagedDolt_DefersStopWhileLiveServerUnresponsive PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-f93a0ca96ccc61d0cae9.log)
  TestRecoverManagedDolt_HealthyGapStartsNewUnresponsiveStreak PASS (integration-packages-cb110414c557c0928e82.log, unit-10cc7795c0ec199a5530.log)
  TestRecoverManagedDolt_ObservedRebindClearsUnresponsiveState PASS (integration-packages-b72a67a2f9444af705e3.log, unit-2ef13e1b6898c5544764.log)
  TestRecoverManagedDolt_ObservedWhileLockHeldClearsUnresponsiveState PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-64014bc0bb3833bc198c.log)
  TestRecoverManagedDolt_StopLogsReasonToDoltLog PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-2bde55fab377f50b4739.log)
  TestRecoverManagedDolt_StopsAfterUnresponsiveWindowElapsed PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-088cc38d26d094729229.log)
  TestRecoverManagedDolt_StopsWhenNotLiveListening PASS (integration-packages-509ef0a6cd81866833cc.log, unit-912ab643330b35bd0b6b.log)
  TestRecoverManagedDolt_SuccessfulProbeClearsUnresponsiveState PASS (integration-packages-75f7330b9b673df65e4f.log, unit-5a9df316125cd0aeff2e.log)
  TestRecoverManagedDolt_ZeroWindowStopsImmediately PASS (integration-packages-568812e4d525bdd1f974.log, unit-cd6930bb0d0da8646500.log)
  TestDaemonDoltUnresponsiveRestartAfter PASS (unit-4bbf1267e0c4ccc54042.log)
  TestParseDoltUnresponsiveRestartAfter PASS (unit-4bbf1267e0c4ccc54042.log)
  TestValidateNonNegativeDurationsRejectsNegativeDoltUnresponsiveRestartAfter PASS (unit-4bbf1267e0c4ccc54042.log)
GATE_TEST_EVIDENCE_RESULT ok: 17 diff-owned tests verified by name
```

## Required CI-job accounting

`ci_required_closure: python3 ci_required_triggers.py /var/tmp/gc-gate-ga-f4hzfz5.DFRt15/merge d5261665dcd8f12e8a5a7ed4fdf9343f849f8c37 94107ef508c94636857b84bd9ff7d414b642185b fork`; workflow blob `e5b11c348505dab28dc223e874bc2c61cbbb33c5`; 4 LEAF jobs. Shared internal/config paths trigger the full union.

| Job | Status | Evidence |
|---|---|---|
| changes | LOCAL-PASS | ci_required_triggers.py exit 0; shared MATCH, suite_mode=full; raw ci-closure.txt. |
| runner-policy | LOCAL-PASS | The job's Python command ran with EVENT_NAME=pull_request, PR_AUTHOR=quad341, FORCE_BLACKSMITH empty and a temporary GITHUB_OUTPUT; runner-job completed with rc=0 and retained output. |
| pack-gate | LOCAL-PASS | Both exact run blocks, including their retry loops, completed with rc=0 in one fresh view: scripts/update-bundled-gastown-pack --check; GC_TEST_GASCITY_PACKS_REGISTRY=main make test-pack-registry-live. Pins match release 0.1.12; live catalog and import tests passed. The precise CI-command interpretation is mayor ruling gm-wisp-ic3ecct, recorded on the bead; it is not a waiver. |
| credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | Triggered by shared config. windows_reach.sh exit 0 with empty stdout: none of its build inputs changed. This Linux host does not run the Windows job. |

`windows_reach: none`; `deferred_to_pr_ci: credential-provider-windows (non-linux-runner)`; `not_covered: none`.

`ci_lane_run: n/a (no CI-job, matrix, timeout or required-check change in this diff)`.

PASS authorizes PR opening. Merge authority must also require CI / required and the deferred Windows job to succeed on the exact PR head.

## Load observations

`load_threshold: 15; load_waited_seconds: 0; load_wait_timed_out: 0; run_start_load: 19.32; run_max_load: 19.18; run_mean_load: 9.84; run_readings: 44`.

Remote admission does not wait for host load. Local floor and supplemental jobs retain their soft load waits. The host samples below are separate from the farm's clear latency probe.

- suite: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=19.32 run_max_load=19.18 run_mean_load=9.84 run_readings=44 wait_first_load=19.32 wait_max_load=19.32 wait_mean_load=19.32 wait_readings=1 read_errors=0`
- pack-job: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=10.19 run_max_load=12.24 run_mean_load=11.63 run_readings=4 wait_first_load=10.19 wait_max_load=10.19 wait_mean_load=10.19 wait_readings=1 read_errors=0`
- runner-job: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=10.50 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=10.50 wait_max_load=10.50 wait_mean_load=10.50 wait_readings=1 read_errors=0`
- repro-job: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=7.68 run_max_load=9.81 run_mean_load=9.48 run_readings=2 wait_first_load=7.68 wait_max_load=7.68 wait_mean_load=7.68 wait_readings=1 read_errors=0`
- base skip comparison: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=10.49 run_max_load=10.97 run_mean_load=10.97 run_readings=1 wait_first_load=10.49 wait_max_load=10.49 wait_mean_load=10.49 wait_readings=1 read_errors=0`

## Retained evidence

- execution.json: `/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/suite/evidence/execution.json`; SHA256 `a5c659b9e53e7825800a17e686014d12e5ce38229445dc4fe20e4eee92c39583`; 250275 bytes, 4789 lines.
- skip-inventory.json: `/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/skip-inventory.json`; SHA256 `0c6883f8ac6ccb3ab97e58606767ccdc5b8f4589594a251b81862d33b9f442f2`; 193013 bytes, 3748 lines.
- floor-topup.json: `/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/floor-topup.json`; SHA256 `1c0173484067a920939c7c04450d772d1e3cb39bab7fd9c17f3205b7084e034a`; 39610 bytes, 901 lines.
- latency-leg.json: `/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/latency-leg.json`; SHA256 `28c109f172c8dd3fe7143789fca1bf0471491e29729dbad6b3b9108c9dc128ef`; 1810 bytes, 56 lines.
- all-skip-explain.txt: `/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/all-skip-explain.txt`; SHA256 `0b6a3b3e175da8c219987add3fcc2e8dd3b36292639c8281cad26a194e6b3096`; 512011 bytes, 4548 lines.
- all-skip-site-proofs.txt: `/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/all-skip-site-proofs.txt`; SHA256 `bddf987fd4a1146c02604f7e6c527b74f253c3f271bc7e8234aeece4a112d4bf`; 55334 bytes, 281 lines.
- skip-comparison.txt: `/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/skip-comparison.txt`; SHA256 `3b50a846bcd2fc0794066c6639d339b252eff201ceb04c294fac2289efd7cb1e`; 37136 bytes, 148 lines.
- retained-logs.sha256: `/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/retained-logs.sha256`; SHA256 `272a3df92303dd5197bb93fb07d1071f9858abb50847fac080d33ad4f6503608`; 85161 bytes, 530 lines.
- witness.json: `/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/repro-recover/witness.json`; SHA256 `671963d2f6b14f74ee7a2494ab3e35bfc07b22ea29afa357e0ecd6c09834cf2f`; 176 bytes, 8 lines.
- witness.json: `/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/repro-health/witness.json`; SHA256 `ae66b158e7730e6fad85dfcaaeed73071b0d68ce0316337fc36e52b6d8bbdca4`; 176 bytes, 8 lines.
- repro-recover-kills-live-dolt.sh.diff: `/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/repro-recover-kills-live-dolt.sh.diff`; SHA256 `dd6b34018fe4bb6d9a754534dcc35ad5427669b59228ac6dc2d3f7eb0783e030`; 3297 bytes, 61 lines.
- repro-beads-health-kills-live-dolt.sh.diff: `/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/repro-beads-health-kills-live-dolt.sh.diff`; SHA256 `9f8e0c3f576c92e725f4e7b850fa481f363765df5103012d0a3fbae311d2276a`; 3335 bytes, 61 lines.
- policy.log: `/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/policy.log`; SHA256 `8d7c1415150c74e0189cc9f92c44105a4e53c93c5f378db7f51d3195f0f97abd`; 18619 bytes, 123 lines.
- drift.log: `/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/drift.log`; SHA256 `9e375930988c297006c29f1dd0276aa44efa17327a9f6bcf9531b8d7a36c0fed`; 5436 bytes, 46 lines.
- pack-output.log: `/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/pack-output.log`; SHA256 `af4f76cb8b1510ae363666dad084bd2bd6db3f861b5f13a388d4d0e2651548d5`; 4043 bytes, 6 lines.
- runner-output.log: `/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/runner-output.log`; SHA256 `41cd046b163690d83489655f94f2791df0b9ce7baaeb95f0a8c56e1af451ee38`; 361 bytes, 9 lines.

Compact execution record (full arrays retained at the pointers above):

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/suite/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":44.117881774902344,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"},"test_exec_bep":{"unit":{"results":389,"remote_executed":389,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":24,"remote_executed":24,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":10443.972,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":65.966,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=61.78 load5=68.88 load15=35.95 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=20s load1=44.24 load5=64.42 load15=35.18 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 31.11 59.71 34.40 6/882 51856"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":30448}},"log_dir":"/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/suite/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":396,"environment-absence":71},"top_level_counts_by_reason_class":{"other":223,"environment-absence":42},"tests":{"elided_array":{"items":467,"bytes":165717}}},"test_cmd":["test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/suite/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/suite/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/suite/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/suite/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-f4hzfz5.DFRt15/suite/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"],"test_counts":{"PASS":140143,"FAIL":0,"SKIP":481},"diff_tests_executed":"diff_tests_executed:\n  TestHealthBeadsProviderBacksOffSecondRecoverAcrossProcesses PASS (integration-packages-509ef0a6cd81866833cc.log, unit-cd6930bb0d0da8646500.log)\n  TestHealthBeadsProviderClearsUnresponsiveStateWhenHealthy PASS (integration-packages-074fc6c34a1fb2577127.log, unit-1cc8c8ab55e59a9e03a9.log)\n  TestLogManagedDoltRecoverDecisionQuotesCaller PASS (integration-packages-18745ddf7d62465654f5.log, unit-23e7d9fc763496dd8095.log)\n  TestManagedDoltRecoverCallerFallsBackToProcess PASS (integration-packages-830685c60d0c2dd88567.log, unit-4d20b3d28497f844aa39.log)\n  TestRecentManagedDoltRecoverIgnoresFutureTimestamp PASS (integration-packages-bc482d2ae768251ad47f.log, unit-f679cd2551fcc64525aa.log)\n  TestRecoverManagedDolt_DefersStopWhileLiveServerUnresponsive PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-f93a0ca96ccc61d0cae9.log)\n  TestRecoverManagedDolt_HealthyGapStartsNewUnresponsiveStreak PASS (integration-packages-cb110414c557c0928e82.log, unit-10cc7795c0ec199a5530.log)\n  TestRecoverManagedDolt_ObservedRebindClearsUnresponsiveState PASS (integration-packages-b72a67a2f9444af705e3.log, unit-2ef13e1b6898c5544764.log)\n  TestRecoverManagedDolt_ObservedWhileLockHeldClearsUnresponsiveState PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-64014bc0bb3833bc198c.log)\n  TestRecoverManagedDolt_StopLogsReasonToDoltLog PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-2bde55fab377f50b4739.log)\n  TestRecoverManagedDolt_StopsAfterUnresponsiveWindowElapsed PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-088cc38d26d094729229.log)\n  TestRecoverManagedDolt_StopsWhenNotLiveListening PASS (integration-packages-509ef0a6cd81866833cc.log, unit-912ab643330b35bd0b6b.log)\n  TestRecoverManagedDolt_SuccessfulProbeClearsUnresponsiveState PASS (integration-packages-75f7330b9b673df65e4f.log, unit-5a9df316125cd0aeff2e.log)\n  TestRecoverManagedDolt_ZeroWindowStopsImmediately PASS (integration-packages-568812e4d525bdd1f974.log, unit-cd6930bb0d0da8646500.log)\n  TestDaemonDoltUnresponsiveRestartAfter PASS (unit-4bbf1267e0c4ccc54042.log)\n  TestParseDoltUnresponsiveRestartAfter PASS (unit-4bbf1267e0c4ccc54042.log)\n  TestValidateNonNegativeDurationsRejectsNegativeDoltUnresponsiveRestartAfter PASS (unit-4bbf1267e0c4ccc54042.log)\nGATE_TEST_EVIDENCE_RESULT ok: 17 diff-owned tests verified by name\n"}
```
