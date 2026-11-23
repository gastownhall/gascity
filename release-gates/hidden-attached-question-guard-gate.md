# Hidden-attach question guard release gate — ga-0mw44b2

**Verdict:** **PASS**

Evaluated: 2026-10-29T04:50:59.375643+00:00.
Reviewed source `e7cb57f0f98b2cb96da2410da50980c9f9c3d896`, review ga-nacj0zt PASS; source/build ga-o5igec, original lineage ga-38q9xh.2.
Mode remote; frozen origin/main `2e934b1a4ee2f737b4acfd4af32f342337f96b50`; materialized merge `033080e9b982017bd8f14e98b6880fa63f3ff3f7`, tree `1ef7e5a6783e6a0e393926120a4064e41a0d3d8a`.
merge_state: clean (origin/main@2e934b1a4ee2f737b4acfd4af32f342337f96b50); gate_base_kind: current-tip.
merge_validation: exact merge result built/vetted here; MPR validates final landing merge.
Evidence root: `/var/tmp/deploy-ga-0mw44b2.pqco4z9j`. docs/PROJECT_MANIFEST.md absent; supplied criteria, TESTING.md and current Bazel CI define the gate.
Already-merged preflight: resolved source not on frozen main; source/pulls and own isolated-head PR queries empty.

| Criterion | Verdict | Evidence |
| --- | --- | --- |
| 1 Review PASS | PASS | ga-nacj0zt closed PASS on this exact resolved source. No reviewer-test carryover or waiver. |
| 2 Acceptance criteria met | PASS | All four changed guard tests pass by name; before-text dialog/capture-failure refusals cause no writes, a newly opened question blocks Enter, and answered/empty panes still deliver. Four real-tmux hidden-attach regression witnesses and three manifest/lane witnesses pass separately. |
| 3 Tests pass | PASS | Fast format/policy (11 targets)/generated (7 targets)/OpenAPI lanes first, each in independent fresh view pinned through gate-base. Five fresh remote phases plus all 16 mandatory native steps pass; every owned root PASS, no attribution/waiver. |
| 4 No unresolved HIGH | PASS | Exact review security/style/spec notes read; no HIGH or blocker finding. Independent source walk confirms shared matcher and pane-fencing semantics retained. |
| 5 Final tree clean | PASS | Exact canonical tree and clean tracked/untracked state verified after tests. Isolated publication starts at reviewed source and adds only this gate record. |
| 6 Merge/build/vet | PASS | Clean normal merge matches prediction; full 1390-target Bazel build/nogo/vet and active check-hooks pass. |
| 7 Single feature theme | PASS | Four files implement and register hidden-attach prompt guards; no unrelated theme, dependency or private-doc contamination. |

## Acceptance and boundary evidence

sendHiddenAttachedText checks the same pendingPromptRecheck/checkNoApprovalPromptIn matcher used by nudgeSession before typing text, then checks again immediately before Enter. A pending question returns runtime.ErrPendingInteraction, a failed pane capture returns errPromptLookFailed, and both refuse the corresponding write. Existing paneGuard/checkLive fencing and the hidden-attached write path remain intact. Answered idle/working composer states and an empty pane are each permitted by the positive test. No public API, config field, wire payload, dependency or process lifecycle rule changes.

Four diff-owned roots are derived mechanically by test/function body, not filename. The new helpers hiddenQuestionTmux, pokeRecorded, execute and executeCtx only provide fake observations/recorded writes, add no real subprocess/sleep/listener/environment mutation and contain no SKIP. The BUILD and runtime-tmux manifest merely register these roots in existing targets/lanes: no new CI job, matrix, timeout or required-check list. No docs/API/dashboard path changed, so no separate Mintlify/dashboard preview lane is applicable.

The seven supplementary witnesses exercise real hidden-attached text/lifecycle, focused-split fencing, byte-exact nudge delivery, and canonical manifest/lane registration. Their supplied-list reader labels them diff-owned; this record explicitly treats them as existing regression witnesses, separate from the four owned tests. All are read from the fresh full-suite logs, with no focused substitute.

The before-look/write boundary still has the inherent narrow observation-to-write interval; this is the existing guard contract. Provider.nudgeNow's prior check can make a hidden nudge perform three captures. Reviewer informational observations require no scope expansion. No claim of atomic prompt/write locking is made.

## Full-suite integrity

