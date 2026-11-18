**Verdict:** **PASS**

# Missing base-ref refusal — release gate

Bead: `ga-803wmu1r`; build: `ga-c1gvvf`; rework: `ga-btqbtato`; review: `ga-6oxsynz4`.
Reviewed source: `ae6711a9985beea5c2050078e5555475815c0772`. Tested merge: `0c17f8de33f381beac3375df26a0302e0003832e`; tree: `992583b97fe227bb6b92528a3ecbb02a02c0959d`.
gate_base: `fab28e15915987aa5fcfdfb94512d104986726b6`
merge_state: clean (`origin/main@fab28e15915987aa5fcfdfb94512d104986726b6`)
gate_base_kind: current-tip
merge_validation: the exact clean merge built and vetted; the merge authority validates the eventual merge.
deploy_mode: remote; push_remote: fork; isolated branch: `deploy/ga-803wmu1r-gate`.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Closed reviewer PASS binds to the resolved source; additive reviewer-test boundary independently verified. |
| 2 | PASS | Confirmed-missing-ref refusal precedes formula, routing and reassign writes; resolvable, absent and undecidable cases retain behavior. Reviewed limits below. |
| 3 | PASS | Full-suite 156928 PASS / 0 FAIL / 613 SKIP; all 10 diff-owned tests PASS; required skip measurements, dashboard checks and six-leaf CI accounting complete. |
| 4 | PASS | Zero unresolved HIGH findings. Review's non-blocking observations remain documented; no waiver. |
| 5 | PASS | Tested merge and source checkout clean; publication adds only this record, with post-commit cleanliness checked before push. |
| 6 | PASS | Whole-tree Bazel build and nogo output-group build exit 0; both BEPs show overallSuccess=true. |
| 7 | PASS | One behavior: reject a confirmed-missing base ref before sling side effects. Git probe, domain preflight and CLI/API adapters implement it together. |

Criterion 6 commands on the tested merge (both exit 0, both BEPs overallSuccess=true):
```text
bazel build --config=remote-exec --keep_going --noremote_local_fallback //...
bazel build --config=remote-exec --keep_going --noremote_local_fallback --output_groups=nogo_fix //...
```

## Acceptance evidence and limits

reviewer_tests_verified: 19a808052a7764aa06b7ba763d5385cf2ca30917 -> ae6711a9985beea5c2050078e5555475815c0772 files=internal/git/worktree_ops_test.go. The helper proved a single directly descending, labelled, additive test-only commit. The review metadata names the build input; its PASS notes explicitly bind to the reviewed source including those added tests.

- AC1: the read-only Git probe tries `origin/<ref>` then the literal commit-ish and reports missing only for git exit status 1 on both. The domain check names the ref and repository basename and says not to retry unchanged. No fetch or semantic prerequisite inspection is added.
- AC1 side effects: the standalone, --on, --reassign, default-formula and dry-run shapes are covered by `TestDoSlingRefusesUnresolvableBaseBranchInEveryFormulaShape` and its six subtests. The check precedes the reassign reopen. Batch preflight checks every open child before routing any child. Refusal, batch and CLI/API adapter tests PASS in the full suite.
- AC2: resolvable refs, no computed base_branch, nil resolver and plain-bead routing remain accepted. `TestSlingAcceptsResolvableBaseBranch`, `TestSlingWithoutBaseBranchOrResolverIsUnchanged`, and `TestDoSlingPlainBeadIgnoresBaseBranch` PASS.
- The reviewer-added `TestBaseRefMissingIsFalseWhenGitFailsAnotherWay` proves the undecidable arm: a healthy repo first reports a missing ref; then a damaged packed-refs file leaves IsRepo true but the verify probe exits 128, so BaseRefMissing must return false. It PASSes along with `TestBaseRefMissing`.
- Scope is the reviewed missing-ref tier. The earlier proposed fallback warning and inheritance from review prose were not implemented. A valid main ref lacking prerequisite code still passes, including the motivating be-aasu6 scenario; no claim is made that this semantic prerequisite problem is solved.

