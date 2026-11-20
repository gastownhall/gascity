# Runtime-lease fixture release-wait gate

**Verdict:** **PASS**

Bead: ga-78resd. Build: ga-0wsm18. Review: ga-h7vaua.
Deploy mode: remote. Reviewed/source commit: 5ace2fa64f2bbb44542763f7c345fabb4a7a45e3.
Pinned main: 975697477422344925c586ff497e91a7dd9972a5. Canonical merge: d2520840f7f951bc493ace23e26be14fe598d27e.
Canonical code tree: 45ef2d4032c24055284f730f4d9c80359405be0c.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Closed review ga-h7vaua names the exact reviewed source. Recomputed one-file patch-id matches the canonical diff; SOURCE_CRITERIA_VERIFIED.json. |
| 2 | Acceptance criteria met | PASS | Both edited lease tests actually PASS by name. The short refusal phases retain 600 ms/busy/no-provider-effect assertions. Only the success phases use the package hang budget; unchanged 200 ms release callback, original wait restored through cleanup. No production code changes. |
| 3 | Tests pass | PASS | Whole unit, full acceptance plus solo, all integration packages and prescribed integration smoke executed uncached/first-attempt on the same canonical tree. One raw acceptance FAIL explicitly attributed below; every other job PASS. All SKIPs individually reviewed and 104 deferred unit-process observations have actual matching integration PASS. FAST 18 and required CI leaf accounting complete. |
| 4 | No high-severity review findings open | PASS | Review records no blocking/high finding; two comment/documentation nits are nonblocking. |
| 5 | Final branch is clean | PASS | Exact canonical scratch clean including untracked files after all checks. Publication also requires the isolated gate-only commit to finish under normal hooks and its final tree to be verified clean. |
| 6 | Branch diverges cleanly from main | PASS | Actual normal-hook canonical merge equals merge-tree prediction at the pinned base; all 1227 build/nogo targets PASS. No content conflict or self-rebase. Supplemental conflict check against current main e55d79665711fd560ded4fd4f8e7c66a9f6629e3 also passed; suite evidence remains pinned to the base above. |
| 7 | Single feature theme | PASS | One file and one runtime-lease fixture timing condition; two direct helper callers. |

## Actual command scope and outcomes

test_cmd_scope: full-suite
heavy_mode: none
waiver_ref: none
ci_lane_run: n/a (no CI-config change)
test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-78resd-current.c3/logs

