# Foreign-pane regression across tmux versions — release gate

**Verdict:** **PASS**

Evaluated 2026-10-21T00:17:33.205513+00:00. Deploy ga-v67x2l2; build ga-dz02lbk; review ga-5pg3n01.

The two unrelated full-integration failures remain FAIL in the raw evidence. Criterion 3 passes under the documented criterion-3a attribution and fix-carrying exception, with all required local checks completed. No waiver or retry of either failed verdict was used.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | PASS — ga-5pg3n01 records PASS for the resolved reviewed source below, with no blocking security or specification finding. |
| 2 | Acceptance criteria met | PASS — production selects the pane marked for the session, independent of row order. The fixture verifies the joined window and swaps by pane ID when needed. Host tmux 3.7c: owned test 25/25; complete tagged package 1535 PASS/0 FAIL/12 SKIP; first-row resolver mutant killed 3/3 at the agent-pane assertion. |
| 3 | Tests pass | PASS with attributed failures — all four full-scope phases completed, 491 forced uncached remote first attempts. Two unrelated failures are attributed below; mandatory local floor and host acceptance passed. |
| 4 | No unresolved HIGH review findings | PASS — 0 unresolved HIGH/blocking findings in the review. |
| 5 | Final branch clean | PASS — clean source and canonical merge scratch before the gate record; only this gate document is added to the isolated deploy branch. Final clean status is checked after its commit. |
| 6 | Merge state and build | PASS — clean merge on the pinned current tip; `bazel build --config=remote-exec --noremote_local_fallback --keep_going //...` completed with nogo lint/vet in a separate fresh view. |
| 7 | Single feature theme | PASS — one existing tmux test, ten added fixture lines; no production, CI configuration, dependency, census or test-target change. |

## Source and merge evidence

```
deploy_mode: remote
push_remote: fork
reviewed_source: 70781541f21654fdcec8ea723639ba2d3cf9a470
gate_base_sha: 323e48048593e5b399c0954448d099481b2fa77b
merge_head: 886b134dfd4997fbc27cb14387f73aa427e2342d
merge_state: clean (origin/main@323e48048593e5b399c0954448d099481b2fa77b)
gate_base_kind: current-tip
merge_validation: merge queue and MPR settle changes to main after this pinned evaluation
heavy_mode: none
```

Preflight found no PR for the reviewed source. The ancestry guard accepts this deploy plus its build bead ga-dz02lbk; the source contains only this theme. The source branch is provenance. The publication branch is mechanically derived as deploy/ga-v67x2l2-gate.

policy_lane: PASS — make lint, workflow runner-policy and suite-coverage tests, Bazel format/policy/scripts targets, and all make-check shell guards. drift_lane: PASS — make bazel-sync followed by git diff --exit-code. Build, policy and drift each ran in a separate fresh view of the same merge commit; their FRESH_TREE_RECORD lines and clean exits are retained in build.log, policy.log and drift.log.

## Test execution

test_cmd_scope: full-suite. The four phases run the canonical unit make-check lane, acceptance targets, all integration packages, and unfiltered full integration. Exact commands and BEP predicates are in the original execution record below. Container setup used the active rootless Podman socket and pinned Dolt image; the pinned test bd was verified by the wrapper. Host acceptance used Bazel remote compilation with TestRunner=local and forced fresh results on tmux 3.7c.

| Lane | PASS | FAIL | SKIP |
|---|---:|---:|---:|
| unit | 72570 | 0 | 255 |
| acceptance | 410 | 0 | 13 |
| integration-packages | 49795 | 0 | 74 |
| integration-full | 190 | 2 | 14 |
| floor | 21 | 0 | 14 |

The tally counts top-level result lines per job; subtests are not added a second time. The host supplement is separate from those totals, and its expected mutant failures are negative-control evidence.

```
GATE_TEST_EVIDENCE logs_dir=/var/tmp/deploy-ga-v67x2l2-fresh.zffepzhx/criterion3/evidence/remote/logs logs=507 logs_with_results=440
test_counts: PASS=122985 FAIL=2 SKIP=370
  (top-level result lines, column 0, summed over the 507 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+55 more)
failed: TestGCLiveContract_BeadsAndEvents (integration-full-caf9552c6795b8d49441.log)
failed: TestGcSeatHookLatencyBudget (integration-full-f7217e413eb188f8ffa6.log)
diff_tests_executed:
  TestProvider_JoinedForeignAgentPaneIsNotTheAgent PASS (integration-packages-5a078b24388ef3c79748.log)
GATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name
```

test_exec_mode: remote-exec for the full four-phase suite; local floor and acceptance are separately identified supplements. test_exec_bep: 491 remote/0 cached/0 retried/0 other-strategy attempts, all inside their phase windows and all terminal. farm_test_seconds: 10371.325. fallback_reason: none. admission: remote, 25 GiB floor, cap 2; observed free 68.446 GiB, wait 0.001s. rbe_state: kill switch absent, certificate and pinned worker environment matched; exact values below.