Review limits retained: local refs only, so a newly pushed but unfetched branch is refused until fetched. Auto-stamped defaults are checked too and can refuse a default absent from the clone. Batch probes are repeated per child. Refusal is a plain exit-1 error. HEAD, @ and other resolving Git revision expressions pass. A leading-dash value is treated as undecidable by the existing ref-argument validator and can reach downstream formula arguments; this check is not a sanitizer. The refusal echoes two ref copies without full quoting, allowing terminal/log control characters from an already-privileged caller. Review classified these as non-blocking, with no new privilege. No waiver or fix to those limits is claimed.

## Full-suite evidence

test_cmd_scope: full-suite
test_exec_mode: remote-exec
heavy_mode: none
failure_attribution: none
waiver_ref: none
test_log_dir: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/c3-run/evidence/remote/logs` (all raw per-job logs retained).

Actual invocation:
```text
env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-803wmu1r DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-ga-803wmu1r.BiVeFV --base fab28e15915987aa5fcfdfb94512d104986726b6 --bead-json /var/tmp/deploy-ga-803wmu1r.lpiuy2co/bead-c3.json --owned /var/tmp/deploy-ga-803wmu1r.lpiuy2co/owned.tsv --run-dir /var/tmp/deploy-ga-803wmu1r.lpiuy2co/c3-run/evidence
```
Five forced-uncached remote phases, followed by the computed native floor. Phase commands:
```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-803wmu1r.lpiuy2co/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-803wmu1r.lpiuy2co/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-803wmu1r.lpiuy2co/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-803wmu1r.lpiuy2co/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-803wmu1r.lpiuy2co/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```
diff_tests_executed:
  TestCLIBranchResolverBaseRefMissing PASS
  TestApiBranchResolverBaseRefMissing PASS
  TestBaseRefMissing PASS
  TestBaseRefMissingIsFalseWhenGitFailsAnotherWay PASS
  TestDoSlingBatchRefusesUnresolvableBaseBranch PASS
  TestDoSlingPlainBeadIgnoresBaseBranch PASS
  TestDoSlingRefusesUnresolvableBaseBranchInEveryFormulaShape PASS
  TestSlingAcceptsResolvableBaseBranch PASS
  TestSlingRefusesUnresolvableBaseBranch PASS
  TestSlingWithoutBaseBranchOrResolverIsUnchanged PASS
GATE_TEST_EVIDENCE_RESULT ok: 10 diff-owned tests verified by name

Native floor: 16 steps, all exit 0. ICU 74 provenance check and rootless podman socket ping passed. Pinned test bd and wrapper behavior are retained in the raw logs. The reviewer's cached/mixed evidence is not used as this gate's suite evidence.

Compact execution record (command and per-test prose shown above); large arrays and the original record are retained by pointer:
```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-803wmu1r.lpiuy2co/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":66.94169235229492,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":415,"remote_executed":415,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":14494.52,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":200.805,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=8.19 load5=15.83 load15=18.36 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 49.54 29.11 22.94 12/1745 59956"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38616}},"log_dir":"/var/tmp/deploy-ga-803wmu1r.lpiuy2co/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":509,"environment-absence":90},"top_level_counts_by_reason_class":{"other":251,"environment-absence":43},"tests":{"elided_array":{"items":599,"bytes":214981}}},"test_counts":{"PASS":156928,"FAIL":0,"SKIP":613}}
```

## Skip integrity

skip_justification: no diff-owned SKIP. Every remote/native skip maps to an unchanged source site with line_in_hunk=no and its reason retained. Ownership recomputed at the actual tested merge equals the reviewed source's ownership. Import reach is computed for each skipped test binary under its tag sets; every reached shared/helper hunk triggers a named comparison at this gate's pinned base, using that head job's command, flags and environment. These comparisons measure skips; the full-suite run above remains the suite evidence.

remote: 599 skip events, 316 unchanged sites; no unmapped or changed skip sites.

native: 72 skip events, 45 unchanged sites; no unmapped or changed skip sites.

SHARED-HUNKS entries:
- `internal/sling/sling_base_branch_test.go`
- `internal/sling/sling_test.go`

Computed closure pairs: 51; reached remote skip events: 260; unreached remote events: 339; native measurement jobs: 1.

COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a08, head rbe-a09)

COMPARE-SUMMARY head_skips=66 judged=66 pre-existing=66 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING

COMPARE-SUMMARY head_skips=194 judged=194 pre-existing=194 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING

COMPARE-SUMMARY head_skips=36 judged=36 pre-existing=36 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING

The shared entries add the sling base-ref fixture/helper and implement the method on the existing test resolver. Each entry and its import reach are retained. Existing platform, privilege, opt-in, process-tier and fixture predicates are unchanged; measured skips repeat for the same reasons. The new Git method is read only and is called only through the two adapters and sling preflight. Its addition changes no global Git configuration or existing skip predicate. No suite or failed test was repeated into PASS; original logs/inventories are untouched.

## Fast lane, dashboard and load

policy_lane: shell guards and 14 Bazel format/policy targets, all exit 0 through gate-base.sh run in a fresh view.
drift_lane: seven generated-artifact targets, OpenAPI compatibility and make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec plus git diff --exit-code, all exit 0 in separate fresh views. Fast lane finished before suite setup/launch.

Dashboard setup correction: the initial command supplied an undefined `--config=ci`; Bazel rejected options before analysis, tests, npm build or preview. The failed setup remains retained. The corrected command removed that deployer-supplied flag and ran the unspent checks in a fresh view. The original 1801-second bounded wait had completed; continuation added no further wait and records its current load separately. This is an invocation correction, not attribution, waiver, or a test rerun into PASS.

dashboard-check: make dashboard-check BAZEL_FLAGS="--config=remote-exec --noremote_local_fallback" PASS; make dashboard-build-npm PASS; actual npm preview served the app at an ephemeral localhost port. DASHBOARD_PREVIEW_HTTP_PASS url=http://127.0.0.1:48387/ html_sha256=d335d94368797417f317dd5e760a5ce0c4d0d607514002e8425f4b52de70a2d4; owned process-group cleanup verified. All ran in a separate fresh view, never the suite scratch.

known_red_precheck:
```text
known-red-precheck: target=check-routed-test-rows verdict=green merge=0 base=- (it passes on the merge tree)
known-red-precheck: stale: tracker=ga-1ptv4jey `make check-routed-test-rows` passes on this merge tree; the tracker may be ready to close
known-red-precheck: proceed reason=nothing-known-red (check-routed-test-rows=green)
```
Native-floor load:
```text
load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=14.16 run_max_load=14.96 run_mean_load=14.48 run_readings=7
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.16 run_max_load=14.96 run_mean_load=14.48 run_readings=7 wait_first_load=14.16 wait_max_load=14.16 wait_mean_load=14.16 wait_readings=1 read_errors=0
```
Named base-measurement load:
```text
base-skips
load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=13.87 run_max_load=28.67 run_mean_load=17.89 run_readings=6
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.87 run_max_load=28.67 run_mean_load=17.89 run_readings=6 wait_first_load=13.87 wait_max_load=13.87 wait_mean_load=13.87 wait_readings=1 read_errors=0
```
Dashboard quality/preview load:
```text
Initial setup window (no tests ran):
load_threshold=15 load_waited_seconds=1801 load_wait_timed_out=1 run_start_load=78.71 run_max_load=NA run_mean_load=NA run_readings=0
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=78.71 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=39.15 wait_max_load=78.71 wait_mean_load=51.43 wait_readings=61 read_errors=0

