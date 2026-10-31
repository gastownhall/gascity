# Confirm liveness before session startup — ga-aj9sx1u

**Verdict:** **PASS**

Evaluated: 2026-10-25T12:18:27.737759+00:00

Reviewed source: a56596e03283a57e0cf04b8a880e76601fc72d5a
Frozen origin/main and canonical gate base: ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083
Tested synthetic merge: 60b58688ea9e7b1cdf086d19b0afa7c465df9fec
Tested tree: 4eeb097ba65e60bac0d8a0a9db933e284b59fa7d
merge_state: clean (origin/main@ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083)
gate_base_kind: current-tip
gate_base: ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083
merge_validation: clean merge result built and vetted with normal hooks; merge authority validates the actual landing result.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Independent review ga-onnu9y5 names this exact source. Reviewer-added test commit independently checked; reviewer-tests-verified.txt records the only changed test file. |
| 2 | Acceptance criteria met | PASS | All 11 changed session tests and 12 additional acceptance roots pass by name in the complete run. Fresh/unknown/absent observations, both start entry points, resume preservation, composite routing and unchanged provider observation sequences are covered. |
| 3 | Tests pass | PASS | Five fresh remote Bazel phases plus required native floor; every raw artifact, BEP execution predicate, changed test result and skip site independently audited. |
| 4 | No unresolved HIGH finding | PASS | Current independent review verdict PASS; no unresolved HIGH finding. Architect FR-04 and required publication disclosure retained. |
| 5 | Final source clean | PASS | Exact tested merge HEAD/tree and clean status independently checked. Publication adds only this report on an isolated reviewed-source branch, with normal hooks and another clean-status check. |
| 6 | Merge state, build and vet | PASS | C6_PASS: normal merge commit hook, complete Bazel build, integration binary compilation, make vet/nogo and make check-hooks. Original build logs retained. |
| 7 | Single feature theme | PASS | Four paths under internal/session implement and test one start-path liveness rule. Full denylist/citation guard passed with confirmed same-feature ga-gkooyt, ga-sm490to and ga-ue1e7xi. |

## Acceptance and review integrity

reviewer_tests_verified: 6e08aa8229429c73d01d105524169cd5ad197ae8 -> a56596e03283a57e0cf04b8a880e76601fc72d5a files=internal/session/ensure_running_unconfirmed_test.go
The runtime-only pre-start gate, both ErrSessionExists convergence paths and both stale-resume-key checks use the confirmed observation. A cached running answer adds no fresh read; a provider without FreshLivenessObserver keeps its prior reads. Unknown liveness returns an error before orphan scanning/start or resume-key reset. Confirmed absence and a corpse can start. The leaf backend supplies the fresh read, with the auto composite consulted on an unknown route to cover its other backend.

Architect ruling ga-f6y5q8n FR-04 supersedes Change 3 and acceptance criteria 8 and 14 of ga-gkooyt: hybrid.Provider does not claim FreshLivenessObserver. The removed earlier RED hybrid-forwarding test is a ruled specification change. The PR must explicitly carry this explanation and Refs #7170; publication verifies the exact body before clearance.

## Full-suite evidence

test_cmd_scope: full-suite
test_cmd: gate-criterion3.sh --repo /var/tmp/deploy-ga-aj9sx1u.mvggn0nm/merge --base ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083 --bead-json bead-for-c3.json --owned required.tsv --run-dir c3/evidence. Five full Bazel phases: make check; acceptance; integration-packages; full integration minus the canonical latency list; the canonical latency list. All use --config=remote-exec --config=ci --keep_going --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 and per-phase BEP. Exact argv/windows/results remain in each phase stdout and execution.json.
test_exec_mode: remote-exec
test_counts: {"FAIL": 0, "PASS": 126573, "SKIP": 370}
Counts are top-level result lines summed across retained job logs; a test run by multiple jobs contributes once per job, rather than once per unique name.
diff_tests_executed: 11/11 PASS; all 23 required roots PASS by name, with no owned FAIL/SKIP/MISSING.
waiver_ref: none; no gascity waiver path.
test_log_dir: /var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/logs
heavy_mode: none
ci_lane_run: n/a — no CI configuration changed.
failure_attribution: none — no failed verdict attributed or retried.

Every result was remote-executed, uncached, attempt 1, with a start inside its phase window and a terminal PASS. Raw log/XML artifacts were SHA256-verified; normalized logs were compared against the originals. final-audit.json and retained-artifact-digests.json record the proof.

