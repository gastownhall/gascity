**Verdict:** **PASS**

# MCP mail dnspython security update — release gate

Bead: `ga-0wc93gpc`; review: `ga-0b5usny0`; build: `ga-9d74sbdn`; scanner condition: `gm-qq561t`.

Reviewed source: `c9b8a7004d5b99540efcf62b4da9d7905a9ffd5a`. Tested merge: `4b57f23dbd587bfca533f35a23388ac1170b558f`; tree: `3e38ca9866a43fb356d32b7a173713f9141de851`.
gate_base: `03f6fe825cf0a54c677c241e4aaa7e76dade28a5`
merge_state: clean (`origin/main@03f6fe825cf0a54c677c241e4aaa7e76dade28a5`)
gate_base_kind: current-tip
merge_validation: exact merge result built and vetted; the merge authority validates the eventual merge.
deploy_mode: remote; push_remote: fork; isolated branch: `deploy/ga-0wc93gpc-gate`.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Closed review PASS at the exact resolved source. No carryover or branch-tip substitution. |
| 2 | PASS | Floor and hashed lock are 2.9.2; pinning test PASS by name; first real PR image rebuild and Trivy Image vulnerabilities job succeed, with the required mail-image policy PASS line below. |
| 3 | PASS | Canonical full suite complete and valid: 156791 PASS / 0 FAIL / 613 SKIP. Required-job accounting and unchanged-skip proofs complete. |
| 4 | PASS | No unresolved HIGH review findings. No waiver or scanner-policy exception was added by this diff. |
| 5 | PASS | Tested tree and source checkout clean. Publication contains the reviewed source and only this gate record; post-commit cleanliness is checked before push. |
| 6 | PASS | Whole-tree Bazel build and nogo output-group build both succeed, with BEP overallSuccess=true. |
| 7 | PASS | One theme: patch dnspython in the MCP mail image and guard the new pins. Three files; no other dependency or complexity-baseline change. |

## Acceptance evidence

Static source inspection: .github/requirements/mcp-agent-mail.in requires dnspython>=2.9.2 and names CVE-2026-128089. The lock selects dnspython==2.9.2 with the two reviewed SHA256 hashes. The lock diff changes only that version and its hashes; its generator header is unchanged. The builder records regeneration with uv 0.12.10; this gate does not claim a second regeneration. The independently rerun pinning test verifies the floor and lock line.

Draft PR: https://github.com/gastownhall/gascity/pull/9269. First real Container Scan run: https://github.com/gastownhall/gascity/actions/runs/44375687387. Image vulnerabilities job: https://github.com/gastownhall/gascity/actions/runs/44375687387/job/135054411151 (success); Enforce image vulnerability policy step success. Run head equals the reviewed source; the image tag below identifies GitHub’s PR merge tree.

```text
PASS: gc-mcp-mail:605525707dd50f59434d350318a6c2caf572e0f9 has no unwaived HIGH/CRITICAL findings
```

No CVE-2026-128089 finding appears in that policy-step output. Full image-build and scan logs are retained. This is image-level proof, rather than a pinning-test proxy. The later gate-only commit changes no image, dependency, workflow, or test input. CI on the published head must still satisfy the merge procedure. The unmerged PR does not retire the repo-wide scanner-advisory exception.

## Full-suite evidence

