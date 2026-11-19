# Watchdog startup identity release gate — ga-v98m6lg

**Verdict:** **PASS**

Gate evaluated 2026-11-19T03:23:48.587364+00:00. Remote deploy to `gastownhall/gascity`, base branch `main`, push remote `fork`. Reviewed source `ba26bf543f068b6aa0b5b9b87b6145b0ed1f89f6` (build `ga-fov5uc`, closed PASS review `ga-170a2s0`); isolated publication branch `deploy/ga-v98m6lg-gate`. Source branch in bead prose is provenance only. No carryover or reviewer-added commits.

merge_state: clean (origin/main@17d7d77ce29c8529f363a14c9b9873b73abc981a)
gate_base_kind: current-tip
gate_base_sha: 17d7d77ce29c8529f363a14c9b9873b73abc981a
tested_merge_head: ef7192c28c532e76927e3a2d994720d9a0f03113
tested_tree: c31b0485ade7ee91d47e63f006e09d7e15c54d1b
publication_base_sha: 7b5614ac70d1031111f072370536bfd440199b34

The clean canonical merge was verified against git merge-tree; criteria 3/3b and CI accounting ran on that tree. The base advanced during publication preparation; this record does not certify the newer merged result. Merge authority must check the current base, exact head and required PR CI. No rebase was performed. The tested scratch was removed only after all evidence and a verified Git bundle were retained.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Closed review ga-170a2s0 names the resolved source SHA above; reviewer notes independently re-read. Single-pass policy applies. |
| 2 | Acceptance criteria met | PASS | Both watchdog spawn paths snapshot identity before handshake. Failure cleanup uses the matching-identity closure; scope-watchdog return record reuses the snapshot. No struct/signature changes. Four regression tests cover simulated changed identity and stable identity in both paths, all PASS by name. |
| 3 | Tests pass | PASS | Canonical full-suite COMPLETE-VALID; five remote phases plus all 16 native floor steps. Policy/format, drift, OpenAPI and BUILD sync passed first. Required-CI accounting below names the deferred Windows runtime job. |
| 4 | No high-severity review findings open | PASS | Zero unresolved HIGH findings. Existing helper-comment wording is a nonblocking review nit; identity-unavailable fallback and check-then-signal semantics are unchanged. |
| 5 | Final branch is clean | PASS | Source and tested merge had empty porcelain status after all lanes. Publication adds only this gate record to the exact reviewed source; normal hooks run at commit/push, followed by clean-status and ancestry assertions. |
| 6 | Merge state recorded; change builds on its merge base | PASS | Clean merge at pinned base above; full Bazel //... build and nogo output-group build both rc0, terminal BEP overallSuccess=true, zero failed completed targets. |
| 7 | Single feature theme | PASS | Four cmd/gc files implement and test one watchdog handshake-failure cleanup guard. Ancestry scope accepts only deploy/build IDs; no unrelated theme or denied internal-doc path. Fresh publication scope check also passed. |

## Criterion 3 execution

test_cmd_scope: full-suite
test_exec_mode: remote-exec
criterion3_valid: true
heavy_mode: none
failure_attribution: TestDatedWaiversAgainstToday -> ga-5550q2dx | publication pre-push only, clause 3a MECHANISM (canonical suite has zero failures)
waiver_ref: none
ci_lane_run: n/a (no CI-config diff)

Primary command (exact recorded argv):

```sh
env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-v98m6lg DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-ga-v98m6lg.ysrqff --base 17d7d77ce29c8529f363a14c9b9873b73abc981a --bead-json /var/tmp/deploy-ga-v98m6lg-20261119.27dez2tq/bead-c3.json --owned /var/tmp/deploy-ga-v98m6lg-20261119.27dez2tq/owned.tsv --run-dir /var/tmp/deploy-ga-v98m6lg-20261119.27dez2tq/c3-run/evidence
```

The isolation wrapper reported PASS-THROUGH: execution ran in place in the disposable merge scratch, with no isolated clone claimed. Remote Bazel actions were hermetic; native runners scrubbed GC_/BD_/BEADS_/DOLT_ state and used the pinned test bd v1.3.4 (7ef2fd05e). Rootless podman ping and ICU74 checks passed. The fleet bd binary was unchanged.

test_cmd (runner-reported):

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-v98m6lg-20261119.27dez2tq/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-v98m6lg-20261119.27dez2tq/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-v98m6lg-20261119.27dez2tq/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-v98m6lg-20261119.27dez2tq/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-v98m6lg-20261119.27dez2tq/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

test_counts: {"FAIL": 0, "PASS": 157110, "SKIP": 613} (execution events, not unique tests). All 541 remote results were first-attempt uncached remote executions, with zero retries/local fallback/other strategies, valid phase windows and successful terminal BEPs. Farm test seconds: 13980.516.

