# OpenClaw bridge test inputs — ga-h1d3toc

**Verdict:** **PASS**

Evaluated by gascity/deployer; completed 2026-10-16T23:56:20.480702+00:00.
Deploy source: `e42e5ca4691d31c76e2c01018b7ccef4de50bc04`. Build bead: ga-r1fzxey; review: ga-4tmjtaf; tracker: ga-v1wkoqh.

`DEPLOY_MODE=remote`; `BASE_REF=origin/main`; isolated branch `deploy/ga-h1d3toc-gate`.
merge_state: clean (origin/main@3c4cf446b497)
gate_base_kind: current-tip
gate_base: 3c4cf446b497c52de11bf769ccbb9fb682d78122
merge_head: 150e45a355665ce4722633f73a68f277e03538a3
merge_validation: merge result built and vetted here; subsequent base movement is settled at merge time.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-4tmjtaf is closed with PASS for the resolved source SHA. Style and security have no findings; one optional style nit. No carryover or reviewer-added test commit. |
| 2 | Acceptance criteria met | PASS | Only contrib/openclaw-bridge/BUILD.bazel changes (+5/-4). The existing source glob and package.json remain for all four tests. Only openclaw-loader receives :node_modules; comment explains the current imports. Built manifests: entrypoints=0, gc_client=0, inbound=0, loader=814 node_modules entries. Four JavaScript targets pass freshly on the farm. Commit cites ga-r1fzxey and ga-v1wkoqh. |
| 3 | Full CI-equivalent tests pass | PASS | gate-criterion3.sh completed all four forced-fresh remote phases and all 16 required native floor jobs; criterion3_valid=true, exit_code=0. By-name totals: 115586 PASS, 0 FAIL, 369 SKIP, counted as top-level execution occurrences across 506 retained logs. Diff owns no test. |
| 4 | No unresolved high findings | PASS | Reviewer reports no security findings or required style fixes; unresolved HIGH count=0. |
| 5 | Final branch is clean | PASS | Isolated reviewed source was clean before adding this record. Deployment adds only this gate record; pre-existing scratch gate records were preserved outside the worktree. |
| 6 | Merge state and build/vet | PASS | Clean merge against the pinned base above. In the scratch: isolated-test-run.sh --rbe ran bazel build --config=remote-exec --noremote_local_fallback --keep_going //cmd/gc:gc, then make vet with nogo remote flags; both exit 0. |
| 7 | Single feature theme | PASS | One BUILD file narrows OpenClaw test inputs. The gate record is deployment evidence. No independent feature, source change, API change or internal configuration file is included. |

Criterion 3b: PASS. Fast lane completed before the suite: make lint (nogo), fresh remote //:go_format_test, and make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary. Generated drift: make bazel-sync with remote flags, then git diff --exit-code; clean. Each lane ran in a fresh view of the same committed merge.

Criterion 3c: n/a — no workflow, job, matrix, timeout or required-check list changes.

heavy_mode: none
failure_attribution: none
waiver_ref: none
review_carryover_verified: none
reviewer_tests_verified: none
diff_tests_executed: none (no diff-owned tests)
test_cmd_scope: full-suite

