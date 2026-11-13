# Scope membership release gate — ga-i7bb92

**Verdict:** **PASS**

- Evaluated: 2026-10-29T03:07:06.242815+00:00.
- Reviewed source `5c179ad0015aff9ce94efa497baf10026f55bb48`, review ga-oo8wlo round2, build ga-ctgcsm/rework ga-47xp4b.
- Mode remote; base origin/main frozen at `a7a6196954672adb2863122d3038b8d4d7868649`.
- Materialized merge `55a5edb5b0938b54075ebf0daa25c052f1135b7b`, tree `c796602a4b84ed04ceb78bdcd69423599793c5ed`.
- `merge_state: clean (origin/main@a7a6196954672adb2863122d3038b8d4d7868649)`; `gate_base_kind: current-tip`.
- `merge_validation: merge result built and vetted here; MPR validates the final merge at landing`.
- Evidence root `/var/tmp/deploy-ga-i7bb92.ok5gqajv`. Prior October4 HOLD and partial suites are historical, never current PASS evidence.
- Already-merged preflight: exact source resolves and is not on main; source/pulls and our deploy-head PR queries return empty. No existing remote PR.
- docs/PROJECT_MANIFEST.md is absent. Supplied seven release criteria and current Bazel CI/TESTING.md define this gate.

| Criterion | Result | Evidence |
| --- | --- | --- |
| 1. Review PASS | PASS | Reviewer round2 PASS names the exact resolved source; no carryover or waiver. |
| 2. Acceptance | PASS | Only dangling-reference validation is wired. Nine owned roots PASS, including templated refs, aggregate errors, no-scope and no-extends behavior, and the controls keeping 4a/4b unwired. Existing bundled-formula corpus witness PASS. |
| 3. Full CI-equivalent tests | PASS | C6 first, then format/policy, all generated drift, OpenAPI and Mintlify lanes in independent fresh views before runtime/suite. Five fresh remote phases plus all mandatory native steps pass. No attribution or waiver. |
| 4. No unresolved HIGH | PASS | Reviewer security/style/spec findings have no blocking HIGH; informational ceilings remain recorded for follow-up ga-prk8k5. |
| 5. Tree clean | PASS | Exact canonical merge tree and clean tracked/untracked state verified after tests. Publication permits only the release-gate record on the isolated branch at reviewed source. |
| 6. Merge/build/vet | PASS | Clean normal merge matches merge-tree prediction. Full1390-target Bazel build, full make vet/nogo and active check-hooks pass. |
| 7. Single feature | PASS | Five files add formula scope-reference validation, directly tested supporting helpers and the corresponding authoring contract. No CI-config diff or private-doc contamination. |

## Acceptance and scope

The mayor's2026-09-30 split on ga-ctgcsm is binding: ship only4c, the dangling gc.scope_ref check. validateScopeMembership invokes only danglingScopeRefs; the dropped-membership and uncovered-sink functions have no production callers and are exercised directly. The nine new test roots cover these functions as well as the public Resolve boundary; every owned root actually passes by name.

A formula using extends validates each nonempty, non-templated scope reference against the merged graph. A valid body has gc.kind=scope and gc.scope_role=body. Every invalid reference is reported together with deterministic ordering. Placeholder refs are left for cook substitution. No-scope and no-extends behavior is preserved. The scope-body lookup matches the documented compile-time identity rules. No new exported API, endpoint or dependency is introduced.

The formula-spec-v2 section accurately distinguishes the three authoring rules from the one enforced check, explains explicit empty opt-out and pack/core release ordering, and introduces no TOML fence or link. Current docsync/link checks and baseline-aware real Mintlify broken-links/rendering pass; raw output is retained in mint-*.raw. No newly introduced doc failure is excused.

The existing TestBundledFormulaCorpusHasNoSelfClosingControlEdges witness compiles the shipped bundle/testdata corpus and passes in this current full suite. It is one supplementary regression witness, not a tenth owned test. The reader's witness output calls its supplied list diff-owned; this record distinguishes it explicitly.

Release order remains: this4c safety check belongs in the core window with ga-0nj7wm, before ga-vzvbpm's pack changes reach users. Build completion alone does not establish deployment. ga-vzvbpm and ga-prk8k5 remain open;4a/4b enablement remains the separate follow-up and depends on the pack being landed and live. MPR/mayor owns final sequencing. No child status is changed here.

