# Executor deadline settlement release gate

**Verdict:** **PASS**

Evaluated 2026-10-24T14:47:06.809004+00:00. Deploy ga-d8zwn7m; build ga-nua32sk; review ga-3bw30c5.
Reviewed source: 8faeae72de72d8ce7665e49d94abb3b8640ea13b.
Canonical merge: 21823ac5535844de7473b4d0142d0b664679ca67; tree: 9cc023ea73f421c0d3cd90dc7b75f4c186589493.
merge_state: clean (origin/main@c05f1d782c3015b556f5abb38a2587aa1e4a4675)
gate_base_kind: current-tip
gate_base: c05f1d782c3015b556f5abb38a2587aa1e4a4675
merge_validation: merge result built/vetted here; subsequent merge-group validation belongs to MPR/GitHub's merge queue.
Evidence retained: /var/tmp/ga-d8zwn7m-deploy.5xxn21qt.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Reviewer PASS on resolved source; no carryover or test repin. |
| 2 | PASS | New regression and all 38 executor-family tests PASS by name. All 34 required corpus targets (57 result records) fresh PASS. Full make check 287/287 PASS. Only run() and 3 test files changed; stop/simGuard unchanged. |
| 3 | PASS, attributed | Five complete farm phases plus 16 successful floor checks. Raw Go events 125653 PASS/1 FAIL/370 SKIP. One retained non-owned legacy-close failure attributed below; all 11 owned tests PASS. |
| 4 | PASS | Zero unresolved blocking/high review findings. |
| 5 | PASS | Isolated reviewed-source branch plus this report; normal commit hook and clean status checked before publication. Prior scratch reports preserved separately. |
| 6 | PASS | Clean canonical merge; full //... build and make vet exit 0. Normal scratch commit hook ran; generated tree equals expected merge tree. |
| 7 | PASS | One executor-settlement theme; ancestry scoped to ga-nua32sk/ga-d8zwn7m, no denied paths or unrelated commits, no stack. |

fast_lane: PASS before suite.
policy_lane: PASS — make fmt-check; make vet/nogo; four shell guards; ten policy targets.
drift_lane: PASS — make bazel-sync + git diff --exit-code; seven generated-artifact targets.

Criteria source: supplied release-gate protocol; docs/PROJECT_MANIFEST.md is absent. Commands resolved from TESTING.md, Makefile and the Bazel CI workflow.

Fast lane evidence: isolated fresh views, frozen base; format, nogo/vet, four shell guards, 10/10 policy targets, make bazel-sync plus empty git diff, 7/7 generated-artifact targets. golangci-lint pin/resolved 2.12.0, mismatch=false.

Phase results: unit 287/287, acceptance 7/7, integration-packages 36/36 PASS; full integration 13/14 shards PASS plus one attributed FAILED shard; latency 1/1 PASS. All 497 remote results uncached, first attempt, inside phase windows. The native floor is required supplemental coverage; no full-suite farm/local stitching.

failure_attribution: TestGCLiveContract_BeadsAndEvents -> ga-kjf435d | clause 3:a. At gc_live_contract_test.go:497 the actual server returned HTTP 500 closing a session: lost the revision fence 3 times. Site UNCHANGED/shared_hunks0, separate test/integration package. Tracker predates this run. Test-binary closure 63 directories excludes cmd/gc; external gc closure 157 directories includes it, and that edge is explicitly accounted: actual server logs legacy fallback because v2 is unavailable on its work-binding stores. The only executor construction is newDefaultPlanner, called only under v2 wiring; legacy wiring rejects a planner. Thus the changed executor is never constructed in the failing server. Close/store/wiring blobs match the frozen base. Repeat fix-carrying exception: own build ga-nua32sk fixes ga-shh294r; blocker has stamped unlanded fix ga-pp5v22v/deploy ga-1hoqgyh, ancestor check 1 at freshly fetched main. Proof integration-failure-proof.json. Tracker sighting comment 7c921e72-0979-56f3-ad0c-20efd57d8f42 read back. No retry, source change, duplicate fix or waiver.

