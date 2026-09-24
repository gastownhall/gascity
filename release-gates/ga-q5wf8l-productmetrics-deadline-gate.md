**Verdict:** **PASS**

# Release gate: product-metrics frozen-clock deadline fix

- Deploy bead: `ga-q5wf8l`
- Review bead: `ga-jdq626`
- Reviewed commit: `efc2c26f5bea4ed721ccb80f5c8599e35bd90f76`
- Base checked: `origin/main@0ab33e5a36b79f7a850d4ce6f2bc39bde0a1120b`
- Deploy mode: `remote` (push remote: `fork`)
- Gate date: 2026-09-19

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-jdq626` records an unambiguous PASS for the exact reviewed commit. The reviewer found no style, security, or specification blocker and corrected the handoff SHA to this resolved 40-character commit before deploy. No review carryover was used. |
| 2 | Acceptance criteria met | PASS | The service dependency now injects the lock-deadline builder alongside the clock; frozen-clock fixtures install a non-expiring builder, while the production default remains `context.WithTimeout`. The real-contention regression passed, the non-fixture default-path test proved a real wall-clock timeout remains, the widened `lookup_inflight` case passed, and the required destination-race stress completed 500/500 iterations. The deliberate 200 ms contention test's fixed-sleep census is synchronized in the manifest, generated documentation, and Go bootstrap policy. |
| 3 | Tests pass | PASS | The documented full local runner, `make test-local-full-parallel`, ran all 40 jobs through `isolated-test-run.sh`: 37 jobs PASS / 3 jobs FAIL / 0 jobs skipped. The three raw failing test occurrences are attributed under 3a and are not diff-owned. Both changed packages reported `ok` in the full suite. A subsequent verbose whole-package run of `./internal/productmetrics/...` resolved all nine diff-owned tests by name with PASS and 0 FAIL/SKIP; the literal acceptance stress separately produced 500 PASS / 0 FAIL / 0 SKIP. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3a | Pre-existing failures may be attributed | PASS | Two occurrences of the ambient-endpoint doctor failure map to pre-existing tracker `ga-x5wacn`; the stale integration Beads-version assertion maps to pre-existing gate tracker `ga-rnwg5u`. Each tracker predates this run and contains this run's sighting. The mechanism and path proofs are recorded below. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`, `make vet`, and `make check-docs` passed through the isolation wrapper; `make check-hooks` confirmed `.githooks` owns `core.hooksPath`. Raw `make lint` found only three findings in an ignored, ambient dashboard `node_modules/flatted` Go file. A disposable checkout of this exact base passed `make lint` with 0 issues; pre-existing tracker `ga-tcdrnz` records the contamination. The candidate changes no dashboard dependency, lint configuration, or build target. |
| 3c | CI-config diff needs its own lane | PASS | `ci_lane_run: n/a` — the candidate changes no workflow, matrix, timeout, or required-check configuration. |
| 4 | No high-severity review findings open | PASS | The exact-head review records no style or security findings and no unresolved HIGH finding. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty at the exact reviewed commit before this gate record was created. The gate record is the only deploy-branch addition. |
| 6 | Branch diverges cleanly from main | PASS | Preflight found no PR carrying the reviewed commit. `origin/main` is an ancestor of the reviewed source, and `git merge-tree --write-tree origin/main efc2c26f...` exited 0 with tree `3b98c318e8f8035df715439442bc0faf3944246c`. A fetch after the suite confirmed the base had not moved. No self-rebase was needed. |
| 7 | Single feature theme | PASS | All three commits serve one `internal/productmetrics` determinism fix: inject the decision deadline builder, apply it in frozen-clock fixtures, test both fixture and production behavior, and bank the regression test's resource-census delta. `assert_deploy_ancestry_scope` passed for the deploy, review, build-chain, and source bead IDs. |

## Criterion 3 evidence

```text
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true isolated-test-run.sh -- bash -lc 'make test-local-full-parallel'
test_cmd_scope: full-suite
runner_jobs: 37 PASS / 3 raw FAIL / 0 SKIP
raw_test_results: 3 FAIL / 0 SKIP (the runner is non-verbose for passing tests)
diff_packages_in_full_suite:
  github.com/gastownhall/gascity/internal/productmetrics ok
  github.com/gastownhall/gascity/internal/testpolicy/resourcecensus ok
full_suite_log: /var/tmp/ga-q5wf8l-full-suite.log
full_suite_job_logs: /var/tmp/gc-local-tests.EOYETh
waiver_ref: none
ci_lane_run: n/a (no CI configuration change)
```

The rootless Podman socket was live before the run, Ryuk was disabled for this
host's external reaper, and the cached `dolthub/dolt:2.1.7` and
`dolthub/dolt-sql-server:2.1.7` images matched `deps.env` before the suite
started. All 40 runner jobs executed.

The mandatory full suite is non-verbose for ordinary package tests, so it
proves both changed packages as complete package-level `ok` results but does
not print passing test names. The follow-up whole-package verbose run resolves
the diff-owned tests without narrowing the criterion-3 command:

