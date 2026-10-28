# Test-owned Dolt server port ownership release gate

**Verdict:** **PASS**

Evaluated at 2026-10-25T08:14:52.633426+00:00. Deploy ga-j1ootdt; build ga-tldkpq; exact-head full review ga-1l7caqi.

Fresh gate after the prerequisite seat-hook fix landed. Earlier held suite results supply no current gate credit. The isolated deploy branch is cut at the reviewed source; only this gate record is committed above it.

source_commit: c15e50cfbbe714a974df5d79add6611170406904

deploy_branch: deploy/ga-j1ootdt-gate

base_ref: origin/main

frozen_base_sha: 5c28b43e2f0f7baf37914b939175df255ce9b65e

gate_base_kind: current-tip

merge_state: clean (origin/main@5c28b43e2f0f7baf37914b939175df255ce9b65e)

gate_merge_head: 5d6a5975c6978c47d449b31c58d168285235a086

gate_merge_tree: 75f77eb9beb41b8eeca4963d77331efc3aefa5d5

deploy_mode: remote

test_cmd_scope: full-suite

test_exec_mode: remote-exec

test_log_dir: /var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs

native_floor_log_dir: /var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/floor-logs

heavy_mode: none

waiver_ref: none; Gas City has no waiver path

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Reviewer ga-1l7caqi fully reviewed exact source, including the event-flush environment change and its test; live mayor KEEP scope decision verified. |
| 2 | Acceptance criteria met | PASS | Eight test-owned server launchers share bounded ownership checking, lost-port retry and owned-process cleanup. All nine owned tests PASS by name, including both held-port cases with real Dolt. Architect replaced unexecuted netns stress with this landing gate and a post-landing observation, specified below; no stress figures receive measured credit. |
| 3 | Full-scope tests and required checks | PASS | All five complete fresh remote phases plus native floor, independent BEP/raw-artifact/by-name/skip audit PASS. |
| 4 | No unresolved HIGH findings | PASS | Exact-head full reviewer PASS and live scope acceptance checked. Fallback listener identity is approximate on hosts without socket tables, as reviewed. Observation duties remain open. |
| 5 | Final branch clean | PASS | Immutable scratch HEAD/tree/parents and clean status independently checked. Publication separately verifies exact reviewed source, only one gate-file commit above it, branch guards, normal hooks and final clean tree. |
| 6 | Merge state and build/vet | PASS | Clean merge against the frozen landed base; normal commit hook, full //... build, integration compilation, vet/nogo and check-hooks PASS. Merge authority tests the eventual merge group. |
| 7 | Single feature theme | PASS | One test-owned Dolt server ownership/retry fix with required package wiring, integration shard-budget entry and generated resource counts; no unrelated implementation theme. |

Fast lanes: four independent fresh pinned-base views; format, hooks, full lint/nogo, shell/policy guards, BUILD regeneration with empty tracked/untracked diff, seven generated-artifact checks, and complexity measurements. All lane contracts passed before C3 runtime setup. Raw complexity returns are recorded verbatim; pinned-base zero growth does not claim raw rc0.

policy_attribution: make complexity-check -> ga-q7p6is for the unchanged red pinned base, with same-run measurements and no worse row or new/regressed diff line under architect ga-41e6k8r.

Acceptance details: the helper permits five attempts, polls each at 25 ms for at most 60 s, compares the listener PID where readable, and otherwise requires a dial plus child survival for 500 ms. It fails closed on non-port startup exits and reports the log tail. Stop sends SIGTERM then SIGKILL after 10 s. Eight migrated callers preserve their query/readiness behavior and wait on the helper-owned Exited channel instead of racing Wait. The source adds a 37th integration package target and 86th test action, updating its budget pin and generated resource inventory.

Reviewed scope addition: test/dolttest/sqlserver.go appends DOLT_DISABLE_EVENT_FLUSH=true using inherited or cloned explicit environment, covered by TestStartSQLServerDisablesDoltEventFlush. Added during rebase-only round 2 outside the original exit contract; reviewed fully in ga-1l7caqi and explicitly accepted by mayor because it prevents detached metrics writes racing held-port-test cleanup. Published PR wording must retain that disclosure.

Architect exit contract: netns-run.sh fixed 3000 4 was NOT EXECUTED / NOT MET and no synthetic host load was generated. Historical stress counts are builder-reported, unverified. Once the fix actually lands on main, record WINDOW START <landing sha> <utc> on open, unslung ga-woq0zj. Deployer closes it only after ga-th9ewjz evaluates >=22 calendar days, >=150 full-suite deploy gates based on that landing, and zero matching gascity test-helper startup port-loss sightings. Bd subprocess startup failures do not count. A matching helper sighting records log and callsite, gets a helper fix, and resets the window to that fix landing. Neither this PASS nor an open PR starts or completes the window.

Canonical full scope: unit make check; Bazel acceptance including acceptance_solo_tests; integration-packages; full integration excluding exactly gate-latency-tests.txt; separate clear-probe latency phase containing exactly that registry. All force --nocache_test_results and --noremote_local_fallback. Native floor independently covers every environment-absence parent, beadstest, Darwin compile and runner self-tests. The isolation wrapper runs in PASS-THROUGH mode for this scratch path, so no cloned isolation is claimed; the caller scrubs live GC/BD/BEADS/DOLT selectors and the native floor uses the documented clean test environment. The five farm phases are Bazel remote actions.

test_cmd:

