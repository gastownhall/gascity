# Drain-ack named-identity claim preservation release gate

**Verdict:** **PASS**

Deploy bead: ga-d0xrtglt; build bead: ga-5dxgol; review: ga-67dh0lbz
Reviewed source: b6dec705e9cd8be59f566ceec9be4eefbc64e061
Evaluated: 2026-11-25T04:52:38.416853+00:00
Evidence root: /var/tmp/gc-ga-d0xrtglt-gate.6e47e2 (retained; .gc-no-reap; per-test logs kept)
Deploy mode: remote; base branch: main; push remote: fork.
docs/PROJECT_MANIFEST.md is absent. This run follows the deployed criteria protocol, TESTING.md, Makefile and the merged CI workflows.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS — ga-67dh0lbz records verdict pass at the exact reviewed source; no carryover or reviewer-added-test exception. |
| 2 | Acceptance | PASS — configured-identity claims retain status, assignee and route; ordinary pool claims, session-name-only claims and dispatch-held claims release. Both seat readers read the same trimmed identity. Two owned tests and all four behavior subcases pass. |
| 3 | Tests and CI jobs | PASS — five-phase full suite plus 16/16 native floor steps, 159212 top-level PASS / 0 FAIL / 618 SKIP. Both owned tests pass by name. Six CI leaf jobs accounted below. Native Windows tests remain for PR CI. |
| 4 | No open HIGH findings | PASS — review has no unresolved HIGH finding; its minor sibling-comment wording note is non-blocking. |
| 5 | Clean tree | PASS — git status --porcelain is empty on the frozen gated merge tree. The gate record is the sole deployment addition. |
| 6 | Merge state and build/vet | PASS — clean merge onto the pinned base; bazel build --config=remote-exec --noremote_local_fallback //... passes including nogo lint/vet (22683 actions); c6.stdout.txt/c6.stderr.txt. |
| 7 | Single theme | PASS — three reviewed commits cite ga-5dxgol and touch two files implementing/testing drain-ack claim preservation. No unrelated theme or internal documentation path. |

merge_state: clean (origin/main@af2429b01c66)
merge_validation: merge result built and vetted here
gate_base_kind: current-tip
gate_base: af2429b01c66823df7950aac09a3a480c56a45b2
gate_head: b6dec705e9cd8be59f566ceec9be4eefbc64e061
merge_tree: 64c1709ca335ac445c384da9deaf126ab6d3da71

Pre-flight found no PR for the reviewed commit; deploy-ref interlock exit 0; gate round 1. Source SHA resolved in the object store. Reviewed source was not rebased or reset; no force-push or merge to the deploy base.

## Fast lane

policy_lane: PASS — git diff --check; make lint (explicit remote config/no fallback); //:go_format_test executed PASS; make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary. gate-base.sh run pins the base.
drift_lane: PASS — make bazel-sync with explicit remote flags, then git diff --exit-code (empty).
known_red_precheck: proceed nothing-known-red; check-routed-test-rows passes, tracker ga-1ptv4jey is stale.
Fast lane completed before the suite. Logs: policy.{stdout,stderr}.txt, drift.{stdout,stderr}.txt, known-red.txt.
Initial unconfigured compilation launches were stopped before any test; corrected build-vet-rbe and policy-rbe services passed. No failed test was retried.

```text
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.yA79Mm head=64c1709ca335 tree=abe46c84d06e dir=/var/tmp/gc-fresh-tree.XvI7GU modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.yA79Mm,bazel-out,bazel-testlogs
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.yA79Mm head=64c1709ca335 tree=abe46c84d06e dir=/var/tmp/gc-fresh-tree.Wh3Sdj modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.yA79Mm,bazel-out,bazel-testlogs
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.yA79Mm head=64c1709ca335 tree=abe46c84d06e dir=/var/tmp/gc-fresh-tree.pmsUxB modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-gc-merge-validate.yA79Mm,bazel-out
```

## Full-suite evidence

test_exec_mode: remote-exec
test_cmd_scope: full-suite
test_counts: PASS=159212 FAIL=0 SKIP=618 (top-level result lines, counted per job)
test_log_dir: /var/tmp/gc-ga-d0xrtglt-gate.6e47e2/criterion3/evidence/remote/logs
waiver_ref: none
heavy_mode: none
failure_attribution: none (no failures)
ci_lane_run: n/a (no CI-config change)
fallback_reason: none
owned_topup: none required; both tests PASS in unit and integration-packages
floor_topup: complete, 16/16 steps exit 0
farm_test_seconds: 14008.964
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72", "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 53.631675720214844, "waited_seconds": 0.001}
delta_gate: shadow; full suite; not-gate-pr; no narrowing applied
execution_record: /var/tmp/gc-ga-d0xrtglt-gate.6e47e2/criterion3/evidence/execution.json (full JSON retained, compact facts here)

test_cmd, detached standard runner:
```bash
env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-d0xrtglt GC_GATE_DELTA=shadow GC_GATE_LOAD_DEFER_MAX=3 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-validate.yA79Mm --base af2429b01c66823df7950aac09a3a480c56a45b2 --bead-json /var/tmp/gc-ga-d0xrtglt-gate.6e47e2/bead.json --owned /var/tmp/gc-ga-d0xrtglt-gate.6e47e2/owned.tsv --run-dir /var/tmp/gc-ga-d0xrtglt-gate.6e47e2/criterion3/evidence
```

