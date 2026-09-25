**Verdict:** **PASS**

# Release gate: hybrid constructor conformance accounting

- Deploy bead: `ga-j7r5gu`
- Reviewed source: `2eff32228e37166e8a8b7353395297688859fadf`
- Amended source: `22984097e4ac28e27b42c1af2abd3f338073c3f1`. The criterion-3
  full-suite evidence below was collected at the reviewed source and has not
  been re-collected at the current tip. The delta is
  `build(hybrid): add conformance_test.go to the bazel go_test rule` plus this
  gate record itself; it changes no Go source and no test the suite executes.
- Base: `origin/main@42d46228da8488e2315d92002f15658f482ce338`
- Diff at that amended source: six files, `+91/-3` (four at the reviewed source, plus the Bazel rule fix and this gate record; subsequent edits to this record itself add to the insertion count)
- Gate date: 2026-09-22

## Criteria

1. **PASS — Review PASS present.** Review bead `ga-6h19th` is closed with verdict PASS for the exact reviewed source. The reviewer recorded no style, security, specification, or other open findings.

2. **PASS — Acceptance criteria met.** The live `cmd/gc.newHybridProvider` wiring was rechecked: it constructs seam-backed tmux and K8s providers before passing them to `internal/runtime/hybrid.New`. Because the K8s constructor requires a resolvable client configuration unavailable to ordinary CI, the production wrapper remains explicitly waived through 2026-10-22 by `ga-80po0c.3`, with the reason naming that unresolved boundary. The new `runtime.composition.hybrid` entry claims only `internal/runtime/hybrid.New`, and its scope explicitly limits the shared conformance proof to the local route while naming separate remote-route tests. It does not claim that fake-backed `hybrid.New` proves the production wrapper. `TestHybridConformance`, `TestStart_RoutesToRemote`, `TestListRunning_MergesBothBackends`, `TestCatalogReturnsIndependentEntries`, and `TestCatalogMatchesProductionWiringAndDocumentation` all pass. `TESTING.md` is synchronized with the checked ledger.

3. **PASS — Tests pass, with attributed pre-existing infrastructure failures.**

   - `test_cmd_scope: full-suite`
   - `test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true ... isolated-test-run.sh -- bash -c "make test-local-full-parallel"`
   - Environment: active rootless Podman 5.8.4 and cached `dolthub/dolt-sql-server:2.1.7`; the suite ran from the detached exact reviewed source.
   - Full-suite raw job counts: **33 PASS, 7 FAIL, 0 SKIP, 0 omitted** across all 40 scheduled jobs. Aggregate output: `/var/tmp/ga-j7r5gu-full-suite.out`; job logs: `/var/tmp/gc-local-tests.eKdZfj/`.
   - `diff_tests_executed:` the successful full-suite package shards include the diff-owned hybrid and provider-ledger packages. Exact-source verbose confirmation passed `TestHybridConformance` with all 34 nested contract checks, `TestCatalogReturnsIndependentEntries`, and `TestCatalogMatchesProductionWiringAndDocumentation`; acceptance-relevant `TestStart_RoutesToRemote` and `TestListRunning_MergesBothBackends` also passed. Focused confirmation log: `/var/tmp/ga-j7r5gu-diff-tests.out`.
   - `skip_justification: none`
   - `waiver_ref: none` for this deploy gate. The dated `runtime.builtin.hybrid` ledger waiver is unchanged product metadata, not a criterion-3 escape.

   **3a — Attributed failures:**

   - `TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint -> ga-woq0zj` — clause 3(b), cross-candidate condition: its test-owned external Dolt SQL server became connection-refused during schema initialization under full-suite load. The open tracker predates this run and records the same external-server readiness/connection-refused condition on unrelated candidate `ga-we6ffm`. This is distinct from the ambient-endpoint defect already fixed on main. No changed package or path overlaps `internal/doctor` or Dolt startup. Log: `/var/tmp/gc-local-tests.eKdZfj/integration-packages-core-1-of-4.log`.
   - `TestAdoptPRFormulaCompileAndRun`, `TestPersonalWorkFormulaCompileAndRun`, `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries`, and `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash -> ga-ycrvza` — clause 3(d), base reproduction recorded by the tracker: each hit the identical 20-second managed-Dolt readiness timeout during fixture initialization. The tracker predates this run and names all four tests. No changed package or test path overlaps `test/integration`. Logs: `/var/tmp/gc-local-tests.eKdZfj/integration-review-formulas-*.log`.
   - `TestAdoptPRFormulaRetriesTransientReviewerStep -> ga-vkhfnj` — clause 3(b), cross-candidate host-contention condition: provider-owned Dolt returned unexpected EOF and then an invalid connection during fixture `gc init`, before formula behavior. The open tracker predates this run and consolidates review-formula initialization connection loss under full-suite load. The candidate changes no fixture-init or production runtime path. Log: `/var/tmp/gc-local-tests.eKdZfj/integration-review-formulas-retries-1-of-2.log`.
   - `TestCleanInstallTutorialPath -> ga-vkhfnj` — clause 3(b), exact cross-candidate recurrence: provider-owned Dolt was unreachable on the test-owned ephemeral port during `gc rig add`. The same test/signature is recorded on unrelated candidates `ga-yshs1v`, `ga-3jvopn`, and `ga-0a2x3v`. The candidate has no tutorial or Dolt initialization path overlap. Log: `/var/tmp/gc-local-tests.eKdZfj/integration-rest-full-2-of-8.log`.

   All trackers were opened and this run's sightings appended. None of the failures is diff-owned. The diff touches `TESTING.md`, `internal/runtime/hybrid/conformance_test.go`, and provider-ledger files; none overlaps the failing packages or production initialization paths.

   **3b — Policy/lint lane:** `make test-ci-policy` PASS; `LINT_BASE=origin/main LINT_CHANGED_REF=HEAD make lint-new` PASS with 0 issues; `make vet` PASS; `make check-docs` PASS; `make check-hooks` PASS; `git diff --check origin/main...HEAD` PASS; changed Go files are `gofmt`-clean.

   **3c — CI-config diff:** not applicable; no CI job, matrix, timeout, or required-check configuration changed.

4. **PASS — No high-severity review findings open.** Reviewer findings record zero unresolved HIGH findings and no security findings.

5. **PASS — Final branch clean.** The reviewed source was clean before branch creation; the deployer-authored additions are this committed gate record and the `internal/runtime/hybrid` Bazel `go_test` srcs/deps fix. The isolated deploy branch is clean after both commits.

6. **PASS — Branch diverges cleanly from main.** After a fresh fetch, `git merge-tree --write-tree origin/main 2eff32228e37166e8a8b7353395297688859fadf` exited 0 and produced tree `987af9cb3691b5dbf3c71f02cd11258d6f754ff0` against `origin/main@42d46228da8488e2315d92002f15658f482ce338`.

7. **PASS — Single feature theme.** All commits and all six changed files account for one boundary: scoped conformance coverage and generated-ledger documentation for the hybrid runtime composition.
