# Acceptance bd proxy-open stall release gate

**Verdict:** **PASS**

Date: 2026-10-23T18:38:17.076786+00:00
Bead: ga-i3b6pec. Build: ga-gx5tpso. Review: ga-c4dd15r. Refs tracker ga-vkhfnj.

- reviewed_commit: 0407ec356a24c72c7d86da60f869c79a38016361
- deploy_mode: remote
- base_ref: origin/main
- gate_base_sha: ab2ff1edd17e66d3cb53320eef7af33b32fa875c
- merge_state: clean (origin/main@ab2ff1edd17e)
- gate_base_kind: current-tip
- gated_merge_head: cc510ed0537f168bb0c3ce01a4c7ff5afc86c8b0
- gated_merge_tree: 1986ab9f00aa2910277694946639b8f9d5489558
- merge_validation: the clean merged tree was built with nogo/vet and tested; MPR retains merge authority and tests the eventual merge group.
- evidence_dir: /var/tmp/gc-gate.ga-i3b6pec.7q2xbh5o
- test_log_dir: /var/tmp/gc-gate.ga-i3b6pec.7q2xbh5o/c3/evidence/remote/logs
- heavy_mode: none
- waiver_ref: none

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Reviewer PASS | PASS | ga-c4dd15r explicitly passes the resolved reviewed commit. No carryover or source substitution. |
| 2 | Acceptance criteria | PASS | Four new tests pass by name; helpers and resource census run in the full unit lane. Linux helpers, Darwin helpers and acceptance_a consumer vet pass. Two BUILD srcs registered; regeneration has no drift. All 20 City.Init fatal messages are unchanged; no new test resource constructs or census changes; both source commits cite ga-gx5tpso. |
| 3 | Full CI-equivalent scope | PASS | Five forced remote phases, mandatory native floor, owned-test verification; two unrelated fixture conditions attributed below. Raw failures are preserved. |
| 4 | No unresolved HIGH finding | PASS | Reviewer records no blocker/major; O1-O7 are observations, not requests. |
| 5 | Clean final branch | PASS | Only the reviewed commits and this gate record are published; branch cleanliness verified after the gate commit. Unrelated scratch gate records preserved outside the deploy branch. |
| 6 | Merge state and build/vet | PASS | Clean canonical merge; bazel build --config=remote-exec --noremote_local_fallback --keep_going //... succeeds, including nogo/vet. |
| 7 | Single feature theme | PASS | Two source commits, four files, +368/-8, all test/acceptance/helpers: bounded recovery for pinned bd's proxy-open stall. The deploy adds only this gate record. |

## Test evidence

- test_cmd_scope: full-suite
- test_cmd: gate-criterion3.sh, with attributed continuation of previously unrun phases only. Unit: make check / bazel test //...; acceptance: bazel test --config=acceptance //test/acceptance:acceptance_test; integration-packages: bazel test --config=integration //test:integration_packages; full integration and the separate declared latency leg: bazel test --config=integration //test/integration:integration_test, partitioned by the shipped gate runner. Every remote phase forces --config=remote-exec --config=ci --nocache_test_results --noremote_local_fallback, verbose test logs and BEP.
- test_exec_mode: remote-exec (BEP-derived); mandatory native floor recorded separately, not substituted for a remote suite.
- fallback_reason: none
- farm_test_seconds: 10321.256
- test_exec_bep: 492 fresh remote results; 0 cached locally, 0 cached remotely, 0 other strategy, 0 retried; all attempt 1 and within their phase windows, every result terminal.
- rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45", "worker_env_tree": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45"}
- admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 74.48135375976562, "waited_seconds": 0.003}; remote cap2/floor25GiB was reacquired for each continuation, with no local downgrade.
- skip_inventory: /var/tmp/gc-gate.ga-i3b6pec.7q2xbh5o/c3/evidence/remote/skip-inventory.json; 356 entries ({'other': 285, 'environment-absence': 71}). All environment-absence selections were offered the native floor by package and parent test. Remaining skips concern unavailable optional providers/platforms, absent fixture inputs, intentional conformance skips, or tests delegated to the integration tier; none belongs to the four new tests. Reasons and terminal native results remain in the retained inventory/floor logs.
- owned_topup: []
- floor_topup: complete; 16 steps, all exit0. Includes beadstest, every environment-absence selection, Darwin fsys compile, push-gate-lock and local-concurrency self-tests. Every step's exact argv, test list and result are retained in /var/tmp/gc-gate.ga-i3b6pec.7q2xbh5o/c3/evidence/remote/floor-topup.json.
- latency_leg: measured on attempt1 after clear farm probe; TestGcBdLatencyBudget PASS and TestGcSeatHookLatencyBudget PASS. Farm probe load1=0.24, load5=2.27, load15=4.77, cpus32; details in latency-leg.json.
- ci_lane_run: n/a (no CI-config change)
- policy_lane: pinned golangci-lint2.12.0 fmt-check; make lint with remote nogo; make check-hooks, check-release-dist-ignore, check-routed-test-rows, check-split-topology-rows, check-residency-boundary; runner policy5 and suite coverage15 Python tests: PASS.
- drift_lane: make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec; git diff --exit-code and empty git status --porcelain --untracked-files=all: PASS in a separate fresh view. Format and policy also ran in separate fresh views against the frozen gate base.
- host_runtime: GATE_ICU74 verdict=OK provenance=recorded. No per-run loader override.
- supplemental_vet: VET_HELPERS_LINUX PASS, VET_HELPERS_DARWIN PASS, VET_ACCEPTANCE_TAG PASS. These are vet results, not test runs.

