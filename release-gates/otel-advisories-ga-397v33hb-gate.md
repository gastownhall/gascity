# Release gate: OpenTelemetry advisory fixes

**Verdict:** **PASS**

Evaluated: 2026-11-24T02:38:26.339141+00:00
Deploy: ga-397v33hb; build: ga-b41drsoj; review: ga-9gv9sa4f.
Reviewed commit: 89e2e91dac9d8a20493e406a666fffbc8124ed8d
Deploy mode: remote; push remote: fork; base ref: origin/main.
Rules: supplied release-gate criteria, TESTING.md and the frozen CI workflow. docs/PROJECT_MANIFEST.md is absent.

gate_base: 9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c
gate_head: 89e2e91dac9d8a20493e406a666fffbc8124ed8d
gate_base_kind: current-tip
merge_state: clean (origin/main@9285cfb3ed95)
merge_validation: raw merge result built and vetted here; MPR validates the current merge result before landing.
Merge tree: ce55d5d6c30d0d1c066373ba4635a9a241ea9ec5
Evidence root: /var/tmp/gc-deploy-ga-397v33hb.cn1zrfH8 (retained independently of temporary checkouts).

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | ga-9gv9sa4f is closed with verdict pass on the exact reviewed source. No carryover or reviewer-added commit. |
| 2 | Acceptance criteria | PASS | Tagged OpenTelemetry 1.47.0 floors and logger-provider migration checked in source and both owned tests. Module count 523 to 523. BUILD sync clean. Fresh govulncheck 1.7.0, built with Go 1.27.4, exits 0 with zero affected vulnerabilities. The unreachable x/crypto advisory remains deliberately unpinned pending a tagged fix. |
| 3 | Tests and required lanes | PASS | Five full remote phases, 16 native floor commands, fast checks and complete six-leaf CI accounting below. Native Windows test execution is deferred to PR CI. |
| 4 | No unresolved HIGH findings | PASS | Review records no blocker, major or minor findings. |
| 5 | Clean deploy branch | PASS | Reviewed content plus this gate record only; committed separately and checked clean before push. |
| 6 | Merge state and build/vet | PASS | Clean raw merge on the pinned base. Remote Bazel build of cmd/gc and cmd/gc-osc7501, plus whole-tree nogo/vet, exit 0. build-fast.result.txt. |
| 7 | Single feature theme | PASS | Dependency advisory fixes and the required logger-provider API migration, with corresponding build/test changes; 11 reviewed files, no unrelated feature. |

fast_lane: PASS before suite setup.
policy_lane: fresh-tree-run.sh -> gate-base.sh run -> isolated-test-run.sh --rbe -> make lint NOGO_BUILD_FLAGS='--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback'; rc=0.
format_lane: own fresh view -> isolated-test-run.sh --rbe -> bazel test --config=remote-exec --noremote_local_fallback //:go_format_test; rc=0.
drift_lane: own fresh view -> make bazel-sync BAZEL_SYNC_FLAGS='--config=remote-exec --noremote_local_fallback', git diff --exit-code and empty git status --porcelain --untracked-files=all; rc=0.
Additional fast checks: all four make check shell guards and strict trusted-base storeclass baseline/growth predicates; rc=0. Proof: build-fast.result.txt, storeclass.result.txt and storeclass-proof/verified.json.
known_red_precheck: proceed reason=nothing-known-red; check-routed-test-rows green, tracker ga-1ptv4jey stale.

## Test evidence

test_cmd_scope: full-suite
test_exec_mode: remote-exec
criterion3_valid: true
test_counts: PASS=159049 FAIL=0 SKIP=618 (top-level result lines across jobs, not distinct test functions).
test_log_dir: /var/tmp/gc-deploy-ga-397v33hb.cn1zrfH8/c3/evidence/remote/logs
heavy_mode: none
failure_attribution: none
waiver_ref: none
ci_lane_run: n/a (no CI-config change).

All phases disable test-result caching and local fallback. Build/nogo may use cache. Independent audit re-read every BEP verdict and verified 1108 retained log/XML SHA256 hashes against CAS digests; 554 fresh first-attempt remote shard results.

