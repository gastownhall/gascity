**Verdict:** **PASS**

# Release gate: live-contract owned fixtures

Evaluated 2026-10-14T22:36:13.402449+00:00 for ga-ri9t3qk.
Review: ga-c95r5wm. Build lineage: ga-wvb825v and ga-oiduefx.
DEPLOY_MODE: remote. PUSH_REMOTE: fork. BASE_REF: origin/main.
Reviewed deploy source: `66e7ae23b877241f33118ae8b80ce2346a9d3651`.
gate_base: `9ef8fc783ee56d6fc3369d865da286a67b34105c`.
Merge-validated commit: `2377b3938c29086dbdb5027bff4764cb0486dfaa`; scratch: `/var/tmp/gc-merge-validate.DAo6EZ`.
Release policy: the deployer release-gate criteria and this repository's TESTING.md/Bazel CI lanes.
`docs/PROJECT_MANIFEST.md` is absent in this checkout.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | PASS — ga-c95r5wm records verdict pass at the resolved source above, with no blocker, major, or minor findings. |
| 2 | Acceptance criteria met | PASS — the order fixture invokes real order dispatch and reads its tracking ID through history; the forged generic API POST returns 422 at body.labels[0]; mail uses the permitted human sender. The owned test and all 45 read-sweep subtests pass in the independent full integration run. |
| 3 | Tests pass | PASS — complete fresh unit/check, acceptance, integration-package, full integration, and native-floor coverage; 109944 PASS, 0 FAIL, 335 SKIP top-level result lines. The diff-owned test passes by name with zero owned skips or failures. Runtime result cache and retries are disabled. |
| 4 | No high-severity review findings open | PASS — unresolved high/blocker/major findings: 0; the review records only two non-blocking test-fixture nits. |
| 5 | Final branch is clean | PASS — reviewed feature checkout and merge scratch checked clean; this gate record is the sole deploy addition. Prior untracked gate drafts are preserved outside the deploy checkout. |
| 6 | Branch diverges cleanly from main | PASS — materialize_merge_tree produced clean merges at the original gate base and at latest main `2d2b85656cd4e6e67ee9884b340d97ff2b19bb63`; whole-tree Bazel build/nogo and tagged integration vet passed in both. Additional runtime and owned live-contract checks passed on the newer merge. No self-rebase was needed. |
| 7 | Single feature theme | PASS — four reviewed commits modify only test/integration/gc_live_contract_test.go (+169/-53), repairing fixtures and read-sweep coverage in the same live API contract. Ancestry guard passed with ga-ri9t3qk, ga-wvb825v, and ga-oiduefx; no .claude paths or stack. |

## Acceptance and coverage

The real exec order creates exactly one history row with a non-empty tracking ID. Output is attached through the operator bead CLI and history detail returns it. The generic HTTP bead API continues to refuse a forged owned order-run row. Extmsg group/binding fixtures and scoped order history remain present for the read sweep and are cleaned up in the test's isolated city.

`TestGCLiveContract_BeadsAndEvents` passed in the full integration phase (122.89 seconds). Its 45 sweep subtests passed, including orders/history and extmsg/transcript, extmsg/bindings, extmsg/groups, and extmsg/adapters; none skipped or failed. The separately tracked maintenance/status gap is ga-gc85sm2, created before review, linked discovered-from ga-wvb825v, and cited on that build bead. Its named exclusion preserves the pre-existing coverage gap; it is outside this gate's accepted scope.

## Static and generated-file checks

