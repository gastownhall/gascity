# Release gate: federated custom scale checks (ga-z1rwpm9)

**Verdict:** **PASS**

City-scoped custom scale checks sum demand from the city and active rig stores. Claim queries exclude suspended rigs, keeping the store sets aligned.

Reviewed source: `7fcf3bcbb7298d9e36cca5220414fa2466c437e0`. Review: ga-006hh0l, round 2 PASS. Build lineage: ga-drb140, ga-i7b6sl, ga-c6p9zc, ga-64hgmss. Source branch builder/ga-ohp3xf is provenance only; deploy branch is deploy/ga-z1rwpm9-gate.

DEPLOY_MODE=remote; PUSH_REMOTE=fork; BASE_REF=origin/main. Pinned gate base: `e2bf5075c4e184126bf9c181b8ab1b8fac6c85d0`. Canonical tested merge: `4fc661b7204b91437dfb0f6d4e00d1d1a2369620`; tree `bdabfe275d86bff43db5aba6300aa5a3ab3599cb`. The materialized tree equals independent git merge-tree output. Real pre-commit hooks ran. This evidence uses a completely fresh gate after ga-6ilvcxw's fix landed; the earlier partial failed gate contributes no results.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Live review ga-006hh0l records verdict pass on the exact reviewed source, 17 owned tests, no uncovered or deferred criteria. |
| 2 | Acceptance criteria | PASS | Code and independent tests verify per-store binding, additive counts, suspended-rig exclusion, both pool forms, allocator symmetry, concurrent probes sharing one semaphore, and per-probe panic recovery. Details below. |
| 3 | Complete CI-equivalent tests | PASS | Four forced remote phases and the mandatory native floor completed. 112718 PASS / 0 FAIL / 340 non-owned SKIP result lines. All 17 owned tests PASS by name. |
| 4 | No unresolved HIGH findings | PASS | Round-2 review has no blockers or HIGH findings; five coverage follow-ups are explicitly non-blocking under ga-b5k1vvz. |
| 5 | Clean release payload | PASS | Tested merge has empty git status; publication verifies a clean isolated branch containing the reviewed source plus only this gate record. |
| 6 | Clean divergence and merged build | PASS | Conflict check rc0, predicted/materialized trees equal; remote all-target build and full-tree make vet/nogo passed on the merged tree. No self-rebase needed. |
| 7 | One feature theme | PASS | One store-federation correction across demand, allocator reads and claims, its regressions, BUILD wiring and contributor documentation. Ancestry guard rc0 with all four confirmed sibling IDs; no .claude paths. |

Acceptance checks:

- A real shell fixture returns distinct counts from city, two active rigs and a suspended rig. Both pool forms and the allocator sum the active counts; the suspended count is excluded.
- Every rig probe carries its own BEADS_DIR and Dolt endpoint. An unresolvable rig environment is reported and contributes zero without reading another store.
- Barrier-based tests establish concurrent probes; a capacity-one test checks use of the caller's semaphore. The aggregate is clamped once, while new demand remains unclamped. A probe panic is recovered and reported.
- appendRigHookStores uses the same suspended-rig helper. This intentionally changes cross-store claims for every eligible city-scoped agent. Rig-local and default checks retain their existing paths.
- engdocs/architecture/dispatch.md documents store-set symmetry and store binding. Review/source inspection confirms no API, schema, dashboard or CI-config change.

Fast lane, before the suite: remote bazel build //..., make vet/nogo, pinned golangci-lint 2.12.0 formatting, full nogo policy and make check shell guards, eight forced remote policy/format targets, and make bazel-sync followed by git diff --exit-code all PASS. Each policy, format and drift lane used its own pristine view and gate-base.sh pinned to the base above. Required generated drift was found in .github/workflows/bazel.yml. make check-hooks confirms .githooks is active. No PROJECT_MANIFEST.md exists; canonical commands come from AGENTS.md, TESTING.md, Makefile and CI.