| Phase | Shard results | Verdict | Cached | Retried | Phase window |
|---|---|---|---|---|---|
| unit | 426 | PASS | 0 | 0 | valid |
| acceptance | 25 | PASS | 0 | 0 | valid |
| integration-packages | 87 | PASS | 0 | 0 | valid |
| integration-full | 15 | PASS | 0 | 0 | valid |
| integration-latency | 1 | PASS | 0 | 0 | valid |

Recorded commands (all rc=0):

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-397v33hb.cn1zrfH8/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-397v33hb.cn1zrfH8/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-397v33hb.cn1zrfH8/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-397v33hb.cn1zrfH8/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-397v33hb.cn1zrfH8/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

```text
diff_tests_executed:
  TestInit_WiresResourceWithInstanceID PASS (unit-cb6fb9eb9ced162fda12.log)
  TestGoModOTelModulesCarryAdvisoryFixes PASS (unit-271bc77ad6715453dfe1.log)
GATE_TEST_EVIDENCE_RESULT ok: 2 diff-owned tests verified by name

```

floor_topup: complete, 16/16 commands rc=0; retained floor-topup.json and floor-logs/. Exact native commands are authorized by standing mayor ruling gcg-15584257 while gm-uqk5td remains open; the suite itself is Bazel.
latency_leg: measured, complete, clear farm probe; both tests in gate-latency-tests.txt PASS in their separate lane.
owned_topup: none needed.
skip_inventory: 676 events including subtests, 319 unique source sites, zero unresolved; all skip sites are unchanged by the diff. Full named reasons and ownership are in current-skip-audit.json, current-skip-sites.stdout and owned-explain.txt.
skip_justification: existing lane/platform/capability cases, plus environment-absence cases selected and rerun by the native floor. Shared test-helper hunks are confined to their own test binaries; only internal/beads has skipped tests in an affected package. Pinned-BASE comparisons prove identical reasons for all 18 unit, 17 integration and 4 native PostgreSQL skips (base-skip-comparison.stdout, base-integration-skip-comparison.stdout, native-base-skip-comparison.stdout). The native comparison uses the exact floor-selected command. No diff-owned SKIP or FAIL.

## CI-required accounting

ci_required_closure: python3 ci_required_triggers.py /var/tmp/gc-merge-ga-397v33hb.70Mv9NWb 9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c 89e2e91dac9d8a20493e406a666fffbc8124ed8d fork; rc=0; required-ci/closure.stdout.
workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612
6 LEAF jobs need a status; the rest are rolled up through their needs. The shared go.mod/go.sum change triggers the credential and pack jobs.

| LEAF job | Status | Evidence |
|---|---|---|
| changes | LOCAL-PASS | Closure tool rc=0; shared filter MATCH, suite_mode full. |
| runner-policy | LOCAL-PASS | Frozen job's script with EVENT_NAME=pull_request, PR_AUTHOR=quad341, FORCE_BLACKSMITH empty and retained GITHUB_OUTPUT; rc=0; required-ci/runner-policy-steps.json and .stdout. |
| credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | Triggered. Own closure computed below; GOOS=windows GOARCH=amd64 go vet ./internal/credentialprovider and go vet -tags=integration ./internal/credentialprovider both rc=0; windows-supplement.result.txt. |
| windows-lane | LOCAL-PASS | Frozen job's Windows allowlist build/test compilation, nogo analyzer build/test compilation, and whole-tree Darwin build/test compilation; every run step rc=0; required-ci/windows-lane-steps.json and .stdout. |
| pack-gate | LOCAL-PASS | Registry precondition probe passes; frozen bundled-pin check and live registry smoke each rc=0; required-ci/pack-probe.json, pack-gate-steps.json and .stdout. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Native Windows job's own test allowlist import closure computed with GOOS=windows GOARCH=amd64 go list -mod=readonly -test -deps. Its packages are covered by the successful Windows test-build allowlist; runtime execution remains a PR-CI requirement. |

windows_reach: credential-provider-windows = go.mod, go.sum; windows-lane-test = go.mod, go.sum.
Closure details: credential-windows-reach.txt and windows-native-reach.json; query rc=0 for both.
deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none

The PASS authorizes opening the PR. Merge requires CI / required green on the exact PR head and both deferred Windows jobs successful there. MPR owns merge authority.

