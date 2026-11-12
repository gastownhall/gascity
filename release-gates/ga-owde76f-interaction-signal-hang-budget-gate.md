# Interaction signal hang-budget release gate — ga-owde76f

**Verdict:** **PASS**

Reviewed source: d2c52aff23df7655ce927b75dcc0651c0254ea0a
Frozen main / gate_base: e8e77bc3029b262b8324287eeb3b885fe6fed147
Materialized merge: 6cfa367b1ebe923e5ad332b144b983bd821edf57
Tested merge tree: c7c7ea36f54089dde61e63286976816f8eb545b2
DEPLOY_MODE: remote; BASE_REF: origin/main

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | ga-qatr95x reviewed this exact resolved source; no review-carryover substitution. |
| 2 | Acceptance | PASS | Three regression guards PASS by name: package-budget invariant, correct 2.371800554s/30s runs accepted, over-budget run rejected. Static inspection confirms elapsed capture immediately after process exit, before log read; four harness/BUILD files only. |
| 3 | Full CI-equivalent tests | PASS | Fresh forced unit, acceptance, integration-packages and full integration all pass; native floor16/16; 113246 PASS,0 FAIL,340 SKIP top-level result lines. All3 owned tests PASS,0 owned SKIP/FAIL/MISSING. |
| 4 | Open HIGH review findings | PASS | Zero blockers/HIGH. Helper-measurement nits and alternate ga-lv9kkw disposition are nonblocking. |
| 5 | Final branch clean | PASS | Tested merge committed/clean; final isolated branch adds only this gate record. Local unrelated gate records are retained outside the deploy checkout during publication. |
| 6 | Clean divergence / merged build | PASS | merge-tree rc0, materialized tree exactly equals prediction. go build ./... and full-tree make vet/nogo pass. No self-rebase required. Main re-fetched before publication remains the frozen base. |
| 7 | Single theme | PASS | Fake-worker interaction signal elapsed bound and three guards,4 files +144/-15, with matching BUILD registration. |

Preflight commit/pulls returned no PR; fork-path interlock passed. Ancestry guard accepted ga-owde76f and source/build ga-4pxoo4r. Source branch name is provenance only. docs/PROJECT_MANIFEST.md is absent; CI coverage/commands come from AGENTS.md, TESTING.md, Makefile and .github/workflows/bazel.yml.

Fast lane PASS before any suite launch: build; full-tree make vet/nogo; pinned2.12.0 fmt-check-changed; affected lint and four shell guards; eight forced static policy tests; make bazel-sync followed by git diff --exit-code; active .githooks confirmed. Every writing policy/drift lane ran in its own fresh view with the frozen gate base. Real pre-commit hook ran while materializing the merge. No API/OpenAPI/dashboard or CI-config change: manual dashboard preview and a new CI-job first-run are not applicable. Logs: /var/tmp/deploy-ga-owde76f.qxx81Ftc.

Criterion3 execution is a single completed detached run, not a stitched/retried chain:
/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc
GATE_RUN_EXIT rc=0 state=complete; systemd readback inactive/MainPID0.

The isolation wrapper explicitly reports PASS-THROUGH, NOT ISOLATED, for the temporary materialized checkout. No clone isolation is claimed. Bazel tests execute remotely in the pinned worker environment. BEP proves484 remote results, no cached flags, no local/other strategy, attempt1 and starts inside each phase window: unit368, acceptance24, integration-packages78, integration-full14. No failed verdict was retried. Native floor is separate required coverage, not a mixed-mode farm substitution.

test_cmd_scope: full-suite
heavy_mode: none (computed classifier)
test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/logs
test_counts: PASS=113246 FAIL=0 SKIP=340 (top-level result lines summed over500 logs, not unique tests)
waiver_ref: none (gascity has no waiver path)
ci_lane_run: n/a (no CI-config change)
owned_topup: [] (not needed)

Commands actually run, preserving phase flags:
```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```
Phase payloads: unit=make check; acceptance=bazel test with --config=acceptance and both acceptance targets; integration-packages=bazel test --config=integration //test:integration_packages; integration-full=bazel test --config=integration --test_arg=-test.v //test/integration:integration_test, no smoke filter.

