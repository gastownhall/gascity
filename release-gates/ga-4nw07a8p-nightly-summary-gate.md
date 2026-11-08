# Nightly aggregate release gate — ga-4nw07a8p

**Verdict:** **FAIL**

**Disposition: HOLD, pre-PASS push guard setup refused.** This is incomplete gate evidence, not a source-code rejection or failed test. Criterion3c requires a real Nightly run; none has been dispatched. Mayor decision ga-aq81v1ys names the exit. No waiver, push, PR, clearance or merge-request.

- Recorded: 2026-11-08T23:46:27.464680+00:00
- Reviewed source: `bb9a9780aea8d94cd864b9de3d476adbd0b75288`; reviewer `ga-4wbahtu1` PASS verified live.
- Mode: remote; base ref origin/main; observed/gated base `c7a45437fcf9bd35d8cda00928dcf3a7794059be` (pinned).
- merge_state: clean (origin/main@c7a45437fcf9bd35d8cda00928dcf3a7794059be).
- gate_base_kind: current-tip; canonical merge `67ac7d14cfdba01c00f9d2d23daa182bda92423b`, tree `d8e4295f60e307b77275586e88d2e9200690c8d4`.
- Source branch is provenance only; isolated target: deploy/ga-4nw07a8p-gate.
- Round: adhoc-1794176682-ga-4nw07a8p-r3 (mayor-granted third round; no fourth round).
- Prior local HOLD commits remain intact. This record supersedes their verdict for the current run, while their history remains in git.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Live reviewer notes name the exact resolved source above and verdict pass. No carryover substitution. |
| 2 | Acceptance criteria met | PASS | Nightly summary needs all five existing jobs, always runs and fails closed on every non-success result; both owned roots plus15 generated cases PASS. Ledger/pin/policy checks and full merged suite PASS. GitHub expression evaluation remains separately required by3c. |
| 3 | Tests and all required gate lanes pass | FAIL (HOLD) | Full suite, native floor, fast checks and owned tests are green, but3c real Nightly run is absent; pre-PASS assert_record_fits refused missing GC_CITY_ROOT before its size check ran. CI-required accounting not run after this refusal. No test failure is attributed or waived. |
| 4 | No unresolved HIGH review findings | SKIPPED | Stopped at3c push-guard refusal; no final sign-off. |
| 5 | Final branch is clean | SKIPPED | This local HOLD record is being committed; no final deploy branch sign-off. |
| 6 | Merge state and whole-tree build/vet | PASS | Correct canonical two-parent scratch verified;1476 successful build target completions, full nogo/vet and hooks checks rc0; c6-canonical-validation.json and raw BEP. |
| 7 | Single feature theme | SKIPPED | Early scope/denylist check passed one Nightly aggregate theme; final sign-off stopped at3c. |

## Completed evidence

fast_lane: PASS; three distinct fresh views at the pinned base;14 policy/format/lint targets,7 generated-drift targets,1 OpenAPI target PASS, no generated changes. Canonical scratch stayed clean. See fast-validation.json.

test_cmd_scope: full-suite

test_exec_mode: remote-exec (derived independently from raw BEPs, never eligibility alone).

test_counts: 149402 PASS,0 FAIL,512 SKIP (top-level lines, summed per job across549 retained farm/native logs; repeated tests count once per job).

diff_tests_executed:
- TestNightlyHasOneAlwaysRunAggregateDependingOnEveryJob PASS.
- TestNightlyAggregateFailsForEveryNonSuccessDependency PASS, with all15 generated dependency/conclusion cases PASS.
-17/17 owned names PASS;0 FAIL/SKIP/MISSING. owned-by-name-subtests.json.

test_exec_bep: `{"unit":{"results":407,"remote_executed":407,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}}`

