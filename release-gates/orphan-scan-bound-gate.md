**Verdict:** **PASS**

# Bound orphan scans before session start

2026-10-30T20:30:01.675032+00:00

Bead: ga-wb0ttep; build ga-4szhuy; review ga-9gjem0u (R5 PASS, zero unresolved HIGH/blockers).
Reviewed source: c59c2143f61163a0a51e36a06f7e1ccfee4d6f1b (resolved commit; never a builder-branch tip).
Provenance branch: builder/ga-n8cneyp (not a push target).
Frozen base: origin/main@adf99aa6c84d83bfc1492de5bd969231a7edf7f0
merge_state: clean; files: none
gate_base_kind: current-tip; gate_base_sha: adf99aa6c84d83bfc1492de5bd969231a7edf7f0
Tested canonical merge: 0d781d66b8fab4cf6c06434827fe1056adad7005; tree: 77737e729c79cf7ae1b0fdf55b2de1b933320b1c
criterion3_city_evidence_commit: 51866795a7906f67f79f667a172745e1cb07ab3f (captured before launch).
Evidence root: /var/tmp/deploy-ga-wb0ttep.xa23xylo

| # | Criterion | Evidence |
|---|---|---|
| 1 | PASS — Review | R5 ga-9gjem0u reviewed the exact resolved source above; original RED/GREEN and mutation receipts retained. No review carryover or reviewer-test repin. |
| 2 | PASS — Acceptance | All28 owned roots and45 total required roots report real PASS in the full suite. All28 owned roots also PASS in each of3 required race runs; manifest roots PASS. Runtime and source audit below. |
| 3 | PASS — Tests/CI | Full canonical five-phase C3:502 fresh uncached first-attempt remote results;16 native checks exit0. {"PASS": 134243, "FAIL": 0, "SKIP": 377} top-level result lines (repeated jobs count repeatedly). All required CI leaf jobs accounted below. Additional race-command nogo validation failure is explicitly attributed, never called a clean build. |
| 4 | PASS — Review findings | Zero unresolved HIGH/blockers. Accepted limit: one outstanding goroutine/latch per city/session can remain until its provider scan returns. |
| 5 | PASS — Clean | Role and canonical scratch clean; canonical tree rechecked immediately before sealing. Isolated branch must preserve the reviewed source and add only this gate record. |
| 6 | PASS — Merge/build/vet | Clean normal-hook merge at the frozen current tip; active .githooks checked; full-target Bazel build/nogo/vet passed. An initial admission refusal ran no vet command; only that unexecuted command was later admitted at the identical canonical tree. |
| 7 | PASS — Theme | One orphan-scan start-path bound, with tmux empty-root return, classifier tests and requirement/BUILD bookkeeping; unchanged ancestry helper passed confirmed feature IDs and denylist. |

## Acceptance verification

Runtime verification: COMPLETE for the full canonical suite, all owned/required names and required race/manifest tests. Final CI-required accounting is now complete as recorded below.

One F9 orphan-scan feature. Resolved reviewed source c59c2143f61163a0a51e36a06f7e1ccfee4d6f1b; frozen main adf99aa6c84d83bfc1492de5bd969231a7edf7f0; clean canonical merge 0d781d66b8fab4cf6c06434827fe1056adad7005 / tree 77737e729c79cf7ae1b0fdf55b2de1b933320b1c. Source fixture fix24d8bd2f57e336c9bad03bcfe9a1ef0961942a8b and T3bridge fix648f3b5e3fc4d1ee2bf17d57a4502bec0cfd765a are ancestors. R5 review and current RulingC are binding inputs; the old own-base failed deploy gate receives no credit.

AC1/13: verbatim historical RED recovered from reviewer receipt, with explicit runtime/helper and Manager compile-error classification, no Manager assertion RED, tmux and both CLI subtests assertion failures. Current source ancestry of RED/GREEN resolved. PR draft says Refs #7170, names the frozen base, explains expiry versus returned-error log-and-kill, preserves R5 mutation attribution, contains no deployment request and makes no current GREEN claim yet. No issue/contributor comments will be posted.
AC2/9: tmux len(found)==0 returns found/scanErr before ListRunning/GetMeta; D1 fail-closed comment and tracking path with actual roots remain intact. Two owned tests count zero executor calls for empty and error roots.
AC3: typed sentinel wraps runtime-unavailable; bounded helper uses WithTimeout, one per-key latch, buffered result delivery, deletion before result delivery, deadline-result recheck and panic logging. Owned helper tests cover expiry, cancel, completed result, boundary tie, one outstanding call, key release, per-key isolation, no leftover goroutine after release and panic/log identity.
AC4/5/6: Manager initializes unexported budget10s, builds a distinct orphan-scan/city/session key, returns typed unfinished-scan error before any kill; Start/StartRuntimeOnly/Create tests pin zero launch/terminate and rollback/log semantics. Returned-error cleanup still logs and processes safe matching roots. Context is checked before each termination, with remaining count and unavailable/context identity. Unchanged identity/infrastructure fences and fail-closed tests require fresh named PASS.
AC7: production classifier is unchanged; two added CLI roots and the two explicit subcases pin deferral before cancel/deadline classification. Unchanged lease/slot witnesses supplement this source audit; no claim that the new tests alone assert every pre-existing lease/slot invariant.
AC8: all26 non-CLI owned roots passed in EACH of3 race runs of the complete runtime/tmux/session family; both CLI classifier roots passed in EACH of3 focused race runs. There were18 uncached first-attempt remote target results and no DATA RACE warning. Original family/classifier commands exit1 due to gocritic failing to load fmt while validating unchanged support packages; original failures are retained, independently reproduced without the feature at frozen main, and attributed under all four clauses to ga-38b2nor. No test failed or was retried. All36 raw race log/XML files were hash-verified; family artifacts were recovered read-only from original CAS after the initial collector lacked local XML copies. The final manifest check executed successfully with its actual named roots verified. Final supplementary receipt: orphan-required-supplement-final-proof.json.
AC10/11: generated BUILD target drift is required fresh; SESSION-RUNTIME-017 occurs once in the canonical tree. No Provider/ProcessTableScanner signature, configuration, CLI/API/event, worker.ObserveBounded or Suspend path changed. Exported helper/sentinel have current-behavior doc comments; the budget is a const/Manager field, not a package variable.
AC12: fresh normal-hook merge/build/nogo/vet, every policy/generated fast lane, zero-growth complexity on pinned main, full canonical C3 and active hooks are required. Mintlify/dashboard are not applicable: no docs-site or API/dashboard code changes.

Scope remains11 tightly coupled files. Ancestry scope helper passed with confirmed same-feature IDs. Review has zero unresolved HIGH/blocking findings. Known accepted limit: a truly wedged scan keeps one goroutine/latch for that city/session until it returns; it is bounded, not cancellable.


## Execution and scope

test_cmd_scope: full-suite
test_cmd: gate-base.sh run -- gate-criterion3.sh, with the complete local Bazel fallback supplied and unused. The remote chain runs unit, acceptance, integration-packages, full integration minus the latency inventory, then only latency under a clear farm probe.
test_exec_mode: remote-exec (derived from BEP; native floor is recorded separately, not a stitched local suite).
heavy_mode: none (mandatory classifier checked); ICU74 verified; rootless Podman/image and pinned bd verified before C3.
waiver_ref: none; no Gascity waiver path.
failure_attribution (canonical full C3): none; no failed candidate test was retried.
policy_attribution (additional race validation): ga-38b2nor, positive same-run BASE-REF reproduction; all31 family failure packages and additional classifier failure packages are unchanged/outside the feature. Original commands exit1; all18 actual race target results PASS. Full C3/nogo without the race configuration is clean.
policy_lane: all11 targets, formatting and guards PASS in independent fresh views before runtime/suite.
drift_lane: make bazel-sync and clean diff/status plus all7 generated-file targets PASS; OpenAPI breaking check PASS, each lane in an independent fresh view pinned by gate-base.sh. Early fast-lane cache results are not claimed as executions: every one of these19 target labels also executed uncached, remote, attempt1 and PASS in canonical C3 unit (fast-targets-current-farm-proof.json).
ci_lane_run: n/a (no CI-config change).
docs_site_and_dashboard: n/a (no docs-site/API/dashboard change).
isolation: PASS-THROUGH / NOT ISOLATED, as printed by the wrapper; no clean-ambient-environment claim.
test_log_dir: /var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs; native /var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs; supplementary /var/tmp/deploy-ga-wb0ttep.xa23xylo/orphan-acceptance-combined-logs. Raw logs/XML remain retained.
skip_justification: every remaining skip is non-owned; literal reasons and exact sites below. Every environment-absence parent received native top-up, and native SKIPs were inventoried. All actual skip sites are UNCHANGED with zero shared hunks; shared fake/synctest hunks affect owned tests only.

## C3 execution record