Per-phase actual flags:
```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-ga-d0xrtglt-gate.6e47e2/criterion3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-ga-d0xrtglt-gate.6e47e2/criterion3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-ga-d0xrtglt-gate.6e47e2/criterion3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-ga-d0xrtglt-gate.6e47e2/criterion3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-ga-d0xrtglt-gate.6e47e2/criterion3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

Phase commands: unit make -k check; acceptance //test/acceptance:acceptance_test plus //test/acceptance:acceptance_solo_tests; integration-packages //test:integration_packages; integration-full //test/integration:integration_test with the computed latency exclusion; integration-latency that target with the shipped latency list, sharding disabled and the farm-load probe. The complementary integration legs cover the full inventory.

| Phase | Remote results | Cache hits | Retries | Window valid |
|---|---:|---:|---:|---|
| unit | 426 | 0 | 0 | true |
| acceptance | 25 | 0 | 0 | true |
| integration-packages | 87 | 0 | 0 | true |
| integration-full | 15 | 0 | 0 | true |
| integration-latency | 1 | 0 | 0 | true |

diff_tests_executed:
  TestDrainAckKeepsConfiguredNamedIdentityClaims PASS (integration-packages-074fc6c34a1fb2577127.log, unit-423c6a0da1986b90e9a7.log)
  TestReleaseSeatReadsNamedIdentity PASS (integration-packages-75af79934639abaae69c.log, unit-1ab526405ace21879f85.log)
GATE_TEST_EVIDENCE_RESULT ok: 2 diff-owned tests verified by name

latency_leg: complete and measured on attempt 1, probe clear after 40 s, load1 77.89 -> 42.68 at threshold 50; both tests PASS; neither ran in the main integration leg.
skip_inventory: {"environment-absence": 128, "other": 548}; 676 records including subtests at 319 sites. Top-level SKIP count is 618.
skip_justification: every site is UNCHANGED in the actual merged frame; none is diff-owned. No SHARED-HUNKS or HELPER-HUNKS exist, so no base-by-name comparison is required. Reasons cover platform restrictions, separate process lanes, absent tools/services or opt-ins, inapplicable storage-topology fixtures, unsupported provider capabilities, deferred contracts and helper-only entrypoints. Those guards do not depend on drain-ack identity matching. Environment-absence top-ups ran locally; remaining non-owned skips are inventoried. Every reason and site proof is in skip-rows.json/skip-sites.txt; skip-summary.json has no unresolved/touched site and base_required=0.

## CI required-job accounting

ci_required_closure: python3 ci_required_triggers.py /var/tmp/gc-merge-validate.yA79Mm af2429b01c66823df7950aac09a3a480c56a45b2 b6dec705e9cd8be59f566ceec9be4eefbc64e061 fork
workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612
6 LEAF jobs need a status; the rest are rolled up through their needs.
| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | Exact workflow Python command, PR_AUTHOR=quad341, EVENT_NAME=pull_request; exit 0; ci-runner-policy.stdout.txt |
| changes | LOCAL-PASS | ci_required_triggers.py output ci-closure.txt; exit 0 |
| credential-provider-windows | NOT-TRIGGERED | credential_provider filter false; exact tool row below |
| pack-gate | NOT-TRIGGERED | packs filter false; exact tool row below |
| windows-lane | LOCAL-PASS | All four exact workflow run steps, Go 1.27.4 linux/amd64; exit 0; ci-windows-lane.sh and ci-windows.stdout.txt |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | blacksmith-4vcpu-windows-2025; job-specific Windows test dependency closure exit 0, reach=[]; ci-windows-native-reach.json |

Exact trigger rows:
```text
diff af2429b01c66823df7950aac09a3a480c56a45b2...b6dec705e9cd8be59f566ceec9be4eefbc64e061: 2 changed files
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

windows_reach: windows-lane-test=none. Its own Windows test allowlist/test-binary closure exits 0 with no changed input in reach; proof ci-windows-native-reach.json, raw graph ci-windows-native-deps.jsons. credential-provider-windows is NOT-TRIGGERED.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
Merge only when CI / required is green on the PR head and windows-lane-test succeeds. A later red deferred job needs a reviewed fix and a re-gate.

windows-lane exact workflow run steps:
```bash
#!/bin/bash
set -euo pipefail
printf '%s\n' 'Build the Windows allowlist'
GOOS=windows go build $(cat .github/windows-lane/build.txt)
printf '%s\n' 'Compile the Windows allowlist'"'"'s tests'
GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt)
printf '%s\n' 'Build the nogo analyzers and their tests for Windows'
( cd tools/nogo
GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/...
)
printf '%s\n' 'Build every package and its tests for macOS'
GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./...
```

## Load evidence

Remote suite admission has no host load wait. The native floor and CI jobs use load-gate-run.sh:
```text
criterion3/evidence/remote/floor-topup: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.79 run_max_load=16.24 run_mean_load=15.38 run_readings=7 wait_first_load=14.79 wait_max_load=14.79 wait_mean_load=14.79 wait_readings=1 read_errors=0
ci-runner-policy: LOAD_GATE_SUMMARY threshold=15 waited_seconds=90 wait_timed_out=0 run_start_load=14.97 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=16.27 wait_max_load=16.27 wait_mean_load=15.75 wait_readings=4 read_errors=0
ci-windows: LOAD_GATE_SUMMARY threshold=15 waited_seconds=30 wait_timed_out=0 run_start_load=14.95 run_max_load=20.07 run_mean_load=17.81 run_readings=4 wait_first_load=15.46 wait_max_load=15.46 wait_mean_load=15.21 wait_readings=2 read_errors=0
```

load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
run_start_load: 14.79
run_max_load: 16.24
run_mean_load: 15.38
run_readings: 7

No waiver or failure attribution was used. Raw logs remain retained.