## Full-suite evidence

- `test_cmd_scope: full-suite`; `heavy_mode: none` (computed classifier).
- `test_exec_mode: remote-exec` derived from BEP.
- `test_exec_bep: /var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/{unit,acceptance,integration-packages,integration-full,integration-latency}.bep.json`.
- `test_log_dir: /var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs`.
- `test_counts: PASS=132419 FAIL=0 SKIP=373` top-level result lines summed per job.
- `diff_tests_executed: 9/9 PASS by name`; one separate corpus witness PASS.
- `waiver_ref: none`; `failure_attribution: none`; `ci_lane_run: n/a (no CI-config diff)`.
- `rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}`.
- `fallback_reason: null`.
- `admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 51.347747802734375, "waited_seconds": 0.001}`.
- `farm_test_seconds: 8635.254`.
- `owned_topup: []`.
- `floor_topup: complete; 16 steps; /var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/floor-topup.json`.
- `skip_inventory: /var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/skip-inventory.json; /var/tmp/deploy-ga-i7bb92.ok5gqajv/farm-skip-inventory.json; /var/tmp/deploy-ga-i7bb92.ok5gqajv/native-skip-inventory.json`.

| Phase | Fresh remote result events | Farm action seconds | Verdict |
| --- | --- | --- | --- |
| unit | 374 | 2159.686 | PASS |
| acceptance | 24 | 1012.734 | PASS |
| integration-packages | 85 | 3365.128 | PASS |
| integration-full | 14 | 2017.41 | PASS |
| integration-latency | 1 | 80.296 | PASS |

All498 results are remote, uncached, first attempts, started inside recorded phase windows and terminal PASS. All996 raw log/XML artifacts hash-match independently. No farm/local suite stitching. Full integration excludes only the canonical latency list, measured separately on a clear-probe first attempt. Native floor covers beadstest, Darwin compile, runner self-tests and every environment-absence package. Remaining non-owned skips are inventoried by exact reason and site; all sites are UNCHANGED with zero shared hunks. TestTutorial01 has literal PASS in the current integration-package logs.

Exact detached command:

```sh
env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-i7bb92 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-validate.er59sf65 --base a7a6196954672adb2863122d3038b8d4d7868649 --bead-json /var/tmp/deploy-ga-i7bb92.ok5gqajv/bead-current.json --owned /var/tmp/deploy-ga-i7bb92.ok5gqajv/owned.tsv --run-dir /var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence -- bash /var/tmp/deploy-ga-i7bb92.ok5gqajv/local-full.sh
```

Resolved phase commands:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

