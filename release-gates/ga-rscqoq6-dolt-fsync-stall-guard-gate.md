# Release gate: bounded fsync probe and recovery guard (ga-rscqoq6)

**Verdict:** **PASS**

The managed exec-provider recovery operation defers while a live, listening Dolt's data-directory probe reports a stall. Explicit restart refuses unless forced. The guard has not been observed to fire against a real stalled filesystem; script tests use fake probe replies.

Deploy source: `469d05da6847394063f71eec2b0d5d249a6b902b`. Original reviewed source: `c0bfe315cc20a0281e37975bcc8a8af922c187db`, reviewer ga-jbflyyl round2 PASS. Message-only reword carryover was independently verified: both trees3f604f93a1ddbfbefc5908a078cda9b9f9c9c401 and stable patch-id14735e53f26e1c9ca130cbeee51cb9550c3fc1b6. The uncited old commit is not deployed. Confirmed ancestry IDs: ga-rscqoq6, ga-yrodto, ga-3ayztoc.

DEPLOY_MODE=remote; PUSH_REMOTE=fork; BASE_REF=origin/main. Pinned gate base: `e8e77bc3029b262b8324287eeb3b885fe6fed147`. Canonical tested merge: `5ffa8a73cfd0ba44cad72242b0dce951570c6888`; tree `cae8d4fd964cc7130a4b50aa59bd1ef7d60dddd1`. Independent merge-tree prediction and materialized tree match exactly. Real pre-commit hooks ran; no self-rebase was needed. The source branch builder/ga-yrodto is provenance only. Publication uses the isolated deploy/ga-rscqoq6-gate branch at the resolved source plus this gate record.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | ga-jbflyyl records PASS on the original source; same-tree/same-patch message-only carryover independently recomputed before use. |
| 2 | Acceptance criteria | PASS | Code, all35 owned tests and the separately verified PR-stage caller/RED/GREEN disclosures cover the eleven acceptance criteria below. The full complexity command's pre-existing failure is explicitly attributed, not claimed green. |
| 3 | Full CI-equivalent suite | PASS | Four forced remote phases and mandatory native floor completed; 113364 PASS / 0 FAIL / 340 non-owned SKIP result lines. All35 owned tests PASS by name. |
| 4 | No unresolved HIGH findings | PASS | Live round2 review has no blocker/major; accepted minor findings and coverage limits remain disclosed. |
| 5 | Clean release payload | PASS | Tested merge status is empty. Publication verifies the isolated branch is clean and adds only this gate record to the reviewed source. |
| 6 | Clean divergence and merged build | PASS | Current-base conflict check rc0; materialized tree matches prediction; remote all-target build and full-tree make vet/nogo PASS. |
| 7 | One feature theme | PASS | Bounded fsync measurement, exec-provider/restart decisions, required metrics/BUILD wiring and direct regressions. Ancestry guard rc0; no .claude contamination. |

Acceptance verification:

