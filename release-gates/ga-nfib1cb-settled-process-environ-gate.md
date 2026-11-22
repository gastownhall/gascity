# Settled process-environment reads — ga-nfib1cb

**Verdict:** **PASS**

## Coordinates

- Reviewed source: 347fc25ac17151fd3f2837c4ed184dc4e435518e (tree 34e59b2b20e575364bbabf0e88007e803e65f40a).
- Independent review: ga-ealrqou, closed/PASS bound to that exact source; source build ga-baumy9, fixes tracker ga-7lhq6f.
- Frozen gate base: ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083.
- Tested merge: 5768ad9b06ab2ed15a407b96397a9fb3addbd4ac (tree 303a4d63907d5c80056742d8b55bf548ca99db55, independently equal to merge-tree prediction).
- merge_state: clean (origin/main@ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083).
- gate_base_kind: current-tip.
- gate_base: ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083.
- merge_validation: this exact merge result built and vetted; normal commit hooks passed. MPR and the merge queue own the final main landing.
- Isolated deploy branch: deploy/ga-nfib1cb-gate; cut at the reviewed source, followed only by this gate record. No rebase or source rewrite.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Independent review ga-ealrqou explicitly binds to the resolved source above. No carryover from the earlier pre-rebase review and no reviewer-added source commit. |
| 2 | Acceptance criteria met | PASS | Two-file scope verified. All3 owned tests, both modified-helper callers and the resource census PASS by name in this gate. Native Linux7.2.4 stress/parser count20 produced200 verbose PASS lines; torn-read/two callers count5 produced15,0 FAIL/0 SKIP, uncached first-attempt local BEP. Original RED message records5/10 failures; independent reviewer reproduced both EACCES and missing-key signatures and explained why older farm kernels cannot discriminate the kernel defect. |
| 3 | Tests pass | PASS | Five complete fresh farm phases,497 uncached attempt1 remote results,16 required native-floor steps rc0, no failed verdict retried. All owned/required names verified. Counts and SKIP evidence below. |
| 4 | No unresolved HIGH review findings | PASS | Exact-source independent review has no blocking style, security or specification finding; informational wording findings remain informational. |
| 5 | Final source and deploy branch clean | PASS | Tested source scratch clean; publication guard requires a clean isolated branch and verifies its only added commit changes this gate record. |
| 6 | Merge state, build and vet | PASS | Canonical merge built across1385 targets, integration binary compiled, full vet/nogo and active check-hooks PASS. Merge tree equals prediction. |
| 7 | Single feature theme | PASS | Only cmd/gc/dolt_scope_watchdog_test.go and cmd/gc/proc_environ_settled_test.go: one settled-environment test-helper theme. Citation/denylist ancestry guards passed for confirmed build ga-baumy9 plus this deploy bead. |

## Acceptance and test evidence

Scope: the helper retries transient EACCES until the existing deadline, rejects empty/torn/bash mid-import environment entries, and retains the stable-exe check. Production readers, kernel behavior and test census limits are untouched. The strict parser has all8 table cases; one child-construction site preserves the subprocess census. Native repeated checks execute the actual Go tests on Linux7.2.4, rather than a reimplementation or a farm-kernel inference. Historical RED is source/review evidence, not a new RED run by this deployer.

policy_lane: PASS — pinned format, full nogo/lint, four make shell guards, ten policy/format Bazel targets. Every lane uses a fresh view; the policy lane also uses gate-base.sh run and the recorded golangci-lint pin.

drift_lane: PASS — make bazel-sync; git diff --exit-code; empty git status --porcelain --untracked-files=all. Seven further generated-artifact targets PASS in a separate fresh view. CI drift step inspected in .github/workflows/bazel.yml.

fast_lane: PASS before C3 launch, 2026-10-25T14:07:27.360879+00:00.

```text
policy: FRESH_TREE_RECORD from=/var/tmp/deploy-ga-nfib1cb.ptxpwxi4/merge head=5768ad9b06ab tree=303a4d63907d dir=/var/tmp/gc-fresh-tree.F1aafU modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
policy: GATE_BASE_RECORD base=ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.F1aafU
policy: GLT_RECORD: {"golangci_lint":{"pin":"2.12.0","resolved":"2.12.0","mismatch":false}}
drift: FRESH_TREE_RECORD from=/var/tmp/deploy-ga-nfib1cb.ptxpwxi4/merge head=5768ad9b06ab tree=303a4d63907d dir=/var/tmp/gc-fresh-tree.LAvwcq modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
generated: FRESH_TREE_RECORD from=/var/tmp/deploy-ga-nfib1cb.ptxpwxi4/merge head=5768ad9b06ab tree=303a4d63907d dir=/var/tmp/gc-fresh-tree.7CL1Um modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
```

test_cmd_scope: full-suite