```json
{
  "test_exec_mode": "remote-exec",
  "fallback_reason": null,
  "exit_code": 0,
  "test_cmd_scope": "full-suite",
  "evidence_dir": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote",
  "admission": {
    "class": "remote",
    "floor_gib": 25.0,
    "free_gib": 50.76187515258789,
    "waited_seconds": 0.001,
    "cap": 2
  },
  "rbe_state": {
    "rc_marker_present": true,
    "kill_switch_absent": true,
    "cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT",
    "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98",
    "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"
  },
  "test_exec_bep": {
    "unit": {
      "results": 378,
      "remote_executed": 378,
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
  },
  "farm_test_seconds": 12363.272,
  "criterion3_valid": true,
  "latency_leg": {
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
        "farm_test_seconds": 69.267,
        "evidence_errors": [],
        "probe": [
          "GATE_FARM_PROBE waiting waited=0s load1=51.35 load5=91.52 load15=57.94 cpus=32 threshold=50 max_wait=600s",
          "GATE_FARM_PROBE verdict=clear waited=10s load1=43.69 load5=88.56 load15=57.34 cpus=32 threshold=50 max_wait=600s"
        ],
        "test": [
          "stores: city proxied-server, rig proxied-server; cpus 32; loadavg 27.40 79.55 55.48 4/889 52452"
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
  },
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
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-cb110414c557c0928e82.log",
            "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestReplay",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
            "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSessionEventPumpLiveHerdr",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-18745ddf7d62465654f5.log",
            "reason": "session_event_pump_live_test.go:23: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSessionEventPumpLiveHerdr",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
            "reason": "session_event_pump_live_test.go:23: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSimCoverageGate",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-568812e4d525bdd1f974.log",
            "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSimCoverageGate",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
            "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-18745ddf7d62465654f5.log",
            "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-752a0bf25b15b465517a.log",
            "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-9c8cd94da10f0dd04b00.log",
            "reason": "tmux_leak_guard_mntns_test.go:98: bwrap not installed; cannot build a private-/tmp mount namespace",
            "class": "environment-absence"
          },
          {
            "package": "cmd/gc",
            "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
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
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-0.log",
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
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-be91eec86b4dbf7e66f1.log",
            "reason": "gastown_test.go:4153: zsh not installed",
            "class": "environment-absence"
          },
          {
            "package": "examples/gastown",
            "test": "TestAttachedRigScopeShellToken/zsh",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ef24b9dbe59f5d04fac1.log",
            "reason": "gastown_test.go:4153: zsh not installed",
            "class": "environment-absence"
          },
          {
            "package": "examples/gastown",
            "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-a0f68450216c5e4d04af.log",
            "reason": "maintenance_scripts_test.go:9330: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
            "class": "environment-absence"
          },
          {
            "package": "examples/gastown",
            "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-df0dcd5ff9a717b4f2a3.log",
            "reason": "maintenance_scripts_test.go:9330: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
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
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-1.log",
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
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-86f8e1ae5bca4d4a5e22.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/BatchClearsOneKeyKeepsAnother",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27371dea3b130a4c231f.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-86f8e1ae5bca4d4a5e22.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27371dea3b130a4c231f.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-86f8e1ae5bca4d4a5e22.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27371dea3b130a4c231f.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-86f8e1ae5bca4d4a5e22.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27371dea3b130a4c231f.log",
            "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
            "class": "environment-absence"
          },
          {
            "package": "internal/beads",
            "test": "TestProbeVersionsAgainstHostBinaries/bd",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-363a36f2662d541bc75e.log",
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
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-2.log",
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
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-3.log",
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
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8544388e16a0d6b0cdb0.log",
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
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-4.log",
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
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-64046beff95fb28ccd66.log",
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
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-5.log",
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
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
            "reason": "checks_custom_types_test.go:467: bd binary not on PATH",
            "class": "environment-absence"
          },
          {
            "package": "internal/doctor",
            "test": "TestCustomTypesCheck_TableDrift",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
            "reason": "checks_custom_types_test.go:297: bd binary not on PATH",
            "class": "environment-absence"
          },
          {
            "package": "internal/doctor",
            "test": "TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
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
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-6.log",
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
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
            "reason": "mcp_conformance_test.go:159: MCP mail script not found: file does not exist",
            "class": "environment-absence"
          },
          {
            "package": "internal/mail/exec",
            "test": "TestMCPMailConformance",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
            "reason": "mcp_conformance_test.go:26: MCP mail script not found: file does not exist",
            "class": "environment-absence"
          },
          {
            "package": "internal/mail/exec",
            "test": "TestMCPMailCrossPodNameResolution",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
            "reason": "mcp_conformance_test.go:195: MCP mail script not found: file does not exist",
            "class": "environment-absence"
          },
          {
            "package": "internal/mail/exec",
            "test": "TestMCPMailProjectKeyIsolation",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
            "reason": "mcp_conformance_test.go:286: MCP mail script not found: file does not exist",
            "class": "environment-absence"
          },
          {
            "package": "internal/mail/exec",
            "test": "TestMCPMailReplyMissingOriginalIsNotFound",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
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
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-7.log",
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
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
            "reason": "activity_live_test.go:20: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestActivityLive",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
            "reason": "activity_live_test.go:20: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestHerdrConformance",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
            "reason": "conformance_test.go:18: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestHerdrConformance",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
            "reason": "conformance_test.go:18: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestProviderLive",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
            "reason": "provider_live_test.go:15: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestProviderLive",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
            "reason": "provider_live_test.go:15: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestProviderLiveClaudeKindPath",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
            "reason": "kindpath_live_test.go:105: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestProviderLiveClaudeKindPath",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
            "reason": "kindpath_live_test.go:105: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestProviderLiveOccupantSwapKeepsLiveness",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
            "reason": "panebinding_live_test.go:21: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestProviderLiveOccupantSwapKeepsLiveness",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
            "reason": "panebinding_live_test.go:21: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestSessionEventsLive",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
            "reason": "events_live_test.go:40: herdr not installed",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/herdr",
            "test": "TestSessionEventsLive",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
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
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-8.log",
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
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:238: Start(skip-start-14): provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/IsRunning_ConcurrentDistinctSessions",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:271: Start(skip-start-18): provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ConcurrentDistinctPrefixes",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:367: Start(skip-start-25): provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_EmptyPrefix",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:349: Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ExcludesStopped",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:333: Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_FindsSessions",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:294: Start skip-start-21: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_PrefixFiltering",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:313: Start skip-start-22: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_ConcurrentDistinctSessions",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:421: Start(skip-start-30): provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_EmptyNamesReturnsTrue",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:395: Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_FalseAfterStop",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:404: Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/SharedSession",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:61: Start shared session: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_ConcurrentDistinctSessions",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:124: concurrent Start(skip-start-3): provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_CreatesRunningSession",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:84: Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_DuplicateReturnsError",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:93: first Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_ConcurrentDistinctSessions",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:209: Start(skip-start-11): provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_AlreadyStopped",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:193: Start: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_NotRunning",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
            "reason": "conformance.go:170: Start sentinel: provider environment unavailable",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/runtimetest",
            "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_MakesSessionNotRunning",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
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
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
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
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
            "reason": "tmux_test.go:3663: not inside tmux \u2014 need server for bind-key",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/tmux",
            "test": "TestGetKeyBinding_SkipsGasTownBindings",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
            "reason": "tmux_test.go:3637: not inside tmux \u2014 need server for bind-key",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/tmux",
            "test": "TestIsGTBinding_DetectsGasTownBindings",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-15c091b746137e5c2921.log",
            "reason": "tmux_test.go:3689: not inside tmux \u2014 need server for bind-key",
            "class": "environment-absence"
          },
          {
            "package": "internal/runtime/tmux",
            "test": "TestSetBindings_PreserveFallbackOnRepeatedCalls",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
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
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-10.log",
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
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
            "reason": "bd_schema_parity_test.go:121: bd not available",
            "class": "environment-absence"
          },
          {
            "package": "test/acceptance/helpers",
            "test": "TestBdLatestSchemaVersionLeavesNothingInItsTempRoot",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
            "reason": "bd_schema_parity_test.go:144: bd not available",
            "class": "environment-absence"
          },
          {
            "package": "test/acceptance/helpers",
            "test": "TestRealBdProvisionLeavesTheHostHomeAlone",
            "status": "SKIP",
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
            "reason": "tool_home_test.go:317: no bd available (set GC_ACCEPTANCE_BD_BIN)",
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
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-11.log",
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
            "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-23490fe556f84649266a.log",
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
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-12.log",
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
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-13.log",
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
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-14.log",
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
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-15.log",
        "exit_code": 0
      }
    ],
    "log_dir": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs"
  },
  "skip_inventory": {
    "counts_by_reason_class": {
      "other": 292,
      "environment-absence": 71
    },
    "top_level_counts_by_reason_class": {
      "other": 220,
      "environment-absence": 42
    },
    "tests": [
      {
        "package": "cmd/gc",
        "test": "TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
        "reason": "cmd_bd_test.go:1314: requires a managed bd lifecycle city; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestBdRuntimeEnvUsesValidProviderStateWhenPublishedStateIsMissing",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-64014bc0bb3833bc198c.log",
        "reason": "bd_env_test.go:1396: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestBuildDesiredState_MinZeroDefaultScaleCheckRoutedWorkCreatesPoolSession",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "build_desired_state_test.go:6331: uses real bd subprocesses for routed-work scale checks; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestBuildDesiredState_PoolCheckUsesManagedCityDoltPortWhenRigHasNoOverride",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4d20b3d28497f844aa39.log",
        "reason": "build_desired_state_test.go:10104: uses a live managed-dolt port probe for scale_check coverage; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestBuildPinnedBDBinaryForTestsUsesGoModSource",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
        "reason": "cmd_wait_test.go:777: builds a real bd binary from source; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestChooseManagedDoltPortKeepsOwnServerPortBeforeItListens",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
        "reason": "cmd_dolt_state_test.go:447: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestChooseManagedDoltPortKeepsOwnServerPortWhenDialsFail",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
        "reason": "cmd_dolt_state_test.go:398: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestChooseManagedDoltPortKeepsPublishedHintPortBeforeItsServerListens",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-752a0bf25b15b465517a.log",
        "reason": "cmd_dolt_state_test.go:485: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestChooseManagedDoltPortSkipsStatePortHeldByAnotherProcess",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
        "reason": "cmd_dolt_state_test.go:526: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestCityInitServiceInitScaffoldsAndFinalizes",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-493754ea2d5b4d8858c8.log",
        "reason": "cityinit_impl_test.go:259: runs the full local init scaffold/finalize path; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestCityRuntimeTick_RefreshesManualSessionOverlayAfterSync",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-088cc38d26d094729229.log",
        "reason": "city_runtime_test.go:4327: runs a full runtime tick/reconcile path; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestCmdGCRealBDTestsUseTestOwnedDoltContext",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
        "reason": "pool_test.go:215: uses real bd to prove test-owned HOME isolation; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestCmdMailInbox_NormalizesCanonicalManagedProviderEnvAndReadsInbox",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-637c74c578a2b17fc654.log",
        "reason": "cmd_mail_test.go:1849: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestControllerReloadCityNameChange",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
        "reason": "controller_test.go:1808: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestControllerReloadInvalidConfig",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
        "reason": "controller_test.go:1746: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestControllerStateMutationRefreshKeepsBuiltinOrdersAndClearsPending",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
        "reason": "api_state_test.go:622: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestControllerStateRuntimeUpdateAcceptsBuiltinAwareRevision",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-423c6a0da1986b90e9a7.log",
        "reason": "api_state_test.go:582: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoBeadsCityUseExternalAdoptUnverifiedSkipsValidation",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
        "reason": "cmd_beads_city_test.go:677: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoBeadsCityUseExternalRewritesCompatRigWithRelativePath",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "cmd_beads_city_test.go:485: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoBeadsCityUseExternalUpdatesIncludedInheritedRigs",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
        "reason": "cmd_beads_city_test.go:188: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoBeadsCityUseExternalWritesVerifiedCityAndInheritedRigs",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
        "reason": "cmd_beads_city_test.go:90: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoDoctorRegistersStaleLocalPackDirCheck",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f679cd2551fcc64525aa.log",
        "reason": "cmd_doctor_test.go:479: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoDoctorRegistersStaleLocalPackDirCheckForDefaultRigRemoteImport",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4d20b3d28497f844aa39.log",
        "reason": "cmd_doctor_test.go:599: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoPrimeWithHook_CodexJSONFormatInfersAgentFromWorkDir",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
        "reason": "cmd_prime_test.go:1336: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoPrimeWithHook_DeliveredStartupPromptCodexJSONHookFormat",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
        "reason": "cmd_prime_test.go:1269: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateAllocatePortCmdRepairsMissingCanonicalProviderStateFromPublishedHint",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2ef13e1b6898c5544764.log",
        "reason": "cmd_dolt_state_test.go:960: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateAllocatePortCmdRepairsMissingProviderStateFromPublishedHint",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4d20b3d28497f844aa39.log",
        "reason": "cmd_dolt_state_test.go:905: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateAllocatePortCmdRepairsStaleProviderStateFromOwnedLivePortHolder",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-23e7d9fc763496dd8095.log",
        "reason": "cmd_dolt_state_test.go:801: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateAllocatePortCmdRepairsStaleWrongPortProviderStateFromPublishedHint",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-64014bc0bb3833bc198c.log",
        "reason": "cmd_dolt_state_test.go:1023: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateAllocatePortCmdRepairsStoppedProviderStateFromOwnedLivePortHolder",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2bde55fab377f50b4739.log",
        "reason": "cmd_dolt_state_test.go:853: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateAllocatePortCmdReusesLiveProviderState",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
        "reason": "cmd_dolt_state_test.go:560: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateExistingManagedCmdFallsBackToPublishedRuntimeState",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "cmd_dolt_state_test.go:1720: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateExistingManagedCmdKeepsOwnedServerWhenDialsFailLocally/dials_never_leave_this_host",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "cmd_dolt_state_test.go:1661: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateExistingManagedCmdKeepsOwnedServerWhenDialsFailLocally/server_refuses",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "cmd_dolt_state_test.go:1661: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateExistingManagedCmdRejectsForeignListenerBackedOnlyByStateDataDir",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
        "reason": "cmd_dolt_state_test.go:1535: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateExistingManagedCmdReportsReusableOwnedServer",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
        "reason": "cmd_dolt_state_test.go:1598: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateInspectManagedCmdReportsPortHolderOwnership",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-423c6a0da1986b90e9a7.log",
        "reason": "cmd_dolt_state_test.go:1307: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateInspectManagedCmdUsesPIDFileAndStateOwnership",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
        "reason": "cmd_dolt_state_test.go:1184: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStatePreflightCleanCmdPreservesLiveArtifacts",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
        "reason": "cmd_dolt_state_test.go:1888: spawns managed dolt holder processes; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateProbeManagedCmdKeepsOwnedHolderRunningWhenDialFailsLocally/dial_never_leaves_this_host",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
        "reason": "cmd_dolt_state_test.go:1407: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateProbeManagedCmdKeepsOwnedHolderRunningWhenDialFailsLocally/server_refuses",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
        "reason": "cmd_dolt_state_test.go:1407: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateProbeManagedCmdReportsImposterHolder",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
        "reason": "cmd_dolt_state_test.go:1445: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateProbeManagedCmdReportsRunningOwnedHolder",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
        "reason": "cmd_dolt_state_test.go:1347: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateRecoverManagedCmdReportsReadOnlyAndRestarts",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "cmd_dolt_state_test.go:3051: spawns managed dolt recovery processes; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateStopManagedCmdDoesNotKillImposterPortHolder",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
        "reason": "cmd_dolt_state_test.go:3010: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateStopManagedCmdStopsManagedPID",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-752a0bf25b15b465517a.log",
        "reason": "cmd_dolt_state_test.go:2888: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestDoltStateWaitReadyCmdReturnsReady",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
        "reason": "cmd_dolt_state_test.go:2780: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestE1PreLeafBooleanHelpNoScopeEager",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-514989cc035c0e9a8ff5.log",
        "reason": "cmd_commands_test.go:814: ga-klo4gz: this test's purpose is exercising ambient cwd-based city resolution (resolveContextFromDir step 10) to distinguish the ambient city's commands from an explicitly-selected one, which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestE1PreLeafBooleanHelpNoScopeEager",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "cmd_commands_test.go:814: ga-klo4gz: this test's purpose is exercising ambient cwd-based city resolution (resolveContextFromDir step 10) to distinguish the ambient city's commands from an explicitly-selected one, which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestEnsureBdRuntimeCustomTypesHealsUpgradedNativeStore",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
        "reason": "gc_beads_bd_config_commit_test.go:457: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestEnsureBeadsProvider_bdAcceptsHealthyServerAfterStartError",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
        "reason": "beads_provider_lifecycle_test.go:1724: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestEvaluatePoolDefaultScaleCheckCountsRoutedReadyWork",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "pool_test.go:108: uses real bd and jq for default scale_check coverage; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestEvaluatePoolDefaultScaleCheckIgnoresRoutedActiveUnassignedWork",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-088cc38d26d094729229.log",
        "reason": "pool_test.go:154: uses real bd and jq for default scale_check coverage; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestEventsMirrorInheritsSetgidGroup",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-9326181a736eb30b71ca.log",
        "reason": "providers_events_mirror_test.go:220: process has no supplementary group to test setgid inheritance with",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestEventsMirrorInheritsSetgidGroup",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2ef13e1b6898c5544764.log",
        "reason": "providers_events_mirror_test.go:220: process has no supplementary group to test setgid inheritance with",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
        "reason": "cmd_bd_test.go:1478: requires a managed bd lifecycle city; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBdUsesEnclosingRigWhenNoFlag",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-9c8cd94da10f0dd04b00.log",
        "reason": "cmd_bd_test.go:1851: ga-klo4gz: this test's purpose is exercising resolveContextFromDir's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit GC_CITY/GC_CITY_PATH/GC_CITY_ROOT override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBdUsesEnclosingRigWhenNoFlag",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-64014bc0bb3833bc198c.log",
        "reason": "cmd_bd_test.go:1851: ga-klo4gz: this test's purpose is exercising resolveContextFromDir's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit GC_CITY/GC_CITY_PATH/GC_CITY_ROOT override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdInitEnsuresProjectIdentityWhenMetadataExistsWithoutProjectID",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
        "reason": "beads_provider_lifecycle_test.go:6678: runs the materialized gc-beads-bd init script with GC_BIN helper; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdInitNormalizesScopeAndRemovesLocalServerArtifacts",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2ef13e1b6898c5544764.log",
        "reason": "beads_provider_lifecycle_test.go:12979: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdProviderOwnedRealInitIgnoresAncestorBeadsWorkspace",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
        "reason": "beads_provider_script_scope_lifecycle_linux_test.go:352: runs the real bd provider init; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdProviderOwnedRealLifecycleStopsOwnedProcesses",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-23e7d9fc763496dd8095.log",
        "reason": "beads_provider_lifecycle_test.go:14191: runs the real bd provider lifecycle; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdStartConcurrentWaitPassesRemainingExistingManagedBudget",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-088cc38d26d094729229.log",
        "reason": "beads_provider_lifecycle_test.go:11057: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdStartDoesNotReplaceLiveLockFileInode",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
        "reason": "beads_provider_lifecycle_test.go:10309: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdStartIgnoresReachableCompatPortFileInput",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
        "reason": "beads_provider_lifecycle_test.go:9694: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdStartIsIdempotentWhenAlreadyRunning",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2ef13e1b6898c5544764.log",
        "reason": "beads_provider_lifecycle_test.go:11681: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdStartLeavesForeignPortHolderAlone",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-493754ea2d5b4d8858c8.log",
        "reason": "beads_provider_lifecycle_test.go:11933: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdStartRestartsServerHoldingDeletedDataInodes",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-64014bc0bb3833bc198c.log",
        "reason": "beads_provider_lifecycle_test.go:11768: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdStartUsesRootBeadsDataDir",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
        "reason": "beads_provider_lifecycle_test.go:5145: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdStartWaitsForConcurrentStarterSuccess",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "beads_provider_lifecycle_test.go:10495: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestGcBeadsBdStartWaitsForSlowConcurrentStarterSuccess",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "beads_provider_lifecycle_test.go:10778: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestHealthBeadsProviderWaitsForStorePingAfterRecovery",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-0e1499ad8e8a0a6b9d81.log",
        "reason": "beads_provider_lifecycle_test.go:5696: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestInstallSupervisorSystemdWarmRefreshLeavesUnregisteredWorkspaceServices",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2bde55fab377f50b4739.log",
        "reason": "cmd_supervisor_test.go:1851: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestInstallSupervisorSystemdWarmRefreshStopsWorkspaceServicesBeforeStart",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-23e7d9fc763496dd8095.log",
        "reason": "cmd_supervisor_test.go:1691: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestLoadStatusSessionSnapshotKillsBdChildOnTimeout",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "city_status_snapshot_test.go:200: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestManagedDoltConfigGoWriterMatchesShellFallbackSemantics",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4d20b3d28497f844aa39.log",
        "reason": "beads_provider_lifecycle_test.go:11537: starts the materialized gc-beads-bd shell fallback; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestManagedDoltDirtyTablesWithPasswordAgainstRealServer",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
        "reason": "beads_provider_dirty_tables_test.go:345: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestManagedDoltHealthCheckWithPasswordUsesDirectHelpersAgainstRealServer",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
        "reason": "dolt_project_id_test.go:184: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestManagedDoltScopeWatchdogHelper",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-aa6cf5c138f0b61b866a.log",
        "reason": "dolt_scope_watchdog_test.go:177: helper process only",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestManagedDoltScopeWatchdogHelper",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
        "reason": "dolt_scope_watchdog_test.go:177: helper process only",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestManagedDoltServingDataDirWithPasswordAgainstRealServer",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
        "reason": "dolt_identity_test.go:156: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestManagedDoltWaitReadyWithPasswordUsesDirectQueryProbe",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
        "reason": "dolt_project_id_test.go:212: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestManagedDoltWatchdogExternalParentHelper",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-c2ab4e7d4735e1bd66a7.log",
        "reason": "dolt_start_managed_test.go:647: helper process only",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestManagedDoltWatchdogExternalParentHelper",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-088cc38d26d094729229.log",
        "reason": "dolt_start_managed_test.go:647: helper process only",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestPackRegistryLiveGascityPacksCatalog",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-e4245ddf6a37030111bc.log",
        "reason": "cmd_pack_registry_test.go:758: set GC_TEST_GASCITY_PACKS_REGISTRY to a gascity-packs registry.toml source to run this live catalog canary",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestPackRegistryLiveGascityPacksCatalog",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
        "reason": "cmd_pack_registry_test.go:758: set GC_TEST_GASCITY_PACKS_REGISTRY to a gascity-packs registry.toml source to run this live catalog canary",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestPrepareCityForSupervisorPrunesLegacyScripts",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
        "reason": "script_resolve_test.go:313: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestProjectIdentityL3AdapterContractAndManagedComposition",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "dolt_project_id_test.go:281: requires a managed dolt server; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestPublishManagedDoltRuntimeStateRecoversMissingProviderStateWithPortHint",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "dolt_runtime_publication_test.go:130: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestPublishManagedDoltRuntimeStateRecoversStaleWrongPortProviderStateWithPublishedHint",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "dolt_runtime_publication_test.go:204: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestPublishManagedDoltRuntimeStateRepairsStaleProviderState",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
        "reason": "dolt_runtime_publication_test.go:28: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestPublishManagedDoltRuntimeStateSucceedsWhenAlreadyValid",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
        "reason": "dolt_runtime_publication_test.go:294: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRecoverManagedDoltProcessKeepsUnmeasuredServerWithoutReportingItHealthy",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
        "reason": "cmd_dolt_state_test.go:3436: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRecoverManagedDoltProcessReturnsWhenConcurrentStarterBecomesReady",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "cmd_dolt_state_test.go:3216: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRecoverManagedDoltProcessReusesHealthyManagedServerOnReboundPort",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-088cc38d26d094729229.log",
        "reason": "cmd_dolt_state_test.go:3313: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRecoverManagedDoltProcessWaitsForLockHolderWhenServerIsUnmeasured/refused_server",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
        "reason": "cmd_dolt_state_test.go:3511: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRecoverManagedDoltProcessWaitsForLockHolderWhenServerIsUnmeasured/unmeasured_server",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
        "reason": "cmd_dolt_state_test.go:3511: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRecoverManagedDoltProcessWithPasswordReusesHealthyRealServer",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "dolt_project_id_test.go:225: requires a managed dolt server; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRegisterCityWithSupervisorKeepsRegistrationWhenReloadFails",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2bde55fab377f50b4739.log",
        "reason": "cmd_supervisor_city_test.go:350: exercises supervisor registration retry behavior; run make test-cmd-gc-process for scenario coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRegisterRealBDServerStopStopsServerBeforeTempDirCleanup",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f679cd2551fcc64525aa.log",
        "reason": "path_helpers_test.go:1055: spawns subprocesses to simulate still-running dolt sql-servers; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestReloadConfigTracedRescansOrdersWhenConfigRevisionUnchanged",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
        "reason": "cmd_reload_test.go:606: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRenderedCrewPromptShowsProviderSpecificInstructionsFile",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-830685c60d0c2dd88567.log",
        "reason": "prompt_test.go:1420: crew.template.md not readable at ../../examples/gastown/packs/gastown/assets/prompts/crew.template.md: open ../../examples/gastown/packs/gastown/assets/prompts/crew.template.md: no such file or directory",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRenderedCrewPromptShowsProviderSpecificInstructionsFile",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
        "reason": "prompt_test.go:1420: crew.template.md not readable at ../../examples/gastown/packs/gastown/assets/prompts/crew.template.md: open ../../examples/gastown/packs/gastown/assets/prompts/crew.template.md: no such file or directory",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRepairedManagedDoltRuntimeStateAcceptsSymlinkEquivalentDataDir",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-0e1499ad8e8a0a6b9d81.log",
        "reason": "cmd_dolt_state_test.go:189: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRepairedManagedDoltRuntimeStateClassifiesDialFailure",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f679cd2551fcc64525aa.log",
        "reason": "cmd_dolt_state_test.go:760: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestReplay",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-cb110414c557c0928e82.log",
        "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestReplay",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
        "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestResolveCityFlag/flag_empty_fallback",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-75f7330b9b673df65e4f.log",
        "reason": "main_test.go:840: ga-klo4gz: this subtest's purpose is exercising resolveCity's ambient cwd-based fallback (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestResolveCityFlag/flag_empty_fallback",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
        "reason": "main_test.go:840: ga-klo4gz: this subtest's purpose is exercising resolveCity's ambient cwd-based fallback (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestResolveStoredSessionLogSource_UniqueWorkDirFallsBackBeyondLatestAlias",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "cmd_session_logs_test.go:388: probes provider transcript lookup before workdir fallback; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestResolvedRuntimeCityDoltTargetDoesNotMaskInvalidCanonicalConfigWithProviderState",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-637c74c578a2b17fc654.log",
        "reason": "bd_env_test.go:1643: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestReviewBdRigWithEdgesRefusesBeforeAnyWrite",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-493754ea2d5b4d8858c8.log",
        "reason": "rig_import_review_test.go:84: no real bd on PATH",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRigAnywhere_ResolveContext/registered_rig_cwd_ambiguous_falls_through",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-cb110414c557c0928e82.log",
        "reason": "rig_anywhere_test.go:489: ga-klo4gz: this subtest's purpose is exercising the fallthrough from an ambiguous registered-rig match (step 9) to resolveContext's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRigAnywhere_ResolveContext/registered_rig_cwd_ambiguous_falls_through",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
        "reason": "rig_anywhere_test.go:489: ga-klo4gz: this subtest's purpose is exercising the fallthrough from an ambiguous registered-rig match (step 9) to resolveContext's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRigAnywhere_ResolveContext/walk_up_fallback",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-cb110414c557c0928e82.log",
        "reason": "rig_anywhere_test.go:378: ga-klo4gz: this subtest's purpose is exercising resolveContext's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRigAnywhere_ResolveContext/walk_up_fallback",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
        "reason": "rig_anywhere_test.go:378: ga-klo4gz: this subtest's purpose is exercising resolveContext's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRigAnywhere_ResolveContext/walk_up_fallback_with_rig_match",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-cb110414c557c0928e82.log",
        "reason": "rig_anywhere_test.go:402: ga-klo4gz: this subtest's purpose is exercising resolveContext's ambient cwd walk-up (step 10) followed by a rig match, which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRigAnywhere_ResolveContext/walk_up_fallback_with_rig_match",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
        "reason": "rig_anywhere_test.go:402: ga-klo4gz: this subtest's purpose is exercising resolveContext's ambient cwd walk-up (step 10) followed by a rig match, which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRigImportFromARealBdRigRaisesItsMinHashLength",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2bde55fab377f50b4739.log",
        "reason": "rig_import_fixtures_test.go:460: no real bd on PATH",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRunManagedDoltSQLEmptyPasswordMasksDefaultProfileAgainstRealServer",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-493754ea2d5b4d8858c8.log",
        "reason": "dolt_sql_health_test.go:525: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestRunWorkflowServeReturnsControlErrorWithoutQuarantine",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
        "reason": "cmd_convoy_dispatch_test.go:3899: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestScopedBdStoreForCityKillsChildOnCtxCancel",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
        "reason": "scoped_store_test.go:364: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestSendReloadControlRequestInvalidConfig",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
        "reason": "cmd_reload_test.go:740: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestSendReloadControlRequestNoChange",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-423c6a0da1986b90e9a7.log",
        "reason": "cmd_reload_test.go:540: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestSessionEventPumpLiveHerdr",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-18745ddf7d62465654f5.log",
        "reason": "session_event_pump_live_test.go:23: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestSessionEventPumpLiveHerdr",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
        "reason": "session_event_pump_live_test.go:23: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestShellExecRunnerKillsProcessGroupAfterWaitDelay",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "order_dispatch_test.go:3718: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestShellExecRunnerKillsProcessGroupOnTimeout",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "order_dispatch_test.go:3695: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestSimCoverageGate",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-568812e4d525bdd1f974.log",
        "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestSimCoverageGate",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
        "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestSplitStrictControllerSharesOneProjection",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-568812e4d525bdd1f974.log",
        "reason": "split_strict_gate_test.go:175: exercises the split-strict gate (GC_TEST_SPLIT_STRICT=1)",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestSplitStrictControllerSharesOneProjection",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
        "reason": "split_strict_gate_test.go:175: exercises the split-strict gate (GC_TEST_SPLIT_STRICT=1)",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestStartTCPListenerProcessInDirRegistersCleanup",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "cmd_dolt_state_test.go:582: spawns a TCP listener process and verifies cleanup; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestStopCityManagedBeadsProviderAfterSuccessfulStopStopsDefaultBD",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2bde55fab377f50b4739.log",
        "reason": "cmd_stop_test.go:1376: exercises managed bd provider shutdown; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestStopManagedDoltRefusesSIGKILLWhileLockHeld",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-423c6a0da1986b90e9a7.log",
        "reason": "dolt_lifecycle_race_test.go:170: spawns real processes; run via make test-cmd-gc-process",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestStopManagedDoltWaitsForLockReleaseAfterExit",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
        "reason": "dolt_lifecycle_race_test.go:238: spawns real processes; run via make test-cmd-gc-process",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestStopSupervisorWithWaitFailsWhenLaunchdStopFails",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-56f04454dd9158a9ee8a.log",
        "reason": "cmd_supervisor_test.go:6604: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestStopSupervisorWithWaitFailsWhenLaunchdStopFails",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
        "reason": "cmd_supervisor_test.go:6604: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestSupervisorInstallUnsupportedOS",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-509ef0a6cd81866833cc.log",
        "reason": "cmd_supervisor_test.go:1473: unsupported-os test only applies outside darwin/linux",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestSupervisorInstallUnsupportedOS",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
        "reason": "cmd_supervisor_test.go:1473: unsupported-os test only applies outside darwin/linux",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-18745ddf7d62465654f5.log",
        "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-752a0bf25b15b465517a.log",
        "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-9c8cd94da10f0dd04b00.log",
        "reason": "tmux_leak_guard_mntns_test.go:98: bwrap not installed; cannot build a private-/tmp mount namespace",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
        "reason": "tmux_leak_guard_mntns_test.go:98: bwrap not installed; cannot build a private-/tmp mount namespace",
        "class": "environment-absence"
      },
      {
        "package": "cmd/gc",
        "test": "TestTerminateManagedDoltPIDRefusesSIGKILLWhileLockHeld",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-637c74c578a2b17fc654.log",
        "reason": "dolt_lifecycle_race_test.go:148: spawns real processes; run via make test-cmd-gc-process",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestTerminateManagedDoltTestPIDKillsProcessGroup",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "dolt_start_managed_test.go:1635: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestTutorial01",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-752a0bf25b15b465517a.log",
        "reason": "main_test.go:379: runs tutorial testscript scenarios; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestUnloadSupervisorServiceDarwinDisablesBootsOutAndVerifiesAbsent",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-75af79934639abaae69c.log",
        "reason": "cmd_supervisor_test.go:2527: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestUnloadSupervisorServiceDarwinDisablesBootsOutAndVerifiesAbsent",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
        "reason": "cmd_supervisor_test.go:2527: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestUnloadSupervisorServiceDarwinFailsWhenTargetStillLoaded",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-2ab73b52d8fd4dc346df.log",
        "reason": "cmd_supervisor_test.go:2642: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestUnloadSupervisorServiceDarwinFailsWhenTargetStillLoaded",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
        "reason": "cmd_supervisor_test.go:2642: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestUnloadSupervisorServiceDarwinReturnsLaunchctlFailures",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-568812e4d525bdd1f974.log",
        "reason": "cmd_supervisor_test.go:2819: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestUnloadSupervisorServiceDarwinReturnsLaunchctlFailures",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
        "reason": "cmd_supervisor_test.go:2819: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestUnloadSupervisorServiceDarwinWaitsThroughSIGTERMedUntilAbsent",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-514989cc035c0e9a8ff5.log",
        "reason": "cmd_supervisor_test.go:2577: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestUnloadSupervisorServiceDarwinWaitsThroughSIGTERMedUntilAbsent",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
        "reason": "cmd_supervisor_test.go:2577: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestV2G1Budget",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-2ab73b52d8fd4dc346df.log",
        "reason": "v2_g1_test.go:120: gate G1 times this host: set GC_V2_G1=1",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestV2G1Budget",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-0e1499ad8e8a0a6b9d81.log",
        "reason": "v2_g1_test.go:120: gate G1 times this host: set GC_V2_G1=1",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestV2GatherProductionShape",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-509ef0a6cd81866833cc.log",
        "reason": "v2_g1_test.go:178: gate G1b reads a store copy: set GC_V2_G1B_STORE",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestV2GatherProductionShape",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
        "reason": "v2_g1_test.go:178: gate G1b reads a store copy: set GC_V2_G1B_STORE",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestValidDoltRuntimeStateAcceptsSymlinkEquivalentDataDir",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-493754ea2d5b4d8858c8.log",
        "reason": "cmd_dolt_state_test.go:142: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestValidDoltRuntimeStateClassifiesDialFailure",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
        "reason": "cmd_dolt_state_test.go:722: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestValidDoltRuntimeStateRejectsAlivePIDThatDoesNotOwnManagedDolt",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
        "reason": "cmd_dolt_state_test.go:694: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestVerifyExternalDoltEndpointRejectsMissingLocalProjectID",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-637c74c578a2b17fc654.log",
        "reason": "cmd_rig_endpoint_test.go:1689: requires a managed external dolt endpoint; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestVerifyExternalDoltEndpointRejectsProjectIdentityMismatch",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
        "reason": "cmd_rig_endpoint_test.go:1651: requires a managed external dolt endpoint; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestVerifySupervisorServiceStoppedDarwinFailsWhenAbsenceUnconfirmed",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-c2ab4e7d4735e1bd66a7.log",
        "reason": "cmd_supervisor_test.go:2695: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestVerifySupervisorServiceStoppedDarwinFailsWhenAbsenceUnconfirmed",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-088cc38d26d094729229.log",
        "reason": "cmd_supervisor_test.go:2695: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestVerifySupervisorServiceStoppedDarwinHonorsShorterCallerDeadline",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-509ef0a6cd81866833cc.log",
        "reason": "cmd_supervisor_test.go:2742: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestVerifySupervisorServiceStoppedDarwinHonorsShorterCallerDeadline",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
        "reason": "cmd_supervisor_test.go:2742: launchd path only applies on darwin",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestWatchConfigTargets_CloseReturnsFDsToBaseline",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-568812e4d525bdd1f974.log",
        "reason": "controller_watcher_fd_test.go:27: darwin-only: the kqueue Close() FD leak (fsnotify#732) is a Darwin/kqueue bug",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestWatchConfigTargets_CloseReturnsFDsToBaseline",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
        "reason": "controller_watcher_fd_test.go:27: darwin-only: the kqueue Close() FD leak (fsnotify#732) is a Darwin/kqueue bug",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestWorkerHandleForSessionWithConfigUsesResolvedProviderOnFirstStart",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f679cd2551fcc64525aa.log",
        "reason": "worker_handle_test.go:36: waits through stale session-key detection; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "cmd/gc",
        "test": "TestWorkerHandleForSessionWithConfigUsesResolvedProviderOnResume",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2ef13e1b6898c5544764.log",
        "reason": "worker_handle_test.go:1266: waits through stale session-key detection; run make test-cmd-gc-process for full coverage",
        "class": "other"
      },
      {
        "package": "examples/gastown",
        "test": "TestAttachedRigScopeShellToken/zsh",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-be91eec86b4dbf7e66f1.log",
        "reason": "gastown_test.go:4153: zsh not installed",
        "class": "environment-absence"
      },
      {
        "package": "examples/gastown",
        "test": "TestAttachedRigScopeShellToken/zsh",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ef24b9dbe59f5d04fac1.log",
        "reason": "gastown_test.go:4153: zsh not installed",
        "class": "environment-absence"
      },
      {
        "package": "examples/gastown",
        "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-a0f68450216c5e4d04af.log",
        "reason": "maintenance_scripts_test.go:9330: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
        "class": "environment-absence"
      },
      {
        "package": "examples/gastown",
        "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-df0dcd5ff9a717b4f2a3.log",
        "reason": "maintenance_scripts_test.go:9330: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
        "class": "environment-absence"
      },
      {
        "package": "examples/gastown",
        "test": "TestTmuxKeybindingsAlternateScreenPassthrough",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-be91eec86b4dbf7e66f1.log",
        "reason": "gastown_test.go:492: embedded gascity-packs does not yet include #{alternate_on} in WheelUpPane (gastownhall/gascity-packs#136); test activates once go.mod is bumped to that release",
        "class": "other"
      },
      {
        "package": "examples/gastown",
        "test": "TestTmuxKeybindingsAlternateScreenPassthrough",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ef24b9dbe59f5d04fac1.log",
        "reason": "gastown_test.go:492: embedded gascity-packs does not yet include #{alternate_on} in WheelUpPane (gastownhall/gascity-packs#136); test activates once go.mod is bumped to that release",
        "class": "other"
      },
      {
        "package": "internal/api",
        "test": "TestStatusListStoreWithTimeoutKillsBdChildOnTimeout",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-c1bdba0ccc8453285d8e.log",
        "reason": "handler_status_scoped_store_test.go:507: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "internal/api",
        "test": "TestStatusReadyStoreWithTimeoutKillsBdChildOnTimeout",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-00130d8c2aec306b5e94.log",
        "reason": "handler_status_scoped_store_test.go:544: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "internal/api",
        "test": "TestStatusSessionSnapshotKillsBdChildOnTimeout",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5d010ccbe1c203fcb722.log",
        "reason": "handler_status_scoped_store_test.go:141: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestBdStoreCloseNotificationConformance/CloseIfMatchAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a4a1a23acc13a6dd98f.log",
        "reason": "close_notification_conformance.go:54: CloseIfMatch: conditional writes unsupported",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestBdStoreCloseNotificationConformance/CloseIfMatchAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-9a6a09bab414814cab53.log",
        "reason": "close_notification_conformance.go:54: CloseIfMatch: conditional writes unsupported",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestBdStoreCloseNotificationConformance/UpdateIfMatchStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a4a1a23acc13a6dd98f.log",
        "reason": "close_notification_conformance.go:57: UpdateIfMatch(status=closed): conditional writes unsupported",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestBdStoreCloseNotificationConformance/UpdateIfMatchStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-9a6a09bab414814cab53.log",
        "reason": "close_notification_conformance.go:57: UpdateIfMatch(status=closed): conditional writes unsupported",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestBdStoreConditionalWriterConformance",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-20cfdbabba8f52b16e0b.log",
        "reason": "bdstore_conditional_integration_test.go:69: installed bd lacks --if-revision (pre-beads#4682); the conformance row skips cleanly and becomes the authoritative guard for the provisional body codes once #4682 lands",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestExecTimeoutHardExitHarness",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a4a1a23acc13a6dd98f.log",
        "reason": "exec_timeout_linux_test.go:66: harness process",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestExecTimeoutHardExitHarness",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-9a6a09bab414814cab53.log",
        "reason": "exec_timeout_linux_test.go:66: harness process",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestFileStore/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a4a1a23acc13a6dd98f.log",
        "reason": "close_notification_conformance.go:89: backing *beads.FileStore lacks the capability UpdateIfAssignmentStatusClosed needs",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestFileStore/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-9a6a09bab414814cab53.log",
        "reason": "close_notification_conformance.go:89: backing *beads.FileStore lacks the capability UpdateIfAssignmentStatusClosed needs",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestFileStoreReadyParityConformance/FileStore/CachedReadyParity",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-ceb32a3c147a95796597.log",
        "reason": "ready_parity_conformance.go:69: skipping CachedReadyParity (bead ga-gmf8r, expires 2026-12-15): MemStore and FileStore Ready block on a missing or foreign blocker and on a closed blocker with gc.work_outcome=blocked, which a primed CachingStore cannot see without a ready projection, and return insertion order with Limit applied mid-scan instead of the canonical (priority, created_at, id) order. Replacement proof: MemStore implements enrichReadyProjectionForCache and canonical ready order, and TestMemStoreReadyParityConformance and TestFileStoreReadyParityConformance run this suite unwaived",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestFileStoreReadyParityConformance/FileStore/CachedReadyParity",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-b398920224b769833cd5.log",
        "reason": "ready_parity_conformance.go:69: skipping CachedReadyParity (bead ga-gmf8r, expires 2026-12-15): MemStore and FileStore Ready block on a missing or foreign blocker and on a closed blocker with gc.work_outcome=blocked, which a primed CachingStore cannot see without a ready projection, and return insertion order with Limit applied mid-scan instead of the canonical (priority, created_at, id) order. Replacement proof: MemStore implements enrichReadyProjectionForCache and canonical ready order, and TestMemStoreReadyParityConformance and TestFileStoreReadyParityConformance run this suite unwaived",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestKillCommandTreeKillsProcessGroup",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ea4511196531bb4b8c55.log",
        "reason": "bdstore_exec_internal_test.go:266: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestMemStore/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-20cfdbabba8f52b16e0b.log",
        "reason": "close_notification_conformance.go:89: backing *beads.MemStore lacks the capability UpdateIfAssignmentStatusClosed needs",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestMemStore/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ea4511196531bb4b8c55.log",
        "reason": "close_notification_conformance.go:89: backing *beads.MemStore lacks the capability UpdateIfAssignmentStatusClosed needs",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestMemStoreReadyParityConformance/MemStore/CachedReadyParity",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-8d820cd4c52192de043e.log",
        "reason": "ready_parity_conformance.go:69: skipping CachedReadyParity (bead ga-gmf8r, expires 2026-12-15): MemStore and FileStore Ready block on a missing or foreign blocker and on a closed blocker with gc.work_outcome=blocked, which a primed CachingStore cannot see without a ready projection, and return insertion order with Limit applied mid-scan instead of the canonical (priority, created_at, id) order. Replacement proof: MemStore implements enrichReadyProjectionForCache and canonical ready order, and TestMemStoreReadyParityConformance and TestFileStoreReadyParityConformance run this suite unwaived",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestMemStoreReadyParityConformance/MemStore/CachedReadyParity",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-363a36f2662d541bc75e.log",
        "reason": "ready_parity_conformance.go:69: skipping CachedReadyParity (bead ga-gmf8r, expires 2026-12-15): MemStore and FileStore Ready block on a missing or foreign blocker and on a closed blocker with gc.work_outcome=blocked, which a primed CachingStore cannot see without a ready projection, and return insertion order with Limit applied mid-scan instead of the canonical (priority, created_at, id) order. Replacement proof: MemStore implements enrichReadyProjectionForCache and canonical ready order, and TestMemStoreReadyParityConformance and TestFileStoreReadyParityConformance run this suite unwaived",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestMetadataEmptyStringClearContract/Postgres/BatchClearsOneKeyKeepsAnother",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-86f8e1ae5bca4d4a5e22.log",
        "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads",
        "test": "TestMetadataEmptyStringClearContract/Postgres/BatchClearsOneKeyKeepsAnother",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27371dea3b130a4c231f.log",
        "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads",
        "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-86f8e1ae5bca4d4a5e22.log",
        "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads",
        "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27371dea3b130a4c231f.log",
        "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads",
        "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-86f8e1ae5bca4d4a5e22.log",
        "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads",
        "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27371dea3b130a4c231f.log",
        "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads",
        "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-86f8e1ae5bca4d4a5e22.log",
        "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads",
        "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27371dea3b130a4c231f.log",
        "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads",
        "test": "TestNativeDoltStoreConformance/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-a9e2a6fbbc2774327994.log",
        "reason": "close_notification_conformance.go:89: backing *beads.NativeDoltStore lacks the capability UpdateIfAssignmentStatusClosed needs",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestNativeDoltStoreConformance/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-3ee1ae2eaa26dfd63a88.log",
        "reason": "close_notification_conformance.go:89: backing *beads.NativeDoltStore lacks the capability UpdateIfAssignmentStatusClosed needs",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestNativeDoltStoreNormalizesRealUpstreamMissingIssueErrors",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-999a89ecc5b3d09abdd4.log",
        "reason": "native_dolt_store_test.go:651: upstream beads storage unavailable: Dolt server unreachable at 127.0.0.1:0: dial tcp 127.0.0.1:0: connect: connection refused",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestNativeDoltStoreNormalizesRealUpstreamMissingIssueErrors",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-a93729df64a652fced43.log",
        "reason": "native_dolt_store_test.go:651: upstream beads storage unavailable: Dolt server unreachable at 127.0.0.1:0: dial tcp 127.0.0.1:0: connect: connection refused",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestProbeVersionsAgainstHostBinaries/bd",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-363a36f2662d541bc75e.log",
        "reason": "binary_versions_test.go:66: bd not probeable in this environment: locate bd: exec: \"bd\": executable file not found in $PATH",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads",
        "test": "TestSQLiteStoreConformance/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-a1f48694841e1859d61a.log",
        "reason": "close_notification_conformance.go:89: backing *beads.SQLiteStore lacks the capability UpdateIfAssignmentStatusClosed needs",
        "class": "other"
      },
      {
        "package": "internal/beads",
        "test": "TestSQLiteStoreConformance/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-9a6a09bab414814cab53.log",
        "reason": "close_notification_conformance.go:89: backing *beads.SQLiteStore lacks the capability UpdateIfAssignmentStatusClosed needs",
        "class": "other"
      },
      {
        "package": "internal/beads/beadstest",
        "test": "TestRequireLedgeredSkipIsDateFree/LongLapsed",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ec8983708a4fd6968a43.log",
        "reason": "conformance_skips_test.go:60: skipping LongLapsed (bead ga-example, expires 2000-01-01): example",
        "class": "other"
      },
      {
        "package": "internal/beads/contract",
        "test": "TestMigrateJournalFileMatchesPinnedBeads",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8544388e16a0d6b0cdb0.log",
        "reason": "migrate_journal_test.go:37: pinned beads v1.3.2 is not unpacked in the local module cache: open /tmp/bt/_tmp/35731ca362c311940500020a6c8a2007/go/pkg/mod/github.com/steveyegge/beads@v1.3.2/cmd/bd/migrate_dolt_mode.go: no such file or directory",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads/exec",
        "test": "TestBrProviderConformance",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-64046beff95fb28ccd66.log",
        "reason": "br_test.go:18: br not available",
        "class": "environment-absence"
      },
      {
        "package": "internal/beads/splittest",
        "test": "TestStrictStorePinnedIDFenceConformance/TheFenceCoversTheGuardedBatch",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-c424d7b9771e02bed0c3.log",
        "reason": "pinned_id_fence_conformance.go:467: store has no guarded batcher",
        "class": "other"
      },
      {
        "package": "internal/beads/splittest",
        "test": "TestStrictStorePinnedIDFenceConformance/TheGuardedBatchChecksTheNamespaceBeforeExistence",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-c424d7b9771e02bed0c3.log",
        "reason": "pinned_id_fence_conformance.go:533: store has no guarded batcher",
        "class": "other"
      },
      {
        "package": "internal/config",
        "test": "TestLoadWithIncludes_PatchTargetingImplicitAgentErrorsToday",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4bbf1267e0c4ccc54042.log",
        "reason": "implicit_agent_patch_repro_test.go:68: bug ga-91uk5m fixed; delete this characterization test",
        "class": "other"
      },
      {
        "package": "internal/connect/tokenrender",
        "test": "TestWriteTokenFileKeepsGroup",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f79997cecbebd6b14e33.log",
        "reason": "write_test.go:376: the test user has no secondary group and is not root",
        "class": "other"
      },
      {
        "package": "internal/connect/tokenrender",
        "test": "TestWriteTokenFileKeepsOwnerAsRoot",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f79997cecbebd6b14e33.log",
        "reason": "write_test.go:475: needs root to own a file as another user",
        "class": "other"
      },
      {
        "package": "internal/convergence",
        "test": "TestResolveConditionPath/darwin_private_temp_alias_must_not_falsely_reject_a_contained_relative_condition_path",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-0fb1903ec0635aebe9eb.log",
        "reason": "condition_test.go:594: darwin-only: the /private/{tmp,var} alias collapse is a no-op on other platforms",
        "class": "other"
      },
      {
        "package": "internal/convergence",
        "test": "TestResolveEvaluateStep_DarwinPrivateTempAliasWithExistingPrompt",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-0fb1903ec0635aebe9eb.log",
        "reason": "evaluate_test.go:189: darwin-only: the /private/{tmp,var} alias collapse is a no-op on other platforms",
        "class": "other"
      },
      {
        "package": "internal/dispatch",
        "test": "TestCrashInjectionCallBudgetMolecule/fail",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-e7f4116e9a278807bf1e.log",
        "reason": "",
        "class": "other"
      },
      {
        "package": "internal/doctor",
        "test": "TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
        "reason": "checks_custom_types_test.go:467: bd binary not on PATH",
        "class": "environment-absence"
      },
      {
        "package": "internal/doctor",
        "test": "TestCustomTypesCheck_TableDrift",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
        "reason": "checks_custom_types_test.go:297: bd binary not on PATH",
        "class": "environment-absence"
      },
      {
        "package": "internal/doctor",
        "test": "TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
        "reason": "checks_custom_types_test.go:399: bd binary not on PATH",
        "class": "environment-absence"
      },
      {
        "package": "internal/formula",
        "test": "TestCompileBugReportFlowV2",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8754fd06808e5b38215b.log",
        "reason": "compile_test.go:1359: tooling formula not present: stat /home/ubuntu/tooling/formulas/mol-bug-report-flow-v2.toml: permission denied",
        "class": "other"
      },
      {
        "package": "internal/fsys",
        "test": "TestWriteFileIfContentOrModeChangedAtomic_RepairsSpecialModeBits",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8baf01f801bd5b165b89.log",
        "reason": "atomic_test.go:185: filesystem did not preserve setuid bit in test mode: -rwxr-xr-x",
        "class": "other"
      },
      {
        "package": "internal/gitcred",
        "test": "TestLoadAcceptsRootOwnedGroupReadable",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5c43118a23fc611d12c8.log",
        "reason": "rules_test.go:186: needs root to chown the fixture to the Secret-mount shape",
        "class": "other"
      },
      {
        "package": "internal/mail/exec",
        "test": "TestMCPMailBridgeSourceable",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
        "reason": "mcp_conformance_test.go:159: MCP mail script not found: file does not exist",
        "class": "environment-absence"
      },
      {
        "package": "internal/mail/exec",
        "test": "TestMCPMailConformance",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
        "reason": "mcp_conformance_test.go:26: MCP mail script not found: file does not exist",
        "class": "environment-absence"
      },
      {
        "package": "internal/mail/exec",
        "test": "TestMCPMailConformanceLive",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
        "reason": "mcp_live_test.go:35: set GC_TEST_MCP_MAIL=1 to run (or use make test-mcp-mail)",
        "class": "other"
      },
      {
        "package": "internal/mail/exec",
        "test": "TestMCPMailCrossPodNameResolution",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
        "reason": "mcp_conformance_test.go:195: MCP mail script not found: file does not exist",
        "class": "environment-absence"
      },
      {
        "package": "internal/mail/exec",
        "test": "TestMCPMailProjectKeyIsolation",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
        "reason": "mcp_conformance_test.go:286: MCP mail script not found: file does not exist",
        "class": "environment-absence"
      },
      {
        "package": "internal/mail/exec",
        "test": "TestMCPMailReplyMissingOriginalIsNotFound",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
        "reason": "mcp_conformance_test.go:80: MCP mail script not found: file does not exist",
        "class": "environment-absence"
      },
      {
        "package": "internal/molecule",
        "test": "TestBuildRecipeApplyPlanBugReportFlowV2",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-46cac984b12b94b66cce.log",
        "reason": "molecule_test.go:30: tooling formula not present: stat /home/ubuntu/tooling/formulas/mol-bug-report-flow-v2.formula.toml: permission denied",
        "class": "other"
      },
      {
        "package": "internal/orders",
        "test": "TestCheckTriggerConditionKillsProcessGroupAfterWaitDelay",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ff9cab7068d884a7ea3a.log",
        "reason": "triggers_test.go:495: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "internal/orders",
        "test": "TestCheckTriggerConditionKillsProcessGroupOnTimeout",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ff9cab7068d884a7ea3a.log",
        "reason": "triggers_test.go:464: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "internal/pidutil",
        "test": "TestAliveWithStartTime_UnreadableIdentityKeepsAliveAnswer",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-cc30e270515707553560.log",
        "reason": "starttime_portable_test.go:86: on linux /proc answers directly, so a ps stub cannot make the identity unreadable",
        "class": "other"
      },
      {
        "package": "internal/pidutil",
        "test": "TestCmdline_FailsClosedWhenUnreadable",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-cc30e270515707553560.log",
        "reason": "cmdline_portable_test.go:141: PID 1 argv is readable in this environment; cannot exercise the fail-closed path this way",
        "class": "other"
      },
      {
        "package": "internal/processgroup",
        "test": "TestTerminateCommandPreservesGroupFailureAfterDirectKill",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-464b529be21ea4ef14f5.log",
        "reason": "processgroup_unix_test.go:71: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
        "class": "other"
      },
      {
        "package": "internal/runproj",
        "test": "TestRegenerateGoldens",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-9cf54403b0e740f317fc.log",
        "reason": "golden_regen_test.go:15: set RUNPROJ_UPDATE_GOLDENS=1 to rewrite testdata goldens",
        "class": "other"
      },
      {
        "package": "internal/runtime/exec",
        "test": "TestExecConformance/FencedInput_AbsentSessionRefuses",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-02ff57e430fd7218601d.log",
        "reason": "conformance.go:551: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/exec",
        "test": "TestExecConformance/FencedInput_AbsentSessionRefuses",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d1f6abca8430b335d6c5.log",
        "reason": "conformance.go:551: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/exec",
        "test": "TestExecConformance/FencedInput_MatchingFenceDelivers",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-02ff57e430fd7218601d.log",
        "reason": "conformance.go:508: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/exec",
        "test": "TestExecConformance/FencedInput_MatchingFenceDelivers",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d1f6abca8430b335d6c5.log",
        "reason": "conformance.go:508: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/exec",
        "test": "TestExecConformance/FencedInput_MismatchedFenceRefusesEveryInput",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-02ff57e430fd7218601d.log",
        "reason": "conformance.go:525: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/exec",
        "test": "TestExecConformance/FencedInput_MismatchedFenceRefusesEveryInput",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d1f6abca8430b335d6c5.log",
        "reason": "conformance.go:525: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestActivityLive",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
        "reason": "activity_live_test.go:20: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestActivityLive",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
        "reason": "activity_live_test.go:20: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestHerdrConformance",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
        "reason": "conformance_test.go:18: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestHerdrConformance",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
        "reason": "conformance_test.go:18: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestProviderLive",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
        "reason": "provider_live_test.go:15: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestProviderLive",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
        "reason": "provider_live_test.go:15: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestProviderLiveClaudeKindPath",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
        "reason": "kindpath_live_test.go:105: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestProviderLiveClaudeKindPath",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
        "reason": "kindpath_live_test.go:105: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestProviderLiveOccupantSwapKeepsLiveness",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
        "reason": "panebinding_live_test.go:21: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestProviderLiveOccupantSwapKeepsLiveness",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
        "reason": "panebinding_live_test.go:21: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestSessionEventsLive",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
        "reason": "events_live_test.go:40: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/herdr",
        "test": "TestSessionEventsLive",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
        "reason": "events_live_test.go:40: herdr not installed",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunLifecycleTestsDoesNotStopUnownedConcurrentStart/suite/Start_ConcurrentDistinctSessions",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:124: concurrent Start(preexisting-concurrent-session): session already exists",
        "class": "other"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/FencedInput_AbsentSessionRefuses",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:551: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/FencedInput_MatchingFenceDelivers",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:508: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/FencedInput_MismatchedFenceRefusesEveryInput",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:525: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Interrupt_ConcurrentDistinctSessions",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:238: Start(skip-start-14): provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/IsRunning_ConcurrentDistinctSessions",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:271: Start(skip-start-18): provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ConcurrentDistinctPrefixes",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:367: Start(skip-start-25): provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_EmptyPrefix",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:349: Start: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ExcludesStopped",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:333: Start: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_FindsSessions",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:294: Start skip-start-21: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_PrefixFiltering",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:313: Start skip-start-22: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_ConcurrentDistinctSessions",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:421: Start(skip-start-30): provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_EmptyNamesReturnsTrue",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:395: Start: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_FalseAfterStop",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:404: Start: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/SharedSession",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:61: Start shared session: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_ConcurrentDistinctSessions",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:124: concurrent Start(skip-start-3): provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_CreatesRunningSession",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:84: Start: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_DuplicateReturnsError",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:93: first Start: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_ConcurrentDistinctSessions",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:209: Start(skip-start-11): provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_AlreadyStopped",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:193: Start: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_NotRunning",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:170: Start sentinel: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_MakesSessionNotRunning",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance.go:134: Start: provider environment unavailable",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/runtimetest",
        "test": "TestStartOrSkipDoesNotStopSessionAfterClassifiedStartFailure/classified_failure",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
        "reason": "conformance_test.go:147: Start: session already exists",
        "class": "other"
      },
      {
        "package": "internal/runtime/ssh",
        "test": "TestConn_ExecOverRealLocalhost",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-60ebdfafc6cd70feb3b1.log",
        "reason": "ssh_localhost_integration_test.go:24: passwordless ssh to localhost unavailable",
        "class": "other"
      },
      {
        "package": "internal/runtime/ssh",
        "test": "TestSSHConformance/FencedInput_AbsentSessionRefuses",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-60ebdfafc6cd70feb3b1.log",
        "reason": "conformance.go:551: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/ssh",
        "test": "TestSSHConformance/FencedInput_MatchingFenceDelivers",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-60ebdfafc6cd70feb3b1.log",
        "reason": "conformance.go:508: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/ssh",
        "test": "TestSSHConformance/FencedInput_MismatchedFenceRefusesEveryInput",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-60ebdfafc6cd70feb3b1.log",
        "reason": "conformance.go:525: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "internal/runtime/t3bridge",
        "test": "TestLookupDeliveryErrors/seam-backed/start_grace",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-a23a357eedd1739a53e0.log",
        "reason": "delivery_lookup_test.go:133: start grace is raw-bridge bookkeeping",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestCleanupOrphanedSessions",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
        "reason": "tmux_test.go:1615: Skipping: GT_TEST_ALLOW_CLEANUP_TEST=1 required (this test kills sessions)",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestCleanupOrphanedSessions_NoSessions",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
        "reason": "tmux_test.go:1713: Skipping: GT_TEST_ALLOW_CLEANUP_TEST=1 required (this test kills sessions)",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestGetKeyBinding_CapturesUserBinding",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
        "reason": "tmux_test.go:3663: not inside tmux \u2014 need server for bind-key",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestGetKeyBinding_SkipsGasTownBindings",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
        "reason": "tmux_test.go:3637: not inside tmux \u2014 need server for bind-key",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestIsGTBinding_DetectsGasTownBindings",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-15c091b746137e5c2921.log",
        "reason": "tmux_test.go:3689: not inside tmux \u2014 need server for bind-key",
        "class": "environment-absence"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestNudgeConfirmBudgetThreshold",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
        "reason": "nudge_confirm_lag_integration_test.go:174: set GC_TMUX_INTEGRATION=1 to run this real-tmux measurement",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestNudgeConfirmBusyRenderLag",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-15c091b746137e5c2921.log",
        "reason": "nudge_confirm_lag_integration_test.go:36: set GC_TMUX_INTEGRATION=1 to run this real-tmux reproduction (spins a throwaway tmux server)",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestNudgePokeRealTmux",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
        "reason": "nudge_poke_integration_test.go:27: set GC_TMUX_INTEGRATION=1 to run this real-tmux dogfood (spins a throwaway tmux server)",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestNudgeRecoversStagedDraftSwallowedByPasteIngest",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
        "reason": "nudge_staged_draft_integration_test.go:26: set GC_TMUX_INTEGRATION=1 to run this real-tmux reproduction (spins a throwaway tmux server)",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestPokeActivityRealTmux",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
        "reason": "activity_poke_integration_test.go:21: set GC_TMUX_INTEGRATION=1 to run this real-tmux dogfood (spins a throwaway tmux server)",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestPokeActivityRealTmux",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ef2749222a71266756ae.log",
        "reason": "activity_poke_integration_test.go:21: set GC_TMUX_INTEGRATION=1 to run this real-tmux dogfood (spins a throwaway tmux server)",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestProcessSnapshotPSArgsRejectsLinuxSyntaxOnDarwin",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
        "reason": "state_cache_test.go:926: Darwin-specific syntax guard",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestProcessSnapshotPSArgsRejectsLinuxSyntaxOnDarwin",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ef2749222a71266756ae.log",
        "reason": "state_cache_test.go:926: Darwin-specific syntax guard",
        "class": "other"
      },
      {
        "package": "internal/runtime/tmux",
        "test": "TestSetBindings_PreserveFallbackOnRepeatedCalls",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
        "reason": "tmux_test.go:3719: not inside tmux \u2014 need server for bind-key",
        "class": "environment-absence"
      },
      {
        "package": "internal/sessionlog",
        "test": "TestFindClaudeLatestSessionFileUsesProjectPathAlias",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
        "reason": "sessionlog_test.go:1397: macOS-only /tmp <-> /private/tmp Claude project path alias",
        "class": "other"
      },
      {
        "package": "internal/sessionlog",
        "test": "TestFindCodexSessionFileMatchesEquivalentResolvedWorkDir",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
        "reason": "sessionlog_test.go:2170: macOS /private/tmp path aliases only apply on darwin",
        "class": "other"
      },
      {
        "package": "internal/sessionlog",
        "test": "TestFindGeminiSessionFileMatchesEquivalentResolvedWorkDir",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
        "reason": "sessionlog_test.go:2315: macOS-only /tmp <-> /private/tmp Gemini project path alias",
        "class": "other"
      },
      {
        "package": "internal/sessionlog",
        "test": "TestFindSessionFileByIDPrefersStoredWorkDirSpelling",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
        "reason": "sessionlog_test.go:1104: macOS-only /tmp <-> /private/tmp Claude project path alias",
        "class": "other"
      },
      {
        "package": "internal/sessionlog",
        "test": "TestFindSessionFileByIDUsesClaudeProjectPathAlias",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
        "reason": "sessionlog_test.go:1084: macOS-only /tmp <-> /private/tmp Claude project path alias",
        "class": "other"
      },
      {
        "package": "internal/sessionlog",
        "test": "TestFindSessionFileUsesClaudeProjectPathAlias",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
        "reason": "sessionlog_test.go:1195: macOS-only /tmp <-> /private/tmp Claude project path alias",
        "class": "other"
      },
      {
        "package": "internal/sessionlog",
        "test": "TestFindSessionFileUsesNewestClaudeProjectPathAliasMatch",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
        "reason": "sessionlog_test.go:1215: macOS-only /tmp <-> /private/tmp Claude project path alias",
        "class": "other"
      },
      {
        "package": "internal/worker/transcript",
        "test": "TestDiscoverFallbackPathUsesNewestClaudeLatestSessionAcrossAliases",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5536c7e35ba99d4d52dd.log",
        "reason": "discovery_test.go:65: macOS-only /tmp <-> /private/tmp Claude project path alias",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/antigravity/tmux-cli/WC-TX-USAGE-001",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/antigravity/tmux-cli/WC-USAGE-COST-001",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/cursor/tmux-cli/WC-TX-USAGE-001",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/cursor/tmux-cli/WC-USAGE-COST-001",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/gemini/tmux-cli/WC-TX-USAGE-001",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/gemini/tmux-cli/WC-USAGE-COST-001",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/kimi/tmux-cli/WC-TX-USAGE-001",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/kimi/tmux-cli/WC-USAGE-COST-001",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/mimocode/tmux-cli/WC-TX-USAGE-001",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/mimocode/tmux-cli/WC-USAGE-COST-001",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/pi/tmux-cli/WC-TX-USAGE-001",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/pi/tmux-cli/WC-USAGE-COST-001",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/zcode/tmux-cli/WC-TX-USAGE-001",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestPhase1Conformance/zcode/tmux-cli/WC-USAGE-COST-001",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
        "class": "other"
      },
      {
        "package": "internal/worker/workertest",
        "test": "TestStructuredConformance/gemini/tmux-cli/WC-STRUCT-003",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
        "reason": "structured_conformance_test.go:153: profile fixture has no edit result to evaluate",
        "class": "other"
      },
      {
        "package": "internal/workspacesvc",
        "test": "TestProxyProcessHardExitHarness",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a7df874ab90d74e027a.log",
        "reason": "proxy_process_test.go:1160: harness process",
        "class": "other"
      },
      {
        "package": "internal/workspacesvc",
        "test": "TestProxyProcessHelper",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a7df874ab90d74e027a.log",
        "reason": "proxy_process_test.go:167: helper process",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestBeadsInitTopologyMatrix/M5-legacy-gc-managed",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-f785906030efd99733e1.log",
        "reason": "beads_topology.go:637: there is no pre-journal gc binary; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc built from a commit before the ownership journal",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestBeadsInitTopologyMatrix/M5-legacy-gc-managed",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-2129301bc9bf3acdcdf7.log",
        "reason": "beads_topology.go:637: there is no pre-journal gc binary; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc built from a commit before the ownership journal",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestBeadsInitTopologyMatrix/M5-legacy-gc-managed",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-00f787a1201c0c97f5dd.log",
        "reason": "beads_topology.go:637: there is no pre-journal gc binary; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc built from a commit before the ownership journal",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestBeadsMigrateLegacyCityToProxied",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-2b4464fe973ad362994d.log",
        "reason": "beads_migrate_proxied_test.go:135: there is no legacy gc available; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc that still initialises the GC-managed direct topology",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestBeadsMigrateProxiedRefusesLiveLegacyServer",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-a4ee7fceff2c46ffacdd.log",
        "reason": "beads_migrate_proxied_test.go:357: there is no legacy gc available; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc that still initialises the GC-managed direct topology",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestManagedDirectSessionBdFollowsMovedDoltPort",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-09e792a237f1c684f6da.log",
        "reason": "managed_session_dolt_port_move_test.go:45: there is no pre-journal gc binary; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc built from a commit before the ownership journal",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestPackRegistryLiveImportsEveryCatalogPack",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-09e792a237f1c684f6da.log",
        "reason": "pack_registry_live_test.go:38: set GC_TEST_GASCITY_PACKS_REGISTRY to a gascity-packs registry.toml source to run this live import smoke test",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestSelfhostUX_BinaryDriftPostMergePlaceholder",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-56dbf54bd097e3e21771.log",
        "reason": "integration_selfhost_ux_test.go:112: placeholder for ga-xxqx",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestSelfhostUX_BinaryDriftRestart",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-fedaecec6af3ef2e4157.log",
        "reason": "integration_selfhost_ux_test.go:54: pending merge of builder/ga-xxqx-1 (PR#2219): drift auto-restart not on origin/main",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestSelfhostUX_Composed",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-5ed75e6deb5279ca2b84.log",
        "reason": "integration_selfhost_ux_test.go:96: placeholder for ga-q0bf.1, ga-r8iz.1, ga-6wrr.1, and post-PR#2219 assertions",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestSelfhostUX_MigrationGuideURLPlaceholder",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-61bd7bf3b62120f8de67.log",
        "reason": "integration_selfhost_ux_test.go:108: placeholder for ga-6wrr.1",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestSelfhostUX_PackV1V2Collision",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-fd5aed82a8b7ea468264.log",
        "reason": "integration_selfhost_ux_test.go:16: pending ga-cwdaqv: supervised gc start does not validate config pre-registration",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestSelfhostUX_StopBypassDetailsPlaceholder",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-b54423ef39aa7297e2c2.log",
        "reason": "integration_selfhost_ux_test.go:104: placeholder for ga-r8iz.1",
        "class": "other"
      },
      {
        "package": "test/acceptance",
        "test": "TestSelfhostUX_StructuredStartSummaryPlaceholder",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-53f66fa06e91bd4169e5.log",
        "reason": "integration_selfhost_ux_test.go:100: placeholder for ga-q0bf.1",
        "class": "other"
      },
      {
        "package": "test/acceptance/helpers",
        "test": "TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
        "reason": "bd_schema_parity_test.go:121: bd not available",
        "class": "environment-absence"
      },
      {
        "package": "test/acceptance/helpers",
        "test": "TestBdLatestSchemaVersionLeavesNothingInItsTempRoot",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
        "reason": "bd_schema_parity_test.go:144: bd not available",
        "class": "environment-absence"
      },
      {
        "package": "test/acceptance/helpers",
        "test": "TestRealBdProvisionLeavesTheHostHomeAlone",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
        "reason": "tool_home_test.go:317: no bd available (set GC_ACCEPTANCE_BD_BIN)",
        "class": "environment-absence"
      },
      {
        "package": "test/containerhost",
        "test": "TestK8sSessionConformance/FencedInput_AbsentSessionRefuses",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-afcc08552b3653c0e0f1.log",
        "reason": "conformance.go:551: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "test/containerhost",
        "test": "TestK8sSessionConformance/FencedInput_MatchingFenceDelivers",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-afcc08552b3653c0e0f1.log",
        "reason": "conformance.go:508: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "test/containerhost",
        "test": "TestK8sSessionConformance/FencedInput_MismatchedFenceRefusesEveryInput",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-afcc08552b3653c0e0f1.log",
        "reason": "conformance.go:525: provider cannot fence session input",
        "class": "other"
      },
      {
        "package": "test/conversationlane",
        "test": "TestX6FanIn/full",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-8ecc4094e08f9bfd6d51.log",
        "reason": "x6_test.go:286: DES-O1's literal X6 line (5,000 notices in 60 s) is a manual measurement: --test_env=GC_LANE_X6_FULL=1",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestBdStoreConformance",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-d0691e3dec42dbb78fd3.log",
        "reason": "bdstore_test.go:54: BdStore fails ParentIDNamesARowThisStoreDoesNotHave: bd refuses an unresolvable --parent (ga-6od57); un-skip is ga-x4j5ka",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestDoltPersistence_AssigneeAndInProgressSurviveSubsequentBdWrite",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-5bb45aeaf433c2072f8e.log",
        "reason": "dolt_persistence_test.go:536: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestDoltPersistence_CloseStatusSurvivesSubsequentBdWrite",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-23490fe556f84649266a.log",
        "reason": "dolt_persistence_test.go:449: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestDoltPersistence_ConcurrentMetadataWriteBurstTimingVisibility",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-d0691e3dec42dbb78fd3.log",
        "reason": "dolt_persistence_test.go:818: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestDoltPersistence_CrossBeadMetadataWritesPersistInDolt",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-35aa679f85944e59e6b9.log",
        "reason": "dolt_persistence_test.go:766: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestDoltPersistence_HandoffRoutingMetadataSurvivesSubsequentBdWrite",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-8e315f95edb09e5631cb.log",
        "reason": "dolt_persistence_test.go:636: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestDoltPersistence_InProgressStatusSurvivesSubsequentBdWrite",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-d41f611925a4b41fb744.log",
        "reason": "dolt_persistence_test.go:679: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestDoltPersistence_MetadataSurvivesSubsequentBdWrites",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-82ff0c4322b44203e167.log",
        "reason": "dolt_persistence_test.go:490: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestDoltPersistence_SequentialMetadataWritesAccumulateInDolt",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-b2288f9e45f57dd8ece2.log",
        "reason": "dolt_persistence_test.go:722: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestDoltPersistence_SessionAwakeMetadataSurvivesSubsequentBdWrite",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-65412ec6e874f54aa912.log",
        "reason": "dolt_persistence_test.go:585: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestHumaBinary_CityUnregisterAsync",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-d0691e3dec42dbb78fd3.log",
        "reason": "huma_binary_test.go:508: flaky on CI under load \u2014 5s supervisor shutdown budget races with in-flight mayor-session-start; tracked in #2090",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestReadProcessSnapshotFallsBackToPSAndFindsRealProcesses",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-d41f611925a4b41fb744.log",
        "reason": "integration_test.go:2841: host has /proc; this test targets the no-/proc (macOS) fallback path",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestStartDrift_PermissionDenied_DescriptiveError",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-8e315f95edb09e5631cb.log",
        "reason": "start_drift_test.go:275: permission-denied scenario requires root to drop to a secondary uid",
        "class": "other"
      },
      {
        "package": "test/integration",
        "test": "TestStartDrift_SystemdManaged_RestartsToNewBuildID",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-23490fe556f84649266a.log",
        "reason": "start_drift_test.go:123: XDG_RUNTIME_DIR not set; user systemd not addressable",
        "class": "environment-absence"
      },
      {
        "package": "test/tmuxtest",
        "test": "TestHelperHoldSentinel",
        "status": "SKIP",
        "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-7989cb1ae7bd292f8cbf.log",
        "reason": "orphan_sweep_sigkill_test.go:29: not invoked as a subprocess helper",
        "class": "other"
      }
    ]
  },
  "test_cmd": [
    "test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"
  ],
  "test_counts": {
    "PASS": 134243,
    "FAIL": 0,
    "SKIP": 377
  },
  "diff_tests_executed": "diff_tests_executed:\n  TestDefersUnfinishedOrphanScanWhenStartDeadlineEndsIt PASS (integration-packages-e4245ddf6a37030111bc.log, unit-2bde55fab377f50b4739.log)\n  TestExecutePreparedStartWaveDefersUnfinishedOrphanScan PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-23e7d9fc763496dd8095.log)\n  TestBoundedScanPanicIsLoggedWithKeyValueAndStack PASS (unit-12382fa48a81def0b895.log)\n  TestBoundedScanPanicIsUnavailableAndReleasesKey PASS (unit-12382fa48a81def0b895.log)\n  TestFindRuntimesBySessionIDBoundedCallerCancelIsUnavailable PASS (unit-12382fa48a81def0b895.log)\n  TestFindRuntimesBySessionIDBoundedExpiresAtBudget PASS (unit-12382fa48a81def0b895.log)\n  TestFindRuntimesBySessionIDBoundedKeyFreeAfterScanReturns PASS (unit-12382fa48a81def0b895.log)\n  TestFindRuntimesBySessionIDBoundedLatchIsPerKey PASS (unit-12382fa48a81def0b895.log)\n  TestFindRuntimesBySessionIDBoundedLeavesNoGoroutineBehind PASS (unit-12382fa48a81def0b895.log)\n  TestFindRuntimesBySessionIDBoundedReturnsAnswerWithinBudget PASS (unit-12382fa48a81def0b895.log)\n  TestFindRuntimesBySessionIDBoundedScanFinishingByBudgetWins PASS (unit-12382fa48a81def0b895.log)\n  TestFindRuntimesBySessionIDBoundedSecondCallWhileOutstandingStartsNoScan PASS (unit-12382fa48a81def0b895.log)\n  TestProcessTableScanUnavailableWrapsRuntimeUnavailable PASS (unit-12382fa48a81def0b895.log)\n  TestFindRuntimesBySessionIDWithNoRootsAsksTmuxNothing PASS (integration-packages-15c091b746137e5c2921.log, unit-4bcc997199c6a7426526.log)\n  TestFindRuntimesBySessionIDWithNoRootsStillReturnsScanError PASS (integration-packages-5a078b24388ef3c79748.log, unit-ce5f199695f6832daef7.log)\n  TestCreateDefersAndRollsBackWhenOrphanScanNeverFinishes PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)\n  TestKillExistingOrphansBudgetIsCappedByContextDeadline PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)\n  TestKillExistingOrphansDefersWhenScanNeverFinishes PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)\n  TestKillExistingOrphansKeysLatchByCityAndSession PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)\n  TestKillExistingOrphansRepeatedStartsOfWedgedSessionScanOnce PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)\n  TestKillExistingOrphansStillTerminatesWhenScanReturns PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)\n  TestKillExistingOrphansStopsBetweenTerminationsWhenContextDone PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)\n  TestManagerOrphanScanBudgetDefaultsToTenSeconds PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)\n  TestOrphanScanBudgetFieldIsHonored PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)\n  TestSameCityDifferentSessionHasItsOwnLatch PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)\n  TestStaleKeyRetryDefersWhenOrphanScanNeverFinishes PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)\n  TestStartDefersWhenOrphanScanNeverFinishes PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)\n  TestStartRuntimeOnlyDefersWhenOrphanScanNeverFinishes PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)\nGATE_TEST_EVIDENCE_RESULT ok: 28 diff-owned tests verified by name\n"
}
```