- test_cmd_scope: full-suite; heavy_mode: none (computed classifier).
- test_exec_mode: remote-exec (derived from BEP, not transport settings).
- test_exec_bep: /var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/{unit,acceptance,integration-packages,integration-full,integration-latency}.bep.json.
- test_log_dir: /var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs.
- test_counts: PASS=132635 FAIL=0 SKIP=373 top-level result lines summed per job.
- diff_tests_executed: 4/4 PASS by name; supplementary witnesses 7/7 PASS.
- failure_attribution: none; waiver_ref: none.
- ci_lane_run: n/a (existing manifest registers tests; no CI job/config change).
- rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}.
- fallback_reason: null.
- admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 46.43720626831055, "waited_seconds": 0.001}.
- farm_test_seconds: 8762.417.
- owned_topup: [].
- floor_topup: complete; all 16 steps; /var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/floor-topup.json.
- skip_inventory: /var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/skip-inventory.json; /var/tmp/deploy-ga-0mw44b2.pqco4z9j/farm-skip-inventory.json; /var/tmp/deploy-ga-0mw44b2.pqco4z9j/native-skip-inventory.json.
- latency_leg: {"attempts": [{"attempt": 1, "evidence_errors": [], "farm_test_seconds": 62.545, "phase": ["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$", "GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"], "probe": ["GATE_FARM_PROBE verdict=clear waited=0s load1=17.40 load5=13.01 load15=8.95 cpus=32 threshold=50 max_wait=600s"], "probe_verdict": "clear", "rc": 0, "results": {"TestGcBdLatencyBudget": {"PASS": 1}, "TestGcSeatHookLatencyBudget": {"PASS": 1}}, "test": ["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 30.65 17.42 10.63 14/1004 50074"]}], "attempts_allowed": 2, "complete": true, "main_leg": ["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"], "main_leg_verdicts": {"TestGcBdLatencyBudget": {}, "TestGcSeatHookLatencyBudget": {}}, "message": "measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS", "phase": "integration-latency", "rc": 0, "retired_farm_test_seconds": 0.0, "tests": ["TestGcBdLatencyBudget", "TestGcSeatHookLatencyBudget"], "verdict": "measured", "verdicts": {"TestGcBdLatencyBudget": {"PASS": 1}, "TestGcSeatHookLatencyBudget": {"PASS": 1}}}.

| Phase | Fresh remote results | Farm action seconds | Verdict |
| --- | --- | --- | --- |
| unit | 374 | 2516.529 | PASS |
| acceptance | 24 | 1523.928 | PASS |
| integration-packages | 85 | 3349.352 | PASS |
| integration-full | 14 | 1310.063 | PASS |
| integration-latency | 1 | 62.545 | PASS |

All 498 result events are remote, uncached, first attempts, start inside recorded phase windows and terminal PASS. All 996 retained raw log/XML artifacts independently hash-match. Full integration excludes only canonical latency-list tests, measured in the separate clear-probe lane. Native floor covers beadstest, Darwin compile, runner self-tests and every environment-absence package. TestTutorial01 has a literal PASS in current logs. Every remaining skip has an explicit reason and exact UNCHANGED site/containing function with zero shared hunks; none is diff-owned. No cached result, old review test, narrowed run, candidate retry or stitched suite supplies evidence.

Exact detached command:

```sh
env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-0mw44b2 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-validate.sv24a95c --base 2e934b1a4ee2f737b4acfd4af32f342337f96b50 --bead-json /var/tmp/deploy-ga-0mw44b2.pqco4z9j/bead-current.json --owned /var/tmp/deploy-ga-0mw44b2.pqco4z9j/owned.tsv --run-dir /var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence -- bash /var/tmp/deploy-ga-0mw44b2.pqco4z9j/local-full.sh
```

Resolved phase commands:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

Changed roots:

- `internal/runtime/tmux :: TestSendHiddenAttachedTextDeliversPastAnsweredQuestionDialog` — PASS by name.
- `internal/runtime/tmux :: TestSendHiddenAttachedTextRefusesEnterWhenQuestionOpensAfterText` — PASS by name.
- `internal/runtime/tmux :: TestSendHiddenAttachedTextRefusesQuestionDialogBeforeAnyWrite` — PASS by name.
- `internal/runtime/tmux :: TestSendHiddenAttachedTextRefusesWhenPaneCaptureFails` — PASS by name.