By-name result:
```text
diff_tests_executed:
  TestPhase2InteractionSignalBoundStaysAHangDetector PASS (unit-8a98a003e4fc4bb087ab.log)
  TestPhase2InteractionSignalResultStillFailsWhenGenuinelyBroken PASS (unit-8a98a003e4fc4bb087ab.log)
  TestPhase2InteractionSignalResultToleratesASlowButCorrectRun PASS (unit-8a98a003e4fc4bb087ab.log)
GATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name
```

Skip review: retained farm inventory has326 skip events (71 environment-absence,255 other; top-level42 and206 respectively). All environment-absence roots were selected for the native floor by package and parent name. All16 floor steps exit0, including beadstest's host-cache check, Darwin compile and both runner self-tests (55/55 and25/25). Remaining SKIPs are unrelated optional live providers/tools, fixtures, platform cases, subprocess sentinels and tier opt-ins, inventoried with exact reasons in retained execution/skip records. The340 aggregate count includes native floor and repeated jobs; it is not the farm-inventory count or unique test count.

Own-package skips are17 unchanged Phase1 usage/profile cases. Official --site on phase1_conformance_test.go:100 and:110 and structured_conformance_test.go:153 reports UNCHANGED, func_touched=no, shared_hunks_in_file=0. The two changed helper files have shared hunks: hangbudget_test.go is comment-only with constant unchanged; phase2_fake_worker_test.go changes only the Phase2 interaction bound and elapsed capture. Neither reaches Phase1 invocation-usage extractor availability nor the structured-profile missing-edit-result decision. The new fixture helper is used only by the three owned guards, all PASS. No diff-owned skip is excused.

Pinned test bd v1.3.1 reports c1c4b642a matching deps.env; test-only PATH/GC_TEST_BD_BIN/GC_INTEGRATION_REAL_BD, fleet bd unchanged. Podman socket active and cached dolt-sql-server:2.2.0 checked. Optional tools/live-provider absence remains explicitly inventoried.

Load/admission:
- Remote class:25GiB floor,cap2,admitted at95.6768GiB after0.001s; remote admission bypasses local load wait. Host observations are not farm load.
- Outer host observation: threshold15,waited_seconds0,wait_timed_out0,run_start_load15.11,run_max_load16.58,run_mean_load13.88,run_readings29.
- Required native floor: threshold15,waited_seconds0,wait_timed_out0,run_start_load11.59,run_max_load15.01,run_mean_load13.81,run_readings6.
Both summaries have read_errors0. The native floor retained its local soft load wait.