The documented commands were make check (whole Bazel //... plus shell guards), full acceptance (`//test/acceptance:acceptance_test` and `//test/acceptance:acceptance_solo_tests`), full `//test:integration_packages`, and the repository's `--config=integration-smoke //test/integration:integration_test`. CI/fork-cache, nocache_test_results, jobs=4 and the repository's configured shards were used. Integration packages include actual GC_FAST_UNIT=0 process coverage, with all 104 omitted unit cases matched by exact package/name to PASS; TestTutorial01 and its agent-pool scenario execute there. Reviewer package evidence is input only.

| Lane | Actual targets | Actual jobs | Raw Go terminal outcomes |
|---|---|---|---|
| unit | {"PASSED": 237} | {"PASSED": 311} | 54998 PASS / 0 FAIL / 211 SKIP |
| acceptance | {"FAILED": 1, "PASSED": 6} | {"FAILED": 1, "PASSED": 23} | 409 PASS / 1 FAIL / 13 SKIP |
| integration-packages | {"PASSED": 26} | {"PASSED": 63} | 36475 PASS / 0 FAIL / 53 SKIP |
| integration-smoke | {"PASSED": 1} | {"PASSED": 14} | 16 PASS / 0 FAIL / 1 SKIP |

diff_tests_executed: unit: TestManagerKillTakesTheRuntimeLease PASS (//internal/session:session_test); integration-packages: TestManagerKillTakesTheRuntimeLease PASS (//internal/session:session_test); unit: TestManagerStartWaitsForTheRuntimeLease PASS (//internal/session:session_test); integration-packages: TestManagerStartWaitsForTheRuntimeLease PASS (//internal/session:session_test)

Standard committed-base OpenAPI compatibility input is a regular file byte-identical to the pinned base. TestCommittedSpecAgainstBase, TestGateAgainstOasdiff, TestOpenAPIBreakingGateRunsInTheBazelUnitLane and TestSchemaFreshness actually PASS. All 17 roots in the runtime-lease fixture file actually PASS in the whole unit lane, recorded in WHOLE_LEASE_FIXTURE_CONSUMERS_UNIT_VERIFIED.json. Existing ready-parity replacement parents TestMemStoreReadyParityConformance and TestFileStoreReadyParityConformance actually PASS.

## Required policy, generated files and CI coverage

policy_lane: actual FAST 11 policy targets plus shell guards, hooks and changed lint PASS under the pinned gate-base wrapper.
drift_lane: actual FAST 6 generated targets and real make bazel-sync/Gazelle drift check PASS in separate fresh views; merge scratch not modified. Standard OpenAPI FAST target PASS. Total 18 first-attempt uncached FAST jobs PASS.

The required `bazel / unit`, `bazel / acceptance`, `bazel / integration-packages`, `bazel / integration-smoke`, and `BUILD files in sync` jobs are covered by the local full-scope commands and real Gazelle/drift check above; this is local equivalent coverage, not a claim that a GitHub run executed. The `bazel test (side-by-side)` aggregate and exact-head PR/merge-group CI remain MPR merge prerequisites. The ci-required origin/fork closures were independently identical. Additional required leaves: runner-policy LOCAL-PASS; changes LOCAL-PASS; credential-provider-windows NOT-TRIGGERED; pack-gate NOT-TRIGGERED. No uncovered or deferred leaf is called executed. Accounting proof: /var/tmp/gc-heavy-gate/runs/ga-78resd-current.ci-accounting/ACCOUNTING_VERIFIED.json.

## Preserved acceptance failure and attribution

failure_attribution: TestProxiedSuspensionIsQuiescenceSuspendedCity -> ga-v3bt9i | clause3:a MECHANISM — changed session test helper cannot enter the acceptance binary or normal gc.

The actual first-attempt failure is six late bead-store calls after the initial pair drain during the suspended-city 30 s window (backstop speedup 6): list, query, dependency lookup, source-bead scan, ephemeral scan and dolt stop. It is retained as FAIL. It is not an initial-drain deadline or cleanup race, and no load-causation claim is made.

- Clause 1: failing function/site 269 and entire file unchanged from the pinned base; whole-file SHA256 2f33bd639c93cd75b35f92373d9a318683d34290960d7b894fc9e48fe2f1e02d; SHARED-HUNKS 0. Both diff-owned tests actually PASS.
- Clause 2: OPEN gate tracker ga-v3bt9i predates this run and was opened in full. Sighting comment 141034ef-1974-5f5e-a64a-07c9c9aa069a readback verified.
- Clause 3(a): full module import closures retained for actual tag sets; changed non-test/module/build/environment inputs empty. The only changed input is a session _test.go helper with exactly two owned, nonparallel callers and cleanup restoration. It is absent from the acceptance binary and normal gc.
- Clause 4: changed test package internal/session, failing package test/acceptance; no package overlap.
- Repeat exception: own build ga-0wsm18 is actually stamped gc.fixes_tracker=ga-rzu9bj (closed blocked); blocking condition has actual OPEN, unlanded fix ga-3xnq72 stamped gc.fixes_tracker=ga-v3bt9i. All-status query and live fix were read. No other bead's special ruling or causal proof is inherited.

Proof: /var/tmp/deploy-ga-78resd-current.ykhg8qjt/ACCEPTANCE_FAILURE_ATTRIBUTION.json.
Original C3 RESULT remains failed, with all first-attempt log/XML/BEP hashes preserved. Unit and acceptance were never retried. Only the previously unrun full integration lanes continued on the same tree. One continuation startup failed before load/Bazel due to my non-idempotent tool-link helper; its original failed RESULT/logs remain. Corrected read-only tool revalidation verifies original hashes/links and current runtime in a separate directory; no test retry resulted.

## Every observed SKIP

- unit: 211 actual SKIP observations; {"deliberate-skip-test": 1, "existing-crash-characterization": 1, "existing-ledger-opt-out": 2, "forbidden-ambient-resolution": 6, "host-precondition": 4, "intentional-provider-skip-contract": 20, "native-upstream-no-endpoint": 1, "optional-herdr": 7, "optional-input": 11, "platform": 20, "retired-characterization": 1, "subprocess-helper": 6, "unavailable-external-source": 10, "unit-deferred-process": 104, "unsupported-profile": 17}. Mechanical/source/reason/import review: /var/tmp/gc-heavy-gate/runs/ga-78resd-current.c3/unit-skip-causal-review.json.
- acceptance: 13 actual SKIP observations; {"existing-placeholder": 7, "optional-input": 1, "optional-legacy-binary": 5}. Mechanical/source/reason/import review: /var/tmp/gc-heavy-gate/runs/ga-78resd-current.c3/acceptance-skip-causal-review.json.
- integration-packages: 53 actual SKIP observations; {"existing-ledger-opt-out": 2, "forbidden-ambient-resolution": 6, "global-cleanup-opt-in": 2, "native-upstream-no-endpoint": 1, "no-ambient-tmux-session": 4, "optional-br-provider": 1, "optional-herdr": 7, "optional-input": 13, "optional-ssh-endpoint": 1, "pinned-bd-feature-unavailable": 1, "platform": 10, "subprocess-helper": 3, "unavailable-external-source": 2}. Mechanical/source/reason/import review: /var/tmp/gc-heavy-gate/runs/ga-78resd-current.c3/integration-packages-skip-causal-review.json.
- integration-smoke: 1 actual SKIP observations; {"existing-unconditional-bd-conformance-skip": 1}. Mechanical/source/reason/import review: /var/tmp/gc-heavy-gate/runs/ga-78resd-current.c3/integration-smoke-skip-causal-review.json.

No owned test was skipped. Each printed reason, exact unchanged site/function, shared hunk and complete import closure is retained. Optional/placeholder observations stay SKIP and contribute no coverage. All 104 unit process omissions have actual integration companion PASS, recorded in PROCESS_COMPANIONS_VERIFIED.json.

## Runtime and load evidence

Verified rootless Podman socket and enabled stale-testcontainer sweep, pinned Dolt 2.2.0 image, test-only bd 1.3.1, ICU 74 private ABI environment, tmux 3.7c and private temporary files. Fleet database/schema was not migrated. Suites ran through the isolation wrapper and one private ABI wrapper. The load gate measures the five-minute average; its ordinary bounded timeout is recorded, without attributing failures to load.

### Original unit + acceptance

- load_threshold: 15
- load_waited_seconds: 1800
- load_wait_timed_out: 1
- run_start_load: 15.73
- run_max_load: 63.92
- run_mean_load: 41.99
- run_readings: 44
- wait_first_load: 68.20
- wait_max_load: 68.20
- wait_mean_load: 33.02
- wait_readings: 61
- read_errors: 0

Original raw summary: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=15.73 run_max_load=63.92 run_mean_load=41.99 run_readings=44 wait_first_load=68.20 wait_max_load=68.20 wait_mean_load=33.02 wait_readings=61 read_errors=0`.
### Remaining integration lanes

- load_threshold: 15
- load_waited_seconds: 1801
- load_wait_timed_out: 1
- run_start_load: 21.84
- run_max_load: 35.50
- run_mean_load: 30.79
- run_readings: 29
- wait_first_load: 25.88
- wait_max_load: 57.66
- wait_mean_load: 32.40
- wait_readings: 61
- read_errors: 0

Original raw summary: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=21.84 run_max_load=35.50 run_mean_load=30.79 run_readings=29 wait_first_load=25.88 wait_max_load=57.66 wait_mean_load=32.40 wait_readings=61 read_errors=0`.

## Retained records

- Whole scope/terminal inventory/artifact hashes/skip review: /var/tmp/gc-heavy-gate/runs/ga-78resd-current.c3/SUITE_MANIFEST_VERIFIED.json.
- Original failed run: /var/tmp/gc-heavy-gate/runs/ga-78resd-current.c3.
- Failed setup-only continuation: /var/tmp/gc-heavy-gate/runs/ga-78resd-current.remaining.
- Complete first-run remaining lanes: /var/tmp/gc-heavy-gate/runs/ga-78resd-current.remaining-v2.
- Source/review proof: /var/tmp/deploy-ga-78resd-current.ykhg8qjt/SOURCE_CRITERIA_VERIFIED.json.

Merge belongs to MPR after exact-head clearance and required CI; the deployer never merges.