test_cmd_scope: full-suite; test_exec_mode: remote-exec; heavy_mode: none; failure_attribution: none; waiver_ref: none.
test_log_dir: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/c3-run/evidence/remote/logs` (all per-job logs retained).

test_cmd: env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-0wc93gpc DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-ga-0wc93gpc.KBjbzb --base 03f6fe825cf0a54c677c241e4aaa7e76dade28a5 --bead-json /var/tmp/deploy-ga-0wc93gpc.ywyxeox0/bead-c3.json --owned /var/tmp/deploy-ga-0wc93gpc.ywyxeox0/owned.tsv --run-dir /var/tmp/deploy-ga-0wc93gpc.ywyxeox0/c3-run/evidence

Canonical full-scope chain: five forced-uncached remote phases and the computed native floor; exact phase flags follow. The invocation is retained verbatim above; scratch paths name the tested checkout.

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-0wc93gpc.ywyxeox0/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-0wc93gpc.ywyxeox0/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-0wc93gpc.ywyxeox0/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-0wc93gpc.ywyxeox0/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-0wc93gpc.ywyxeox0/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

diff_tests_executed:
  TestMCPMailImagePinsPatchedPythonDependencies PASS (unit-9f907e0fea7ca680f21c.log)
GATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name

Native floor: 16 steps, all rc=0. ICU74 loader check OK, recorded provenance; rootless podman ping OK. The changed pinning test reads checked-in dependency files and requires no container runtime. Pinned test bd is reported in the raw wrapper logs.

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-0wc93gpc.ywyxeox0/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":67.20627975463867,"waited_seconds":0.0,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":415,"remote_executed":415,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":14405.108,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":115.897,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=29.89 load5=29.68 load15=28.90 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 32.00 29.91 29.01 25/1554 56416"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38616}},"log_dir":"/var/tmp/deploy-ga-0wc93gpc.ywyxeox0/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":509,"environment-absence":90},"top_level_counts_by_reason_class":{"other":251,"environment-absence":43},"tests":{"elided_array":{"items":599,"bytes":214981}}},"test_cmd":{"elided_array":{"items":5,"bytes":2059}},"test_counts":{"PASS":156791,"FAIL":0,"SKIP":613},"diff_tests_executed":"diff_tests_executed:\n  TestMCPMailImagePinsPatchedPythonDependencies PASS (unit-9f907e0fea7ca680f21c.log)\nGATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name\n"}
```

## Skip integrity

skip_justification: no diff-owned skips; every remote/native skip maps an unchanged source site and reason, as proved below and in retained audits.
Top-level counts exclude subtests. Remote inventory: 599 events at 316 unchanged source sites. Native inventory: 72 events at 45 unchanged sites. Owned-name sets are recomputed at the actual merged head and match the reviewed source. There are no changed shared/helper Go hunks, and no skipped tests in scripts, the package containing the changed pinning test and the manifest readers. Existing OS, privilege, opt-in and fixture skip predicates in other packages are unchanged. Every inventory entry is mapped to its source site and reason in the retained audits.

Any inventory reason recovery is recorded with the original log hash and source location in the audit. Original inventories and logs remain unchanged. No test was rerun to account for skips.

## Fast lane and host load

policy_lane: shell guards plus14 Bazel policy/format targets, rc0, through gate-base.sh run in a fresh view.
drift_lane: seven generated-artifact targets, OpenAPI compatibility, and make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec plus git diff --exit-code, each in its own fresh view; every rc0. Fast lane completed before suite environment setup or launch.

known_red_precheck: known-red-precheck: target=check-routed-test-rows verdict=green merge=0 base=- (it passes on the merge tree)
known-red-precheck: stale: tracker=ga-1ptv4jey `make check-routed-test-rows` passes on this merge tree; the tracker may be ready to close
known-red-precheck: proceed reason=nothing-known-red (check-routed-test-rows=green)

Native-floor load:
```text
load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=7.23 run_max_load=8.10 run_mean_load=7.71 run_readings=6
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=7.23 run_max_load=8.10 run_mean_load=7.71 run_readings=6 wait_first_load=7.23 wait_max_load=7.23 wait_mean_load=7.23 wait_readings=1 read_errors=0
```

Own CI compile-job load:
```text
load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=7.60 run_max_load=10.12 run_mean_load=9.12 run_readings=3
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=7.60 run_max_load=10.12 run_mean_load=9.12 run_readings=3 wait_first_load=7.60 wait_max_load=7.60 wait_mean_load=7.60 wait_readings=1 read_errors=0
```

## Required-job accounting

ci_required_closure: ["python3", "/home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py", "/var/tmp/gc-merge-ga-0wc93gpc.KBjbzb", "03f6fe825cf0a54c677c241e4aaa7e76dade28a5", "c9b8a7004d5b99540efcf62b4da9d7905a9ffd5a", "fork"]; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs.