Native floor details:

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
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-821a75e4a503601af052.log",
          "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestReplay",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4d20b3d28497f844aa39.log",
          "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSessionEventPumpLiveHerdr",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-bf8ed8ba46004086b3b9.log",
          "reason": "session_event_pump_live_test.go:23: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSessionEventPumpLiveHerdr",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-2ef13e1b6898c5544764.log",
          "reason": "session_event_pump_live_test.go:23: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSimCoverageGate",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-56f04454dd9158a9ee8a.log",
          "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSimCoverageGate",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-23e7d9fc763496dd8095.log",
          "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-074fc6c34a1fb2577127.log",
          "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-2ef13e1b6898c5544764.log",
          "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-75af79934639abaae69c.log",
          "reason": "tmux_leak_guard_mntns_test.go:98: bwrap not installed; cannot build a private-/tmp mount namespace",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-64014bc0bb3833bc198c.log",
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
      "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/floor-logs/floor-0.log",
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
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-be91eec86b4dbf7e66f1.log",
          "reason": "gastown_test.go:4153: zsh not installed",
          "class": "environment-absence"
        },
        {
          "package": "examples/gastown",
          "test": "TestAttachedRigScopeShellToken/zsh",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-ef24b9dbe59f5d04fac1.log",
          "reason": "gastown_test.go:4153: zsh not installed",
          "class": "environment-absence"
        },
        {
          "package": "examples/gastown",
          "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-a0f68450216c5e4d04af.log",
          "reason": "maintenance_scripts_test.go:9321: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
          "class": "environment-absence"
        },
        {
          "package": "examples/gastown",
          "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-df0dcd5ff9a717b4f2a3.log",
          "reason": "maintenance_scripts_test.go:9321: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
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
      "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/floor-logs/floor-1.log",
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
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-999a89ecc5b3d09abdd4.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/BatchClearsOneKeyKeepsAnother",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-7f2c404ca64cd2da0395.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-999a89ecc5b3d09abdd4.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-7f2c404ca64cd2da0395.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-999a89ecc5b3d09abdd4.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-7f2c404ca64cd2da0395.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-999a89ecc5b3d09abdd4.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-7f2c404ca64cd2da0395.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestProbeVersionsAgainstHostBinaries/bd",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-363a36f2662d541bc75e.log",
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
      "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/floor-logs/floor-2.log",
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
      "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/floor-logs/floor-3.log",
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
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-8544388e16a0d6b0cdb0.log",
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
      "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/floor-logs/floor-4.log",
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
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-64046beff95fb28ccd66.log",
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
      "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/floor-logs/floor-5.log",
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
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
          "reason": "checks_custom_types_test.go:467: bd binary not on PATH",
          "class": "environment-absence"
        },
        {
          "package": "internal/doctor",
          "test": "TestCustomTypesCheck_TableDrift",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
          "reason": "checks_custom_types_test.go:297: bd binary not on PATH",
          "class": "environment-absence"
        },
        {
          "package": "internal/doctor",
          "test": "TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
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
      "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/floor-logs/floor-6.log",
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
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:159: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailConformance",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:26: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailCrossPodNameResolution",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:195: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailProjectKeyIsolation",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:286: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailReplyMissingOriginalIsNotFound",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
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
      "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/floor-logs/floor-7.log",
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
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "activity_live_test.go:20: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestActivityLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "activity_live_test.go:20: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestHerdrConformance",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "conformance_test.go:18: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestHerdrConformance",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "conformance_test.go:18: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "provider_live_test.go:15: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "provider_live_test.go:15: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveClaudeKindPath",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "kindpath_live_test.go:105: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveClaudeKindPath",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "kindpath_live_test.go:105: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveOccupantSwapKeepsLiveness",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "panebinding_live_test.go:21: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveOccupantSwapKeepsLiveness",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "panebinding_live_test.go:21: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestSessionEventsLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "events_live_test.go:40: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestSessionEventsLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
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
      "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/floor-logs/floor-8.log",
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
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:238: Start(skip-start-14): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/IsRunning_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:271: Start(skip-start-18): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ConcurrentDistinctPrefixes",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:367: Start(skip-start-25): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_EmptyPrefix",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:349: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ExcludesStopped",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:333: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_FindsSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:294: Start skip-start-21: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_PrefixFiltering",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:313: Start skip-start-22: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:421: Start(skip-start-30): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_EmptyNamesReturnsTrue",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:395: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_FalseAfterStop",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:404: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/SharedSession",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:61: Start shared session: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:124: concurrent Start(skip-start-3): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_CreatesRunningSession",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:84: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_DuplicateReturnsError",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:93: first Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:209: Start(skip-start-11): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_AlreadyStopped",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:193: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_NotRunning",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:170: Start sentinel: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_MakesSessionNotRunning",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
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
      "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/floor-logs/floor-9.log",
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
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
          "reason": "tmux_test.go:3663: not inside tmux \u2014 need server for bind-key",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/tmux",
          "test": "TestGetKeyBinding_SkipsGasTownBindings",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
          "reason": "tmux_test.go:3637: not inside tmux \u2014 need server for bind-key",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/tmux",
          "test": "TestIsGTBinding_DetectsGasTownBindings",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-15c091b746137e5c2921.log",
          "reason": "tmux_test.go:3689: not inside tmux \u2014 need server for bind-key",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/tmux",
          "test": "TestSetBindings_PreserveFallbackOnRepeatedCalls",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
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
      "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/floor-logs/floor-10.log",
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
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-2af37a65322c095289c3.log",
          "reason": "bd_schema_parity_test.go:121: bd not available",
          "class": "environment-absence"
        },
        {
          "package": "test/acceptance/helpers",
          "test": "TestBdLatestSchemaVersionLeavesNothingInItsTempRoot",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-2af37a65322c095289c3.log",
          "reason": "bd_schema_parity_test.go:144: bd not available",
          "class": "environment-absence"
        },
        {
          "package": "test/acceptance/helpers",
          "test": "TestRealBdProvisionLeavesTheHostHomeAlone",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/unit-2af37a65322c095289c3.log",
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
      "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/floor-logs/floor-11.log",
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
          "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/logs/integration-full-23490fe556f84649266a.log",
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
      "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/floor-logs/floor-12.log",
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
      "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/floor-logs/floor-13.log",
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
      "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/floor-logs/floor-14.log",
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
      "log": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/floor-logs/floor-15.log",
      "exit_code": 0
    }
  ],
  "log_dir": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/c3-run/evidence/remote/floor-logs"
}
```

Skip audit:

```json
{
  "farm": {
    "rows": 359,
    "unique_tests": 302,
    "site_candidates": 282,
    "all_sites_unchanged": true,
    "shared_hunks_at_skip_sites": 0,
    "classes": {
      "other": 288,
      "environment-absence": 71
    },
    "proof": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/farm-skip-sites-proof.txt"
  },
  "native": {
    "rows": 39,
    "unique_tests": 39,
    "site_candidates": 36,
    "all_sites_unchanged": true,
    "shared_hunks_at_skip_sites": 0,
    "classes": {
      "environment-absence": 29,
      "other": 10
    },
    "proof": "/var/tmp/deploy-ga-0mw44b2.pqco4z9j/native-skip-sites-proof.txt"
  },
  "diff_shared_hunk": "Added fake-pane tests and their recording helper have no SKIP, TestMain/init, subprocess, sleep, listener or environment mutation. Existing package BUILD/manifest gain only these four roots; no new test target or census increase. Changed production sendHiddenAttachedText performs prompt captures/refusals before text and Enter through existing pane guard; it does not alter environment/platform/deferred skip checks. Every retained skip site and containing function is mechanically checked UNCHANGED with shared_hunks_in_file=0."
}
```

## Environment and load

ICU74 global loader/hash, rootless Podman _ping and pinned bd runtime preflight pass. Paired disabled-Ryuk/unreaped opt-out is prescribed with the host sweep. Isolation disclosures are retained; no stronger isolation claim than actual PASS-THROUGH is made. No loader path/overlay/shim/pin override. Remote admission 25 GiB/cap 2, no host-load wait; mandatory local floor 40 GiB and host-wide cap remain unchanged with ordinary threshold 15 bounded soft wait. Actual load summaries below include any timeout and run conditions; no timed-out wait is itself a failure.

- remote-host: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=42.70 run_max_load=78.59 run_mean_load=47.39 run_readings=101 wait_first_load=42.70 wait_max_load=42.70 wait_mean_load=42.70 wait_readings=1 read_errors=0`
- native-floor: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=68.16 run_max_load=62.98 run_mean_load=52.09 run_readings=7 wait_first_load=31.73 wait_max_load=78.68 wait_mean_load=40.67 wait_readings=61 read_errors=0`
