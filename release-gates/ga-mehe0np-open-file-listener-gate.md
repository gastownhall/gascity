# Open-file listener fixture release gate

**Verdict:** **PASS**

Evaluated 2026-10-24T19:19:24.380336+00:00.
Deploy: ga-mehe0np. Build: ga-bd8cn1x. Review: ga-9b3xk19.
Reviewed source: 945b8ad426ea6c6153f7a8a44b5371a8c6439294.
Canonical merge: f4155af8a84860b5c82a082d00ac6a26a318ae6d; tree: 15797180c0a992058c3f0cf6faf4cb0aad772349.
merge_state: clean (origin/main@0e82a42ba3ef3cd53225637b1fbc94cfd8939b7a)
gate_base_kind: current-tip
gate_base: 0e82a42ba3ef3cd53225637b1fbc94cfd8939b7a
merge_validation: merge result built and vetted here; subsequent merge validation belongs to MPR/GitHub's merge queue.
Evidence retained at /var/tmp/deploy-ga-mehe0np-fresh.4kr1Sqty.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Reviewer PASS on resolved reviewed source; no rebase, carryover or repin. |
| 2 | PASS | Child binds loopback port 0 and reports its selected port; parent closes its pipe write end and includes exit status/stderr on EOF. All three affected callers and the new child-exit regression pass by name in the full unit and integration-package runs. Historical investigation established EADDRINUSE from another worker slot, with 24 failures in 38,400 pre-fix remote iterations; fixture code and independent suite exercise the test-only remedy. |
| 3 | PASS with attribution | All five farm phases ran once, followed by all 16 native floor steps. Raw 126125 PASS/4 FAIL/370 SKIP result lines: four failed lines represent three independently attributed conditions below. All four diff-owned tests PASS; none skips or fails. |
| 4 | PASS | Reviewer style/security/spec verdicts have no open HIGH or other blocker. |
| 5 | PASS | Reviewed tree clean. The isolated deploy branch adds only this gate record; post-commit clean status is checked before push. Other scratch gate reports are preserved separately. |
| 6 | PASS | Clean canonical merge, exact expected merge tree; normal staged hook, explicit all-target Bazel build, full nogo/vet and check-hooks passed. |
| 7 | PASS | One test-fixture listener/diagnosis theme; only cmd/gc/cmd_dolt_state_test.go and its resource-census count change. Ancestry scope check passed, no stack or denied paths. |

Criteria source: supplied release-gate protocol and TESTING.md/Makefile/Bazel CI. docs/PROJECT_MANIFEST.md is absent.

fast_lane: PASS 2026-10-24T18:02:04Z, before runtime setup and suite.
policy_lane: PASS — pinned formatting, full nogo/vet, four shell guards and 10/10 policy targets.
drift_lane: PASS — make bazel-sync plus empty git diff; 7/7 generated-artifact targets. Each lane used a fresh view and the frozen base.

```text
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-mehe0np-fresh.4kr1Sqty/merge head=f4155af8a848 tree=15797180c0a9 dir=/var/tmp/gc-fresh-tree.sSTJEj modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=0e82a42ba3ef3cd53225637b1fbc94cfd8939b7a knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.sSTJEj
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-mehe0np-fresh.4kr1Sqty/merge head=f4155af8a848 tree=15797180c0a9 dir=/var/tmp/gc-fresh-tree.avCp6M modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-mehe0np-fresh.4kr1Sqty/merge head=f4155af8a848 tree=15797180c0a9 dir=/var/tmp/gc-fresh-tree.1HeSRq modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
```

Coverage: unit 286/287 targets PASS with one attributed API test target failure; acceptance 7/7 and integration-packages 36/36 PASS; full integration 13/14 shards PASS with one attributed fixture failure; latency phase completed with bd-budget PASS and one attributed prime-hook budget failure under a clear probe. All 497 results are remote, uncached, first attempt, terminal and inside their phase windows. All 994 raw artifacts and 1010 retained artifacts/floor logs were SHA256 verified. No farm phase was rerun, and no farm/local full-suite stitching occurred.

The native floor completed all 16 checks, including beadstest, Darwin compile, runner self-tests and every environment-absence package selected by the canonical inventory. Its Go result lines are 21 PASS/0 FAIL/14 SKIP.

## Failure attribution