All533 remote results were uncached, strategy remote, attempt1, inside each phase's start/end window, terminal PASS; all1066 raw log/XML artifact hashes verified. Native floor: all16 canonical steps rc0, including beadstest pin, Darwin compile and both runner self-tests. Farm seconds: 11477.903; fallback_reason: none; owned_topup: none; latency_leg: measured, both budget tests PASS, probe clear on first attempt. Native steps do not change the BEP-derived remote execution mode.

admission: `{"class":"remote","floor_gib":25.0,"free_gib":87.54984283447266,"waited_seconds":0.001,"cap":2}`

rbe_state: `{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}`

skip_inventory:537 farm/native inventory rows,289 sites,131 byte-identical files at base/head. All39 skipping test-binary closures exclude scripts/cipolicy and shared test/workflows helpers; no changed skip body/helper can reach them. Environment-absence native topups completed. Residual non-owned SKIPs are platform, optional backend or fixture limits; per-test reasons and site/reach proofs are retained. No diff-owned SKIP is excused. See skip-audit.json, skip-site-proof.txt, skip-sites-resolved.json, skip-file-blobs.json, skip-reach-proof.json and floor-topup.json.

Actual phase commands (the canonical gate-criterion3.sh driver ran these, with unit make -k check, acceptance, integration-packages, full integration minus latency and separate latency):

- test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec

Native floor commands/individual rc0 results: /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/c3-run/evidence/remote/floor-topup.json. Raw execution record (kept out of this markdown): /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/c3-run/evidence/execution.json; SHA256 0ff668e25bffe27a932257f3072e0dcae584d65214fab8d1fa411b62fbdeff6a.

test_log_dir: /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/c3-run/evidence/remote/logs and adjacent floor-logs (retained).

Observed host telemetry, not executor load or native-floor exemption:

`LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=33.31 run_max_load=49.15 run_mean_load=22.42 run_readings=73 wait_first_load=33.31 wait_max_load=33.31 wait_mean_load=33.31 wait_readings=1 read_errors=0`

The native floor retains its canonical local bounded load wait; remote admission uses no host-heavy load wait. Runtime/socket/image/ICU and enabled sweep/recent execution proof: runtime-proof.json. Pack evidence at criterion3 start: `c8f51f6381ab7178f45df68228c5a738ab8baa03`.

## Guard refusal and concrete recovery

Pre-PASS preparation service: /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/ci-push-preparation-run; terminal GATE_RUN_EXIT rc=1 state=failed.

Exact diagnostic:

```text
assert_record_fits: GC_CITY_ROOT is not set; the size check did not run
```

The ancestry, safe-push-target and reviewed-SHA-presence guards preceding it succeeded. This caller omitted an environment variable; no content-size verdict was reached. No origin dry-run, real push, workflow dispatch or PR ran. Existing local branch still has the exact reviewed source plus only historical gate records; no reset, rebase, repoint or force-push occurred.

Prepared correction, not executed: /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c/prepare-ci-push.py, set os.environ['GC_CITY_ROOT'] from the recorded city context before child guards. SHA256 836bbda03cc6edf2310b514fc67a9d3faa496575ea4cabec9d7daf5019094540. All guards must run normally afterward. A reviewed-SHA refspec can publish the mechanically derived target without dropping local historical gate commits.

Formula mol-deployer-gate3c requires: "treat any refusal as stop-and-ask". Same-store decision ga-aq81v1ys records the concrete correction and named exit; parent remains OPEN, held for mayor. No test tracker, attribution, waiver or new full-suite round is claimed.

ci_lane_run: not-yet-run (.github/workflows/nightly.yml; schedule/workflow_dispatch only, no PR trigger).

ci_required_closure: not run (pre-PASS push guard stopped3c first).

ci_required_accounting: pending; deferred_to_pr_ci: undecided; not_covered: undecided.

Evidence root: /var/tmp/deploy-ga-4nw07a8p-r3.0wxjw73c; .gc-no-reap present. Both scratch views and all raw artifacts/logs are retained until a resolved continuation can finish and perform owner-specific cleanup. The held run does not publish deploy clearance.
