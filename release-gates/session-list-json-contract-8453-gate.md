# Session-list JSON contract release gate — ga-mm1auxv

**Verdict:** **PASS**

- Evaluated: 2026-10-29T00:29:03.677168+00:00
- Reviewed source: `9d342c1d09af757339439f872f4d425ee23a0f90`; build `ga-za2drj7`, review `ga-65z6z0l`.
- Deploy mode: remote; base ref: origin/main; frozen base: `73cb909ff2c1ebc24a3b328f56b2bbddb15c00ad`.
- Gated materialized merge: `64155679c595dd78e9d2173ed3826cc5a9f3224a`; tree: `95bd6e1ed9b25ff1a60edd9689c5c5dbc975fe82`.
- `merge_state: clean (origin/main@73cb909ff2c1ebc24a3b328f56b2bbddb15c00ad)`
- `gate_base_kind: current-tip`
- `merge_validation: MPR validates the current merge result at merge time; deployer does not rebase or merge.`
- Already-merged preflight: reviewed source absent from main and no existing own deploy PR at intake.
- Evidence root: `/var/tmp/deploy-ga-mm1auxv.gh_kgg3x`; raw logs, BEP, XML, mutation diffs and preview HTML are retained.
- docs/PROJECT_MANIFEST.md is absent at this source. The supplied seven criteria and repository TESTING.md/Bazel CI lanes are authoritative here.

| Criterion | Result | Evidence |
| --- | --- | --- |
| 1. Review PASS | PASS | Live review ga-65z6z0l explicitly approves the resolved source. No carryover, added-reviewer-test exception or waiver. |
| 2. Acceptance | PASS | Closed API/local oneOf schema, route matrix, unchanged local values/routing, additive API schema_version/ok migration, worker_dir declaration, by-name sync/conformance tests, actual loopback-binary acceptance and mutation checks below. |
| 3. Full CI-equivalent tests | PASS | Fast policy/format/all generated drift/OpenAPI and baseline-aware docs render passed before all five fresh remote phases. Mandatory native floor 16/16 steps exit 0. |
| 4. Open HIGH findings | PASS | Zero blocking HIGH findings. Nonblocking review notes: nil client misuse guard, historical bare-array comment, local help field optionality. |
| 5. Final tree clean | PASS | Canonical merge clean and tree unchanged after testing. Publication guard permits only the gate-record commit on the exact reviewed source and verifies a clean isolated branch before push. |
| 6. Merge/build/vet | PASS | Criterion 6 ran first: normal merge hooks, predicted tree equality, complete Bazel build, make vet/nogo and check-hooks passed. |
| 7. Single theme | PASS | Twelve source files implement and document one session-list JSON contract. No unrelated feature, CI configuration change or private-doc contamination. |

## Acceptance evidence

The API rendering retains _cache_age_s, sessions and each running row value, and adds schema_version=1 and ok=true. Consumers rejecting unknown keys must accept those additions. Local output retains schema_version, ok, filters, sessions and summary; optional worker_dir was emitted already and is now declared. Both schema variants close top-level and row properties and are mutually exclusive. Whitespace is outside the contract.

The real Cobra command and SupervisorMux are exercised through an in-process HTTP client: explicit remote routes in every hosting mode; supervisor-hosted local; standalone/unknown hosting with API port; no controller with reachable/unreachable supervisor client; GC_NO_API; populated and empty outputs. The production route decision and existing route-selection test are unchanged. Running=false means not observed running, while absent running/cache age on local output means not provided by that route.

All four changed test roots PASS in the full-suite logs; the real binary acceptance subtest TestSessionDefaultNamedSession/List_JSON_LoopbackCityURL reports PASS. TestTutorial01 reports PASS in the integration-package lane with process coverage enabled. API schema/struct synchrony and generated CLI documentation pass their actual drift checks. make dashboard-check passes all ten supplementary targets (cached checks allowed here; the canonical unit phase is fresh), and an actual npm preview serves HTTP200, HTML hash 86e1c6434496852e1c4659fa5163a89ee7c1bdcb24fa2778d402a30cf2781930.