```text
diff 03f6fe825cf0a54c677c241e4aaa7e76dade28a5...c9b8a7004d5b99540efcf62b4da9d7905a9ffd5a: 3 changed files
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
| runner-policy | LOCAL-PASS | {"command": "EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<file> python3 .github/workflows/scripts/runner_policy.py", "log": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/ci-required/runner-policy.log", "rc": 0} |
| changes | LOCAL-PASS | {"evidence": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/ci-required/closure.txt"} |
| credential-provider-windows | NOT-TRIGGERED | {"evidence": "credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'"} |
| pack-gate | NOT-TRIGGERED | {"evidence": "pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'"} |
| windows-lane | LOCAL-PASS | {"command_file": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/windows-lane.sh", "rc": 0, "run_dir": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/ci-required/windows-lane", "stderr": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/ci-required/windows-lane-terminal.stderr", "stdout": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/ci-required/windows-lane-terminal.stdout"} |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | {"evidence": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/ci-required/windows-lane-test-reach.json", "reach": "none", "resolved_runs_on": "blacksmith-4vcpu-windows-2025"} |

windows_reach: windows-lane-test none, from its own GOOS=windows test-binary closure over the job allowlist.
deferred_to_pr_ci: windows-lane-test (non-linux-runner).
not_covered: none.
ci_lane_run: n/a (no CI job, matrix, timeout, or required-check list changes). The required acceptance scan uses the guarded pre-PASS draft route and is recorded separately above.

Exact windows-lane commands:
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

This PASS authorizes PR publication. Exact-head CI / required and each deferred job must succeed before MPR merges.

## Retained evidence

Durable evidence: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157`; original run root: `/var/tmp/deploy-ga-0wc93gpc.ywyxeox0`. Exact merged tree retained in a verified Git bundle.

Full execution.json retained: 5999 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/c3-run/evidence/execution.json; SHA256 3fc7f318eafa8e01186aff02c99a1b3996acfe3cfdde7ed827a06f426e46945e.

Full remote skip inventory retained: 4794 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/c3-run/evidence/remote/skip-inventory.json; SHA256 3fa8bbb47ca7f7147db00f4354d69a397cf0cacc9ed544dc4cb254c41b006156.

Full native skip inventory retained: 578 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/native-skip-inventory.json; SHA256 5f6dbdabac8b505e9f3af02addc417474b7c372b5f0f83c4ed1b930c1f2aeb80.

Full remote skip-site proofs retained: 1898 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/remote-skip-site-results.json; SHA256 6d674c7454e3d451effde0318bc5013164631e84f66d6eb561728cff1425794e.

Full native skip-site proofs retained: 272 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/native-skip-site-results.json; SHA256 3cb44fb048a62597718ad397e3011157087efb4aaaafbf22db37136032c5c7a7.

Full floor steps retained: 1056 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/c3-run/evidence/remote/floor-topup.json; SHA256 950d99245c2508ff7c4a8d24544de3b6186d91c8310d645daa156ea16f095697.

Full CI accounting retained: 58 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/ci-required/accounting.json; SHA256 65f8eb150f4fb4df599e85d166df677f72fcbde55b0701bc1bbe643639f4f84a.

Full Windows reach proof retained: 36 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/ci-required/windows-lane-test-reach.json; SHA256 39f862c05b28f75fddb912aa293cb329d43d578d8d97079bbc9ced2f5e8a2036.

Full scan acceptance retained: 15 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/scan-acceptance.json; SHA256 f1577455c445317a76f4e7f8f5492b553e366e82781bbf1a765535e1b926ae37.

Full scan merge identity retained: 22 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/scan-merge-identity.json; SHA256 1cd6966d96cf8d97ccf09233abb752f9991c1bdc964834fc336fd98ad02ffeab.

Full image scan job log retained: 5809 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-0wc93gpc/gate-20261118T185325Z-7eb9a157/image-vulnerabilities-job.log; SHA256 3ca6bacb0d3667d858cb3b423671aa78e8d6de62481ed7f71df8c96b2ddb3f63.