## Full-suite independent audit

```json
{
  "complete": true,
  "bead": "ga-wb0ttep",
  "pr_head": "c59c2143f61163a0a51e36a06f7e1ccfee4d6f1b",
  "tested_merge": "0d781d66b8fab4cf6c06434827fe1056adad7005",
  "tested_tree": "77737e729c79cf7ae1b0fdf55b2de1b933320b1c",
  "owned_roots": 28,
  "required_roots": 45,
  "additional_unchanged_witnesses": 17,
  "fresh_remote_results": 502,
  "native_steps": 16,
  "test_counts": {
    "PASS": 134243,
    "FAIL": 0,
    "SKIP": 377
  },
  "no_test_failure_attributed_or_waived": true
}
```

## Exact skip audit

```json
{
  "farm": {
    "rows": 363,
    "unique_tests": 305,
    "site_candidates": 285,
    "all_sites_unchanged": true,
    "shared_hunks_at_skip_sites": 0,
    "classes": {
      "other": 292,
      "environment-absence": 71
    },
    "proof": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/farm-skip-sites-proof.txt"
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
    "proof": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/native-skip-sites-proof.txt"
  },
  "diff_shared_hunk": "Shared test hunks add scanner/provider fakes and synctest helpers used only by the owned orphan-scan roots; none adds a skip condition or changes TestMain, init or build tags. The new helper and Manager test files contain no skip site. The classifier fakes are local to its changed boundary-test file. Every actual skip site must separately resolve UNCHANGED with zero shared hunks; no package-wide skip exemption is claimed."
}
```

## Required supplementary proof with original command outcomes/attribution

```json
{
  "complete": true,
  "tree": "77737e729c79cf7ae1b0fdf55b2de1b933320b1c",
  "actual_race_target_results": 18,
  "actual_manifest_target_results": 1,
  "all28_owned_roots_PASS_in_each_of_three_race_runs": true,
  "no_DATA_RACE_warning": true,
  "family_proof": {
    "all_15_actual_race_results_PASS": true,
    "fresh_remote_attempt1": true,
    "raw_artifacts": 30,
    "all_original_BEP_artifact_hashes_verified": true,
    "no_DATA_RACE_warning": true,
    "owned_roots_checked_in_each_run": [
      {
        "run": 1,
        "owned_non_cli_roots_actual_pass": 26
      },
      {
        "run": 2,
        "owned_non_cli_roots_actual_pass": 26
      },
      {
        "run": 3,
        "owned_non_cli_roots_actual_pass": 26
      }
    ],
    "original_command_exit": 1,
    "original_build_failed_on_unchanged_nogo_validation": true,
    "only_BEP_predicate_error": [
      "build not successfully finished"
    ],
    "base_ref_same_condition_proof": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/race-nogo-base-reproduction.json",
    "not_a_clean_build_or_overall_gate_PASS": true
  },
  "classifier_proof": {
    "complete_actual_race_checks": true,
    "actual_target_results": 3,
    "owned_roots": 2,
    "each_owned_root_PASS_in_each_of_three_runs": true,
    "fresh_remote_attempt1": true,
    "raw_artifacts": 6,
    "no_DATA_RACE_warning": true,
    "original_command_exit": 1,
    "original_build_failed_on_unchanged_nogo_validation": true,
    "attribution_tracker": "ga-38b2nor",
    "unchanged_failed_packages": [
      ".",
      "examples/bd",
      "examples/bd/dolt",
      "internal/api/apicontract",
      "internal/api/apierr",
      "internal/api/dashboardspa",
      "internal/api/genclient",
      "internal/bazeltest",
      "internal/beadmeta",
      "internal/beads/ledgerfeed",
      "internal/beads/proxyendpoint/proxyendpointtest",
      "internal/bootstrap/packs/core",
      "internal/chartest",
      "internal/citywriteauth",
      "internal/clientauth",
      "internal/clientcontext",
      "internal/clientgrant",
      "internal/clock",
      "internal/connect",
      "internal/deps",
      "internal/doltpool",
      "internal/doltversion",
      "internal/events",
      "internal/execenv",
      "internal/execgrace",
      "internal/fanout",
      "internal/fanout/fanouttest",
      "internal/fsyncprobe",
      "internal/gchome",
      "internal/hostboot",
      "internal/logutil",
      "internal/loopbackport",
      "internal/mail",
      "internal/modelwindow",
      "internal/overlay",
      "internal/pathdurability",
      "internal/pathutil",
      "internal/pidutil",
      "internal/poolplan",
      "internal/pricing",
      "internal/processgroup",
      "internal/processgroup/processgrouptest",
      "internal/promptmeta",
      "internal/promptsafe",
      "internal/reconcilekey",
      "internal/remotesource",
      "internal/resilience",
      "internal/rollout/gate",
      "internal/runtime/herdr/herdrtest",
      "internal/sdnotify",
      "internal/searchpath",
      "internal/shellquote",
      "internal/ssrf",
      "internal/telemetry",
      "internal/testenv",
      "internal/testpolicy/waiverclock",
      "internal/usage",
      "internal/worker/adapters/zcode",
      "internal/worker/builtin",
      "internal/workladder",
      "pkg/eventexport",
      "test/toolhome"
    ],
    "base_ref_same_condition_proof": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/race-nogo-base-reproduction.json",
    "original_verdicts_not_retried": true,
    "reader_correction": "Initial extra audit expected6 BEP results by counting2 test roots x3 runs; BEP records one target result per run, so actual3 results contain both roots each time. This is an audit count correction, no test rerun.",
    "not_a_clean_build_or_overall_gate_PASS": true
  },
  "manifest_roots_actual_PASS": [
    "TestRuntimeTmuxGuardAddingATestCostsOnlyManifestLines",
    "TestRuntimeTmuxIntegrationShardClearsAmbientManifestOnDynamicFallback",
    "TestRuntimeTmuxIntegrationShardPreservesDynamicDiscoveryOnDarwin",
    "TestRuntimeTmuxIntegrationShardPreservesDynamicDiscoveryOnLinuxArm64",
    "TestRuntimeTmuxIntegrationShardSelectsUsingSanitizedTarget",
    "TestRuntimeTmuxIntegrationShardUsesCheckedManifestOnLinux",
    "TestRuntimeTmuxLaneDriftDiagnostics",
    "TestRuntimeTmuxLaneRecordMatchesTree",
    "TestRuntimeTmuxLaneRecordParser",
    "TestRuntimeTmuxManifestDiscoveryDistinguishesTestMainHarness",
    "TestRuntimeTmuxManifestDiscoveryUsesCanonicalLinuxPlatform",
    "TestRuntimeTmuxManifestDriftDiagnostics",
    "TestRuntimeTmuxManifestMatchesCanonicalLinuxIntegrationInventory",
    "TestRuntimeTmuxPerFileDiscoveryOrdersFilesThenTests"
  ],
  "manifest_evidence": {
    "test_exec_mode": "remote-exec",
    "test_exec_bep": {
      "results": 1,
      "remote_executed": 1,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "farm_test_seconds": 5.446,
    "verdicts": 1,
    "transport_failure": false,
    "errors": []
  },
  "raw_supplementary_artifacts": 38,
  "validation_attribution": {
    "verified": true,
    "tracker": "ga-38b2nor",
    "clause1": "31 unchanged support-package actions",
    "clause2": "new tracker with positive same-run base proof (gm-sf3238)",
    "clause3": "d BASE-REF identical loader failure",
    "clause4": "no package overlap",
    "same_package_guard": "not applicable",
    "no_test_failure_or_retry": true,
    "note": "failure_attribution: required race-family nogo validation -> ga-38b2nor; clause1: failing actions are in31 unchanged support packages, none diff-owned; clause2: new root-condition tracker under gm-sf3238, after search/read of open and73 closed trackers, justified by same-run positive base proof; clause3: BASE-REF (d), race-enabled shellquote build at adf99aa6c84d83bfc1492de5bd969231a7edf7f0 without the feature reproduces the identical gocritic embedded-rule loader error cannot load fmt; clause4: no package overlap with feature, so same-package load guard is inapplicable. Original race command rc1 is retained and is not called a clean build. All15 actual race test results are uncached first-attempt remote PASSED; all26 non-CLI owned roots PASS separately in EACH of3 runs; no DATA RACE warning. All30 raw logs/XML recovered through read-only CAS and matched to original BEP SHA256 and length. No test was retried. Canonical full C3/nogo remains a clean PASS. Proceed only with previously unexecuted classifier/manifest supplementary phases, preserving the original command failure and attribution; overall gate still waits for those and CI accounting. Proofs /var/tmp/deploy-ga-wb0ttep.xa23xylo/race-nogo-base-reproduction.json and /var/tmp/deploy-ga-wb0ttep.xa23xylo/orphan-acceptance-evidence/race-family-independent-test-proof.json."
  },
  "no_test_failure_or_test_retry": true,
  "original_family_and_classifier_commands_exit1_on_attributed_nogo_validation": "Preserved in original receipts; not called clean builds. Base-ref reproduces same condition; no package overlap.",
  "ad_hoc_audit_count_correction": "Two classifier roots across3runs produce3 target-level BEP results, not6. Existing original results audited; none rerun."
}
```

## Policy/generated targets independently executed in C3

```json
{
  "policy": {
    "labels": [
      "//:ci_runner_policy_test",
      "//:ci_suite_coverage_test",
      "//:go_format_test",
      "//:goreleaser_check_test",
      "//scripts/cipolicy:cipolicy_test",
      "//scripts/prwatchdog:prwatchdog_test",
      "//scripts:check_core_boundary_test",
      "//scripts:check_eventexport_isolation_test",
      "//scripts:check_gomod_replace_test",
      "//scripts:check_native_dependency_surface_test",
      "//test/docsync:docsync_test"
    ],
    "early_fast_results": 11,
    "early_fast_cached": 5,
    "all_also_executed_uncached_remote_attempt1_in_canonical_C3_unit": true
  },
  "drift": {
    "labels": [
      "//cmd/genschema:genschema_in_sync_test",
      "//cmd/genspec:genspec_in_sync_test",
      "//contrib/openclaw-bridge:lockfiles_in_sync_test",
      "//internal/api/dashboardspa/web/shared:generated_client_in_sync_test",
      "//internal/api/dashboardspa/web:lockfiles_in_sync_test",
      "//internal/api/dashboardspa:dist_in_sync_test",
      "//internal/api/genclient:genclient_test"
    ],
    "early_fast_results": 7,
    "early_fast_cached": 4,
    "all_also_executed_uncached_remote_attempt1_in_canonical_C3_unit": true
  },
  "openapi": {
    "labels": [
      "//cmd/openapi-breaking:openapi-breaking_test"
    ],
    "early_fast_results": 1,
    "early_fast_cached": 0,
    "all_also_executed_uncached_remote_attempt1_in_canonical_C3_unit": true
  }
}
```

## Load evidence

Remote admission is25GiB/cap2; it does not wait on host load. The mandatory native floor and local CI job each keep the standard threshold15/1800s soft wait. A bounded wait timeout proceeds under that documented rule and is not a waived failure.

Native floor
```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=69.27 run_max_load=73.88 run_mean_load=67.83 run_readings=10 wait_first_load=157.24 wait_max_load=157.24 wait_mean_load=106.25 wait_readings=61 read_errors=0
```

C3 outer sampler
```text
GATE_ADMISSION {"class": "remote", "floor_gib": 25.0, "free_gib": 50.76187515258789, "waited_seconds": 0.001, "cap": 2}
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=79.53 run_max_load=160.00 run_mean_load=104.50 run_readings=119 wait_first_load=79.53 wait_max_load=79.53 wait_mean_load=79.53 wait_readings=1 read_errors=0
```

CI runner-policy load/admission
```text
GATE_ADMISSION prewait class=local-heavy result=timed_out waited_s=1800 readings=61
GATE_ADMISSION {"class": "local-heavy", "floor_gib": 40.0, "free_gib": 60.58344650268555, "waited_seconds": 0.001, "cap": 3, "prewait_s": 1800.727}
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=59.07 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=74.57 wait_max_load=99.55 wait_mean_load=81.73 wait_readings=62 read_errors=0 prewait_s=1800
```

## Required CI accounting

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.wb0ttep.5o1_2t2s adf99aa6c84d83bfc1492de5bd969231a7edf7f0 c59c2143f61163a0a51e36a06f7e1ccfee4d6f1b fork; exit0; workflow blob e5b11c348505dab28dc223e874bc2c61cbbb33c5; 4 LEAF jobs need a status. Runner expressions read from merged workflow and runner-policy outputs.

runner-policy | LOCAL-PASS | Exact workflow command/environment in runner-policy.sh; durable runner-policy-run complete exit0 after standard local admission/load wait and isolation wrapper.
changes | LOCAL-PASS | Merged workflow closure/path-matcher tool above exit0; closure.txt.
credential-provider-windows | NOT-TRIGGERED | credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
pack-gate | NOT-TRIGGERED | pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'

check is AGGREGATOR and gets no separate status.
windows_reach: none (exit0 and empty output)
deferred_to_pr_ci: none
not_covered: none

```text
diff adf99aa6c84d83bfc1492de5bd969231a7edf7f0...c59c2143f61163a0a51e36a06f7e1ccfee4d6f1b: 11 changed files
  filter packs                no match
  filter credential_provider  no match
  filter shared               no match
suite_mode: filtered  (shared filter did not match)

ci-required needs 5 jobs; transitive closure 5 jobs
job key                                  kind       runs-on                    trigger
runner-policy                            LEAF       linux                      EVERY-PR
changes                                  LEAF       linux                      EVERY-PR
check                                    AGGREGATOR linux                      EVERY-PR
  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
  pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'

4 LEAF jobs need a status; the rest are rolled up through their needs.
```

## Owned results

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs logs=518 logs_with_results=452
test_counts: PASS=134243 FAIL=0 SKIP=377
  (top-level result lines, column 0, summed over the 518 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
diff_tests_executed:
  TestDefersUnfinishedOrphanScanWhenStartDeadlineEndsIt PASS (integration-packages-e4245ddf6a37030111bc.log, unit-2bde55fab377f50b4739.log)
  TestExecutePreparedStartWaveDefersUnfinishedOrphanScan PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-23e7d9fc763496dd8095.log)
  TestBoundedScanPanicIsLoggedWithKeyValueAndStack PASS (unit-12382fa48a81def0b895.log)
  TestBoundedScanPanicIsUnavailableAndReleasesKey PASS (unit-12382fa48a81def0b895.log)
  TestFindRuntimesBySessionIDBoundedCallerCancelIsUnavailable PASS (unit-12382fa48a81def0b895.log)
  TestFindRuntimesBySessionIDBoundedExpiresAtBudget PASS (unit-12382fa48a81def0b895.log)
  TestFindRuntimesBySessionIDBoundedKeyFreeAfterScanReturns PASS (unit-12382fa48a81def0b895.log)
  TestFindRuntimesBySessionIDBoundedLatchIsPerKey PASS (unit-12382fa48a81def0b895.log)
  TestFindRuntimesBySessionIDBoundedLeavesNoGoroutineBehind PASS (unit-12382fa48a81def0b895.log)
  TestFindRuntimesBySessionIDBoundedReturnsAnswerWithinBudget PASS (unit-12382fa48a81def0b895.log)
  TestFindRuntimesBySessionIDBoundedScanFinishingByBudgetWins PASS (unit-12382fa48a81def0b895.log)
  TestFindRuntimesBySessionIDBoundedSecondCallWhileOutstandingStartsNoScan PASS (unit-12382fa48a81def0b895.log)
  TestProcessTableScanUnavailableWrapsRuntimeUnavailable PASS (unit-12382fa48a81def0b895.log)
  TestFindRuntimesBySessionIDWithNoRootsAsksTmuxNothing PASS (integration-packages-15c091b746137e5c2921.log, unit-4bcc997199c6a7426526.log)
  TestFindRuntimesBySessionIDWithNoRootsStillReturnsScanError PASS (integration-packages-5a078b24388ef3c79748.log, unit-ce5f199695f6832daef7.log)
  TestCreateDefersAndRollsBackWhenOrphanScanNeverFinishes PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestKillExistingOrphansBudgetIsCappedByContextDeadline PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestKillExistingOrphansDefersWhenScanNeverFinishes PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestKillExistingOrphansKeysLatchByCityAndSession PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestKillExistingOrphansRepeatedStartsOfWedgedSessionScanOnce PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestKillExistingOrphansStillTerminatesWhenScanReturns PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestKillExistingOrphansStopsBetweenTerminationsWhenContextDone PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestManagerOrphanScanBudgetDefaultsToTenSeconds PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestOrphanScanBudgetFieldIsHonored PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestSameCityDifferentSessionHasItsOwnLatch PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestStaleKeyRetryDefersWhenOrphanScanNeverFinishes PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestStartDefersWhenOrphanScanNeverFinishes PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestStartRuntimeOnlyDefersWhenOrphanScanNeverFinishes PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