Native floor:

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
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-514989cc035c0e9a8ff5.log",
          "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestReplay",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-0e1499ad8e8a0a6b9d81.log",
          "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSessionEventPumpLiveHerdr",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-2ab73b52d8fd4dc346df.log",
          "reason": "session_event_pump_live_test.go:23: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSessionEventPumpLiveHerdr",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-3d6d3036a2f5902472b5.log",
          "reason": "session_event_pump_live_test.go:23: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSimCoverageGate",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-074fc6c34a1fb2577127.log",
          "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSimCoverageGate",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-64014bc0bb3833bc198c.log",
          "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-c2ab4e7d4735e1bd66a7.log",
          "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-3d6d3036a2f5902472b5.log",
          "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-509ef0a6cd81866833cc.log",
          "reason": "tmux_leak_guard_mntns_test.go:98: bwrap not installed; cannot build a private-/tmp mount namespace",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
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
      "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/floor-logs/floor-0.log",
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
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-be91eec86b4dbf7e66f1.log",
          "reason": "gastown_test.go:4153: zsh not installed",
          "class": "environment-absence"
        },
        {
          "package": "examples/gastown",
          "test": "TestAttachedRigScopeShellToken/zsh",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-ef24b9dbe59f5d04fac1.log",
          "reason": "gastown_test.go:4153: zsh not installed",
          "class": "environment-absence"
        },
        {
          "package": "examples/gastown",
          "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-a0f68450216c5e4d04af.log",
          "reason": "maintenance_scripts_test.go:9321: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
          "class": "environment-absence"
        },
        {
          "package": "examples/gastown",
          "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-df0dcd5ff9a717b4f2a3.log",
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
      "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/floor-logs/floor-1.log",
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
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-999a89ecc5b3d09abdd4.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/BatchClearsOneKeyKeepsAnother",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-7f2c404ca64cd2da0395.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-999a89ecc5b3d09abdd4.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-7f2c404ca64cd2da0395.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-999a89ecc5b3d09abdd4.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-7f2c404ca64cd2da0395.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-999a89ecc5b3d09abdd4.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-7f2c404ca64cd2da0395.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestProbeVersionsAgainstHostBinaries/bd",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-363a36f2662d541bc75e.log",
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
      "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/floor-logs/floor-2.log",
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
      "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/floor-logs/floor-3.log",
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
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-8544388e16a0d6b0cdb0.log",
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
      "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/floor-logs/floor-4.log",
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
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-64046beff95fb28ccd66.log",
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
      "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/floor-logs/floor-5.log",
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
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
          "reason": "checks_custom_types_test.go:467: bd binary not on PATH",
          "class": "environment-absence"
        },
        {
          "package": "internal/doctor",
          "test": "TestCustomTypesCheck_TableDrift",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
          "reason": "checks_custom_types_test.go:297: bd binary not on PATH",
          "class": "environment-absence"
        },
        {
          "package": "internal/doctor",
          "test": "TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
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
      "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/floor-logs/floor-6.log",
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
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:159: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailConformance",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:26: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailCrossPodNameResolution",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:195: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailProjectKeyIsolation",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:286: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailReplyMissingOriginalIsNotFound",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
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
      "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/floor-logs/floor-7.log",
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
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "activity_live_test.go:20: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestActivityLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "activity_live_test.go:20: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestHerdrConformance",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "conformance_test.go:18: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestHerdrConformance",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "conformance_test.go:18: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "provider_live_test.go:15: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "provider_live_test.go:15: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveClaudeKindPath",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "kindpath_live_test.go:105: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveClaudeKindPath",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "kindpath_live_test.go:105: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveOccupantSwapKeepsLiveness",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "panebinding_live_test.go:21: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveOccupantSwapKeepsLiveness",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "panebinding_live_test.go:21: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestSessionEventsLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "events_live_test.go:40: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestSessionEventsLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
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
      "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/floor-logs/floor-8.log",
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
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:238: Start(skip-start-14): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/IsRunning_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:271: Start(skip-start-18): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ConcurrentDistinctPrefixes",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:367: Start(skip-start-25): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_EmptyPrefix",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:349: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ExcludesStopped",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:333: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_FindsSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:294: Start skip-start-21: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_PrefixFiltering",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:313: Start skip-start-22: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:421: Start(skip-start-30): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_EmptyNamesReturnsTrue",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:395: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_FalseAfterStop",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:404: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/SharedSession",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:61: Start shared session: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:124: concurrent Start(skip-start-3): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_CreatesRunningSession",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:84: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_DuplicateReturnsError",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:93: first Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:209: Start(skip-start-11): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_AlreadyStopped",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:193: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_NotRunning",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:170: Start sentinel: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_MakesSessionNotRunning",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
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
      "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/floor-logs/floor-9.log",
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
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
          "reason": "tmux_test.go:3663: not inside tmux \u2014 need server for bind-key",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/tmux",
          "test": "TestGetKeyBinding_SkipsGasTownBindings",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-15c091b746137e5c2921.log",
          "reason": "tmux_test.go:3637: not inside tmux \u2014 need server for bind-key",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/tmux",
          "test": "TestIsGTBinding_DetectsGasTownBindings",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
          "reason": "tmux_test.go:3689: not inside tmux \u2014 need server for bind-key",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/tmux",
          "test": "TestSetBindings_PreserveFallbackOnRepeatedCalls",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-packages-15c091b746137e5c2921.log",
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
      "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/floor-logs/floor-10.log",
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
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-2af37a65322c095289c3.log",
          "reason": "bd_schema_parity_test.go:121: bd not available",
          "class": "environment-absence"
        },
        {
          "package": "test/acceptance/helpers",
          "test": "TestBdLatestSchemaVersionLeavesNothingInItsTempRoot",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-2af37a65322c095289c3.log",
          "reason": "bd_schema_parity_test.go:144: bd not available",
          "class": "environment-absence"
        },
        {
          "package": "test/acceptance/helpers",
          "test": "TestRealBdProvisionLeavesTheHostHomeAlone",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/unit-2af37a65322c095289c3.log",
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
      "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/floor-logs/floor-11.log",
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
          "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/logs/integration-full-23490fe556f84649266a.log",
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
      "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/floor-logs/floor-12.log",
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
      "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/floor-logs/floor-13.log",
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
      "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/floor-logs/floor-14.log",
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
      "log": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/floor-logs/floor-15.log",
      "exit_code": 0
    }
  ],
  "log_dir": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/c3-ready-run/evidence/remote/floor-logs"
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
    "proof": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/farm-skip-sites-proof.txt"
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
    "proof": "/var/tmp/deploy-ga-i7bb92.ok5gqajv/native-skip-sites-proof.txt"
  },
  "diff_shared_hunk": "All nine roots in the new scope-membership test file are owned and must PASS. Its new fixture helpers contain no skips or environment/resource changes. Existing skip sites have no shared hunks. Production validation only inspects already-merged formula metadata; it does not change platform, external-checkout or optional-runtime availability that controls the inventoried skips."
}
```

## Changed tests

- `internal/formula :: TestDanglingScopeRefs` — PASS by name.
- `internal/formula :: TestDroppedScopeRefs` — PASS by name.
- `internal/formula :: TestResolveScopeMembershipAcrossExtends` — PASS by name.
- `internal/formula :: TestResolveScopeMembershipAgainstBundledScopedWork` — PASS by name.
- `internal/formula :: TestResolveScopeMembershipDoesNotCheckFormulasWithoutExtends` — PASS by name.
- `internal/formula :: TestResolveScopeMembershipEnforcesOnlyDanglingRefs` — PASS by name.
- `internal/formula :: TestResolveScopeMembershipIsInertWithoutScopes` — PASS by name.
- `internal/formula :: TestResolveScopeMembershipRejectsExtendingABaseWithoutTheBody` — PASS by name.
- `internal/formula :: TestUncoveredSinks` — PASS by name.

## Fast lanes and load

Pinned format/lint and shell guards,11 policy targets, make bazel-sync with tracked/untracked cleanliness,7 generated drift targets, OpenAPI breaking check and real baseline-aware Mintlify check PASS. Each lane runs in its own fresh view pinned through gate-base.sh, before runtime/classifier/suite. No docs/API generator modifies the canonical suite scratch.

ICU74 global loader/hash, rootless Podman socket and pinned bd preflights PASS. Isolation's PASS-THROUGH/NOT ISOLATED and ambient-name disclosures are retained; no stronger isolation claim is made. No library override, overlay or shim. Remote floor25GiB/cap2; required local floor retains its prescribed local-heavy soft load wait. No downgrade or candidate-test retry.

Preparation history: the runtime-only preflight initially refused at its default40GiB floor before any command ran; its documented remote25GiB per-call setting was then applied. The first canonical driver selected local/not-own-author because the older mint said Reviewed and PASSED, while the selector expects Reviewed + PASSED. It was stopped during load prewait, before admission, workload or any test result. All4 source commits were then authenticated through GitHub as quad341 and the exact reviewer round2 PASS re-read. Only that equivalent review-status wording was normalized; source, reviewed SHA and gate tree never changed. The fresh ready-run's selector is remote-exec and its actual mode is independently proven by BEP above. Original local-selection records, no-command observations, load spot reading and old observer output remain retained. No existing result was discarded or rerun to green.

- remote-host: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=61.93 run_max_load=108.18 run_mean_load=60.19 run_readings=111 wait_first_load=61.93 wait_max_load=61.93 wait_mean_load=61.93 wait_readings=1 read_errors=0`
- native-floor: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=92.65 run_max_load=89.59 run_mean_load=70.89 run_readings=8 wait_first_load=55.58 wait_max_load=107.75 wait_mean_load=57.42 wait_readings=61 read_errors=0`