Required CI coverage: bazel.yml unit (`make check`, //...), acceptance (including solo targets), integration-packages, and full integration without a smoke filter (covers the integration-smoke lane); BUILD files in sync reproduced by make bazel-sync plus git diff --exit-code. Build/nogo may use cache; no test result in the four remote phases was cache-credited. All 490 test attempts used remote strategy, attempt 1, and start times inside the corresponding phase window.

skip_justification: All remaining skips are pre-existing and not diff-owned. The only changed file configures JavaScript runfiles; it changes no Go test body, helper, skip condition, production import or additional test load. The farm inventory classifies 71 skip occurrences as environment-absence and 284 as other (including subtests), with 42 and 206 top-level occurrences respectively. Every selected environment-absence test was revisited by its parent test/package on the native floor, alongside beadstest, Darwin compile and runner self-tests. All 16 jobs exited 0; remaining non-owned skips cover absent live herdr/br/Postgres/replay inputs, platform or provider capabilities, intentional helper-only tests and existing tracked exclusions. Exact names, reasons and top-up commands are retained in the execution record below. No skipped or failed test was credited as a real executed PASS.

load_threshold: 15
load_waited_seconds: 1801
load_wait_timed_out: 1
run_start_load: 57.79
run_max_load: 57.03
run_mean_load: 49.57
run_readings: 10
wait_first_load: 83.62
wait_max_load: 136.80
wait_mean_load: 104.82
wait_readings: 61
read_errors: 0

Those values are the native floor's load gate. The ordinary soft wait expired and then ran, as configured; all native jobs passed. The outer remote run did not require a load wait: waited_seconds=0, wait_timed_out=0, run_start_load=61.47, run_max_load=136.96, run_mean_load=87.78, run_readings=94. No heavy slot or local full-suite fallback was used.

Evidence directory: `/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence`.

test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs

All 980 retained test.log/test.xml artifacts were independently SHA256-checked against their manifests. Full execution.json SHA256: `f29ffa38b1171a52dc3f902e4659fde4d24e15d59435eb0d63d6c48e460dbb0b`. The complete record follows, including test_exec_mode, test_exec_bep, rbe_state, admission, fallback_reason, farm_test_seconds, skip_inventory, owned_topup and floor_topup.

Fast-lane isolation records:

```text
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.n6ygot head=150e45a35566 tree=801d35a6c5e6 dir=/var/tmp/gc-fresh-tree.y3FyW3 modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.n6ygot,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=3c4cf446b497c52de11bf769ccbb9fb682d78122 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.y3FyW3
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.n6ygot head=150e45a35566 tree=801d35a6c5e6 dir=/var/tmp/gc-fresh-tree.fRUpbN modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.n6ygot,bazel-out,bazel-testlogs
```

test_bd:

```text
isolated-test-run:   pinned test bd (gm-i3csn7): GASCITY_TEST_BD version=v1.3.1 bin=/home/jaword/.local/bd-versions/v1.3.1/bd reports='bd version 1.3.1 (c1c4b642a: HEAD@c1c4b642ac1c)' ref_check=match:c1c4b642a origin=preexisting pin_source=/var/tmp/gc-merge-validate.n6ygot/deps.env displaces='bd version 1.1.0 (0954be416)'
```

Actual phase commands:

```text
unit: make check
acceptance: bazel test $BAZEL_FLAGS --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests
integration-packages: bazel test $BAZEL_FLAGS --config=integration //test:integration_packages
integration-full: bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v //test/integration:integration_test
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

Complete execution record:

<details>
<summary>Criterion 3 execution.json</summary>

```json
{
  "test_exec_mode": "remote-exec",
  "fallback_reason": null,
  "exit_code": 0,
  "test_cmd_scope": "full-suite",
  "evidence_dir": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote",
  "admission": {
    "class": "remote",
    "floor_gib": 25.0,
    "free_gib": 84.21821594238281,
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
      "results": 371,
      "remote_executed": 371,
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
      "results": 81,
      "remote_executed": 81,
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
  "farm_test_seconds": 7677.245,
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
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-568812e4d525bdd1f974.log",
            "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestReplay",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-752a0bf25b15b465517a.log",
            "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSessionEventPumpLiveHerdr",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-c2ab4e7d4735e1bd66a7.log",
            "reason": "session_event_pump_live_test.go:23: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSessionEventPumpLiveHerdr",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
            "reason": "session_event_pump_live_test.go:23: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSimCoverageGate",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-509ef0a6cd81866833cc.log",
            "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSimCoverageGate",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
            "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-bc482d2ae768251ad47f.log",
            "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
            "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-18745ddf7d62465654f5.log",
            "reason": "tmux_leak_guard_mntns_test.go:98: bwrap not installed; cannot build a private-/tmp mount namespace",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/floor-logs/floor-0.log",
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
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-be91eec86b4dbf7e66f1.log",
            "reason": "gastown_test.go:4110: zsh not installed",
            "class": "environment-absence"
          },
          {
            "package": "examples/gastown",
            "test": "TestAttachedRigScopeShellToken/zsh",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-ef24b9dbe59f5d04fac1.log",
            "reason": "gastown_test.go:4110: zsh not installed",
            "class": "environment-absence"
          },
          {
            "package": "examples/gastown",
            "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-ff9cb121652191f033f5.log",
            "reason": "maintenance_scripts_test.go:9249: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
            "class": "environment-absence"
          },
          {
            "package": "examples/gastown",
            "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-29937721dc700c4b8d8b.log",
            "reason": "maintenance_scripts_test.go:9249: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/floor-logs/floor-1.log",
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
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-a9e2a6fbbc2774327994.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/BatchClearsOneKeyKeepsAnother",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-0b74cd4d9bd61207e1fd.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-a9e2a6fbbc2774327994.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-0b74cd4d9bd61207e1fd.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-a9e2a6fbbc2774327994.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-0b74cd4d9bd61207e1fd.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-a9e2a6fbbc2774327994.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-0b74cd4d9bd61207e1fd.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestProbeVersionsAgainstHostBinaries/bd",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4d4fb620ab73bb85eb23.log",
            "reason": "binary_versions_test.go:66: bd not probeable in this environment: locate bd: exec: \"bd\": executable file not found in $PATH",
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/floor-logs/floor-2.log",
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/floor-logs/floor-3.log",
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
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8544388e16a0d6b0cdb0.log",
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/floor-logs/floor-4.log",
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
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-64046beff95fb28ccd66.log",
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/floor-logs/floor-5.log",
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
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
            "reason": "checks_custom_types_test.go:467: bd binary not on PATH",
            "class": "environment-absence"
          },
          {
            "package": "internal/doctor",
            "test": "TestCustomTypesCheck_TableDrift",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
            "reason": "checks_custom_types_test.go:297: bd binary not on PATH",
            "class": "environment-absence"
          },
          {
            "package": "internal/doctor",
            "test": "TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/floor-logs/floor-6.log",
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
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
            "reason": "mcp_conformance_test.go:159: MCP mail script not found: file does not exist",
            "class": "environment-absence"
          },
          {
            "package": "internal/mail/exec",
            "test": "TestMCPMailConformance",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
            "reason": "mcp_conformance_test.go:26: MCP mail script not found: file does not exist",
            "class": "environment-absence"
          },
          {
            "package": "internal/mail/exec",
            "test": "TestMCPMailCrossPodNameResolution",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
            "reason": "mcp_conformance_test.go:195: MCP mail script not found: file does not exist",
            "class": "environment-absence"
          },
          {
            "package": "internal/mail/exec",
            "test": "TestMCPMailProjectKeyIsolation",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
            "reason": "mcp_conformance_test.go:286: MCP mail script not found: file does not exist",
            "class": "environment-absence"
          },
          {
            "package": "internal/mail/exec",
            "test": "TestMCPMailReplyMissingOriginalIsNotFound",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/floor-logs/floor-7.log",
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
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
            "reason": "activity_live_test.go:20: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestActivityLive",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
            "reason": "activity_live_test.go:20: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestHerdrConformance",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
            "reason": "conformance_test.go:18: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestHerdrConformance",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
            "reason": "conformance_test.go:18: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestProviderLive",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
            "reason": "provider_live_test.go:15: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestProviderLive",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
            "reason": "provider_live_test.go:15: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestProviderLiveClaudeKindPath",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
            "reason": "kindpath_live_test.go:105: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestProviderLiveClaudeKindPath",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
            "reason": "kindpath_live_test.go:105: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestProviderLiveOccupantSwapKeepsLiveness",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
            "reason": "panebinding_live_test.go:21: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestProviderLiveOccupantSwapKeepsLiveness",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
            "reason": "panebinding_live_test.go:21: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestSessionEventsLive",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
            "reason": "events_live_test.go:40: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestSessionEventsLive",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/floor-logs/floor-8.log",
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
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:236: Start(skip-start-14): provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/IsRunning_ConcurrentDistinctSessions",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:269: Start(skip-start-18): provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ConcurrentDistinctPrefixes",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:365: Start(skip-start-25): provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_EmptyPrefix",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:347: Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ExcludesStopped",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:331: Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_FindsSessions",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:292: Start skip-start-21: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_PrefixFiltering",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:311: Start skip-start-22: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_ConcurrentDistinctSessions",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:419: Start(skip-start-30): provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_EmptyNamesReturnsTrue",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:393: Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_FalseAfterStop",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:402: Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/SharedSession",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:59: Start shared session: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_ConcurrentDistinctSessions",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:122: concurrent Start(skip-start-3): provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_CreatesRunningSession",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:82: Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_DuplicateReturnsError",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:91: first Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_ConcurrentDistinctSessions",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:207: Start(skip-start-11): provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_AlreadyStopped",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:191: Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_NotRunning",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:168: Start sentinel: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_MakesSessionNotRunning",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/floor-logs/floor-9.log",
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
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
            "reason": "tmux_test.go:3663: not inside tmux \u2014 need server for bind-key",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/tmux",
            "test": "TestGetKeyBinding_SkipsGasTownBindings",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
            "reason": "tmux_test.go:3637: not inside tmux \u2014 need server for bind-key",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/tmux",
            "test": "TestIsGTBinding_DetectsGasTownBindings",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-15c091b746137e5c2921.log",
            "reason": "tmux_test.go:3689: not inside tmux \u2014 need server for bind-key",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/tmux",
            "test": "TestSetBindings_PreserveFallbackOnRepeatedCalls",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/floor-logs/floor-10.log",
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
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
            "reason": "bd_schema_parity_test.go:121: bd not available",
            "class": "environment-absence"
          },
          {
            "package": "test/acceptance/helpers",
            "test": "TestBdLatestSchemaVersionLeavesNothingInItsTempRoot",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
            "reason": "bd_schema_parity_test.go:144: bd not available",
            "class": "environment-absence"
          },
          {
            "package": "test/acceptance/helpers",
            "test": "TestRealBdProvisionLeavesTheHostHomeAlone",
            "status": "SKIP",
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/floor-logs/floor-11.log",
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
            "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-full-fc69bfc5c61515cd4436.log",
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/floor-logs/floor-12.log",
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/floor-logs/floor-13.log",
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/floor-logs/floor-14.log",
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/floor-logs/floor-15.log",
        "exit_code": 0
      }
    ],
    "log_dir": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/floor-logs"
  },
  "skip_inventory": {
    "counts_by_reason_class": {
      "other": 284,
      "environment-absence": 71
    },
    "top_level_counts_by_reason_class": {
      "other": 206,
      "environment-absence": 42
    },
    "tests": [
      {
        "package": "cmd/gc",
        "test": "TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-3d6d3036a2f5902472b5.log",
        "reason": "cmd_bd_test.go:1315: requires a managed bd lifecycle city; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestBdRuntimeEnvUsesValidProviderStateWhenPublishedStateIsMissing",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "bd_env_test.go:1351: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestBuildDesiredState_MinZeroDefaultScaleCheckRoutedWorkCreatesPoolSession",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-752a0bf25b15b465517a.log",
        "reason": "build_desired_state_test.go:6331: uses real bd subprocesses for routed-work scale checks; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestBuildDesiredState_PoolCheckUsesManagedCityDoltPortWhenRigHasNoOverride",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
        "reason": "build_desired_state_test.go:10104: uses a live managed-dolt port probe for scale_check coverage; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestBuildPinnedBDBinaryForTestsUsesGoModSource",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
        "reason": "cmd_wait_test.go:777: builds a real bd binary from source; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestCityInitServiceInitScaffoldsAndFinalizes",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
        "reason": "cityinit_impl_test.go:259: runs the full local init scaffold/finalize path; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestCityRuntimeTick_RefreshesManualSessionOverlayAfterSync",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-64014bc0bb3833bc198c.log",
        "reason": "city_runtime_test.go:4324: runs a full runtime tick/reconcile path; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestCmdGCRealBDTestsUseTestOwnedDoltContext",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
        "reason": "pool_test.go:215: uses real bd to prove test-owned HOME isolation; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestCmdMailInbox_NormalizesCanonicalManagedProviderEnvAndReadsInbox",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-3d6d3036a2f5902472b5.log",
        "reason": "cmd_mail_test.go:1849: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestControllerReloadCityNameChange",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
        "reason": "controller_test.go:1803: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestControllerReloadInvalidConfig",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-752a0bf25b15b465517a.log",
        "reason": "controller_test.go:1741: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestControllerStateMutationRefreshKeepsBuiltinOrdersAndClearsPending",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "api_state_test.go:622: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestControllerStateRuntimeUpdateAcceptsBuiltinAwareRevision",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "api_state_test.go:582: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoBeadsCityUseExternalAdoptUnverifiedSkipsValidation",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-637c74c578a2b17fc654.log",
        "reason": "cmd_beads_city_test.go:677: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoBeadsCityUseExternalRewritesCompatRigWithRelativePath",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-493754ea2d5b4d8858c8.log",
        "reason": "cmd_beads_city_test.go:485: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoBeadsCityUseExternalUpdatesIncludedInheritedRigs",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-f679cd2551fcc64525aa.log",
        "reason": "cmd_beads_city_test.go:188: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoBeadsCityUseExternalWritesVerifiedCityAndInheritedRigs",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
        "reason": "cmd_beads_city_test.go:90: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoDoctorRegistersStaleLocalPackDirCheck",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
        "reason": "cmd_doctor_test.go:479: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoDoctorRegistersStaleLocalPackDirCheckForDefaultRigRemoteImport",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-2bde55fab377f50b4739.log",
        "reason": "cmd_doctor_test.go:599: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoPrimeWithHook_CodexJSONFormatInfersAgentFromWorkDir",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-2ef13e1b6898c5544764.log",
        "reason": "cmd_prime_test.go:1336: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoPrimeWithHook_DeliveredStartupPromptCodexJSONHookFormat",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4d20b3d28497f844aa39.log",
        "reason": "cmd_prime_test.go:1269: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateAllocatePortCmdRepairsMissingCanonicalProviderStateFromPublishedHint",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
        "reason": "cmd_dolt_state_test.go:710: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateAllocatePortCmdRepairsMissingProviderStateFromPublishedHint",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
        "reason": "cmd_dolt_state_test.go:655: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateAllocatePortCmdRepairsStaleProviderStateFromOwnedLivePortHolder",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-088cc38d26d094729229.log",
        "reason": "cmd_dolt_state_test.go:551: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateAllocatePortCmdRepairsStaleWrongPortProviderStateFromPublishedHint",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-f679cd2551fcc64525aa.log",
        "reason": "cmd_dolt_state_test.go:773: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateAllocatePortCmdRepairsStoppedProviderStateFromOwnedLivePortHolder",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
        "reason": "cmd_dolt_state_test.go:603: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateAllocatePortCmdReusesLiveProviderState",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
        "reason": "cmd_dolt_state_test.go:388: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateExistingManagedCmdFallsBackToPublishedRuntimeState",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
        "reason": "cmd_dolt_state_test.go:1359: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateExistingManagedCmdRejectsForeignListenerBackedOnlyByStateDataDir",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-637c74c578a2b17fc654.log",
        "reason": "cmd_dolt_state_test.go:1230: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateExistingManagedCmdReportsReusableOwnedServer",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-423c6a0da1986b90e9a7.log",
        "reason": "cmd_dolt_state_test.go:1293: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateInspectManagedCmdReportsPortHolderOwnership",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-493754ea2d5b4d8858c8.log",
        "reason": "cmd_dolt_state_test.go:1057: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateInspectManagedCmdUsesPIDFileAndStateOwnership",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-2ef13e1b6898c5544764.log",
        "reason": "cmd_dolt_state_test.go:934: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStatePreflightCleanCmdPreservesLiveArtifacts",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-752a0bf25b15b465517a.log",
        "reason": "cmd_dolt_state_test.go:1528: spawns managed dolt holder processes; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateProbeManagedCmdReportsImposterHolder",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-3d6d3036a2f5902472b5.log",
        "reason": "cmd_dolt_state_test.go:1139: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateProbeManagedCmdReportsRunningOwnedHolder",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-0e1499ad8e8a0a6b9d81.log",
        "reason": "cmd_dolt_state_test.go:1097: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateRecoverManagedCmdReportsReadOnlyAndRestarts",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-637c74c578a2b17fc654.log",
        "reason": "cmd_dolt_state_test.go:2632: spawns managed dolt recovery processes; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateStopManagedCmdDoesNotKillImposterPortHolder",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
        "reason": "cmd_dolt_state_test.go:2591: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateStopManagedCmdStopsManagedPID",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-0e1499ad8e8a0a6b9d81.log",
        "reason": "cmd_dolt_state_test.go:2469: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateWaitReadyCmdReturnsReady",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-64014bc0bb3833bc198c.log",
        "reason": "cmd_dolt_state_test.go:2360: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestE1PreLeafBooleanHelpNoScopeEager",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-bc482d2ae768251ad47f.log",
        "reason": "cmd_commands_test.go:812: ga-klo4gz: this test's purpose is exercising ambient cwd-based city resolution (resolveContextFromDir step 10) to distinguish the ambient city's commands from an explicitly-selected one, which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestE1PreLeafBooleanHelpNoScopeEager",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-493754ea2d5b4d8858c8.log",
        "reason": "cmd_commands_test.go:812: ga-klo4gz: this test's purpose is exercising ambient cwd-based city resolution (resolveContextFromDir step 10) to distinguish the ambient city's commands from an explicitly-selected one, which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestEnsureBdRuntimeCustomTypesHealsUpgradedNativeStore",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
        "reason": "gc_beads_bd_config_commit_test.go:457: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestEnsureBeadsProvider_bdAcceptsHealthyServerAfterStartError",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
        "reason": "beads_provider_lifecycle_test.go:1723: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestEvaluatePoolDefaultScaleCheckCountsRoutedReadyWork",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-088cc38d26d094729229.log",
        "reason": "pool_test.go:108: uses real bd and jq for default scale_check coverage; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestEvaluatePoolDefaultScaleCheckIgnoresRoutedActiveUnassignedWork",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
        "reason": "pool_test.go:154: uses real bd and jq for default scale_check coverage; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
        "reason": "cmd_bd_test.go:1479: requires a managed bd lifecycle city; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBdUsesEnclosingRigWhenNoFlag",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-56f04454dd9158a9ee8a.log",
        "reason": "cmd_bd_test.go:1852: ga-klo4gz: this test's purpose is exercising resolveContextFromDir's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit GC_CITY/GC_CITY_PATH/GC_CITY_ROOT override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBdUsesEnclosingRigWhenNoFlag",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
        "reason": "cmd_bd_test.go:1852: ga-klo4gz: this test's purpose is exercising resolveContextFromDir's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit GC_CITY/GC_CITY_PATH/GC_CITY_ROOT override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdInitEnsuresProjectIdentityWhenMetadataExistsWithoutProjectID",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
        "reason": "beads_provider_lifecycle_test.go:6677: runs the materialized gc-beads-bd init script with GC_BIN helper; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdInitNormalizesScopeAndRemovesLocalServerArtifacts",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "beads_provider_lifecycle_test.go:12978: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdProviderOwnedRealInitIgnoresAncestorBeadsWorkspace",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-2ef13e1b6898c5544764.log",
        "reason": "beads_provider_script_scope_lifecycle_linux_test.go:352: runs the real bd provider init; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdProviderOwnedRealLifecycleStopsOwnedProcesses",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-752a0bf25b15b465517a.log",
        "reason": "beads_provider_lifecycle_test.go:14190: runs the real bd provider lifecycle; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdStartConcurrentWaitPassesRemainingExistingManagedBudget",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-637c74c578a2b17fc654.log",
        "reason": "beads_provider_lifecycle_test.go:11056: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdStartDoesNotReplaceLiveLockFileInode",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-0e1499ad8e8a0a6b9d81.log",
        "reason": "beads_provider_lifecycle_test.go:10308: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdStartIgnoresReachableCompatPortFileInput",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-493754ea2d5b4d8858c8.log",
        "reason": "beads_provider_lifecycle_test.go:9693: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdStartIsIdempotentWhenAlreadyRunning",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "beads_provider_lifecycle_test.go:11680: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdStartLeavesForeignPortHolderAlone",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-088cc38d26d094729229.log",
        "reason": "beads_provider_lifecycle_test.go:11932: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdStartRestartsServerHoldingDeletedDataInodes",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "beads_provider_lifecycle_test.go:11767: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdStartUsesRootBeadsDataDir",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
        "reason": "beads_provider_lifecycle_test.go:5144: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdStartWaitsForConcurrentStarterSuccess",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-3d6d3036a2f5902472b5.log",
        "reason": "beads_provider_lifecycle_test.go:10494: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdStartWaitsForSlowConcurrentStarterSuccess",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
        "reason": "beads_provider_lifecycle_test.go:10777: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestHealthBeadsProviderWaitsForStorePingAfterRecovery",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
        "reason": "beads_provider_lifecycle_test.go:5695: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestInstallSupervisorSystemdWarmRefreshLeavesUnregisteredWorkspaceServices",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-2bde55fab377f50b4739.log",
        "reason": "cmd_supervisor_test.go:1851: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestInstallSupervisorSystemdWarmRefreshStopsWorkspaceServicesBeforeStart",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-23e7d9fc763496dd8095.log",
        "reason": "cmd_supervisor_test.go:1691: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestLoadStatusSessionSnapshotKillsBdChildOnTimeout",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4d20b3d28497f844aa39.log",
        "reason": "city_status_snapshot_test.go:200: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestManagedDoltConfigGoWriterMatchesShellFallbackSemantics",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
        "reason": "beads_provider_lifecycle_test.go:11536: starts the materialized gc-beads-bd shell fallback; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestManagedDoltDirtyTablesWithPasswordAgainstRealServer",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-2ef13e1b6898c5544764.log",
        "reason": "beads_provider_dirty_tables_test.go:345: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestManagedDoltHealthCheckWithPasswordUsesDirectHelpersAgainstRealServer",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
        "reason": "dolt_project_id_test.go:184: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestManagedDoltScopeWatchdogHelper",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-568812e4d525bdd1f974.log",
        "reason": "dolt_scope_watchdog_test.go:176: helper process only",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestManagedDoltScopeWatchdogHelper",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-2bde55fab377f50b4739.log",
        "reason": "dolt_scope_watchdog_test.go:176: helper process only",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestManagedDoltServingDataDirWithPasswordAgainstRealServer",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-637c74c578a2b17fc654.log",
        "reason": "dolt_identity_test.go:156: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestManagedDoltWaitReadyWithPasswordUsesDirectQueryProbe",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-637c74c578a2b17fc654.log",
        "reason": "dolt_project_id_test.go:212: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestManagedDoltWatchdogExternalParentHelper",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-12697e2ea0d32c278115.log",
        "reason": "dolt_start_managed_test.go:647: helper process only",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestManagedDoltWatchdogExternalParentHelper",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
        "reason": "dolt_start_managed_test.go:647: helper process only",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestPackRegistryLiveGascityPacksCatalog",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-251f1cf8cede4438b2d5.log",
        "reason": "cmd_pack_registry_test.go:758: set GC_TEST_GASCITY_PACKS_REGISTRY to a gascity-packs registry.toml source to run this live catalog canary",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestPackRegistryLiveGascityPacksCatalog",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-2bde55fab377f50b4739.log",
        "reason": "cmd_pack_registry_test.go:758: set GC_TEST_GASCITY_PACKS_REGISTRY to a gascity-packs registry.toml source to run this live catalog canary",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestPrepareCityForSupervisorPrunesLegacyScripts",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
        "reason": "script_resolve_test.go:313: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestProjectIdentityL3AdapterContractAndManagedComposition",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
        "reason": "dolt_project_id_test.go:281: requires a managed dolt server; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestPublishManagedDoltRuntimeStateRecoversMissingProviderStateWithPortHint",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-423c6a0da1986b90e9a7.log",
        "reason": "dolt_runtime_publication_test.go:130: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestPublishManagedDoltRuntimeStateRecoversStaleWrongPortProviderStateWithPublishedHint",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
        "reason": "dolt_runtime_publication_test.go:204: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestPublishManagedDoltRuntimeStateRepairsStaleProviderState",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
        "reason": "dolt_runtime_publication_test.go:28: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestPublishManagedDoltRuntimeStateSucceedsWhenAlreadyValid",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
        "reason": "dolt_runtime_publication_test.go:294: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRecoverManagedDoltProcessReturnsWhenConcurrentStarterBecomesReady",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-423c6a0da1986b90e9a7.log",
        "reason": "cmd_dolt_state_test.go:2797: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRecoverManagedDoltProcessReusesHealthyManagedServerOnReboundPort",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
        "reason": "cmd_dolt_state_test.go:2894: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRecoverManagedDoltProcessWithPasswordReusesHealthyRealServer",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-423c6a0da1986b90e9a7.log",
        "reason": "dolt_project_id_test.go:225: requires a managed dolt server; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRegisterCityWithSupervisorKeepsRegistrationWhenReloadFails",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-2bde55fab377f50b4739.log",
        "reason": "cmd_supervisor_city_test.go:317: exercises supervisor registration retry behavior; run make test-cmd-gc-process for scenario coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRegisterRealBDServerStopStopsServerBeforeTempDirCleanup",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
        "reason": "path_helpers_test.go:1042: spawns subprocesses to simulate still-running dolt sql-servers; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestReloadConfigTracedRescansOrdersWhenConfigRevisionUnchanged",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-088cc38d26d094729229.log",
        "reason": "cmd_reload_test.go:605: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRenderedCrewPromptShowsProviderSpecificInstructionsFile",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-e4245ddf6a37030111bc.log",
        "reason": "prompt_test.go:1420: crew.template.md not readable at ../../examples/gastown/packs/gastown/assets/prompts/crew.template.md: open ../../examples/gastown/packs/gastown/assets/prompts/crew.template.md: no such file or directory",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRenderedCrewPromptShowsProviderSpecificInstructionsFile",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
        "reason": "prompt_test.go:1420: crew.template.md not readable at ../../examples/gastown/packs/gastown/assets/prompts/crew.template.md: open ../../examples/gastown/packs/gastown/assets/prompts/crew.template.md: no such file or directory",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRepairedManagedDoltRuntimeStateAcceptsSymlinkEquivalentDataDir",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-3d6d3036a2f5902472b5.log",
        "reason": "cmd_dolt_state_test.go:186: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestReplay",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-568812e4d525bdd1f974.log",
        "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestReplay",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-752a0bf25b15b465517a.log",
        "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestResolveCityFlag/flag_empty_fallback",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-509ef0a6cd81866833cc.log",
        "reason": "main_test.go:810: ga-klo4gz: this subtest's purpose is exercising resolveCity's ambient cwd-based fallback (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestResolveCityFlag/flag_empty_fallback",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
        "reason": "main_test.go:810: ga-klo4gz: this subtest's purpose is exercising resolveCity's ambient cwd-based fallback (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestResolveStoredSessionLogSource_UniqueWorkDirFallsBackBeyondLatestAlias",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
        "reason": "cmd_session_logs_test.go:388: probes provider transcript lookup before workdir fallback; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestResolvedRuntimeCityDoltTargetDoesNotMaskInvalidCanonicalConfigWithProviderState",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
        "reason": "bd_env_test.go:1598: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRigAnywhere_ResolveContext/registered_rig_cwd_ambiguous_falls_through",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-18745ddf7d62465654f5.log",
        "reason": "rig_anywhere_test.go:488: ga-klo4gz: this subtest's purpose is exercising the fallthrough from an ambiguous registered-rig match (step 9) to resolveContext's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRigAnywhere_ResolveContext/registered_rig_cwd_ambiguous_falls_through",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "rig_anywhere_test.go:488: ga-klo4gz: this subtest's purpose is exercising the fallthrough from an ambiguous registered-rig match (step 9) to resolveContext's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRigAnywhere_ResolveContext/walk_up_fallback",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-18745ddf7d62465654f5.log",
        "reason": "rig_anywhere_test.go:377: ga-klo4gz: this subtest's purpose is exercising resolveContext's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRigAnywhere_ResolveContext/walk_up_fallback",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "rig_anywhere_test.go:377: ga-klo4gz: this subtest's purpose is exercising resolveContext's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRigAnywhere_ResolveContext/walk_up_fallback_with_rig_match",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-18745ddf7d62465654f5.log",
        "reason": "rig_anywhere_test.go:401: ga-klo4gz: this subtest's purpose is exercising resolveContext's ambient cwd walk-up (step 10) followed by a rig match, which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRigAnywhere_ResolveContext/walk_up_fallback_with_rig_match",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "rig_anywhere_test.go:401: ga-klo4gz: this subtest's purpose is exercising resolveContext's ambient cwd walk-up (step 10) followed by a rig match, which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRunManagedDoltSQLEmptyPasswordMasksDefaultProfileAgainstRealServer",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
        "reason": "dolt_sql_health_test.go:525: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRunWorkflowServeReturnsControlErrorWithoutQuarantine",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
        "reason": "cmd_convoy_dispatch_test.go:3899: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestScopedBdStoreForCityKillsChildOnCtxCancel",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "scoped_store_test.go:364: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestSendReloadControlRequestInvalidConfig",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
        "reason": "cmd_reload_test.go:733: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestSendReloadControlRequestNoChange",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "cmd_reload_test.go:539: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestSessionEventPumpLiveHerdr",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-c2ab4e7d4735e1bd66a7.log",
        "reason": "session_event_pump_live_test.go:23: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestSessionEventPumpLiveHerdr",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
        "reason": "session_event_pump_live_test.go:23: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestShellExecRunnerKillsProcessGroupAfterWaitDelay",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "order_dispatch_test.go:3541: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestShellExecRunnerKillsProcessGroupOnTimeout",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
        "reason": "order_dispatch_test.go:3518: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestSimCoverageGate",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-509ef0a6cd81866833cc.log",
        "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestSimCoverageGate",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
        "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestStartTCPListenerProcessInDirRegistersCleanup",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-752a0bf25b15b465517a.log",
        "reason": "cmd_dolt_state_test.go:410: spawns a TCP listener process and verifies cleanup; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestStopCityManagedBeadsProviderAfterSuccessfulStopStopsDefaultBD",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4d20b3d28497f844aa39.log",
        "reason": "cmd_stop_test.go:1376: exercises managed bd provider shutdown; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestStopManagedDoltRefusesSIGKILLWhileLockHeld",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-2ef13e1b6898c5544764.log",
        "reason": "dolt_lifecycle_race_test.go:170: spawns real processes; run via make test-cmd-gc-process",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestStopManagedDoltWaitsForLockReleaseAfterExit",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-493754ea2d5b4d8858c8.log",
        "reason": "dolt_lifecycle_race_test.go:238: spawns real processes; run via make test-cmd-gc-process",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestStopSupervisorWithWaitFailsWhenLaunchdStopFails",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-aa6cf5c138f0b61b866a.log",
        "reason": "cmd_supervisor_test.go:6604: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestStopSupervisorWithWaitFailsWhenLaunchdStopFails",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
        "reason": "cmd_supervisor_test.go:6604: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestSupervisorInstallUnsupportedOS",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-514989cc035c0e9a8ff5.log",
        "reason": "cmd_supervisor_test.go:1473: unsupported-os test only applies outside darwin/linux",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestSupervisorInstallUnsupportedOS",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
        "reason": "cmd_supervisor_test.go:1473: unsupported-os test only applies outside darwin/linux",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-bc482d2ae768251ad47f.log",
        "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
        "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-18745ddf7d62465654f5.log",
        "reason": "tmux_leak_guard_mntns_test.go:98: bwrap not installed; cannot build a private-/tmp mount namespace",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "tmux_leak_guard_mntns_test.go:98: bwrap not installed; cannot build a private-/tmp mount namespace",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestTerminateManagedDoltPIDRefusesSIGKILLWhileLockHeld",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4d20b3d28497f844aa39.log",
        "reason": "dolt_lifecycle_race_test.go:148: spawns real processes; run via make test-cmd-gc-process",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestTerminateManagedDoltTestPIDKillsProcessGroup",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
        "reason": "dolt_start_managed_test.go:1635: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestTutorial01",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-752a0bf25b15b465517a.log",
        "reason": "main_test.go:349: runs tutorial testscript scenarios; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestUnloadSupervisorServiceDarwinDisablesBootsOutAndVerifiesAbsent",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-821a75e4a503601af052.log",
        "reason": "cmd_supervisor_test.go:2527: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestUnloadSupervisorServiceDarwinDisablesBootsOutAndVerifiesAbsent",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
        "reason": "cmd_supervisor_test.go:2527: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestUnloadSupervisorServiceDarwinFailsWhenTargetStillLoaded",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-074fc6c34a1fb2577127.log",
        "reason": "cmd_supervisor_test.go:2642: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestUnloadSupervisorServiceDarwinFailsWhenTargetStillLoaded",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "cmd_supervisor_test.go:2642: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestUnloadSupervisorServiceDarwinReturnsLaunchctlFailures",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-c2ab4e7d4735e1bd66a7.log",
        "reason": "cmd_supervisor_test.go:2819: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestUnloadSupervisorServiceDarwinReturnsLaunchctlFailures",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
        "reason": "cmd_supervisor_test.go:2819: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestUnloadSupervisorServiceDarwinWaitsThroughSIGTERMedUntilAbsent",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-bf8ed8ba46004086b3b9.log",
        "reason": "cmd_supervisor_test.go:2577: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestUnloadSupervisorServiceDarwinWaitsThroughSIGTERMedUntilAbsent",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "cmd_supervisor_test.go:2577: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestV2G1Budget",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-514989cc035c0e9a8ff5.log",
        "reason": "v2_g1_test.go:120: gate G1 times this host: set GC_V2_G1=1",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestV2G1Budget",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
        "reason": "v2_g1_test.go:120: gate G1 times this host: set GC_V2_G1=1",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestV2GatherProductionShape",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-c2ab4e7d4735e1bd66a7.log",
        "reason": "v2_g1_test.go:178: gate G1b reads a store copy: set GC_V2_G1B_STORE",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestV2GatherProductionShape",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-423c6a0da1986b90e9a7.log",
        "reason": "v2_g1_test.go:178: gate G1b reads a store copy: set GC_V2_G1B_STORE",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestValidDoltRuntimeStateAcceptsSymlinkEquivalentDataDir",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-0e1499ad8e8a0a6b9d81.log",
        "reason": "cmd_dolt_state_test.go:139: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestValidDoltRuntimeStateRejectsAlivePIDThatDoesNotOwnManagedDolt",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "cmd_dolt_state_test.go:522: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestVerifyExternalDoltEndpointRejectsMissingLocalProjectID",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "cmd_rig_endpoint_test.go:1689: requires a managed external dolt endpoint; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestVerifyExternalDoltEndpointRejectsProjectIdentityMismatch",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
        "reason": "cmd_rig_endpoint_test.go:1651: requires a managed external dolt endpoint; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestVerifySupervisorServiceStoppedDarwinFailsWhenAbsenceUnconfirmed",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-75af79934639abaae69c.log",
        "reason": "cmd_supervisor_test.go:2695: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestVerifySupervisorServiceStoppedDarwinFailsWhenAbsenceUnconfirmed",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-088cc38d26d094729229.log",
        "reason": "cmd_supervisor_test.go:2695: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestVerifySupervisorServiceStoppedDarwinHonorsShorterCallerDeadline",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-514989cc035c0e9a8ff5.log",
        "reason": "cmd_supervisor_test.go:2742: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestVerifySupervisorServiceStoppedDarwinHonorsShorterCallerDeadline",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
        "reason": "cmd_supervisor_test.go:2742: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestWatchConfigTargets_CloseReturnsFDsToBaseline",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-2ab73b52d8fd4dc346df.log",
        "reason": "controller_watcher_fd_test.go:27: darwin-only: the kqueue Close() FD leak (fsnotify#732) is a Darwin/kqueue bug",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestWatchConfigTargets_CloseReturnsFDsToBaseline",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
        "reason": "controller_watcher_fd_test.go:27: darwin-only: the kqueue Close() FD leak (fsnotify#732) is a Darwin/kqueue bug",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestWorkerHandleForSessionWithConfigUsesResolvedProviderOnFirstStart",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-088cc38d26d094729229.log",
        "reason": "worker_handle_test.go:36: waits through stale session-key detection; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestWorkerHandleForSessionWithConfigUsesResolvedProviderOnResume",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-f679cd2551fcc64525aa.log",
        "reason": "worker_handle_test.go:1266: waits through stale session-key detection; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "examples/gastown",
        "test": "TestAttachedRigScopeShellToken/zsh",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-be91eec86b4dbf7e66f1.log",
        "reason": "gastown_test.go:4110: zsh not installed",
        "class": "environment-absence"
      },
      {
        "package": "examples/gastown",
        "test": "TestAttachedRigScopeShellToken/zsh",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-ef24b9dbe59f5d04fac1.log",
        "reason": "gastown_test.go:4110: zsh not installed",
        "class": "environment-absence"
      },
      {
        "package": "examples/gastown",
        "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-ff9cb121652191f033f5.log",
        "reason": "maintenance_scripts_test.go:9249: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
        "class": "environment-absence"
      },
      {
        "package": "examples/gastown",
        "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-29937721dc700c4b8d8b.log",
        "reason": "maintenance_scripts_test.go:9249: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
        "class": "environment-absence"
      },
      {
        "package": "examples/gastown",
        "test": "TestTmuxKeybindingsAlternateScreenPassthrough",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-be91eec86b4dbf7e66f1.log",
        "reason": "gastown_test.go:460: embedded gascity-packs does not yet include #{alternate_on} in WheelUpPane (gastownhall/gascity-packs#136); test activates once go.mod is bumped to that release",
        "class": "other"
      },
      {
        "package": "examples/gastown",
        "test": "TestTmuxKeybindingsAlternateScreenPassthrough",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-ef24b9dbe59f5d04fac1.log",
        "reason": "gastown_test.go:460: embedded gascity-packs does not yet include #{alternate_on} in WheelUpPane (gastownhall/gascity-packs#136); test activates once go.mod is bumped to that release",
        "class": "other"
      },
      {
        "package": "internal/api",
        "test": "TestStatusListStoreWithTimeoutKillsBdChildOnTimeout",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-5d010ccbe1c203fcb722.log",
        "reason": "handler_status_scoped_store_test.go:507: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "internal/api",
        "test": "TestStatusReadyStoreWithTimeoutKillsBdChildOnTimeout",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-c1bdba0ccc8453285d8e.log",
        "reason": "handler_status_scoped_store_test.go:544: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "internal/api",
        "test": "TestStatusSessionSnapshotKillsBdChildOnTimeout",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-753c0e3c761abb7535f5.log",
        "reason": "handler_status_scoped_store_test.go:141: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestBdStoreCloseNotificationConformance/CloseIfMatchAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-ceb32a3c147a95796597.log",
        "reason": "close_notification_conformance.go:54: CloseIfMatch: conditional writes unsupported",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestBdStoreCloseNotificationConformance/CloseIfMatchAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-3ee1ae2eaa26dfd63a88.log",
        "reason": "close_notification_conformance.go:54: CloseIfMatch: conditional writes unsupported",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestBdStoreCloseNotificationConformance/UpdateIfMatchStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-ceb32a3c147a95796597.log",
        "reason": "close_notification_conformance.go:57: UpdateIfMatch(status=closed): conditional writes unsupported",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestBdStoreCloseNotificationConformance/UpdateIfMatchStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-3ee1ae2eaa26dfd63a88.log",
        "reason": "close_notification_conformance.go:57: UpdateIfMatch(status=closed): conditional writes unsupported",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestBdStoreConditionalWriterConformance",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-20cfdbabba8f52b16e0b.log",
        "reason": "bdstore_conditional_integration_test.go:69: installed bd lacks --if-revision (pre-beads#4682); the conformance row skips cleanly and becomes the authoritative guard for the provisional body codes once #4682 lands",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestExecTimeoutHardExitHarness",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-ad823e3d7eb71467b6af.log",
        "reason": "exec_timeout_linux_test.go:66: harness process",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestExecTimeoutHardExitHarness",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-0018349265fa0d9a3ea4.log",
        "reason": "exec_timeout_linux_test.go:66: harness process",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestFileStore/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-9ba15fc4ea01a6e0b979.log",
        "reason": "close_notification_conformance.go:89: backing *beads.FileStore lacks the capability UpdateIfAssignmentStatusClosed needs",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestFileStore/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-a93729df64a652fced43.log",
        "reason": "close_notification_conformance.go:89: backing *beads.FileStore lacks the capability UpdateIfAssignmentStatusClosed needs",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestFileStoreReadyParityConformance/FileStore/CachedReadyParity",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-50e5e5740ff056c0f347.log",
        "reason": "ready_parity_conformance.go:69: skipping CachedReadyParity (bead ga-gmf8r, expires 2026-12-15): MemStore and FileStore Ready block on a missing or foreign blocker and on a closed blocker with gc.work_outcome=blocked, which a primed CachingStore cannot see without a ready projection, and return insertion order with Limit applied mid-scan instead of the canonical (priority, created_at, id) order. Replacement proof: MemStore implements enrichReadyProjectionForCache and canonical ready order, and TestMemStoreReadyParityConformance and TestFileStoreReadyParityConformance run this suite unwaived",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestFileStoreReadyParityConformance/FileStore/CachedReadyParity",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8c9c804909a3bb579837.log",
        "reason": "ready_parity_conformance.go:69: skipping CachedReadyParity (bead ga-gmf8r, expires 2026-12-15): MemStore and FileStore Ready block on a missing or foreign blocker and on a closed blocker with gc.work_outcome=blocked, which a primed CachingStore cannot see without a ready projection, and return insertion order with Limit applied mid-scan instead of the canonical (priority, created_at, id) order. Replacement proof: MemStore implements enrichReadyProjectionForCache and canonical ready order, and TestMemStoreReadyParityConformance and TestFileStoreReadyParityConformance run this suite unwaived",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestKillCommandTreeKillsProcessGroup",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-7f2c404ca64cd2da0395.log",
        "reason": "bdstore_exec_internal_test.go:266: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestMemStore/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-a9e2a6fbbc2774327994.log",
        "reason": "close_notification_conformance.go:89: backing *beads.MemStore lacks the capability UpdateIfAssignmentStatusClosed needs",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestMemStore/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-0b74cd4d9bd61207e1fd.log",
        "reason": "close_notification_conformance.go:89: backing *beads.MemStore lacks the capability UpdateIfAssignmentStatusClosed needs",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestMemStoreReadyParityConformance/MemStore/CachedReadyParity",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-50e5e5740ff056c0f347.log",
        "reason": "ready_parity_conformance.go:69: skipping CachedReadyParity (bead ga-gmf8r, expires 2026-12-15): MemStore and FileStore Ready block on a missing or foreign blocker and on a closed blocker with gc.work_outcome=blocked, which a primed CachingStore cannot see without a ready projection, and return insertion order with Limit applied mid-scan instead of the canonical (priority, created_at, id) order. Replacement proof: MemStore implements enrichReadyProjectionForCache and canonical ready order, and TestMemStoreReadyParityConformance and TestFileStoreReadyParityConformance run this suite unwaived",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestMemStoreReadyParityConformance/MemStore/CachedReadyParity",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8c9c804909a3bb579837.log",
        "reason": "ready_parity_conformance.go:69: skipping CachedReadyParity (bead ga-gmf8r, expires 2026-12-15): MemStore and FileStore Ready block on a missing or foreign blocker and on a closed blocker with gc.work_outcome=blocked, which a primed CachingStore cannot see without a ready projection, and return insertion order with Limit applied mid-scan instead of the canonical (priority, created_at, id) order. Replacement proof: MemStore implements enrichReadyProjectionForCache and canonical ready order, and TestMemStoreReadyParityConformance and TestFileStoreReadyParityConformance run this suite unwaived",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestMetadataEmptyStringClearContract/Postgres/BatchClearsOneKeyKeepsAnother",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-a9e2a6fbbc2774327994.log",
        "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads",
        "test": "TestMetadataEmptyStringClearContract/Postgres/BatchClearsOneKeyKeepsAnother",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-0b74cd4d9bd61207e1fd.log",
        "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads",
        "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-a9e2a6fbbc2774327994.log",
        "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads",
        "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-0b74cd4d9bd61207e1fd.log",
        "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads",
        "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-a9e2a6fbbc2774327994.log",
        "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads",
        "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-0b74cd4d9bd61207e1fd.log",
        "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads",
        "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-a9e2a6fbbc2774327994.log",
        "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads",
        "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-0b74cd4d9bd61207e1fd.log",
        "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads",
        "test": "TestNativeDoltStoreConformance/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-999a89ecc5b3d09abdd4.log",
        "reason": "close_notification_conformance.go:89: backing *beads.NativeDoltStore lacks the capability UpdateIfAssignmentStatusClosed needs",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestNativeDoltStoreConformance/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-9a6a09bab414814cab53.log",
        "reason": "close_notification_conformance.go:89: backing *beads.NativeDoltStore lacks the capability UpdateIfAssignmentStatusClosed needs",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestNativeDoltStoreNormalizesRealUpstreamMissingIssueErrors",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-ad823e3d7eb71467b6af.log",
        "reason": "native_dolt_store_test.go:648: upstream beads storage unavailable: Dolt server unreachable at 127.0.0.1:0: dial tcp 127.0.0.1:0: connect: connection refused",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestNativeDoltStoreNormalizesRealUpstreamMissingIssueErrors",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4d4fb620ab73bb85eb23.log",
        "reason": "native_dolt_store_test.go:648: upstream beads storage unavailable: Dolt server unreachable at 127.0.0.1:0: dial tcp 127.0.0.1:0: connect: connection refused",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestProbeVersionsAgainstHostBinaries/bd",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4d4fb620ab73bb85eb23.log",
        "reason": "binary_versions_test.go:66: bd not probeable in this environment: locate bd: exec: \"bd\": executable file not found in $PATH",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads",
        "test": "TestSQLiteStoreConformance/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-50e5e5740ff056c0f347.log",
        "reason": "close_notification_conformance.go:89: backing *beads.SQLiteStore lacks the capability UpdateIfAssignmentStatusClosed needs",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestSQLiteStoreConformance/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-363a36f2662d541bc75e.log",
        "reason": "close_notification_conformance.go:89: backing *beads.SQLiteStore lacks the capability UpdateIfAssignmentStatusClosed needs",
        "class": "other"
      },
      {
        "package": "internal/beads/beadstest",
        "test": "TestRequireLedgeredSkipIsDateFree/LongLapsed",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-ec8983708a4fd6968a43.log",
        "reason": "conformance_skips_test.go:60: skipping LongLapsed (bead ga-example, expires 2000-01-01): example",
        "class": "other"
      },
      {
        "package": "internal/beads/contract",
        "test": "TestMigrateJournalFileMatchesPinnedBeads",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8544388e16a0d6b0cdb0.log",
        "reason": "migrate_journal_test.go:37: pinned beads v1.3.1 is not unpacked in the local module cache: open /tmp/bt/_tmp/35731ca362c311940500020a6c8a2007/go/pkg/mod/github.com/steveyegge/beads@v1.3.1/cmd/bd/migrate_dolt_mode.go: no such file or directory",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads/exec",
        "test": "TestBrProviderConformance",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-64046beff95fb28ccd66.log",
        "reason": "br_test.go:18: br not available",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads/splittest",
        "test": "TestStrictStorePinnedIDFenceConformance/TheFenceCoversTheGuardedBatch",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-c424d7b9771e02bed0c3.log",
        "reason": "pinned_id_fence_conformance.go:467: store has no guarded batcher",
        "class": "other"
      },
      {
        "package": "internal/beads/splittest",
        "test": "TestStrictStorePinnedIDFenceConformance/TheGuardedBatchChecksTheNamespaceBeforeExistence",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-c424d7b9771e02bed0c3.log",
        "reason": "pinned_id_fence_conformance.go:533: store has no guarded batcher",
        "class": "other"
      },
      {
        "package": "internal/config",
        "test": "TestLoadWithIncludes_PatchTargetingImplicitAgentErrorsToday",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4bbf1267e0c4ccc54042.log",
        "reason": "implicit_agent_patch_repro_test.go:68: bug ga-91uk5m fixed; delete this characterization test",
        "class": "other"
      },
      {
        "package": "internal/connect/tokenrender",
        "test": "TestWriteTokenFileKeepsGroup",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-f79997cecbebd6b14e33.log",
        "reason": "write_test.go:376: the test user has no secondary group and is not root",
        "class": "other"
      },
      {
        "package": "internal/connect/tokenrender",
        "test": "TestWriteTokenFileKeepsOwnerAsRoot",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-f79997cecbebd6b14e33.log",
        "reason": "write_test.go:475: needs root to own a file as another user",
        "class": "other"
      },
      {
        "package": "internal/convergence",
        "test": "TestResolveConditionPath/darwin_private_temp_alias_must_not_falsely_reject_a_contained_relative_condition_path",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-0fb1903ec0635aebe9eb.log",
        "reason": "condition_test.go:594: darwin-only: the /private/{tmp,var} alias collapse is a no-op on other platforms",
        "class": "other"
      },
      {
        "package": "internal/convergence",
        "test": "TestResolveEvaluateStep_DarwinPrivateTempAliasWithExistingPrompt",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-0fb1903ec0635aebe9eb.log",
        "reason": "evaluate_test.go:189: darwin-only: the /private/{tmp,var} alias collapse is a no-op on other platforms",
        "class": "other"
      },
      {
        "package": "internal/dispatch",
        "test": "TestCrashInjectionCallBudgetMolecule/fail",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-e7f4116e9a278807bf1e.log",
        "reason": "call_budget_test.go:536: ga-vnycm2.27: interrupted abort-scope converges with different skip metadata",
        "class": "other"
      },
      {
        "package": "internal/doctor",
        "test": "TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
        "reason": "checks_custom_types_test.go:467: bd binary not on PATH",
        "class": "environment-absence"
      },
      {
        "package": "internal/doctor",
        "test": "TestCustomTypesCheck_TableDrift",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
        "reason": "checks_custom_types_test.go:297: bd binary not on PATH",
        "class": "environment-absence"
      },
      {
        "package": "internal/doctor",
        "test": "TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
        "reason": "checks_custom_types_test.go:399: bd binary not on PATH",
        "class": "environment-absence"
      },
      {
        "package": "internal/formula",
        "test": "TestCompileBugReportFlowV2",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8754fd06808e5b38215b.log",
        "reason": "compile_test.go:1359: tooling formula not present: stat /home/ubuntu/tooling/formulas/mol-bug-report-flow-v2.toml: permission denied",
        "class": "other"
      },
      {
        "package": "internal/fsys",
        "test": "TestWriteFileIfContentOrModeChangedAtomic_RepairsSpecialModeBits",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8baf01f801bd5b165b89.log",
        "reason": "atomic_test.go:185: filesystem did not preserve setuid bit in test mode: -rwxr-xr-x",
        "class": "other"
      },
      {
        "package": "internal/gitcred",
        "test": "TestLoadAcceptsRootOwnedGroupReadable",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-5c43118a23fc611d12c8.log",
        "reason": "rules_test.go:186: needs root to chown the fixture to the Secret-mount shape",
        "class": "other"
      },
      {
        "package": "internal/mail/exec",
        "test": "TestMCPMailBridgeSourceable",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
        "reason": "mcp_conformance_test.go:159: MCP mail script not found: file does not exist",
        "class": "environment-absence"
      },
      {
        "package": "internal/mail/exec",
        "test": "TestMCPMailConformance",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
        "reason": "mcp_conformance_test.go:26: MCP mail script not found: file does not exist",
        "class": "environment-absence"
      },
      {
        "package": "internal/mail/exec",
        "test": "TestMCPMailConformanceLive",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
        "reason": "mcp_live_test.go:35: set GC_TEST_MCP_MAIL=1 to run (or use make test-mcp-mail)",
        "class": "other"
      },
      {
        "package": "internal/mail/exec",
        "test": "TestMCPMailCrossPodNameResolution",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
        "reason": "mcp_conformance_test.go:195: MCP mail script not found: file does not exist",
        "class": "environment-absence"
      },
      {
        "package": "internal/mail/exec",
        "test": "TestMCPMailProjectKeyIsolation",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
        "reason": "mcp_conformance_test.go:286: MCP mail script not found: file does not exist",
        "class": "environment-absence"
      },
      {
        "package": "internal/mail/exec",
        "test": "TestMCPMailReplyMissingOriginalIsNotFound",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
        "reason": "mcp_conformance_test.go:80: MCP mail script not found: file does not exist",
        "class": "environment-absence"
      },
      {
        "package": "internal/molecule",
        "test": "TestBuildRecipeApplyPlanBugReportFlowV2",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-46cac984b12b94b66cce.log",
        "reason": "molecule_test.go:30: tooling formula not present: stat /home/ubuntu/tooling/formulas/mol-bug-report-flow-v2.formula.toml: permission denied",
        "class": "other"
      },
      {
        "package": "internal/orders",
        "test": "TestCheckTriggerConditionKillsProcessGroupAfterWaitDelay",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-ff9cab7068d884a7ea3a.log",
        "reason": "triggers_test.go:495: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "internal/orders",
        "test": "TestCheckTriggerConditionKillsProcessGroupOnTimeout",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-ff9cab7068d884a7ea3a.log",
        "reason": "triggers_test.go:464: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "internal/pidutil",
        "test": "TestAliveWithStartTime_UnreadableIdentityKeepsAliveAnswer",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-cc30e270515707553560.log",
        "reason": "starttime_portable_test.go:86: on linux /proc answers directly, so a ps stub cannot make the identity unreadable",
        "class": "other"
      },
      {
        "package": "internal/pidutil",
        "test": "TestCmdline_FailsClosedWhenUnreadable",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-cc30e270515707553560.log",
        "reason": "cmdline_portable_test.go:141: PID 1 argv is readable in this environment; cannot exercise the fail-closed path this way",
        "class": "other"
      },
      {
        "package": "internal/processgroup",
        "test": "TestTerminateCommandPreservesGroupFailureAfterDirectKill",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-464b529be21ea4ef14f5.log",
        "reason": "processgroup_unix_test.go:71: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "internal/runproj",
        "test": "TestRegenerateGoldens",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-9cf54403b0e740f317fc.log",
        "reason": "golden_regen_test.go:15: set RUNPROJ_UPDATE_GOLDENS=1 to rewrite testdata goldens",
        "class": "other"
      },
      {
        "package": "internal/runtime/acp",
        "test": "TestACPConformance/FencedInput_AbsentSessionRefuses",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-cc430839a756656daa1e.log",
        "reason": "conformance.go:507: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/acp",
        "test": "TestACPConformance/FencedInput_MatchingFenceDelivers",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-cc430839a756656daa1e.log",
        "reason": "conformance.go:472: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/acp",
        "test": "TestACPConformance/FencedInput_MismatchedFenceRefusesEveryInput",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-cc430839a756656daa1e.log",
        "reason": "conformance.go:485: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/acp",
        "test": "TestACPDefaultDirConformance/FencedInput_AbsentSessionRefuses",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-cc430839a756656daa1e.log",
        "reason": "conformance.go:507: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/acp",
        "test": "TestACPDefaultDirConformance/FencedInput_MatchingFenceDelivers",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-cc430839a756656daa1e.log",
        "reason": "conformance.go:472: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/acp",
        "test": "TestACPDefaultDirConformance/FencedInput_MismatchedFenceRefusesEveryInput",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-cc430839a756656daa1e.log",
        "reason": "conformance.go:485: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/exec",
        "test": "TestExecConformance/FencedInput_AbsentSessionRefuses",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-02ff57e430fd7218601d.log",
        "reason": "conformance.go:507: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/exec",
        "test": "TestExecConformance/FencedInput_AbsentSessionRefuses",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d1f6abca8430b335d6c5.log",
        "reason": "conformance.go:507: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/exec",
        "test": "TestExecConformance/FencedInput_MatchingFenceDelivers",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-02ff57e430fd7218601d.log",
        "reason": "conformance.go:472: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/exec",
        "test": "TestExecConformance/FencedInput_MatchingFenceDelivers",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d1f6abca8430b335d6c5.log",
        "reason": "conformance.go:472: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/exec",
        "test": "TestExecConformance/FencedInput_MismatchedFenceRefusesEveryInput",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-02ff57e430fd7218601d.log",
        "reason": "conformance.go:485: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/exec",
        "test": "TestExecConformance/FencedInput_MismatchedFenceRefusesEveryInput",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d1f6abca8430b335d6c5.log",
        "reason": "conformance.go:485: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestActivityLive",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
        "reason": "activity_live_test.go:20: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestActivityLive",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
        "reason": "activity_live_test.go:20: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestHerdrConformance",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
        "reason": "conformance_test.go:18: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestHerdrConformance",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
        "reason": "conformance_test.go:18: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestProviderLive",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
        "reason": "provider_live_test.go:15: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestProviderLive",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
        "reason": "provider_live_test.go:15: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestProviderLiveClaudeKindPath",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
        "reason": "kindpath_live_test.go:105: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestProviderLiveClaudeKindPath",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
        "reason": "kindpath_live_test.go:105: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestProviderLiveOccupantSwapKeepsLiveness",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
        "reason": "panebinding_live_test.go:21: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestProviderLiveOccupantSwapKeepsLiveness",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
        "reason": "panebinding_live_test.go:21: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestSessionEventsLive",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
        "reason": "events_live_test.go:40: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestSessionEventsLive",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
        "reason": "events_live_test.go:40: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunLifecycleTestsDoesNotStopUnownedConcurrentStart/suite/Start_ConcurrentDistinctSessions",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:122: concurrent Start(preexisting-concurrent-session): session already exists",
        "class": "other"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/FencedInput_AbsentSessionRefuses",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:507: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/FencedInput_MatchingFenceDelivers",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:472: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/FencedInput_MismatchedFenceRefusesEveryInput",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:485: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Interrupt_ConcurrentDistinctSessions",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:236: Start(skip-start-14): provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/IsRunning_ConcurrentDistinctSessions",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:269: Start(skip-start-18): provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ConcurrentDistinctPrefixes",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:365: Start(skip-start-25): provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_EmptyPrefix",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:347: Start: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ExcludesStopped",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:331: Start: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_FindsSessions",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:292: Start skip-start-21: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_PrefixFiltering",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:311: Start skip-start-22: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_ConcurrentDistinctSessions",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:419: Start(skip-start-30): provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_EmptyNamesReturnsTrue",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:393: Start: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_FalseAfterStop",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:402: Start: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/SharedSession",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:59: Start shared session: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_ConcurrentDistinctSessions",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:122: concurrent Start(skip-start-3): provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_CreatesRunningSession",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:82: Start: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_DuplicateReturnsError",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:91: first Start: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_ConcurrentDistinctSessions",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:207: Start(skip-start-11): provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_AlreadyStopped",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:191: Start: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_NotRunning",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:168: Start sentinel: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_MakesSessionNotRunning",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:132: Start: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestStartOrSkipDoesNotStopSessionAfterClassifiedStartFailure/classified_failure",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance_test.go:147: Start: session already exists",
        "class": "other"
      },
      {
        "package": "internal/runtime/ssh",
        "test": "TestConn_ExecOverRealLocalhost",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-60ebdfafc6cd70feb3b1.log",
        "reason": "ssh_localhost_integration_test.go:24: passwordless ssh to localhost unavailable",
        "class": "other"
      },
      {
        "package": "internal/runtime/ssh",
        "test": "TestSSHConformance/FencedInput_AbsentSessionRefuses",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-60ebdfafc6cd70feb3b1.log",
        "reason": "conformance.go:507: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/ssh",
        "test": "TestSSHConformance/FencedInput_MatchingFenceDelivers",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-60ebdfafc6cd70feb3b1.log",
        "reason": "conformance.go:472: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/ssh",
        "test": "TestSSHConformance/FencedInput_MismatchedFenceRefusesEveryInput",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-60ebdfafc6cd70feb3b1.log",
        "reason": "conformance.go:485: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/subprocess",
        "test": "TestSubprocessDefaultDirSeamConformance/FencedInput_AbsentSessionRefuses",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-74db6ddfc6b7303645d2.log",
        "reason": "conformance.go:507: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/subprocess",
        "test": "TestSubprocessDefaultDirSeamConformance/FencedInput_MatchingFenceDelivers",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-74db6ddfc6b7303645d2.log",
        "reason": "conformance.go:472: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/subprocess",
        "test": "TestSubprocessDefaultDirSeamConformance/FencedInput_MismatchedFenceRefusesEveryInput",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-74db6ddfc6b7303645d2.log",
        "reason": "conformance.go:485: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/subprocess",
        "test": "TestSubprocessSeamConformance/FencedInput_AbsentSessionRefuses",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-74db6ddfc6b7303645d2.log",
        "reason": "conformance.go:507: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/subprocess",
        "test": "TestSubprocessSeamConformance/FencedInput_MatchingFenceDelivers",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-74db6ddfc6b7303645d2.log",
        "reason": "conformance.go:472: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/subprocess",
        "test": "TestSubprocessSeamConformance/FencedInput_MismatchedFenceRefusesEveryInput",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-74db6ddfc6b7303645d2.log",
        "reason": "conformance.go:485: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestCleanupOrphanedSessions",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
        "reason": "tmux_test.go:1615: Skipping: GT_TEST_ALLOW_CLEANUP_TEST=1 required (this test kills sessions)",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestCleanupOrphanedSessions_NoSessions",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
        "reason": "tmux_test.go:1713: Skipping: GT_TEST_ALLOW_CLEANUP_TEST=1 required (this test kills sessions)",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestGetKeyBinding_CapturesUserBinding",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
        "reason": "tmux_test.go:3663: not inside tmux \u2014 need server for bind-key",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestGetKeyBinding_SkipsGasTownBindings",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
        "reason": "tmux_test.go:3637: not inside tmux \u2014 need server for bind-key",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestIsGTBinding_DetectsGasTownBindings",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-15c091b746137e5c2921.log",
        "reason": "tmux_test.go:3689: not inside tmux \u2014 need server for bind-key",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestNudgeConfirmBudgetThreshold",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
        "reason": "nudge_confirm_lag_integration_test.go:174: set GC_TMUX_INTEGRATION=1 to run this real-tmux measurement",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestNudgeConfirmBusyRenderLag",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
        "reason": "nudge_confirm_lag_integration_test.go:36: set GC_TMUX_INTEGRATION=1 to run this real-tmux reproduction (spins a throwaway tmux server)",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestNudgePokeRealTmux",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
        "reason": "nudge_poke_integration_test.go:27: set GC_TMUX_INTEGRATION=1 to run this real-tmux dogfood (spins a throwaway tmux server)",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestNudgeRecoversStagedDraftSwallowedByPasteIngest",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
        "reason": "nudge_staged_draft_integration_test.go:26: set GC_TMUX_INTEGRATION=1 to run this real-tmux reproduction (spins a throwaway tmux server)",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestPokeActivityRealTmux",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
        "reason": "activity_poke_integration_test.go:21: set GC_TMUX_INTEGRATION=1 to run this real-tmux dogfood (spins a throwaway tmux server)",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestPokeActivityRealTmux",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-ef2749222a71266756ae.log",
        "reason": "activity_poke_integration_test.go:21: set GC_TMUX_INTEGRATION=1 to run this real-tmux dogfood (spins a throwaway tmux server)",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestProcessSnapshotPSArgsRejectsLinuxSyntaxOnDarwin",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
        "reason": "state_cache_test.go:926: Darwin-specific syntax guard",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestProcessSnapshotPSArgsRejectsLinuxSyntaxOnDarwin",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-4bcc997199c6a7426526.log",
        "reason": "state_cache_test.go:926: Darwin-specific syntax guard",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestSetBindings_PreserveFallbackOnRepeatedCalls",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
        "reason": "tmux_test.go:3719: not inside tmux \u2014 need server for bind-key",
        "class": "environment-absence"
      },
      {
        "package": "internal/sessionlog",
        "test": "TestFindClaudeLatestSessionFileUsesProjectPathAlias",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
        "reason": "sessionlog_test.go:1397: macOS-only /tmp <-> /private/tmp Claude project path alias",
        "class": "other"
      },
      {
        "package": "internal/sessionlog",
        "test": "TestFindCodexSessionFileMatchesEquivalentResolvedWorkDir",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
        "reason": "sessionlog_test.go:2170: macOS /private/tmp path aliases only apply on darwin",
        "class": "other"
      },
      {
        "package": "internal/sessionlog",
        "test": "TestFindGeminiSessionFileMatchesEquivalentResolvedWorkDir",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
        "reason": "sessionlog_test.go:2315: macOS-only /tmp <-> /private/tmp Gemini project path alias",
        "class": "other"
      },
      {
        "package": "internal/sessionlog",
        "test": "TestFindSessionFileByIDPrefersStoredWorkDirSpelling",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
        "reason": "sessionlog_test.go:1104: macOS-only /tmp <-> /private/tmp Claude project path alias",
        "class": "other"
      },
      {
        "package": "internal/sessionlog",
        "test": "TestFindSessionFileByIDUsesClaudeProjectPathAlias",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
        "reason": "sessionlog_test.go:1084: macOS-only /tmp <-> /private/tmp Claude project path alias",
        "class": "other"
      },
      {
        "package": "internal/sessionlog",
        "test": "TestFindSessionFileUsesClaudeProjectPathAlias",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
        "reason": "sessionlog_test.go:1195: macOS-only /tmp <-> /private/tmp Claude project path alias",
        "class": "other"
      },
      {
        "package": "internal/sessionlog",
        "test": "TestFindSessionFileUsesNewestClaudeProjectPathAliasMatch",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
        "reason": "sessionlog_test.go:1215: macOS-only /tmp <-> /private/tmp Claude project path alias",
        "class": "other"
      },
      {
        "package": "internal/worker/transcript",
        "test": "TestDiscoverFallbackPathUsesNewestClaudeLatestSessionAcrossAliases",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-5536c7e35ba99d4d52dd.log",
        "reason": "discovery_test.go:65: macOS-only /tmp <-> /private/tmp Claude project path alias",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/antigravity/tmux-cli/WC-TX-USAGE-001",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/antigravity/tmux-cli/WC-USAGE-COST-001",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/cursor/tmux-cli/WC-TX-USAGE-001",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/cursor/tmux-cli/WC-USAGE-COST-001",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/gemini/tmux-cli/WC-TX-USAGE-001",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/gemini/tmux-cli/WC-USAGE-COST-001",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/kimi/tmux-cli/WC-TX-USAGE-001",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/kimi/tmux-cli/WC-USAGE-COST-001",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/mimocode/tmux-cli/WC-TX-USAGE-001",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/mimocode/tmux-cli/WC-USAGE-COST-001",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/opencode/tmux-cli/WC-TX-USAGE-001",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/opencode/tmux-cli/WC-USAGE-COST-001",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/pi/tmux-cli/WC-TX-USAGE-001",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/pi/tmux-cli/WC-USAGE-COST-001",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/zcode/tmux-cli/WC-TX-USAGE-001",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/zcode/tmux-cli/WC-USAGE-COST-001",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestStructuredConformance/gemini/tmux-cli/WC-STRUCT-003",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "structured_conformance_test.go:153: profile fixture has no edit result to evaluate",
        "class": "other"
      },
      {
        "package": "internal/workspacesvc",
        "test": "TestProxyProcessHardExitHarness",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-5a7df874ab90d74e027a.log",
        "reason": "proxy_process_test.go:1160: harness process",
        "class": "other"
      },
      {
        "package": "internal/workspacesvc",
        "test": "TestProxyProcessHelper",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-5a7df874ab90d74e027a.log",
        "reason": "proxy_process_test.go:167: helper process",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestBeadsInitTopologyMatrix/M5-legacy-gc-managed",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/acceptance-00f787a1201c0c97f5dd.log",
        "reason": "beads_topology.go:632: there is no pre-journal gc binary; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc built from a commit before the ownership journal",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestBeadsInitTopologyMatrix/M5-legacy-gc-managed",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/acceptance-f785906030efd99733e1.log",
        "reason": "beads_topology.go:632: there is no pre-journal gc binary; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc built from a commit before the ownership journal",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestBeadsInitTopologyMatrix/M5-legacy-gc-managed",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/acceptance-2129301bc9bf3acdcdf7.log",
        "reason": "beads_topology.go:632: there is no pre-journal gc binary; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc built from a commit before the ownership journal",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestBeadsMigrateLegacyCityToProxied",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/acceptance-2b4464fe973ad362994d.log",
        "reason": "beads_migrate_proxied_test.go:135: there is no legacy gc available; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc that still initialises the GC-managed direct topology",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestBeadsMigrateProxiedRefusesLiveLegacyServer",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/acceptance-a4ee7fceff2c46ffacdd.log",
        "reason": "beads_migrate_proxied_test.go:357: there is no legacy gc available; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc that still initialises the GC-managed direct topology",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestPackRegistryLiveImportsEveryCatalogPack",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/acceptance-5ed75e6deb5279ca2b84.log",
        "reason": "pack_registry_live_test.go:38: set GC_TEST_GASCITY_PACKS_REGISTRY to a gascity-packs registry.toml source to run this live import smoke test",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestSelfhostUX_BinaryDriftPostMergePlaceholder",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/acceptance-56dbf54bd097e3e21771.log",
        "reason": "integration_selfhost_ux_test.go:112: placeholder for ga-xxqx",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestSelfhostUX_BinaryDriftRestart",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/acceptance-fedaecec6af3ef2e4157.log",
        "reason": "integration_selfhost_ux_test.go:54: pending merge of builder/ga-xxqx-1 (PR#2219): drift auto-restart not on origin/main",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestSelfhostUX_Composed",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/acceptance-5ed75e6deb5279ca2b84.log",
        "reason": "integration_selfhost_ux_test.go:96: placeholder for ga-q0bf.1, ga-r8iz.1, ga-6wrr.1, and post-PR#2219 assertions",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestSelfhostUX_MigrationGuideURLPlaceholder",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/acceptance-61bd7bf3b62120f8de67.log",
        "reason": "integration_selfhost_ux_test.go:108: placeholder for ga-6wrr.1",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestSelfhostUX_PackV1V2Collision",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/acceptance-fd5aed82a8b7ea468264.log",
        "reason": "integration_selfhost_ux_test.go:16: pending ga-cwdaqv: supervised gc start does not validate config pre-registration",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestSelfhostUX_StopBypassDetailsPlaceholder",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/acceptance-b54423ef39aa7297e2c2.log",
        "reason": "integration_selfhost_ux_test.go:104: placeholder for ga-r8iz.1",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestSelfhostUX_StructuredStartSummaryPlaceholder",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/acceptance-53f66fa06e91bd4169e5.log",
        "reason": "integration_selfhost_ux_test.go:100: placeholder for ga-q0bf.1",
        "class": "other"
      },
      {
        "package": "test/acceptance/helpers",
        "test": "TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
        "reason": "bd_schema_parity_test.go:121: bd not available",
        "class": "environment-absence"
      },
      {
        "package": "test/acceptance/helpers",
        "test": "TestBdLatestSchemaVersionLeavesNothingInItsTempRoot",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
        "reason": "bd_schema_parity_test.go:144: bd not available",
        "class": "environment-absence"
      },
      {
        "package": "test/acceptance/helpers",
        "test": "TestRealBdProvisionLeavesTheHostHomeAlone",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
        "reason": "tool_home_test.go:315: no bd available (set GC_ACCEPTANCE_BD_BIN)",
        "class": "environment-absence"
      },
      {
        "package": "test/containerhost",
        "test": "TestK8sSessionConformance/FencedInput_AbsentSessionRefuses",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-afcc08552b3653c0e0f1.log",
        "reason": "conformance.go:507: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "test/containerhost",
        "test": "TestK8sSessionConformance/FencedInput_MatchingFenceDelivers",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-afcc08552b3653c0e0f1.log",
        "reason": "conformance.go:472: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "test/containerhost",
        "test": "TestK8sSessionConformance/FencedInput_MismatchedFenceRefusesEveryInput",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-packages-afcc08552b3653c0e0f1.log",
        "reason": "conformance.go:485: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestBdStoreConformance",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-full-fc69bfc5c61515cd4436.log",
        "reason": "bdstore_test.go:55: bd 1.0.4 trips the silent-fallback guard (pre-existing, non-regression; pinned to avoid 1.0.5 corruption) \u2014 ga-e7z613",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestDoltPersistence_AssigneeAndInProgressSurviveSubsequentBdWrite",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-full-e914b9bbd3c7222e0ecf.log",
        "reason": "dolt_persistence_test.go:535: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestDoltPersistence_CloseStatusSurvivesSubsequentBdWrite",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-full-c6460b0f70fa79cef8f7.log",
        "reason": "dolt_persistence_test.go:448: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestDoltPersistence_ConcurrentMetadataWriteBurstTimingVisibility",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-full-fc69bfc5c61515cd4436.log",
        "reason": "dolt_persistence_test.go:817: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestDoltPersistence_CrossBeadMetadataWritesPersistInDolt",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-full-c02db3e138643d8a1b39.log",
        "reason": "dolt_persistence_test.go:765: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestDoltPersistence_HandoffRoutingMetadataSurvivesSubsequentBdWrite",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-full-f7217e413eb188f8ffa6.log",
        "reason": "dolt_persistence_test.go:635: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestDoltPersistence_InProgressStatusSurvivesSubsequentBdWrite",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-full-c8d8bacbe500deacd942.log",
        "reason": "dolt_persistence_test.go:678: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestDoltPersistence_MetadataSurvivesSubsequentBdWrites",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-full-c9e44143ff02c6cd1f9f.log",
        "reason": "dolt_persistence_test.go:489: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestDoltPersistence_SequentialMetadataWritesAccumulateInDolt",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-full-a8d16ec096a75542e5fb.log",
        "reason": "dolt_persistence_test.go:721: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestDoltPersistence_SessionAwakeMetadataSurvivesSubsequentBdWrite",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-full-caf9552c6795b8d49441.log",
        "reason": "dolt_persistence_test.go:584: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestHumaBinary_CityUnregisterAsync",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-full-caf9552c6795b8d49441.log",
        "reason": "huma_binary_test.go:506: flaky on CI under load \u2014 5s supervisor shutdown budget races with in-flight mayor-session-start; tracked in #2090",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestReadProcessSnapshotFallsBackToPSAndFindsRealProcesses",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-full-c6460b0f70fa79cef8f7.log",
        "reason": "integration_test.go:2804: host has /proc; this test targets the no-/proc (macOS) fallback path",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestStartDrift_PermissionDenied_DescriptiveError",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-full-1f156f3a859a93c84364.log",
        "reason": "start_drift_test.go:275: permission-denied scenario requires root to drop to a secondary uid",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestStartDrift_SystemdManaged_RestartsToNewBuildID",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/integration-full-fc69bfc5c61515cd4436.log",
        "reason": "start_drift_test.go:123: XDG_RUNTIME_DIR not set; user systemd not addressable",
        "class": "environment-absence"
      },
      {
        "package": "test/tmuxtest",
        "test": "TestHelperHoldSentinel",
        "status": "SKIP",
        "log": "/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/logs/unit-7989cb1ae7bd292f8cbf.log",
        "reason": "orphan_sweep_sigkill_test.go:29: not invoked as a subprocess helper",
        "class": "other"
      }
    ]
  },
  "test_cmd": [
    "test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-h1d3toc.c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"
  ],
  "test_counts": {
    "PASS": 115586,
    "FAIL": 0,
    "SKIP": 369
  },
  "diff_tests_executed": "diff_tests_executed: none (no diff-owned tests)\nGATE_TEST_EVIDENCE_RESULT ok: the diff owns no test\n"
}
```

</details>

The referenced docs/PROJECT_MANIFEST.md is absent from this repository. The supplied seven criteria, TESTING.md and current Bazel CI define this gate. Tracker ga-v1wkoqh remains open until the remedy lands; this gate does not close or credit any held deploy.