| Phase | Targets/shards | Remote results | Verdict |
|---|---:|---:|---|
| unit | 309 targets | 415 | PASS |
| acceptance | 8 targets | 25 | PASS |
| integration-packages | 37 targets | 86 | PASS |
| integration-full | 14 shards | 14 | PASS |
| integration-latency | 1 action | 1 | PASS |

Latency: TestGcBdLatencyBudget and TestGcSeatHookLatencyBudget both PASS in the dedicated leg, probe clear after 10 seconds at load1=45.12, threshold=50. Native floor: all 16 command steps complete, rc0; these are required coverage, not replacements for the remote suite.

diff_tests_executed:
  TestStartManagedDoltSQLServerWithScopeWatchdogSkipsReusedPIDOnHandshakeFailure PASS (integration-packages-cb110414c557c0928e82.log, unit-088cc38d26d094729229.log)
  TestStartManagedDoltSQLServerWithScopeWatchdogTerminatesWatchdogOnHandshakeFailure PASS (integration-packages-bc482d2ae768251ad47f.log, unit-cd6930bb0d0da8646500.log)
  TestStartManagedDoltSQLServerWithTestWatchdogSkipsReusedPIDOnHandshakeFailure PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-752a0bf25b15b465517a.log)
  TestStartManagedDoltSQLServerWithTestWatchdogTerminatesWatchdogOnHandshakeFailure PASS (integration-packages-509ef0a6cd81866833cc.log, unit-1cc8c8ab55e59a9e03a9.log)
GATE_TEST_EVIDENCE_RESULT ok: 4 diff-owned tests verified by name

