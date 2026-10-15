# Tmux teardown socket settlement release gate

**Verdict:** **PASS**

Deploy bead: `ga-7ig6rpp`; build `ga-k52krce`; review `ga-zljlflf` (PASS, round 2).
Reviewed source: `74b767207d0859594d4a6c25fa39409e47dfe7ff` (resolved against the object store).
Deploy mode: remote; push remote: fork; base ref: origin/main.
gate_base: `8ed726532c960f7da00153b22f208666dbde334d`.
Canonical tested merge: `399cfc0578e2f0e6f1d422f2213637acb76a6c34`; tree `415da98fd92f9377fcc576884709fceab47eae96`.
No associated PR existed at preflight. No self-rebase was needed. Ancestry scope passes with the deploy and build IDs.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Live review ga-zljlflf records PASS for the exact reviewed source. |
| 2 | Acceptance criteria met | PASS | Successful teardown now waits for the socket observer to report absent/stale; a live socket fails closed after five seconds and already-gone teardown remains immediate. Independent prebuilt integration-tagged teardown/restart scenario: 1500 PASS, zero FAIL/SKIP. Both production behavior and negative controls pass in the full unit and tagged package tiers. Manifest is 715 = 545 untagged + 170 tagged-only, with six disjoint shards. |
| 3 | Full CI-equivalent suite passes | PASS | All four forced remote phases pass: unit 368, acceptance 24, integration packages 78, full integration 14. Every one of 484 attempts is remote, uncached, first-attempt, in-window, with a terminal PASS. Native floor 16/16 steps exit zero. All seven changed tests pass by name. |
| 4 | No open high-severity findings | PASS | The reviewer records zero blockers and majors. Listed observations are non-blocking. |
| 5 | Clean tested source | PASS | Canonical merge is clean; only this release record is added on the isolated deploy branch. Prior held gate evidence is preserved outside the checkout. |
| 6 | Diverges cleanly from main | PASS | Textual merge is conflict-free and matches the predicted tree. go build ./..., go vet ./..., and go vet -tags integration ./internal/runtime/tmux all exit zero on a fresh view. |
| 7 | Single feature theme | PASS | Four reviewed commits affect tmux teardown, its unit tests, and the existing test manifest. No unrelated or denied paths. |

## Test evidence

test_cmd: gate-criterion3.sh through gate-detached-run.sh; make check, acceptance, integration-packages and full integration (no smoke filter). The runner uses the isolation wrapper --rbe, followed by the mandatory native floor through load-gate-run.sh.
test_cmd_scope: full-suite.
test_counts: 112666 PASS, 0 FAIL, 340 SKIP (top-level result lines across 500 retained logs, not distinct tests).
test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-7ig6rpp.c3.qjud786v/evidence/remote/logs.
test_exec_mode: remote-exec (derived from BEP).
test_exec_bep: /var/tmp/gc-heavy-gate/runs/ga-7ig6rpp.c3.qjud786v/evidence/remote/{unit,acceptance,integration-packages,integration-full}.bep.json.
heavy_mode: none.
ci_lane_run: n/a (no CI configuration changed).
waiver_ref: none; gascity has no waiver path.
failure_attribution: none; no candidate test failure.
skip_inventory: /var/tmp/gc-heavy-gate/runs/ga-7ig6rpp.c3.qjud786v/evidence/remote/skip-inventory.json (326 farm records: 71 environment-absence, 255 other).
owned_topup: none needed; all seven owned tests report real PASS in the full-suite logs.
floor_topup: complete, 16/16 steps exit zero; /var/tmp/gc-heavy-gate/runs/ga-7ig6rpp.c3.qjud786v/evidence/remote/floor-topup.json.

Every environment-absence farm SKIP was selected for a native package top-up, with parent selection for subtests. Remaining skips are existing platform/opt-in/helper conditions, including replay/coverage inputs, live herdr, tmux integration/cleanup opt-ins and tests needing an attached tmux server. All 228 farm skip source sites independently read UNCHANGED against this canonical merged candidate; no changed test file contains a skipped test, and no shared hunk reaches a skip. The tmux socket settlement behavior does not control these early environment/opt-in guards. Native skips remain non-owned and are retained verbatim. Detailed site proof: /var/tmp/deploy-ga-7ig6rpp.qjud786v/skip-site-merge-proof.json. No owned SKIP/FAIL/MISSING exists.