```json
[
  "test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
  "test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
  "test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
  "test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
  "test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"
]
```

test_counts:

```json
{
  "PASS": 126495,
  "FAIL": 0,
  "SKIP": 370
}
```

test_exec_bep:

```json
{
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
    "results": 86,
    "remote_executed": 86,
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
}
```

rbe_state:

```json
{
  "rc_marker_present": true,
  "kill_switch_absent": true,
  "cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT",
  "worker_env_tree": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45",
  "worker_env_origin_main": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45"
}
```

fallback_reason:

```json
null
```

admission:

```json
{
  "class": "remote",
  "floor_gib": 25.0,
  "free_gib": 60.883087158203125,
  "waited_seconds": 0.002,
  "cap": 2
}
```

farm_test_seconds:

```json
10247.959
```

owned_topup:

```json
[]
```

floor_topup:

```json
{
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
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-c2ab4e7d4735e1bd66a7.log",
          "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestReplay",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
          "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSessionEventPumpLiveHerdr",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-821a75e4a503601af052.log",
          "reason": "session_event_pump_live_test.go:23: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSessionEventPumpLiveHerdr",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-493754ea2d5b4d8858c8.log",
          "reason": "session_event_pump_live_test.go:23: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSimCoverageGate",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-514989cc035c0e9a8ff5.log",
          "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSimCoverageGate",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-637c74c578a2b17fc654.log",
          "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-568812e4d525bdd1f974.log",
          "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
          "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-75f7330b9b673df65e4f.log",
          "reason": "tmux_leak_guard_mntns_test.go:98: bwrap not installed; cannot build a private-/tmp mount namespace",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-752a0bf25b15b465517a.log",
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
      "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/floor-logs/floor-0.log",
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
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-be91eec86b4dbf7e66f1.log",
          "reason": "gastown_test.go:4153: zsh not installed",
          "class": "environment-absence"
        },
        {
          "package": "examples/gastown",
          "test": "TestAttachedRigScopeShellToken/zsh",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-ef24b9dbe59f5d04fac1.log",
          "reason": "gastown_test.go:4153: zsh not installed",
          "class": "environment-absence"
        },
        {
          "package": "examples/gastown",
          "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-ff9cb121652191f033f5.log",
          "reason": "maintenance_scripts_test.go:9254: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
          "class": "environment-absence"
        },
        {
          "package": "examples/gastown",
          "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-29937721dc700c4b8d8b.log",
          "reason": "maintenance_scripts_test.go:9254: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
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
      "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/floor-logs/floor-1.log",
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
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-8272c8197ebafcf5385f.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/BatchClearsOneKeyKeepsAnother",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-364160de6f2f808aa619.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-8272c8197ebafcf5385f.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-364160de6f2f808aa619.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-8272c8197ebafcf5385f.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-364160de6f2f808aa619.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-8272c8197ebafcf5385f.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-364160de6f2f808aa619.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestProbeVersionsAgainstHostBinaries/bd",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-ea4511196531bb4b8c55.log",
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
      "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/floor-logs/floor-2.log",
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
      "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/floor-logs/floor-3.log",
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
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-8544388e16a0d6b0cdb0.log",
          "reason": "migrate_journal_test.go:37: pinned beads v1.3.2 is not unpacked in the local module cache: open /tmp/bt/_tmp/35731ca362c311940500020a6c8a2007/go/pkg/mod/github.com/steveyegge/beads@v1.3.2/cmd/bd/migrate_dolt_mode.go: no such file or directory",
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
      "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/floor-logs/floor-4.log",
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
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-64046beff95fb28ccd66.log",
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
      "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/floor-logs/floor-5.log",
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
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
          "reason": "checks_custom_types_test.go:467: bd binary not on PATH",
          "class": "environment-absence"
        },
        {
          "package": "internal/doctor",
          "test": "TestCustomTypesCheck_TableDrift",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
          "reason": "checks_custom_types_test.go:297: bd binary not on PATH",
          "class": "environment-absence"
        },
        {
          "package": "internal/doctor",
          "test": "TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
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
      "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/floor-logs/floor-6.log",
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
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:159: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailConformance",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:26: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailCrossPodNameResolution",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:195: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailProjectKeyIsolation",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:286: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailReplyMissingOriginalIsNotFound",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
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
      "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/floor-logs/floor-7.log",
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
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "activity_live_test.go:20: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestActivityLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "activity_live_test.go:20: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestHerdrConformance",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "conformance_test.go:18: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestHerdrConformance",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "conformance_test.go:18: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "provider_live_test.go:15: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "provider_live_test.go:15: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveClaudeKindPath",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "kindpath_live_test.go:105: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveClaudeKindPath",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "kindpath_live_test.go:105: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveOccupantSwapKeepsLiveness",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "panebinding_live_test.go:21: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveOccupantSwapKeepsLiveness",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "panebinding_live_test.go:21: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestSessionEventsLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "events_live_test.go:40: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestSessionEventsLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
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
      "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/floor-logs/floor-8.log",
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
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:238: Start(skip-start-14): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/IsRunning_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:271: Start(skip-start-18): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ConcurrentDistinctPrefixes",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:367: Start(skip-start-25): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_EmptyPrefix",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:349: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ExcludesStopped",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:333: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_FindsSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:294: Start skip-start-21: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_PrefixFiltering",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:313: Start skip-start-22: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:421: Start(skip-start-30): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_EmptyNamesReturnsTrue",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:395: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_FalseAfterStop",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:404: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/SharedSession",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:61: Start shared session: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:124: concurrent Start(skip-start-3): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_CreatesRunningSession",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:84: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_DuplicateReturnsError",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:93: first Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:209: Start(skip-start-11): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_AlreadyStopped",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:193: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_NotRunning",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:170: Start sentinel: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_MakesSessionNotRunning",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:134: Start: provider environment unavailable",
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
      "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/floor-logs/floor-9.log",
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
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
          "reason": "tmux_test.go:3663: not inside tmux \u2014 need server for bind-key",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/tmux",
          "test": "TestGetKeyBinding_SkipsGasTownBindings",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
          "reason": "tmux_test.go:3637: not inside tmux \u2014 need server for bind-key",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/tmux",
          "test": "TestIsGTBinding_DetectsGasTownBindings",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-15c091b746137e5c2921.log",
          "reason": "tmux_test.go:3689: not inside tmux \u2014 need server for bind-key",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/tmux",
          "test": "TestSetBindings_PreserveFallbackOnRepeatedCalls",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
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
      "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/floor-logs/floor-10.log",
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
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
          "reason": "bd_schema_parity_test.go:121: bd not available",
          "class": "environment-absence"
        },
        {
          "package": "test/acceptance/helpers",
          "test": "TestBdLatestSchemaVersionLeavesNothingInItsTempRoot",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
          "reason": "bd_schema_parity_test.go:144: bd not available",
          "class": "environment-absence"
        },
        {
          "package": "test/acceptance/helpers",
          "test": "TestRealBdProvisionLeavesTheHostHomeAlone",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
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
      "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/floor-logs/floor-11.log",
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
          "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs/integration-full-0c1c194fca4897b865f3.log",
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
      "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/floor-logs/floor-12.log",
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
      "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/floor-logs/floor-13.log",
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
      "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/floor-logs/floor-14.log",
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
      "log": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/floor-logs/floor-15.log",
      "exit_code": 0
    }
  ],
  "log_dir": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/floor-logs"
}
```