Original execution exit 1/criterion3_valid=false is preserved in execution.original.json. A guarded continuation ran only latency/floor, hashes of completed phases unchanged, zero completed-phase reruns. Original FAILED evidence remains FAILED. assessment.json records the gate attribution separately.

skip_justification: all reported skip sites UNCHANGED, zero touched: 267 sites plus 4 supplemental shared-helper sites. Original 356 entries comprise 71 environment-absence entries selected for floor top-up and 285 other platform/provider/opt-in exclusions. Unsupported exec/ssh/K8s fencing explains shared conformance skips. The raw dispatch log supplies the parser's missing reason: ga-vnycm2.27 abort-scope metadata. This diff changes neither skip conditions nor provider capabilities; no owned test skips. The native top-ups retain 39 non-owned skip entries at 36 additional verified UNCHANGED sites (floor-skip-inventory.json and floor-skip-sites.txt), covering unchanged platform/provider/opt-in guards. Full original reasons/site proofs remain in skip-inventory.json, skip-sites-evidence.txt and skip-sites-supplement.txt.

Runtime: rootless podman active; cached dolt-sql-server:2.2.0 matches deps.env; wrapper pins test bd 1.3.1. ICU74 probe OK with recorded provenance. Initial cold local-heavy scratch setup ran no tests; owned service stopped, same staged merge committed through normal hooks/configured RBE. Logs retained.

```json
{
  "verdict": "PASS",
  "criterion3": "PASS with one attributed non-owned failure",
  "test_cmd_scope": "full-suite",
  "test_exec_mode": "remote-exec",
  "test_counts": {
    "PASS": 125653,
    "FAIL": 1,
    "SKIP": 370
  },
  "test_exec_bep": {
    "unit": {
      "results": 373,
      "remote_executed": 373,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "acceptance": {
      "results": 24,
      "remote_executed": 24,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "integration-packages": {
      "results": 85,
      "remote_executed": 85,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "integration-full": {
      "results": 14,
      "remote_executed": 14,
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
  "farm_test_seconds": 9017.961,
  "test_log_dir": "/var/tmp/ga-d8zwn7m-deploy.5xxn21qt/c3/evidence/remote/logs",
  "rbe_state": {
    "rc_marker_present": true,
    "kill_switch_absent": true,
    "cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT",
    "worker_env_tree": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45",
    "worker_env_origin_main": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45"
  },
  "fallback_reason": "none",
  "admission": {
    "class": "remote",
    "floor_gib": 25.0,
    "free_gib": 37.675689697265625,
    "waited_seconds": 0.001,
    "cap": 2
  },
  "continuation_admission": {
    "class": "remote",
    "floor_gib": 25.0,
    "free_gib": 39.40684127807617,
    "waited_seconds": 0.002,
    "cap": 2
  },
  "skip_inventory": {
    "environment-absence": 71,
    "other": 285,
    "file": "/var/tmp/ga-d8zwn7m-deploy.5xxn21qt/c3/evidence/remote/skip-inventory.json",
    "unchanged_sites": 267,
    "supplemental_sites": 4
  },
  "owned_topup": [],
  "floor_topup": {
    "complete": true,
    "steps": 16,
    "record": "/var/tmp/ga-d8zwn7m-deploy.5xxn21qt/c3/evidence/remote/floor-topup.json",
    "skip_entries": 39,
    "skip_inventory": "/var/tmp/ga-d8zwn7m-deploy.5xxn21qt/floor-skip-inventory.json"
  },
  "latency_leg": {
    "complete": true,
    "phase": "integration-latency",
    "tests": [
      "TestGcBdLatencyBudget",
      "TestGcSeatHookLatencyBudget"
    ],
    "attempts_allowed": 2,
    "attempts": [
      {
        "attempt": 1,
        "rc": 0,
        "probe_verdict": "clear",
        "phase": [
          "GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$",
          "GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"
        ],
        "results": {
          "TestGcBdLatencyBudget": {
            "PASS": 1
          },
          "TestGcSeatHookLatencyBudget": {
            "PASS": 1
          }
        },
        "farm_test_seconds": 56.395,
        "evidence_errors": [],
        "probe": [
          "GATE_FARM_PROBE verdict=clear waited=0s load1=0.01 load5=1.59 load15=4.12 cpus=32 threshold=50 max_wait=600s"
        ],
        "test": [
          "stores: city proxied-server, rig proxied-server; cpus 32; loadavg 1.81 1.88 4.15 4/949 48281"
        ]
      }
    ],
    "retired_farm_test_seconds": 0.0,
    "main_leg": [
      "GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"
    ],
    "main_leg_verdicts": {
      "TestGcBdLatencyBudget": {},
      "TestGcSeatHookLatencyBudget": {}
    },
    "verdicts": {
      "TestGcBdLatencyBudget": {
        "PASS": 1
      },
      "TestGcSeatHookLatencyBudget": {
        "PASS": 1
      }
    },
    "verdict": "measured",
    "message": "measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS",
    "rc": 0
  },
  "waiver_ref": "none",
  "heavy_mode": "none",
  "ci_lane_run": "n/a (no CI-config diff)",
  "failure_attribution": {
    "TestGCLiveContract_BeadsAndEvents": "ga-kjf435d; proof a; fix-carrying ga-nua32sk / ga-pp5v22v"
  },
  "test_cmd": [
    "test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/ga-d8zwn7m-deploy.5xxn21qt/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/ga-d8zwn7m-deploy.5xxn21qt/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/ga-d8zwn7m-deploy.5xxn21qt/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/ga-d8zwn7m-deploy.5xxn21qt/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/ga-d8zwn7m-deploy.5xxn21qt/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"
  ],
  "remote_phase_host_load": {
    "threshold": "15",
    "waited_seconds": "0",
    "wait_timed_out": "0",
    "run_start_load": "72.58",
    "run_max_load": "92.02",
    "run_mean_load": "64.11",
    "run_readings": "28",
    "wait_first_load": "72.58",
    "wait_max_load": "72.58",
    "wait_mean_load": "72.58",
    "wait_readings": "1",
    "read_errors": "0"
  },
  "load_threshold": "15",
  "load_waited_seconds": "1801",
  "load_wait_timed_out": "1",
  "run_start_load": "15.54",
  "run_max_load": "22.69",
  "run_mean_load": "19.87",
  "run_readings": "9"
}
```