All 968 retained remote artifacts were independently SHA-256 verified: unit 736, acceptance 48, integration packages 156, full integration 28. Verification: /var/tmp/deploy-ga-7ig6rpp.qjud786v/artifact-verification.json. The isolation wrapper reports PASS-THROUGH -- NOT ISOLATED. The supplemental native process explicitly scrubs store selectors and pins SHELL=/bin/sh; test bd is v1.3.1, ref_check match:c1c4b642a. Rootless podman socket is active and the pinned dolt-sql-server:2.2.0 image is cached.

```json
{
  "test_exec_mode": "remote-exec",
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
  "rbe_state": {
    "rc_marker_present": true,
    "kill_switch_absent": true,
    "cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT",
    "worker_env_tree": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0",
    "worker_env_origin_main": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0"
  },
  "fallback_reason": null,
  "admission": {
    "class": "remote",
    "floor_gib": 25.0,
    "free_gib": 102.33196640014648,
    "waited_seconds": 0.001,
    "cap": 2
  },
  "farm_test_seconds": 8570.593,
  "owned_topup": [],
  "test_counts": {
    "PASS": 112666,
    "FAIL": 0,
    "SKIP": 340
  },
  "diff_tests_executed": "diff_tests_executed:\n  TestKillServerAlreadyGoneDoesNotWait PASS (integration-packages-15c091b746137e5c2921.log, unit-ce5f199695f6832daef7.log)\n  TestKillServerFailsClosedWhenSocketStaysLive PASS (integration-packages-f1416b63f1c12109ea7e.log, unit-4bcc997199c6a7426526.log)\n  TestKillServerWaitsForSocketToStopAnswering PASS (integration-packages-5a078b24388ef3c79748.log, unit-ef2749222a71266756ae.log)\n  TestTeardownServerCallsKillServer PASS (integration-packages-15c091b746137e5c2921.log, unit-ce5f199695f6832daef7.log)\n  TestTeardownServerTreatsAlreadyGoneServerAsSuccess PASS (integration-packages-5a078b24388ef3c79748.log, unit-ef2749222a71266756ae.log)\n  TestRuntimeTmuxManifestMatchesCanonicalLinuxIntegrationInventory PASS (unit-b0c13e82fd961ff4f637.log)\n  TestRuntimeTmuxManifestSixShardsPartitionInventoryExactlyOnce PASS (unit-ee115c844954da53d34d.log)\nGATE_TEST_EVIDENCE_RESULT ok: 7 diff-owned tests verified by name\n",
  "criterion3_valid": true,
  "floor_topup": {
    "complete": true,
    "steps": 16,
    "record": "/var/tmp/gc-heavy-gate/runs/ga-7ig6rpp.c3.qjud786v/evidence/remote/floor-topup.json"
  }
}
```