| Phase | Fresh remote results | Farm test seconds | Result |
|---|---:|---:|---|
| unit | 373 | 3172.19 | attributed fixture failure |
| acceptance | 24 | 1036.324 | PASS |
| integration-packages | 80 | 2898.329 | PASS |
| integration-full | 14 | 3144.511 | attributed fixture failure |
| integration-latency | 1 | 69.902 | PASS |

The unmodified execution.json retains exit_code1 and criterion3_valid=false from the original unit failure. The separate attributed-final-completion.json records the complete union after permitted attribution. No failed remote phase was rerun or repaired, and no result was cached into a PASS claim.

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/gc-gate.ga-i3b6pec.7q2xbh5o/c3/evidence/remote/logs logs=508 logs_with_results=442
test_counts: PASS=125541 FAIL=3 SKIP=370
  (top-level result lines, column 0, summed over the 508 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
failed: TestGCLiveContract_BeadsAndEvents (integration-full-65412ec6e874f54aa912.log)
failed: TestBdStoreReleaseIfCurrentAgainstRealBd/a_non-exact_id_refuses_instead_of_releasing_a_different_bead (unit-1eb6f8c6e2b565a3e9c5.log)
failed: TestBdStoreReleaseIfCurrentAgainstRealBd (unit-1eb6f8c6e2b565a3e9c5.log)
diff_tests_executed:
  TestBDScopePIDs PASS (unit-2af37a65322c095289c3.log)
  TestInitRecoveringBDProxyOpenStall PASS (unit-2af37a65322c095289c3.log)
  TestIsBDProxyOpenStall PASS (unit-2af37a65322c095289c3.log)
  TestResetBDProxiedCityEmptiesTheDirectory PASS (unit-2af37a65322c095289c3.log)
GATE_TEST_EVIDENCE_RESULT ok: 4 diff-owned tests verified by name
```

## Failure attribution

- failure_attribution: TestBdStoreReleaseIfCurrentAgainstRealBd/a_non-exact_id_refuses_instead_of_releasing_a_different_bead (and parent) -> ga-5wi30ie; clause3(a), mechanism. The unchanged fixture's shortened ID tst-9s matches tst-9sg and tst-9sq, so pinned bd rejects an ambiguous ID. Failure site208 is UNCHANGED, zero shared hunks; the integration-tagged test-binary closure (54 entries/51 unique module packages) excludes acceptance/helpers. The external bd binary/pin is unchanged and cannot execute these helpers. No package/path overlap, census bump or new suite target. The tracker was created during this first-sighting run; the same-run exception applies because clauses1/4 and proof3(a) are independently established. Raw log and failure-proof.json retained.
- failure_attribution: TestGCLiveContract_BeadsAndEvents/GET_/v0/city/{cityName}/work/active-step (and parent) -> ga-od4rcxy; clause3(b), cross-PR. Independent own-code PR8233's full deploy gate fails with the same HTTP422/query.assignee-required diagnostic on disjoint runtime/t3bridge changes; the fixture blob is identical, changed-file overlap empty, failure site1512 UNCHANGED and zero shared hunks. Comparison14 fresh remote results and28 artifacts verified. CI smoke PASS is not used as full-integration proof. This binary does import acceptance/helpers, so mechanism proof is not asserted; the focused coverage diagnostic has unusable LF0 metadata and is not asserted as proof either. Repeat fix-carrying applies: build ga-gx5tpso is gc.fixes_tracker=ga-vkhfnj; designated fix ga-ze0aym0 is gc.fixes_tracker=ga-od4rcxy with blocked work record, and its resolved commit is not on frozen/current main. Full evidence in integration-failure-proof.json.

## Load observations

The remote driver observes host load without imposing a local-heavy wait. The native floor retains its original soft threshold15/max1800s wait; timeout is reported, never treated as a test result. These are driver-host readings, not an attribution of remote failures to load.

```text
c3/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=58.83 run_max_load=62.24 run_mean_load=56.90 run_readings=10 wait_first_load=51.37 wait_max_load=114.34 wait_mean_load=88.13 wait_readings=61 read_errors=0
```

## Acceptance details

Recovery is armed only when the city directory was empty before attempt1 and the failed output contains the pinned v1.3.1 line ending. It logs the first output, kills only matching per-city proxy/Dolt processes, empties that city, and retries once. Reset failures, second failures and other signatures still fail. The end-of-line anchor rejects the extended upstream diagnostic, retiring this workaround with that bd upgrade. Product gc behavior, assertions, skips, timeouts and the bd pin are unchanged.

The reviewed test-only RED is a compile failure, not a behavioral assertion failure: vet reports `vet: test/acceptance/helpers/bd_proxy_open_stall_test.go:43:14: undefined: isBDProxyOpenStall`, and the test command ends `FAIL github.com/gastownhall/gascity/test/acceptance/helpers [build failed]` (five undefined symbols). This gate does not claim to rerun that historic RED. The four added tests pass in this gate's full-suite output; mutation and earlier reviewer evidence remain supplementary.