```text
supplemental_cmd: isolated-test-run.sh -- bash -lc 'go test -v ./internal/productmetrics/...'
supplemental_counts: 471 top-level/subtest PASS / 0 FAIL / 0 SKIP
supplemental_log: /var/tmp/ga-q5wf8l-productmetrics.log
diff_tests_executed:
  TestRecordOnceReservesAndStartsAfterReleasingStateTransaction PASS
  TestRecordOnceKeepsStoredEventWhenSpawnWindowExpires PASS
  TestRecordOnceRejectsRegeneratedPriorSpawnToken PASS
  TestRecordOnceDoesNotStartWhenRetainedThrottleLeaseCloseFails PASS
  TestRecordOnceWritesOneImmutableEventAndConservativeQuotaWithoutScanning PASS
  TestRecordOnceRealLockContentionDoesNotExpireFrozenClockBudget PASS
  TestOpenWithDependenciesDefaultWithDeadlineUsesRealWallClockBudget PASS
  TestRecordOnceSpentBudgetAfterReservationLeavesOnlySafeOvercount PASS
  TestEventInstallCrashWindowCannotLeaveTwoNamesForOneReservation PASS
```

The acceptance command was also run literally through the isolation wrapper:

```text
go test -run TestRecordOnceFreshQuotaBootstrapNeverReplacesDestinationRace -count=500 -v ./internal/productmetrics/
500 PASS / 0 FAIL / 0 SKIP
log: /var/tmp/ga-q5wf8l-race-stress.log
```

### Failure attribution

- `TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint` (two jobs) -> `ga-x5wacn`.
  - Clause 1: the failing doctor test is not diff-owned.
  - Clause 2: `ga-x5wacn` predates this run, covers the exact JSON-parse corruption condition, and this run's two sightings were appended before attribution.
  - Clause 3(a), mechanism: the failure occurs while the doctor test parses external `bd config get` output containing an unexpected `/`. `go list -deps ./internal/doctor` has no dependency on either changed Go package (`internal/productmetrics` or `internal/testpolicy/resourcecensus`), so the failing package cannot reach the changed production code.
  - Clause 4: no failing test or doctor path overlaps the seven-file candidate diff.
- `TestPinnedIntegrationBeadsModuleVersion` -> `ga-rnwg5u`.
  - Clause 1: neither the integration test nor the dependency version pin is diff-owned.
  - Clause 2: `ga-rnwg5u` predates this run, covers the exact `v1.3.0` versus `v1.3.0-rc.2` condition, and this run's sighting was appended before attribution.
  - Clause 3(a), mechanism: the assertion compares the current module pin with an untouched hard-coded expectation; product-metrics deadline injection cannot affect either input.
  - Clause 4: the failing test, module pin, and integration paths do not overlap the candidate diff.

No inconclusive attribution path and no waiver were used.

### Policy attribution

`make lint` reported two `govet` inline findings and one `revive` package-comment
finding in
`internal/api/dashboardspa/web/node_modules/flatted/golang/pkg/flatted/flatted.go`.
That file is ignored by the dashboard's `node_modules/` rule and is not tracked.
A clean disposable checkout of `origin/main@0ab33e5a...` passed the same command
with zero issues, establishing that the raw failure is ambient worktree
contamination rather than candidate content. Tracker `ga-tcdrnz`, created before
this run, covers the condition and records this sighting. There is no changed-path
overlap with the dashboard dependency tree, package manifests, lint configuration,
or Makefile.

### Pre-push attribution

The normal guarded push ran `make test-fast-parallel`: 9 jobs PASS / 1 job
FAIL / 0 jobs skipped. The sole failing `unit-core` job contained two raw
failures, both covered by pre-existing trackers and disjoint from the candidate:

- `TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint` ->
  `ga-x5wacn`, with the same external `bd config get` JSON-parse corruption
  already attributed above. The tracker now records this push-gate sighting.
- `TestSetsidDoesNotPreventOrphanSelection` -> `ga-961qe1`.
  - Clause 1: the proctable test is not diff-owned.
  - Clause 2: `ga-961qe1` predates this run and covers the exact false
    `setsid did not take effect` condition; this sighting was appended before
    attribution.
  - Clause 3(a), mechanism: the test discovers a `sleep 300` process with an
    unscoped host-wide `pgrep -f` and can select another concurrent process;
    this run reported a child PID with a different session ID. In addition,
    `go list -deps ./internal/runtime/proctable` reaches neither changed Go
    package.
  - Clause 4: the candidate has no `internal/runtime/proctable` path overlap.

No raw failure was rerun. After this attribution was committed, the exact head
was eligible for the protocol's standing `git push --no-verify` authorization.

```text
pre_push_log: /var/tmp/ga-q5wf8l-push.log
failed_job_log: /var/tmp/gc-local-tests.wSCpXD/unit-core.log
```

## Acceptance and scope evidence

- `service.go` defaults the injectable builder to `context.WithTimeout` when callers do not supply one.
- `spool.go` changes only the builder seam used to create the existing lock deadline.
- Frozen-clock fixtures supply a cancelable, non-expiring deadline builder so elapsed real time cannot contradict the injected clock.
- `TestRecordOnceRealLockContentionDoesNotExpireFrozenClockBudget` holds a real lock for 200 ms and passes through the frozen-clock fixture.
- `TestOpenWithDependenciesDefaultWithDeadlineUsesRealWallClockBudget` exercises the unoverridden production default and passes.
- The widened `lookup_inflight` coverage passes in the whole-package run.
- The resource-census changes are the mechanical record of the deliberate fixed sleep in the regression test, not a second feature.