1. Round1 RED remains explicitly uncommitted, as accepted by the reviewer. Its historical raw log expired. The committed round2 RED was independently replayed with four genuine failing regression roots; exact terminal output and current-candidate GREEN are in the PR. The replay is focused supplementary history evidence and contributes no criterion3 counts.
2. The hidden command returns typed tab-separated measurements, finished/unfinished exit0 and error nonzero. Source inspection verifies unique0600 regular dotfile, O_EXCL,4096bytes in the data-directory root and removal after completion; tests cover result format, errors, positive bounds and real command chaining. Uniqueness/O_EXCL are code-inspected, not claimed as dedicated tested invariants.
3. The caller abandons the private result wait at its timer while a detached Setsid worker can finish later. TestProbeBoundHoldsForCaller uses the internal start seam with a never-answering stand-in; the real command-chain test covers finished/error cases. No in-tree real-wrapper never-answering-worker test is claimed. Process spawning is outside the result-wait bound.
4. op_recover checks ENOSPC first, then a TCP-gated fsync result, before the recover-managed helper or fallback stop. Fake call logs prove recovery is not invoked for unfinished/slow-finished measurements. Existing ENOSPC behavior is tested.
5. Missing/erroring helpers and malformed results fail open with a visible note; a non-listening legacy server is not probed. Remote servers retain their earlier refusal.
6. WARN1000ms is the probe bound and stall threshold; ERROR5000ms is parsed but has no consumer here. Empty, invalid and non-positive env values use defaults; leading zeros normalize before Go flag parsing.
7. Explicit restart probes before stop and refuses unless forced; force overrides refusal while still running the probe. Fail-open cases and ENOSPC precedence pass.
8. PR caller audit enumerates exec-provider health recovery, managed bd transport retry, private recover-managed/fallback stop, explicit restart, intentionally exempt explicit/shutdown/quiescence stops, and currently uncovered provider-owned/non-listening paths. The guard's unobserved live activation and internal/fsyncprobe home are stated.
9. Probe dotfiles leave journal sums and pack health/backup/compact listings unchanged in the dedicated fixtures.
10. Materialized pack contains the new helper, verified by an owned test; make bazel-sync produces no drift.
11. Full nogo/vet, syntax checks, pinned formatting, policy and full suite pass. The acceptance-required complexity command has the exact baseline-policy attribution below.

Fast lane before the suite: remote bazel build //..., full-tree make vet/nogo, pinned golangci-lint2.12.0 format, shell make check guards, eight forced remote policy/format targets, and make bazel-sync followed by git diff --exit-code PASS. Format/policy/drift/static checks ran in separate pristine views with gate-base.sh pinning this exact base. No API/schema/dashboard or CI-config change is in the diff. make check-hooks confirms .githooks is active. PROJECT_MANIFEST.md is absent; canonical commands come from AGENTS.md, TESTING.md, Makefile and CI.

policy_attribution: make complexity-check -> ga-q7p6is, explicit criterion3b baseline-policy mirror. Candidate and pristine base both exit2 with identical239-line reports, SHA25689d827214a59aeeb2f8e48c90294957e43725ecfbf711d3b3d5dc7f7ca4e28bc. scripts/ci/complexity.sh and engdocs/complexity-baseline.json are unchanged. newDoltStateCmd is touched but measures56 on both trees against stale baseline44; the check reports no threshold change versus pinned base. Added tests are excluded from this static analyzer. The existing tracker predates the run and the exact sighting was read-verified. The raw failure remains recorded; no baseline edit, runtime-failure attribution or waiver was used.

