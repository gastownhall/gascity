# Release Gate: Dolt leak-guard grace period

- Deploy bead: `ga-d5nmtj`
- Review bead: `ga-7r2h5i`
- Reviewed round-3 source: `917780062bc80b858fee3b5eadf3b25b6e7ede8d`
- Evaluated source after bounded self-rebase: `f7f06b97d8a0e23dabfc1c7831c2cf8ee55db6e3`
- Base: `origin/main@a4e4cc2bfac251b65116d536addbb4a7be9d95cd`
- Deploy branch: `deploy/ga-d5nmtj-gate`
- Verdict: **PASS**

The description's round-2 `Commit:` field is superseded by the bead's
round-3 metadata and re-review notes, both of which name `917780062b` as the
reviewed deploy source. `docs/PROJECT_MANIFEST.md` is absent from this
repository, so this checklist uses the seven release criteria in the active
deployer protocol.

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | `ga-7r2h5i` closed PASS after independently validating the error-capture fix. `ga-d5nmtj` then records an independent round-3 re-review PASS for `917780062b`, including code-path tracing, scope verification, and the full targeted test family. |
| 2 | Acceptance criteria met | **PASS** | Independent diff read and execution confirm: a Dolt process that exits within the injected grace window does not fail or reap; a process still present after the window still fails and is reaped; final-scan enumeration errors fail the guard instead of being swallowed; timing is injectable in tests; and the production ceiling uses `config.DefaultDoltStopTimeout` with an independent 15-second floor regression. `go build ./...` and `go vet ./...` are clean. |
| 3 | Tests pass | **PASS** | Required high-contention process gate: 7 PASS jobs, 0 FAIL. Fast baseline: 10 PASS jobs, 0 FAIL. All six diff-owned top-level tests passed by name, with 0 FAIL and 0 SKIP. Direct provider-ledger package run passed, including `TestCatalogMatchesProductionWiringAndDocumentation`; `go mod tidy -diff` was clean. `waiver_ref: none`. |
| 4 | No high-severity review findings open | **PASS** | Both review passes record no security/style blockers; the round-3 reviewer found no new failure. Unresolved HIGH count: `0`. |
| 5 | Final branch is clean | **PASS** | Before adding this checklist, `git status --short --branch` reported only `## deploy/ga-d5nmtj-gate`. `git diff --check` and `gofmt -l` on changed Go files produced no output. Repository hooks resolve to `/home/jaword/projects/gascity/.githooks`. |
| 6 | Branch diverges cleanly from main | **PASS** | No target PR existed for the reviewed SHA. The canonical bounded helper rebased isolated branch `deploy/ga-d5nmtj-gate` from `917780062b` to `f7f06b97d8` and returned `0`; current `origin/main` and waiver-fix commit `11edccc178` are ancestors of the evaluated head. The helper pushed with `--force-with-lease`, and the before/after SHAs are recorded on the bead. |
| 7 | Single feature theme | **PASS** | The three-file diff is one `cmd/gc` test-harness reliability change: grace-period leak detection, its regression tests, and promotion of the already-present backoff v4 dependency from indirect to direct. No independent feature is bundled. |

## Test evidence

Environment setup before test execution:

- `DOCKER_HOST=unix:///run/user/$(id -u)/podman/podman.sock`
- `TESTCONTAINERS_RYUK_DISABLED=true`
- Rootless Podman socket confirmed present.

Commands and counts:

- `LOCAL_TEST_JOBS=16 CMD_GC_PROCESS_TOTAL=6 make test-cmd-gc-process-parallel`: **7 PASS jobs, 0 FAIL** — six `cmd/gc` process shards plus `productmetrics-testhook`.
- `LOCAL_TEST_JOBS=16 make test-fast-parallel`: **10 PASS jobs, 0 FAIL** — all six `cmd/gc` unit shards, `unit-core`, filesystem cross-compile, and both concurrency/guard self-tests.
- `go test -count=1 -v ./cmd/gc/... -run '^(TestDoltLeakGuardedTestingMFinalSnapshotRunsBeforeRegistryReap|TestDoltLeakGuardedTestingMRunWithSweepsOrphanDirsAtStartup|TestDoltLeakGuardedTestingMToleratesLeakClearingWithinGraceWindow|TestDoltLeakGuardedTestingMWaitForFinalScanToClearReturnsEnumerationError|TestDoltLeakGuardedTestingMRunWithFailsOnFinalScanError|TestDoltLeakGuardGraceMaxElapsedTimeBudget)$'`: **6 PASS, 0 FAIL, 0 SKIP**.
- `go test -count=1 -v ./internal/testutil/providerledger/...`: PASS; the renewed provider ledger and production wiring/documentation check execute successfully.
- `go build ./...`: PASS.
- `go vet ./...`: PASS.
- `go mod tidy -diff`: PASS (no output).

`diff_tests_executed`:

- `TestDoltLeakGuardedTestingMFinalSnapshotRunsBeforeRegistryReap`: PASS
- `TestDoltLeakGuardedTestingMRunWithSweepsOrphanDirsAtStartup`: PASS
- `TestDoltLeakGuardedTestingMToleratesLeakClearingWithinGraceWindow`: PASS
- `TestDoltLeakGuardedTestingMWaitForFinalScanToClearReturnsEnumerationError`: PASS
- `TestDoltLeakGuardedTestingMRunWithFailsOnFinalScanError`: PASS
- `TestDoltLeakGuardGraceMaxElapsedTimeBudget`: PASS

`test_counts`: required job gates `17 PASS jobs / 0 FAIL`; diff-owned tests
`6 PASS / 0 FAIL / 0 SKIP`.

`skip_justification`: not applicable — zero skips.

`waiver_ref`: none required.