Execution fields (full raw execution record retained at /var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/execution.json, sha256 3fde031df226db8d89ae0ebb79532b325b90a3c0ea57150965b1c8a1a24238e8;190007 bytes, summarized here to keep the checklist reviewable and below the bead CLI's single-argument limit):
```json
{
  "test_exec_mode": "remote-exec",
  "fallback_reason": null,
  "exit_code": 0,
  "test_cmd_scope": "full-suite",
  "evidence_dir": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote",
  "admission": {
    "class": "remote",
    "floor_gib": 25.0,
    "free_gib": 95.67680358886719,
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
  "farm_test_seconds": 7648.931,
  "criterion3_valid": true,
  "owned_topup": [],
  "skip_inventory": {
    "counts_by_reason_class": {
      "other": 255,
      "environment-absence": 71
    },
    "top_level_counts_by_reason_class": {
      "other": 206,
      "environment-absence": 42
    },
    "tests_recorded": 326,
    "retained_file": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/skip-inventory.json"
  },
  "floor_topup": {
    "complete": true,
    "log_dir": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/floor-logs",
    "steps": [
      {
        "target": "cmd/gc",
        "exit_code": 0,
        "tests": [
          "TestReplay",
          "TestSessionEventPumpLiveHerdr",
          "TestSimCoverageGate",
          "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
          "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace"
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/floor-logs/floor-0.log"
      },
      {
        "target": "examples/gastown",
        "exit_code": 0,
        "tests": [
          "TestAttachedRigScopeShellToken",
          "TestJsonlExportPushRetryDelayIsLocaleIndependent"
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/floor-logs/floor-1.log"
      },
      {
        "target": "internal/beads",
        "exit_code": 0,
        "tests": [
          "TestMetadataEmptyStringClearContract",
          "TestProbeVersionsAgainstHostBinaries"
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/floor-logs/floor-2.log"
      },
      {
        "target": "internal/beads/beadstest",
        "exit_code": 0,
        "tests": [
          "TestPinnedBeadsModuleDirResolvesThisBuildsOwnCache"
        ],
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/floor-logs/floor-3.log"
      },
      {
        "target": "internal/beads/contract",
        "exit_code": 0,
        "tests": [
          "TestMigrateJournalFileMatchesPinnedBeads"
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/floor-logs/floor-4.log"
      },
      {
        "target": "internal/beads/exec",
        "exit_code": 0,
        "tests": [
          "TestBrProviderConformance"
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/floor-logs/floor-5.log"
      },
      {
        "target": "internal/doctor",
        "exit_code": 0,
        "tests": [
          "TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint",
          "TestCustomTypesCheck_TableDrift",
          "TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext"
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/floor-logs/floor-6.log"
      },
      {
        "target": "internal/mail/exec",
        "exit_code": 0,
        "tests": [
          "TestMCPMailBridgeSourceable",
          "TestMCPMailConformance",
          "TestMCPMailCrossPodNameResolution",
          "TestMCPMailProjectKeyIsolation",
          "TestMCPMailReplyMissingOriginalIsNotFound"
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/floor-logs/floor-7.log"
      },
      {
        "target": "internal/runtime/herdr",
        "exit_code": 0,
        "tests": [
          "TestActivityLive",
          "TestHerdrConformance",
          "TestProviderLive",
          "TestProviderLiveClaudeKindPath",
          "TestProviderLiveOccupantSwapKeepsLiveness",
          "TestSessionEventsLive"
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/floor-logs/floor-8.log"
      },
      {
        "target": "internal/runtime/runtimetest",
        "exit_code": 0,
        "tests": [
          "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors"
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/floor-logs/floor-9.log"
      },
      {
        "target": "internal/runtime/tmux",
        "exit_code": 0,
        "tests": [
          "TestGetKeyBinding_CapturesUserBinding",
          "TestGetKeyBinding_SkipsGasTownBindings",
          "TestIsGTBinding_DetectsGasTownBindings",
          "TestSetBindings_PreserveFallbackOnRepeatedCalls"
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/floor-logs/floor-10.log"
      },
      {
        "target": "test/acceptance/helpers",
        "exit_code": 0,
        "tests": [
          "TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig",
          "TestBdLatestSchemaVersionLeavesNothingInItsTempRoot",
          "TestRealBdProvisionLeavesTheHostHomeAlone"
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/floor-logs/floor-11.log"
      },
      {
        "target": "test/integration",
        "exit_code": 0,
        "tests": [
          "TestStartDrift_SystemdManaged_RestartsToNewBuildID"
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
        "log": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/floor-logs/floor-12.log"
      },
      {
        "target": "fsys-darwin-compile",
        "exit_code": 0,
        "tests": [],
        "command": [
          "make",
          "test-fsys-darwin-compile"
        ],
        "log": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/floor-logs/floor-13.log"
      },
      {
        "target": "push-gate-lock-selftest",
        "exit_code": 0,
        "tests": [],
        "command": [
          "bash",
          "scripts/test-push-gate-lock.sh"
        ],
        "log": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/floor-logs/floor-14.log"
      },
      {
        "target": "local-concurrency-selftest",
        "exit_code": 0,
        "tests": [],
        "command": [
          "bash",
          "scripts/test-local-concurrency.sh"
        ],
        "log": "/var/tmp/gc-heavy-gate/runs/ga-owde76f.c3.qxx81Ftc/evidence/remote/floor-logs/floor-15.log"
      }
    ]
  },
  "test_counts": {
    "PASS": 113246,
    "FAIL": 0,
    "SKIP": 340
  }
}
```

Raw BEPs, retained logs/artifacts, skip inventory, full execution record, pinned-tool proof, owned-test explanation and lane logs are retained under the run/evidence directories above; never deleted by scratch cleanup. Build lineage ga-4pxoo4r fixes tracker ga-hvjhay; that tracker stays OPEN until actual main landing. This is a test-harness correction; no production gc build/deploy is requested. The deployer opens the PR and routes an exact-head merge-request; only MPR may merge.