latency_leg:

```json
{
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
      "farm_test_seconds": 85.889,
      "evidence_errors": [],
      "probe": [
        "GATE_FARM_PROBE waiting waited=0s load1=56.41 load5=46.59 load15=21.54 cpus=32 threshold=50 max_wait=600s",
        "GATE_FARM_PROBE verdict=clear waited=10s load1=47.74 load5=45.06 load15=21.31 cpus=32 threshold=50 max_wait=600s"
      ],
      "test": [
        "stores: city proxied-server, rig proxied-server; cpus 32; loadavg 31.10 41.29 20.81 6/977 50848"
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
}
```

load_threshold: 15

load_waited_seconds: 1802

load_wait_timed_out: 1

run_start_load: 73.13

run_max_load: 70.53

run_mean_load: 61.38

run_readings: 9

Observed load summaries:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1802 wait_timed_out=1 run_start_load=73.13 run_max_load=70.53 run_mean_load=61.38 run_readings=9 wait_first_load=56.00 wait_max_load=105.86 wait_mean_load=70.43 wait_readings=61 read_errors=0
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=96.41 run_max_load=124.30 run_mean_load=74.27 run_readings=103 wait_first_load=96.41 wait_max_load=96.41 wait_mean_load=96.41 wait_readings=1 read_errors=0
```

skip_inventory: 395 events including subtests across 268 unchanged sites; none unresolved. Shared-hunk reasoning for any unchanged test in a changed file is retained below. Official test_counts sums top-level results per job/log. Every environment-absence package was topped up; remaining optional/provider/input/platform skips are retained with their raw reasons in skip-inventory.json and floor-skip-inventory.json. No owned SKIP or FAIL.

ci_lane_run: n/a (no CI-config changes). Runtime socket and pinned Dolt image checked before C3; native floor uses pinned bd and scrubbed live-store selectors. ICU74 evidence:

```text
GATE_ICU74 lib=libicui18n.so.74 file="/usr/local/lib64/icu74-compat/libicui18n.so.74.2" sha256=d90d62f0d7b6c6fbc279ae5e433cc7004aa47cdb106a71a02350099f8df9ae51 recorded=yes
GATE_ICU74 lib=libicuuc.so.74 file="/usr/local/lib64/icu74-compat/libicuuc.so.74.2" sha256=33499dd97a5393926b32e0b535f5340f7dae14b35e59025b572e6bd9ad48a2ae recorded=yes
GATE_ICU74 lib=libicudata.so.74 file="/usr/local/lib64/icu74-compat/libicudata.so.74.2" sha256=c891c5fdb45e3cf756b728abe256be7af1bfc97f00ca2f2fac450c96c09bd4b7 recorded=yes
GATE_ICU74 verdict=OK provenance=recorded sonames="libicui18n.so.74 libicuuc.so.74 libicudata.so.74"
```

Acceptance source and PR proof:

```json
{
  "source": "c15e50cfbbe714a974df5d79add6611170406904",
  "base": "5c28b43e2f0f7baf37914b939175df255ce9b65e",
  "merge": "5d6a5975c6978c47d449b31c58d168285235a086",
  "pre_suite_verdict": "PASS",
  "candidate_green_verdict": "PASS",
  "migrated_helpers": {
    "cmd/gc/dolt_project_id_test.go": "startRealDoltServer",
    "examples/bd/dolt/compact_real_dolt_test.go": "startRealDoltServerForCompactTest",
    "examples/gastown/dolt_orphan_sweep_integration_test.go": "startSweepDoltServer",
    "examples/gastown/maintenance_bd_integration_test.go": "ensureServer",
    "examples/gastown/maintenance_scripts_dolt_integration_test.go": "startDoltServerForMaintenanceTest",
    "internal/beads/native_dolt_store_integration_test.go": "startTestDoltServer",
    "test/integration/bdstore_test.go": "startSharedDoltServer",
    "test/integration/dolt_config_test.go": "startDoltServerOnAllInterfaces"
  },
  "bounded_ownership_retry_and_cleanup": true,
  "owned_tests": 9,
  "required_tests": 10,
  "scope_authorization": "Live mayor KEEP decision on deploy bead; full exact-head review ga-1l7caqi",
  "scope_disclosure_required": "test/dolttest/sqlserver.go event flush environment hunk and TestStartSQLServerDisablesDoltEventFlush entered rebase-only round 2 outside original exit contract; full review; hardens held-port test cleanup",
  "netns_stress": "NOT EXECUTED / NOT MET; architect replacement exit contract applies",
  "observation": "Only after actual main landing: WINDOW START <landing sha> <utc> on open unslung ga-woq0zj; deployer waits for ga-th9ewjz evaluation mail; >=22 days, >=150 full-suite deploy gates with base containing landing, zero matching helper port-loss sightings. Matching helper sighting resets window to fix landing. Bd subprocess servers excluded.",
  "historical_stress_numbers": "builder-reported and unverified; no runtime credit",
  "ci_config_changed": false,
  "scope": "one test-owned Dolt server ownership/retry theme plus reviewed test wiring and generated resource inventory",
  "timestamp": "2026-10-25T07:20:37.089517+00:00",
  "candidate_green_evidence": {
    "source": "c15e50cfbbe714a974df5d79add6611170406904",
    "merge": "5d6a5975c6978c47d449b31c58d168285235a086",
    "full_suite_execution": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/execution.json",
    "by_name": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/final-owned-by-name.txt"
  }
}
```

Complexity measurements:

```json
{
  "base": {
    "rc": 2,
    "stdout": "",
    "stderr": "complexity: changes relative to baseline:\nregressed: 625 main reconcileSessionBeadsTracedWithNamedDemand cmd/gc/session_reconciler.go (baseline 614)\nregressed: 139 config LoadWithIncludesOptions internal/config/compose.go (baseline 138)\nregressed: 97 beads (*CachingStore).applyEvent internal/beads/caching_store_events.go (baseline 95)\nregressed: 90 main (*CityRuntime).reloadConfigTraced cmd/gc/city_runtime.go (baseline 86)\nregressed: 88 main resolveTemplate cmd/gc/template_resolve.go (baseline 87)\nregressed: 64 opjournal planRules internal/opjournal/lane_plan.go (baseline 60)\nregressed: 61 main startOneCity cmd/gc/cmd_supervisor.go (baseline 60)\nregressed: 57 main doPrimeWithHookFormatOpts cmd/gc/cmd_prime.go (baseline 53)\nregressed: 53 main cmdNudgeDrainWithFormat cmd/gc/cmd_nudge.go (baseline 45)\nregressed: 53 materialize Run internal/materialize/skills.go (baseline 44)\nnew: 42 beads (*CachingStore).RefreshRowReport internal/beads/caching_store_reads.go\nregressed: 41 main cmdHookWithOptions cmd/gc/cmd_hook.go (baseline 40)\nnew: 39 api (*Server).acceptSessionCreate internal/api/huma_handlers_sessions_command.go\nnew: 39 session (*Manager).StopTurnWith internal/session/stop_turn_with.go\nregressed: 37 main tryDeliverQueuedNudgesByPoller cmd/gc/cmd_nudge.go (baseline 32)\nnew: 37 worker OperatorKill internal/worker/operator_kill.go\nregressed: 36 dispatch expandDrain internal/dispatch/drain.go (baseline 33)\nregressed: 35 main (*hookClaimOps).applyDefaults cmd/gc/cmd_hook_claim.go (baseline 31)\nregressed: 35 opjournal (*takePlan).planContent internal/opjournal/lane_plan.go (baseline 33)\nnew: 34 main initAndHookDirResolvingDatabase cmd/gc/beads_provider_lifecycle.go\nnew: 34 main startBeadsLifecycleResolvingCityDatabase cmd/gc/beads_provider_lifecycle.go\nregressed: 34 tmux (*Tmux).nudgeSession internal/runtime/tmux/tmux.go (baseline 28)\nnew: 34 tmux closeClaudeBackgroundShells internal/runtime/tmux/claude_background_stop.go\nregressed: 32 config mergeFragment internal/config/compose.go (baseline 27)\nregressed: 32 opjournal (*ConversationLane).Pass internal/opjournal/lane.go (baseline 24)\nnew: 31 main buildDesiredStateWithSessionBeadsAtOpts cmd/gc/build_desired_state.go\nregressed: 31 main parseBdByIDUpdateArgs cmd/gc/cmd_bd_by_id.go (baseline 30)\nnew: 30 opjournal (*rulesPlan).planNowInterrupt internal/opjournal/lane_now.go\nregressed: 30 sessionlog ReadCodexFile internal/sessionlog/codex_reader.go (baseline 23)\nregressed: 29 main doSessionWake cmd/gc/cmd_session_wake.go (baseline 27)\nnew: 28 contract resolveDoltConnectionTarget internal/beads/contract/connection.go\nnew: 28 main cmdSessionSubmit cmd/gc/cmd_session.go\nregressed: 28 main runNudgeBackstop cmd/gc/nudge_backstop.go (baseline 21)\nnew: 27 main deliverSessionNudgeLegacy cmd/gc/cmd_nudge.go\nregressed: 27 main wakeWillNotStart cmd/gc/api_state_wake_refusal.go (baseline 25)\nregressed: 27 opjournal (*ConversationLane).settle internal/opjournal/lane_done.go (baseline 23)\nregressed: 27 tmux (*tmuxFetcher).FetchState internal/runtime/tmux/state_cache.go (baseline 26)\nnew: 26 main (*conversationLaneWorker).await cmd/gc/conversation_lane_worker.go\nregressed: 26 main handleControllerConn cmd/gc/controller.go (baseline 21)\nregressed: 26 opjournal (*ConversationLane).reconcile internal/opjournal/lane_done.go (baseline 23)\nregressed: 26 session (*Manager).Suspend internal/session/manager.go (baseline 24)\nregressed: 25 api (*SupervisorMux).streamGlobalEvents internal/api/huma_handlers_supervisor.go (baseline 24)\nregressed: 25 main v2ClassStoreCapabilities cmd/gc/city_runtime_v2.go (baseline 24)\nregressed: 24 beads sqliteListSQL internal/beads/sqlite_store.go (baseline 22)\nnew: 24 dispatch processWorkflowFinalize internal/dispatch/runtime.go\nnew: 24 main (*ledgerFeed).refreshRows cmd/gc/ledger_feed.go\nregressed: 24 main classRoutedHookClaimOps cmd/gc/claim_class_route.go (baseline 23)\nnew: 24 main normalizeCanonicalBdScopeFilesResolvingCityDatabase cmd/gc/beads_provider_lifecycle.go\nnew: 24 materialize (MCPProjection).applyWithStderr internal/materialize/mcp_project.go\nregressed: 24 worker (*SessionHandle).Nudge internal/worker/handle_lifecycle.go (baseline 21)\nregressed: 23 main claimFirstReadyHookAssignment cmd/gc/cmd_hook_claim.go (baseline 21)\nregressed: 23 main dispatchQueuedNudges cmd/gc/nudge_dispatcher.go (baseline 22)\nregressed: 23 opjournal (*ConversationLane).writeDone internal/opjournal/lane_done.go (baseline 20)\nregressed: 23 session (*Store).CommitClose internal/session/store.go (baseline 21)\nnew: 22 events readFilteredTrackedContext internal/events/reader_context.go\nnew: 22 main (*laneTurnObserver).writeOnce cmd/gc/conversation_lane_turns.go\nnew: 22 main bestStoreWithWork cmd/gc/hook_cross_store.go\nregressed: 22 opjournal (*takePlan).formBatch internal/opjournal/lane_plan.go (baseline 20)\nregressed: 22 opjournal validateRequestShape internal/opjournal/request.go (baseline 20)\nregressed: 21 api humaSessionManagerError internal/api/huma_handlers_sessions.go (baseline 20)\nnew: 21 beads (*BdStore).ChildrenOfAny internal/beads/children_batch.go\nnew: 21 main (*controllerState).ConditionalWritesStatus cmd/gc/api_state_conditional_writes.go\nnew: 21 opjournal planTake internal/opjournal/lane_plan.go\nnew: 20 api writeSessionManagerError internal/api/handler_session_errors.go\nnew: 20 beads (*BdStore).fetchViaBDList internal/beads/bdstore.go\nnew: 20 main claimFirstEligibleHookCandidate cmd/gc/cmd_hook_claim.go\nnew: 20 main resolveProjectedMCPForTarget cmd/gc/mcp_integration.go\nnew: 20 main tryHookClaim cmd/gc/cmd_hook_claim.go\nnew: 20 main writeHookClaimWorkResultForBead cmd/gc/cmd_hook_claim.go\nnew: 20 opjournal (RuntimeRow).validateShape internal/opjournal/runtime_row.go\nnew: 20 tmux readCodexSubmitRecord internal/runtime/tmux/submit_turn_guard.go\nmake: *** [Makefile:216: complexity-check] Error 1\n"
  },
  "merge": {
    "rc": 2,
    "stdout": "",
    "stderr": "complexity: changes relative to baseline:\nregressed: 625 main reconcileSessionBeadsTracedWithNamedDemand cmd/gc/session_reconciler.go (baseline 614)\nregressed: 139 config LoadWithIncludesOptions internal/config/compose.go (baseline 138)\nregressed: 97 beads (*CachingStore).applyEvent internal/beads/caching_store_events.go (baseline 95)\nregressed: 90 main (*CityRuntime).reloadConfigTraced cmd/gc/city_runtime.go (baseline 86)\nregressed: 88 main resolveTemplate cmd/gc/template_resolve.go (baseline 87)\nregressed: 64 opjournal planRules internal/opjournal/lane_plan.go (baseline 60)\nregressed: 61 main startOneCity cmd/gc/cmd_supervisor.go (baseline 60)\nregressed: 57 main doPrimeWithHookFormatOpts cmd/gc/cmd_prime.go (baseline 53)\nregressed: 53 main cmdNudgeDrainWithFormat cmd/gc/cmd_nudge.go (baseline 45)\nregressed: 53 materialize Run internal/materialize/skills.go (baseline 44)\nnew: 42 beads (*CachingStore).RefreshRowReport internal/beads/caching_store_reads.go\nregressed: 41 main cmdHookWithOptions cmd/gc/cmd_hook.go (baseline 40)\nnew: 39 api (*Server).acceptSessionCreate internal/api/huma_handlers_sessions_command.go\nnew: 39 session (*Manager).StopTurnWith internal/session/stop_turn_with.go\nregressed: 37 main tryDeliverQueuedNudgesByPoller cmd/gc/cmd_nudge.go (baseline 32)\nnew: 37 worker OperatorKill internal/worker/operator_kill.go\nregressed: 36 dispatch expandDrain internal/dispatch/drain.go (baseline 33)\nregressed: 35 main (*hookClaimOps).applyDefaults cmd/gc/cmd_hook_claim.go (baseline 31)\nregressed: 35 opjournal (*takePlan).planContent internal/opjournal/lane_plan.go (baseline 33)\nnew: 34 main initAndHookDirResolvingDatabase cmd/gc/beads_provider_lifecycle.go\nnew: 34 main startBeadsLifecycleResolvingCityDatabase cmd/gc/beads_provider_lifecycle.go\nregressed: 34 tmux (*Tmux).nudgeSession internal/runtime/tmux/tmux.go (baseline 28)\nnew: 34 tmux closeClaudeBackgroundShells internal/runtime/tmux/claude_background_stop.go\nregressed: 32 config mergeFragment internal/config/compose.go (baseline 27)\nregressed: 32 opjournal (*ConversationLane).Pass internal/opjournal/lane.go (baseline 24)\nnew: 31 main buildDesiredStateWithSessionBeadsAtOpts cmd/gc/build_desired_state.go\nregressed: 31 main parseBdByIDUpdateArgs cmd/gc/cmd_bd_by_id.go (baseline 30)\nnew: 30 opjournal (*rulesPlan).planNowInterrupt internal/opjournal/lane_now.go\nregressed: 30 sessionlog ReadCodexFile internal/sessionlog/codex_reader.go (baseline 23)\nregressed: 29 main doSessionWake cmd/gc/cmd_session_wake.go (baseline 27)\nnew: 28 contract resolveDoltConnectionTarget internal/beads/contract/connection.go\nnew: 28 main cmdSessionSubmit cmd/gc/cmd_session.go\nregressed: 28 main runNudgeBackstop cmd/gc/nudge_backstop.go (baseline 21)\nnew: 27 main deliverSessionNudgeLegacy cmd/gc/cmd_nudge.go\nregressed: 27 main wakeWillNotStart cmd/gc/api_state_wake_refusal.go (baseline 25)\nregressed: 27 opjournal (*ConversationLane).settle internal/opjournal/lane_done.go (baseline 23)\nregressed: 27 tmux (*tmuxFetcher).FetchState internal/runtime/tmux/state_cache.go (baseline 26)\nnew: 26 main (*conversationLaneWorker).await cmd/gc/conversation_lane_worker.go\nregressed: 26 main handleControllerConn cmd/gc/controller.go (baseline 21)\nregressed: 26 opjournal (*ConversationLane).reconcile internal/opjournal/lane_done.go (baseline 23)\nregressed: 26 session (*Manager).Suspend internal/session/manager.go (baseline 24)\nregressed: 25 api (*SupervisorMux).streamGlobalEvents internal/api/huma_handlers_supervisor.go (baseline 24)\nregressed: 25 main v2ClassStoreCapabilities cmd/gc/city_runtime_v2.go (baseline 24)\nregressed: 24 beads sqliteListSQL internal/beads/sqlite_store.go (baseline 22)\nnew: 24 dispatch processWorkflowFinalize internal/dispatch/runtime.go\nnew: 24 main (*ledgerFeed).refreshRows cmd/gc/ledger_feed.go\nregressed: 24 main classRoutedHookClaimOps cmd/gc/claim_class_route.go (baseline 23)\nnew: 24 main normalizeCanonicalBdScopeFilesResolvingCityDatabase cmd/gc/beads_provider_lifecycle.go\nnew: 24 materialize (MCPProjection).applyWithStderr internal/materialize/mcp_project.go\nregressed: 24 worker (*SessionHandle).Nudge internal/worker/handle_lifecycle.go (baseline 21)\nregressed: 23 main claimFirstReadyHookAssignment cmd/gc/cmd_hook_claim.go (baseline 21)\nregressed: 23 main dispatchQueuedNudges cmd/gc/nudge_dispatcher.go (baseline 22)\nregressed: 23 opjournal (*ConversationLane).writeDone internal/opjournal/lane_done.go (baseline 20)\nregressed: 23 session (*Store).CommitClose internal/session/store.go (baseline 21)\nnew: 22 events readFilteredTrackedContext internal/events/reader_context.go\nnew: 22 main (*laneTurnObserver).writeOnce cmd/gc/conversation_lane_turns.go\nnew: 22 main bestStoreWithWork cmd/gc/hook_cross_store.go\nregressed: 22 opjournal (*takePlan).formBatch internal/opjournal/lane_plan.go (baseline 20)\nregressed: 22 opjournal validateRequestShape internal/opjournal/request.go (baseline 20)\nregressed: 21 api humaSessionManagerError internal/api/huma_handlers_sessions.go (baseline 20)\nnew: 21 beads (*BdStore).ChildrenOfAny internal/beads/children_batch.go\nnew: 21 main (*controllerState).ConditionalWritesStatus cmd/gc/api_state_conditional_writes.go\nnew: 21 opjournal planTake internal/opjournal/lane_plan.go\nnew: 20 api writeSessionManagerError internal/api/handler_session_errors.go\nnew: 20 beads (*BdStore).fetchViaBDList internal/beads/bdstore.go\nnew: 20 main claimFirstEligibleHookCandidate cmd/gc/cmd_hook_claim.go\nnew: 20 main resolveProjectedMCPForTarget cmd/gc/mcp_integration.go\nnew: 20 main tryHookClaim cmd/gc/cmd_hook_claim.go\nnew: 20 main writeHookClaimWorkResultForBead cmd/gc/cmd_hook_claim.go\nnew: 20 opjournal (RuntimeRow).validateShape internal/opjournal/runtime_row.go\nnew: 20 tmux readCodexSubmitRecord internal/runtime/tmux/submit_turn_guard.go\nmake: *** [Makefile:216: complexity-check] Error 1\n"
  },
  "diff": {
    "rc": 0,
    "stdout": "complexity: diff against 5c28b43e2f0f7baf37914b939175df255ce9b65e\ncomplexity: no threshold changes\n",
    "stderr": ""
  },
  "base_rows": 71,
  "merge_rows": 71,
  "new_or_worse": [],
  "diff_new_or_regressed": false,
  "reports_identical": true,
  "zero_growth": true,
  "rule": "ga-41e6k8r zero growth against pinned base; no waiver, baseline unchanged",
  "base_report_sha256": "bc843975eaeafbdb6ab6df5968f88b123d52601c383ea0340a4df242729a57a1",
  "merge_report_sha256": "bc843975eaeafbdb6ab6df5968f88b123d52601c383ea0340a4df242729a57a1"
}
```

Independent final audit:

```json
{
  "at": "2026-10-25T08:14:52.593840+00:00",
  "source": "c15e50cfbbe714a974df5d79add6611170406904",
  "head": "5d6a5975c6978c47d449b31c58d168285235a086",
  "tree": "75f77eb9beb41b8eeca4963d77331efc3aefa5d5",
  "base": "5c28b43e2f0f7baf37914b939175df255ce9b65e",
  "phases": {
    "unit": {
      "fresh_remote_results": 373,
      "artifacts": 746,
      "phase_start_ms": 1792912879726,
      "phase_end_ms": 1792913186968,
      "summary": [
        "Executed 287 out of 287 tests: 287 tests pass."
      ]
    },
    "acceptance": {
      "fresh_remote_results": 24,
      "artifacts": 48,
      "phase_start_ms": 1792913190987,
      "phase_end_ms": 1792913311211,
      "summary": [
        "Executed 7 out of 7 tests: 7 tests pass."
      ]
    },
    "integration-packages": {
      "fresh_remote_results": 86,
      "artifacts": 172,
      "phase_start_ms": 1792913316281,
      "phase_end_ms": 1792913542340,
      "summary": [
        "Executed 37 out of 37 tests: 37 tests pass."
      ]
    },
    "integration-full": {
      "fresh_remote_results": 14,
      "artifacts": 28,
      "phase_start_ms": 1792913543825,
      "phase_end_ms": 1792913795147,
      "summary": [
        "Executed 1 out of 1 test: 1 test passes."
      ]
    },
    "integration-latency": {
      "fresh_remote_results": 1,
      "artifacts": 2,
      "phase_start_ms": 1792913796291,
      "phase_end_ms": 1792913886811,
      "summary": [
        "Executed 1 out of 1 test: 1 test passes."
      ]
    }
  },
  "fresh_bep_results": 498,
  "artifacts": 996,
  "owned_roots": 9,
  "required_roots": 10,
  "native_floor_steps": 16,
  "test_counts": {
    "PASS": 126495,
    "FAIL": 0,
    "SKIP": 370
  },
  "skip_events": 395,
  "skip_sites": 268,
  "retained_file_digests": 1526,
  "tree_clean": true
}
```

Changed-file skip reasoning:

```json
{
  "source": "c15e50cfbbe714a974df5d79add6611170406904",
  "base": "5c28b43e2f0f7baf37914b939175df255ce9b65e",
  "merge": "5d6a5975c6978c47d449b31c58d168285235a086",
  "all_shared_hunks_answered": true,
  "site_evidence": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/changed-skip-source-evidence.txt",
  "files": {
    "cmd/gc/dolt_project_id_test.go": {
      "sites": [
        "SITE cmd/gc/dolt_project_id_test.go:183  func=TestManagedDoltHealthCheckWithPasswordUsesDirectHelpersAgainstRealServer kind=test lines=170-196  line_in_hunk=no  func_touched=no  shared_hunks_in_file=4  => UNCHANGED",
        "SITE cmd/gc/dolt_project_id_test.go:211  func=TestManagedDoltWaitReadyWithPasswordUsesDirectQueryProbe kind=test lines=198-221  line_in_hunk=no  func_touched=no  shared_hunks_in_file=4  => UNCHANGED",
        "SITE cmd/gc/dolt_project_id_test.go:224  func=TestRecoverManagedDoltProcessWithPasswordReusesHealthyRealServer kind=test lines=223-277  line_in_hunk=no  func_touched=no  shared_hunks_in_file=4  => UNCHANGED",
        "SITE cmd/gc/dolt_project_id_test.go:280  func=TestProjectIdentityL3AdapterContractAndManagedComposition kind=test lines=279-352  line_in_hunk=no  func_touched=no  shared_hunks_in_file=4  => UNCHANGED"
      ],
      "tests": [
        "TestManagedDoltHealthCheckWithPasswordUsesDirectHelpersAgainstRealServer",
        "TestManagedDoltWaitReadyWithPasswordUsesDirectQueryProbe",
        "TestProjectIdentityL3AdapterContractAndManagedComposition",
        "TestRecoverManagedDoltProcessWithPasswordReusesHealthyRealServer"
      ],
      "test_bodies_identical_to_base": true,
      "unchanged_fast_guard_prefix_sha256": "e2c8813fd21cb7c4b6bd47e0d88b2d3952cc9a7c153aebecf1808235964c65eb",
      "shared_hunks": {
        "dolttest_import": "New dependency has only constant/variable declarations and function bodies; no init or environment mutation. It cannot change GC_FAST_UNIT, read by unchanged skipSlowCmdGCTest.",
        "launcher_replacement": "Existing startRealDoltServer fast-unit guard and all setup preceding the changed launcher are byte-identical at pinned base and candidate. Unit skip happens before reaching the replaced launcher.",
        "cleanup_replacement": "The Process.Kill/Exited cleanup closure is created only after the unchanged fast-unit guard and successful launch. It cannot reach the guard.",
        "query_failure_log_tail": "Changed cleanup/log-tail failure branch runs only after launching and waiting for SQL readiness, after the guard."
      },
      "broader_lane_PASS_evidence": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/unchanged-real-dolt-callers-by-name.txt",
      "reason": "Four unedited caller tests intentionally SKIP the fast unit tier via an unchanged preceding helper guard; all four PASS by name in integration-packages. No omitted real-Dolt coverage is being excused."
    },
    "test/integration/bdstore_test.go": {
      "sites": [
        "SITE test/integration/bdstore_test.go:44  func=TestBdStoreConformance kind=test lines=35-86  line_in_hunk=no  func_touched=no  shared_hunks_in_file=4  => UNCHANGED"
      ],
      "tests": [
        "TestBdStoreConformance"
      ],
      "test_body_identical_to_base": true,
      "shared_hunks": {
        "imports": "Replacing net import with dolttest adds no initializer that changes control flow; skip is unconditional.",
        "server_deadline_constants": "Removing obsolete server-startup constants while retaining bdInitTimeout affects only later setup and cannot bypass or cause the earlier unconditional skip.",
        "shared_server_launcher": "Replacement StartSQLServer helper is called after requireDoltIntegration; both occur after the unchanged unconditional t.Skip.",
        "shared_server_cleanup": "New owned-process cleanup is installed only inside the later launcher, which this test never reaches."
      },
      "reason": "Unconditional unchanged upstream parent-ID conformance skip executes before all fixture setup and before the changed helper. Literal reason is retained; no tracker or waiver is used to justify an owned result."
    }
  },
  "at": "2026-10-25T07:40:45.378570+00:00"
}
```

Historical TDD objects (not current C3 credit):

```json
{
  "tdd_red": {
    "commit": "a284e321d8d84e4f3719e919b18f36961ab27dba",
    "source_commit_evidence": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/historical-tdd_red-commit.txt",
    "sha256": "f10afaebd140572588c63b64ec613a240a23bee54099901c1131071fa9980fda",
    "credit": "Historical builder record, corroborated by full reviewer; not current criterion3 execution evidence"
  },
  "tdd_green": {
    "commit": "e714e5a4e2ab29ea0ba117edf0aa5fd5abb57eba",
    "source_commit_evidence": "/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/historical-tdd_green-commit.txt",
    "sha256": "93605c4d584ef8e71b9426368304e82c0f81a5a7bb6b88b4c051086229e50287",
    "credit": "Historical builder record, corroborated by full reviewer; not current criterion3 execution evidence"
  }
}
```

diff_tests_executed:

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/deploy-ga-j1ootdt-fresh.mlsu2t19/c3/evidence/remote/logs logs=514 logs_with_results=448
test_counts: PASS=126495 FAIL=0 SKIP=370
  (top-level result lines, column 0, summed over the 514 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
diff_tests_executed:
  TestStartRealDoltServerForCompactTestOwnsItsPort PASS (integration-packages-a9e12e81511731ef69dd.log)
  TestSQLServerStopKillsAProcessThatIgnoresSIGTERM PASS (integration-packages-beb200754a14f803b870.log, unit-d6a5281ab972f0008c8d.log)
  TestStartSQLServerDisablesDoltEventFlush PASS (integration-packages-beb200754a14f803b870.log, unit-d6a5281ab972f0008c8d.log)
  TestStartSQLServerFailsOnNonPortExit PASS (integration-packages-beb200754a14f803b870.log)
  TestStartSQLServerGivesUpWhenEveryProbedPortIsLost PASS (integration-packages-beb200754a14f803b870.log, unit-d6a5281ab972f0008c8d.log)
  TestStartSQLServerReportsTheLogTailWhenTheServerExitsEarly PASS (integration-packages-beb200754a14f803b870.log, unit-d6a5281ab972f0008c8d.log)
  TestStartSQLServerRetriesWhenProbedPortIsTaken PASS (integration-packages-beb200754a14f803b870.log)
  TestStartSQLServerWithoutSocketTablesStillRejectsALostPort PASS (integration-packages-beb200754a14f803b870.log)
  TestDoltConfigTimeoutsUseNamedConstants PASS (unit-025c2540939eef68deba.log)
GATE_TEST_EVIDENCE_RESULT ok: 9 diff-owned tests verified by name
```