## Advisory scan, load and retained evidence

govulncheck: 1.7.0 built with go1.27.4; GOTOOLCHAIN=go1.27.4 govulncheck -show verbose ./...; rc=0, zero affected vulnerabilities. Binary build metadata/hash: govulncheck-build.txt and govulncheck.sha256; full current report: vuln-scan.result.txt.
module_count: 523 at base and head; module-counts.txt. No pseudo-version added.

Commands ran sequentially in fresh views through the required wrappers. Remote admission does not wait for host load; native floor and supplemental jobs do. NA means no periodic sample, not zero load.

```text
c3.result.txt: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=11.57 run_max_load=16.40 run_mean_load=12.55 run_readings=53 wait_first_load=11.57 wait_max_load=11.57 wait_mean_load=11.57 wait_readings=1 read_errors=0
native-base.result.txt: LOAD_GATE_SUMMARY threshold=15 waited_seconds=60 wait_timed_out=0 run_start_load=14.72 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=16.16 wait_max_load=16.16 wait_mean_load=15.39 wait_readings=3 read_errors=0
windows-supplement.result.txt: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.31 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=14.31 wait_max_load=14.31 wait_mean_load=14.31 wait_readings=1 read_errors=0
vuln-scan.result.txt: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.82 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=13.82 wait_max_load=13.82 wait_mean_load=13.82 wait_readings=1 read_errors=0
windows-lane.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.98 run_max_load=19.87 run_mean_load=18.38 run_readings=4 wait_first_load=13.98 wait_max_load=13.98 wait_mean_load=13.98 wait_readings=1 read_errors=0
pack-gate.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=240 wait_timed_out=0 run_start_load=14.97 run_max_load=16.69 run_mean_load=16.01 run_readings=4 wait_first_load=19.18 wait_max_load=19.18 wait_mean_load=16.80 wait_readings=9 read_errors=0
```

load_threshold/load_waited_seconds/load_wait_timed_out/run_start_load/run_max_load/run_mean_load/run_readings: the per-command LOAD_GATE_SUMMARY fields above.

Full execution record: /var/tmp/gc-deploy-ga-397v33hb.cn1zrfH8/c3/evidence/execution.json. Independent farm audit: remote-independent-audit.json. Per-job command/result records and hashes remain in the evidence root; temporary checkouts may be removed without deleting these logs.

Compact execution metadata:

```json
{
  "test_exec_mode": "remote-exec",
  "fallback_reason": null,
  "admission": {
    "class": "remote",
    "floor_gib": 25.0,
    "free_gib": 71.14165496826172,
    "waited_seconds": 0.001,
    "cap": 2
  },
  "rbe_state": {
    "rc_marker_present": true,
    "kill_switch_absent": true,
    "cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT",
    "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72",
    "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"
  },
  "test_exec_bep": {
    "unit": {
      "results": 426,
      "remote_executed": 426,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "acceptance": {
      "results": 25,
      "remote_executed": 25,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "integration-packages": {
      "results": 87,
      "remote_executed": 87,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "integration-full": {
      "results": 15,
      "remote_executed": 15,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "integration-latency": {
      "results": 1,
      "remote_executed": 1,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    }
  },
  "farm_test_seconds": 13930.949,
  "owned_topup": [],
  "floor_topup": {
    "complete": true,
    "steps": 16,
    "exit_codes": [
      0,
      0,
      0,
      0,
      0,
      0,
      0,
      0,
      0,
      0,
      0,
      0,
      0,
      0,
      0,
      0
    ],
    "log_dir": "/var/tmp/gc-deploy-ga-397v33hb.cn1zrfH8/c3/evidence/remote/floor-logs"
  },
  "skip_inventory": {
    "counts_by_reason_class": {
      "other": 514,
      "environment-absence": 90
    },
    "top_level_counts_by_reason_class": {
      "other": 256,
      "environment-absence": 43
    }
  },
  "latency_leg": {
    "complete": true,
    "verdict": "measured",
    "rc": 0,
    "tests": [
      "TestGcBdLatencyBudget",
      "TestGcSeatHookLatencyBudget"
    ],
    "message": "measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS"
  }
}
```