Supplementary expected-red mutations use separate disposable fresh views, never the canonical tree: removing running fails both route conformance and loopback acceptance; emitting an undeclared row field fails local schema/struct synchrony; removing worker_dir from the schema fails local synchrony. Every receipt has a named assertion FAIL and exit3. These focused mutations are supplemental acceptance evidence, not the full-suite result. Three earlier reviewer-reproduced mutations (supervisor route, _cache_age_s, schema_version) are attributed to the independent review. The original test-only commit does not compile without the new HTTP-client constructor; no claim of an executable original RED is made.

## Full-suite evidence

- `test_cmd_scope: full-suite`
- `heavy_mode: none` (computed classifier)
- `test_exec_mode: remote-exec` (BEP-derived)
- `test_exec_bep: /var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/{unit,acceptance,integration-packages,integration-full,integration-latency}.bep.json`
- `test_log_dir: /var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs`
- `test_counts: PASS=131445 FAIL=0 SKIP=373` (top-level per-job result lines; repeats count)
- `diff_tests_executed: 4/4 PASS by name; no owned FAIL, SKIP or MISSING`
- `waiver_ref: none`; `failure_attribution: none`; `ci_lane_run: n/a (no CI-config diff)`
- `rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}`
- `fallback_reason: null`
- `admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 50.08003234863281, "waited_seconds": 0.001}`
- `farm_test_seconds: 11389.943`
- `owned_topup: []`
- `floor_topup: complete; 16 steps; /var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/floor-topup.json`
- `skip_inventory: /var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/skip-inventory.json; /var/tmp/deploy-ga-mm1auxv.gh_kgg3x/farm-skip-inventory.json; /var/tmp/deploy-ga-mm1auxv.gh_kgg3x/native-skip-inventory.json`

| Phase | Fresh remote result events | Farm action seconds | Verdict |
| --- | --- | --- | --- |
| unit | 374 | 3245.413 | PASS |
| acceptance | 24 | 1226.139 | PASS |
| integration-packages | 85 | 3858.794 | PASS |
| integration-full | 14 | 2947.462 | PASS |
| integration-latency | 1 | 112.135 | PASS |

All 498 result events use remote execution, neither cache flag, first attempt, valid phase start window and terminal PASS. All 996 downloaded log/XML artifacts were independently SHA256-verified. No farm/local suite stitching occurred. Every remote phase forces --nocache_test_results and --noremote_local_fallback. Full integration omits exactly the canonical latency inventory; the separate latency phase measured both budgets on its first clear-probe attempt.

Exact canonical detached command:

```sh
env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-mm1auxv /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-validate.xxwtrhzh --base 73cb909ff2c1ebc24a3b328f56b2bbddb15c00ad --bead-json /var/tmp/deploy-ga-mm1auxv.gh_kgg3x/bead-current.json --owned /var/tmp/deploy-ga-mm1auxv.gh_kgg3x/owned.tsv --run-dir /var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence -- bash /var/tmp/deploy-ga-mm1auxv.gh_kgg3x/local-full.sh
```

Resolved whole-tree phase commands:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

