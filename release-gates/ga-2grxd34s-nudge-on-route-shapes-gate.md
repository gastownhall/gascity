**Verdict:** **PASS**

# Route-event shape compatibility — release gate

Bead: `ga-2grxd34s`; build: `ga-5bn7yx`; rework: `ga-9662d30`; review: `ga-ca8s0k17`.
Reviewed source: `77f738145cf652099ad5834f9d9ce9a2a0747ebd`. Tested merge: `8ee54f5e6a8619138bbe2d7fcef72c526809a138`; tree: `b19a9b072d203730243a88247f998b38cc7b1c1b`.
gate_base: `17d7d77ce29c8529f363a14c9b9873b73abc981a`
merge_state: clean (`origin/main@17d7d77ce29c8529f363a14c9b9873b73abc981a`)
gate_base_kind: current-tip
merge_validation: exact clean merge built and vetted; merge authority validates eventual merge.
deploy_mode: remote; push_remote: fork; isolated branch: `deploy/ga-2grxd34s-gate`.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Closed final reviewer PASS metadata and notes bind to the resolved reviewed source. |
| 2 | PASS | Nested and flat routing events each nudge once, duplicate shapes deduplicate, and unrouted/empty targets do not nudge. Required PR explanation below is independently checked against code and history. |
| 3 | PASS | Full-suite 157106 PASS / 0 FAIL / 613 SKIP; both diff-owned tests PASS; required skip comparisons and six-leaf CI accounting complete. |
| 4 | PASS | No unresolved HIGH findings. P3 hardening follow-up ga-8j6fmsae remains open; no waiver. |
| 5 | PASS | Tested merge and source checkout clean; publication adds only this record, with cleanliness checked before push. |
| 6 | PASS | Whole-tree Bazel build and nogo output-group build exit 0; both BEPs overallSuccess=true. |
| 7 | PASS | One behavior: accept both existing bead.updated payload shapes in the core route-event consumer, with its tests and BUILD source registration. |

Criterion 6 commands on the tested merge (both exit 0):
```text
bazel build --config=remote-exec --keep_going --noremote_local_fallback //...
bazel build --config=remote-exec --keep_going --noremote_local_fallback --output_groups=nogo_fix //...
```

## Acceptance evidence and limits

The typed API envelope printed by `gc events` serializes the bead under `.payload.bead`; `CachingStore.notifyChange` writes a raw bead snapshot to `.gc/events.jsonl`. Earlier #4548 targeted the flat snapshot. Both formats remain in use, so normalize both rather than picking one.