GATE_TEST_EVIDENCE_RESULT ok: 28 diff-owned tests verified by name
```

## All required named results

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs logs=518 logs_with_results=452
test_counts: PASS=134243 FAIL=0 SKIP=377
  (top-level result lines, column 0, summed over the 518 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
diff_tests_executed:
  TestAsyncStartReleasesItsLeaseBeforeTheFollowUp PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-27b23e708b477fce4dad.log)
  TestDefersUnfinishedOrphanScanWhenStartDeadlineEndsIt PASS (integration-packages-e4245ddf6a37030111bc.log, unit-2bde55fab377f50b4739.log)
  TestExecutePlannedStarts_PrepareBudgetDeferralTakesNoAsyncSlotOrLease PASS (integration-packages-e5330b8c981927473156.log, unit-cd6930bb0d0da8646500.log)
  TestExecutePreparedStartWaveDefersUnavailableWithoutRollbackOrWakeFailure PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-5a9df316125cd0aeff2e.log)
  TestExecutePreparedStartWaveDefersUnfinishedOrphanScan PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-23e7d9fc763496dd8095.log)
  TestExecutePreparedStartWaveUsesWorkerBoundaryForKnownSession PASS (integration-packages-18745ddf7d62465654f5.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestLegacyAsyncStartHoldsTheLeaseThroughItsCommit PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-5a9df316125cd0aeff2e.log)
  TestLegacyStartReleasesEachBatchsLeases PASS (integration-packages-821a75e4a503601af052.log, unit-4d20b3d28497f844aa39.log)
  TestLegacyStartReleasesLeasesOnAnEarlyReturn PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-2ef13e1b6898c5544764.log)
  TestStartLeaseNameIsTheStartsName PASS (integration-packages-9326181a736eb30b71ca.log, unit-f679cd2551fcc64525aa.log)
  TestStartPreparedStartCandidateConvergesFromConfirmedAbsence PASS (integration-packages-e5330b8c981927473156.log, unit-cd6930bb0d0da8646500.log)
  TestStartPreparedStartCandidateDefersWhenLivenessUnavailable PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-088cc38d26d094729229.log)
  TestStartPreparedStartCandidateDoesNotKillSessionSuspendedDuringSpawn PASS (integration-packages-b72a67a2f9444af705e3.log, unit-912ab643330b35bd0b6b.log)
  TestStartPreparedStartCandidateRefusesRigSuspendedBeforeSpawn PASS (integration-packages-830685c60d0c2dd88567.log, unit-f93a0ca96ccc61d0cae9.log)
  TestStartPreparedStartCandidateRuntimeOnlyBorrowsTheCandidateLease PASS (integration-packages-12697e2ea0d32c278115.log, unit-10cc7795c0ec199a5530.log)
  TestStartPreparedStartCandidateUsesWorkerBoundaryForRuntimeOnlyTarget PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-abd97d2cb9b450225d57.log)
  TestBoundedScanPanicIsLoggedWithKeyValueAndStack PASS (unit-12382fa48a81def0b895.log)
  TestBoundedScanPanicIsUnavailableAndReleasesKey PASS (unit-12382fa48a81def0b895.log)
  TestFindRuntimesBySessionIDBoundedCallerCancelIsUnavailable PASS (unit-12382fa48a81def0b895.log)
  TestFindRuntimesBySessionIDBoundedExpiresAtBudget PASS (unit-12382fa48a81def0b895.log)
  TestFindRuntimesBySessionIDBoundedKeyFreeAfterScanReturns PASS (unit-12382fa48a81def0b895.log)
  TestFindRuntimesBySessionIDBoundedLatchIsPerKey PASS (unit-12382fa48a81def0b895.log)
  TestFindRuntimesBySessionIDBoundedLeavesNoGoroutineBehind PASS (unit-12382fa48a81def0b895.log)
  TestFindRuntimesBySessionIDBoundedReturnsAnswerWithinBudget PASS (unit-12382fa48a81def0b895.log)
  TestFindRuntimesBySessionIDBoundedScanFinishingByBudgetWins PASS (unit-12382fa48a81def0b895.log)
  TestFindRuntimesBySessionIDBoundedSecondCallWhileOutstandingStartsNoScan PASS (unit-12382fa48a81def0b895.log)
  TestProcessTableScanUnavailableWrapsRuntimeUnavailable PASS (unit-12382fa48a81def0b895.log)
  TestObserveTurnStalledServerReturnsByTheDeadline PASS (unit-a23a357eedd1739a53e0.log)
  TestFindRuntimesBySessionIDReportsTrackingFailure PASS (integration-packages-5a078b24388ef3c79748.log, unit-ce5f199695f6832daef7.log)
  TestFindRuntimesBySessionIDWithNoRootsAsksTmuxNothing PASS (integration-packages-15c091b746137e5c2921.log, unit-4bcc997199c6a7426526.log)
  TestFindRuntimesBySessionIDWithNoRootsStillReturnsScanError PASS (integration-packages-5a078b24388ef3c79748.log, unit-ce5f199695f6832daef7.log)
  TestTerminateRuntimeBindsKillToScannedIdentity PASS (integration-packages-74db6ddfc6b7303645d2.log, integration-packages-f1416b63f1c12109ea7e.log +2 more)
  TestCreateDefersAndRollsBackWhenOrphanScanNeverFinishes PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestKillExistingOrphansBudgetIsCappedByContextDeadline PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestKillExistingOrphansDefersWhenScanNeverFinishes PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestKillExistingOrphansKeysLatchByCityAndSession PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestKillExistingOrphansRepeatedStartsOfWedgedSessionScanOnce PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestKillExistingOrphansStillTerminatesWhenScanReturns PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestKillExistingOrphansStopsBetweenTerminationsWhenContextDone PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestManagerOrphanScanBudgetDefaultsToTenSeconds PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestOrphanScanBudgetFieldIsHonored PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestSameCityDifferentSessionHasItsOwnLatch PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestStaleKeyRetryDefersWhenOrphanScanNeverFinishes PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestStartDefersWhenOrphanScanNeverFinishes PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestStartRuntimeOnlyDefersWhenOrphanScanNeverFinishes PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
GATE_TEST_EVIDENCE_RESULT ok: 45 diff-owned tests verified by name
```

## Required race roots

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/deploy-ga-wb0ttep.xa23xylo/orphan-acceptance-combined-logs logs=19 logs_with_results=19
test_counts: PASS=16104 FAIL=0 SKIP=6
  (top-level result lines, column 0, summed over the 19 logs; a test that several jobs run counts once per job; sub-tests are not counted)
diff_tests_executed:
  TestDefersUnfinishedOrphanScanWhenStartDeadlineEndsIt PASS (race-classifier-67b32d42865b6957b213.log, race-classifier-96deccc8a77ff841d4bb.log +1 more)
  TestExecutePreparedStartWaveDefersUnfinishedOrphanScan PASS (race-classifier-67b32d42865b6957b213.log, race-classifier-96deccc8a77ff841d4bb.log +1 more)
  TestBoundedScanPanicIsLoggedWithKeyValueAndStack PASS (race-family-c9b609ce6eb3ce5f7b1a.log, race-family-d40d1721acebf68b7e51.log +1 more)
  TestBoundedScanPanicIsUnavailableAndReleasesKey PASS (race-family-c9b609ce6eb3ce5f7b1a.log, race-family-d40d1721acebf68b7e51.log +1 more)
  TestFindRuntimesBySessionIDBoundedCallerCancelIsUnavailable PASS (race-family-c9b609ce6eb3ce5f7b1a.log, race-family-d40d1721acebf68b7e51.log +1 more)
  TestFindRuntimesBySessionIDBoundedExpiresAtBudget PASS (race-family-c9b609ce6eb3ce5f7b1a.log, race-family-d40d1721acebf68b7e51.log +1 more)
  TestFindRuntimesBySessionIDBoundedKeyFreeAfterScanReturns PASS (race-family-c9b609ce6eb3ce5f7b1a.log, race-family-d40d1721acebf68b7e51.log +1 more)
  TestFindRuntimesBySessionIDBoundedLatchIsPerKey PASS (race-family-c9b609ce6eb3ce5f7b1a.log, race-family-d40d1721acebf68b7e51.log +1 more)
  TestFindRuntimesBySessionIDBoundedLeavesNoGoroutineBehind PASS (race-family-c9b609ce6eb3ce5f7b1a.log, race-family-d40d1721acebf68b7e51.log +1 more)
  TestFindRuntimesBySessionIDBoundedReturnsAnswerWithinBudget PASS (race-family-c9b609ce6eb3ce5f7b1a.log, race-family-d40d1721acebf68b7e51.log +1 more)
  TestFindRuntimesBySessionIDBoundedScanFinishingByBudgetWins PASS (race-family-c9b609ce6eb3ce5f7b1a.log, race-family-d40d1721acebf68b7e51.log +1 more)
  TestFindRuntimesBySessionIDBoundedSecondCallWhileOutstandingStartsNoScan PASS (race-family-c9b609ce6eb3ce5f7b1a.log, race-family-d40d1721acebf68b7e51.log +1 more)
  TestProcessTableScanUnavailableWrapsRuntimeUnavailable PASS (race-family-c9b609ce6eb3ce5f7b1a.log, race-family-d40d1721acebf68b7e51.log +1 more)
  TestFindRuntimesBySessionIDWithNoRootsAsksTmuxNothing PASS (race-family-3c30f55d944a2448f2ae.log, race-family-cd6551a3f56b82a1ec06.log +1 more)
  TestFindRuntimesBySessionIDWithNoRootsStillReturnsScanError PASS (race-family-1ddccfda4c693777a770.log, race-family-ac8c82be8e257bce54b8.log +1 more)
  TestCreateDefersAndRollsBackWhenOrphanScanNeverFinishes PASS (race-family-1caaa76d31e8642b5cad.log, race-family-2e6d75624223c777126e.log +1 more)
  TestKillExistingOrphansBudgetIsCappedByContextDeadline PASS (race-family-1caaa76d31e8642b5cad.log, race-family-2e6d75624223c777126e.log +1 more)
  TestKillExistingOrphansDefersWhenScanNeverFinishes PASS (race-family-1caaa76d31e8642b5cad.log, race-family-2e6d75624223c777126e.log +1 more)
  TestKillExistingOrphansKeysLatchByCityAndSession PASS (race-family-1caaa76d31e8642b5cad.log, race-family-2e6d75624223c777126e.log +1 more)
  TestKillExistingOrphansRepeatedStartsOfWedgedSessionScanOnce PASS (race-family-1caaa76d31e8642b5cad.log, race-family-2e6d75624223c777126e.log +1 more)
  TestKillExistingOrphansStillTerminatesWhenScanReturns PASS (race-family-1caaa76d31e8642b5cad.log, race-family-2e6d75624223c777126e.log +1 more)
  TestKillExistingOrphansStopsBetweenTerminationsWhenContextDone PASS (race-family-1caaa76d31e8642b5cad.log, race-family-2e6d75624223c777126e.log +1 more)
  TestManagerOrphanScanBudgetDefaultsToTenSeconds PASS (race-family-1caaa76d31e8642b5cad.log, race-family-2e6d75624223c777126e.log +1 more)
  TestOrphanScanBudgetFieldIsHonored PASS (race-family-1caaa76d31e8642b5cad.log, race-family-2e6d75624223c777126e.log +1 more)
  TestSameCityDifferentSessionHasItsOwnLatch PASS (race-family-1caaa76d31e8642b5cad.log, race-family-2e6d75624223c777126e.log +1 more)
  TestStaleKeyRetryDefersWhenOrphanScanNeverFinishes PASS (race-family-1caaa76d31e8642b5cad.log, race-family-2e6d75624223c777126e.log +1 more)
  TestStartDefersWhenOrphanScanNeverFinishes PASS (race-family-1caaa76d31e8642b5cad.log, race-family-2e6d75624223c777126e.log +1 more)
  TestStartRuntimeOnlyDefersWhenOrphanScanNeverFinishes PASS (race-family-1caaa76d31e8642b5cad.log, race-family-2e6d75624223c777126e.log +1 more)
GATE_TEST_EVIDENCE_RESULT ok: 28 diff-owned tests verified by name
```

## Complexity at frozen base

```text
complexity: changes relative to baseline:
regressed: 630 main reconcileSessionBeadsTracedWithNamedDemand cmd/gc/session_reconciler.go (baseline 614)
regressed: 255 main syncSessionBeadsWithSnapshotAndRigStores cmd/gc/session_beads.go (baseline 253)
regressed: 172 main doStorageRecoverStranded cmd/gc/infra_class_recover.go (baseline 95)
regressed: 139 config LoadWithIncludesOptions internal/config/compose.go (baseline 138)
new: 98 beads (*CachingStore).applyEventFor internal/beads/caching_store_events.go
regressed: 96 main executePlannedStartsTraced cmd/gc/session_lifecycle_parallel.go (baseline 94)
regressed: 96 main resolveTemplate cmd/gc/template_resolve.go (baseline 87)
regressed: 92 main (*CityRuntime).reloadConfigTraced cmd/gc/city_runtime.go (baseline 86)
regressed: 81 main desiredRowIdentity cmd/gc/reconcile_arms_metadata.go (baseline 80)
regressed: 64 opjournal planRules internal/opjournal/lane_plan.go (baseline 60)
regressed: 63 main startOneCity cmd/gc/cmd_supervisor.go (baseline 60)
new: 60 main importRig cmd/gc/rig_import.go
regressed: 58 main clearRetainedInfraCopies cmd/gc/infra_class_clear.go (baseline 54)
regressed: 57 api (*Server).humaHandleSling internal/api/huma_handlers_sling.go (baseline 56)
regressed: 56 main buildDoctorChecks cmd/gc/cmd_doctor.go (baseline 55)
regressed: 55 main newFormulaCookCmd cmd/gc/cmd_formula.go (baseline 50)
regressed: 55 workladder Decide internal/workladder/decide.go (baseline 53)
regressed: 53 main cmdNudgeDrainWithFormat cmd/gc/cmd_nudge.go (baseline 45)
regressed: 53 main runPreparedStartCandidate cmd/gc/session_lifecycle_parallel.go (baseline 51)
regressed: 53 materialize Run internal/materialize/skills.go (baseline 44)
regressed: 52 api (*Server).humaHandleAgentList internal/api/huma_handlers_agents.go (baseline 51)
regressed: 49 main doBd cmd/gc/cmd_bd.go (baseline 40)
new: 46 main rigImportClear cmd/gc/rig_import.go
new: 44 main planRigImport cmd/gc/rig_import_plan.go
new: 42 beads (*CachingStore).RefreshRowReport internal/beads/caching_store_reads.go
regressed: 41 main cmdHookWithOptions cmd/gc/cmd_hook.go (baseline 40)
new: 41 main doStorageStatusCity cmd/gc/cmd_storage.go
new: 39 api (*Server).acceptSessionCreate internal/api/huma_handlers_sessions_command.go
new: 39 main (*workMoleculeGC).pass cmd/gc/wisp_gc_work_lane.go
regressed: 39 main doInit cmd/gc/cmd_init.go (baseline 38)
new: 39 session (*Manager).StopTurnWith internal/session/stop_turn_with.go
new: 38 dispatch ReleaseCrossStoreGates internal/dispatch/cross_store_gate.go
new: 38 main resolveBdInventory cmd/gc/bd_inventory_trust.go
regressed: 38 opjournal (*takePlan).planContent internal/opjournal/lane_plan.go (baseline 33)
regressed: 37 main tryDeliverQueuedNudgesByPoller cmd/gc/cmd_nudge.go (baseline 32)
new: 37 worker OperatorKill internal/worker/operator_kill.go
regressed: 36 dispatch expandDrain internal/dispatch/drain.go (baseline 33)
regressed: 35 api (*Server).createProviderSession internal/api/handler_session_create.go (baseline 34)
regressed: 35 main (*hookClaimOps).applyDefaults cmd/gc/cmd_hook_claim.go (baseline 31)
new: 35 main censusRig cmd/gc/rig_import_census.go
regressed: 35 main cmdInitFromTOMLFileWithOptionsInternal cmd/gc/cmd_init.go (baseline 34)
regressed: 34 config mergeFragment internal/config/compose.go (baseline 27)
new: 34 main initAndHookDirResolvingDatabase cmd/gc/beads_provider_lifecycle.go
new: 34 main startBeadsLifecycleResolvingCityDatabase cmd/gc/beads_provider_lifecycle.go
regressed: 34 tmux (*Tmux).nudgeSession internal/runtime/tmux/tmux.go (baseline 28)
new: 34 tmux closeClaudeBackgroundShells internal/runtime/tmux/claude_background_stop.go
regressed: 33 main bdRigQualifiedMetadataRefusal cmd/gc/cmd_bd.go (baseline 31)
regressed: 33 main classRoutedHookClaimOps cmd/gc/claim_class_route.go (baseline 23)
regressed: 33 main doInitFromDirWithOptionsFSInternal cmd/gc/cmd_init.go (baseline 32)
regressed: 32 opjournal (*ConversationLane).Pass internal/opjournal/lane.go (baseline 24)
regressed: 31 api (*Server).execSling internal/api/handler_sling.go (baseline 29)
new: 31 main (*danglingBlockingEdgesCheck).collect cmd/gc/doctor_cross_store.go
new: 31 main buildDesiredStateWithSessionBeadsAtOpts cmd/gc/build_desired_state.go
regressed: 31 main parseBdByIDUpdateArgs cmd/gc/cmd_bd_by_id.go (baseline 30)
regressed: 31 main runInfraClassMigrationFrom cmd/gc/infra_class_migrate.go (baseline 28)
new: 30 opjournal (*rulesPlan).planNowInterrupt internal/opjournal/lane_now.go
regressed: 30 sessionlog ReadCodexFile internal/sessionlog/codex_reader.go (baseline 23)
regressed: 29 main collectAssignedWorkBeadsWithStores cmd/gc/build_desired_state.go (baseline 27)
regressed: 29 main doSessionWake cmd/gc/cmd_session_wake.go (baseline 27)
new: 29 main rigImportVerify cmd/gc/rig_import.go
regressed: 29 main runNudgeBackstop cmd/gc/nudge_backstop.go (baseline 21)
new: 29 readywork (*crossStore).blockedRows internal/readywork/crossstore.go
new: 28 contract resolveDoltConnectionTarget internal/beads/contract/connection.go
regressed: 28 main (*routeRecoveryLane).backstopLeg cmd/gc/route_recovery_lane.go (baseline 22)
new: 28 main deliverSessionNudgeLegacy cmd/gc/cmd_nudge.go
regressed: 28 main doRigAddWithResult cmd/gc/cmd_rig.go (baseline 26)
new: 28 main readInfraCopyManifestRecordAt cmd/gc/infra_copy_manifest.go
new: 28 main rigImportReprove cmd/gc/rig_import.go
regressed: 28 sling preflight internal/sling/sling_core.go (baseline 27)
regressed: 27 coordclass classifyFields internal/coordclass/classify.go (baseline 23)
regressed: 27 main commitAsyncStartResultWithContext cmd/gc/session_lifecycle_parallel.go (baseline 23)
regressed: 27 main wakeWillNotStart cmd/gc/api_state_wake_refusal.go (baseline 25)
regressed: 27 opjournal (*ConversationLane).settle internal/opjournal/lane_done.go (baseline 23)
regressed: 27 tmux (*tmuxFetcher).FetchState internal/runtime/tmux/state_cache.go (baseline 26)
regressed: 27 workladder sameRecord internal/workladder/decide.go (baseline 26)
new: 26 bdflags ParseArgs internal/bdflags/commands.go
new: 26 main (*bdInventoryCheck).Run cmd/gc/doctor_bd_inventory.go
new: 26 main (*conversationLaneWorker).await cmd/gc/conversation_lane_worker.go
regressed: 26 main handleControllerConn cmd/gc/controller.go (baseline 21)
regressed: 26 opjournal (*ConversationLane).reconcile internal/opjournal/lane_done.go (baseline 23)
regressed: 26 session (*Manager).Suspend internal/session/manager.go (baseline 24)
regressed: 25 api (*SupervisorMux).streamGlobalEvents internal/api/huma_handlers_supervisor.go (baseline 24)
new: 25 main executePreparedStartWaveForCity cmd/gc/session_lifecycle_parallel.go
regressed: 25 main v2ClassStoreCapabilities cmd/gc/city_runtime_v2.go (baseline 24)
regressed: 25 workladder DecodeLadder internal/workladder/codec.go (baseline 21)
regressed: 24 beads sqliteListSQL internal/beads/sqlite_store.go (baseline 22)
new: 24 dispatch processWorkflowFinalize internal/dispatch/runtime.go
new: 24 main (*ledgerFeed).refreshRows cmd/gc/ledger_feed.go
new: 24 main dispatchReadyWaitNudgesWithSnapshot cmd/gc/cmd_wait.go
new: 24 main doStorageAbandonRigStore cmd/gc/rig_import.go
new: 24 main fixDanglingEdge cmd/gc/doctor_cross_store.go
new: 24 main judgeBdImport cmd/gc/bd_split_judges.go
new: 24 main normalizeCanonicalBdScopeFilesResolvingCityDatabase cmd/gc/beads_provider_lifecycle.go
new: 24 main resolveBdSplitCommand cmd/gc/bd_split_allowlist.go
new: 24 materialize (MCPProjection).applyWithStderr internal/materialize/mcp_project.go
regressed: 24 worker (*SessionHandle).Nudge internal/worker/handle_lifecycle.go (baseline 21)
regressed: 23 main claimFirstReadyHookAssignment cmd/gc/cmd_hook_claim.go (baseline 21)
new: 23 main cmdSessionSubmit cmd/gc/cmd_session.go
regressed: 23 main dispatchQueuedNudges cmd/gc/nudge_dispatcher.go (baseline 22)
new: 23 main storageBootGate cmd/gc/storage_boot.go
regressed: 23 opjournal (*ConversationLane).writeDone internal/opjournal/lane_done.go (baseline 20)
regressed: 23 session (*Store).CommitClose internal/session/store.go (baseline 21)
new: 23 sling checkBatchNoMoleculeChildren internal/sling/sling_attachment.go
new: 22 events readFilteredTrackedContext internal/events/reader_context.go
new: 22 main (*laneTurnObserver).writeOnce cmd/gc/conversation_lane_turns.go
new: 22 main bestStoreWithWork cmd/gc/hook_cross_store.go
new: 22 main collectExpiredBeadClosureStatus cmd/gc/wisp_gc.go
new: 22 main rigImportSharedStoreRefusal cmd/gc/rig_import.go
regressed: 22 opjournal (*takePlan).formBatch internal/opjournal/lane_plan.go (baseline 20)
new: 22 opjournal planTake internal/opjournal/lane_plan.go
regressed: 22 opjournal validateRequestShape internal/opjournal/request.go (baseline 20)
regressed: 21 api humaSessionManagerError internal/api/huma_handlers_sessions.go (baseline 20)
new: 21 beads (*BdStore).ChildrenOfAny internal/beads/children_batch.go
new: 21 main (*controllerState).ConditionalWritesStatus cmd/gc/api_state_conditional_writes.go
new: 21 main rigClearChunks cmd/gc/rig_import.go
new: 21 main tryHookClaim cmd/gc/cmd_hook_claim.go
new: 20 api writeSessionManagerError internal/api/handler_session_errors.go
new: 20 beads (*BdStore).fetchViaBDList internal/beads/bdstore.go
new: 20 dispatch drainProjectedBlockerIDs internal/dispatch/drain.go
new: 20 main (*opSweeper).sweep cmd/gc/op_sweep.go
new: 20 main claimFirstEligibleHookCandidate cmd/gc/cmd_hook_claim.go
new: 20 main closeAbandonedRootCandidatesMode cmd/gc/wisp_gc.go
new: 20 main cmdWaitSetStateResult cmd/gc/cmd_wait.go
new: 20 main purgeExpiredBeadClosuresWith cmd/gc/wisp_gc.go
new: 20 main resolveProjectedMCPForTarget cmd/gc/mcp_integration.go
new: 20 main writeHookClaimWorkResultForBead cmd/gc/cmd_hook_claim.go
new: 20 opjournal (RuntimeRow).validateShape internal/opjournal/runtime_row.go
new: 20 tmux readCodexSubmitRecord internal/runtime/tmux/submit_turn_guard.go
make: *** [Makefile:216: complexity-check] Error 1
```

## Complexity at canonical merge

```text
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.wb0ttep.5o1_2t2s head=0d781d66b8fa tree=77737e729c79 dir=/var/tmp/gc-fresh-tree.uqSKP9 modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.wb0ttep.5o1_2t2s,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=adf99aa6c84d83bfc1492de5bd969231a7edf7f0 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.uqSKP9
complexity: changes relative to baseline:
regressed: 630 main reconcileSessionBeadsTracedWithNamedDemand cmd/gc/session_reconciler.go (baseline 614)
regressed: 255 main syncSessionBeadsWithSnapshotAndRigStores cmd/gc/session_beads.go (baseline 253)
regressed: 172 main doStorageRecoverStranded cmd/gc/infra_class_recover.go (baseline 95)
regressed: 139 config LoadWithIncludesOptions internal/config/compose.go (baseline 138)
new: 98 beads (*CachingStore).applyEventFor internal/beads/caching_store_events.go
regressed: 96 main executePlannedStartsTraced cmd/gc/session_lifecycle_parallel.go (baseline 94)
regressed: 96 main resolveTemplate cmd/gc/template_resolve.go (baseline 87)
regressed: 92 main (*CityRuntime).reloadConfigTraced cmd/gc/city_runtime.go (baseline 86)
regressed: 81 main desiredRowIdentity cmd/gc/reconcile_arms_metadata.go (baseline 80)
regressed: 64 opjournal planRules internal/opjournal/lane_plan.go (baseline 60)
regressed: 63 main startOneCity cmd/gc/cmd_supervisor.go (baseline 60)
new: 60 main importRig cmd/gc/rig_import.go
regressed: 58 main clearRetainedInfraCopies cmd/gc/infra_class_clear.go (baseline 54)
regressed: 57 api (*Server).humaHandleSling internal/api/huma_handlers_sling.go (baseline 56)
regressed: 56 main buildDoctorChecks cmd/gc/cmd_doctor.go (baseline 55)
regressed: 55 main newFormulaCookCmd cmd/gc/cmd_formula.go (baseline 50)
regressed: 55 workladder Decide internal/workladder/decide.go (baseline 53)
regressed: 53 main cmdNudgeDrainWithFormat cmd/gc/cmd_nudge.go (baseline 45)
regressed: 53 main runPreparedStartCandidate cmd/gc/session_lifecycle_parallel.go (baseline 51)
regressed: 53 materialize Run internal/materialize/skills.go (baseline 44)
regressed: 52 api (*Server).humaHandleAgentList internal/api/huma_handlers_agents.go (baseline 51)
regressed: 49 main doBd cmd/gc/cmd_bd.go (baseline 40)
new: 46 main rigImportClear cmd/gc/rig_import.go
new: 44 main planRigImport cmd/gc/rig_import_plan.go
new: 42 beads (*CachingStore).RefreshRowReport internal/beads/caching_store_reads.go
regressed: 41 main cmdHookWithOptions cmd/gc/cmd_hook.go (baseline 40)
new: 41 main doStorageStatusCity cmd/gc/cmd_storage.go
new: 39 api (*Server).acceptSessionCreate internal/api/huma_handlers_sessions_command.go
new: 39 main (*workMoleculeGC).pass cmd/gc/wisp_gc_work_lane.go
regressed: 39 main doInit cmd/gc/cmd_init.go (baseline 38)
new: 39 session (*Manager).StopTurnWith internal/session/stop_turn_with.go
new: 38 dispatch ReleaseCrossStoreGates internal/dispatch/cross_store_gate.go
new: 38 main resolveBdInventory cmd/gc/bd_inventory_trust.go
regressed: 38 opjournal (*takePlan).planContent internal/opjournal/lane_plan.go (baseline 33)
regressed: 37 main tryDeliverQueuedNudgesByPoller cmd/gc/cmd_nudge.go (baseline 32)
new: 37 worker OperatorKill internal/worker/operator_kill.go
regressed: 36 dispatch expandDrain internal/dispatch/drain.go (baseline 33)
regressed: 35 api (*Server).createProviderSession internal/api/handler_session_create.go (baseline 34)
regressed: 35 main (*hookClaimOps).applyDefaults cmd/gc/cmd_hook_claim.go (baseline 31)
new: 35 main censusRig cmd/gc/rig_import_census.go
regressed: 35 main cmdInitFromTOMLFileWithOptionsInternal cmd/gc/cmd_init.go (baseline 34)
regressed: 34 config mergeFragment internal/config/compose.go (baseline 27)
new: 34 main initAndHookDirResolvingDatabase cmd/gc/beads_provider_lifecycle.go
new: 34 main startBeadsLifecycleResolvingCityDatabase cmd/gc/beads_provider_lifecycle.go
regressed: 34 tmux (*Tmux).nudgeSession internal/runtime/tmux/tmux.go (baseline 28)
new: 34 tmux closeClaudeBackgroundShells internal/runtime/tmux/claude_background_stop.go
regressed: 33 main bdRigQualifiedMetadataRefusal cmd/gc/cmd_bd.go (baseline 31)
regressed: 33 main classRoutedHookClaimOps cmd/gc/claim_class_route.go (baseline 23)
regressed: 33 main doInitFromDirWithOptionsFSInternal cmd/gc/cmd_init.go (baseline 32)
regressed: 32 opjournal (*ConversationLane).Pass internal/opjournal/lane.go (baseline 24)
regressed: 31 api (*Server).execSling internal/api/handler_sling.go (baseline 29)
new: 31 main (*danglingBlockingEdgesCheck).collect cmd/gc/doctor_cross_store.go
new: 31 main buildDesiredStateWithSessionBeadsAtOpts cmd/gc/build_desired_state.go
regressed: 31 main parseBdByIDUpdateArgs cmd/gc/cmd_bd_by_id.go (baseline 30)
regressed: 31 main runInfraClassMigrationFrom cmd/gc/infra_class_migrate.go (baseline 28)
new: 30 opjournal (*rulesPlan).planNowInterrupt internal/opjournal/lane_now.go
regressed: 30 sessionlog ReadCodexFile internal/sessionlog/codex_reader.go (baseline 23)
regressed: 29 main collectAssignedWorkBeadsWithStores cmd/gc/build_desired_state.go (baseline 27)
regressed: 29 main doSessionWake cmd/gc/cmd_session_wake.go (baseline 27)
new: 29 main rigImportVerify cmd/gc/rig_import.go
regressed: 29 main runNudgeBackstop cmd/gc/nudge_backstop.go (baseline 21)
new: 29 readywork (*crossStore).blockedRows internal/readywork/crossstore.go
new: 28 contract resolveDoltConnectionTarget internal/beads/contract/connection.go
regressed: 28 main (*routeRecoveryLane).backstopLeg cmd/gc/route_recovery_lane.go (baseline 22)
new: 28 main deliverSessionNudgeLegacy cmd/gc/cmd_nudge.go
regressed: 28 main doRigAddWithResult cmd/gc/cmd_rig.go (baseline 26)
new: 28 main readInfraCopyManifestRecordAt cmd/gc/infra_copy_manifest.go
new: 28 main rigImportReprove cmd/gc/rig_import.go
regressed: 28 sling preflight internal/sling/sling_core.go (baseline 27)
regressed: 27 coordclass classifyFields internal/coordclass/classify.go (baseline 23)
regressed: 27 main commitAsyncStartResultWithContext cmd/gc/session_lifecycle_parallel.go (baseline 23)
regressed: 27 main wakeWillNotStart cmd/gc/api_state_wake_refusal.go (baseline 25)
regressed: 27 opjournal (*ConversationLane).settle internal/opjournal/lane_done.go (baseline 23)
regressed: 27 tmux (*tmuxFetcher).FetchState internal/runtime/tmux/state_cache.go (baseline 26)
regressed: 27 workladder sameRecord internal/workladder/decide.go (baseline 26)
new: 26 bdflags ParseArgs internal/bdflags/commands.go
new: 26 main (*bdInventoryCheck).Run cmd/gc/doctor_bd_inventory.go
new: 26 main (*conversationLaneWorker).await cmd/gc/conversation_lane_worker.go
regressed: 26 main handleControllerConn cmd/gc/controller.go (baseline 21)
regressed: 26 opjournal (*ConversationLane).reconcile internal/opjournal/lane_done.go (baseline 23)
regressed: 26 session (*Manager).Suspend internal/session/manager.go (baseline 24)
regressed: 25 api (*SupervisorMux).streamGlobalEvents internal/api/huma_handlers_supervisor.go (baseline 24)
new: 25 main executePreparedStartWaveForCity cmd/gc/session_lifecycle_parallel.go
regressed: 25 main v2ClassStoreCapabilities cmd/gc/city_runtime_v2.go (baseline 24)
regressed: 25 workladder DecodeLadder internal/workladder/codec.go (baseline 21)
regressed: 24 beads sqliteListSQL internal/beads/sqlite_store.go (baseline 22)
new: 24 dispatch processWorkflowFinalize internal/dispatch/runtime.go
new: 24 main (*ledgerFeed).refreshRows cmd/gc/ledger_feed.go
new: 24 main dispatchReadyWaitNudgesWithSnapshot cmd/gc/cmd_wait.go
new: 24 main doStorageAbandonRigStore cmd/gc/rig_import.go
new: 24 main fixDanglingEdge cmd/gc/doctor_cross_store.go
new: 24 main judgeBdImport cmd/gc/bd_split_judges.go
new: 24 main normalizeCanonicalBdScopeFilesResolvingCityDatabase cmd/gc/beads_provider_lifecycle.go
new: 24 main resolveBdSplitCommand cmd/gc/bd_split_allowlist.go
new: 24 materialize (MCPProjection).applyWithStderr internal/materialize/mcp_project.go
regressed: 24 worker (*SessionHandle).Nudge internal/worker/handle_lifecycle.go (baseline 21)
regressed: 23 main claimFirstReadyHookAssignment cmd/gc/cmd_hook_claim.go (baseline 21)
new: 23 main cmdSessionSubmit cmd/gc/cmd_session.go
regressed: 23 main dispatchQueuedNudges cmd/gc/nudge_dispatcher.go (baseline 22)
new: 23 main storageBootGate cmd/gc/storage_boot.go
regressed: 23 opjournal (*ConversationLane).writeDone internal/opjournal/lane_done.go (baseline 20)
regressed: 23 session (*Store).CommitClose internal/session/store.go (baseline 21)
new: 23 sling checkBatchNoMoleculeChildren internal/sling/sling_attachment.go
new: 22 events readFilteredTrackedContext internal/events/reader_context.go
new: 22 main (*laneTurnObserver).writeOnce cmd/gc/conversation_lane_turns.go
new: 22 main bestStoreWithWork cmd/gc/hook_cross_store.go
new: 22 main collectExpiredBeadClosureStatus cmd/gc/wisp_gc.go
new: 22 main rigImportSharedStoreRefusal cmd/gc/rig_import.go
regressed: 22 opjournal (*takePlan).formBatch internal/opjournal/lane_plan.go (baseline 20)
new: 22 opjournal planTake internal/opjournal/lane_plan.go
regressed: 22 opjournal validateRequestShape internal/opjournal/request.go (baseline 20)
regressed: 21 api humaSessionManagerError internal/api/huma_handlers_sessions.go (baseline 20)
new: 21 beads (*BdStore).ChildrenOfAny internal/beads/children_batch.go
new: 21 main (*controllerState).ConditionalWritesStatus cmd/gc/api_state_conditional_writes.go
new: 21 main rigClearChunks cmd/gc/rig_import.go
new: 21 main tryHookClaim cmd/gc/cmd_hook_claim.go
new: 20 api writeSessionManagerError internal/api/handler_session_errors.go
new: 20 beads (*BdStore).fetchViaBDList internal/beads/bdstore.go
new: 20 dispatch drainProjectedBlockerIDs internal/dispatch/drain.go
new: 20 main (*opSweeper).sweep cmd/gc/op_sweep.go
new: 20 main claimFirstEligibleHookCandidate cmd/gc/cmd_hook_claim.go
new: 20 main closeAbandonedRootCandidatesMode cmd/gc/wisp_gc.go
new: 20 main cmdWaitSetStateResult cmd/gc/cmd_wait.go
new: 20 main purgeExpiredBeadClosuresWith cmd/gc/wisp_gc.go
new: 20 main resolveProjectedMCPForTarget cmd/gc/mcp_integration.go
new: 20 main writeHookClaimWorkResultForBead cmd/gc/cmd_hook_claim.go
new: 20 opjournal (RuntimeRow).validateShape internal/opjournal/runtime_row.go
new: 20 tmux readCodexSubmitRecord internal/runtime/tmux/submit_turn_guard.go
make: *** [Makefile:216: complexity-check] Error 1
```

## Complexity diff

```text
complexity: diff against adf99aa6c84d83bfc1492de5bd969231a7edf7f0
complexity: no threshold changes
```

## Farm skip sites

```text
diff-owned-tests evidence (gm-vlkzni)  base=adf99aa6c84d head=0d781d66b8fa
rule: OWNED = the diff adds the test, or a hunk lands inside its own function
FILE cmd/gc/session_lifecycle_start_boundary_test.go
  hunks (new side): +413,199
  hunk +413,199  touches method Start (lines 421-421)  [shared: tests that call it are affected but not owned]
  hunk +413,199  touches TestExecutePreparedStartWaveDefersUnfinishedOrphanScan (lines 429-526)
  hunk +413,199  touches method Start (lines 535-537)  [shared: tests that call it are affected but not owned]
  hunk +413,199  touches TestDefersUnfinishedOrphanScanWhenStartDeadlineEndsIt (lines 539-611)
  TestExecutePreparedStartWaveUsesWorkerBoundaryForKnownSession lines 54-104  not owned  (no hunk touches lines 54-104)
  TestStartPreparedStartCandidateUsesWorkerBoundaryForRuntimeOnlyTarget lines 106-157  not owned  (no hunk touches lines 106-157)
  TestStartPreparedStartCandidateRefusesRigSuspendedBeforeSpawn lines 159-180  not owned  (no hunk touches lines 159-180)
  TestStartPreparedStartCandidateDoesNotKillSessionSuspendedDuringSpawn lines 182-213  not owned  (no hunk touches lines 182-213)
  TestStartPreparedStartCandidateDefersWhenLivenessUnavailable lines 215-248  not owned  (no hunk touches lines 215-248)
  TestStartPreparedStartCandidateConvergesFromConfirmedAbsence lines 250-271  not owned  (no hunk touches lines 250-271)
  TestExecutePreparedStartWaveDefersUnavailableWithoutRollbackOrWakeFailure lines 273-349  not owned  (no hunk touches lines 273-349)
  TestStartPreparedStartCandidateRuntimeOnlyBorrowsTheCandidateLease lines 356-393  not owned  (no hunk touches lines 356-393)
  TestStartLeaseNameIsTheStartsName        lines 398-412  not owned  (no hunk touches lines 398-412)
  TestExecutePreparedStartWaveDefersUnfinishedOrphanScan lines 429-526  OWNED      (hunk +413,199 touches the function)
  TestDefersUnfinishedOrphanScanWhenStartDeadlineEndsIt lines 539-611  OWNED      (hunk +413,199 touches the function)