policy_lane: PASS — fresh-tree-run.sh → gate-base.sh run → gofmt check, make lint (Bazel nogo over //...), and run-pinned-lint.sh for integration-tagged fmt/run with the pinned gate base. Golangci-lint reported 0 issues, pin/resolved 2.12.0, mismatch false.

```text
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.DAo6EZ head=2377b3938c29 tree=490dde8d6bc6 dir=/var/tmp/gc-fresh-tree.SmOWVq modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.DAo6EZ,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=9ef8fc783ee56d6fc3369d865da286a67b34105c knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.SmOWVq
GLT_RECORD: {"golangci_lint":{"pin":"2.12.0","resolved":"2.12.0","mismatch":false}}
```

drift_lane: PASS — CI's make bazel-sync followed by git diff --exit-code in a separate fresh checkout; no generated drift.

```text
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.DAo6EZ head=2377b3938c29 tree=490dde8d6bc6 dir=/var/tmp/gc-fresh-tree.RxWa5S modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.DAo6EZ,bazel-out,bazel-testlogs
```

ci_lane_run: n/a — no CI-config change. heavy_mode: none.
waiver_ref: none. failure_attribution: n/a — zero failures.

## Current-main freshness

Main advanced during the native-floor load wait with an independent Codex trust-dialog fix in internal/runtime/dialog.go and three runtime test files; it does not overlap the reviewed integration file. The original full-suite and native-floor evidence remains pinned to the gate base above. An additional clean merge with the newer main was built/vetted, and its complete runtime package and the edited live-contract test were run fresh remotely. These focused checks supplement the completed full suite and do not replace or relabel it.

```json
{
  "result": "PASS",
  "test_cmd_scope": "focused (additive; full-suite evidence unchanged)",
  "base": "2d2b85656cd4e6e67ee9884b340d97ff2b19bb63",
  "merge": "b794a3a3bc4b5dfb222c3e942814c9e45dd8f537",
  "scratch": "/var/tmp/gc-merge-validate.sVoF40",
  "build_vet": "bazel build --config=remote-exec --noremote_local_fallback //...; go vet -tags integration ./test/integration/",
  "runtime_cmd": "bazel test --config=remote-exec --config=ci --nocache_test_results --noremote_local_fallback //internal/runtime:runtime_test",
  "owned_cmd": "bazel test --config=remote-exec --config=ci --config=integration --nocache_test_results --noremote_local_fallback --test_filter=^(TestGCLiveContract_BeadsAndEvents)$ //test/integration:integration_test",
  "checks": {
    "runtime": {
      "result": "PASS",
      "executions": 1,
      "strategy": "remote",
      "cached": 0,
      "retries": 0,
      "window_ok": true,
      "bep": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/freshness-runtime.bep.json"
    },
    "owned": {
      "result": "PASS",
      "executions": 14,
      "strategy": "remote",
      "cached": 0,
      "retries": 0,
      "window_ok": true,
      "bep": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/freshness-owned.bep.json"
    }
  },
  "by_name": "GATE_TEST_EVIDENCE logs_dir=/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/freshness-logs logs=2 logs_with_results=2\ntest_counts: PASS=350 FAIL=0 SKIP=0\n  (top-level result lines, column 0, summed over the 2 logs; a test that several jobs run counts once per job; sub-tests are not counted)\ndiff_tests_executed:\n  TestGCLiveContract_BeadsAndEvents PASS (owned.log)\nGATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name\n"
}
```

## Runtime evidence

test_cmd_scope: full-suite. Runtime tests use --nocache_test_results and --noremote_local_fallback; all four phase BEP predicates require remote strategy, attempt 1, an in-window start, uncached results, and terminal verdicts. Native checks supplement the farm coverage; they do not replace a failed farm run.

test_log_dir: `/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs`.
Native floor logs: `/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs`.
Full execution record: `/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/execution.json`.
Full skip inventory: `/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/skip-inventory.json`.

skip_justification: non-owned skips are inventoried by name and reason. Environment-absence skips receive computed native package top-ups. Remaining skips are explicit exclusions or missing optional fixtures in unchanged tests, including unsupported usage extractors, tracked conformance exclusions, helper-process entry points, optional provider/database configuration, and packaged assets. The sole changed file contains one test, and that test has no skip; its changed fixture/sweep helpers are used only there and cannot reach these other skip sites. Native top-up skips remain recorded rather than counted as passes; every floor command exits 0.

The required native floor includes beadstest, Darwin compile, runner self-tests, and every environment-absence skip selected by package (parent selected for a subtest). Its computed record is complete. The farm's initial host-load wait is inapplicable under remote admission (25 GiB floor, cap 2); the native floor retains the configured local soft load wait, measured against the five-minute load average. Earlier one-minute progress snapshots are not gate-load evidence; the runner's exact summary below is authoritative.

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=18.06 run_max_load=22.63 run_mean_load=19.61 run_readings=8 wait_first_load=34.97 wait_max_load=34.97 wait_mean_load=24.76 wait_readings=61 read_errors=0
```

```json
{
  "test_exec_mode": "remote-exec",
  "fallback_reason": null,
  "exit_code": 0,
  "test_cmd_scope": "full-suite",
  "evidence_dir": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote",
  "admission": {
    "class": "remote",
    "floor_gib": 25.0,
    "free_gib": 72.47290420532227,
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
  "farm_test_seconds": 8894.289,
  "criterion3_valid": true,
  "owned_topup": [],
  "floor_topup": {
    "complete": true,
    "steps": [
      {
        "package": "cmd/gc",
        "tests": [
          "TestReplay",
          "TestSessionEventPumpLiveHerdr",
          "TestSimCoverageGate",
          "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
          "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace"
        ],
        "skip_reasons": [
          {
            "package": "cmd/gc",
            "test": "TestReplay",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-e4245ddf6a37030111bc.log",
            "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestReplay",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-64014bc0bb3833bc198c.log",
            "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSessionEventPumpLiveHerdr",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-12697e2ea0d32c278115.log",
            "reason": "session_event_pump_live_test.go:23: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSessionEventPumpLiveHerdr",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-2bde55fab377f50b4739.log",
            "reason": "session_event_pump_live_test.go:23: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSimCoverageGate",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-9326181a736eb30b71ca.log",
            "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSimCoverageGate",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4d20b3d28497f844aa39.log",
            "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-074fc6c34a1fb2577127.log",
            "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-0e1499ad8e8a0a6b9d81.log",
            "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-75af79934639abaae69c.log",
            "reason": "tmux_leak_guard_mntns_test.go:98: bwrap not installed; cannot build a private-/tmp mount namespace",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-3d6d3036a2f5902472b5.log",
            "reason": "tmux_leak_guard_mntns_test.go:98: bwrap not installed; cannot build a private-/tmp mount namespace",
            "class": "environment-absence"
          }
        ],
        "command": [
          "go",
          "test",
          "-tags",
          "integration",
          "-count=1",
          "-v",
          "-run",
          "^(TestReplay|TestSessionEventPumpLiveHerdr|TestSimCoverageGate|TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace|TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace)$",
          "./cmd/gc"
        ],
        "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-0.log",
        "exit_code": 0
      },
      {
        "package": "examples/gastown",
        "tests": [
          "TestAttachedRigScopeShellToken",
          "TestJsonlExportPushRetryDelayIsLocaleIndependent"
        ],
        "skip_reasons": [
          {
            "package": "examples/gastown",
            "test": "TestAttachedRigScopeShellToken/zsh",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-be91eec86b4dbf7e66f1.log",
            "reason": "gastown_test.go:4110: zsh not installed",
            "class": "environment-absence"
          },
          {
            "package": "examples/gastown",
            "test": "TestAttachedRigScopeShellToken/zsh",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-ef24b9dbe59f5d04fac1.log",
            "reason": "gastown_test.go:4110: zsh not installed",
            "class": "environment-absence"
          },
          {
            "package": "examples/gastown",
            "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-ff9cb121652191f033f5.log",
            "reason": "maintenance_scripts_test.go:9240: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
            "class": "environment-absence"
          },
          {
            "package": "examples/gastown",
            "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-29937721dc700c4b8d8b.log",
            "reason": "maintenance_scripts_test.go:9240: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
            "class": "environment-absence"
          }
        ],
        "command": [
          "go",
          "test",
          "-tags",
          "integration",
          "-count=1",
          "-v",
          "-run",
          "^(TestAttachedRigScopeShellToken|TestJsonlExportPushRetryDelayIsLocaleIndependent)$",
          "./examples/gastown"
        ],
        "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-1.log",
        "exit_code": 0
      },
      {
        "package": "internal/beads",
        "tests": [
          "TestMetadataEmptyStringClearContract",
          "TestProbeVersionsAgainstHostBinaries"
        ],
        "skip_reasons": [
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/BatchClearsOneKeyKeepsAnother",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-999a89ecc5b3d09abdd4.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/BatchClearsOneKeyKeepsAnother",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-9a6a09bab414814cab53.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-999a89ecc5b3d09abdd4.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-9a6a09bab414814cab53.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-999a89ecc5b3d09abdd4.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-9a6a09bab414814cab53.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-999a89ecc5b3d09abdd4.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-9a6a09bab414814cab53.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestProbeVersionsAgainstHostBinaries/bd",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-363a36f2662d541bc75e.log",
            "reason": "binary_versions_test.go:62: bd not probeable in this environment: locate bd: exec: \"bd\": executable file not found in $PATH",
            "class": "environment-absence"
          }
        ],
        "command": [
          "go",
          "test",
          "-tags",
          "integration",
          "-count=1",
          "-v",
          "-run",
          "^(TestMetadataEmptyStringClearContract|TestProbeVersionsAgainstHostBinaries)$",
          "./internal/beads"
        ],
        "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-2.log",
        "exit_code": 0
      },
      {
        "package": "internal/beads/beadstest",
        "tests": [
          "TestPinnedBeadsModuleDirResolvesThisBuildsOwnCache"
        ],
        "skip_reasons": [],
        "command": [
          "go",
          "test",
          "-tags",
          "integration",
          "-count=1",
          "-v",
          "-run",
          "^(TestPinnedBeadsModuleDirResolvesThisBuildsOwnCache)$",
          "./internal/beads/beadstest"
        ],
        "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-3.log",
        "exit_code": 0
      },
      {
        "package": "internal/beads/contract",
        "tests": [
          "TestMigrateJournalFileMatchesPinnedBeads"
        ],
        "skip_reasons": [
          {
            "package": "internal/beads/contract",
            "test": "TestMigrateJournalFileMatchesPinnedBeads",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-8544388e16a0d6b0cdb0.log",
            "reason": "migrate_journal_test.go:37: pinned beads v1.3.1 is not unpacked in the local module cache: open /tmp/bt/_tmp/35731ca362c311940500020a6c8a2007/go/pkg/mod/github.com/steveyegge/beads@v1.3.1/cmd/bd/migrate_dolt_mode.go: no such file or directory",
            "class": "environment-absence"
          }
        ],
        "command": [
          "go",
          "test",
          "-tags",
          "integration",
          "-count=1",
          "-v",
          "-run",
          "^(TestMigrateJournalFileMatchesPinnedBeads)$",
          "./internal/beads/contract"
        ],
        "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-4.log",
        "exit_code": 0
      },
      {
        "package": "internal/beads/exec",
        "tests": [
          "TestBrProviderConformance"
        ],
        "skip_reasons": [
          {
            "package": "internal/beads/exec",
            "test": "TestBrProviderConformance",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-64046beff95fb28ccd66.log",
            "reason": "br_test.go:18: br not available",
            "class": "environment-absence"
          }
        ],
        "command": [
          "go",
          "test",
          "-tags",
          "integration",
          "-count=1",
          "-v",
          "-run",
          "^(TestBrProviderConformance)$",
          "./internal/beads/exec"
        ],
        "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-5.log",
        "exit_code": 0
      },
      {
        "package": "internal/doctor",
        "tests": [
          "TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint",
          "TestCustomTypesCheck_TableDrift",
          "TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext"
        ],
        "skip_reasons": [
          {
            "package": "internal/doctor",
            "test": "TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
            "reason": "checks_custom_types_test.go:467: bd binary not on PATH",
            "class": "environment-absence"
          },
          {
            "package": "internal/doctor",
            "test": "TestCustomTypesCheck_TableDrift",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
            "reason": "checks_custom_types_test.go:297: bd binary not on PATH",
            "class": "environment-absence"
          },
          {
            "package": "internal/doctor",
            "test": "TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
            "reason": "checks_custom_types_test.go:399: bd binary not on PATH",
            "class": "environment-absence"
          }
        ],
        "command": [
          "go",
          "test",
          "-tags",
          "integration",
          "-count=1",
          "-v",
          "-run",
          "^(TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint|TestCustomTypesCheck_TableDrift|TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext)$",
          "./internal/doctor"
        ],
        "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-6.log",
        "exit_code": 0
      },
      {
        "package": "internal/mail/exec",
        "tests": [
          "TestMCPMailBridgeSourceable",
          "TestMCPMailConformance",
          "TestMCPMailCrossPodNameResolution",
          "TestMCPMailProjectKeyIsolation",
          "TestMCPMailReplyMissingOriginalIsNotFound"
        ],
        "skip_reasons": [
          {
            "package": "internal/mail/exec",
            "test": "TestMCPMailBridgeSourceable",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
            "reason": "mcp_conformance_test.go:159: MCP mail script not found: file does not exist",
            "class": "environment-absence"
          },
          {
            "package": "internal/mail/exec",
            "test": "TestMCPMailConformance",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
            "reason": "mcp_conformance_test.go:26: MCP mail script not found: file does not exist",
            "class": "environment-absence"
          },
          {
            "package": "internal/mail/exec",
            "test": "TestMCPMailCrossPodNameResolution",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
            "reason": "mcp_conformance_test.go:195: MCP mail script not found: file does not exist",
            "class": "environment-absence"
          },
          {
            "package": "internal/mail/exec",
            "test": "TestMCPMailProjectKeyIsolation",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
            "reason": "mcp_conformance_test.go:286: MCP mail script not found: file does not exist",
            "class": "environment-absence"
          },
          {
            "package": "internal/mail/exec",
            "test": "TestMCPMailReplyMissingOriginalIsNotFound",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
            "reason": "mcp_conformance_test.go:80: MCP mail script not found: file does not exist",
            "class": "environment-absence"
          }
        ],
        "command": [
          "go",
          "test",
          "-tags",
          "integration",
          "-count=1",
          "-v",
          "-run",
          "^(TestMCPMailBridgeSourceable|TestMCPMailConformance|TestMCPMailCrossPodNameResolution|TestMCPMailProjectKeyIsolation|TestMCPMailReplyMissingOriginalIsNotFound)$",
          "./internal/mail/exec"
        ],
        "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-7.log",
        "exit_code": 0
      },
      {
        "package": "internal/runtime/herdr",
        "tests": [
          "TestActivityLive",
          "TestHerdrConformance",
          "TestProviderLive",
          "TestProviderLiveClaudeKindPath",
          "TestProviderLiveOccupantSwapKeepsLiveness",
          "TestSessionEventsLive"
        ],
        "skip_reasons": [
          {
            "package": "internal/runtime/herdr",
            "test": "TestActivityLive",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
            "reason": "activity_live_test.go:20: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestActivityLive",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
            "reason": "activity_live_test.go:20: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestHerdrConformance",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
            "reason": "conformance_test.go:18: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestHerdrConformance",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
            "reason": "conformance_test.go:18: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestProviderLive",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
            "reason": "provider_live_test.go:15: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestProviderLive",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
            "reason": "provider_live_test.go:15: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestProviderLiveClaudeKindPath",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
            "reason": "kindpath_live_test.go:105: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestProviderLiveClaudeKindPath",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
            "reason": "kindpath_live_test.go:105: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestProviderLiveOccupantSwapKeepsLiveness",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
            "reason": "panebinding_live_test.go:21: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestProviderLiveOccupantSwapKeepsLiveness",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
            "reason": "panebinding_live_test.go:21: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestSessionEventsLive",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
            "reason": "events_live_test.go:40: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestSessionEventsLive",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
            "reason": "events_live_test.go:40: herdr not installed",
            "class": "environment-absence"
          }
        ],
        "command": [
          "go",
          "test",
          "-tags",
          "integration",
          "-count=1",
          "-v",
          "-run",
          "^(TestActivityLive|TestHerdrConformance|TestProviderLive|TestProviderLiveClaudeKindPath|TestProviderLiveOccupantSwapKeepsLiveness|TestSessionEventsLive)$",
          "./internal/runtime/herdr"
        ],
        "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-8.log",
        "exit_code": 0
      },
      {
        "package": "internal/runtime/runtimetest",
        "tests": [
          "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors"
        ],
        "skip_reasons": [
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Interrupt_ConcurrentDistinctSessions",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:236: Start(skip-start-14): provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/IsRunning_ConcurrentDistinctSessions",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:269: Start(skip-start-18): provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ConcurrentDistinctPrefixes",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:365: Start(skip-start-25): provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_EmptyPrefix",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:347: Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ExcludesStopped",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:331: Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_FindsSessions",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:292: Start skip-start-21: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_PrefixFiltering",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:311: Start skip-start-22: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_ConcurrentDistinctSessions",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:419: Start(skip-start-30): provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_EmptyNamesReturnsTrue",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:393: Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_FalseAfterStop",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:402: Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/SharedSession",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:59: Start shared session: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_ConcurrentDistinctSessions",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:122: concurrent Start(skip-start-3): provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_CreatesRunningSession",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:82: Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_DuplicateReturnsError",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:91: first Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_ConcurrentDistinctSessions",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:207: Start(skip-start-11): provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_AlreadyStopped",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:191: Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_NotRunning",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:168: Start sentinel: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_MakesSessionNotRunning",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:132: Start: provider environment unavailable",
            "class": "environment-absence"
          }
        ],
        "command": [
          "go",
          "test",
          "-tags",
          "integration",
          "-count=1",
          "-v",
          "-run",
          "^(TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors)$",
          "./internal/runtime/runtimetest"
        ],
        "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-9.log",
        "exit_code": 0
      },
      {
        "package": "internal/runtime/tmux",
        "tests": [
          "TestGetKeyBinding_CapturesUserBinding",
          "TestGetKeyBinding_SkipsGasTownBindings",
          "TestIsGTBinding_DetectsGasTownBindings",
          "TestSetBindings_PreserveFallbackOnRepeatedCalls"
        ],
        "skip_reasons": [
          {
            "package": "internal/runtime/tmux",
            "test": "TestGetKeyBinding_CapturesUserBinding",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
            "reason": "tmux_test.go:3663: not inside tmux \u2014 need server for bind-key",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/tmux",
            "test": "TestGetKeyBinding_SkipsGasTownBindings",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
            "reason": "tmux_test.go:3637: not inside tmux \u2014 need server for bind-key",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/tmux",
            "test": "TestIsGTBinding_DetectsGasTownBindings",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-15c091b746137e5c2921.log",
            "reason": "tmux_test.go:3689: not inside tmux \u2014 need server for bind-key",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/tmux",
            "test": "TestSetBindings_PreserveFallbackOnRepeatedCalls",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
            "reason": "tmux_test.go:3719: not inside tmux \u2014 need server for bind-key",
            "class": "environment-absence"
          }
        ],
        "command": [
          "go",
          "test",
          "-tags",
          "integration",
          "-count=1",
          "-v",
          "-run",
          "^(TestGetKeyBinding_CapturesUserBinding|TestGetKeyBinding_SkipsGasTownBindings|TestIsGTBinding_DetectsGasTownBindings|TestSetBindings_PreserveFallbackOnRepeatedCalls)$",
          "./internal/runtime/tmux"
        ],
        "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-10.log",
        "exit_code": 0
      },
      {
        "package": "test/acceptance/helpers",
        "tests": [
          "TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig",
          "TestBdLatestSchemaVersionLeavesNothingInItsTempRoot",
          "TestRealBdProvisionLeavesTheHostHomeAlone"
        ],
        "skip_reasons": [
          {
            "package": "test/acceptance/helpers",
            "test": "TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
            "reason": "bd_schema_parity_test.go:121: bd not available",
            "class": "environment-absence"
          },
          {
            "package": "test/acceptance/helpers",
            "test": "TestBdLatestSchemaVersionLeavesNothingInItsTempRoot",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
            "reason": "bd_schema_parity_test.go:144: bd not available",
            "class": "environment-absence"
          },
          {
            "package": "test/acceptance/helpers",
            "test": "TestRealBdProvisionLeavesTheHostHomeAlone",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
            "reason": "tool_home_test.go:315: no bd available (set GC_ACCEPTANCE_BD_BIN)",
            "class": "environment-absence"
          }
        ],
        "command": [
          "go",
          "test",
          "-tags",
          "integration",
          "-count=1",
          "-v",
          "-run",
          "^(TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig|TestBdLatestSchemaVersionLeavesNothingInItsTempRoot|TestRealBdProvisionLeavesTheHostHomeAlone)$",
          "./test/acceptance/helpers"
        ],
        "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-11.log",
        "exit_code": 0
      },
      {
        "package": "test/integration",
        "tests": [
          "TestStartDrift_SystemdManaged_RestartsToNewBuildID"
        ],
        "skip_reasons": [
          {
            "package": "test/integration",
            "test": "TestStartDrift_SystemdManaged_RestartsToNewBuildID",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/logs/integration-full-c02db3e138643d8a1b39.log",
            "reason": "start_drift_test.go:123: XDG_RUNTIME_DIR not set; user systemd not addressable",
            "class": "environment-absence"
          }
        ],
        "command": [
          "go",
          "test",
          "-tags",
          "integration",
          "-count=1",
          "-v",
          "-run",
          "^(TestStartDrift_SystemdManaged_RestartsToNewBuildID)$",
          "./test/integration"
        ],
        "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-12.log",
        "exit_code": 0
      },
      {
        "check": "fsys-darwin-compile",
        "tests": [],
        "skip_reasons": [],
        "command": [
          "make",
          "test-fsys-darwin-compile"
        ],
        "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-13.log",
        "exit_code": 0
      },
      {
        "check": "push-gate-lock-selftest",
        "tests": [],
        "skip_reasons": [],
        "command": [
          "bash",
          "scripts/test-push-gate-lock.sh"
        ],
        "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-14.log",
        "exit_code": 0
      },
      {
        "check": "local-concurrency-selftest",
        "tests": [],
        "skip_reasons": [],
        "command": [
          "bash",
          "scripts/test-local-concurrency.sh"
        ],
        "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-15.log",
        "exit_code": 0
      }
    ],
    "log_dir": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs"
  },
  "skip_inventory": {
    "counts_by_reason_class": {
      "other": 250,
      "environment-absence": 71
    },
    "top_level_counts_by_reason_class": {
      "other": 201,
      "environment-absence": 42
    },
    "full_inventory": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/skip-inventory.json"
  },
  "test_cmd": [
    "test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"
  ],
  "test_counts": {
    "PASS": 109944,
    "FAIL": 0,
    "SKIP": 335
  },
  "diff_tests_executed": "diff_tests_executed:\n  TestGCLiveContract_BeadsAndEvents PASS (integration-full-e914b9bbd3c7222e0ecf.log)\nGATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name\n",
  "native_skip_inventory": [
    {
      "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-0.log",
      "test": "TestReplay",
      "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set"
    },
    {
      "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-0.log",
      "test": "TestSimCoverageGate",
      "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset"
    },
    {
      "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-0.log",
      "test": "TestSessionEventPumpLiveHerdr",
      "reason": "session_event_pump_live_test.go:23: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`"
    },
    {
      "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-5.log",
      "test": "TestBrProviderConformance",
      "reason": "br_test.go:18: br not available"
    },
    {
      "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-8.log",
      "test": "TestActivityLive",
      "reason": "activity_live_test.go:20: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`"
    },
    {
      "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-8.log",
      "test": "TestHerdrConformance",
      "reason": "conformance_test.go:18: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`"
    },
    {
      "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-8.log",
      "test": "TestSessionEventsLive",
      "reason": "events_live_test.go:40: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`"
    },
    {
      "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-8.log",
      "test": "TestProviderLiveClaudeKindPath",
      "reason": "kindpath_live_test.go:105: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`"
    },
    {
      "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-8.log",
      "test": "TestProviderLiveOccupantSwapKeepsLiveness",
      "reason": "panebinding_live_test.go:21: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`"
    },
    {
      "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-8.log",
      "test": "TestProviderLive",
      "reason": "provider_live_test.go:15: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`"
    },
    {
      "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-10.log",
      "test": "TestGetKeyBinding_SkipsGasTownBindings",
      "reason": "tmux_test.go:3637: not inside tmux \u2014 need server for bind-key"
    },
    {
      "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-10.log",
      "test": "TestGetKeyBinding_CapturesUserBinding",
      "reason": "tmux_test.go:3663: not inside tmux \u2014 need server for bind-key"
    },
    {
      "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-10.log",
      "test": "TestIsGTBinding_DetectsGasTownBindings",
      "reason": "tmux_test.go:3689: not inside tmux \u2014 need server for bind-key"
    },
    {
      "log": "/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/floor-logs/floor-10.log",
      "test": "TestSetBindings_PreserveFallbackOnRepeatedCalls",
      "reason": "tmux_test.go:3719: not inside tmux \u2014 need server for bind-key"
    }
  ],
  "native_load": {
    "load_metric": "five-minute load average",
    "load_threshold": 15.0,
    "load_waited_seconds": 1801,
    "load_wait_timed_out": true,
    "run_start_load": "18.06",
    "run_max_load": "22.63",
    "run_mean_load": "19.61",
    "run_readings": "8",
    "wait_first_load": "34.97",
    "wait_max_load": "34.97",
    "wait_mean_load": "24.76",
    "wait_readings": "61",
    "read_errors": "0"
  }
}
```

## Retained operational evidence

Fast lanes: `/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/fast-run`; build/vet, policy, and drift logs live in `/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu`.
Criterion 3 detached service: `/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3`. The driver survives session recycling.
The first scratch materialization's cold local pre-commit build was canceled before any test verdict, with its client verified against the session identity; its output base was released. Scratch commit hooks were suppressed only when recreating the disposable merge tree. Assigned deploy worktree hooks stay active. The runner refused an unused alternate scratch as BUSY; no second gate ran.

The landing guard ga-6ilvcxw stays open until mayor/MPR verifies actual landing on main. Opening this PR and publishing clearance do not release the dependent deploys.

## Exact full-suite commands

These are the phase commands executed by gate-rbe-phase.sh in the pinned merge scratch. The phase flags above and retained BEPs bind each result to its invocation.

```bash
BAZEL_FLAGS="--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/unit.bep.json" NOGO_BUILD_FLAGS="--keep_going --output_groups=nogo_fix --config=remote-exec" make check

bazel test --config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/acceptance.bep.json --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests

bazel test --config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/integration-packages.bep.json --config=integration //test:integration_packages

bazel test --config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-ri9t3qk.8jCw5gwu/criterion3/evidence/remote/integration-full.bep.json --config=integration --test_arg=-test.v //test/integration:integration_test
```