```json
{
  "at": "2026-10-25T12:18:09.725204+00:00",
  "source": "a56596e03283a57e0cf04b8a880e76601fc72d5a",
  "head": "60b58688ea9e7b1cdf086d19b0afa7c465df9fec",
  "tree": "4eeb097ba65e60bac0d8a0a9db933e284b59fa7d",
  "base": "ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083",
  "phases": {
    "unit": {
      "fresh_remote_results": 373,
      "artifacts": 746,
      "phase_start_ms": 1792927118893,
      "phase_end_ms": 1792927277158,
      "summary": [
        "Executed 287 out of 287 tests: 287 tests pass."
      ]
    },
    "acceptance": {
      "fresh_remote_results": 24,
      "artifacts": 48,
      "phase_start_ms": 1792927277945,
      "phase_end_ms": 1792927401538,
      "summary": [
        "Executed 7 out of 7 tests: 7 tests pass."
      ]
    },
    "integration-packages": {
      "fresh_remote_results": 85,
      "artifacts": 170,
      "phase_start_ms": 1792927402037,
      "phase_end_ms": 1792927662291,
      "summary": [
        "Executed 36 out of 36 tests: 36 tests pass."
      ]
    },
    "integration-full": {
      "fresh_remote_results": 14,
      "artifacts": 28,
      "phase_start_ms": 1792927663341,
      "phase_end_ms": 1792928109186,
      "summary": [
        "Executed 1 out of 1 test: 1 test passes."
      ]
    },
    "integration-latency": {
      "fresh_remote_results": 1,
      "artifacts": 2,
      "phase_start_ms": 1792928112017,
      "phase_end_ms": 1792928210780,
      "summary": [
        "Executed 1 out of 1 test: 1 test passes."
      ]
    }
  },
  "fresh_bep_results": 497,
  "artifacts": 994,
  "owned_roots": 11,
  "required_roots": 23,
  "native_floor_steps": 16,
  "test_counts": {
    "PASS": 126573,
    "FAIL": 0,
    "SKIP": 370
  },
  "skip_events": 395,
  "skip_sites": 268,
  "retained_file_digests": 1523,
  "tree_clean": true
}
```

## Fast lane and environment

fast_lane: PASS 2026-10-25T11:17:57.049376+00:00 merge_head=60b58688ea9e7b1cdf086d19b0afa7c465df9fec
Each policy/format, BUILD drift and generated-artifact lane ran in its own fresh committed-tree view. The policy lane used the frozen gate base and pinned formatter/linter. make bazel-sync left no tracked or untracked drift. Seven generated-artifact targets passed. No lane wrote into the suite checkout.

policy_lane: PASS
```
GLT_RECORD: {"golangci_lint":{"pin":"2.12.0","resolved":"2.12.0","mismatch":false}}
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/merge head=60b58688ea9e tree=4eeb097ba65e dir=/var/tmp/gc-fresh-tree.5t8M0j modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.5t8M0j
```

drift_lane: PASS
```
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/merge head=60b58688ea9e tree=4eeb097ba65e dir=/var/tmp/gc-fresh-tree.5p5v7g modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
```

generated_lane: PASS
```
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/merge head=60b58688ea9e tree=4eeb097ba65e dir=/var/tmp/gc-fresh-tree.M4FDRB modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
```

The podman socket, pinned Dolt image and host ICU74 loader were checked before C3. Native floor/top-ups retain the standard soft load wait; no floor, admission or load override. The remote class uses its standard 25GiB floor and cap2.

```
GATE_ICU74 lib=libicui18n.so.74 file="/usr/local/lib64/icu74-compat/libicui18n.so.74.2" sha256=d90d62f0d7b6c6fbc279ae5e433cc7004aa47cdb106a71a02350099f8df9ae51 recorded=yes
GATE_ICU74 lib=libicuuc.so.74 file="/usr/local/lib64/icu74-compat/libicuuc.so.74.2" sha256=33499dd97a5393926b32e0b535f5340f7dae14b35e59025b572e6bd9ad48a2ae recorded=yes
GATE_ICU74 lib=libicudata.so.74 file="/usr/local/lib64/icu74-compat/libicudata.so.74.2" sha256=c891c5fdb45e3cf756b728abe256be7af1bfc97f00ca2f2fac450c96c09bd4b7 recorded=yes
GATE_ICU74 verdict=OK provenance=recorded sonames="libicui18n.so.74 libicuuc.so.74 libicudata.so.74"
```