SHARED-HUNKS cmd/gc/session_lifecycle_start_boundary_test.go: 1  (a helper, TestMain/init or file-level code changed; a not-owned SKIP in this file is pre-existing ONLY if none of these can reach it)
FILE internal/runtime/process_table_scan_bounded_test.go
  hunks (new side): +1,311
  hunk +1,311    touches helper newBlockedScanner (lines 30-32)  [shared: tests that call it are affected but not owned]
  hunk +1,311    touches method FindRuntimesBySessionID (lines 34-38)  [shared: tests that call it are affected but not owned]
  hunk +1,311    touches method TerminateRuntime (lines 40-40)  [shared: tests that call it are affected but not owned]
  hunk +1,311    touches method unblock (lines 45-45)  [shared: tests that call it are affected but not owned]
  hunk +1,311    touches method FindRuntimesBySessionID (lines 54-57)  [shared: tests that call it are affected but not owned]
  hunk +1,311    touches method TerminateRuntime (lines 59-59)  [shared: tests that call it are affected but not owned]
  hunk +1,311    touches TestFindRuntimesBySessionIDBoundedReturnsAnswerWithinBudget (lines 61-81)
  hunk +1,311    touches TestFindRuntimesBySessionIDBoundedExpiresAtBudget (lines 83-110)
  hunk +1,311    touches TestFindRuntimesBySessionIDBoundedCallerCancelIsUnavailable (lines 112-131)
  hunk +1,311    touches TestFindRuntimesBySessionIDBoundedSecondCallWhileOutstandingStartsNoScan (lines 133-157)
  hunk +1,311    touches TestFindRuntimesBySessionIDBoundedLatchIsPerKey (lines 159-176)
  hunk +1,311    touches TestFindRuntimesBySessionIDBoundedKeyFreeAfterScanReturns (lines 178-200)
  hunk +1,311    touches TestFindRuntimesBySessionIDBoundedScanFinishingByBudgetWins (lines 202-219)
  hunk +1,311    touches TestFindRuntimesBySessionIDBoundedLeavesNoGoroutineBehind (lines 221-232)
  hunk +1,311    touches TestProcessTableScanUnavailableWrapsRuntimeUnavailable (lines 234-238)
  hunk +1,311    touches method FindRuntimesBySessionID (lines 243-245)  [shared: tests that call it are affected but not owned]
  hunk +1,311    touches method TerminateRuntime (lines 247-247)  [shared: tests that call it are affected but not owned]
  hunk +1,311    touches TestBoundedScanPanicIsUnavailableAndReleasesKey (lines 249-266)
  hunk +1,311    touches TestBoundedScanPanicIsLoggedWithKeyValueAndStack (lines 271-311)
  TestFindRuntimesBySessionIDBoundedReturnsAnswerWithinBudget lines 61-81  OWNED      (hunk +1,311 touches the function)
  TestFindRuntimesBySessionIDBoundedExpiresAtBudget lines 83-110  OWNED      (hunk +1,311 touches the function)
  TestFindRuntimesBySessionIDBoundedCallerCancelIsUnavailable lines 112-131  OWNED      (hunk +1,311 touches the function)
  TestFindRuntimesBySessionIDBoundedSecondCallWhileOutstandingStartsNoScan lines 133-157  OWNED      (hunk +1,311 touches the function)
  TestFindRuntimesBySessionIDBoundedLatchIsPerKey lines 159-176  OWNED      (hunk +1,311 touches the function)
  TestFindRuntimesBySessionIDBoundedKeyFreeAfterScanReturns lines 178-200  OWNED      (hunk +1,311 touches the function)
  TestFindRuntimesBySessionIDBoundedScanFinishingByBudgetWins lines 202-219  OWNED      (hunk +1,311 touches the function)
  TestFindRuntimesBySessionIDBoundedLeavesNoGoroutineBehind lines 221-232  OWNED      (hunk +1,311 touches the function)
  TestProcessTableScanUnavailableWrapsRuntimeUnavailable lines 234-238  OWNED      (hunk +1,311 touches the function)
  TestBoundedScanPanicIsUnavailableAndReleasesKey lines 249-266  OWNED      (hunk +1,311 touches the function)
  TestBoundedScanPanicIsLoggedWithKeyValueAndStack lines 271-311  OWNED      (hunk +1,311 touches the function)
SHARED-HUNKS internal/runtime/process_table_scan_bounded_test.go: 1  (a helper, TestMain/init or file-level code changed; a not-owned SKIP in this file is pre-existing ONLY if none of these can reach it)
FILE internal/runtime/tmux/proctable_tracking_test.go
  hunks (new side): +98,42
  hunk +98,42    touches TestFindRuntimesBySessionIDWithNoRootsAsksTmuxNothing (lines 102-119)
  hunk +98,42    touches TestFindRuntimesBySessionIDWithNoRootsStillReturnsScanError (lines 121-139)
  TestFindRuntimesBySessionIDReportsTrackingFailure lines 46-75  not owned  (no hunk touches lines 46-75)
  TestTerminateRuntimeBindsKillToScannedIdentity lines 84-97  not owned  (no hunk touches lines 84-97)
  TestFindRuntimesBySessionIDWithNoRootsAsksTmuxNothing lines 102-119  OWNED      (hunk +98,42 touches the function)
  TestFindRuntimesBySessionIDWithNoRootsStillReturnsScanError lines 121-139  OWNED      (hunk +98,42 touches the function)
FILE internal/session/orphan_scan_bound_test.go
  hunks (new side): +1,449
  hunk +1,449    touches helper newWedgedScanProvider (lines 46-51)  [shared: tests that call it are affected but not owned]
  hunk +1,449    touches method unwedge (lines 56-56)  [shared: tests that call it are affected but not owned]
  hunk +1,449    touches method Start (lines 58-61)  [shared: tests that call it are affected but not owned]
  hunk +1,449    touches method FindRuntimesBySessionID (lines 63-76)  [shared: tests that call it are affected but not owned]
  hunk +1,449    touches method TerminateRuntime (lines 78-84)  [shared: tests that call it are affected but not owned]
  hunk +1,449    touches helper createOptsForOrphanScan (lines 86-89)  [shared: tests that call it are affected but not owned]
  hunk +1,449    touches helper seedSuspendedTargetWithProvider (lines 95-112)  [shared: tests that call it are affected but not owned]
  hunk +1,449    touches helper assertDeferredByUnfinishedScan (lines 118-135)  [shared: tests that call it are affected but not owned]
  hunk +1,449    touches TestManagerOrphanScanBudgetDefaultsToTenSeconds (lines 137-142)
  hunk +1,449    touches TestKillExistingOrphansDefersWhenScanNeverFinishes (lines 144-166)
  hunk +1,449    touches TestKillExistingOrphansBudgetIsCappedByContextDeadline (lines 168-190)
  hunk +1,449    touches TestKillExistingOrphansRepeatedStartsOfWedgedSessionScanOnce (lines 192-209)
  hunk +1,449    touches TestKillExistingOrphansKeysLatchByCityAndSession (lines 211-232)
  hunk +1,449    touches TestStartDefersWhenOrphanScanNeverFinishes (lines 234-250)
  hunk +1,449    touches TestStartRuntimeOnlyDefersWhenOrphanScanNeverFinishes (lines 252-269)
  hunk +1,449    touches TestCreateDefersAndRollsBackWhenOrphanScanNeverFinishes (lines 271-293)
  hunk +1,449    touches TestKillExistingOrphansStillTerminatesWhenScanReturns (lines 297-312)
  hunk +1,449    touches TestKillExistingOrphansStopsBetweenTerminationsWhenContextDone (lines 317-340)
  hunk +1,449    touches TestSameCityDifferentSessionHasItsOwnLatch (lines 342-363)
  hunk +1,449    touches TestOrphanScanBudgetFieldIsHonored (lines 365-384)
  hunk +1,449    touches method Start (lines 393-400)  [shared: tests that call it are affected but not owned]
  hunk +1,449    touches TestStaleKeyRetryDefersWhenOrphanScanNeverFinishes (lines 402-449)
  TestManagerOrphanScanBudgetDefaultsToTenSeconds lines 137-142  OWNED      (hunk +1,449 touches the function)
  TestKillExistingOrphansDefersWhenScanNeverFinishes lines 144-166  OWNED      (hunk +1,449 touches the function)
  TestKillExistingOrphansBudgetIsCappedByContextDeadline lines 168-190  OWNED      (hunk +1,449 touches the function)
  TestKillExistingOrphansRepeatedStartsOfWedgedSessionScanOnce lines 192-209  OWNED      (hunk +1,449 touches the function)
  TestKillExistingOrphansKeysLatchByCityAndSession lines 211-232  OWNED      (hunk +1,449 touches the function)
  TestStartDefersWhenOrphanScanNeverFinishes lines 234-250  OWNED      (hunk +1,449 touches the function)
  TestStartRuntimeOnlyDefersWhenOrphanScanNeverFinishes lines 252-269  OWNED      (hunk +1,449 touches the function)
  TestCreateDefersAndRollsBackWhenOrphanScanNeverFinishes lines 271-293  OWNED      (hunk +1,449 touches the function)
  TestKillExistingOrphansStillTerminatesWhenScanReturns lines 297-312  OWNED      (hunk +1,449 touches the function)
  TestKillExistingOrphansStopsBetweenTerminationsWhenContextDone lines 317-340  OWNED      (hunk +1,449 touches the function)
  TestSameCityDifferentSessionHasItsOwnLatch lines 342-363  OWNED      (hunk +1,449 touches the function)
  TestOrphanScanBudgetFieldIsHonored       lines 365-384  OWNED      (hunk +1,449 touches the function)
  TestStaleKeyRetryDefersWhenOrphanScanNeverFinishes lines 402-449  OWNED      (hunk +1,449 touches the function)
SHARED-HUNKS internal/session/orphan_scan_bound_test.go: 1  (a helper, TestMain/init or file-level code changed; a not-owned SKIP in this file is pre-existing ONLY if none of these can reach it)
SITE cmd/gc/api_state_test.go:582  func=TestControllerStateRuntimeUpdateAcceptsBuiltinAwareRevision kind=test lines=581-619  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/api_state_test.go:622  func=TestControllerStateMutationRefreshKeepsBuiltinOrdersAndClearsPending kind=test lines=621-661  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/bd_env_test.go:1396  func=TestBdRuntimeEnvUsesValidProviderStateWhenPublishedStateIsMissing kind=test lines=1364-1448  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/bd_env_test.go:1643  func=TestResolvedRuntimeCityDoltTargetDoesNotMaskInvalidCanonicalConfigWithProviderState kind=test lines=1621-1662  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/beads_provider_dirty_tables_test.go:345  func=TestManagedDoltDirtyTablesWithPasswordAgainstRealServer kind=test lines=338-379  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/beads_provider_lifecycle_test.go:10309  func=TestGcBeadsBdStartDoesNotReplaceLiveLockFileInode kind=test lines=10308-10492  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/beads_provider_lifecycle_test.go:10495  func=TestGcBeadsBdStartWaitsForConcurrentStarterSuccess kind=test lines=10494-10775  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/beads_provider_lifecycle_test.go:10778  func=TestGcBeadsBdStartWaitsForSlowConcurrentStarterSuccess kind=test lines=10777-11054  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/beads_provider_lifecycle_test.go:11057  func=TestGcBeadsBdStartConcurrentWaitPassesRemainingExistingManagedBudget kind=test lines=11056-11245  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/beads_provider_lifecycle_test.go:11537  func=TestManagedDoltConfigGoWriterMatchesShellFallbackSemantics kind=test lines=11536-11631  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/beads_provider_lifecycle_test.go:11681  func=TestGcBeadsBdStartIsIdempotentWhenAlreadyRunning kind=test lines=11680-11765  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/beads_provider_lifecycle_test.go:11768  func=TestGcBeadsBdStartRestartsServerHoldingDeletedDataInodes kind=test lines=11767-11919  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/beads_provider_lifecycle_test.go:11933  func=TestGcBeadsBdStartLeavesForeignPortHolderAlone kind=test lines=11928-12059  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/beads_provider_lifecycle_test.go:12979  func=TestGcBeadsBdInitNormalizesScopeAndRemovesLocalServerArtifacts kind=test lines=12978-13159  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/beads_provider_lifecycle_test.go:14191  func=TestGcBeadsBdProviderOwnedRealLifecycleStopsOwnedProcesses kind=test lines=14190-14438  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/beads_provider_lifecycle_test.go:1724  func=TestEnsureBeadsProvider_bdAcceptsHealthyServerAfterStartError kind=test lines=1718-1772  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/beads_provider_lifecycle_test.go:5145  func=TestGcBeadsBdStartUsesRootBeadsDataDir kind=test lines=5144-5223  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/beads_provider_lifecycle_test.go:5696  func=TestHealthBeadsProviderWaitsForStorePingAfterRecovery kind=test lines=5682-5793  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/beads_provider_lifecycle_test.go:6678  func=TestGcBeadsBdInitEnsuresProjectIdentityWhenMetadataExistsWithoutProjectID kind=test lines=6677-6809  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/beads_provider_lifecycle_test.go:9694  func=TestGcBeadsBdStartIgnoresReachableCompatPortFileInput kind=test lines=9693-9825  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/beads_provider_script_scope_lifecycle_linux_test.go:352  func=TestGcBeadsBdProviderOwnedRealInitIgnoresAncestorBeadsWorkspace kind=test lines=351-427  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/build_desired_state_test.go:10104  func=TestBuildDesiredState_PoolCheckUsesManagedCityDoltPortWhenRigHasNoOverride kind=test lines=10103-10171  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/build_desired_state_test.go:6331  func=TestBuildDesiredState_MinZeroDefaultScaleCheckRoutedWorkCreatesPoolSession kind=test lines=6330-6395  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/city_runtime_test.go:4327  func=TestCityRuntimeTick_RefreshesManualSessionOverlayAfterSync kind=test lines=4326-4441  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/city_status_snapshot_test.go:200  func=TestLoadStatusSessionSnapshotKillsBdChildOnTimeout kind=test lines=199-238  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cityinit_impl_test.go:259  func=TestCityInitServiceInitScaffoldsAndFinalizes kind=test lines=258-288  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_bd_test.go:1314  func=TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore kind=test lines=1305-1475  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_bd_test.go:1478  func=TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix kind=test lines=1477-1512  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_bd_test.go:1851  func=TestGcBdUsesEnclosingRigWhenNoFlag kind=test lines=1850-1940  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_beads_city_test.go:188  func=TestDoBeadsCityUseExternalUpdatesIncludedInheritedRigs kind=test lines=187-263  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_beads_city_test.go:485  func=TestDoBeadsCityUseExternalRewritesCompatRigWithRelativePath kind=test lines=484-540  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_beads_city_test.go:677  func=TestDoBeadsCityUseExternalAdoptUnverifiedSkipsValidation kind=test lines=676-723  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_beads_city_test.go:90  func=TestDoBeadsCityUseExternalWritesVerifiedCityAndInheritedRigs kind=test lines=89-185  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_commands_test.go:814  func=TestE1PreLeafBooleanHelpNoScopeEager kind=test lines=813-844  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_convoy_dispatch_test.go:3899  func=TestRunWorkflowServeReturnsControlErrorWithoutQuarantine kind=test lines=3898-3956  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_doctor_test.go:479  func=TestDoDoctorRegistersStaleLocalPackDirCheck kind=test lines=478-511  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_doctor_test.go:599  func=TestDoDoctorRegistersStaleLocalPackDirCheckForDefaultRigRemoteImport kind=test lines=598-641  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:1023  func=TestDoltStateAllocatePortCmdRepairsStaleWrongPortProviderStateFromPublishedHint kind=test lines=1000-1064  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:1184  func=TestDoltStateInspectManagedCmdUsesPIDFileAndStateOwnership kind=test lines=1177-1223  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:1307  func=TestDoltStateInspectManagedCmdReportsPortHolderOwnership kind=test lines=1299-1334  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:1347  func=TestDoltStateProbeManagedCmdReportsRunningOwnedHolder kind=test lines=1336-1383  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:1407  func=TestDoltStateProbeManagedCmdKeepsOwnedHolderRunningWhenDialFailsLocally kind=test lines=1388-1439  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:142  func=TestValidDoltRuntimeStateAcceptsSymlinkEquivalentDataDir kind=test lines=134-179  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:1445  func=TestDoltStateProbeManagedCmdReportsImposterHolder kind=test lines=1441-1472  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:1535  func=TestDoltStateExistingManagedCmdRejectsForeignListenerBackedOnlyByStateDataDir kind=test lines=1527-1571  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:1598  func=TestDoltStateExistingManagedCmdReportsReusableOwnedServer kind=test lines=1573-1637  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:1661  func=TestDoltStateExistingManagedCmdKeepsOwnedServerWhenDialsFailLocally kind=test lines=1642-1693  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:1720  func=TestDoltStateExistingManagedCmdFallsBackToPublishedRuntimeState kind=test lines=1695-1760  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:1888  func=TestDoltStatePreflightCleanCmdPreservesLiveArtifacts kind=test lines=1887-1929  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:189  func=TestRepairedManagedDoltRuntimeStateAcceptsSymlinkEquivalentDataDir kind=test lines=181-235  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:2780  func=TestDoltStateWaitReadyCmdReturnsReady kind=test lines=2759-2809  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:2888  func=TestDoltStateStopManagedCmdStopsManagedPID kind=test lines=2877-2948  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:3010  func=TestDoltStateStopManagedCmdDoesNotKillImposterPortHolder kind=test lines=3000-3048  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:3051  func=TestDoltStateRecoverManagedCmdReportsReadOnlyAndRestarts kind=test lines=3050-3200  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:3216  func=TestRecoverManagedDoltProcessReturnsWhenConcurrentStarterBecomesReady kind=test lines=3202-3293  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:3313  func=TestRecoverManagedDoltProcessReusesHealthyManagedServerOnReboundPort kind=test lines=3295-3412  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:3436  func=TestRecoverManagedDoltProcessKeepsUnmeasuredServerWithoutReportingItHealthy kind=test lines=3418-3490  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:3511  func=TestRecoverManagedDoltProcessWaitsForLockHolderWhenServerIsUnmeasured kind=test lines=3492-3544  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:398  func=TestChooseManagedDoltPortKeepsOwnServerPortWhenDialsFail kind=test lines=388-432  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:447  func=TestChooseManagedDoltPortKeepsOwnServerPortBeforeItListens kind=test lines=437-470  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:485  func=TestChooseManagedDoltPortKeepsPublishedHintPortBeforeItsServerListens kind=test lines=475-511  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:526  func=TestChooseManagedDoltPortSkipsStatePortHeldByAnotherProcess kind=test lines=516-550  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:560  func=TestDoltStateAllocatePortCmdReusesLiveProviderState kind=test lines=552-579  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:582  func=TestStartTCPListenerProcessInDirRegistersCleanup kind=test lines=581-605  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:694  func=TestValidDoltRuntimeStateRejectsAlivePIDThatDoesNotOwnManagedDolt kind=test lines=683-709  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:722  func=TestValidDoltRuntimeStateClassifiesDialFailure kind=test lines=711-751  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:760  func=TestRepairedManagedDoltRuntimeStateClassifiesDialFailure kind=test lines=753-787  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:801  func=TestDoltStateAllocatePortCmdRepairsStaleProviderStateFromOwnedLivePortHolder kind=test lines=789-839  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:853  func=TestDoltStateAllocatePortCmdRepairsStoppedProviderStateFromOwnedLivePortHolder kind=test lines=841-891  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:905  func=TestDoltStateAllocatePortCmdRepairsMissingProviderStateFromPublishedHint kind=test lines=893-947  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_dolt_state_test.go:960  func=TestDoltStateAllocatePortCmdRepairsMissingCanonicalProviderStateFromPublishedHint kind=test lines=949-998  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_mail_test.go:1849  func=TestCmdMailInbox_NormalizesCanonicalManagedProviderEnvAndReadsInbox kind=test lines=1842-1939  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_pack_registry_test.go:758  func=TestPackRegistryLiveGascityPacksCatalog kind=test lines=755-799  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_prime_test.go:1269  func=TestDoPrimeWithHook_DeliveredStartupPromptCodexJSONHookFormat kind=test lines=1268-1333  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_prime_test.go:1336  func=TestDoPrimeWithHook_CodexJSONFormatInfersAgentFromWorkDir kind=test lines=1335-1447  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_reload_test.go:540  func=TestSendReloadControlRequestNoChange kind=test lines=539-603  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_reload_test.go:606  func=TestReloadConfigTracedRescansOrdersWhenConfigRevisionUnchanged kind=test lines=605-707  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_reload_test.go:740  func=TestSendReloadControlRequestInvalidConfig kind=test lines=739-833  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_rig_endpoint_test.go:1651  func=TestVerifyExternalDoltEndpointRejectsProjectIdentityMismatch kind=test lines=1650-1686  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_rig_endpoint_test.go:1689  func=TestVerifyExternalDoltEndpointRejectsMissingLocalProjectID kind=test lines=1688-1717  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_session_logs_test.go:388  func=TestResolveStoredSessionLogSource_UniqueWorkDirFallsBackBeyondLatestAlias kind=test lines=387-422  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_stop_test.go:1376  func=TestStopCityManagedBeadsProviderAfterSuccessfulStopStopsDefaultBD kind=test lines=1375-1428  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_supervisor_city_test.go:350  func=TestRegisterCityWithSupervisorKeepsRegistrationWhenReloadFails kind=test lines=349-396  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_supervisor_test.go:1473  func=TestSupervisorInstallUnsupportedOS kind=test lines=1471-1482  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_supervisor_test.go:1691  func=TestInstallSupervisorSystemdWarmRefreshStopsWorkspaceServicesBeforeStart kind=test lines=1690-1812  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_supervisor_test.go:1851  func=TestInstallSupervisorSystemdWarmRefreshLeavesUnregisteredWorkspaceServices kind=test lines=1814-1888  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_supervisor_test.go:2527  func=TestUnloadSupervisorServiceDarwinDisablesBootsOutAndVerifiesAbsent kind=test lines=2525-2573  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_supervisor_test.go:2577  func=TestUnloadSupervisorServiceDarwinWaitsThroughSIGTERMedUntilAbsent kind=test lines=2575-2638  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_supervisor_test.go:2642  func=TestUnloadSupervisorServiceDarwinFailsWhenTargetStillLoaded kind=test lines=2640-2687  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_supervisor_test.go:2695  func=TestVerifySupervisorServiceStoppedDarwinFailsWhenAbsenceUnconfirmed kind=test lines=2693-2733  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_supervisor_test.go:2742  func=TestVerifySupervisorServiceStoppedDarwinHonorsShorterCallerDeadline kind=test lines=2740-2781  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_supervisor_test.go:2819  func=TestUnloadSupervisorServiceDarwinReturnsLaunchctlFailures kind=test lines=2817-2865  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_supervisor_test.go:6604  func=TestStopSupervisorWithWaitFailsWhenLaunchdStopFails kind=test lines=6602-6666  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/cmd_wait_test.go:777  func=TestBuildPinnedBDBinaryForTestsUsesGoModSource kind=test lines=772-827  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/controller_test.go:1746  func=TestControllerReloadInvalidConfig kind=test lines=1745-1805  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/controller_test.go:1808  func=TestControllerReloadCityNameChange kind=test lines=1807-1860  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/controller_watcher_fd_test.go:27  func=TestWatchConfigTargets_CloseReturnsFDsToBaseline kind=test lines=25-80  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/dolt_identity_test.go:156  func=TestManagedDoltServingDataDirWithPasswordAgainstRealServer kind=test lines=149-169  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/dolt_lifecycle_race_test.go:148  func=TestTerminateManagedDoltPIDRefusesSIGKILLWhileLockHeld kind=test lines=147-167  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/dolt_lifecycle_race_test.go:170  func=TestStopManagedDoltRefusesSIGKILLWhileLockHeld kind=test lines=169-208  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/dolt_lifecycle_race_test.go:238  func=TestStopManagedDoltWaitsForLockReleaseAfterExit kind=test lines=237-272  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/dolt_project_id_test.go:184  func=TestManagedDoltHealthCheckWithPasswordUsesDirectHelpersAgainstRealServer kind=test lines=171-197  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/dolt_project_id_test.go:212  func=TestManagedDoltWaitReadyWithPasswordUsesDirectQueryProbe kind=test lines=199-222  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/dolt_project_id_test.go:225  func=TestRecoverManagedDoltProcessWithPasswordReusesHealthyRealServer kind=test lines=224-278  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/dolt_project_id_test.go:281  func=TestProjectIdentityL3AdapterContractAndManagedComposition kind=test lines=280-353  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/dolt_runtime_publication_test.go:130  func=TestPublishManagedDoltRuntimeStateRecoversMissingProviderStateWithPortHint kind=test lines=119-180  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/dolt_runtime_publication_test.go:204  func=TestPublishManagedDoltRuntimeStateRecoversStaleWrongPortProviderStateWithPublishedHint kind=test lines=182-245  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/dolt_runtime_publication_test.go:28  func=TestPublishManagedDoltRuntimeStateRepairsStaleProviderState kind=test lines=17-73  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/dolt_runtime_publication_test.go:294  func=TestPublishManagedDoltRuntimeStateSucceedsWhenAlreadyValid kind=test lines=283-328  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/dolt_scope_watchdog_test.go:177  func=TestManagedDoltScopeWatchdogHelper kind=test lines=175-216  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/dolt_sql_health_test.go:525  func=TestRunManagedDoltSQLEmptyPasswordMasksDefaultProfileAgainstRealServer kind=test lines=524-546  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/dolt_start_managed_test.go:1635  func=TestTerminateManagedDoltTestPIDKillsProcessGroup kind=test lines=1631-1685  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/dolt_start_managed_test.go:647  func=TestManagedDoltWatchdogExternalParentHelper kind=test lines=645-681  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/gc_beads_bd_config_commit_test.go:457  func=TestEnsureBdRuntimeCustomTypesHealsUpgradedNativeStore kind=test lines=450-598  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/main_test.go:379  func=TestTutorial01 kind=test lines=378-381  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/main_test.go:840  func=TestResolveCityFlag kind=test lines=805-1003  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/order_dispatch_test.go:3695  func=TestShellExecRunnerKillsProcessGroupOnTimeout kind=test lines=3694-3715  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/order_dispatch_test.go:3718  func=TestShellExecRunnerKillsProcessGroupAfterWaitDelay kind=test lines=3717-3743  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/path_helpers_test.go:1055  func=TestRegisterRealBDServerStopStopsServerBeforeTempDirCleanup kind=test lines=1054-1087  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/pool_test.go:108  func=TestEvaluatePoolDefaultScaleCheckCountsRoutedReadyWork kind=test lines=107-151  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/pool_test.go:154  func=TestEvaluatePoolDefaultScaleCheckIgnoresRoutedActiveUnassignedWork kind=test lines=153-201  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/pool_test.go:215  func=TestCmdGCRealBDTestsUseTestOwnedDoltContext kind=test lines=214-246  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/prompt_test.go:1420  func=TestRenderedCrewPromptShowsProviderSpecificInstructionsFile kind=test lines=1406-1451  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/providers_events_mirror_test.go:220  func=TestEventsMirrorInheritsSetgidGroup kind=test lines=219-249  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/reconcile_replay_test.go:574  func=TestReplay kind=test lines=571-583  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/reconcile_sim_coverage_test.go:305  func=TestSimCoverageGate kind=test lines=301-334  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/rig_anywhere_test.go:378  func=TestRigAnywhere_ResolveContext kind=test lines=157-568  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/rig_anywhere_test.go:402  func=TestRigAnywhere_ResolveContext kind=test lines=157-568  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/rig_anywhere_test.go:489  func=TestRigAnywhere_ResolveContext kind=test lines=157-568  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/rig_import_fixtures_test.go:460  func=TestRigImportFromARealBdRigRaisesItsMinHashLength kind=test lines=456-506  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/rig_import_review_test.go:84  func=TestReviewBdRigWithEdgesRefusesBeforeAnyWrite kind=test lines=80-108  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/scoped_store_test.go:364  func=TestScopedBdStoreForCityKillsChildOnCtxCancel kind=test lines=363-405  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/script_resolve_test.go:313  func=TestPrepareCityForSupervisorPrunesLegacyScripts kind=test lines=312-359  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/session_event_pump_live_test.go:23  func=TestSessionEventPumpLiveHerdr kind=test lines=22-102  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/split_strict_gate_test.go:175  func=TestSplitStrictControllerSharesOneProjection kind=test lines=173-204  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/tmux_leak_guard_mntns_test.go:76  func=TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace kind=test lines=71-87  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/tmux_leak_guard_mntns_test.go:98  func=TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace kind=test lines=93-107  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/v2_g1_test.go:120  func=TestV2G1Budget kind=test lines=118-153  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/v2_g1_test.go:178  func=TestV2GatherProductionShape kind=test lines=175-214  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/worker_handle_test.go:1266  func=TestWorkerHandleForSessionWithConfigUsesResolvedProviderOnResume kind=test lines=1265-1331  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/worker_handle_test.go:36  func=TestWorkerHandleForSessionWithConfigUsesResolvedProviderOnFirstStart kind=test lines=35-109  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE examples/gastown/gastown_test.go:4153  func=TestAttachedRigScopeShellToken kind=test lines=4148-4175  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE examples/gastown/gastown_test.go:492  func=TestTmuxKeybindingsAlternateScreenPassthrough kind=test lines=484-499  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE examples/gastown/maintenance_scripts_test.go:9330  func=TestJsonlExportPushRetryDelayIsLocaleIndependent kind=test lines=9322-9377  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/api/handler_status_scoped_store_test.go:141  func=TestStatusSessionSnapshotKillsBdChildOnTimeout kind=test lines=140-180  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/api/handler_status_scoped_store_test.go:507  func=TestStatusListStoreWithTimeoutKillsBdChildOnTimeout kind=test lines=506-537  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/api/handler_status_scoped_store_test.go:544  func=TestStatusReadyStoreWithTimeoutKillsBdChildOnTimeout kind=test lines=543-574  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/bdstore_conditional_integration_test.go:69  func=TestBdStoreConditionalWriterConformance kind=test lines=40-148  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/bdstore_exec_internal_test.go:266  func=TestKillCommandTreeKillsProcessGroup kind=test lines=265-307  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/beadstest/close_notification_conformance.go:54  func=RunCloseNotificationTests kind=helper lines=26-133  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/beadstest/close_notification_conformance.go:57  func=RunCloseNotificationTests kind=helper lines=26-133  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/beadstest/close_notification_conformance.go:89  func=RunCloseNotificationTests kind=helper lines=26-133  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/beadstest/conformance.go:508  func=RunStoreTestsWithOptions kind=helper lines=41-1332  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/beadstest/conformance.go:525  func=RunStoreTestsWithOptions kind=helper lines=41-1332  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/beadstest/conformance.go:551  func=RunStoreTestsWithOptions kind=helper lines=41-1332  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/beadstest/conformance_skips_test.go:60  func=TestRequireLedgeredSkipIsDateFree kind=test lines=47-65  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/beadstest/pinned_id_fence_conformance.go:467  func=RunPinnedIDFenceConformance kind=helper lines=44-553  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/beadstest/pinned_id_fence_conformance.go:533  func=RunPinnedIDFenceConformance kind=helper lines=44-553  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/beadstest/ready_parity_conformance.go:69  func=RunReadyParityConformanceWithOptions kind=helper lines=65-87  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/binary_versions_test.go:66  func=TestProbeVersionsAgainstHostBinaries kind=test lines=55-73  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/contract/migrate_journal_test.go:37  func=TestMigrateJournalFileMatchesPinnedBeads kind=test lines=32-53  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/exec/br_test.go:18  func=TestBrProviderConformance kind=test lines=15-30  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/exec_timeout_linux_test.go:66  func=TestExecTimeoutHardExitHarness kind=test lines=64-83  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/metadata_empty_clear_conformance_test.go:55  func=TestMetadataEmptyStringClearContract kind=test lines=32-112  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/native_dolt_store_test.go:651  func=TestNativeDoltStoreNormalizesRealUpstreamMissingIssueErrors kind=test lines=647-689  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/config/implicit_agent_patch_repro_test.go:68  func=TestLoadWithIncludes_PatchTargetingImplicitAgentErrorsToday kind=test lines=50-73  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/connect/tokenrender/write_test.go:376  func=TestWriteTokenFileKeepsGroup kind=test lines=360-398  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/connect/tokenrender/write_test.go:475  func=TestWriteTokenFileKeepsOwnerAsRoot kind=test lines=473-492  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/convergence/condition_test.go:594  func=TestResolveConditionPath kind=test lines=218-617  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/convergence/evaluate_test.go:189  func=TestResolveEvaluateStep_DarwinPrivateTempAliasWithExistingPrompt kind=test lines=187-210  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/dispatch/call_budget_test.go:536  func=TestCrashInjectionCallBudgetMolecule kind=test lines=520-541  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/doctor/checks_custom_types_test.go:297  func=TestCustomTypesCheck_TableDrift kind=test lines=295-384  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/doctor/checks_custom_types_test.go:399  func=TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext kind=test lines=397-457  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/doctor/checks_custom_types_test.go:467  func=TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint kind=test lines=465-610  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/events/eventstest/conformance.go:508  func=RunProviderTests kind=helper lines=55-715  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/events/eventstest/conformance.go:525  func=RunProviderTests kind=helper lines=55-715  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/events/eventstest/conformance.go:551  func=RunProviderTests kind=helper lines=55-715  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/formula/compile_test.go:1359  func=TestCompileBugReportFlowV2 kind=test lines=1352-1409  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/fsys/atomic_test.go:185  func=TestWriteFileIfContentOrModeChangedAtomic_RepairsSpecialModeBits kind=test lines=169-202  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/fsys/fsystest/conformance.go:508  func=(none: outside every function)  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/fsys/fsystest/conformance.go:525  func=(none: outside every function)  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/fsys/fsystest/conformance.go:551  func=(none: outside every function)  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/gitcred/rules_test.go:186  func=TestLoadAcceptsRootOwnedGroupReadable kind=test lines=178-207  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/mail/exec/mcp_conformance_test.go:159  func=TestMCPMailBridgeSourceable kind=test lines=153-179  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/mail/exec/mcp_conformance_test.go:195  func=TestMCPMailCrossPodNameResolution kind=test lines=189-273  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/mail/exec/mcp_conformance_test.go:26  func=TestMCPMailConformance kind=test lines=18-60  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/mail/exec/mcp_conformance_test.go:286  func=TestMCPMailProjectKeyIsolation kind=test lines=280-356  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/mail/exec/mcp_conformance_test.go:80  func=TestMCPMailReplyMissingOriginalIsNotFound kind=test lines=74-133  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/mail/exec/mcp_live_test.go:35  func=TestMCPMailConformanceLive kind=test lines=33-78  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/mail/mailtest/conformance.go:508  func=RunProviderTests kind=helper lines=16-806  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/mail/mailtest/conformance.go:525  func=RunProviderTests kind=helper lines=16-806  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/mail/mailtest/conformance.go:551  func=RunProviderTests kind=helper lines=16-806  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/molecule/molecule_test.go:30  func=TestBuildRecipeApplyPlanBugReportFlowV2 kind=test lines=25-79  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/orders/triggers_test.go:464  func=TestCheckTriggerConditionKillsProcessGroupOnTimeout kind=test lines=463-492  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/orders/triggers_test.go:495  func=TestCheckTriggerConditionKillsProcessGroupAfterWaitDelay kind=test lines=494-528  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/pidutil/cmdline_portable_test.go:141  func=TestCmdline_FailsClosedWhenUnreadable kind=test lines=139-150  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/pidutil/starttime_portable_test.go:86  func=TestAliveWithStartTime_UnreadableIdentityKeepsAliveAnswer kind=test lines=84-97  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/processgroup/processgroup_unix_test.go:71  func=TestTerminateCommandPreservesGroupFailureAfterDirectKill kind=test lines=70-92  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runproj/golden_regen_test.go:15  func=TestRegenerateGoldens kind=test lines=13-38  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/herdr/activity_live_test.go:20  func=TestActivityLive kind=test lines=19-152  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/herdr/conformance_test.go:18  func=TestHerdrConformance kind=test lines=17-27  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/herdr/events_live_test.go:40  func=TestSessionEventsLive kind=test lines=39-154  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/herdr/kindpath_live_test.go:105  func=TestProviderLiveClaudeKindPath kind=test lines=104-155  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/herdr/panebinding_live_test.go:21  func=TestProviderLiveOccupantSwapKeepsLiveness kind=test lines=20-72  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/herdr/provider_live_test.go:15  func=TestProviderLive kind=test lines=14-90  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:124  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:134  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:170  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:193  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:209  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:238  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:271  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:294  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:313  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:333  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:349  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:367  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:395  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:404  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:421  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:508  func=runFencedInputTests kind=helper lines=503-561  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:525  func=runFencedInputTests kind=helper lines=503-561  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:551  func=runFencedInputTests kind=helper lines=503-561  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:61  func=RunProviderTestsWithOptions kind=helper lines=54-64  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:84  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:93  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance_test.go:147  func=TestStartOrSkipDoesNotStopSessionAfterClassifiedStartFailure kind=test lines=137-164  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/ssh/ssh_localhost_integration_test.go:24  func=TestConn_ExecOverRealLocalhost kind=test lines=17-37  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/t3bridge/delivery_lookup_test.go:133  func=TestLookupDeliveryErrors kind=test lines=109-166  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/tmux/activity_poke_integration_test.go:21  func=TestPokeActivityRealTmux kind=test lines=19-103  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/tmux/nudge_confirm_lag_integration_test.go:174  func=TestNudgeConfirmBudgetThreshold kind=test lines=172-230  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/tmux/nudge_confirm_lag_integration_test.go:36  func=TestNudgeConfirmBusyRenderLag kind=test lines=34-146  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/tmux/nudge_poke_integration_test.go:27  func=TestNudgePokeRealTmux kind=test lines=25-235  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/tmux/nudge_staged_draft_integration_test.go:26  func=TestNudgeRecoversStagedDraftSwallowedByPasteIngest kind=test lines=24-130  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/tmux/state_cache_test.go:926  func=TestProcessSnapshotPSArgsRejectsLinuxSyntaxOnDarwin kind=test lines=921-934  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/tmux/tmux_test.go:1615  func=TestCleanupOrphanedSessions kind=test lines=1610-1707  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/tmux/tmux_test.go:1713  func=TestCleanupOrphanedSessions_NoSessions kind=test lines=1709-1743  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/tmux/tmux_test.go:3637  func=TestGetKeyBinding_SkipsGasTownBindings kind=test lines=3632-3656  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/tmux/tmux_test.go:3663  func=TestGetKeyBinding_CapturesUserBinding kind=test lines=3658-3682  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/tmux/tmux_test.go:3689  func=TestIsGTBinding_DetectsGasTownBindings kind=test lines=3684-3712  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/tmux/tmux_test.go:3719  func=TestSetBindings_PreserveFallbackOnRepeatedCalls kind=test lines=3714-3750  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/sessionlog/sessionlog_test.go:1084  func=TestFindSessionFileByIDUsesClaudeProjectPathAlias kind=test lines=1083-1101  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/sessionlog/sessionlog_test.go:1104  func=TestFindSessionFileByIDPrefersStoredWorkDirSpelling kind=test lines=1103-1134  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/sessionlog/sessionlog_test.go:1195  func=TestFindSessionFileUsesClaudeProjectPathAlias kind=test lines=1194-1212  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/sessionlog/sessionlog_test.go:1215  func=TestFindSessionFileUsesNewestClaudeProjectPathAliasMatch kind=test lines=1214-1245  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/sessionlog/sessionlog_test.go:1397  func=TestFindClaudeLatestSessionFileUsesProjectPathAlias kind=test lines=1396-1414  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/sessionlog/sessionlog_test.go:2170  func=TestFindCodexSessionFileMatchesEquivalentResolvedWorkDir kind=test lines=2168-2189  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/sessionlog/sessionlog_test.go:2315  func=TestFindGeminiSessionFileMatchesEquivalentResolvedWorkDir kind=test lines=2313-2339  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/worker/transcript/discovery_test.go:65  func=TestDiscoverFallbackPathUsesNewestClaudeLatestSessionAcrossAliases kind=test lines=63-97  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/worker/workertest/phase1_conformance_test.go:100  func=TestPhase1Conformance kind=test lines=69-125  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/worker/workertest/phase1_conformance_test.go:110  func=TestPhase1Conformance kind=test lines=69-125  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/worker/workertest/structured_conformance_test.go:153  func=TestStructuredConformance kind=test lines=135-160  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/workspacesvc/proxy_process_test.go:1160  func=TestProxyProcessHardExitHarness kind=test lines=1158-1206  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/workspacesvc/proxy_process_test.go:167  func=TestProxyProcessHelper kind=test lines=165-214  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/acceptance/beads_migrate_proxied_test.go:135  func=TestBeadsMigrateLegacyCityToProxied kind=test lines=133-348  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/acceptance/beads_migrate_proxied_test.go:357  func=TestBeadsMigrateProxiedRefusesLiveLegacyServer kind=test lines=355-408  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/acceptance/helpers/bd_schema_parity_test.go:121  func=TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig kind=test lines=120-137  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/acceptance/helpers/bd_schema_parity_test.go:144  func=TestBdLatestSchemaVersionLeavesNothingInItsTempRoot kind=test lines=143-172  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/acceptance/helpers/beads_topology.go:637  func=ForEachTopology kind=helper lines=632-641  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/acceptance/helpers/tool_home_test.go:317  func=TestRealBdProvisionLeavesTheHostHomeAlone kind=test lines=314-349  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/acceptance/integration_selfhost_ux_test.go:100  func=TestSelfhostUX_StructuredStartSummaryPlaceholder kind=test lines=99-101  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/acceptance/integration_selfhost_ux_test.go:104  func=TestSelfhostUX_StopBypassDetailsPlaceholder kind=test lines=103-105  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/acceptance/integration_selfhost_ux_test.go:108  func=TestSelfhostUX_MigrationGuideURLPlaceholder kind=test lines=107-109  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/acceptance/integration_selfhost_ux_test.go:112  func=TestSelfhostUX_BinaryDriftPostMergePlaceholder kind=test lines=111-113  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/acceptance/integration_selfhost_ux_test.go:16  func=TestSelfhostUX_PackV1V2Collision kind=test lines=15-39  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/acceptance/integration_selfhost_ux_test.go:54  func=TestSelfhostUX_BinaryDriftRestart kind=test lines=53-82  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/acceptance/integration_selfhost_ux_test.go:96  func=TestSelfhostUX_Composed kind=test lines=95-97  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/acceptance/managed_session_dolt_port_move_test.go:45  func=TestManagedDirectSessionBdFollowsMovedDoltPort kind=test lines=34-120  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/acceptance/pack_registry_live_test.go:38  func=TestPackRegistryLiveImportsEveryCatalogPack kind=test lines=35-167  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/conversationlane/x6_test.go:286  func=TestX6FanIn kind=test lines=251-298  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/integration/bdstore_test.go:54  func=TestBdStoreConformance kind=test lines=45-96  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/integration/dolt_persistence_test.go:449  func=TestDoltPersistence_CloseStatusSurvivesSubsequentBdWrite kind=test lines=448-484  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/integration/dolt_persistence_test.go:490  func=TestDoltPersistence_MetadataSurvivesSubsequentBdWrites kind=test lines=489-529  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/integration/dolt_persistence_test.go:536  func=TestDoltPersistence_AssigneeAndInProgressSurviveSubsequentBdWrite kind=test lines=535-578  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/integration/dolt_persistence_test.go:585  func=TestDoltPersistence_SessionAwakeMetadataSurvivesSubsequentBdWrite kind=test lines=584-630  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/integration/dolt_persistence_test.go:636  func=TestDoltPersistence_HandoffRoutingMetadataSurvivesSubsequentBdWrite kind=test lines=635-673  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/integration/dolt_persistence_test.go:679  func=TestDoltPersistence_InProgressStatusSurvivesSubsequentBdWrite kind=test lines=678-711  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/integration/dolt_persistence_test.go:722  func=TestDoltPersistence_SequentialMetadataWritesAccumulateInDolt kind=test lines=721-759  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/integration/dolt_persistence_test.go:766  func=TestDoltPersistence_CrossBeadMetadataWritesPersistInDolt kind=test lines=765-809  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/integration/dolt_persistence_test.go:818  func=TestDoltPersistence_ConcurrentMetadataWriteBurstTimingVisibility kind=test lines=817-878  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/integration/huma_binary_test.go:508  func=TestHumaBinary_CityUnregisterAsync kind=test lines=498-739  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/integration/integration_test.go:2841  func=TestReadProcessSnapshotFallsBackToPSAndFindsRealProcesses kind=test lines=2839-2854  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/integration/start_drift_test.go:123  func=TestStartDrift_SystemdManaged_RestartsToNewBuildID kind=test lines=122-156  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/integration/start_drift_test.go:275  func=TestStartDrift_PermissionDenied_DescriptiveError kind=test lines=273-292  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE test/tmuxtest/orphan_sweep_sigkill_test.go:29  func=TestHelperHoldSentinel kind=test lines=27-47  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
```

## Native skip sites

```text
diff-owned-tests evidence (gm-vlkzni)  base=adf99aa6c84d head=0d781d66b8fa
rule: OWNED = the diff adds the test, or a hunk lands inside its own function
FILE cmd/gc/session_lifecycle_start_boundary_test.go
  hunks (new side): +413,199
  hunk +413,199  touches method Start (lines 421-421)  [shared: tests that call it are affected but not owned]
  hunk +413,199  touches TestExecutePreparedStartWaveDefersUnfinishedOrphanScan (lines 429-526)
  hunk +413,199  touches method Start (lines 535-537)  [shared: tests that call it are affected but not owned]
  hunk +413,199  touches TestDefersUnfinishedOrphanScanWhenStartDeadlineEndsIt (lines 539-611)
  TestExecutePreparedStartWaveUsesWorkerBoundaryForKnownSession lines 54-104  not owned  (no hunk touches lines 54-104)
  TestStartPreparedStartCandidateUsesWorkerBoundaryForRuntimeOnlyTarget lines 106-157  not owned  (no hunk touches lines 106-157)
  TestStartPreparedStartCandidateRefusesRigSuspendedBeforeSpawn lines 159-180  not owned  (no hunk touches lines 159-180)
  TestStartPreparedStartCandidateDoesNotKillSessionSuspendedDuringSpawn lines 182-213  not owned  (no hunk touches lines 182-213)
  TestStartPreparedStartCandidateDefersWhenLivenessUnavailable lines 215-248  not owned  (no hunk touches lines 215-248)
  TestStartPreparedStartCandidateConvergesFromConfirmedAbsence lines 250-271  not owned  (no hunk touches lines 250-271)
  TestExecutePreparedStartWaveDefersUnavailableWithoutRollbackOrWakeFailure lines 273-349  not owned  (no hunk touches lines 273-349)
  TestStartPreparedStartCandidateRuntimeOnlyBorrowsTheCandidateLease lines 356-393  not owned  (no hunk touches lines 356-393)
  TestStartLeaseNameIsTheStartsName        lines 398-412  not owned  (no hunk touches lines 398-412)
  TestExecutePreparedStartWaveDefersUnfinishedOrphanScan lines 429-526  OWNED      (hunk +413,199 touches the function)
  TestDefersUnfinishedOrphanScanWhenStartDeadlineEndsIt lines 539-611  OWNED      (hunk +413,199 touches the function)
