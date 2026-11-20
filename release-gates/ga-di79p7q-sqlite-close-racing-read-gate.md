# SQLite read/close race release gate — ga-di79p7q

**Verdict:** **PASS**

Evaluated 2026-10-15T07:42:11.420334+00:00. Remote PR path; no production binary installation or rollout.

- Reviewed source: eedde9dfc7e2da423831eae040d8897b107d2411
- Review: ga-67vd7dp; designated fix/build: ga-2mchg52
- Gate base: d8adec76e8634fa82582c4054e9b924f30d1f126 (origin/main captured before validation)
- Canonical tested merge: a0bacc5e730f789226e6134525a4f115d38d50f9
- Canonical tested tree: ad2f91130d6af6d58b2cf4c0159a5eddd1ef25b4
- Test policy: TESTING.md, Makefile and .github/workflows/bazel.yml. docs/PROJECT_MANIFEST.md is absent.
- Full evidence context: /var/tmp/deploy-ga-di79p7q.fresh.eok_kygf/context.json

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Reviewer verdict pass at the exact resolved source; no blocking or requested findings. Fresh live review archived in review-live.json. |
| 2 | Acceptance criteria met | PASS | CloseStore atomically latches closure, preserves both closed handles, and ensureOpen rejects new calls. Deterministic read/close test passed by name in the full suite. Static code evidence: /var/tmp/deploy-ga-di79p7q.fresh.eok_kygf/acceptance-static.json. |
| 3 | Tests pass | PASS | Four full forced-execution farm phases plus all mandatory native floor steps pass; 110036 PASS, 0 FAIL, 336 SKIP top-level job-log results. The sole diff-owned test is PASS. |
| 4 | No open high-severity review findings | PASS | Review says REQUIRED (blocking): none and REQUESTED IN THE SAME ROUND: none. Optional ga-y0urr6p and ga-ds8b0wo remain non-gating. |
| 5 | Final feature tree clean | PASS | Canonical merge checkout clean, exact merge/tree asserted after all lanes. Publication adds only this gate Markdown; final head and merge-tree proof are recorded before push. |
| 6 | Branch integrates cleanly with main | PASS | Clean materialized merge at the pinned base; //cmd/gc:gc_test build and whole-repository make vet/nogo pass. No self-rebase needed. |
| 7 | Single feature theme | PASS | Three source paths under internal/beads: safe SQLite closure during an in-flight read, its regression test and a corrected test comment. Ancestry scope guard accepts this deploy and its confirmed build bead. |

## Fast lane and CI coverage

Evaluation order: already-merged preflight, merge/build/vet validation, review and acceptance checks, then isolated fresh-view fast lanes before runtime setup or the suite. The commit/pulls preflight returned no associated PR.

Formatting, shell policy guards, hook ownership, CI policy tests and whole-tree lint pass. Base-relative gocyclo v0.6.0 at CCN20 reports no threshold changes. Policy targets 11/11, generated drift 6/6, OpenAPI compatibility 1/1 pass. make bazel-sync followed by git diff --exit-code and a clean tracked/untracked status passes. Every lane ran in its own fresh view; the suite's merge checkout stayed unchanged.

Fast artifacts, including digest-verified test logs: /var/tmp/gc-heavy-gate/runs/deploy-ga-di79p7q.fresh.eok_kygf/fast.

## Fresh full-suite execution

| Phase | Results | Remote executed | Cached remote / local | Retried | Start window and terminal verdicts |
|---|---|---|---|---|---|
| unit | 366 | 366 | 0 / 0 | 0 | PASS |
| acceptance | 24 | 24 | 0 / 0 | 0 | PASS |
| integration-packages | 77 | 77 | 0 / 0 | 0 | PASS |
| integration-full | 14 | 14 | 0 / 0 | 0 | PASS |

test_cmd_scope: full-suite. Phase 4 is full integration, without a smoke filter. Build/nogo caches are permitted; no cached test result is credited.

test_cmd:
~~~json
[
  "test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/deploy-ga-di79p7q.fresh.eok_kygf/criterion3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
  "test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/deploy-ga-di79p7q.fresh.eok_kygf/criterion3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
  "test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/deploy-ga-di79p7q.fresh.eok_kygf/criterion3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
  "test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/deploy-ga-di79p7q.fresh.eok_kygf/criterion3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"
]
~~~

test_log_dir: /var/tmp/gc-heavy-gate/runs/deploy-ga-di79p7q.fresh.eok_kygf/criterion3/evidence/remote/logs. The retained logs were read with gate-test-evidence.py, with the diff-owned inventory, after all phases and native top-ups. Counts sum top-level results across job logs, so a test executed by multiple jobs is counted per job.

diff_tests_executed: diff_tests_executed:
  TestSQLiteStoreGetRacingCloseStoreReturnsAnError PASS (integration-packages-999a89ecc5b3d09abdd4.log, unit-a93729df64a652fced43.log)
GATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name


skip_justification: all skips are non-owned. Farm skips are retained individually with their printed reason in execution.json's skip_inventory; native top-up skips are retained in floor-skip-inventory.json, summarized below. Native floor selection covers every environment-absence skip by package, beadstest, Darwin compilation and runner self-tests; all floor commands exit0. Remaining platform, opt-in and lane-gating skips are inventoried. In the two touched test files, shared hunks are a comment, a stdlib driver import and new gate helpers used only by the new regression; they do not alter existing skip conditions. No owned test skipped or failed.

