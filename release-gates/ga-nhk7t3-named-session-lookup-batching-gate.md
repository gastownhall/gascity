**Verdict:** **PASS**

# Release gate: named-session closed-bead lookup batching

- Deploy bead: `ga-nhk7t3`
- Reviewed source: `afb07bb2f9d3fc91c3cd4aa060e4246c8352c99d`
- Base checked: `origin/main@b2decffd64551a9fc6d2b5521a97d1527d2ee8a2`
- Gate date: 2026-09-22

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-6hp319` records `verdict: pass` for the exact source SHA. No review carryover is involved. |
| 2 | Acceptance criteria met | **PASS** | The diff replaces per-identity closed-session reads with a two-query, deduplicated and globally sorted index; preserves named-bead precedence and repairable legacy shapes; performs no index read when no `on_demand` named session exists; and leaves the existing single-identity lookup in place. The four diff-owned tests below directly verify constant read count, the zero-read path, reference-lookup equivalence, and the documented no-type/no-label boundary. |
| 3 | Tests pass | **PASS (with attributed pre-existing failures)** | The documented full local CI suite ran through the isolation wrapper at the reviewed SHA with the rootless Podman socket enabled and Ryuk disabled. All 40 jobs were scheduled: 34 job PASS, 6 raw job FAIL, 0 omitted, and 0 explicit SKIP results. The six failing top-level tests are attributed below under criterion 3a. All four diff-owned tests passed. No waiver was used. |
| 3a | Pre-existing failures attributed | **PASS** | Five managed-Dolt readiness failures map to pre-run tracker `ga-ycrvza`, which records identical reproduction at untouched merge base `849cb1a3978baf3eac0cfce5258c5598b2d0eecd`; the failing file is outside the diff. `TestGcBeadsBdProviderOwnedRealLifecycleStopsOwnedProcesses/proxied` maps to pre-run tracker `ga-u7dpag`, which records the identical line-13099 failure on untouched `origin/main@af7ad8a0fe9bc52f6ff4b902be8be2a713511ba2`. That test shares the broad `cmd/gc` package with production changes, so the stronger same-package rule applies: proof (d) landed, no resource-census or Makefile/test-target change exists, and no new test file was added. Both trackers were opened and updated with this run's sightings. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, `LINT_BASE=origin/main LINT_CHANGED_REF=HEAD make lint-new`, `make vet`, `make check-docs`, `make check-hooks`, `git diff --check origin/main...HEAD`, and changed-file `gofmt -l` all passed. |
| 3c | CI-config lane | **PASS (not applicable)** | No workflow, job matrix, timeout, or required-check configuration changed. `ci_lane_run: n/a (no CI-config change in this diff)`. |
| 4 | No high-severity review findings open | **PASS** | Reviewer recorded no blockers, majors, security findings, or unresolved high-severity findings. |
| 5 | Final branch clean | **PASS** | The reviewed source checkout was clean before the gate record was added; the only gate-time worktree change is this release-gate file. |
| 6 | Branch diverges cleanly from main | **PASS** | After a post-test fetch, `git merge-tree --write-tree origin/main afb07bb2...` exited 0 and produced tree `9bafdea0102cf85633b9942ec466f73566039a59`. |
| 7 | Single feature theme | **PASS** | All four commits and all five changed files implement or test one feature: batching closed named-session lookup while preserving lookup semantics and avoiding reads when unused. |

## Criterion 3 evidence

- `test_cmd`: `LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m make test-local-full-parallel`, invoked via `/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh`.
- `test_cmd_scope`: `full-suite`.
- Environment: Podman 5.8.4, rootless socket `unix:///run/user/1000/podman/podman.sock`, `TESTCONTAINERS_RYUK_DISABLED=true`, cached `dolthub/dolt-sql-server:2.1.7` present.
- `test_counts`: 40 jobs scheduled; 34 job PASS; 6 raw job FAIL; 0 omitted; 6 failing top-level tests; 0 explicit SKIP results.
- Full-suite log: `/var/tmp/ga-nhk7t3-full-suite.out`; per-job logs: `/var/tmp/gc-local-tests.fcjmg1`.
- `waiver_ref`: none.
- `skip_justification`: none needed; the harness reported no explicit skipped result and omitted no job.
- `diff_tests_executed`:
  - `TestReadyAssignedWorkAssigneesStoreReadsAreIndependentOfNamedSessionCount`: **PASS** (green full-suite process and integration shards; exact-name confirmation PASS).
  - `TestReadyAssignedWorkAssigneesSkipsClosedIndexWithoutOnDemandNamedSession`: **PASS** (green full-suite integration shard; exact-name confirmation PASS).
  - `TestClosedNamedSessionBeadIndexMatchesPerIdentityLookup`: **PASS** (full-suite `internal/session` package PASS; exact-name confirmation PASS).
  - `TestClosedNamedSessionBeadIndexMissesBeadWithNeitherTypeNorLabel`: **PASS** (full-suite `internal/session` package PASS; exact-name confirmation PASS).
- Exact-name confirmation log: `/var/tmp/ga-nhk7t3-diff-tests.out` (`4 PASS`, `0 FAIL`, `0 SKIP`; supplemental to, not a replacement for, the full-suite run).

### Failure attribution

- `TestAdoptPRFormulaCompileAndRun` -> `ga-ycrvza` | clause 3(d): identical managed-Dolt readiness timeout reproduced at the untouched merge base; failing path `test/integration/review_formula_test.go` is outside the diff.
- `TestPersonalWorkFormulaCompileAndRun` -> `ga-ycrvza` | clause 3(d): same base reproduction and no path overlap.
- `TestAdoptPRFormulaRetriesTransientReviewerStep` -> `ga-ycrvza` | clause 3(d): same base reproduction and no path overlap.
- `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` -> `ga-ycrvza` | clause 3(d): same base reproduction and no path overlap.
- `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` -> `ga-ycrvza` | clause 3(d): same base reproduction and no path overlap.
- `TestGcBeadsBdProviderOwnedRealLifecycleStopsOwnedProcesses/proxied` -> `ga-u7dpag` | clause 3(d): identical provider-owned-process-remained-alive failure reproduced on untouched main. `clause-4-guard: same_package=yes proof=d added_test_load=no` (no census bump, no new test target, no new test file).