Native floor and per-package environment top-ups:

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
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-821a75e4a503601af052.log",
          "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestReplay",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4d20b3d28497f844aa39.log",
          "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSessionEventPumpLiveHerdr",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-9326181a736eb30b71ca.log",
          "reason": "session_event_pump_live_test.go:23: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSessionEventPumpLiveHerdr",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-f679cd2551fcc64525aa.log",
          "reason": "session_event_pump_live_test.go:23: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSimCoverageGate",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-56f04454dd9158a9ee8a.log",
          "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSimCoverageGate",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-23e7d9fc763496dd8095.log",
          "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-b72a67a2f9444af705e3.log",
          "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
          "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-f7fba3a97bc02cffff0b.log",
          "reason": "tmux_leak_guard_mntns_test.go:98: bwrap not installed; cannot build a private-/tmp mount namespace",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
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
      "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/floor-logs/floor-0.log",
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
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-be91eec86b4dbf7e66f1.log",
          "reason": "gastown_test.go:4153: zsh not installed",
          "class": "environment-absence"
        },
        {
          "package": "examples/gastown",
          "test": "TestAttachedRigScopeShellToken/zsh",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-ef24b9dbe59f5d04fac1.log",
          "reason": "gastown_test.go:4153: zsh not installed",
          "class": "environment-absence"
        },
        {
          "package": "examples/gastown",
          "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-a0f68450216c5e4d04af.log",
          "reason": "maintenance_scripts_test.go:9321: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
          "class": "environment-absence"
        },
        {
          "package": "examples/gastown",
          "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-df0dcd5ff9a717b4f2a3.log",
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
      "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/floor-logs/floor-1.log",
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
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-20cfdbabba8f52b16e0b.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/BatchClearsOneKeyKeepsAnother",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-ea4511196531bb4b8c55.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-20cfdbabba8f52b16e0b.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-ea4511196531bb4b8c55.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-20cfdbabba8f52b16e0b.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-ea4511196531bb4b8c55.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-20cfdbabba8f52b16e0b.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-ea4511196531bb4b8c55.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestProbeVersionsAgainstHostBinaries/bd",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-3ee1ae2eaa26dfd63a88.log",
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
      "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/floor-logs/floor-2.log",
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
      "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/floor-logs/floor-3.log",
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
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-8544388e16a0d6b0cdb0.log",
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
      "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/floor-logs/floor-4.log",
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
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-64046beff95fb28ccd66.log",
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
      "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/floor-logs/floor-5.log",
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
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
          "reason": "checks_custom_types_test.go:467: bd binary not on PATH",
          "class": "environment-absence"
        },
        {
          "package": "internal/doctor",
          "test": "TestCustomTypesCheck_TableDrift",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
          "reason": "checks_custom_types_test.go:297: bd binary not on PATH",
          "class": "environment-absence"
        },
        {
          "package": "internal/doctor",
          "test": "TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
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
      "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/floor-logs/floor-6.log",
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
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:159: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailConformance",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:26: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailCrossPodNameResolution",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:195: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailProjectKeyIsolation",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:286: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailReplyMissingOriginalIsNotFound",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
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
      "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/floor-logs/floor-7.log",
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
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "activity_live_test.go:20: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestActivityLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "activity_live_test.go:20: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestHerdrConformance",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "conformance_test.go:18: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestHerdrConformance",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "conformance_test.go:18: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "provider_live_test.go:15: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "provider_live_test.go:15: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveClaudeKindPath",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "kindpath_live_test.go:105: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveClaudeKindPath",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "kindpath_live_test.go:105: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveOccupantSwapKeepsLiveness",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "panebinding_live_test.go:21: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveOccupantSwapKeepsLiveness",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "panebinding_live_test.go:21: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestSessionEventsLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "events_live_test.go:40: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestSessionEventsLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
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
      "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/floor-logs/floor-8.log",
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
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:238: Start(skip-start-14): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/IsRunning_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:271: Start(skip-start-18): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ConcurrentDistinctPrefixes",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:367: Start(skip-start-25): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_EmptyPrefix",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:349: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ExcludesStopped",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:333: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_FindsSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:294: Start skip-start-21: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_PrefixFiltering",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:313: Start skip-start-22: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:421: Start(skip-start-30): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_EmptyNamesReturnsTrue",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:395: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_FalseAfterStop",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:404: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/SharedSession",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:61: Start shared session: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:124: concurrent Start(skip-start-3): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_CreatesRunningSession",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:84: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_DuplicateReturnsError",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:93: first Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:209: Start(skip-start-11): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_AlreadyStopped",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:193: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_NotRunning",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:170: Start sentinel: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_MakesSessionNotRunning",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
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
      "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/floor-logs/floor-9.log",
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
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
          "reason": "tmux_test.go:3663: not inside tmux \u2014 need server for bind-key",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/tmux",
          "test": "TestGetKeyBinding_SkipsGasTownBindings",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-15c091b746137e5c2921.log",
          "reason": "tmux_test.go:3637: not inside tmux \u2014 need server for bind-key",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/tmux",
          "test": "TestIsGTBinding_DetectsGasTownBindings",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
          "reason": "tmux_test.go:3689: not inside tmux \u2014 need server for bind-key",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/tmux",
          "test": "TestSetBindings_PreserveFallbackOnRepeatedCalls",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-packages-15c091b746137e5c2921.log",
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
      "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/floor-logs/floor-10.log",
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
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-2af37a65322c095289c3.log",
          "reason": "bd_schema_parity_test.go:121: bd not available",
          "class": "environment-absence"
        },
        {
          "package": "test/acceptance/helpers",
          "test": "TestBdLatestSchemaVersionLeavesNothingInItsTempRoot",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-2af37a65322c095289c3.log",
          "reason": "bd_schema_parity_test.go:144: bd not available",
          "class": "environment-absence"
        },
        {
          "package": "test/acceptance/helpers",
          "test": "TestRealBdProvisionLeavesTheHostHomeAlone",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/unit-2af37a65322c095289c3.log",
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
      "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/floor-logs/floor-11.log",
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
          "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/logs/integration-full-23490fe556f84649266a.log",
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
      "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/floor-logs/floor-12.log",
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
      "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/floor-logs/floor-13.log",
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
      "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/floor-logs/floor-14.log",
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
      "log": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/floor-logs/floor-15.log",
      "exit_code": 0
    }
  ],
  "log_dir": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/c3-run/evidence/remote/floor-logs"
}
```

Skip audit (the inventories retain each test, exact reason and site):

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
    "proof": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/farm-skip-sites-proof.txt"
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
    "proof": "/var/tmp/deploy-ga-mm1auxv.gh_kgg3x/native-skip-sites-proof.txt"
  },
  "diff_shared_hunk": "The extracted management JSON validation helper preserves decode/validation behavior and adds no skip condition. New contract helpers are invoked only by their owned test roots. Acceptance helpers only parse published schema and the existing supervisor port. Import-named-session change is comment-only. Exact skip sites and shared hunks must still be audited; this is not an automatic exemption."
}
```