Compact execution.json (large arrays elided by gate-record.sh; full data retained by pointer):

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-v98m6lg-20261119.27dez2tq/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":55.85530471801758,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":415,"remote_executed":415,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":13980.516,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":{"elided_array":{"items":1,"bytes":823}},"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":39464}},"log_dir":"/var/tmp/deploy-ga-v98m6lg-20261119.27dez2tq/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":509,"environment-absence":90},"top_level_counts_by_reason_class":{"other":251,"environment-absence":43},"tests":{"elided_array":{"items":599,"bytes":219773}}},"test_cmd":{"elided_array":{"items":5,"bytes":2099}},"test_counts":{"PASS":157110,"FAIL":0,"SKIP":613},"diff_tests_executed":"diff_tests_executed:\n  TestStartManagedDoltSQLServerWithScopeWatchdogSkipsReusedPIDOnHandshakeFailure PASS (integration-packages-cb110414c557c0928e82.log, unit-088cc38d26d094729229.log)\n  TestStartManagedDoltSQLServerWithScopeWatchdogTerminatesWatchdogOnHandshakeFailure PASS (integration-packages-bc482d2ae768251ad47f.log, unit-cd6930bb0d0da8646500.log)\n  TestStartManagedDoltSQLServerWithTestWatchdogSkipsReusedPIDOnHandshakeFailure PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-752a0bf25b15b465517a.log)\n  TestStartManagedDoltSQLServerWithTestWatchdogTerminatesWatchdogOnHandshakeFailure PASS (integration-packages-509ef0a6cd81866833cc.log, unit-1cc8c8ab55e59a9e03a9.log)\nGATE_TEST_EVIDENCE_RESULT ok: 4 diff-owned tests verified by name\n"}
```

## Fast lane, skip integrity and load

3b policy/format: 14 Bazel targets plus shell guards and check-hooks PASS. Generated drift: 7 targets PASS. OpenAPI breaking: 1 target PASS. BUILD sync: make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec followed by git diff --exit-code, rc0. Each lane used a separate fresh tree. Generated artifacts remained clean.

SKIP integrity: remote 599 events mapped to 316 unchanged sites; native 72 events mapped to 45 unchanged sites; zero changed/unmapped sites. The inventory counts describe all observed skip rows across legs and are not the execution counter above. All four changed production/test inputs were included in 51 phase/tag test-import closures. Of remote rows, 342 could not reach changed inputs; all 257 reached rows reproduced their reason at the pinned BASE. All 36 reached native rows also reproduced at BASE under the same Docker/Ryuk/pinned-bd environment. These supplementary named BASE measurements never substitute for or retry the primary suite.

COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a03, head rbe-a00)
COMPARE-SUMMARY head_skips=66 judged=66 pre-existing=66 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=191 judged=191 pre-existing=191 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=36 judged=36 pre-existing=36 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING

Observed load (verbatim wrapper summaries; remote-class admission does not wait on host load; actual native work waited separately):

```text
primary remote-class host telemetry: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=79.33 run_max_load=80.98 run_mean_load=49.44 run_readings=92 wait_first_load=79.33 wait_max_load=79.33 wait_mean_load=79.33 wait_readings=1 read_errors=0
native floor: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1260 wait_timed_out=0 run_start_load=14.60 run_max_load=14.00 run_mean_load=13.19 run_readings=6 wait_first_load=57.54 wait_max_load=78.01 wait_mean_load=49.92 wait_readings=43 read_errors=0
remote BASE auxiliary host telemetry: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=77.18 run_max_load=76.38 run_mean_load=73.03 run_readings=4 wait_first_load=77.18 wait_max_load=77.18 wait_mean_load=77.18 wait_readings=1 read_errors=0
native BASE auxiliary: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.76 run_max_load=11.79 run_mean_load=11.47 run_readings=2 wait_first_load=9.76 wait_max_load=9.76 wait_mean_load=9.76 wait_readings=1 read_errors=0
windows-lane native compile: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.62 run_max_load=11.01 run_mean_load=10.47 run_readings=5 wait_first_load=9.62 wait_max_load=9.62 wait_mean_load=9.62 wait_readings=1 read_errors=0
```

## Required CI closure (3e)

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-ga-v98m6lg.ysrqff 17d7d77ce29c8529f363a14c9b9873b73abc981a ba26bf543f068b6aa0b5b9b87b6145b0ed1f89f6 fork
workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612
6 LEAF jobs need a status; the rest are rolled up through their needs.

ci_required_accounting:

| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<file> python3 .github/workflows/scripts/runner_policy.py; rc=0; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/ci-required/runner-policy.log |
| changes | LOCAL-PASS | Closure tool command above rc0; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/ci-required/closure.txt |
| credential-provider-windows | NOT-TRIGGERED | credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true' |
| windows-lane-test | DEFERRED-TO-PR-CI | reason=non-linux-runner; resolved runs-on=blacksmith-4vcpu-windows-2025; own Windows GOOS test-import closure rc0, no changed inputs; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/ci-required/windows-lane-test-reach.json |
| windows-lane | LOCAL-PASS | Exact four workflow run blocks in windows-lane.sh, job env empty, rc0; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/ci-required/windows-lane-terminal.stdout and .stderr |
| pack-gate | NOT-TRIGGERED | pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true' |

Windows-lane commands (compile coverage only; job words/env match the workflow):

```sh
#!/usr/bin/env bash
set -euo pipefail
# Build the Windows allowlist
GOOS=windows go build $(cat .github/windows-lane/build.txt)
# Compile the Windows allowlist's tests
GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt)
# Build the nogo analyzers and their tests for Windows
(
cd tools/nogo
GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/...
)
# Build every package and its tests for macOS
GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./...
```

windows_reach: none for windows-lane-test (its own GOOS=windows/GOARCH=amd64 test-import closure, not the credential-provider helper); Windows vet top-up not required because no changed input reaches that job.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none

PASS authorizes PR-open. Merge requires CI / required green on the exact PR head and windows-lane-test success (or a legitimate path-filter skip). MPR watches PR CI under the handoff protocol; a newer head requires a fresh gate.

## Retained evidence

Evidence root: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1. Raw run paths beginning /var/tmp/deploy-ga-v98m6lg-20261119.27dez2tq map to the same relative path under that durable root. Raw per-job logs are retained. All 1082 downloaded CAS artifacts (84,757,180 bytes) were independently rehashed against SHA256 and blob URI hash/size. Auxiliary symlink directories are recorded in archive-symlink-map.json; their primary data is retained once.

Full review record retained: 85 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/review-prepublication.json; SHA256 a5e4b7e51abac0ded812dc61a7e3c7e9fed85fe0b3e56bf100ea5b8975faab52.
Full acceptance mapping retained: 29 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/acceptance-mapping.json; SHA256 0411247a707bf922a078e8ff0988f8f43127f970673b82f6b1a9cc50f04b16ea.
Full full execution retained: 6000 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/c3-run/evidence/execution.json; SHA256 3a509fbd849417b5a62c06efa4d0578d9a8b27382cefe1546be9ebdcf04063be.
Full fast-lane audit retained: 40 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/fast-verified.json; SHA256 8defa3380d76b7ebd3b97c1dd64e2dd52026bd71a3d2feb5bbc9dc512aea6be6.
Full C6 build BEP retained: 5851 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/c6-build.bep.json; SHA256 b6571ff5e586ecab86dd452221a7637c69b9cfa2656db71b66c0be2ca4405ade.
Full C6 nogo BEP retained: 4041 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/c6-vet.bep.json; SHA256 42632de27295f850102c7b5466431041558506492ca085dcf738ecae97ca4077.
Full remote SKIP inventory retained: 4794 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/c3-run/evidence/remote/skip-inventory.json; SHA256 9d58e79d9843c341e5e766d962aca246ed1b69ddc3d39f786ce901c24f7942f3.
Full native SKIP inventory retained: 578 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/native-skip-inventory.json; SHA256 f7ffa5d835f53a0ec8bbb9ef8bb2e2babf2905c6b393cebc0b93046395db8a74.
Full remote SKIP site audit retained: 2432 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/remote-skip-audit.json; SHA256 eccf999e0381609c9bd40e1b527646ee80b69bcebff6ee037e5868bdd2c1b662.
Full native SKIP site audit retained: 303 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/native-skip-audit.json; SHA256 a5f6ace357ffce95fd402a1c0daf53511fc2b487df73f955b3f9eabb46779b06.
Full phase/tag import reach plan retained: 10360 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/skip-measure-plan.json; SHA256 2a87785255ce2091d758d6599e4d902ee2a3dcaabd2b037030fdcccc61d0f83e.
Full BASE SKIP comparisons retained: 460 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/skip-comparisons.json; SHA256 a10bbbb153f136f6e96d65bcf49af87e1ae5acb8dff6ce8f4b4d45b55d8d3f5b.
Full CI accounting retained: 100 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/ci-required/accounting.json; SHA256 7222a67f799ac1a44d20a6518b1d11c18655a90b5a24aa7804cd05f060b5540f.
Full owned tests by name retained: 34 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/owned-unit-verified.json; SHA256 379da7348c168b1354a54e41356041d64038d1f5287f5881c9f351521f18ca77.
Full CAS artifact verification retained: 12 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/cas-artifacts-verified.json; SHA256 7377dca1b5dbc8092f725078fe1ad34ae9d7f356d522c18345d34d5e9589e996.
Full tested merge bundle retained: 26 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/tested-merge.bundle; SHA256 92331351ce51742c5ddd966a2a46097f84ec6a701cd4d3361933e7f8ec6d324e.
Full scratch cleanup proof retained: 6 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/merge-scratch-cleaned.json; SHA256 78d8fe98d979fdbf86c898fc865cd167e9a602a93bb0f21e664c1172fb017957.
Full normal publication hook selection retained: 22 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/publication-worker-pin-selection.json; SHA256 00cf6b45d034a46ab2104587940f6665b954b0e8490399a13532d28a4f425cda.

## Publication hook attribution

The ordinary bare-branch pre-push ran its full Bazel suite with fork-cache, completing rc1: 288/289 targets PASS; only TestDatedWaiversAgainstToday failed on historical catalog/census waivers. This is not a criterion-3 failure: that same test PASSed by name in the complete canonical current-base run above.

failure_attribution: TestDatedWaiversAgainstToday -> ga-5550q2dx | clause 3: a MECHANISM — full module test-import closure rc0 contains no changed cmd/gc package or production input. Clause1: failing test/function untouched. Clause2: existing tracker opened; its 2026-11-18 sighting explicitly names this centralized waiver-clock test and stale-catalog condition. Clause4: no package/path overlap. First such hit on this deploy bead. The landed renewal 08190f2b9975f9415fd0621483b6a54961f1379d is in the pinned gate base, absent from the reviewed source. The normal hook's failure is retained, never retried into green.

The standing pre-push exception in non-diff-owned-gate-failure.md.tmpl permits a specific-head no-verify push after this attribution. It grants no waiver and changes no source; live ownership, exact source ancestry, branch safety, gate size and clean status are checked separately before that push. This record-only descendant retains the reviewed source unchanged. No clearance is carried across a changed source head.

Full pre-push attribution retained: 39 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/hook-failure/attribution.json; SHA256 ecd2d47c26503e7eb1e321a6f83f65e6dd0a5608e2ebd3e5c48acf9819bca812.
Full pre-push failed-test log retained: 65 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/hook-failure/waiverexpiry.log; SHA256 df97bc04a2265c43baaa5917c76655208db34077c81d1956cc3ae3ceee6f7864.
Full test-import closure retained: 22 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/hook-failure/import-closure.stdout; SHA256 112b838fa368d06af8445a74c4ff3c35ddeafaf2c54ad6a28764e2064dee840f.
Full full normal pre-push output retained: 370 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/push-terminal.stdout; SHA256 00f2aa1feaa98da1fdb19f30fb02a08e89b88e98f9d8b3c402deca78b7a1fddf.
Full full normal pre-push stderr retained: 559 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-v98m6lg/gate-20261119T030946Z-843mxei1/push-terminal.stderr; SHA256 b6d38ac34a05178dde90187493f625ccbf23b0e2b2f9390aee02ebd1e02da64a.