Owned results from full-suite output:

```text
diff_tests_executed:
  TestAbandonedCreateSettlesAmbiguousWithToken PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-752a0bf25b15b465517a.log)
  TestAmbiguousFinalizeIsPacedByItsOwnBackoff PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestCreateAbandonedBeforeItsWriteStaysThrottled PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestDeadlinesHaveNoException PASS (integration-packages-e5330b8c981927473156.log, unit-f679cd2551fcc64525aa.log)
  TestEffectContextCarriesTheDeadline PASS (integration-packages-12697e2ea0d32c278115.log, unit-2bde55fab377f50b4739.log)
  TestExecutorCausesCarryTheFinalizePrefix PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-23e7d9fc763496dd8095.log)
  TestExecutorPostsALateLandingsEvent PASS (integration-packages-9326181a736eb30b71ca.log, unit-4d20b3d28497f844aa39.log)
  TestExecutorSettlesAnAbandonedEffectByItsLatch PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-fd986cf5bab8709b8d45.log)
  TestExecutorSettlesAtTheDeadlineWhenTheClockMovesAfterARead PASS (integration-packages-75af79934639abaae69c.log, unit-27b23e708b477fce4dad.log)
  TestExecutorSettlesOnceUnderFakeClock PASS (integration-packages-830685c60d0c2dd88567.log, unit-cd6930bb0d0da8646500.log)
  TestTxCallIsGatedLikeAWrite PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-088cc38d26d094729229.log)
GATE_TEST_EVIDENCE_RESULT ok: 11 diff-owned tests verified by name
```

Native load-gate metrics (threshold, wait/timed-out, run_start/max/mean/readings):

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=15.54 run_max_load=22.69 run_mean_load=19.87 run_readings=9 wait_first_load=82.83 wait_max_load=87.11 wait_mean_load=46.87 wait_readings=61 read_errors=0
```

Deployer performs no merge. Clearance and merge-request pin the resulting isolated deploy head; a changed head requires a fresh gate.