Canonical gate-criterion3.sh launched by gate-detached-run.sh under normal admission, load-gate-run.sh and isolated-test-run.sh. Actual phase commands (flags retained):

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-nfib1cb.ptxpwxi4/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-nfib1cb.ptxpwxi4/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-nfib1cb.ptxpwxi4/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-nfib1cb.ptxpwxi4/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-nfib1cb.ptxpwxi4/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

Unit287/287 targets, acceptance7/7, integration-packages36/36, full integration14/14 shards and the dedicated latency leg all PASS. First attempt only; test-cache hits, remote-local fallback, retries and mixed execution are absent from every remote phase BEP. Build/nogo cache use is distinct from fresh test evidence.

```text
test_exec_mode: remote-exec
test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 24, "results": 24, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 85, "results": 85, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 373, "results": 373, "retried": 0, "window_ok": true}}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45", "worker_env_tree": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45"}
fallback_reason: None
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 65.67060089111328, "waited_seconds": 0.001}
farm_test_seconds: 9485.843
skip_inventory: /var/tmp/deploy-ga-nfib1cb.ptxpwxi4/c3/evidence/remote/skip-inventory.json; /var/tmp/deploy-ga-nfib1cb.ptxpwxi4/floor-skip-inventory.json
owned_topup: []
floor_topup: complete, all16 steps rc0 (13 package jobs, Darwin compile, push-gate lock and local concurrency self-tests)
latency_leg: measured; first attempt; farm probe clear; both TestGcBdLatencyBudget and TestGcSeatHookLatencyBudget PASS
test_log_dir: /var/tmp/deploy-ga-nfib1cb.ptxpwxi4/c3/evidence/remote/logs
waiver_ref: none
heavy_mode: none
ci_lane_run: n/a (only two test Go files changed; no CI configuration diff)
```

GATE_TEST_EVIDENCE logs_dir=/var/tmp/deploy-ga-nfib1cb.ptxpwxi4/c3/evidence/remote/logs logs=513 logs_with_results=447
test_counts: PASS=126512 FAIL=0 SKIP=370
  (top-level result lines, column 0, summed over the 513 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
diff_tests_executed:
  TestParseSettledEnvironRejectsUnsettledShapes PASS (integration-packages-b72a67a2f9444af705e3.log, unit-2bde55fab377f50b4739.log)
  TestWaitForProcEnvironNeverReturnsATornRead PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-f679cd2551fcc64525aa.log)
  TestWaitForProcEnvironRetriesThroughExecWindows PASS (integration-packages-830685c60d0c2dd88567.log, unit-23e7d9fc763496dd8095.log)
GATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name


Required roots additionally include TestManagedDoltScopeWatchdogDoesNotInheritSessionIdentity, TestStartManagedDoltSQLServerDirectSpawnScrubsSessionIdentity and TestRepositoryLedgerMatchesCensusAndDocumentation, all PASS (final-required-by-name.txt). These6 roots are not6 newly added tests.

Native acceptance commands, run on the identical committed merge in a separate fresh view with local-heavy admission,40GiB floor and threshold15 soft load wait:

```text
#!/usr/bin/env bash
set -euo pipefail
h=$(cd "$(dirname "$0")" && pwd)
expected=$(jq -r .head "$h/context.json")
test "$(git rev-parse HEAD)" = "$expected"
uname -r
/home/jaword/projects/gc-management/packs/actual/all/scripts/gate-icu74.sh check
/home/jaword/projects/gc-management/packs/actual/all/scripts/gascity-bazel-legs.sh test --config=fork-cache --config=ci --nocache_test_results --noremote_local_fallback --test_sharding_strategy=disabled --test_env=GO_TEST_WRAP_TESTV=1 --test_output=all --test_arg=-test.count=20 --test_filter='^(TestWaitForProcEnvironRetriesThroughExecWindows|TestParseSettledEnvironRejectsUnsettledShapes)$' --build_event_json_file="$h/native-repeat20.bep.json" //cmd/gc:gc_test > "$h/native-repeat20.log" 2>&1
/home/jaword/projects/gc-management/packs/actual/all/scripts/gascity-bazel-legs.sh test --config=fork-cache --config=ci --nocache_test_results --noremote_local_fallback --test_sharding_strategy=disabled --test_env=GO_TEST_WRAP_TESTV=1 --test_output=all --test_arg=-test.count=5 --test_filter='^(TestWaitForProcEnvironNeverReturnsATornRead|TestManagedDoltScopeWatchdogDoesNotInheritSessionIdentity|TestStartManagedDoltSQLServerDirectSpawnScrubsSessionIdentity)$' --build_event_json_file="$h/native-repeat5.bep.json" //cmd/gc:gc_test > "$h/native-repeat5.log" 2>&1
printf 'NATIVE_ACCEPTANCE_PASS\n'

```