Actual phase command flags:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-7ig6rpp.c3.qjud786v/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-7ig6rpp.c3.qjud786v/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-7ig6rpp.c3.qjud786v/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-7ig6rpp.c3.qjud786v/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/gc-heavy-gate/runs/ga-7ig6rpp.c3.qjud786v/evidence/remote/logs logs=500 logs_with_results=434
test_counts: PASS=112666 FAIL=0 SKIP=340
  (top-level result lines, column 0, summed over the 500 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
diff_tests_executed:
  TestKillServerAlreadyGoneDoesNotWait PASS (integration-packages-15c091b746137e5c2921.log, unit-ce5f199695f6832daef7.log)
  TestKillServerFailsClosedWhenSocketStaysLive PASS (integration-packages-f1416b63f1c12109ea7e.log, unit-4bcc997199c6a7426526.log)
  TestKillServerWaitsForSocketToStopAnswering PASS (integration-packages-5a078b24388ef3c79748.log, unit-ef2749222a71266756ae.log)
  TestTeardownServerCallsKillServer PASS (integration-packages-15c091b746137e5c2921.log, unit-ce5f199695f6832daef7.log)
  TestTeardownServerTreatsAlreadyGoneServerAsSuccess PASS (integration-packages-5a078b24388ef3c79748.log, unit-ef2749222a71266756ae.log)
  TestRuntimeTmuxManifestMatchesCanonicalLinuxIntegrationInventory PASS (unit-b0c13e82fd961ff4f637.log)
  TestRuntimeTmuxManifestSixShardsPartitionInventoryExactlyOnce PASS (unit-ee115c844954da53d34d.log)
GATE_TEST_EVIDENCE_RESULT ok: 7 diff-owned tests verified by name
```

## Load and supplemental acceptance

Remote admission uses the documented 25 GiB floor/cap 2 and bypasses the local host-load wait. The native floor retains its local load wait:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=7.71 run_max_load=11.93 run_mean_load=10.34 run_readings=6 wait_first_load=7.71 wait_max_load=7.71 wait_mean_load=7.71 wait_readings=1 read_errors=0
```

The independent 1500-repeat check of TestConfigureServerReappliesExitEmptyAfterReplacement used a prebuilt tagged binary, package-directory CWD, scrubbed store environment and a private TMUX_TMPDIR. Result: 1500 PASS, 0 FAIL, 0 SKIP; /var/tmp/deploy-ga-7ig6rpp.qjud786v/native-repeat.log. Its load evidence:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=150 wait_timed_out=0 run_start_load=14.37 run_max_load=13.58 run_mean_load=11.49 run_readings=13 wait_first_load=18.17 wait_max_load=18.17 wait_mean_load=16.30 wait_readings=6 read_errors=0
```

## Fast lanes

policy_lane: PASS — remote nogo on changed tmux/scripts packages plus all make-check shell guards.
format_lane: PASS — golangci-lint 2.12.0, changed tracked Go files against the recorded base.
drift_lane: PASS — CI's make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec followed by git diff --exit-code, in a separate fresh view.
hooks: make check-hooks confirms .githooks is active. Canonical scratch materialization used the normal pre-commit hook. Publication hooks are run by the final commit/push.

An initial fast-lane launcher passed trusted helper Make assignments as Bazel flags, causing an invocation error before policy/test execution; its logs remain separate. Its formatter also selected an empty worktree scope and is not credited. The corrected launcher selects tracked changes against the pinned base and passes all fast checks. This is a tooling correction, not a retried candidate test verdict.

```text
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.56aegd head=399cfc0578e2 tree=415da98fd92f dir=/var/tmp/gc-fresh-tree.sBXitX modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.56aegd,bazel-out,bazel-testlogs
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.56aegd head=399cfc0578e2 tree=415da98fd92f dir=/var/tmp/gc-fresh-tree.qMO5IT modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.56aegd,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=8ed726532c960f7da00153b22f208666dbde334d knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.qMO5IT
GLT_RECORD: {"golangci_lint":{"pin":"2.12.0","resolved":"2.12.0","mismatch":false}}
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.56aegd head=399cfc0578e2 tree=415da98fd92f dir=/var/tmp/gc-fresh-tree.TsQetS modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.56aegd,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=8ed726532c960f7da00153b22f208666dbde334d knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.TsQetS
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.56aegd head=399cfc0578e2 tree=415da98fd92f dir=/var/tmp/gc-fresh-tree.uq05gp modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.56aegd,bazel-out,bazel-testlogs
```

Applicable CI commands were resolved from TESTING.md, Makefile and .github/workflows/bazel.yml; docs/PROJECT_MANIFEST.md is absent. No API, generated client, dashboard or documentation page changed, so dashboard preview is not applicable.

## Handoff

The F8 review ga-4qya39x must stay held until BOTH this fix and ga-v67x2l2 actually LAND on origin/main. Closing a deploy bead with blocked work outcome is not landing. Keep ga-vsfwb8l open until the fix lands; report a recurrence of the teardown/restart live-socket signature there. The PR carries the reported compile-RED/GREEN, reviewer A/B and socket-probe evidence, and the builder-host Bazel limitation. No production gc binary deployment is part of this gate.

Raw evidence is retained at /var/tmp/deploy-ga-7ig6rpp.qjud786v and /var/tmp/gc-heavy-gate/runs/ga-7ig6rpp.c3.qjud786v/evidence. Gate and supplemental services are complete with zero exit status; scratch/output-base cleanup follows publication.