SHARED-HUNKS cmd/gc/session_lifecycle_start_boundary_test.go: 1  (a helper, TestMain/init or file-level code changed; a not-owned SKIP in this file is pre-existing ONLY if none of these can reach it)
FILE internal/runtime/process_table_scan_bounded_test.go
  hunks (new side): +1,311
  hunk +1,311    touches helper newBlockedScanner (lines 30-32)  [shared: tests that call it are affected but not owned]
  hunk +1,311    touches method FindRuntimesBySessionID (lines 34-38)  [shared: tests that call it are affected but not owned]
  hunk +1,311    touches method TerminateRuntime (lines 40-40)  [shared: tests that call it are affected but not owned]
  hunk +1,311    touches method unblock (lines 45-45)  [shared: tests that call it are affected but not owned]
  hunk +1,311    touches method FindRuntimesBySessionID (lines 54-57)  [shared: tests that call it are affected but not owned]
  hunk +1,311    touches method TerminateRuntime (lines 59-59)  [shared: tests that call it are affected but not owned]
  hunk +1,311    touches TestFindRuntimesBySessionIDBoundedReturnsAnswerWithinBudget (lines 61-81)
  hunk +1,311    touches TestFindRuntimesBySessionIDBoundedExpiresAtBudget (lines 83-110)
  hunk +1,311    touches TestFindRuntimesBySessionIDBoundedCallerCancelIsUnavailable (lines 112-131)
  hunk +1,311    touches TestFindRuntimesBySessionIDBoundedSecondCallWhileOutstandingStartsNoScan (lines 133-157)
  hunk +1,311    touches TestFindRuntimesBySessionIDBoundedLatchIsPerKey (lines 159-176)
  hunk +1,311    touches TestFindRuntimesBySessionIDBoundedKeyFreeAfterScanReturns (lines 178-200)
  hunk +1,311    touches TestFindRuntimesBySessionIDBoundedScanFinishingByBudgetWins (lines 202-219)
  hunk +1,311    touches TestFindRuntimesBySessionIDBoundedLeavesNoGoroutineBehind (lines 221-232)
  hunk +1,311    touches TestProcessTableScanUnavailableWrapsRuntimeUnavailable (lines 234-238)
  hunk +1,311    touches method FindRuntimesBySessionID (lines 243-245)  [shared: tests that call it are affected but not owned]
  hunk +1,311    touches method TerminateRuntime (lines 247-247)  [shared: tests that call it are affected but not owned]
  hunk +1,311    touches TestBoundedScanPanicIsUnavailableAndReleasesKey (lines 249-266)
  hunk +1,311    touches TestBoundedScanPanicIsLoggedWithKeyValueAndStack (lines 271-311)
  TestFindRuntimesBySessionIDBoundedReturnsAnswerWithinBudget lines 61-81  OWNED      (hunk +1,311 touches the function)
  TestFindRuntimesBySessionIDBoundedExpiresAtBudget lines 83-110  OWNED      (hunk +1,311 touches the function)
  TestFindRuntimesBySessionIDBoundedCallerCancelIsUnavailable lines 112-131  OWNED      (hunk +1,311 touches the function)
  TestFindRuntimesBySessionIDBoundedSecondCallWhileOutstandingStartsNoScan lines 133-157  OWNED      (hunk +1,311 touches the function)
  TestFindRuntimesBySessionIDBoundedLatchIsPerKey lines 159-176  OWNED      (hunk +1,311 touches the function)
  TestFindRuntimesBySessionIDBoundedKeyFreeAfterScanReturns lines 178-200  OWNED      (hunk +1,311 touches the function)
  TestFindRuntimesBySessionIDBoundedScanFinishingByBudgetWins lines 202-219  OWNED      (hunk +1,311 touches the function)
  TestFindRuntimesBySessionIDBoundedLeavesNoGoroutineBehind lines 221-232  OWNED      (hunk +1,311 touches the function)
  TestProcessTableScanUnavailableWrapsRuntimeUnavailable lines 234-238  OWNED      (hunk +1,311 touches the function)
  TestBoundedScanPanicIsUnavailableAndReleasesKey lines 249-266  OWNED      (hunk +1,311 touches the function)
  TestBoundedScanPanicIsLoggedWithKeyValueAndStack lines 271-311  OWNED      (hunk +1,311 touches the function)
SHARED-HUNKS internal/runtime/process_table_scan_bounded_test.go: 1  (a helper, TestMain/init or file-level code changed; a not-owned SKIP in this file is pre-existing ONLY if none of these can reach it)
FILE internal/runtime/tmux/proctable_tracking_test.go
  hunks (new side): +98,42
  hunk +98,42    touches TestFindRuntimesBySessionIDWithNoRootsAsksTmuxNothing (lines 102-119)
  hunk +98,42    touches TestFindRuntimesBySessionIDWithNoRootsStillReturnsScanError (lines 121-139)
  TestFindRuntimesBySessionIDReportsTrackingFailure lines 46-75  not owned  (no hunk touches lines 46-75)
  TestTerminateRuntimeBindsKillToScannedIdentity lines 84-97  not owned  (no hunk touches lines 84-97)
  TestFindRuntimesBySessionIDWithNoRootsAsksTmuxNothing lines 102-119  OWNED      (hunk +98,42 touches the function)
  TestFindRuntimesBySessionIDWithNoRootsStillReturnsScanError lines 121-139  OWNED      (hunk +98,42 touches the function)
FILE internal/session/orphan_scan_bound_test.go
  hunks (new side): +1,449
  hunk +1,449    touches helper newWedgedScanProvider (lines 46-51)  [shared: tests that call it are affected but not owned]
  hunk +1,449    touches method unwedge (lines 56-56)  [shared: tests that call it are affected but not owned]
  hunk +1,449    touches method Start (lines 58-61)  [shared: tests that call it are affected but not owned]
  hunk +1,449    touches method FindRuntimesBySessionID (lines 63-76)  [shared: tests that call it are affected but not owned]
  hunk +1,449    touches method TerminateRuntime (lines 78-84)  [shared: tests that call it are affected but not owned]
  hunk +1,449    touches helper createOptsForOrphanScan (lines 86-89)  [shared: tests that call it are affected but not owned]
  hunk +1,449    touches helper seedSuspendedTargetWithProvider (lines 95-112)  [shared: tests that call it are affected but not owned]
  hunk +1,449    touches helper assertDeferredByUnfinishedScan (lines 118-135)  [shared: tests that call it are affected but not owned]
  hunk +1,449    touches TestManagerOrphanScanBudgetDefaultsToTenSeconds (lines 137-142)
  hunk +1,449    touches TestKillExistingOrphansDefersWhenScanNeverFinishes (lines 144-166)
  hunk +1,449    touches TestKillExistingOrphansBudgetIsCappedByContextDeadline (lines 168-190)
  hunk +1,449    touches TestKillExistingOrphansRepeatedStartsOfWedgedSessionScanOnce (lines 192-209)
  hunk +1,449    touches TestKillExistingOrphansKeysLatchByCityAndSession (lines 211-232)
  hunk +1,449    touches TestStartDefersWhenOrphanScanNeverFinishes (lines 234-250)
  hunk +1,449    touches TestStartRuntimeOnlyDefersWhenOrphanScanNeverFinishes (lines 252-269)
  hunk +1,449    touches TestCreateDefersAndRollsBackWhenOrphanScanNeverFinishes (lines 271-293)
  hunk +1,449    touches TestKillExistingOrphansStillTerminatesWhenScanReturns (lines 297-312)
  hunk +1,449    touches TestKillExistingOrphansStopsBetweenTerminationsWhenContextDone (lines 317-340)
  hunk +1,449    touches TestSameCityDifferentSessionHasItsOwnLatch (lines 342-363)
  hunk +1,449    touches TestOrphanScanBudgetFieldIsHonored (lines 365-384)
  hunk +1,449    touches method Start (lines 393-400)  [shared: tests that call it are affected but not owned]
  hunk +1,449    touches TestStaleKeyRetryDefersWhenOrphanScanNeverFinishes (lines 402-449)
  TestManagerOrphanScanBudgetDefaultsToTenSeconds lines 137-142  OWNED      (hunk +1,449 touches the function)
  TestKillExistingOrphansDefersWhenScanNeverFinishes lines 144-166  OWNED      (hunk +1,449 touches the function)
  TestKillExistingOrphansBudgetIsCappedByContextDeadline lines 168-190  OWNED      (hunk +1,449 touches the function)
  TestKillExistingOrphansRepeatedStartsOfWedgedSessionScanOnce lines 192-209  OWNED      (hunk +1,449 touches the function)
  TestKillExistingOrphansKeysLatchByCityAndSession lines 211-232  OWNED      (hunk +1,449 touches the function)
  TestStartDefersWhenOrphanScanNeverFinishes lines 234-250  OWNED      (hunk +1,449 touches the function)
  TestStartRuntimeOnlyDefersWhenOrphanScanNeverFinishes lines 252-269  OWNED      (hunk +1,449 touches the function)
  TestCreateDefersAndRollsBackWhenOrphanScanNeverFinishes lines 271-293  OWNED      (hunk +1,449 touches the function)
  TestKillExistingOrphansStillTerminatesWhenScanReturns lines 297-312  OWNED      (hunk +1,449 touches the function)
  TestKillExistingOrphansStopsBetweenTerminationsWhenContextDone lines 317-340  OWNED      (hunk +1,449 touches the function)
  TestSameCityDifferentSessionHasItsOwnLatch lines 342-363  OWNED      (hunk +1,449 touches the function)
  TestOrphanScanBudgetFieldIsHonored       lines 365-384  OWNED      (hunk +1,449 touches the function)
  TestStaleKeyRetryDefersWhenOrphanScanNeverFinishes lines 402-449  OWNED      (hunk +1,449 touches the function)