All skip-site candidates are UNCHANGED with zero shared hunks at those sites. Remaining non-owned skips concern documented platform/environment/optional fixtures or intentional capability cases; every environment-absence package receives the native top-up. New contract helpers only serve their owned roots, and the extracted existing JSON validator changes no skip gate. No changed test is excused.

## Changed tests by name

- `cmd/gc :: TestRouteSessionList_APIJSONIncludesCacheAge` — PASS by name.
- `cmd/gc :: TestSessionListJSONRouteConformance` — PASS by name.
- `cmd/gc :: TestSessionListRowStructsMatchSchema` — PASS by name.
- `test/acceptance :: TestSessionDefaultNamedSession` — PASS by name.

## Fast lanes and environment

Policy PASS: pinned lint, fmt-check-changed, shell guards, nogo/vet and 11 policy targets. Drift PASS: make bazel-sync plus git diff --exit-code and clean tracked/untracked status, followed by all seven generated checks. OpenAPI breaking check PASS. Baseline-aware .github/scripts/docs-render-check.sh PASS, including actual Mintlify no-broken-links output. Each lane runs in its own fresh committed view, before runtime/classifier/suite launch. The docs generation sources and outputs remain synchronized.

ICU74 global loader/hashes, rootless Podman socket and pinned bd 1.3.2 preflight PASS. Isolation wrapper reports PASS-THROUGH/NOT ISOLATED: the ambient environment is not claimed clean; its unstripped names are retained in logs. No library-path override or namespace overlay was used. Native floor runs under the retained remote admission with the prescribed local-heavy soft load wait. The unused complete-local downgrade requires 40 GiB and local-heavy admission. Remote admission is 25 GiB/cap 2.

- remote-host: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=70.84 run_max_load=108.40 run_mean_load=90.53 run_readings=118 wait_first_load=70.84 wait_max_load=70.84 wait_mean_load=70.84 wait_readings=1 read_errors=0`
- native-floor: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=69.47 run_max_load=82.13 run_mean_load=76.87 run_readings=13 wait_first_load=91.05 wait_max_load=104.99 wait_mean_load=94.35 wait_readings=61 read_errors=0`
- dashboard: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=99.28 run_max_load=97.13 run_mean_load=97.13 run_readings=1 wait_first_load=70.84 wait_max_load=108.40 wait_mean_load=90.71 wait_readings=62 read_errors=0 prewait_s=1801`

Invocation corrections: initial continuation wrappers misread status exit codes and stopped before suite execution; their failed artifacts remain. The initial docs-render setup hit a transient npm registry404 before Mintlify checking; both head/base setup outputs and a subsequent200 registry response are retained. The exact unchanged-source docs command later passed in a fresh view; no version pin, attribution or test waiver was used. The initial result observer later failed on a bd database timeout; a replacement observer affects no gate command or result.