- API structured-promotion test -> ga-a36cdy4, proof (a). Official site line1212 UNCHANGED/shared_hunks0; the fresh API test-binary closure has 115 module directories, none containing a changed non-test input. No package overlap. Tracker predates this run; designated ga-ieeks88 is stamped and open/unimplemented. Own build ga-bd8cn1x is stamped ga-eyeue4p. Census fixed_sleep decreases 9 to 8; no new target or file. Same-package helper hunks affect only the four owned fixture tests, which all PASS. Proof api-failure-proof.json; tracker sighting read back.
- Active-step read probe -> ga-od4rcxy, proof (d). Official site1512 UNCHANGED/shared_hunks0. Positive pristine-base reproduction log was re-read and SHA256 verified; both default-query construction and required-assignee declaration match byte-for-byte across that proven failing base, the frozen base and this merge. External gc edge explicitly checked: its closure conservatively includes the census's enclosing root, so an import-only no-match is NOT the causal proof. No package overlap or added resource/target/file load. Designated stamped ga-ze0aym0 is closed/blocked and its resolved commit is not on the recorded base. Proof integration-failure-proof.json; tracker sighting read back.
- Seat-hook latency -> ga-bqsrp4o, proof (b). Prime fastest1294ms exceeded the unchanged1000ms budget after a clear probe (load1=2.39, load5=1.75, load15=.78, waited0). Actual retained gate evidence for our own PR8236 reproduces the same prime budget miss1210ms under a clear probe on a disjoint baseline-only diff; source ranges, two artifact hashes and exact assertion were independently verified. This is measured gate-record comparison, not a claim about current gh-pr-checks. Site180 UNCHANGED/shared_hunks0; no package overlap or test-load guard trip. Stamped designated ga-yu01v6x remains blocked/unlanded on the recorded base. Proof latency-failure-proof.json; tracker sighting read back.

All three repeat-hit attributions use the standing fix-carrying exception and retain clauses1-4. No waiver, limit change or candidate retry. Original execution.json remains exit1/criterion3_valid=false; original latency-leg.json remains failed/complete=false. Guarded continuations executed only the previously unrun phases/floor, with completed-phase hashes unchanged. gate-final-completion.json and final-evidence-audit.json separately record completion and attribution; raw verdicts were never rewritten.

## Skips and environment

Farm inventory: 356 entries (71 environment-absence entries selected for native top-up; 285 platform/provider/opt-in exclusions). Native inventory: 39 entries; the 14 top-level native skips remain non-owned platform, optional-provider/tool or opt-in exclusions. Every printed reason maps to a source site: 280 unique sites independently checked UNCHANGED, zero unresolved rows and zero touched sites. Full reasons/site checks are in skip-sites-evidence.txt and the two inventories. The changed cmd/gc shared hunks are bufio/errors imports and listener helpers used only by the three changed callers/new regression, all PASS; they cannot reach other tests' unchanged skip branches. No diff-owned SKIP.