SHARED-HUNKS internal/session/orphan_scan_bound_test.go: 1  (a helper, TestMain/init or file-level code changed; a not-owned SKIP in this file is pre-existing ONLY if none of these can reach it)
SITE cmd/gc/reconcile_replay_test.go:574  func=TestReplay kind=test lines=571-583  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/reconcile_sim_coverage_test.go:305  func=TestSimCoverageGate kind=test lines=301-334  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE cmd/gc/session_event_pump_live_test.go:23  func=TestSessionEventPumpLiveHerdr kind=test lines=22-102  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/exec/br_test.go:18  func=TestBrProviderConformance kind=test lines=15-30  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/beads/metadata_empty_clear_conformance_test.go:55  func=TestMetadataEmptyStringClearContract kind=test lines=32-112  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/herdr/activity_live_test.go:20  func=TestActivityLive kind=test lines=19-152  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/herdr/conformance_test.go:18  func=TestHerdrConformance kind=test lines=17-27  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/herdr/events_live_test.go:40  func=TestSessionEventsLive kind=test lines=39-154  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/herdr/kindpath_live_test.go:105  func=TestProviderLiveClaudeKindPath kind=test lines=104-155  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/herdr/panebinding_live_test.go:21  func=TestProviderLiveOccupantSwapKeepsLiveness kind=test lines=20-72  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/herdr/provider_live_test.go:15  func=TestProviderLive kind=test lines=14-90  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:124  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:134  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:170  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:193  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:209  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:238  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:271  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:294  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:313  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:333  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:349  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:367  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:395  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:404  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:421  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:508  func=runFencedInputTests kind=helper lines=503-561  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:525  func=runFencedInputTests kind=helper lines=503-561  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:551  func=runFencedInputTests kind=helper lines=503-561  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:61  func=RunProviderTestsWithOptions kind=helper lines=54-64  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:84  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/runtimetest/conformance.go:93  func=RunLifecycleTestsWithOptions kind=helper lines=77-443  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/tmux/tmux_test.go:3637  func=TestGetKeyBinding_SkipsGasTownBindings kind=test lines=3632-3656  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/tmux/tmux_test.go:3663  func=TestGetKeyBinding_CapturesUserBinding kind=test lines=3658-3682  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/tmux/tmux_test.go:3689  func=TestIsGTBinding_DetectsGasTownBindings kind=test lines=3684-3712  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
SITE internal/runtime/tmux/tmux_test.go:3719  func=TestSetBindings_PreserveFallbackOnRepeatedCalls kind=test lines=3714-3750  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED
```

Complexity formii: literal make complexity-check exits2 at both frozen base and merge,128 findings each with no worsening row; complexity-diff exits0 and reports no threshold changes. The feature does not edit the baseline.

## Full skip inventories

```json
[
  {
    "package": "cmd/gc",
    "test": "TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
    "reason": "cmd_bd_test.go:1314: requires a managed bd lifecycle city; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestBdRuntimeEnvUsesValidProviderStateWhenPublishedStateIsMissing",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-64014bc0bb3833bc198c.log",
    "reason": "bd_env_test.go:1396: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestBuildDesiredState_MinZeroDefaultScaleCheckRoutedWorkCreatesPoolSession",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
    "reason": "build_desired_state_test.go:6331: uses real bd subprocesses for routed-work scale checks; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestBuildDesiredState_PoolCheckUsesManagedCityDoltPortWhenRigHasNoOverride",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4d20b3d28497f844aa39.log",
    "reason": "build_desired_state_test.go:10104: uses a live managed-dolt port probe for scale_check coverage; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestBuildPinnedBDBinaryForTestsUsesGoModSource",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
    "reason": "cmd_wait_test.go:777: builds a real bd binary from source; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestChooseManagedDoltPortKeepsOwnServerPortBeforeItListens",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
    "reason": "cmd_dolt_state_test.go:447: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestChooseManagedDoltPortKeepsOwnServerPortWhenDialsFail",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
    "reason": "cmd_dolt_state_test.go:398: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestChooseManagedDoltPortKeepsPublishedHintPortBeforeItsServerListens",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-752a0bf25b15b465517a.log",
    "reason": "cmd_dolt_state_test.go:485: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestChooseManagedDoltPortSkipsStatePortHeldByAnotherProcess",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
    "reason": "cmd_dolt_state_test.go:526: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestCityInitServiceInitScaffoldsAndFinalizes",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-493754ea2d5b4d8858c8.log",
    "reason": "cityinit_impl_test.go:259: runs the full local init scaffold/finalize path; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestCityRuntimeTick_RefreshesManualSessionOverlayAfterSync",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-088cc38d26d094729229.log",
    "reason": "city_runtime_test.go:4327: runs a full runtime tick/reconcile path; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestCmdGCRealBDTestsUseTestOwnedDoltContext",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
    "reason": "pool_test.go:215: uses real bd to prove test-owned HOME isolation; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestCmdMailInbox_NormalizesCanonicalManagedProviderEnvAndReadsInbox",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-637c74c578a2b17fc654.log",
    "reason": "cmd_mail_test.go:1849: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestControllerReloadCityNameChange",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
    "reason": "controller_test.go:1808: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestControllerReloadInvalidConfig",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
    "reason": "controller_test.go:1746: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestControllerStateMutationRefreshKeepsBuiltinOrdersAndClearsPending",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
    "reason": "api_state_test.go:622: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestControllerStateRuntimeUpdateAcceptsBuiltinAwareRevision",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-423c6a0da1986b90e9a7.log",
    "reason": "api_state_test.go:582: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoBeadsCityUseExternalAdoptUnverifiedSkipsValidation",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
    "reason": "cmd_beads_city_test.go:677: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoBeadsCityUseExternalRewritesCompatRigWithRelativePath",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
    "reason": "cmd_beads_city_test.go:485: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoBeadsCityUseExternalUpdatesIncludedInheritedRigs",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
    "reason": "cmd_beads_city_test.go:188: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoBeadsCityUseExternalWritesVerifiedCityAndInheritedRigs",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
    "reason": "cmd_beads_city_test.go:90: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoDoctorRegistersStaleLocalPackDirCheck",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f679cd2551fcc64525aa.log",
    "reason": "cmd_doctor_test.go:479: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoDoctorRegistersStaleLocalPackDirCheckForDefaultRigRemoteImport",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4d20b3d28497f844aa39.log",
    "reason": "cmd_doctor_test.go:599: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoPrimeWithHook_CodexJSONFormatInfersAgentFromWorkDir",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
    "reason": "cmd_prime_test.go:1336: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoPrimeWithHook_DeliveredStartupPromptCodexJSONHookFormat",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
    "reason": "cmd_prime_test.go:1269: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateAllocatePortCmdRepairsMissingCanonicalProviderStateFromPublishedHint",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2ef13e1b6898c5544764.log",
    "reason": "cmd_dolt_state_test.go:960: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateAllocatePortCmdRepairsMissingProviderStateFromPublishedHint",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4d20b3d28497f844aa39.log",
    "reason": "cmd_dolt_state_test.go:905: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateAllocatePortCmdRepairsStaleProviderStateFromOwnedLivePortHolder",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-23e7d9fc763496dd8095.log",
    "reason": "cmd_dolt_state_test.go:801: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateAllocatePortCmdRepairsStaleWrongPortProviderStateFromPublishedHint",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-64014bc0bb3833bc198c.log",
    "reason": "cmd_dolt_state_test.go:1023: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateAllocatePortCmdRepairsStoppedProviderStateFromOwnedLivePortHolder",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2bde55fab377f50b4739.log",
    "reason": "cmd_dolt_state_test.go:853: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateAllocatePortCmdReusesLiveProviderState",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
    "reason": "cmd_dolt_state_test.go:560: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateExistingManagedCmdFallsBackToPublishedRuntimeState",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
    "reason": "cmd_dolt_state_test.go:1720: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateExistingManagedCmdKeepsOwnedServerWhenDialsFailLocally/dials_never_leave_this_host",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
    "reason": "cmd_dolt_state_test.go:1661: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateExistingManagedCmdKeepsOwnedServerWhenDialsFailLocally/server_refuses",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
    "reason": "cmd_dolt_state_test.go:1661: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateExistingManagedCmdRejectsForeignListenerBackedOnlyByStateDataDir",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
    "reason": "cmd_dolt_state_test.go:1535: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateExistingManagedCmdReportsReusableOwnedServer",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
    "reason": "cmd_dolt_state_test.go:1598: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateInspectManagedCmdReportsPortHolderOwnership",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-423c6a0da1986b90e9a7.log",
    "reason": "cmd_dolt_state_test.go:1307: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateInspectManagedCmdUsesPIDFileAndStateOwnership",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
    "reason": "cmd_dolt_state_test.go:1184: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStatePreflightCleanCmdPreservesLiveArtifacts",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
    "reason": "cmd_dolt_state_test.go:1888: spawns managed dolt holder processes; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateProbeManagedCmdKeepsOwnedHolderRunningWhenDialFailsLocally/dial_never_leaves_this_host",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
    "reason": "cmd_dolt_state_test.go:1407: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateProbeManagedCmdKeepsOwnedHolderRunningWhenDialFailsLocally/server_refuses",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
    "reason": "cmd_dolt_state_test.go:1407: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateProbeManagedCmdReportsImposterHolder",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
    "reason": "cmd_dolt_state_test.go:1445: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateProbeManagedCmdReportsRunningOwnedHolder",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
    "reason": "cmd_dolt_state_test.go:1347: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateRecoverManagedCmdReportsReadOnlyAndRestarts",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
    "reason": "cmd_dolt_state_test.go:3051: spawns managed dolt recovery processes; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateStopManagedCmdDoesNotKillImposterPortHolder",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
    "reason": "cmd_dolt_state_test.go:3010: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateStopManagedCmdStopsManagedPID",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-752a0bf25b15b465517a.log",
    "reason": "cmd_dolt_state_test.go:2888: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestDoltStateWaitReadyCmdReturnsReady",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
    "reason": "cmd_dolt_state_test.go:2780: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestE1PreLeafBooleanHelpNoScopeEager",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-514989cc035c0e9a8ff5.log",
    "reason": "cmd_commands_test.go:814: ga-klo4gz: this test's purpose is exercising ambient cwd-based city resolution (resolveContextFromDir step 10) to distinguish the ambient city's commands from an explicitly-selected one, which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestE1PreLeafBooleanHelpNoScopeEager",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
    "reason": "cmd_commands_test.go:814: ga-klo4gz: this test's purpose is exercising ambient cwd-based city resolution (resolveContextFromDir step 10) to distinguish the ambient city's commands from an explicitly-selected one, which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestEnsureBdRuntimeCustomTypesHealsUpgradedNativeStore",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
    "reason": "gc_beads_bd_config_commit_test.go:457: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestEnsureBeadsProvider_bdAcceptsHealthyServerAfterStartError",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
    "reason": "beads_provider_lifecycle_test.go:1724: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestEvaluatePoolDefaultScaleCheckCountsRoutedReadyWork",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
    "reason": "pool_test.go:108: uses real bd and jq for default scale_check coverage; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestEvaluatePoolDefaultScaleCheckIgnoresRoutedActiveUnassignedWork",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-088cc38d26d094729229.log",
    "reason": "pool_test.go:154: uses real bd and jq for default scale_check coverage; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestEventsMirrorInheritsSetgidGroup",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-9326181a736eb30b71ca.log",
    "reason": "providers_events_mirror_test.go:220: process has no supplementary group to test setgid inheritance with",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestEventsMirrorInheritsSetgidGroup",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2ef13e1b6898c5544764.log",
    "reason": "providers_events_mirror_test.go:220: process has no supplementary group to test setgid inheritance with",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
    "reason": "cmd_bd_test.go:1478: requires a managed bd lifecycle city; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestGcBdUsesEnclosingRigWhenNoFlag",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-9c8cd94da10f0dd04b00.log",
    "reason": "cmd_bd_test.go:1851: ga-klo4gz: this test's purpose is exercising resolveContextFromDir's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit GC_CITY/GC_CITY_PATH/GC_CITY_ROOT override would make it a no-op test rather than a fix",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestGcBdUsesEnclosingRigWhenNoFlag",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-64014bc0bb3833bc198c.log",
    "reason": "cmd_bd_test.go:1851: ga-klo4gz: this test's purpose is exercising resolveContextFromDir's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit GC_CITY/GC_CITY_PATH/GC_CITY_ROOT override would make it a no-op test rather than a fix",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestGcBeadsBdInitEnsuresProjectIdentityWhenMetadataExistsWithoutProjectID",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
    "reason": "beads_provider_lifecycle_test.go:6678: runs the materialized gc-beads-bd init script with GC_BIN helper; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestGcBeadsBdInitNormalizesScopeAndRemovesLocalServerArtifacts",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2ef13e1b6898c5544764.log",
    "reason": "beads_provider_lifecycle_test.go:12979: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestGcBeadsBdProviderOwnedRealInitIgnoresAncestorBeadsWorkspace",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
    "reason": "beads_provider_script_scope_lifecycle_linux_test.go:352: runs the real bd provider init; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestGcBeadsBdProviderOwnedRealLifecycleStopsOwnedProcesses",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-23e7d9fc763496dd8095.log",
    "reason": "beads_provider_lifecycle_test.go:14191: runs the real bd provider lifecycle; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestGcBeadsBdStartConcurrentWaitPassesRemainingExistingManagedBudget",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-088cc38d26d094729229.log",
    "reason": "beads_provider_lifecycle_test.go:11057: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestGcBeadsBdStartDoesNotReplaceLiveLockFileInode",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
    "reason": "beads_provider_lifecycle_test.go:10309: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestGcBeadsBdStartIgnoresReachableCompatPortFileInput",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
    "reason": "beads_provider_lifecycle_test.go:9694: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestGcBeadsBdStartIsIdempotentWhenAlreadyRunning",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2ef13e1b6898c5544764.log",
    "reason": "beads_provider_lifecycle_test.go:11681: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestGcBeadsBdStartLeavesForeignPortHolderAlone",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-493754ea2d5b4d8858c8.log",
    "reason": "beads_provider_lifecycle_test.go:11933: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestGcBeadsBdStartRestartsServerHoldingDeletedDataInodes",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-64014bc0bb3833bc198c.log",
    "reason": "beads_provider_lifecycle_test.go:11768: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestGcBeadsBdStartUsesRootBeadsDataDir",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
    "reason": "beads_provider_lifecycle_test.go:5145: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestGcBeadsBdStartWaitsForConcurrentStarterSuccess",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
    "reason": "beads_provider_lifecycle_test.go:10495: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestGcBeadsBdStartWaitsForSlowConcurrentStarterSuccess",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
    "reason": "beads_provider_lifecycle_test.go:10778: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestHealthBeadsProviderWaitsForStorePingAfterRecovery",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-0e1499ad8e8a0a6b9d81.log",
    "reason": "beads_provider_lifecycle_test.go:5696: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestInstallSupervisorSystemdWarmRefreshLeavesUnregisteredWorkspaceServices",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2bde55fab377f50b4739.log",
    "reason": "cmd_supervisor_test.go:1851: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestInstallSupervisorSystemdWarmRefreshStopsWorkspaceServicesBeforeStart",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-23e7d9fc763496dd8095.log",
    "reason": "cmd_supervisor_test.go:1691: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestLoadStatusSessionSnapshotKillsBdChildOnTimeout",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
    "reason": "city_status_snapshot_test.go:200: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestManagedDoltConfigGoWriterMatchesShellFallbackSemantics",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4d20b3d28497f844aa39.log",
    "reason": "beads_provider_lifecycle_test.go:11537: starts the materialized gc-beads-bd shell fallback; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestManagedDoltDirtyTablesWithPasswordAgainstRealServer",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
    "reason": "beads_provider_dirty_tables_test.go:345: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestManagedDoltHealthCheckWithPasswordUsesDirectHelpersAgainstRealServer",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
    "reason": "dolt_project_id_test.go:184: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestManagedDoltScopeWatchdogHelper",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-aa6cf5c138f0b61b866a.log",
    "reason": "dolt_scope_watchdog_test.go:177: helper process only",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestManagedDoltScopeWatchdogHelper",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
    "reason": "dolt_scope_watchdog_test.go:177: helper process only",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestManagedDoltServingDataDirWithPasswordAgainstRealServer",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
    "reason": "dolt_identity_test.go:156: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestManagedDoltWaitReadyWithPasswordUsesDirectQueryProbe",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
    "reason": "dolt_project_id_test.go:212: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestManagedDoltWatchdogExternalParentHelper",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-c2ab4e7d4735e1bd66a7.log",
    "reason": "dolt_start_managed_test.go:647: helper process only",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestManagedDoltWatchdogExternalParentHelper",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-088cc38d26d094729229.log",
    "reason": "dolt_start_managed_test.go:647: helper process only",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestPackRegistryLiveGascityPacksCatalog",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-e4245ddf6a37030111bc.log",
    "reason": "cmd_pack_registry_test.go:758: set GC_TEST_GASCITY_PACKS_REGISTRY to a gascity-packs registry.toml source to run this live catalog canary",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestPackRegistryLiveGascityPacksCatalog",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
    "reason": "cmd_pack_registry_test.go:758: set GC_TEST_GASCITY_PACKS_REGISTRY to a gascity-packs registry.toml source to run this live catalog canary",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestPrepareCityForSupervisorPrunesLegacyScripts",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
    "reason": "script_resolve_test.go:313: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestProjectIdentityL3AdapterContractAndManagedComposition",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
    "reason": "dolt_project_id_test.go:281: requires a managed dolt server; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestPublishManagedDoltRuntimeStateRecoversMissingProviderStateWithPortHint",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
    "reason": "dolt_runtime_publication_test.go:130: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestPublishManagedDoltRuntimeStateRecoversStaleWrongPortProviderStateWithPublishedHint",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
    "reason": "dolt_runtime_publication_test.go:204: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestPublishManagedDoltRuntimeStateRepairsStaleProviderState",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
    "reason": "dolt_runtime_publication_test.go:28: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestPublishManagedDoltRuntimeStateSucceedsWhenAlreadyValid",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
    "reason": "dolt_runtime_publication_test.go:294: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRecoverManagedDoltProcessKeepsUnmeasuredServerWithoutReportingItHealthy",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
    "reason": "cmd_dolt_state_test.go:3436: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRecoverManagedDoltProcessReturnsWhenConcurrentStarterBecomesReady",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
    "reason": "cmd_dolt_state_test.go:3216: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRecoverManagedDoltProcessReusesHealthyManagedServerOnReboundPort",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-088cc38d26d094729229.log",
    "reason": "cmd_dolt_state_test.go:3313: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRecoverManagedDoltProcessWaitsForLockHolderWhenServerIsUnmeasured/refused_server",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
    "reason": "cmd_dolt_state_test.go:3511: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRecoverManagedDoltProcessWaitsForLockHolderWhenServerIsUnmeasured/unmeasured_server",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
    "reason": "cmd_dolt_state_test.go:3511: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRecoverManagedDoltProcessWithPasswordReusesHealthyRealServer",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
    "reason": "dolt_project_id_test.go:225: requires a managed dolt server; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRegisterCityWithSupervisorKeepsRegistrationWhenReloadFails",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2bde55fab377f50b4739.log",
    "reason": "cmd_supervisor_city_test.go:350: exercises supervisor registration retry behavior; run make test-cmd-gc-process for scenario coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRegisterRealBDServerStopStopsServerBeforeTempDirCleanup",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f679cd2551fcc64525aa.log",
    "reason": "path_helpers_test.go:1055: spawns subprocesses to simulate still-running dolt sql-servers; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestReloadConfigTracedRescansOrdersWhenConfigRevisionUnchanged",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
    "reason": "cmd_reload_test.go:606: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRenderedCrewPromptShowsProviderSpecificInstructionsFile",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-830685c60d0c2dd88567.log",
    "reason": "prompt_test.go:1420: crew.template.md not readable at ../../examples/gastown/packs/gastown/assets/prompts/crew.template.md: open ../../examples/gastown/packs/gastown/assets/prompts/crew.template.md: no such file or directory",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRenderedCrewPromptShowsProviderSpecificInstructionsFile",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
    "reason": "prompt_test.go:1420: crew.template.md not readable at ../../examples/gastown/packs/gastown/assets/prompts/crew.template.md: open ../../examples/gastown/packs/gastown/assets/prompts/crew.template.md: no such file or directory",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRepairedManagedDoltRuntimeStateAcceptsSymlinkEquivalentDataDir",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-0e1499ad8e8a0a6b9d81.log",
    "reason": "cmd_dolt_state_test.go:189: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRepairedManagedDoltRuntimeStateClassifiesDialFailure",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f679cd2551fcc64525aa.log",
    "reason": "cmd_dolt_state_test.go:760: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestReplay",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-cb110414c557c0928e82.log",
    "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
    "class": "environment-absence"
  },
  {
    "package": "cmd/gc",
    "test": "TestReplay",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
    "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
    "class": "environment-absence"
  },
  {
    "package": "cmd/gc",
    "test": "TestResolveCityFlag/flag_empty_fallback",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-75f7330b9b673df65e4f.log",
    "reason": "main_test.go:840: ga-klo4gz: this subtest's purpose is exercising resolveCity's ambient cwd-based fallback (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestResolveCityFlag/flag_empty_fallback",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
    "reason": "main_test.go:840: ga-klo4gz: this subtest's purpose is exercising resolveCity's ambient cwd-based fallback (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestResolveStoredSessionLogSource_UniqueWorkDirFallsBackBeyondLatestAlias",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
    "reason": "cmd_session_logs_test.go:388: probes provider transcript lookup before workdir fallback; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestResolvedRuntimeCityDoltTargetDoesNotMaskInvalidCanonicalConfigWithProviderState",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-637c74c578a2b17fc654.log",
    "reason": "bd_env_test.go:1643: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestReviewBdRigWithEdgesRefusesBeforeAnyWrite",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-493754ea2d5b4d8858c8.log",
    "reason": "rig_import_review_test.go:84: no real bd on PATH",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRigAnywhere_ResolveContext/registered_rig_cwd_ambiguous_falls_through",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-cb110414c557c0928e82.log",
    "reason": "rig_anywhere_test.go:489: ga-klo4gz: this subtest's purpose is exercising the fallthrough from an ambiguous registered-rig match (step 9) to resolveContext's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRigAnywhere_ResolveContext/registered_rig_cwd_ambiguous_falls_through",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
    "reason": "rig_anywhere_test.go:489: ga-klo4gz: this subtest's purpose is exercising the fallthrough from an ambiguous registered-rig match (step 9) to resolveContext's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRigAnywhere_ResolveContext/walk_up_fallback",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-cb110414c557c0928e82.log",
    "reason": "rig_anywhere_test.go:378: ga-klo4gz: this subtest's purpose is exercising resolveContext's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRigAnywhere_ResolveContext/walk_up_fallback",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
    "reason": "rig_anywhere_test.go:378: ga-klo4gz: this subtest's purpose is exercising resolveContext's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRigAnywhere_ResolveContext/walk_up_fallback_with_rig_match",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-cb110414c557c0928e82.log",
    "reason": "rig_anywhere_test.go:402: ga-klo4gz: this subtest's purpose is exercising resolveContext's ambient cwd walk-up (step 10) followed by a rig match, which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRigAnywhere_ResolveContext/walk_up_fallback_with_rig_match",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-fd986cf5bab8709b8d45.log",
    "reason": "rig_anywhere_test.go:402: ga-klo4gz: this subtest's purpose is exercising resolveContext's ambient cwd walk-up (step 10) followed by a rig match, which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRigImportFromARealBdRigRaisesItsMinHashLength",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2bde55fab377f50b4739.log",
    "reason": "rig_import_fixtures_test.go:460: no real bd on PATH",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRunManagedDoltSQLEmptyPasswordMasksDefaultProfileAgainstRealServer",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-493754ea2d5b4d8858c8.log",
    "reason": "dolt_sql_health_test.go:525: requires a real Dolt server; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestRunWorkflowServeReturnsControlErrorWithoutQuarantine",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
    "reason": "cmd_convoy_dispatch_test.go:3899: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestScopedBdStoreForCityKillsChildOnCtxCancel",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
    "reason": "scoped_store_test.go:364: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestSendReloadControlRequestInvalidConfig",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
    "reason": "cmd_reload_test.go:740: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestSendReloadControlRequestNoChange",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-423c6a0da1986b90e9a7.log",
    "reason": "cmd_reload_test.go:540: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestSessionEventPumpLiveHerdr",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-18745ddf7d62465654f5.log",
    "reason": "session_event_pump_live_test.go:23: herdr not installed",
    "class": "environment-absence"
  },
  {
    "package": "cmd/gc",
    "test": "TestSessionEventPumpLiveHerdr",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
    "reason": "session_event_pump_live_test.go:23: herdr not installed",
    "class": "environment-absence"
  },
  {
    "package": "cmd/gc",
    "test": "TestShellExecRunnerKillsProcessGroupAfterWaitDelay",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
    "reason": "order_dispatch_test.go:3718: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestShellExecRunnerKillsProcessGroupOnTimeout",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
    "reason": "order_dispatch_test.go:3695: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestSimCoverageGate",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-568812e4d525bdd1f974.log",
    "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
    "class": "environment-absence"
  },
  {
    "package": "cmd/gc",
    "test": "TestSimCoverageGate",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
    "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
    "class": "environment-absence"
  },
  {
    "package": "cmd/gc",
    "test": "TestSplitStrictControllerSharesOneProjection",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-568812e4d525bdd1f974.log",
    "reason": "split_strict_gate_test.go:175: exercises the split-strict gate (GC_TEST_SPLIT_STRICT=1)",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestSplitStrictControllerSharesOneProjection",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
    "reason": "split_strict_gate_test.go:175: exercises the split-strict gate (GC_TEST_SPLIT_STRICT=1)",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestStartTCPListenerProcessInDirRegistersCleanup",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
    "reason": "cmd_dolt_state_test.go:582: spawns a TCP listener process and verifies cleanup; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestStopCityManagedBeadsProviderAfterSuccessfulStopStopsDefaultBD",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2bde55fab377f50b4739.log",
    "reason": "cmd_stop_test.go:1376: exercises managed bd provider shutdown; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestStopManagedDoltRefusesSIGKILLWhileLockHeld",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-423c6a0da1986b90e9a7.log",
    "reason": "dolt_lifecycle_race_test.go:170: spawns real processes; run via make test-cmd-gc-process",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestStopManagedDoltWaitsForLockReleaseAfterExit",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d9e667428f972d067bd3.log",
    "reason": "dolt_lifecycle_race_test.go:238: spawns real processes; run via make test-cmd-gc-process",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestStopSupervisorWithWaitFailsWhenLaunchdStopFails",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-56f04454dd9158a9ee8a.log",
    "reason": "cmd_supervisor_test.go:6604: launchd path only applies on darwin",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestStopSupervisorWithWaitFailsWhenLaunchdStopFails",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1ab526405ace21879f85.log",
    "reason": "cmd_supervisor_test.go:6604: launchd path only applies on darwin",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestSupervisorInstallUnsupportedOS",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-509ef0a6cd81866833cc.log",
    "reason": "cmd_supervisor_test.go:1473: unsupported-os test only applies outside darwin/linux",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestSupervisorInstallUnsupportedOS",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
    "reason": "cmd_supervisor_test.go:1473: unsupported-os test only applies outside darwin/linux",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-18745ddf7d62465654f5.log",
    "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
    "class": "environment-absence"
  },
  {
    "package": "cmd/gc",
    "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-752a0bf25b15b465517a.log",
    "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
    "class": "environment-absence"
  },
  {
    "package": "cmd/gc",
    "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-9c8cd94da10f0dd04b00.log",
    "reason": "tmux_leak_guard_mntns_test.go:98: bwrap not installed; cannot build a private-/tmp mount namespace",
    "class": "environment-absence"
  },
  {
    "package": "cmd/gc",
    "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-1cc8c8ab55e59a9e03a9.log",
    "reason": "tmux_leak_guard_mntns_test.go:98: bwrap not installed; cannot build a private-/tmp mount namespace",
    "class": "environment-absence"
  },
  {
    "package": "cmd/gc",
    "test": "TestTerminateManagedDoltPIDRefusesSIGKILLWhileLockHeld",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-637c74c578a2b17fc654.log",
    "reason": "dolt_lifecycle_race_test.go:148: spawns real processes; run via make test-cmd-gc-process",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestTerminateManagedDoltTestPIDKillsProcessGroup",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
    "reason": "dolt_start_managed_test.go:1635: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestTutorial01",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-752a0bf25b15b465517a.log",
    "reason": "main_test.go:379: runs tutorial testscript scenarios; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestUnloadSupervisorServiceDarwinDisablesBootsOutAndVerifiesAbsent",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-75af79934639abaae69c.log",
    "reason": "cmd_supervisor_test.go:2527: launchd path only applies on darwin",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestUnloadSupervisorServiceDarwinDisablesBootsOutAndVerifiesAbsent",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-abd97d2cb9b450225d57.log",
    "reason": "cmd_supervisor_test.go:2527: launchd path only applies on darwin",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestUnloadSupervisorServiceDarwinFailsWhenTargetStillLoaded",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-2ab73b52d8fd4dc346df.log",
    "reason": "cmd_supervisor_test.go:2642: launchd path only applies on darwin",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestUnloadSupervisorServiceDarwinFailsWhenTargetStillLoaded",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-912ab643330b35bd0b6b.log",
    "reason": "cmd_supervisor_test.go:2642: launchd path only applies on darwin",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestUnloadSupervisorServiceDarwinReturnsLaunchctlFailures",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-568812e4d525bdd1f974.log",
    "reason": "cmd_supervisor_test.go:2819: launchd path only applies on darwin",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestUnloadSupervisorServiceDarwinReturnsLaunchctlFailures",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
    "reason": "cmd_supervisor_test.go:2819: launchd path only applies on darwin",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestUnloadSupervisorServiceDarwinWaitsThroughSIGTERMedUntilAbsent",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-514989cc035c0e9a8ff5.log",
    "reason": "cmd_supervisor_test.go:2577: launchd path only applies on darwin",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestUnloadSupervisorServiceDarwinWaitsThroughSIGTERMedUntilAbsent",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f93a0ca96ccc61d0cae9.log",
    "reason": "cmd_supervisor_test.go:2577: launchd path only applies on darwin",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestV2G1Budget",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-2ab73b52d8fd4dc346df.log",
    "reason": "v2_g1_test.go:120: gate G1 times this host: set GC_V2_G1=1",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestV2G1Budget",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-0e1499ad8e8a0a6b9d81.log",
    "reason": "v2_g1_test.go:120: gate G1 times this host: set GC_V2_G1=1",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestV2GatherProductionShape",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-509ef0a6cd81866833cc.log",
    "reason": "v2_g1_test.go:178: gate G1b reads a store copy: set GC_V2_G1B_STORE",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestV2GatherProductionShape",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
    "reason": "v2_g1_test.go:178: gate G1b reads a store copy: set GC_V2_G1B_STORE",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestValidDoltRuntimeStateAcceptsSymlinkEquivalentDataDir",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-493754ea2d5b4d8858c8.log",
    "reason": "cmd_dolt_state_test.go:142: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestValidDoltRuntimeStateClassifiesDialFailure",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
    "reason": "cmd_dolt_state_test.go:722: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestValidDoltRuntimeStateRejectsAlivePIDThatDoesNotOwnManagedDolt",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a9df316125cd0aeff2e.log",
    "reason": "cmd_dolt_state_test.go:694: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestVerifyExternalDoltEndpointRejectsMissingLocalProjectID",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-637c74c578a2b17fc654.log",
    "reason": "cmd_rig_endpoint_test.go:1689: requires a managed external dolt endpoint; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestVerifyExternalDoltEndpointRejectsProjectIdentityMismatch",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27b23e708b477fce4dad.log",
    "reason": "cmd_rig_endpoint_test.go:1651: requires a managed external dolt endpoint; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestVerifySupervisorServiceStoppedDarwinFailsWhenAbsenceUnconfirmed",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-c2ab4e7d4735e1bd66a7.log",
    "reason": "cmd_supervisor_test.go:2695: launchd path only applies on darwin",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestVerifySupervisorServiceStoppedDarwinFailsWhenAbsenceUnconfirmed",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-088cc38d26d094729229.log",
    "reason": "cmd_supervisor_test.go:2695: launchd path only applies on darwin",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestVerifySupervisorServiceStoppedDarwinHonorsShorterCallerDeadline",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-509ef0a6cd81866833cc.log",
    "reason": "cmd_supervisor_test.go:2742: launchd path only applies on darwin",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestVerifySupervisorServiceStoppedDarwinHonorsShorterCallerDeadline",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-cd6930bb0d0da8646500.log",
    "reason": "cmd_supervisor_test.go:2742: launchd path only applies on darwin",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestWatchConfigTargets_CloseReturnsFDsToBaseline",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-568812e4d525bdd1f974.log",
    "reason": "controller_watcher_fd_test.go:27: darwin-only: the kqueue Close() FD leak (fsnotify#732) is a Darwin/kqueue bug",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestWatchConfigTargets_CloseReturnsFDsToBaseline",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-10cc7795c0ec199a5530.log",
    "reason": "controller_watcher_fd_test.go:27: darwin-only: the kqueue Close() FD leak (fsnotify#732) is a Darwin/kqueue bug",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestWorkerHandleForSessionWithConfigUsesResolvedProviderOnFirstStart",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f679cd2551fcc64525aa.log",
    "reason": "worker_handle_test.go:36: waits through stale session-key detection; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestWorkerHandleForSessionWithConfigUsesResolvedProviderOnResume",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2ef13e1b6898c5544764.log",
    "reason": "worker_handle_test.go:1266: waits through stale session-key detection; run make test-cmd-gc-process for full coverage",
    "class": "other"
  },
  {
    "package": "examples/gastown",
    "test": "TestAttachedRigScopeShellToken/zsh",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-be91eec86b4dbf7e66f1.log",
    "reason": "gastown_test.go:4153: zsh not installed",
    "class": "environment-absence"
  },
  {
    "package": "examples/gastown",
    "test": "TestAttachedRigScopeShellToken/zsh",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ef24b9dbe59f5d04fac1.log",
    "reason": "gastown_test.go:4153: zsh not installed",
    "class": "environment-absence"
  },
  {
    "package": "examples/gastown",
    "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-a0f68450216c5e4d04af.log",
    "reason": "maintenance_scripts_test.go:9330: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
    "class": "environment-absence"
  },
  {
    "package": "examples/gastown",
    "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-df0dcd5ff9a717b4f2a3.log",
    "reason": "maintenance_scripts_test.go:9330: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
    "class": "environment-absence"
  },
  {
    "package": "examples/gastown",
    "test": "TestTmuxKeybindingsAlternateScreenPassthrough",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-be91eec86b4dbf7e66f1.log",
    "reason": "gastown_test.go:492: embedded gascity-packs does not yet include #{alternate_on} in WheelUpPane (gastownhall/gascity-packs#136); test activates once go.mod is bumped to that release",
    "class": "other"
  },
  {
    "package": "examples/gastown",
    "test": "TestTmuxKeybindingsAlternateScreenPassthrough",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ef24b9dbe59f5d04fac1.log",
    "reason": "gastown_test.go:492: embedded gascity-packs does not yet include #{alternate_on} in WheelUpPane (gastownhall/gascity-packs#136); test activates once go.mod is bumped to that release",
    "class": "other"
  },
  {
    "package": "internal/api",
    "test": "TestStatusListStoreWithTimeoutKillsBdChildOnTimeout",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-c1bdba0ccc8453285d8e.log",
    "reason": "handler_status_scoped_store_test.go:507: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
    "class": "other"
  },
  {
    "package": "internal/api",
    "test": "TestStatusReadyStoreWithTimeoutKillsBdChildOnTimeout",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-00130d8c2aec306b5e94.log",
    "reason": "handler_status_scoped_store_test.go:544: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
    "class": "other"
  },
  {
    "package": "internal/api",
    "test": "TestStatusSessionSnapshotKillsBdChildOnTimeout",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5d010ccbe1c203fcb722.log",
    "reason": "handler_status_scoped_store_test.go:141: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestBdStoreCloseNotificationConformance/CloseIfMatchAnnouncesOneClose",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a4a1a23acc13a6dd98f.log",
    "reason": "close_notification_conformance.go:54: CloseIfMatch: conditional writes unsupported",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestBdStoreCloseNotificationConformance/CloseIfMatchAnnouncesOneClose",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-9a6a09bab414814cab53.log",
    "reason": "close_notification_conformance.go:54: CloseIfMatch: conditional writes unsupported",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestBdStoreCloseNotificationConformance/UpdateIfMatchStatusClosedAnnouncesOneClose",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a4a1a23acc13a6dd98f.log",
    "reason": "close_notification_conformance.go:57: UpdateIfMatch(status=closed): conditional writes unsupported",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestBdStoreCloseNotificationConformance/UpdateIfMatchStatusClosedAnnouncesOneClose",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-9a6a09bab414814cab53.log",
    "reason": "close_notification_conformance.go:57: UpdateIfMatch(status=closed): conditional writes unsupported",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestBdStoreConditionalWriterConformance",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-20cfdbabba8f52b16e0b.log",
    "reason": "bdstore_conditional_integration_test.go:69: installed bd lacks --if-revision (pre-beads#4682); the conformance row skips cleanly and becomes the authoritative guard for the provisional body codes once #4682 lands",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestExecTimeoutHardExitHarness",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a4a1a23acc13a6dd98f.log",
    "reason": "exec_timeout_linux_test.go:66: harness process",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestExecTimeoutHardExitHarness",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-9a6a09bab414814cab53.log",
    "reason": "exec_timeout_linux_test.go:66: harness process",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestFileStore/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a4a1a23acc13a6dd98f.log",
    "reason": "close_notification_conformance.go:89: backing *beads.FileStore lacks the capability UpdateIfAssignmentStatusClosed needs",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestFileStore/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-9a6a09bab414814cab53.log",
    "reason": "close_notification_conformance.go:89: backing *beads.FileStore lacks the capability UpdateIfAssignmentStatusClosed needs",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestFileStoreReadyParityConformance/FileStore/CachedReadyParity",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-ceb32a3c147a95796597.log",
    "reason": "ready_parity_conformance.go:69: skipping CachedReadyParity (bead ga-gmf8r, expires 2026-12-15): MemStore and FileStore Ready block on a missing or foreign blocker and on a closed blocker with gc.work_outcome=blocked, which a primed CachingStore cannot see without a ready projection, and return insertion order with Limit applied mid-scan instead of the canonical (priority, created_at, id) order. Replacement proof: MemStore implements enrichReadyProjectionForCache and canonical ready order, and TestMemStoreReadyParityConformance and TestFileStoreReadyParityConformance run this suite unwaived",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestFileStoreReadyParityConformance/FileStore/CachedReadyParity",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-b398920224b769833cd5.log",
    "reason": "ready_parity_conformance.go:69: skipping CachedReadyParity (bead ga-gmf8r, expires 2026-12-15): MemStore and FileStore Ready block on a missing or foreign blocker and on a closed blocker with gc.work_outcome=blocked, which a primed CachingStore cannot see without a ready projection, and return insertion order with Limit applied mid-scan instead of the canonical (priority, created_at, id) order. Replacement proof: MemStore implements enrichReadyProjectionForCache and canonical ready order, and TestMemStoreReadyParityConformance and TestFileStoreReadyParityConformance run this suite unwaived",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestKillCommandTreeKillsProcessGroup",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ea4511196531bb4b8c55.log",
    "reason": "bdstore_exec_internal_test.go:266: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestMemStore/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-20cfdbabba8f52b16e0b.log",
    "reason": "close_notification_conformance.go:89: backing *beads.MemStore lacks the capability UpdateIfAssignmentStatusClosed needs",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestMemStore/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ea4511196531bb4b8c55.log",
    "reason": "close_notification_conformance.go:89: backing *beads.MemStore lacks the capability UpdateIfAssignmentStatusClosed needs",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestMemStoreReadyParityConformance/MemStore/CachedReadyParity",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-8d820cd4c52192de043e.log",
    "reason": "ready_parity_conformance.go:69: skipping CachedReadyParity (bead ga-gmf8r, expires 2026-12-15): MemStore and FileStore Ready block on a missing or foreign blocker and on a closed blocker with gc.work_outcome=blocked, which a primed CachingStore cannot see without a ready projection, and return insertion order with Limit applied mid-scan instead of the canonical (priority, created_at, id) order. Replacement proof: MemStore implements enrichReadyProjectionForCache and canonical ready order, and TestMemStoreReadyParityConformance and TestFileStoreReadyParityConformance run this suite unwaived",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestMemStoreReadyParityConformance/MemStore/CachedReadyParity",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-363a36f2662d541bc75e.log",
    "reason": "ready_parity_conformance.go:69: skipping CachedReadyParity (bead ga-gmf8r, expires 2026-12-15): MemStore and FileStore Ready block on a missing or foreign blocker and on a closed blocker with gc.work_outcome=blocked, which a primed CachingStore cannot see without a ready projection, and return insertion order with Limit applied mid-scan instead of the canonical (priority, created_at, id) order. Replacement proof: MemStore implements enrichReadyProjectionForCache and canonical ready order, and TestMemStoreReadyParityConformance and TestFileStoreReadyParityConformance run this suite unwaived",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestMetadataEmptyStringClearContract/Postgres/BatchClearsOneKeyKeepsAnother",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-86f8e1ae5bca4d4a5e22.log",
    "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
    "class": "environment-absence"
  },
  {
    "package": "internal/beads",
    "test": "TestMetadataEmptyStringClearContract/Postgres/BatchClearsOneKeyKeepsAnother",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27371dea3b130a4c231f.log",
    "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
    "class": "environment-absence"
  },
  {
    "package": "internal/beads",
    "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-86f8e1ae5bca4d4a5e22.log",
    "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
    "class": "environment-absence"
  },
  {
    "package": "internal/beads",
    "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27371dea3b130a4c231f.log",
    "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
    "class": "environment-absence"
  },
  {
    "package": "internal/beads",
    "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-86f8e1ae5bca4d4a5e22.log",
    "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
    "class": "environment-absence"
  },
  {
    "package": "internal/beads",
    "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27371dea3b130a4c231f.log",
    "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
    "class": "environment-absence"
  },
  {
    "package": "internal/beads",
    "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-86f8e1ae5bca4d4a5e22.log",
    "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
    "class": "environment-absence"
  },
  {
    "package": "internal/beads",
    "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-27371dea3b130a4c231f.log",
    "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
    "class": "environment-absence"
  },
  {
    "package": "internal/beads",
    "test": "TestNativeDoltStoreConformance/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-a9e2a6fbbc2774327994.log",
    "reason": "close_notification_conformance.go:89: backing *beads.NativeDoltStore lacks the capability UpdateIfAssignmentStatusClosed needs",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestNativeDoltStoreConformance/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-3ee1ae2eaa26dfd63a88.log",
    "reason": "close_notification_conformance.go:89: backing *beads.NativeDoltStore lacks the capability UpdateIfAssignmentStatusClosed needs",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestNativeDoltStoreNormalizesRealUpstreamMissingIssueErrors",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-999a89ecc5b3d09abdd4.log",
    "reason": "native_dolt_store_test.go:651: upstream beads storage unavailable: Dolt server unreachable at 127.0.0.1:0: dial tcp 127.0.0.1:0: connect: connection refused",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestNativeDoltStoreNormalizesRealUpstreamMissingIssueErrors",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-a93729df64a652fced43.log",
    "reason": "native_dolt_store_test.go:651: upstream beads storage unavailable: Dolt server unreachable at 127.0.0.1:0: dial tcp 127.0.0.1:0: connect: connection refused",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestProbeVersionsAgainstHostBinaries/bd",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-363a36f2662d541bc75e.log",
    "reason": "binary_versions_test.go:66: bd not probeable in this environment: locate bd: exec: \"bd\": executable file not found in $PATH",
    "class": "environment-absence"
  },
  {
    "package": "internal/beads",
    "test": "TestSQLiteStoreConformance/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-a1f48694841e1859d61a.log",
    "reason": "close_notification_conformance.go:89: backing *beads.SQLiteStore lacks the capability UpdateIfAssignmentStatusClosed needs",
    "class": "other"
  },
  {
    "package": "internal/beads",
    "test": "TestSQLiteStoreConformance/UpdateIfAssignmentStatusClosedAnnouncesOneClose",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-9a6a09bab414814cab53.log",
    "reason": "close_notification_conformance.go:89: backing *beads.SQLiteStore lacks the capability UpdateIfAssignmentStatusClosed needs",
    "class": "other"
  },
  {
    "package": "internal/beads/beadstest",
    "test": "TestRequireLedgeredSkipIsDateFree/LongLapsed",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ec8983708a4fd6968a43.log",
    "reason": "conformance_skips_test.go:60: skipping LongLapsed (bead ga-example, expires 2000-01-01): example",
    "class": "other"
  },
  {
    "package": "internal/beads/contract",
    "test": "TestMigrateJournalFileMatchesPinnedBeads",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8544388e16a0d6b0cdb0.log",
    "reason": "migrate_journal_test.go:37: pinned beads v1.3.2 is not unpacked in the local module cache: open /tmp/bt/_tmp/35731ca362c311940500020a6c8a2007/go/pkg/mod/github.com/steveyegge/beads@v1.3.2/cmd/bd/migrate_dolt_mode.go: no such file or directory",
    "class": "environment-absence"
  },
  {
    "package": "internal/beads/exec",
    "test": "TestBrProviderConformance",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-64046beff95fb28ccd66.log",
    "reason": "br_test.go:18: br not available",
    "class": "environment-absence"
  },
  {
    "package": "internal/beads/splittest",
    "test": "TestStrictStorePinnedIDFenceConformance/TheFenceCoversTheGuardedBatch",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-c424d7b9771e02bed0c3.log",
    "reason": "pinned_id_fence_conformance.go:467: store has no guarded batcher",
    "class": "other"
  },
  {
    "package": "internal/beads/splittest",
    "test": "TestStrictStorePinnedIDFenceConformance/TheGuardedBatchChecksTheNamespaceBeforeExistence",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-c424d7b9771e02bed0c3.log",
    "reason": "pinned_id_fence_conformance.go:533: store has no guarded batcher",
    "class": "other"
  },
  {
    "package": "internal/config",
    "test": "TestLoadWithIncludes_PatchTargetingImplicitAgentErrorsToday",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4bbf1267e0c4ccc54042.log",
    "reason": "implicit_agent_patch_repro_test.go:68: bug ga-91uk5m fixed; delete this characterization test",
    "class": "other"
  },
  {
    "package": "internal/connect/tokenrender",
    "test": "TestWriteTokenFileKeepsGroup",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f79997cecbebd6b14e33.log",
    "reason": "write_test.go:376: the test user has no secondary group and is not root",
    "class": "other"
  },
  {
    "package": "internal/connect/tokenrender",
    "test": "TestWriteTokenFileKeepsOwnerAsRoot",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-f79997cecbebd6b14e33.log",
    "reason": "write_test.go:475: needs root to own a file as another user",
    "class": "other"
  },
  {
    "package": "internal/convergence",
    "test": "TestResolveConditionPath/darwin_private_temp_alias_must_not_falsely_reject_a_contained_relative_condition_path",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-0fb1903ec0635aebe9eb.log",
    "reason": "condition_test.go:594: darwin-only: the /private/{tmp,var} alias collapse is a no-op on other platforms",
    "class": "other"
  },
  {
    "package": "internal/convergence",
    "test": "TestResolveEvaluateStep_DarwinPrivateTempAliasWithExistingPrompt",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-0fb1903ec0635aebe9eb.log",
    "reason": "evaluate_test.go:189: darwin-only: the /private/{tmp,var} alias collapse is a no-op on other platforms",
    "class": "other"
  },
  {
    "package": "internal/dispatch",
    "test": "TestCrashInjectionCallBudgetMolecule/fail",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-e7f4116e9a278807bf1e.log",
    "reason": "call_budget_test.go:536: ga-vnycm2.27: interrupted abort-scope converges with different skip metadata",
    "class": "other",
    "raw_reason": "",
    "reason_recovered_from": {
      "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-e7f4116e9a278807bf1e.log",
      "line": 3781
    }
  },
  {
    "package": "internal/doctor",
    "test": "TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
    "reason": "checks_custom_types_test.go:467: bd binary not on PATH",
    "class": "environment-absence"
  },
  {
    "package": "internal/doctor",
    "test": "TestCustomTypesCheck_TableDrift",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
    "reason": "checks_custom_types_test.go:297: bd binary not on PATH",
    "class": "environment-absence"
  },
  {
    "package": "internal/doctor",
    "test": "TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
    "reason": "checks_custom_types_test.go:399: bd binary not on PATH",
    "class": "environment-absence"
  },
  {
    "package": "internal/formula",
    "test": "TestCompileBugReportFlowV2",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8754fd06808e5b38215b.log",
    "reason": "compile_test.go:1359: tooling formula not present: stat /home/ubuntu/tooling/formulas/mol-bug-report-flow-v2.toml: permission denied",
    "class": "other"
  },
  {
    "package": "internal/fsys",
    "test": "TestWriteFileIfContentOrModeChangedAtomic_RepairsSpecialModeBits",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8baf01f801bd5b165b89.log",
    "reason": "atomic_test.go:185: filesystem did not preserve setuid bit in test mode: -rwxr-xr-x",
    "class": "other"
  },
  {
    "package": "internal/gitcred",
    "test": "TestLoadAcceptsRootOwnedGroupReadable",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5c43118a23fc611d12c8.log",
    "reason": "rules_test.go:186: needs root to chown the fixture to the Secret-mount shape",
    "class": "other"
  },
  {
    "package": "internal/mail/exec",
    "test": "TestMCPMailBridgeSourceable",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
    "reason": "mcp_conformance_test.go:159: MCP mail script not found: file does not exist",
    "class": "environment-absence"
  },
  {
    "package": "internal/mail/exec",
    "test": "TestMCPMailConformance",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
    "reason": "mcp_conformance_test.go:26: MCP mail script not found: file does not exist",
    "class": "environment-absence"
  },
  {
    "package": "internal/mail/exec",
    "test": "TestMCPMailConformanceLive",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
    "reason": "mcp_live_test.go:35: set GC_TEST_MCP_MAIL=1 to run (or use make test-mcp-mail)",
    "class": "other"
  },
  {
    "package": "internal/mail/exec",
    "test": "TestMCPMailCrossPodNameResolution",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
    "reason": "mcp_conformance_test.go:195: MCP mail script not found: file does not exist",
    "class": "environment-absence"
  },
  {
    "package": "internal/mail/exec",
    "test": "TestMCPMailProjectKeyIsolation",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
    "reason": "mcp_conformance_test.go:286: MCP mail script not found: file does not exist",
    "class": "environment-absence"
  },
  {
    "package": "internal/mail/exec",
    "test": "TestMCPMailReplyMissingOriginalIsNotFound",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
    "reason": "mcp_conformance_test.go:80: MCP mail script not found: file does not exist",
    "class": "environment-absence"
  },
  {
    "package": "internal/molecule",
    "test": "TestBuildRecipeApplyPlanBugReportFlowV2",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-46cac984b12b94b66cce.log",
    "reason": "molecule_test.go:30: tooling formula not present: stat /home/ubuntu/tooling/formulas/mol-bug-report-flow-v2.formula.toml: permission denied",
    "class": "other"
  },
  {
    "package": "internal/orders",
    "test": "TestCheckTriggerConditionKillsProcessGroupAfterWaitDelay",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ff9cab7068d884a7ea3a.log",
    "reason": "triggers_test.go:495: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
    "class": "other"
  },
  {
    "package": "internal/orders",
    "test": "TestCheckTriggerConditionKillsProcessGroupOnTimeout",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ff9cab7068d884a7ea3a.log",
    "reason": "triggers_test.go:464: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
    "class": "other"
  },
  {
    "package": "internal/pidutil",
    "test": "TestAliveWithStartTime_UnreadableIdentityKeepsAliveAnswer",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-cc30e270515707553560.log",
    "reason": "starttime_portable_test.go:86: on linux /proc answers directly, so a ps stub cannot make the identity unreadable",
    "class": "other"
  },
  {
    "package": "internal/pidutil",
    "test": "TestCmdline_FailsClosedWhenUnreadable",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-cc30e270515707553560.log",
    "reason": "cmdline_portable_test.go:141: PID 1 argv is readable in this environment; cannot exercise the fail-closed path this way",
    "class": "other"
  },
  {
    "package": "internal/processgroup",
    "test": "TestTerminateCommandPreservesGroupFailureAfterDirectKill",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-464b529be21ea4ef14f5.log",
    "reason": "processgroup_unix_test.go:71: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1",
    "class": "other"
  },
  {
    "package": "internal/runproj",
    "test": "TestRegenerateGoldens",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-9cf54403b0e740f317fc.log",
    "reason": "golden_regen_test.go:15: set RUNPROJ_UPDATE_GOLDENS=1 to rewrite testdata goldens",
    "class": "other"
  },
  {
    "package": "internal/runtime/exec",
    "test": "TestExecConformance/FencedInput_AbsentSessionRefuses",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-02ff57e430fd7218601d.log",
    "reason": "conformance.go:551: provider cannot fence session input",
    "class": "other"
  },
  {
    "package": "internal/runtime/exec",
    "test": "TestExecConformance/FencedInput_AbsentSessionRefuses",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d1f6abca8430b335d6c5.log",
    "reason": "conformance.go:551: provider cannot fence session input",
    "class": "other"
  },
  {
    "package": "internal/runtime/exec",
    "test": "TestExecConformance/FencedInput_MatchingFenceDelivers",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-02ff57e430fd7218601d.log",
    "reason": "conformance.go:508: provider cannot fence session input",
    "class": "other"
  },
  {
    "package": "internal/runtime/exec",
    "test": "TestExecConformance/FencedInput_MatchingFenceDelivers",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d1f6abca8430b335d6c5.log",
    "reason": "conformance.go:508: provider cannot fence session input",
    "class": "other"
  },
  {
    "package": "internal/runtime/exec",
    "test": "TestExecConformance/FencedInput_MismatchedFenceRefusesEveryInput",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-02ff57e430fd7218601d.log",
    "reason": "conformance.go:525: provider cannot fence session input",
    "class": "other"
  },
  {
    "package": "internal/runtime/exec",
    "test": "TestExecConformance/FencedInput_MismatchedFenceRefusesEveryInput",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d1f6abca8430b335d6c5.log",
    "reason": "conformance.go:525: provider cannot fence session input",
    "class": "other"
  },
  {
    "package": "internal/runtime/herdr",
    "test": "TestActivityLive",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
    "reason": "activity_live_test.go:20: herdr not installed",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/herdr",
    "test": "TestActivityLive",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
    "reason": "activity_live_test.go:20: herdr not installed",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/herdr",
    "test": "TestHerdrConformance",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
    "reason": "conformance_test.go:18: herdr not installed",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/herdr",
    "test": "TestHerdrConformance",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
    "reason": "conformance_test.go:18: herdr not installed",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/herdr",
    "test": "TestProviderLive",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
    "reason": "provider_live_test.go:15: herdr not installed",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/herdr",
    "test": "TestProviderLive",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
    "reason": "provider_live_test.go:15: herdr not installed",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/herdr",
    "test": "TestProviderLiveClaudeKindPath",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
    "reason": "kindpath_live_test.go:105: herdr not installed",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/herdr",
    "test": "TestProviderLiveClaudeKindPath",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
    "reason": "kindpath_live_test.go:105: herdr not installed",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/herdr",
    "test": "TestProviderLiveOccupantSwapKeepsLiveness",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
    "reason": "panebinding_live_test.go:21: herdr not installed",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/herdr",
    "test": "TestProviderLiveOccupantSwapKeepsLiveness",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
    "reason": "panebinding_live_test.go:21: herdr not installed",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/herdr",
    "test": "TestSessionEventsLive",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
    "reason": "events_live_test.go:40: herdr not installed",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/herdr",
    "test": "TestSessionEventsLive",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-d920ddbd08781200929a.log",
    "reason": "events_live_test.go:40: herdr not installed",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunLifecycleTestsDoesNotStopUnownedConcurrentStart/suite/Start_ConcurrentDistinctSessions",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:124: concurrent Start(preexisting-concurrent-session): session already exists",
    "class": "other"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/FencedInput_AbsentSessionRefuses",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:551: provider cannot fence session input",
    "class": "other"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/FencedInput_MatchingFenceDelivers",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:508: provider cannot fence session input",
    "class": "other"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/FencedInput_MismatchedFenceRefusesEveryInput",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:525: provider cannot fence session input",
    "class": "other"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Interrupt_ConcurrentDistinctSessions",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:238: Start(skip-start-14): provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/IsRunning_ConcurrentDistinctSessions",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:271: Start(skip-start-18): provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ConcurrentDistinctPrefixes",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:367: Start(skip-start-25): provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_EmptyPrefix",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:349: Start: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ExcludesStopped",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:333: Start: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_FindsSessions",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:294: Start skip-start-21: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_PrefixFiltering",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:313: Start skip-start-22: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_ConcurrentDistinctSessions",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:421: Start(skip-start-30): provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_EmptyNamesReturnsTrue",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:395: Start: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_FalseAfterStop",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:404: Start: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/SharedSession",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:61: Start shared session: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_ConcurrentDistinctSessions",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:124: concurrent Start(skip-start-3): provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_CreatesRunningSession",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:84: Start: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_DuplicateReturnsError",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:93: first Start: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_ConcurrentDistinctSessions",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:209: Start(skip-start-11): provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_AlreadyStopped",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:193: Start: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_NotRunning",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:170: Start sentinel: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_MakesSessionNotRunning",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance.go:134: Start: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestStartOrSkipDoesNotStopSessionAfterClassifiedStartFailure/classified_failure",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
    "reason": "conformance_test.go:147: Start: session already exists",
    "class": "other"
  },
  {
    "package": "internal/runtime/ssh",
    "test": "TestConn_ExecOverRealLocalhost",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-60ebdfafc6cd70feb3b1.log",
    "reason": "ssh_localhost_integration_test.go:24: passwordless ssh to localhost unavailable",
    "class": "other"
  },
  {
    "package": "internal/runtime/ssh",
    "test": "TestSSHConformance/FencedInput_AbsentSessionRefuses",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-60ebdfafc6cd70feb3b1.log",
    "reason": "conformance.go:551: provider cannot fence session input",
    "class": "other"
  },
  {
    "package": "internal/runtime/ssh",
    "test": "TestSSHConformance/FencedInput_MatchingFenceDelivers",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-60ebdfafc6cd70feb3b1.log",
    "reason": "conformance.go:508: provider cannot fence session input",
    "class": "other"
  },
  {
    "package": "internal/runtime/ssh",
    "test": "TestSSHConformance/FencedInput_MismatchedFenceRefusesEveryInput",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-60ebdfafc6cd70feb3b1.log",
    "reason": "conformance.go:525: provider cannot fence session input",
    "class": "other"
  },
  {
    "package": "internal/runtime/t3bridge",
    "test": "TestLookupDeliveryErrors/seam-backed/start_grace",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-a23a357eedd1739a53e0.log",
    "reason": "delivery_lookup_test.go:133: start grace is raw-bridge bookkeeping",
    "class": "other"
  },
  {
    "package": "internal/runtime/tmux",
    "test": "TestCleanupOrphanedSessions",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
    "reason": "tmux_test.go:1615: Skipping: GT_TEST_ALLOW_CLEANUP_TEST=1 required (this test kills sessions)",
    "class": "other"
  },
  {
    "package": "internal/runtime/tmux",
    "test": "TestCleanupOrphanedSessions_NoSessions",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
    "reason": "tmux_test.go:1713: Skipping: GT_TEST_ALLOW_CLEANUP_TEST=1 required (this test kills sessions)",
    "class": "other"
  },
  {
    "package": "internal/runtime/tmux",
    "test": "TestGetKeyBinding_CapturesUserBinding",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
    "reason": "tmux_test.go:3663: not inside tmux \u2014 need server for bind-key",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/tmux",
    "test": "TestGetKeyBinding_SkipsGasTownBindings",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
    "reason": "tmux_test.go:3637: not inside tmux \u2014 need server for bind-key",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/tmux",
    "test": "TestIsGTBinding_DetectsGasTownBindings",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-15c091b746137e5c2921.log",
    "reason": "tmux_test.go:3689: not inside tmux \u2014 need server for bind-key",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/tmux",
    "test": "TestNudgeConfirmBudgetThreshold",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
    "reason": "nudge_confirm_lag_integration_test.go:174: set GC_TMUX_INTEGRATION=1 to run this real-tmux measurement",
    "class": "other"
  },
  {
    "package": "internal/runtime/tmux",
    "test": "TestNudgeConfirmBusyRenderLag",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-15c091b746137e5c2921.log",
    "reason": "nudge_confirm_lag_integration_test.go:36: set GC_TMUX_INTEGRATION=1 to run this real-tmux reproduction (spins a throwaway tmux server)",
    "class": "other"
  },
  {
    "package": "internal/runtime/tmux",
    "test": "TestNudgePokeRealTmux",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
    "reason": "nudge_poke_integration_test.go:27: set GC_TMUX_INTEGRATION=1 to run this real-tmux dogfood (spins a throwaway tmux server)",
    "class": "other"
  },
  {
    "package": "internal/runtime/tmux",
    "test": "TestNudgeRecoversStagedDraftSwallowedByPasteIngest",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
    "reason": "nudge_staged_draft_integration_test.go:26: set GC_TMUX_INTEGRATION=1 to run this real-tmux reproduction (spins a throwaway tmux server)",
    "class": "other"
  },
  {
    "package": "internal/runtime/tmux",
    "test": "TestPokeActivityRealTmux",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
    "reason": "activity_poke_integration_test.go:21: set GC_TMUX_INTEGRATION=1 to run this real-tmux dogfood (spins a throwaway tmux server)",
    "class": "other"
  },
  {
    "package": "internal/runtime/tmux",
    "test": "TestPokeActivityRealTmux",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ef2749222a71266756ae.log",
    "reason": "activity_poke_integration_test.go:21: set GC_TMUX_INTEGRATION=1 to run this real-tmux dogfood (spins a throwaway tmux server)",
    "class": "other"
  },
  {
    "package": "internal/runtime/tmux",
    "test": "TestProcessSnapshotPSArgsRejectsLinuxSyntaxOnDarwin",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
    "reason": "state_cache_test.go:926: Darwin-specific syntax guard",
    "class": "other"
  },
  {
    "package": "internal/runtime/tmux",
    "test": "TestProcessSnapshotPSArgsRejectsLinuxSyntaxOnDarwin",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-ef2749222a71266756ae.log",
    "reason": "state_cache_test.go:926: Darwin-specific syntax guard",
    "class": "other"
  },
  {
    "package": "internal/runtime/tmux",
    "test": "TestSetBindings_PreserveFallbackOnRepeatedCalls",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
    "reason": "tmux_test.go:3719: not inside tmux \u2014 need server for bind-key",
    "class": "environment-absence"
  },
  {
    "package": "internal/sessionlog",
    "test": "TestFindClaudeLatestSessionFileUsesProjectPathAlias",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
    "reason": "sessionlog_test.go:1397: macOS-only /tmp <-> /private/tmp Claude project path alias",
    "class": "other"
  },
  {
    "package": "internal/sessionlog",
    "test": "TestFindCodexSessionFileMatchesEquivalentResolvedWorkDir",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
    "reason": "sessionlog_test.go:2170: macOS /private/tmp path aliases only apply on darwin",
    "class": "other"
  },
  {
    "package": "internal/sessionlog",
    "test": "TestFindGeminiSessionFileMatchesEquivalentResolvedWorkDir",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
    "reason": "sessionlog_test.go:2315: macOS-only /tmp <-> /private/tmp Gemini project path alias",
    "class": "other"
  },
  {
    "package": "internal/sessionlog",
    "test": "TestFindSessionFileByIDPrefersStoredWorkDirSpelling",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
    "reason": "sessionlog_test.go:1104: macOS-only /tmp <-> /private/tmp Claude project path alias",
    "class": "other"
  },
  {
    "package": "internal/sessionlog",
    "test": "TestFindSessionFileByIDUsesClaudeProjectPathAlias",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
    "reason": "sessionlog_test.go:1084: macOS-only /tmp <-> /private/tmp Claude project path alias",
    "class": "other"
  },
  {
    "package": "internal/sessionlog",
    "test": "TestFindSessionFileUsesClaudeProjectPathAlias",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
    "reason": "sessionlog_test.go:1195: macOS-only /tmp <-> /private/tmp Claude project path alias",
    "class": "other"
  },
  {
    "package": "internal/sessionlog",
    "test": "TestFindSessionFileUsesNewestClaudeProjectPathAliasMatch",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-eb757c3339d13fe4ad40.log",
    "reason": "sessionlog_test.go:1215: macOS-only /tmp <-> /private/tmp Claude project path alias",
    "class": "other"
  },
  {
    "package": "internal/worker/transcript",
    "test": "TestDiscoverFallbackPathUsesNewestClaudeLatestSessionAcrossAliases",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5536c7e35ba99d4d52dd.log",
    "reason": "discovery_test.go:65: macOS-only /tmp <-> /private/tmp Claude project path alias",
    "class": "other"
  },
  {
    "package": "internal/worker/workertest",
    "test": "TestPhase1Conformance/antigravity/tmux-cli/WC-TX-USAGE-001",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
    "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
    "class": "other"
  },
  {
    "package": "internal/worker/workertest",
    "test": "TestPhase1Conformance/antigravity/tmux-cli/WC-USAGE-COST-001",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
    "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
    "class": "other"
  },
  {
    "package": "internal/worker/workertest",
    "test": "TestPhase1Conformance/cursor/tmux-cli/WC-TX-USAGE-001",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
    "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
    "class": "other"
  },
  {
    "package": "internal/worker/workertest",
    "test": "TestPhase1Conformance/cursor/tmux-cli/WC-USAGE-COST-001",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
    "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
    "class": "other"
  },
  {
    "package": "internal/worker/workertest",
    "test": "TestPhase1Conformance/gemini/tmux-cli/WC-TX-USAGE-001",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
    "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
    "class": "other"
  },
  {
    "package": "internal/worker/workertest",
    "test": "TestPhase1Conformance/gemini/tmux-cli/WC-USAGE-COST-001",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
    "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
    "class": "other"
  },
  {
    "package": "internal/worker/workertest",
    "test": "TestPhase1Conformance/kimi/tmux-cli/WC-TX-USAGE-001",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
    "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
    "class": "other"
  },
  {
    "package": "internal/worker/workertest",
    "test": "TestPhase1Conformance/kimi/tmux-cli/WC-USAGE-COST-001",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
    "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
    "class": "other"
  },
  {
    "package": "internal/worker/workertest",
    "test": "TestPhase1Conformance/mimocode/tmux-cli/WC-TX-USAGE-001",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
    "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
    "class": "other"
  },
  {
    "package": "internal/worker/workertest",
    "test": "TestPhase1Conformance/mimocode/tmux-cli/WC-USAGE-COST-001",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
    "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
    "class": "other"
  },
  {
    "package": "internal/worker/workertest",
    "test": "TestPhase1Conformance/pi/tmux-cli/WC-TX-USAGE-001",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
    "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
    "class": "other"
  },
  {
    "package": "internal/worker/workertest",
    "test": "TestPhase1Conformance/pi/tmux-cli/WC-USAGE-COST-001",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
    "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
    "class": "other"
  },
  {
    "package": "internal/worker/workertest",
    "test": "TestPhase1Conformance/zcode/tmux-cli/WC-TX-USAGE-001",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
    "reason": "phase1_conformance_test.go:100: provider family \"\" has no invocation-usage extractor (out of scope)",
    "class": "other"
  },
  {
    "package": "internal/worker/workertest",
    "test": "TestPhase1Conformance/zcode/tmux-cli/WC-USAGE-COST-001",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
    "reason": "phase1_conformance_test.go:110: provider family \"\" has no invocation-usage extractor (out of scope)",
    "class": "other"
  },
  {
    "package": "internal/worker/workertest",
    "test": "TestStructuredConformance/gemini/tmux-cli/WC-STRUCT-003",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-8a98a003e4fc4bb087ab.log",
    "reason": "structured_conformance_test.go:153: profile fixture has no edit result to evaluate",
    "class": "other"
  },
  {
    "package": "internal/workspacesvc",
    "test": "TestProxyProcessHardExitHarness",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a7df874ab90d74e027a.log",
    "reason": "proxy_process_test.go:1160: harness process",
    "class": "other"
  },
  {
    "package": "internal/workspacesvc",
    "test": "TestProxyProcessHelper",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-5a7df874ab90d74e027a.log",
    "reason": "proxy_process_test.go:167: helper process",
    "class": "other"
  },
  {
    "package": "test/acceptance",
    "test": "TestBeadsInitTopologyMatrix/M5-legacy-gc-managed",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-f785906030efd99733e1.log",
    "reason": "beads_topology.go:637: there is no pre-journal gc binary; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc built from a commit before the ownership journal",
    "class": "other"
  },
  {
    "package": "test/acceptance",
    "test": "TestBeadsInitTopologyMatrix/M5-legacy-gc-managed",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-2129301bc9bf3acdcdf7.log",
    "reason": "beads_topology.go:637: there is no pre-journal gc binary; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc built from a commit before the ownership journal",
    "class": "other"
  },
  {
    "package": "test/acceptance",
    "test": "TestBeadsInitTopologyMatrix/M5-legacy-gc-managed",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-00f787a1201c0c97f5dd.log",
    "reason": "beads_topology.go:637: there is no pre-journal gc binary; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc built from a commit before the ownership journal",
    "class": "other"
  },
  {
    "package": "test/acceptance",
    "test": "TestBeadsMigrateLegacyCityToProxied",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-2b4464fe973ad362994d.log",
    "reason": "beads_migrate_proxied_test.go:135: there is no legacy gc available; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc that still initialises the GC-managed direct topology",
    "class": "other"
  },
  {
    "package": "test/acceptance",
    "test": "TestBeadsMigrateProxiedRefusesLiveLegacyServer",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-a4ee7fceff2c46ffacdd.log",
    "reason": "beads_migrate_proxied_test.go:357: there is no legacy gc available; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc that still initialises the GC-managed direct topology",
    "class": "other"
  },
  {
    "package": "test/acceptance",
    "test": "TestManagedDirectSessionBdFollowsMovedDoltPort",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-09e792a237f1c684f6da.log",
    "reason": "managed_session_dolt_port_move_test.go:45: there is no pre-journal gc binary; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc built from a commit before the ownership journal",
    "class": "other"
  },
  {
    "package": "test/acceptance",
    "test": "TestPackRegistryLiveImportsEveryCatalogPack",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-09e792a237f1c684f6da.log",
    "reason": "pack_registry_live_test.go:38: set GC_TEST_GASCITY_PACKS_REGISTRY to a gascity-packs registry.toml source to run this live import smoke test",
    "class": "other"
  },
  {
    "package": "test/acceptance",
    "test": "TestSelfhostUX_BinaryDriftPostMergePlaceholder",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-56dbf54bd097e3e21771.log",
    "reason": "integration_selfhost_ux_test.go:112: placeholder for ga-xxqx",
    "class": "other"
  },
  {
    "package": "test/acceptance",
    "test": "TestSelfhostUX_BinaryDriftRestart",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-fedaecec6af3ef2e4157.log",
    "reason": "integration_selfhost_ux_test.go:54: pending merge of builder/ga-xxqx-1 (PR#2219): drift auto-restart not on origin/main",
    "class": "other"
  },
  {
    "package": "test/acceptance",
    "test": "TestSelfhostUX_Composed",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-5ed75e6deb5279ca2b84.log",
    "reason": "integration_selfhost_ux_test.go:96: placeholder for ga-q0bf.1, ga-r8iz.1, ga-6wrr.1, and post-PR#2219 assertions",
    "class": "other"
  },
  {
    "package": "test/acceptance",
    "test": "TestSelfhostUX_MigrationGuideURLPlaceholder",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-61bd7bf3b62120f8de67.log",
    "reason": "integration_selfhost_ux_test.go:108: placeholder for ga-6wrr.1",
    "class": "other"
  },
  {
    "package": "test/acceptance",
    "test": "TestSelfhostUX_PackV1V2Collision",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-fd5aed82a8b7ea468264.log",
    "reason": "integration_selfhost_ux_test.go:16: pending ga-cwdaqv: supervised gc start does not validate config pre-registration",
    "class": "other"
  },
  {
    "package": "test/acceptance",
    "test": "TestSelfhostUX_StopBypassDetailsPlaceholder",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-b54423ef39aa7297e2c2.log",
    "reason": "integration_selfhost_ux_test.go:104: placeholder for ga-r8iz.1",
    "class": "other"
  },
  {
    "package": "test/acceptance",
    "test": "TestSelfhostUX_StructuredStartSummaryPlaceholder",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/acceptance-53f66fa06e91bd4169e5.log",
    "reason": "integration_selfhost_ux_test.go:100: placeholder for ga-q0bf.1",
    "class": "other"
  },
  {
    "package": "test/acceptance/helpers",
    "test": "TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
    "reason": "bd_schema_parity_test.go:121: bd not available",
    "class": "environment-absence"
  },
  {
    "package": "test/acceptance/helpers",
    "test": "TestBdLatestSchemaVersionLeavesNothingInItsTempRoot",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
    "reason": "bd_schema_parity_test.go:144: bd not available",
    "class": "environment-absence"
  },
  {
    "package": "test/acceptance/helpers",
    "test": "TestRealBdProvisionLeavesTheHostHomeAlone",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-2af37a65322c095289c3.log",
    "reason": "tool_home_test.go:317: no bd available (set GC_ACCEPTANCE_BD_BIN)",
    "class": "environment-absence"
  },
  {
    "package": "test/containerhost",
    "test": "TestK8sSessionConformance/FencedInput_AbsentSessionRefuses",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-afcc08552b3653c0e0f1.log",
    "reason": "conformance.go:551: provider cannot fence session input",
    "class": "other"
  },
  {
    "package": "test/containerhost",
    "test": "TestK8sSessionConformance/FencedInput_MatchingFenceDelivers",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-afcc08552b3653c0e0f1.log",
    "reason": "conformance.go:508: provider cannot fence session input",
    "class": "other"
  },
  {
    "package": "test/containerhost",
    "test": "TestK8sSessionConformance/FencedInput_MismatchedFenceRefusesEveryInput",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-afcc08552b3653c0e0f1.log",
    "reason": "conformance.go:525: provider cannot fence session input",
    "class": "other"
  },
  {
    "package": "test/conversationlane",
    "test": "TestX6FanIn/full",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-packages-8ecc4094e08f9bfd6d51.log",
    "reason": "x6_test.go:286: DES-O1's literal X6 line (5,000 notices in 60 s) is a manual measurement: --test_env=GC_LANE_X6_FULL=1",
    "class": "other"
  },
  {
    "package": "test/integration",
    "test": "TestBdStoreConformance",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-d0691e3dec42dbb78fd3.log",
    "reason": "bdstore_test.go:54: BdStore fails ParentIDNamesARowThisStoreDoesNotHave: bd refuses an unresolvable --parent (ga-6od57); un-skip is ga-x4j5ka",
    "class": "other"
  },
  {
    "package": "test/integration",
    "test": "TestDoltPersistence_AssigneeAndInProgressSurviveSubsequentBdWrite",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-5bb45aeaf433c2072f8e.log",
    "reason": "dolt_persistence_test.go:536: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
    "class": "other"
  },
  {
    "package": "test/integration",
    "test": "TestDoltPersistence_CloseStatusSurvivesSubsequentBdWrite",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-23490fe556f84649266a.log",
    "reason": "dolt_persistence_test.go:449: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
    "class": "other"
  },
  {
    "package": "test/integration",
    "test": "TestDoltPersistence_ConcurrentMetadataWriteBurstTimingVisibility",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-d0691e3dec42dbb78fd3.log",
    "reason": "dolt_persistence_test.go:818: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
    "class": "other"
  },
  {
    "package": "test/integration",
    "test": "TestDoltPersistence_CrossBeadMetadataWritesPersistInDolt",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-35aa679f85944e59e6b9.log",
    "reason": "dolt_persistence_test.go:766: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
    "class": "other"
  },
  {
    "package": "test/integration",
    "test": "TestDoltPersistence_HandoffRoutingMetadataSurvivesSubsequentBdWrite",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-8e315f95edb09e5631cb.log",
    "reason": "dolt_persistence_test.go:636: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
    "class": "other"
  },
  {
    "package": "test/integration",
    "test": "TestDoltPersistence_InProgressStatusSurvivesSubsequentBdWrite",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-d41f611925a4b41fb744.log",
    "reason": "dolt_persistence_test.go:679: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
    "class": "other"
  },
  {
    "package": "test/integration",
    "test": "TestDoltPersistence_MetadataSurvivesSubsequentBdWrites",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-82ff0c4322b44203e167.log",
    "reason": "dolt_persistence_test.go:490: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
    "class": "other"
  },
  {
    "package": "test/integration",
    "test": "TestDoltPersistence_SequentialMetadataWritesAccumulateInDolt",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-b2288f9e45f57dd8ece2.log",
    "reason": "dolt_persistence_test.go:722: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
    "class": "other"
  },
  {
    "package": "test/integration",
    "test": "TestDoltPersistence_SessionAwakeMetadataSurvivesSubsequentBdWrite",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-65412ec6e874f54aa912.log",
    "reason": "dolt_persistence_test.go:585: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337",
    "class": "other"
  },
  {
    "package": "test/integration",
    "test": "TestHumaBinary_CityUnregisterAsync",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-d0691e3dec42dbb78fd3.log",
    "reason": "huma_binary_test.go:508: flaky on CI under load \u2014 5s supervisor shutdown budget races with in-flight mayor-session-start; tracked in #2090",
    "class": "other"
  },
  {
    "package": "test/integration",
    "test": "TestReadProcessSnapshotFallsBackToPSAndFindsRealProcesses",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-d41f611925a4b41fb744.log",
    "reason": "integration_test.go:2841: host has /proc; this test targets the no-/proc (macOS) fallback path",
    "class": "other"
  },
  {
    "package": "test/integration",
    "test": "TestStartDrift_PermissionDenied_DescriptiveError",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-8e315f95edb09e5631cb.log",
    "reason": "start_drift_test.go:275: permission-denied scenario requires root to drop to a secondary uid",
    "class": "other"
  },
  {
    "package": "test/integration",
    "test": "TestStartDrift_SystemdManaged_RestartsToNewBuildID",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/integration-full-23490fe556f84649266a.log",
    "reason": "start_drift_test.go:123: XDG_RUNTIME_DIR not set; user systemd not addressable",
    "class": "environment-absence"
  },
  {
    "package": "test/tmuxtest",
    "test": "TestHelperHoldSentinel",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/logs/unit-7989cb1ae7bd292f8cbf.log",
    "reason": "orphan_sweep_sigkill_test.go:29: not invoked as a subprocess helper",
    "class": "other"
  }
]
```

```json
[
  {
    "package": "cmd/gc",
    "test": "TestReplay",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-0.log",
    "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
    "class": "environment-absence"
  },
  {
    "package": "cmd/gc",
    "test": "TestSessionEventPumpLiveHerdr",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-0.log",
    "reason": "session_event_pump_live_test.go:23: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`",
    "class": "other"
  },
  {
    "package": "cmd/gc",
    "test": "TestSimCoverageGate",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-0.log",
    "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
    "class": "environment-absence"
  },
  {
    "package": "internal/beads",
    "test": "TestMetadataEmptyStringClearContract/Postgres/BatchClearsOneKeyKeepsAnother",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-2.log",
    "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
    "class": "environment-absence"
  },
  {
    "package": "internal/beads",
    "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-2.log",
    "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
    "class": "environment-absence"
  },
  {
    "package": "internal/beads",
    "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-2.log",
    "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
    "class": "environment-absence"
  },
  {
    "package": "internal/beads",
    "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-2.log",
    "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
    "class": "environment-absence"
  },
  {
    "package": "internal/beads/exec",
    "test": "TestBrProviderConformance",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-5.log",
    "reason": "br_test.go:18: br not available",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/herdr",
    "test": "TestActivityLive",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-8.log",
    "reason": "activity_live_test.go:20: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`",
    "class": "other"
  },
  {
    "package": "internal/runtime/herdr",
    "test": "TestHerdrConformance",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-8.log",
    "reason": "conformance_test.go:18: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`",
    "class": "other"
  },
  {
    "package": "internal/runtime/herdr",
    "test": "TestProviderLive",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-8.log",
    "reason": "provider_live_test.go:15: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`",
    "class": "other"
  },
  {
    "package": "internal/runtime/herdr",
    "test": "TestProviderLiveClaudeKindPath",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-8.log",
    "reason": "kindpath_live_test.go:105: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`",
    "class": "other"
  },
  {
    "package": "internal/runtime/herdr",
    "test": "TestProviderLiveOccupantSwapKeepsLiveness",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-8.log",
    "reason": "panebinding_live_test.go:21: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`",
    "class": "other"
  },
  {
    "package": "internal/runtime/herdr",
    "test": "TestSessionEventsLive",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-8.log",
    "reason": "events_live_test.go:40: skipping live herdr journey in unit lane; set GC_FAST_UNIT=0 or GC_HERDR_LIVE_TESTS=1, or run `make test-herdr-live`",
    "class": "other"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/FencedInput_AbsentSessionRefuses",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:551: provider cannot fence session input",
    "class": "other"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/FencedInput_MatchingFenceDelivers",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:508: provider cannot fence session input",
    "class": "other"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/FencedInput_MismatchedFenceRefusesEveryInput",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:525: provider cannot fence session input",
    "class": "other"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Interrupt_ConcurrentDistinctSessions",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:238: Start(skip-start-14): provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/IsRunning_ConcurrentDistinctSessions",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:271: Start(skip-start-18): provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ConcurrentDistinctPrefixes",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:367: Start(skip-start-25): provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_EmptyPrefix",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:349: Start: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ExcludesStopped",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:333: Start: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_FindsSessions",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:294: Start skip-start-21: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_PrefixFiltering",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:313: Start skip-start-22: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_ConcurrentDistinctSessions",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:421: Start(skip-start-30): provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_EmptyNamesReturnsTrue",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:395: Start: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_FalseAfterStop",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:404: Start: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/SharedSession",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:61: Start shared session: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_ConcurrentDistinctSessions",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:124: concurrent Start(skip-start-3): provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_CreatesRunningSession",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:84: Start: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_DuplicateReturnsError",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:93: first Start: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_ConcurrentDistinctSessions",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:209: Start(skip-start-11): provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_AlreadyStopped",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:193: Start: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_NotRunning",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:170: Start sentinel: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/runtimetest",
    "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_MakesSessionNotRunning",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-9.log",
    "reason": "conformance.go:134: Start: provider environment unavailable",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/tmux",
    "test": "TestGetKeyBinding_CapturesUserBinding",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-10.log",
    "reason": "tmux_test.go:3663: not inside tmux \u2014 need server for bind-key",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/tmux",
    "test": "TestGetKeyBinding_SkipsGasTownBindings",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-10.log",
    "reason": "tmux_test.go:3637: not inside tmux \u2014 need server for bind-key",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/tmux",
    "test": "TestIsGTBinding_DetectsGasTownBindings",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-10.log",
    "reason": "tmux_test.go:3689: not inside tmux \u2014 need server for bind-key",
    "class": "environment-absence"
  },
  {
    "package": "internal/runtime/tmux",
    "test": "TestSetBindings_PreserveFallbackOnRepeatedCalls",
    "status": "SKIP",
    "log": "/var/tmp/deploy-ga-wb0ttep.xa23xylo/c3/evidence/remote/floor-logs/floor-10.log",
    "reason": "tmux_test.go:3719: not inside tmux \u2014 need server for bind-key",
    "class": "environment-absence"
  }
]
```