Independently inspected `internal/api/event_payloads.go` (BeadEventPayload.Bead), `internal/beads/caching_store_events.go` (notifyChange/emitChange), and `internal/beads/event_payload.go` (EncodeBeadEventPayload raw json.Marshal; DecodeBeadEventPayload accepts raw and wrapped). Local git history resolves c43feb6b0547410524c3ecda486a097fda4813e1 (#4548 flat selector) and d9225156f9b30fa4fe7fee28c30699ee2cc8cc3d (shared raw-first decoder). Neither historical PR was opened or touched. This explanation is mandatory in the public PR body and verified after publication, before close/handoff.

`TestNudgeOnRouteMatchesEveryBeadUpdatedShape` runs the real script against hand-reduced nested and flat event fixtures using a fake gc; its third subtest presents the same routing in both formats and proves exactly one nudge. `TestNudgeOnRouteIgnoresUnroutedBeads` checks absent and empty routing targets in both formats. Both tests require jq and actually PASS, rather than SKIP, in this gate. Existing state-file deduplication remains in place. Tests use disposable state and fake nudges, not production-city traffic.

This city has nudge-on-route disabled in its orders skip list; the deploy does not enable it. On a city where the order runs, the first scan can nudge routed beads in the default two-minute lookback, then state-file deduplication applies with one-hour retention.

Nonblocking limits retained in open P3 ga-8j6fmsae: a malformed last event can cause a jq runtime error and discard the batch through pipefail plus the existing empty fallback; current typed producers cannot produce that malformed payload. A third unknown event shape still matches nothing without diagnostics. The new test comment says captured, although the fixtures are hand-reduced. No fix or waiver for those limits is claimed.

## Full-suite evidence

test_cmd_scope: full-suite
test_exec_mode: remote-exec
heavy_mode: none
failure_attribution: none
waiver_ref: none
test_log_dir: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/c3-run/evidence/remote/logs` (raw per-job logs retained).

Actual invocation:
```text
env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-2grxd34s DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-ga-2grxd34s.GEcnCp --base 17d7d77ce29c8529f363a14c9b9873b73abc981a --bead-json /var/tmp/deploy-ga-2grxd34s.edc2f119/bead-c3.json --owned /var/tmp/deploy-ga-2grxd34s.edc2f119/owned.tsv --run-dir /var/tmp/deploy-ga-2grxd34s.edc2f119/c3-run/evidence
```
Five forced-uncached remote phases followed by the computed native floor. Phase commands:
```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-2grxd34s.edc2f119/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-2grxd34s.edc2f119/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-2grxd34s.edc2f119/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-2grxd34s.edc2f119/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-2grxd34s.edc2f119/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```
diff_tests_executed:
  TestNudgeOnRouteIgnoresUnroutedBeads PASS
  TestNudgeOnRouteMatchesEveryBeadUpdatedShape PASS
GATE_TEST_EVIDENCE_RESULT ok: 2 diff-owned tests verified by name

Native floor: 16 steps, all exit 0. ICU 74 provenance and rootless podman socket ping passed; no testcontainers dependency or pinned Dolt image was found. Pinned test bd and wrapper behavior are retained. The SDK wrapper reports PASS-THROUGH, NOT ISOLATED: fresh source views and Bazel sandboxing are the isolation used, not a claimed container boundary. Reviewer evidence is not this gate's suite evidence.

Compact execution record; original commands, population arrays and logs remain retained:
```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-2grxd34s.edc2f119/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":63.70740509033203,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":415,"remote_executed":415,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":14345.664,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":73.445,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=28.02 load5=32.68 load15=24.74 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 20.04 29.92 24.14 9/1695 55405"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38616}},"log_dir":"/var/tmp/deploy-ga-2grxd34s.edc2f119/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":509,"environment-absence":90},"top_level_counts_by_reason_class":{"other":251,"environment-absence":43},"tests":{"elided_array":{"items":599,"bytes":214981}}},"test_counts":{"PASS":157106,"FAIL":0,"SKIP":613}}
```

## Skip integrity

skip_justification: no diff-owned SKIP. Every remote/native skip maps to an unchanged source site with line_in_hunk=no and its reason retained. Ownership recomputed at the tested merge equals ownership at reviewed source. The ownership helper prints no SHARED-HUNKS or HELPER-HUNKS for this change. Conservatively, reach also includes the modified embedded production script (nearest enclosing Go package internal/bootstrap/packs/core) and the new test-file constants. The script is not treated as unreachable merely because its suffix is .sh. Each skipped test binary's import closure is computed under its actual job tag set. Reached skips are measured by name at this gate's pinned base with that job's flags and environment; comparisons supplement the full suite.

remote: 599 skip events, 316 unchanged sites; no unmapped or changed skip sites.
native: 72 skip events, 45 unchanged sites; no unmapped or changed skip sites.
Changed shared inputs conservatively included in reach:
- `internal/bootstrap/packs/core/assets/scripts/nudge-on-route.sh`
- `internal/bootstrap/packs/core/nudge_on_route_script_test.go`
Computed closure pairs: 51; reached remote skip events: 487; unreached remote events: 112; native measurement jobs: 3.
COMPARE-SUMMARY head_skips=14 judged=14 pre-existing=14 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=14 judged=14 pre-existing=14 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a09, head rbe-a11)
COMPARE-SUMMARY head_skips=103 judged=103 pre-existing=103 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-NOTE a generated path name differs between the runs in 1 SKIP(s), read as the same reason because it is the only difference and the same file and line were printed (a generated name is the digits Go appends to a temp directory's name, or a UUID right after a /work/ directory, which is how a Bazel remote worker names its work directory; the logs are not changed): TestProxyProcessReapRegistryMatchesAnEquivalentSpelling (base 3787461704, head 2519348325)
COMPARE-SUMMARY head_skips=356 judged=356 pre-existing=356 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=36 judged=36 pre-existing=36 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=4 judged=4 pre-existing=4 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=4 judged=4 pre-existing=4 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING

Native comparison setup correction: the original three BASE native commands passed, but their driver omitted the DOCKER_HOST and TESTCONTAINERS_RYUK_DISABLED values used by the head. Those original logs remain retained and are excluded from the proof. Only the three named BASE measurements were repeated with the exact values read from the head command argv. Corrected results and load are retained separately; comparisons above use those corrected logs. No failed test or full suite was repeated. The full failure protocol was read before correction.

Existing platform, privilege, opt-in, process-tier and fixture predicates are unchanged. Required measurements repeat the same skip reasons; any permitted generated-path or remote-account normalization is shown in the COMPARE-NOTEs above. Unreached entries have a computed import-closure proof. No failed test or suite was repeated into PASS.

## Fast lane and load

policy_lane: shell guards and 14 Bazel format/policy targets PASS in a fresh view; changed script bash syntax checked, shellcheck runs when installed (raw policy output records availability).
drift_lane: seven generated-artifact targets, OpenAPI compatibility, make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec and git diff --exit-code all PASS in separate fresh views. Fast lane completed before suite setup/launch. No API/spec/dashboard source changed, so no additional dashboard preview is required.

known_red_precheck:
```text
known-red-precheck: target=check-routed-test-rows verdict=green merge=0 base=- (it passes on the merge tree)
known-red-precheck: stale: tracker=ga-1ptv4jey `make check-routed-test-rows` passes on this merge tree; the tracker may be ready to close
known-red-precheck: proceed reason=nothing-known-red (check-routed-test-rows=green)
```
Native-floor load:
```text
load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=9.10 run_max_load=9.85 run_mean_load=8.79 run_readings=6
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.10 run_max_load=9.85 run_mean_load=8.79 run_readings=6 wait_first_load=9.10 wait_max_load=9.10 wait_mean_load=9.10 wait_readings=1 read_errors=0
```
Named base-measurement load:
```text
load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=8.40 run_max_load=8.48 run_mean_load=7.88 run_readings=9
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=8.40 run_max_load=8.48 run_mean_load=7.88 run_readings=9 wait_first_load=8.40 wait_max_load=8.40 wait_mean_load=8.40 wait_readings=1 read_errors=0

Matched-environment native comparison continuation:
load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=8.56 run_max_load=NA run_mean_load=NA run_readings=0
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=8.56 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=8.56 wait_max_load=8.56 wait_mean_load=8.56 wait_readings=1 read_errors=0
```
Own CI job loads:
```text
windows-lane
load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=8.46 run_max_load=9.39 run_mean_load=9.07 run_readings=5
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=8.46 run_max_load=9.39 run_mean_load=9.07 run_readings=5 wait_first_load=8.46 wait_max_load=8.46 wait_mean_load=8.46 wait_readings=1 read_errors=0
```

## Required-job accounting

ci_required_closure: ["python3", "/home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py", "/var/tmp/gc-merge-ga-2grxd34s.GEcnCp", "17d7d77ce29c8529f363a14c9b9873b73abc981a", "77f738145cf652099ad5834f9d9ce9a2a0747ebd", "fork"]; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs.
```text
diff 17d7d77ce29c8529f363a14c9b9873b73abc981a...77f738145cf652099ad5834f9d9ce9a2a0747ebd: 3 changed files
  filter packs                no match
  filter credential_provider  no match
  filter shared               no match
suite_mode: filtered  (shared filter did not match)

ci-required needs 7 jobs; transitive closure 7 jobs
job key                                  kind       runs-on                    trigger
runner-policy                            LEAF       linux                      EVERY-PR
changes                                  LEAF       linux                      EVERY-PR
check                                    AGGREGATOR linux                      EVERY-PR
  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
  pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
  windows-lane                           LEAF       linux                      EVERY-PR
  windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR

6 LEAF jobs need a status; the rest are rolled up through their needs.
```
ci_required_accounting:

| Job | Status | Command / evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | {"command": "EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<file> python3 .github/workflows/scripts/runner_policy.py", "log": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/ci-required/runner-policy.log", "rc": 0} |
| changes | LOCAL-PASS | {"evidence": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/ci-required/closure.txt"} |
| credential-provider-windows | NOT-TRIGGERED | {"evidence": "credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'"} |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | {"proof": "ci-required/accounting.json and per-job reach files (hashed below)", "reach_rc": 0, "resolved_runs_on": "blacksmith-4vcpu-windows-2025", "windows_reach": []} |
| windows-lane | LOCAL-PASS | {"command_file": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/windows-lane.sh", "rc": 0, "resource_env": {"GOFLAGS": "-p=2", "GOMAXPROCS": "2", "LOCAL_TEST_LIMIT": "2"}, "run_dir": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/ci-required/windows-lane", "stderr": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/ci-required/windows-lane-terminal.stderr", "stdout": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/ci-required/windows-lane-terminal.stdout"} |
| pack-gate | NOT-TRIGGERED | {"evidence": "pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'"} |

windows_reach: windows-lane-test: none.
Windows vet: not required (own-job reach empty).
deferred_to_pr_ci: windows-lane-test (non-linux-runner).
not_covered: none.
ci_lane_run: n/a (no CI job, matrix, timeout or required-check-list changes).

Exact CI compile-job commands and triggered pack job when applicable:
```bash
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
This PASS authorizes PR-open. Exact-head CI / required and every deferred job must succeed before MPR merges.

## Publication hooks

The reviewed source predates main's worker-environment re-pin. Before launching any publication hook, pins were compared and the documented fork-cache mode selected for normal Bazel precommit docs and full prepush against the unchanged source. Primary criterion 3 remains strictly remote on the tested merge. Own local scheduling is capped at two CPUs, two test jobs and 8 GiB, with exact original rc restoration after delivery. No rebase, failed-test rerun, waiver or hook bypass is involved.

## Retained evidence

Durable evidence: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76`; original run: `/var/tmp/deploy-ga-2grxd34s.edc2f119`. Verified Git bundles retain tested merge and gate-only commit.

Full merge build BEP retained: 5851 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/c6-build.bep.json; SHA256 be92ad64f692690c9601f2c085f0db3d9e08141ee76e6b45a861d475c12b8d12.
Full merge nogo BEP retained: 4041 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/c6-vet.bep.json; SHA256 01dea8bb024b2b6b83e9bcdb20635b74eb268a79cb96d1acbcc9c50e6ea02710.
Full merge build driver retained: 1 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/c6-terminal.stdout; SHA256 b412894777c125e12117008a96979f56bad973c01b1b978c567daa2ee0b7cfd8.
Full execution retained: 5999 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/c3-run/evidence/execution.json; SHA256 56f821bd1b9edbfc41a136996251c7c5490c7c0660de36e45c7aea1e0149374d.
Full execution projection retained: 5991 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/execution-record-summary.json; SHA256 21ce956f101b17732db97ed1bb576fb472e604d28084ac1059a2576b1e16cea2.
Full remote skip inventory retained: 4794 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/c3-run/evidence/remote/skip-inventory.json; SHA256 f8399b486294acac3d36a12f0bf673876cb680599945857e885a2b3dd3b10e50.
Full native skip inventory retained: 578 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/native-skip-inventory.json; SHA256 bb8acc87289b2ae56fdd25f250b9acef3e19df8ccb3f3e0d90feb49ede646755.
Full remote sites retained: 1898 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/remote-skip-site-results.json; SHA256 fedd2110d4ea72b693a2db624a246345139ca4ccecd7381b7640902d89dee81c.
Full native sites retained: 272 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/native-skip-site-results.json; SHA256 3cb44fb048a62597718ad397e3011157087efb4aaaafbf22db37136032c5c7a7.
Full changed-input reach and closures retained: 11763 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/skip-measure-plan.json; SHA256 b07ce9c18f6cfb3797a9ec7005584ee5eb8e6ab8f97eaaa7cd406cd37cfc09a5.
Full base comparison verdicts retained: 775 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/skip-comparisons.json; SHA256 1efbc20c52df4db1643f2ab376b2319074052ee03dc6a17d29d39a2edb563864.
Full native comparison setup correction retained: 18 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/native-base-environment-correction.json; SHA256 44f0e66ba87eb5972192e87e52906518a72668c4f224b913e49f0cb6cccc39c3.
Full matched-environment native measurements retained: 94 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/base-skips-native-matched/native-results.json; SHA256 562506c6008c3ca166956a0f2dfd755f32764a2604275ecdbab674f819da655d.
Full matched-environment load retained: 9 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/native-base-matched-terminal.stderr; SHA256 c8a1c6581810d34bba48b53c5c098934e3ea7f6713ea042ba4551525d421f17e.
Full base remote commands and results retained: 368 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/base-skips/remote-results.json; SHA256 5ab7025b1d9dd1ab68cf718791d45328a55b115dd2041fc19891feb31fabe4f6.
Full CI accounting retained: 100 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/ci-required/accounting.json; SHA256 3e82907e3bd507d7b01b834a9eecae26c16599e6e5bf4233f6e261307c299c3a.
Full Windows native-job reach retained: 36 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/ci-required/windows-lane-test-reach.json; SHA256 c500cb29f7c9a6d5fea7538fe44dd1b349aeaaf7e754831019ce03b462ca9ecc.
Full publication worker-pin selection retained: 24 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/publication-worker-pin-selection.json; SHA256 e5575c2e30fa0be8021f27a54a4ec7c0fbe46a7ead14ef48a87ffdd62e8b3071.
Full event producer contracts retained: 132 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/event-contract-source.txt; SHA256 50c2b3f3d1cd481e171d5d8f2fd255f0dbc8a9cfc42d904e685ed01d1ed1f836.
Full raw bead encoder retained: 22 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/event-contract-encoder.txt; SHA256 ca47021f0afc6e3a90e7b4326669aab2607459b47385925b0aa4d168fc5f79bc.
Full resolved history SHAs retained: 10 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-2grxd34s/gate-20261119T012620Z-j4rfht76/history-shas.json; SHA256 2b4bb1bd60e4762e164241bd2c65a40fc1ef53a20cf3d4a720e92ab46754bfb1.