owned_topup: [] — the one owned test already passed by name in the integration-package phase. floor_topup: complete, 16 steps, all exit 0, including beadstest, Darwin compile, both runner self-tests and every environment-absence SKIP selected by package/parent. Full floor manifest: criterion3/evidence/remote/floor-topup.json. test_log_dir: /var/tmp/deploy-ga-v67x2l2-fresh.zffepzhx/criterion3/evidence/remote/logs; floor logs: /var/tmp/deploy-ga-v67x2l2-fresh.zffepzhx/criterion3/evidence/remote/floor-logs. All 982 original test.log/test.xml artifacts were hash-verified after retention.

skip_inventory: 356 remote records, classes {'other': 285, 'environment-absence': 71}; every name and printed reason is retained in criterion3/evidence/remote/skip-inventory.json. Remaining skips concern unchanged platform, opt-in, unavailable-provider, live-journey or fixture inputs. Every environment-absence group received its required native top-up. No owned test was skipped; the changed test body reports PASS both remotely and on the host. The diff-owned explanation is owned-explain.txt; candidate-file-skip-sites.json confirms there were no remote skip sites in the changed adapter_test.go. The untouched test binaries cannot import the changed test file.

ci_lane_run: n/a — no CI-configuration change. waiver_ref: none; gascity has no waiver path. policy_attribution: none.

## Failure attribution

The original full-integration verdicts are preserved. Both trackers were opened, both signatures match, and both tracker creation times precede this run. The successful import-closure computation is failing-test-import-closure.txt; the only changed path is a test file in another binary, with no changed production input even in the gc executable spawned by these tests. The integration fixture, resource census and target lists are unchanged. There is no path overlap.

- TestGcSeatHookLatencyBudget -> ga-bqsrp4o; clause 3(a), mechanism. The unchanged 1s budget failed with fastest prime/drain/mail 1864/1303/1003ms. Designated fix ga-yu01v6x remains OPEN and unlanded; its listTierPair and newIsolatedCommandEnvDirectBD entrypoints are absent from origin/main. The existing diagnosis also records identical full-phase failures on pristine main. Sighting comment 6be47623-9c81-5f6f-b383-8704841851fc was read back.
- TestGCLiveContract_BeadsAndEvents active-step subtest -> ga-od4rcxy; clause 3(a), mechanism. The unchanged probe returns 422 for missing query.assignee at gc_live_contract_test.go:1509. Fix ga-ze0aym0 has a blocked work record; its resolved commit f39fa5525a3a947a8e5f28548e3087cf73fcf2e1 returns NOT-LANDED from work-record-check. Sighting comment 2f098492-0fe5-5c5c-bb9a-45e5b5bf1229 was read back.

fix-carrying repeat exception: this deploy's build_bead=ga-dz02lbk is gc.fixes_tracker=ga-rkis7u4 stamped. Both blocking conditions have separately stamped, unlanded fixes. Thus the documented exception permits attribution of either tracked condition under clauses 1-4; it is not a waiver. No failing test was rerun. Full clause evidence: attribution.json.

The canonical driver stopped at the failed final phase before running its floor. Its original execution.json remains unchanged with exit_code 1 and criterion3_valid=false. After attribution, the required floor ran separately on the same merge scratch; this supplies the missing local checks without retrying either failure or claiming that the raw run was green.

The first separate floor launch was stopped during its load wait, before any floor test ran, to correct missing remote admission. The admitted continuation preserved the original wait deadline, with 136s remaining after 1664s elapsed. Its admission record is floor-admission.json (remote class, 25 GiB floor, cap 2); floor-wait-correction.json records both launch times. The abandoned wait produced no test verdict.

## Local load evidence

```
floor-admitted: LOAD_GATE_SUMMARY threshold=15 waited_seconds=150 wait_timed_out=1 run_start_load=24.67 run_max_load=39.12 run_mean_load=31.99 run_readings=11 wait_first_load=31.71 wait_max_load=31.71 wait_mean_load=27.62 wait_readings=6 read_errors=0
host-acceptance: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=20.17 run_max_load=26.05 run_mean_load=22.93 run_readings=10 wait_first_load=30.54 wait_max_load=30.54 wait_mean_load=19.33 wait_readings=61 read_errors=0
```

## Original execution record

```json
{
  "test_exec_mode": "remote-exec",
  "fallback_reason": null,
  "exit_code": 1,
  "test_cmd_scope": "full-suite",
  "evidence_dir": "/var/tmp/deploy-ga-v67x2l2-fresh.zffepzhx/criterion3/evidence/remote",
  "admission": {
    "class": "remote",
    "floor_gib": 25.0,
    "free_gib": 68.44609069824219,
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
      "results": 80,
      "remote_executed": 80,
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
  "farm_test_seconds": 10371.325,
  "criterion3_valid": false,
  "test_cmd": [
    "test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-v67x2l2-fresh.zffepzhx/criterion3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-v67x2l2-fresh.zffepzhx/criterion3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-v67x2l2-fresh.zffepzhx/criterion3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
    "test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-v67x2l2-fresh.zffepzhx/criterion3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"
  ]
}
```

All evidence paths above are relative to /var/tmp/deploy-ga-v67x2l2-fresh.zffepzhx, unless absolute. No evidence logs are deleted.