The standard isolation wrapper reported PASS-THROUGH for this private gascity checkout. It supplied the pinned bd binary and noninteractive credential environment. Remote test actions ran under the canonical Bazel worker environment; no wrapper clone or host runtime overlay is claimed.

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
    "results": 85,
    "remote_executed": 85,
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
  "free_gib": 81.2796630859375,
  "waited_seconds": 0.001,
  "cap": 2
}
```

farm_test_seconds:
```json
10542.583
```

skip_inventory:
```json
{
  "counts_by_reason_class": {
    "other": 285,
    "environment-absence": 71
  },
  "top_level_counts_by_reason_class": {
    "other": 213,
    "environment-absence": 42
  },
  "full_inventory": "/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/skip-inventory.json"
}
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
      "log": "/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/floor-logs/floor-0.log",
      "exit_code": 0
    },
    {
      "package": "examples/gastown",
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
      "log": "/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/floor-logs/floor-1.log",
      "exit_code": 0
    },
    {
      "package": "internal/beads",
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
      "log": "/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/floor-logs/floor-2.log",
      "exit_code": 0
    },
    {
      "package": "internal/beads/beadstest",
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
      "log": "/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/floor-logs/floor-3.log",
      "exit_code": 0
    },
    {
      "package": "internal/beads/contract",
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
      "log": "/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/floor-logs/floor-4.log",
      "exit_code": 0
    },
    {
      "package": "internal/beads/exec",
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
      "log": "/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/floor-logs/floor-5.log",
      "exit_code": 0
    },
    {
      "package": "internal/doctor",
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
      "log": "/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/floor-logs/floor-6.log",
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
      "log": "/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/floor-logs/floor-7.log",
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
      "log": "/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/floor-logs/floor-8.log",
      "exit_code": 0
    },
    {
      "package": "internal/runtime/runtimetest",
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
      "log": "/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/floor-logs/floor-9.log",
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
      "log": "/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/floor-logs/floor-10.log",
      "exit_code": 0
    },
    {
      "package": "test/acceptance/helpers",
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
      "log": "/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/floor-logs/floor-11.log",
      "exit_code": 0
    },
    {
      "package": "test/integration",
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
      "log": "/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/floor-logs/floor-12.log",
      "exit_code": 0
    },
    {
      "check": "fsys-darwin-compile",
      "tests": [],
      "command": [
        "make",
        "test-fsys-darwin-compile"
      ],
      "log": "/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/floor-logs/floor-13.log",
      "exit_code": 0
    },
    {
      "check": "push-gate-lock-selftest",
      "tests": [],
      "command": [
        "bash",
        "scripts/test-push-gate-lock.sh"
      ],
      "log": "/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/floor-logs/floor-14.log",
      "exit_code": 0
    },
    {
      "check": "local-concurrency-selftest",
      "tests": [],
      "command": [
        "bash",
        "scripts/test-local-concurrency.sh"
      ],
      "log": "/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/floor-logs/floor-15.log",
      "exit_code": 0
    }
  ]
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
      "farm_test_seconds": 87.963,
      "evidence_errors": [],
      "probe": [
        "GATE_FARM_PROBE verdict=clear waited=0s load1=2.40 load5=6.48 load15=4.52 cpus=32 threshold=50 max_wait=600s"
      ],
      "test": [
        "stores: city proxied-server, rig proxied-server; cpus 32; loadavg 4.48 6.57 4.63 8/1063 49961"
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

Observed host load (remote class and native floor are separate measurements):
```
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=51.82 run_max_load=109.42 run_mean_load=65.55 run_readings=108 wait_first_load=51.82 wait_max_load=51.82 wait_mean_load=51.82 wait_readings=1 read_errors=0
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1802 wait_timed_out=1 run_start_load=96.35 run_max_load=109.34 run_mean_load=103.54 run_readings=11 wait_first_load=47.22 wait_max_load=97.35 wait_mean_load=76.75 wait_readings=61 read_errors=0
```

## Reviewed native-only skip reasons

Native environment-absence top-ups reran every named parent. On this host herdr is installed, so its existing first-entry RequireLive gate reports explicit unit-lane opt-in rather than the farm missing-binary message. SkipReason checks only testing.Short, LookPath, GC_FAST_UNIT and GC_HERDR_LIVE_TESTS before any tested behavior; all seven sites and helper are unchanged, no shared hunk reaches them. The documented live profile is separate from the canonical floor; no owned SKIP is excused and all eleven owned/23 required roots remain required actual PASS. This records optional unowned skips, not test evidence for a live provider journey.

Proof: native-skip-review.json; all seven sites read UNCHANGED, source digests retained.

## Skips and by-name results

All skip sites are unchanged and mechanically mapped; no unresolved site or changed skip condition. Environment-absence skips receive the mandatory package-level native floor/top-up. Remaining platform/tool/fixture skips occur before the affected startup code and do not conceal any changed test. The 11 changed tests have no skipped outcome. No tests outside this diff were edited to make the gate pass.

```
GATE_TEST_EVIDENCE logs_dir=/var/tmp/deploy-ga-aj9sx1u.mvggn0nm/c3/evidence/remote/logs logs=513 logs_with_results=447
test_counts: PASS=126573 FAIL=0 SKIP=370
  (top-level result lines, column 0, summed over the 513 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
diff_tests_executed:
  TestEnsureRunning_CachedRunningTakesNoFreshReadAndStartsNothing PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_CompositeInFrontOfCachingProviderReadsTheLeafFresh PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_ConfirmedNotRunningStartsOnce PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_ProviderWithLivenessReadButNoFreshReadTakesNoExtraRead PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_ProviderWithoutFreshReadKeepsItsIsRunningReadsBeforeStart PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_SessionExistsConvergesOnFreshRead PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_StaleAbsentSnapshotOverLiveRuntimeConverges PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_StaleKeyCheckOnConfirmedAbsenceStillRetriesFresh PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_StaleKeyCheckOnUnconfirmedLivenessDoesNotRetainACommittedConversation PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_StaleKeyCheckOnUnconfirmedLivenessKeepsResumeState PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_UnconfirmedLivenessStartsNothingAndChangesNothing PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
GATE_TEST_EVIDENCE_RESULT ok: 11 diff-owned tests verified by name
```

All 23 required roots below comprise the 11 changed tests plus 12 unchanged acceptance pins. Ownership comes from owned.tsv; required.tsv is the stricter execution checklist.
```
  TestEnqueuePreparedStartWaveReleasesItsSlotWhenAPostStartObservationHangs PASS (integration-packages-75af79934639abaae69c.log, unit-27b23e708b477fce4dad.log)
  TestExecutePreparedStartWaveDefersErrStateSyncRecoveryWhenObservationUnavailable PASS (integration-packages-18745ddf7d62465654f5.log, unit-912ab643330b35bd0b6b.log)
  TestReadRuntimeClassifiesTheFreshRead PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-3d6d3036a2f5902472b5.log)
  TestRunPreparedStartCandidateDefersWhenAPostStartObservationHangs PASS (integration-packages-e5330b8c981927473156.log, unit-f679cd2551fcc64525aa.log)
  TestRunPreparedStartCandidateLeavesOneAbandonedObservationPerSession PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestRunPreparedStartCandidateSkipsTheCollisionObservationAfterTheStartDeadline PASS (integration-packages-12697e2ea0d32c278115.log, unit-2bde55fab377f50b4739.log)
  TestRunPreparedStartCandidateUsesAnObservationAnswerJustBeforeTheBound PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-23e7d9fc763496dd8095.log)
  TestTutorial01 PASS (integration-packages-b72a67a2f9444af705e3.log; also SKIP: TestTutorial01 in unit-3d6d3036a2f5902472b5.log (PASS in another job))
  TestCompositesForwardEveryLeafCapability PASS (unit-12382fa48a81def0b895.log)
  TestEnsureRunning_CachedRunningTakesNoFreshReadAndStartsNothing PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_CompositeInFrontOfCachingProviderReadsTheLeafFresh PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_ConfirmedNotRunningStartsOnce PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_ProviderWithLivenessReadButNoFreshReadTakesNoExtraRead PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_ProviderWithoutFreshReadKeepsItsIsRunningReadsBeforeStart PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_SessionExistsConvergesOnFreshRead PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_StaleAbsentSnapshotOverLiveRuntimeConverges PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_StaleKeyCheckOnConfirmedAbsenceStillRetriesFresh PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_StaleKeyCheckOnUnconfirmedLivenessDoesNotRetainACommittedConversation PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_StaleKeyCheckOnUnconfirmedLivenessKeepsResumeState PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestEnsureRunning_UnconfirmedLivenessStartsNothingAndChangesNothing PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestSubmitDefaultCodexDismissesDeferredDialogsOnFirstDelivery PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestSessionHandleMessageInterruptNowUsesWorkerBoundary PASS (unit-481d1d821b2235b34d5b.log)
  TestSessionHandleNudgeWaitIdleUsesWorkerBoundary PASS (unit-481d1d821b2235b34d5b.log)
```

## Retained records

Evidence root: /var/tmp/deploy-ga-aj9sx1u.mvggn0nm
C6 original stdout/stderr, three fast-lane logs, per-phase BEP and raw artifacts, floor logs, skip inventory/site proof and independent final audit are retained. No source rebase, force push, hook bypass, runtime deployment or merge was performed by the gate.