test_cmd: detached gate-criterion3.sh through remote admission, load-gate-run.sh --class remote and isolated-test-run.sh --rbe. Phases are make check (bazel test //... plus shell guards), acceptance plus solo targets, integration-packages, and FULL integration with no smoke filter. All phases force --nocache_test_results and --noremote_local_fallback. The native floor includes beadstest, Darwin compilation, runner self-tests and every environment-absence SKIP by package. Exact argv, raw BEPs, logs and verdicts are retained.

| Phase | Fresh remote result attempts | Cached | Retried | Terminal verdict |
|---|---:|---:|---:|---|
| unit | 369 | 0 | 0 | PASS |
| acceptance | 24 | 0 | 0 | PASS |
| integration-packages | 78 | 0 | 0 | PASS |
| integration-full | 14 | 0 | 0 | PASS |

All attempts use strategy remote, attempt1, starts within the recorded phase window, and terminal PASS. All 970 raw artifacts match their SHA256/CAS digests; 485 normalized logs match documented0x16 marker removal. Native evidence is separate and does not replace or stitch the remote phases. Counts are top-level result lines summed per job, not distinct test identities.

```json
{
  "test_exec_mode": "remote-exec",
  "fallback_reason": null,
  "exit_code": 0,
  "test_cmd_scope": "full-suite",
  "evidence_dir": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote",
  "admission": {
    "class": "remote",
    "floor_gib": 25.0,
    "free_gib": 57.169559478759766,
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
      "results": 369,
      "remote_executed": 369,
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
  "farm_test_seconds": 7553.831,
  "criterion3_valid": true,
  "owned_topup": [],
  "floor_topup": {
    "complete": true,
    "steps": [
      {
        "package": "cmd/gc",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/floor-logs/floor-0.log"
      },
      {
        "package": "examples/gastown",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/floor-logs/floor-1.log"
      },
      {
        "package": "internal/beads",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/floor-logs/floor-2.log"
      },
      {
        "package": "internal/beads/beadstest",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/floor-logs/floor-3.log"
      },
      {
        "package": "internal/beads/contract",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/floor-logs/floor-4.log"
      },
      {
        "package": "internal/beads/exec",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/floor-logs/floor-5.log"
      },
      {
        "package": "internal/doctor",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/floor-logs/floor-6.log"
      },
      {
        "package": "internal/mail/exec",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/floor-logs/floor-7.log"
      },
      {
        "package": "internal/runtime/herdr",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/floor-logs/floor-8.log"
      },
      {
        "package": "internal/runtime/runtimetest",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/floor-logs/floor-9.log"
      },
      {
        "package": "internal/runtime/tmux",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/floor-logs/floor-10.log"
      },
      {
        "package": "test/acceptance/helpers",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/floor-logs/floor-11.log"
      },
      {
        "package": "test/integration",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/floor-logs/floor-12.log"
      },
      {
        "check": "fsys-darwin-compile",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/floor-logs/floor-13.log"
      },
      {
        "check": "push-gate-lock-selftest",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/floor-logs/floor-14.log"
      },
      {
        "check": "local-concurrency-selftest",
        "exit_code": 0,
        "log": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/floor-logs/floor-15.log"
      }
    ],
    "full_record": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/floor-topup.json"
  },
  "skip_inventory": {
    "file": "/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/skip-inventory.json",
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
    "test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"
  ],
  "test_counts": {
    "PASS": 113364,
    "FAIL": 0,
    "SKIP": 340
  },
  "diff_tests_executed": "diff_tests_executed:\n  TestDoltStateFsyncProbeErrorExitsNonZeroWithMessage PASS (integration-packages-074fc6c34a1fb2577127.log, unit-088cc38d26d094729229.log)\n  TestDoltStateFsyncProbeFinishedPrintsStatusAndMeasuredMS PASS (integration-packages-821a75e4a503601af052.log, unit-f93a0ca96ccc61d0cae9.log)\n  TestDoltStateFsyncProbeIsHidden PASS (integration-packages-509ef0a6cd81866833cc.log, unit-23e7d9fc763496dd8095.log)\n  TestDoltStateFsyncProbeRealChain PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-10cc7795c0ec199a5530.log)\n  TestDoltStateFsyncProbeRejectsNonPositiveTimeout PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-f679cd2551fcc64525aa.log)\n  TestDoltStateFsyncProbeRequiresDirAndTimeout PASS (integration-packages-75af79934639abaae69c.log, unit-cd6930bb0d0da8646500.log)\n  TestDoltStateFsyncProbeUnfinishedExitsZeroWithBound PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-912ab643330b35bd0b6b.log)\n  TestDoltStateFsyncProbeUsesTheRealProbe PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-5a9df316125cd0aeff2e.log)\n  TestFsyncStallErrorThresholdFromEnv PASS (integration-packages-830685c60d0c2dd88567.log, unit-27b23e708b477fce4dad.log)\n  TestGcBeadsBdRecoverENOSPCWinsOverFsyncStall PASS (integration-packages-b72a67a2f9444af705e3.log, unit-637c74c578a2b17fc654.log)\n  TestGcBeadsBdRecoverFsyncGuardFailsOpenWhenGCBinUnset PASS (integration-packages-bc482d2ae768251ad47f.log, unit-493754ea2d5b4d8858c8.log)\n  TestGcBeadsBdRecoverFsyncGuardFailsOpenWithOneStderrLine PASS (integration-packages-cb110414c557c0928e82.log, unit-64014bc0bb3833bc198c.log)\n  TestGcBeadsBdRecoverFsyncWarnThresholdFromEnv PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-3d6d3036a2f5902472b5.log)\n  TestGcBeadsBdRecoverNeedsNoProbeWhenDoltNotListening PASS (integration-packages-18745ddf7d62465654f5.log, unit-0e1499ad8e8a0a6b9d81.log)\n  TestGcBeadsBdRecoverProceedsWhenFsyncFast PASS (integration-packages-75f7330b9b673df65e4f.log, unit-2ef13e1b6898c5544764.log)\n  TestGcBeadsBdRecoverSkipsWhenFsyncFinishedAtOrAboveWarn PASS (integration-packages-568812e4d525bdd1f974.log, unit-4d20b3d28497f844aa39.log)\n  TestGcBeadsBdRecoverSkipsWhenFsyncProbeUnfinished PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-2bde55fab377f50b4739.log)\n  TestMaterializedBdPackCarriesFsyncStallHelper PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-423c6a0da1986b90e9a7.log)\n  TestDotfileProbeFileIsInvisibleToDataDirListings PASS (integration-packages-0708b0d0c2f548d0a722.log, unit-da81bca7c80446c5af28.log)\n  TestRestartENOSPCRefusalStillWinsOverFsyncStall PASS (integration-packages-17007f3ad20cb33b8d6a.log, unit-ff6413fa61a58d5e02ec.log)\n  TestRestartFsyncGuardFailsOpenWhenGCBinUnset PASS (integration-packages-2c974992a3e988636c94.log, unit-89d44f035a0dc6a9b6c9.log)\n  TestRestartFsyncGuardFailsOpenWithNote PASS (integration-packages-130aa78402132a9cb052.log, unit-9d67ff2f074bc8f49779.log)\n  TestRestartProceedsWhenFsyncFast PASS (integration-packages-aa557e5bc0c7045b52d7.log, unit-98fce3c243b1e6971368.log)\n  TestRestartRefusesFsyncStallUnlessForced PASS (integration-packages-a9e12e81511731ef69dd.log, unit-6b47d0615f42f4ae2b8d.log)\n  TestSumJournalFilesIgnoresFsyncProbeDotfile PASS (unit-7bce503b7c011c7475f3.log)\n  TestMeasureErrorCases PASS (unit-d50e950eb1e22cc863ea.log)\n  TestMeasureFinishedReportsMeasuredTimeAndRemovesFile PASS (unit-d50e950eb1e22cc863ea.log)\n  TestProbeBoundHoldsForCaller PASS (unit-d50e950eb1e22cc863ea.log)\n  TestProbeFinished PASS (unit-d50e950eb1e22cc863ea.log)\n  TestProbeMissingDirIsErrorWithoutSpawning PASS (unit-d50e950eb1e22cc863ea.log)\n  TestProbeRejectsUnknownWorkerStatus PASS (unit-d50e950eb1e22cc863ea.log)\n  TestProbeSpawnFailureIsError PASS (unit-d50e950eb1e22cc863ea.log)\n  TestProbeWorkerThatExitsWithoutAnswerIsError PASS (unit-d50e950eb1e22cc863ea.log)\n  TestRunWorkerWritesJSONResult PASS (unit-d50e950eb1e22cc863ea.log)\n  TestWorkerCommandIsDetached PASS (unit-d50e950eb1e22cc863ea.log)\nGATE_TEST_EVIDENCE_RESULT ok: 35 diff-owned tests verified by name\n"
}
```

The wrapper reports PASS-THROUGH for this materialized scratch; no isolated-clone claim is made. Bazel test actions use the pinned worker environment. bd v1.3.1/refc1c4b642a is verified. Podman socket and cached dolt-sql-server:2.2.0 were checked; setup40GiB disk floor passed at57.1GiB free. Remote admission uses25GiB/cap2, while native checks retain their local load wait.

skip_justification: Every owned test passes. All 251 resolved remote skip sites are UNCHANGED, no shared hunks affect their conditions, and none is unresolved. One inventory row had an empty reason due to interleaved concurrent output; its exact raw-log reason and unchanged call_budget_test.go:536 site are recorded in skip-supplemental-proof.json. The raw inventory remains unchanged. The native floor attempted all environment-absence cases. Remaining non-owned tooling/fixture/platform/lane skips are inventoried without counting them as PASS. Runtime candidate failures: none.

test_cmd_scope: full-suite
test_exec_mode: remote-exec
heavy_mode: none
ci_lane_run: n/a (no CI-config change)
waiver_ref: none (gascity has no waiver path)
test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/logs

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/logs logs=501 logs_with_results=435
test_counts: PASS=113364 FAIL=0 SKIP=340
  (top-level result lines, column 0, summed over the 501 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
diff_tests_executed:
  TestDoltStateFsyncProbeErrorExitsNonZeroWithMessage PASS (integration-packages-074fc6c34a1fb2577127.log, unit-088cc38d26d094729229.log)
  TestDoltStateFsyncProbeFinishedPrintsStatusAndMeasuredMS PASS (integration-packages-821a75e4a503601af052.log, unit-f93a0ca96ccc61d0cae9.log)
  TestDoltStateFsyncProbeIsHidden PASS (integration-packages-509ef0a6cd81866833cc.log, unit-23e7d9fc763496dd8095.log)
  TestDoltStateFsyncProbeRealChain PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-10cc7795c0ec199a5530.log)
  TestDoltStateFsyncProbeRejectsNonPositiveTimeout PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-f679cd2551fcc64525aa.log)
  TestDoltStateFsyncProbeRequiresDirAndTimeout PASS (integration-packages-75af79934639abaae69c.log, unit-cd6930bb0d0da8646500.log)
  TestDoltStateFsyncProbeUnfinishedExitsZeroWithBound PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-912ab643330b35bd0b6b.log)
  TestDoltStateFsyncProbeUsesTheRealProbe PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-5a9df316125cd0aeff2e.log)
  TestFsyncStallErrorThresholdFromEnv PASS (integration-packages-830685c60d0c2dd88567.log, unit-27b23e708b477fce4dad.log)
  TestGcBeadsBdRecoverENOSPCWinsOverFsyncStall PASS (integration-packages-b72a67a2f9444af705e3.log, unit-637c74c578a2b17fc654.log)
  TestGcBeadsBdRecoverFsyncGuardFailsOpenWhenGCBinUnset PASS (integration-packages-bc482d2ae768251ad47f.log, unit-493754ea2d5b4d8858c8.log)
  TestGcBeadsBdRecoverFsyncGuardFailsOpenWithOneStderrLine PASS (integration-packages-cb110414c557c0928e82.log, unit-64014bc0bb3833bc198c.log)
  TestGcBeadsBdRecoverFsyncWarnThresholdFromEnv PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-3d6d3036a2f5902472b5.log)
  TestGcBeadsBdRecoverNeedsNoProbeWhenDoltNotListening PASS (integration-packages-18745ddf7d62465654f5.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestGcBeadsBdRecoverProceedsWhenFsyncFast PASS (integration-packages-75f7330b9b673df65e4f.log, unit-2ef13e1b6898c5544764.log)
  TestGcBeadsBdRecoverSkipsWhenFsyncFinishedAtOrAboveWarn PASS (integration-packages-568812e4d525bdd1f974.log, unit-4d20b3d28497f844aa39.log)
  TestGcBeadsBdRecoverSkipsWhenFsyncProbeUnfinished PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-2bde55fab377f50b4739.log)
  TestMaterializedBdPackCarriesFsyncStallHelper PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-423c6a0da1986b90e9a7.log)
  TestDotfileProbeFileIsInvisibleToDataDirListings PASS (integration-packages-0708b0d0c2f548d0a722.log, unit-da81bca7c80446c5af28.log)
  TestRestartENOSPCRefusalStillWinsOverFsyncStall PASS (integration-packages-17007f3ad20cb33b8d6a.log, unit-ff6413fa61a58d5e02ec.log)
  TestRestartFsyncGuardFailsOpenWhenGCBinUnset PASS (integration-packages-2c974992a3e988636c94.log, unit-89d44f035a0dc6a9b6c9.log)
  TestRestartFsyncGuardFailsOpenWithNote PASS (integration-packages-130aa78402132a9cb052.log, unit-9d67ff2f074bc8f49779.log)
  TestRestartProceedsWhenFsyncFast PASS (integration-packages-aa557e5bc0c7045b52d7.log, unit-98fce3c243b1e6971368.log)
  TestRestartRefusesFsyncStallUnlessForced PASS (integration-packages-a9e12e81511731ef69dd.log, unit-6b47d0615f42f4ae2b8d.log)
  TestSumJournalFilesIgnoresFsyncProbeDotfile PASS (unit-7bce503b7c011c7475f3.log)
  TestMeasureErrorCases PASS (unit-d50e950eb1e22cc863ea.log)
  TestMeasureFinishedReportsMeasuredTimeAndRemovesFile PASS (unit-d50e950eb1e22cc863ea.log)
  TestProbeBoundHoldsForCaller PASS (unit-d50e950eb1e22cc863ea.log)
  TestProbeFinished PASS (unit-d50e950eb1e22cc863ea.log)
  TestProbeMissingDirIsErrorWithoutSpawning PASS (unit-d50e950eb1e22cc863ea.log)
  TestProbeRejectsUnknownWorkerStatus PASS (unit-d50e950eb1e22cc863ea.log)
  TestProbeSpawnFailureIsError PASS (unit-d50e950eb1e22cc863ea.log)
  TestProbeWorkerThatExitsWithoutAnswerIsError PASS (unit-d50e950eb1e22cc863ea.log)
  TestRunWorkerWritesJSONResult PASS (unit-d50e950eb1e22cc863ea.log)
  TestWorkerCommandIsDetached PASS (unit-d50e950eb1e22cc863ea.log)
GATE_TEST_EVIDENCE_RESULT ok: 35 diff-owned tests verified by name
```

Load observations:

```text
/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=16.14 run_max_load=18.02 run_mean_load=13.72 run_readings=29 wait_first_load=16.14 wait_max_load=16.14 wait_mean_load=16.14 wait_readings=1 read_errors=0
/var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=11.48 run_max_load=12.17 run_mean_load=12.00 run_readings=6 wait_first_load=11.48 wait_max_load=11.48 wait_mean_load=11.48 wait_readings=1 read_errors=0
```

The PR records that provider-owned recovery and a running-but-not-listening server are follow-up work. The doctor/error-threshold/troubleshooting follow-up ga-dnqmzh should ship immediately after this change because the refusal currently points to its absent page. OPEN landing guard ga-3zonqfe protects both downstream beads until actual main landing, not this deploy close. No production gc build/deployment or upstream comment/issue is requested.

Retained evidence: /var/tmp/deploy-ga-rscqoq6.k6Lv4gXu and /var/tmp/gc-heavy-gate/runs/ga-rscqoq6.c3.k6Lv4gXu/evidence. Original review notes, exact RED/GREEN excerpts, caller audit, complexity proof, inventories and artifact hashes remain available.