ICU74 check OK with recorded provenance; rootless podman socket active and cached dolt-sql-server tag matches deps.env (2.2.0); pinned test bd1.3.1. Remote admission used25GiB floor/cap2, no host-load admission wait. Native soft load wait timed out per policy, then ran once and passed; no remote failure is attributed to host load.

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=115.69 run_max_load=144.64 run_mean_load=129.97 run_readings=11 wait_first_load=127.94 wait_max_load=127.94 wait_mean_load=107.02 wait_readings=61 read_errors=0
```

## Machine-readable evidence

```json
{
  "test_cmd_scope": "full-suite",
  "test_exec_mode": "remote-exec",
  "test_exec_bep": {
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
  },
  "test_counts": {
    "PASS": 126125,
    "FAIL": 4,
    "SKIP": 370
  },
  "farm_test_seconds": 10022.259,
  "rbe_state": {
    "rc_marker_present": true,
    "kill_switch_absent": true,
    "cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT",
    "worker_env_tree": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45",
    "worker_env_origin_main": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45"
  },
  "fallback_reason": null,
  "admission": {
    "class": "remote",
    "floor_gib": 25.0,
    "free_gib": 38.23452377319336,
    "waited_seconds": 0.001,
    "cap": 2
  },
  "skip_inventory": {
    "farm_entries": 356,
    "native_entries": 39,
    "unchanged_sites": 280,
    "unresolved_rows": 0,
    "farm_record": "/var/tmp/deploy-ga-mehe0np-fresh.4kr1Sqty/c3/evidence/remote/skip-inventory.json",
    "native_record": "/var/tmp/deploy-ga-mehe0np-fresh.4kr1Sqty/native-skip-inventory.json"
  },
  "owned_topup": [],
  "floor_topup": {
    "complete": true,
    "steps": 16,
    "native_counts": {
      "PASS": 21,
      "FAIL": 0,
      "SKIP": 14
    },
    "record": "/var/tmp/deploy-ga-mehe0np-fresh.4kr1Sqty/c3/evidence/remote/floor-topup.json"
  },
  "latency_leg": {
    "complete": false,
    "phase": "integration-latency",
    "tests": [
      "TestGcBdLatencyBudget",
      "TestGcSeatHookLatencyBudget"
    ],
    "attempts_allowed": 2,
    "attempts": [
      {
        "attempt": 1,
        "rc": 1,
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
            "FAIL": 1
          }
        },
        "farm_test_seconds": 133.374,
        "evidence_errors": [
          "test verdict FAILED",
          "summary verdict FAILED",
          "build not successfully finished"
        ],
        "probe": [
          "GATE_FARM_PROBE verdict=clear waited=0s load1=2.39 load5=1.75 load15=0.78 cpus=32 threshold=50 max_wait=600s"
        ],
        "test": [
          "stores: city proxied-server, rig proxied-server; cpus 32; loadavg 5.66 2.57 1.08 10/1328 48023"
        ],
        "stdout_tail": [
          "Target //test/integration:integration_test up-to-date:",
          "  bazel-bin/test/integration/integration_test_/integration_test",
          "INFO: Elapsed time: 161.826s, Critical Path: 139.89s",
          "INFO: 2 processes: 3077 action cache hit, 1 internal, 1 remote.",
          "INFO: Build completed, 1 test FAILED, 2 total actions",
          "INFO: ",
          "//test/integration:integration_test                                      FAILED in 133.4s",
          "  /home/jaword/.cache/bazel/_bazel_jaword/4bdaff22d7beafddd55dd80ea249051b/execroot/_main/bazel-out/k8-fastbuild/testlogs/test/integration/integration_test/test.log",
          "Executed 1 out of 1 test: 1 fails remotely.",
          "INFO: Build Event Protocol files produced successfully.",
          "{\"test_exec_mode\": \"remote-exec\", \"test_exec_bep\": {\"results\": 1, \"remote_executed\": 1, \"cached_remotely\": 0, \"cached_locally\": 0, \"other_strategy\": 0, \"retried\": 0, \"window_ok\": true}, \"farm_test_seconds\": 133.374, \"verdicts\": 1, \"transport_failure\": false, \"errors\": [\"test verdict FAILED\", \"summar",
          "GATE_PHASE phase=integration-latency test_rc=3 predicate_rc=1 start_ms=1792866481577 end_ms=1792866648585"
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
        "FAIL": 1
      }
    },
    "verdict": "failed",
    "message": "a latency test failed after a probe that said clear (TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=FAIL): it ran on a worker whose load average was under the threshold, so this is a real verdict, not a deferral, and it is never retried into a pass. The load it ran under is in latency-leg.json.",
    "rc": 1
  },
  "test_log_dir": "/var/tmp/deploy-ga-mehe0np-fresh.4kr1Sqty/c3/evidence/remote/logs",
  "waiver_ref": "none",
  "heavy_mode": "none",
  "ci_lane_run": "n/a (no CI-config change)",
  "test_cmd": [
    "test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-mehe0np-fresh.4kr1Sqty/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-mehe0np-fresh.4kr1Sqty/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-mehe0np-fresh.4kr1Sqty/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-mehe0np-fresh.4kr1Sqty/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-mehe0np-fresh.4kr1Sqty/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"
  ],
  "failure_attribution": {
    "TestSessionStreamStructuredPromotesFallbackToHistoryWithoutReconnect/city-huma": {
      "tracker": "ga-a36cdy4",
      "proof": "a",
      "record": "/var/tmp/deploy-ga-mehe0np-fresh.4kr1Sqty/api-failure-proof.json"
    },
    "TestGCLiveContract_BeadsAndEvents/GET_/v0/city/{cityName}/work/active-step": {
      "tracker": "ga-od4rcxy",
      "proof": "d",
      "record": "/var/tmp/deploy-ga-mehe0np-fresh.4kr1Sqty/integration-failure-proof.json"
    },
    "TestGcSeatHookLatencyBudget": {
      "tracker": "ga-bqsrp4o",
      "proof": "b",
      "record": "/var/tmp/deploy-ga-mehe0np-fresh.4kr1Sqty/latency-failure-proof.json"
    }
  },
  "load_threshold": "15",
  "load_waited_seconds": "1801",
  "load_wait_timed_out": "1",
  "run_start_load": "115.69",
  "run_max_load": "144.64",
  "run_mean_load": "129.97",
  "run_readings": "11"
}
```

## Diff-owned test evidence

```text
TestDoltStateExistingManagedCmdReportsDeletedInodes PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-10cc7795c0ec199a5530.log)
  TestDoltStateProbeManagedCmdReportsDeletedOwnedHolder PASS (integration-packages-821a75e4a503601af052.log, unit-f93a0ca96ccc61d0cae9.log)
  TestDoltStateWaitReadyCmdDetectsDeletedInodes PASS (integration-packages-e4245ddf6a37030111bc.log, unit-abd97d2cb9b450225d57.log)
  TestStartOpenFileAndTCPListenerReportsChildStderrWhenChildExits PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-2bde55fab377f50b4739.log)
GATE_TEST_EVIDENCE_RESULT ok: 4 diff-owned tests verified by name
```

Deployer does not merge. Clearance and merge-request pin the final isolated deploy head; a changed head requires a fresh gate.