Corrected continuation (original1801s wait spent;additional wait budget0):
load_threshold=15 load_waited_seconds=0 load_wait_timed_out=1 run_start_load=84.46 run_max_load=88.84 run_mean_load=86.82 run_readings=3
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=1 run_start_load=84.46 run_max_load=88.84 run_mean_load=86.82 run_readings=3 wait_first_load=84.46 wait_max_load=84.46 wait_mean_load=84.46 wait_readings=1 read_errors=0
```
Own CI job loads:
```text
windows-lane
load_threshold=15 load_waited_seconds=840 load_wait_timed_out=0 run_start_load=14.58 run_max_load=14.46 run_mean_load=13.83 run_readings=5
LOAD_GATE_SUMMARY threshold=15 waited_seconds=840 wait_timed_out=0 run_start_load=14.58 run_max_load=14.46 run_mean_load=13.83 run_readings=5 wait_first_load=78.50 wait_max_load=78.50 wait_mean_load=42.71 wait_readings=29 read_errors=0
```

## Required-job accounting

ci_required_closure: ["python3", "/home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py", "/var/tmp/gc-merge-ga-803wmu1r.BiVeFV", "fab28e15915987aa5fcfdfb94512d104986726b6", "ae6711a9985beea5c2050078e5555475815c0772", "fork"]; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs.
```text
diff fab28e15915987aa5fcfdfb94512d104986726b6...ae6711a9985beea5c2050078e5555475815c0772: 12 changed files
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
| runner-policy | LOCAL-PASS | {"command": "EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<file> python3 .github/workflows/scripts/runner_policy.py", "log": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/ci-required/runner-policy.log", "rc": 0} |
| changes | LOCAL-PASS | {"evidence": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/ci-required/closure.txt"} |
| credential-provider-windows | NOT-TRIGGERED | {"evidence": "credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'"} |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | {"proof": "ci-required/accounting.json and per-job reach files (hashed below)", "reach_rc": 0, "resolved_runs_on": "blacksmith-4vcpu-windows-2025", "windows_reach": []} |
| windows-lane | LOCAL-PASS | {"command_file": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/windows-lane.sh", "rc": 0, "resource_env": {"GOFLAGS": "-p=2", "GOMAXPROCS": "2", "LOCAL_TEST_LIMIT": "2"}, "run_dir": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/ci-required/windows-lane", "stderr": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/ci-required/windows-lane-terminal.stderr", "stdout": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/ci-required/windows-lane-terminal.stdout"} |
| pack-gate | NOT-TRIGGERED | {"evidence": "pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'"} |

windows_reach: windows-lane-test: none.
Windows vet: not required (own-job reach empty).
deferred_to_pr_ci: windows-lane-test (non-linux-runner).
not_covered: none.
ci_lane_run: n/a (no CI job, matrix, timeout or required-check-list changes).

Exact CI compile-job commands (and triggered pack job when applicable):
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
This PASS authorizes opening the PR. Exact-head CI / required and every deferred job must succeed before MPR merges.

## Publication hooks

The reviewed source predates main's worker-environment re-pin. Its first pre-commit docs attempt queued without a matching remote worker and was stopped with Bazel reporting zero tests executed. The project-documented `--config=fork-cache` path runs the normal Bazel docs and full pre-push hooks against this unchanged source. Primary criterion 3 above remains strictly remote on the tested merge. Local hook scheduling is capped at two CPUs, two test jobs and 8 GiB; the original local rc is retained for exact restoration after delivery. This is setup recovery, without a failed-test rerun, attribution, waiver or hook bypass.

## Retained evidence

Durable evidence: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207`; original run: `/var/tmp/deploy-ga-803wmu1r.lpiuy2co`. Verified Git bundles retain the tested merge and gate-only commit.

Full merge build BEP retained: 5839 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/c6-build.bep.json; SHA256 2d5ee0d3e16cea17361023690e4c2139434d616f237bbf4e74c821c6ed3d97c0.

Full merge nogo BEP retained: 4031 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/c6-vet.bep.json; SHA256 54b5866ac84b79af01e01fa90972cae847ff89c359e4c5f786f724b4f9ad31c4.

Full merge build driver retained: 1 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/c6-terminal.stdout; SHA256 b412894777c125e12117008a96979f56bad973c01b1b978c567daa2ee0b7cfd8.

Full execution retained: 5999 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/c3-run/evidence/execution.json; SHA256 4f8b1a90cc3804624977013e221eb984e88bf6d4e1eda3f90f13c95aa204e533.

Full execution projection retained: 5991 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/execution-record-summary.json; SHA256 28c618ef1537e4b74438a4d168a1a45ac5f515ff001078982457aaa4724ff86f.

Full remote skip inventory retained: 4794 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/c3-run/evidence/remote/skip-inventory.json; SHA256 e3de4aa8bca2527f2d3a8eb1eb80d59785df317aef27226558c741089464f261.

Full native skip inventory retained: 578 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/native-skip-inventory.json; SHA256 6455c55425d5323845520286f993a49c6e54e3ed85cb7723765b8fa3b5cb161a.

Full remote sites retained: 1898 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/remote-skip-site-results.json; SHA256 6d674c7454e3d451effde0318bc5013164631e84f66d6eb561728cff1425794e.

Full native sites retained: 272 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/native-skip-site-results.json; SHA256 3cb44fb048a62597718ad397e3011157087efb4aaaafbf22db37136032c5c7a7.

Full shared hunks and closures retained: 9871 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/skip-measure-plan.json; SHA256 c944c9308bf69d69ceac454e5a0aa6a5963dba2a110ba7baa76427244a3bcacb.

Full base comparison verdicts retained: 464 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/skip-comparisons.json; SHA256 910bfe1f7b2b858c2f0c0d73385f32014b4b59abef2b911822bc57d946627578.

Full base remote commands and results retained: 216 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/base-skips/remote-results.json; SHA256 d4857198ecb024bb81839769e2b64e74e7594fb4c1114e9d931d5e128c068a28.

Full CI accounting retained: 100 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/ci-required/accounting.json; SHA256 59f00b3c75c276e2993dd70d568cd0946d42086d338fe2bc48f500adeffc1e09.

Full Windows native-job reach retained: 36 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/ci-required/windows-lane-test-reach.json; SHA256 a30905758f5e6a0fcc77165d85b2650050d440155ffdc0d66df20d753b1f3030.

Full reviewer tests retained: 1 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/reviewer-tests-check.stdout; SHA256 a3fa525236ae041566be309b0811a544008d178fb706e79c1ec33525273ae311.

Full dashboard response retained: 29 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/dashboard-preview.html; SHA256 d335d94368797417f317dd5e760a5ce0c4d0d607514002e8425f4b52de70a2d4.

Full dashboard headers retained: 10 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/dashboard-preview.headers; SHA256 bb3b60c28809b011838fbb27dd84d429beb4480d55bdbe8983f31a12dc698c15.

Full dashboard server retained: 5 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/dashboard-preview-server.log; SHA256 78756d7ff131173271b49915ac76cdfb665aa813b06fcf5d1be00ef8b04f2f88.

Full dashboard setup correction retained: 10 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/dashboard-setup-correction.json; SHA256 8d565447947453ea21320f3a6929f586d5ce6220e6d71be17811f9a80e8519dc.

Full dashboard initial setup output retained: 15 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/dashboard-preview-initial-terminal.stderr; SHA256 c2340c16458dcc9e1667f93e1afce06d2a4b2dad93f1457238e38aa2ab27f895.

Full publication worker-pin fallback retained: 19 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/publication-worker-pin-fallback.json; SHA256 55bd2c3ed59195a0c8860a2e1750126edc89dca494093cc7c69fabe1012ee464.

Full queued precommit output retained: 54 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-803wmu1r/gate-20261118T233402Z-nhz3e207/gate-commit-remote-stopped.stderr; SHA256 3ee88cd42af3aecffa5e25890a45a5790a8031b84efb187f0c02f0daba91995b.