test_cmd: gate-criterion3.sh, detached through gate-detached-run.sh; remote admission -> load-gate-run.sh --class remote -> isolated-test-run.sh --rbe -> gate-rbe-run.py. Phases are make check (bazel test //... plus shell guards), acceptance including solo targets, integration-packages, and FULL integration without a smoke filter. Every phase forces --nocache_test_results and --noremote_local_fallback. Exact argv and flags are retained with each phase. The native floor includes beadstest, Darwin compile, runner self-tests and every environment-absence SKIP by package.

| Phase | Fresh remote result attempts | Cached | Retried | Terminal verdict |
|---|---:|---:|---:|---|
| unit | 368 | 0 | 0 | PASS |
| acceptance | 24 | 0 | 0 | PASS |
| integration-packages | 78 | 0 | 0 | PASS |
| integration-full | 14 | 0 | 0 | PASS |

All attempts used strategy remote, attempt 1, with starts inside their phase window and terminal PASS verdicts. Native top-ups are separate host evidence, not a stitched replacement suite. All 968 raw artifacts were independently verified against SHA256/CAS digests; 484 normalized log copies match the raw logs after the runner's documented removal of control byte0x16.

```json
{
  "test_exec_mode": "remote-exec",
  "fallback_reason": null,
  "exit_code": 0,
  "test_cmd_scope": "full-suite",
  "evidence_dir": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote",
  "admission": {
    "class": "remote",
    "floor_gib": 25.0,
    "free_gib": 102.41288375854492,
    "waited_seconds": 0.001,
    "cap": 2
  },
  "rbe_state": {
    "rc_marker_present": true,
    "kill_switch_absent": true,
    "cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT",
    "worker_env_tree": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0",
    "worker_env_origin_main": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0"
  },
  "test_exec_bep": {
    "unit": {
      "results": 368,
      "remote_executed": 368,
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
      "results": 78,
      "remote_executed": 78,
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
  "farm_test_seconds": 7617.371,
  "criterion3_valid": true,
  "owned_topup": [],
  "floor_topup": {
    "complete": true,
    "steps": [
      {
        "package": "cmd/gc",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/floor-logs/floor-0.log"
      },
      {
        "package": "examples/gastown",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/floor-logs/floor-1.log"
      },
      {
        "package": "internal/beads",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/floor-logs/floor-2.log"
      },
      {
        "package": "internal/beads/beadstest",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/floor-logs/floor-3.log"
      },
      {
        "package": "internal/beads/contract",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/floor-logs/floor-4.log"
      },
      {
        "package": "internal/beads/exec",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/floor-logs/floor-5.log"
      },
      {
        "package": "internal/doctor",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/floor-logs/floor-6.log"
      },
      {
        "package": "internal/mail/exec",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/floor-logs/floor-7.log"
      },
      {
        "package": "internal/runtime/herdr",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/floor-logs/floor-8.log"
      },
      {
        "package": "internal/runtime/runtimetest",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/floor-logs/floor-9.log"
      },
      {
        "package": "internal/runtime/tmux",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/floor-logs/floor-10.log"
      },
      {
        "package": "test/acceptance/helpers",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/floor-logs/floor-11.log"
      },
      {
        "package": "test/integration",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/floor-logs/floor-12.log"
      },
      {
        "check": "fsys-darwin-compile",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/floor-logs/floor-13.log"
      },
      {
        "check": "push-gate-lock-selftest",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/floor-logs/floor-14.log"
      },
      {
        "check": "local-concurrency-selftest",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/floor-logs/floor-15.log"
      }
    ],
    "full_record": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/floor-topup.json"
  },
  "skip_inventory": {
    "file": "/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/skip-inventory.json",
    "events": 326,
    "unchanged_sites": 251,
    "unresolved": 0,
    "counts_by_reason_class": {
      "other": 255,
      "environment-absence": 71
    },
    "top_level_counts_by_reason_class": {
      "other": 206,
      "environment-absence": 42
    }
  },
  "test_cmd": [
    "test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"
  ],
  "test_counts": {
    "PASS": 112718,
    "FAIL": 0,
    "SKIP": 340
  },
  "diff_tests_executed": "diff_tests_executed:\n  TestAppendRigHookStoresExcludesSuspendedRig PASS (integration-packages-830685c60d0c2dd88567.log, unit-0e1499ad8e8a0a6b9d81.log)\n  TestBuildDemandTargetsGoldenAgainstInlineLoop PASS (integration-packages-cb110414c557c0928e82.log, unit-cd6930bb0d0da8646500.log)\n  TestBuildDesiredState_CityScopedCustomScaleCheckSumsEveryActiveStore PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-1cc8c8ab55e59a9e03a9.log)\n  TestCityScopedFanOutProbesBindEachProbeToItsOwnStore PASS (integration-packages-074fc6c34a1fb2577127.log, unit-1ab526405ace21879f85.log)\n  TestCityScopedFanOutProbesIncludesCityAndNonSuspendedRigsOnly PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-423c6a0da1986b90e9a7.log)\n  TestCityScopedFanOutRigEnvFailureContributesZeroAndIsNeverRunUnderCityEnv PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-752a0bf25b15b465517a.log)\n  TestEvaluatePendingPoolsFanOutPrefixesEachProbeWithItsOwnDoltEndpoint PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-fd986cf5bab8709b8d45.log)\n  TestEvaluatePendingPoolsFanOutRecoversPanickingProbe PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-3d6d3036a2f5902472b5.log)\n  TestEvaluatePoolFanOutSumBestEffortOnProbeError PASS (integration-packages-9326181a736eb30b71ca.log, unit-0e1499ad8e8a0a6b9d81.log)\n  TestEvaluatePoolFanOutSumClampsAggregateOnce PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-64014bc0bb3833bc198c.log)\n  TestEvaluatePoolFanOutSumNewDemandLeavesAggregateUnclamped PASS (integration-packages-12697e2ea0d32c278115.log, unit-493754ea2d5b4d8858c8.log)\n  TestEvaluatePoolFanOutSumRealRunnerSumsEachStoresOwnCount PASS (integration-packages-75af79934639abaae69c.log, unit-d9e667428f972d067bd3.log)\n  TestEvaluatePoolFanOutSumRunsProbesConcurrently PASS (integration-packages-e4245ddf6a37030111bc.log, unit-27b23e708b477fce4dad.log)\n  TestEvaluatePoolFanOutSumSharesCallerSemaphoreNotNested PASS (integration-packages-821a75e4a503601af052.log, unit-637c74c578a2b17fc654.log)\n  TestEvaluatePoolFanOutSumSumsAcrossProbes PASS (integration-packages-e5330b8c981927473156.log, unit-2ef13e1b6898c5544764.log)\n  TestRunCustomScaleChecks_CityScopedCustomScaleCheckSumsEveryActiveStore PASS (integration-packages-509ef0a6cd81866833cc.log, unit-abd97d2cb9b450225d57.log)\n  TestRunCustomScaleChecks_CityScopedRigEnvFailureIsPartial PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-f93a0ca96ccc61d0cae9.log)\nGATE_TEST_EVIDENCE_RESULT ok: 17 diff-owned tests verified by name\n"
}
```

The wrapper explicitly reports PASS-THROUGH for this materialized scratch; no isolated-clone claim is made. The remote test actions use Bazel's pinned worker environment. bd is pinned to v1.3.1, verified ref c1c4b642a. Podman socket and cached dolt-sql-server:2.2.0 were checked before launch. Initial setup floor40GiB passed at102.4GiB free. Remote admission uses25GiB/cap2; native runs retain their local load wait. Exact LOAD_GATE_SUMMARY lines are appended below.

skip_justification: Every owned test passed. All 251 resolved remote skip sites are UNCHANGED under diff-owned-tests.py, and no shared hunk can change their skip conditions. The mandatory native floor attempted all environment-absence cases; remaining non-owned skips are inventoried as unavailable tooling/fixtures, platform requirements or configured lane exclusions. None is credited as a test PASS. No candidate failures were attributed.

test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/logs
test_cmd_scope: full-suite
heavy_mode: none
ci_lane_run: n/a (no CI-config change)
failure_attribution: none
waiver_ref: none (gascity has no waiver path)

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/logs logs=500 logs_with_results=434
test_counts: PASS=112718 FAIL=0 SKIP=340
  (top-level result lines, column 0, summed over the 500 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
diff_tests_executed:
  TestAppendRigHookStoresExcludesSuspendedRig PASS (integration-packages-830685c60d0c2dd88567.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestBuildDemandTargetsGoldenAgainstInlineLoop PASS (integration-packages-cb110414c557c0928e82.log, unit-cd6930bb0d0da8646500.log)
  TestBuildDesiredState_CityScopedCustomScaleCheckSumsEveryActiveStore PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestCityScopedFanOutProbesBindEachProbeToItsOwnStore PASS (integration-packages-074fc6c34a1fb2577127.log, unit-1ab526405ace21879f85.log)
  TestCityScopedFanOutProbesIncludesCityAndNonSuspendedRigsOnly PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-423c6a0da1986b90e9a7.log)
  TestCityScopedFanOutRigEnvFailureContributesZeroAndIsNeverRunUnderCityEnv PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-752a0bf25b15b465517a.log)
  TestEvaluatePendingPoolsFanOutPrefixesEachProbeWithItsOwnDoltEndpoint PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-fd986cf5bab8709b8d45.log)
  TestEvaluatePendingPoolsFanOutRecoversPanickingProbe PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-3d6d3036a2f5902472b5.log)
  TestEvaluatePoolFanOutSumBestEffortOnProbeError PASS (integration-packages-9326181a736eb30b71ca.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestEvaluatePoolFanOutSumClampsAggregateOnce PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-64014bc0bb3833bc198c.log)
  TestEvaluatePoolFanOutSumNewDemandLeavesAggregateUnclamped PASS (integration-packages-12697e2ea0d32c278115.log, unit-493754ea2d5b4d8858c8.log)
  TestEvaluatePoolFanOutSumRealRunnerSumsEachStoresOwnCount PASS (integration-packages-75af79934639abaae69c.log, unit-d9e667428f972d067bd3.log)
  TestEvaluatePoolFanOutSumRunsProbesConcurrently PASS (integration-packages-e4245ddf6a37030111bc.log, unit-27b23e708b477fce4dad.log)
  TestEvaluatePoolFanOutSumSharesCallerSemaphoreNotNested PASS (integration-packages-821a75e4a503601af052.log, unit-637c74c578a2b17fc654.log)
  TestEvaluatePoolFanOutSumSumsAcrossProbes PASS (integration-packages-e5330b8c981927473156.log, unit-2ef13e1b6898c5544764.log)
  TestRunCustomScaleChecks_CityScopedCustomScaleCheckSumsEveryActiveStore PASS (integration-packages-509ef0a6cd81866833cc.log, unit-abd97d2cb9b450225d57.log)
  TestRunCustomScaleChecks_CityScopedRigEnvFailureIsPartial PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-f93a0ca96ccc61d0cae9.log)
GATE_TEST_EVIDENCE_RESULT ok: 17 diff-owned tests verified by name
```

Non-blocking regression follow-up ga-b5k1vvz already has the OPEN main-landing guard ga-9ic6ri. Neither closes when this deploy bead closes; actual main landing is the named exit. No production binary deployment is requested. Merge authority remains MPR's.

Retained evidence: /var/tmp/deploy-ga-z1rwpm9.7DM5BRpc and /var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence. Source and merge objects are resolved by git, not inferred from branch tips.

Load observations:

```text
/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=11.99 run_max_load=16.54 run_mean_load=14.51 run_readings=34 wait_first_load=11.99 wait_max_load=11.99 wait_mean_load=11.99 wait_readings=1 read_errors=0
/var/tmp/gc-heavy-gate/runs/ga-z1rwpm9.c3.7DM5BRpc/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.95 run_max_load=14.09 run_mean_load=13.82 run_readings=6 wait_first_load=13.95 wait_max_load=13.95 wait_mean_load=13.95 wait_readings=1 read_errors=0
```