Native repeated test BEPs prove uncached linux-sandbox execution, attempt1, on kernel 7.2.4-200.fc44.x86_64. Counts:200 PASS lines for20 repetitions of2 roots plus their subtests;15 PASS lines for5 repetitions of3 roots. Neither run replaces the full C3 chain. Original stdout logs retain rules_go's0x16 prefix; the derived count parser strips that wrapper byte only. No test was repeated to repair an outcome.

## SKIP and artifact audit

Canonical513 logs (497 farm plus16 floor),126512 PASS /0 FAIL /370 SKIP top-level lines, counted per job rather than as unique test names. Full inventories contain395 records, including subtests and native-floor skips, at268 skip sites; every site is UNCHANGED, no unresolved source mapping, no owned SKIP/FAIL/MISSING. The71 farm environment-absence records were selected by the mandatory native floor; remaining non-owned top-up SKIPs are inventoried, not credited as executed passes. Ordinary fixture-only helper entry points, explicit feature opt-ins and platform/environment guards remain non-owned.

The only skipped entry in either changed file is TestManagedDoltScopeWatchdogHelper at dolt_scope_watchdog_test.go:177, reason "helper process only" (twice: unit and integration-packages). Its unchanged early environment-marker check skips before all changed helpers; its body calls none of waitForProcEnviron/readSettledProcEnviron/parseSettledEnviron/procEnvironChild. The shared changes are a standard-library import, comments, pure parsing/read helpers and the child constructor used only by the explicitly passing tests; no initialization, environment setter or skip condition changed. Therefore no shared hunk can cause this skip. No skip exists in proc_environ_settled_test.go. All other skip sites are outside both changed test files; no production package input changed.

Independent final audit rehashed all994 original farm log/XML artifacts and checked normalized log bytes against originals;1523 retained file digests recorded. All5 phase windows, every remote strategy, attempt number, cached flags, terminal PASSED verdicts and required test names were checked. Raw BEP, stdout, logs, inventory, native acceptance and first-attempt outcomes remain under /var/tmp/deploy-ga-nfib1cb.ptxpwxi4. No failure attribution or waiver was used.

## Environment and load

Runtime socket active, exact deps.env-pinned Dolt image present; pinned bdv1.3.2 identity verified by the wrapper. ICU74 loader/provenance check OK. The wrapper reported PASS-THROUGH for these gascity checkouts: caller selectors were scrubbed by the launcher and canonical runner, without claiming a separate container clone. No schema, host library, cache, process, tmux or runtime deployment change.

```text
GATE_ICU74 lib=libicui18n.so.74 file="/usr/local/lib64/icu74-compat/libicui18n.so.74.2" sha256=d90d62f0d7b6c6fbc279ae5e433cc7004aa47cdb106a71a02350099f8df9ae51 recorded=yes
GATE_ICU74 lib=libicuuc.so.74 file="/usr/local/lib64/icu74-compat/libicuuc.so.74.2" sha256=33499dd97a5393926b32e0b535f5340f7dae14b35e59025b572e6bd9ad48a2ae recorded=yes
GATE_ICU74 lib=libicudata.so.74 file="/usr/local/lib64/icu74-compat/libicudata.so.74.2" sha256=c891c5fdb45e3cf756b728abe256be7af1bfc97f00ca2f2fac450c96c09bd4b7 recorded=yes
GATE_ICU74 verdict=OK provenance=recorded sonames="libicui18n.so.74 libicuuc.so.74 libicudata.so.74"

build: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.80 run_max_load=13.96 run_mean_load=12.34 run_readings=7 wait_first_load=9.80 wait_max_load=9.80 wait_mean_load=9.80 wait_readings=1 read_errors=0
c3: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=15.43 run_max_load=16.24 run_mean_load=12.85 run_readings=37 wait_first_load=15.43 wait_max_load=15.43 wait_mean_load=15.43 wait_readings=1 read_errors=0
native-acceptance: LOAD_GATE_SUMMARY threshold=15 waited_seconds=120 wait_timed_out=0 run_start_load=14.91 run_max_load=15.73 run_mean_load=15.73 run_readings=1 wait_first_load=15.49 wait_max_load=16.21 wait_mean_load=15.50 wait_readings=6 read_errors=0 prewait_s=120
```

Remote class intentionally has no host-load wait. Native floor retained its canonical local wait; the additional native acceptance waited120s at threshold15 before admission/run. No load, timeout, census or admission override.

## Retained records

Evidence: /var/tmp/deploy-ga-nfib1cb.ptxpwxi4; final-audit.json; native-acceptance-proof.json; source-ownership-explain.txt; skip-sites-evidence.txt; all command records and raw results. docs/PROJECT_MANIFEST.md is absent in this repository; current TESTING.md, Makefile, CI definitions and the mandatory release criteria determine coverage. No public PR, clearance or merge is implied by this record alone.