Runtime setup verified the rootless podman socket, enabled testcontainer sweep and pinned Dolt image 2.2.0. The wrapper reports PASS-THROUGH — NOT ISOLATED; its raw inherited-environment banner is retained, and this record makes no cloned-environment claim. Tests use the CI-pinned bd v1.3.1 rather than the fleet bd.

## Execution and load evidence

~~~json
{
  "test_exec_mode": "remote-exec",
  "test_exec_bep": {
    "unit": {
      "results": 366,
      "remote_executed": 366,
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
      "results": 77,
      "remote_executed": 77,
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
    }
  },
  "rbe_state": {
    "rc_marker_present": true,
    "kill_switch_absent": true,
    "cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT",
    "worker_env_tree": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0",
    "worker_env_origin_main": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0"
  },
  "fallback_reason": null,
  "admission": {
    "class": "remote",
    "floor_gib": 25.0,
    "free_gib": 102.20189666748047,
    "waited_seconds": 0.0,
    "cap": 2
  },
  "farm_test_seconds": 8333.451,
  "owned_topup": [],
  "floor_skip_inventory": {
    "path": "/var/tmp/deploy-ga-di79p7q.fresh.eok_kygf/floor-skip-inventory.json",
    "counts_by_reason_class": {
      "environment-absence": 29,
      "other": 7
    }
  },
  "test_cmd_scope": "full-suite",
  "test_counts": {
    "PASS": 110036,
    "FAIL": 0,
    "SKIP": 336
  },
  "diff_tests_executed": "diff_tests_executed:\n  TestSQLiteStoreGetRacingCloseStoreReturnsAnError PASS (integration-packages-999a89ecc5b3d09abdd4.log, unit-a93729df64a652fced43.log)\nGATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name\n",
  "skip_inventory": {
    "path": "/var/tmp/gc-heavy-gate/runs/deploy-ga-di79p7q.fresh.eok_kygf/criterion3/evidence/execution.json",
    "counts_by_reason_class": {
      "other": 250,
      "environment-absence": 72
    },
    "top_level_counts_by_reason_class": {
      "other": 201,
      "environment-absence": 42
    }
  },
  "floor_topup": {
    "complete": true,
    "steps": 16,
    "all_exit_codes": 0,
    "record": "/var/tmp/gc-heavy-gate/runs/deploy-ga-di79p7q.fresh.eok_kygf/criterion3/evidence/remote/floor-topup.json"
  },
  "load_evidence": [
    {
      "file": "/var/tmp/gc-heavy-gate/runs/deploy-ga-di79p7q.fresh.eok_kygf/chain/units/gate.w1.stderr.log",
      "summary": "LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=16.62 run_max_load=50.53 run_mean_load=33.29 run_readings=103 wait_first_load=16.62 wait_max_load=16.62 wait_mean_load=16.62 wait_readings=1 read_errors=0",
      "metrics": {
        "run_start_load": "16.62",
        "run_max_load": "50.53",
        "run_mean_load": "33.29",
        "run_readings": "103",
        "wait_first_load": "16.62",
        "wait_max_load": "16.62",
        "wait_mean_load": "16.62",
        "wait_readings": "1",
        "read_errors": "0",
        "load_threshold": "15",
        "load_waited_seconds": "0",
        "load_wait_timed_out": "0"
      }
    },
    {
      "file": "/var/tmp/gc-heavy-gate/runs/deploy-ga-di79p7q.fresh.eok_kygf/criterion3/evidence/remote/floor-topup.stdout",
      "summary": "LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=33.90 run_max_load=36.52 run_mean_load=34.84 run_readings=9 wait_first_load=35.12 wait_max_load=49.78 wait_mean_load=31.28 wait_readings=61 read_errors=0",
      "metrics": {
        "run_start_load": "33.90",
        "run_max_load": "36.52",
        "run_mean_load": "34.84",
        "run_readings": "9",
        "wait_first_load": "35.12",
        "wait_max_load": "49.78",
        "wait_mean_load": "31.28",
        "wait_readings": "61",
        "read_errors": "0",
        "load_threshold": "15",
        "load_waited_seconds": "1800",
        "load_wait_timed_out": "1"
      }
    }
  ],
  "waiver_ref": "none",
  "heavy_mode": "none",
  "ci_lane_run": "n/a (no CI-config diff)"
}
~~~

The remote suite has no host-load wait. The native floor uses the recorded local-heavy soft load gate; the raw LOAD_GATE_SUMMARY lines above contain threshold, waited seconds, timeout flag, run_start_load, run_max_load, run_mean_load and run_readings.

failure_attribution: none; no failed verdict. policy_attribution: none. waiver_ref: none; gascity has no waiver path.

## Landing obligations

Keep ga-qcg71ff OPEN until actual main landing. ga-kkseu6c must verify this fix is on main before its own fresh gate. ga-y0urr6p (write-path test coverage) and ga-ds8b0wo (watcher lifecycle) remain separate follow-ups. Opening a PR or closing this deploy bead does not establish landing.
