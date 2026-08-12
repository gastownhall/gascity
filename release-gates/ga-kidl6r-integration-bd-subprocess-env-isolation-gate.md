# Release Gate: integration BdStore subprocess environment isolation

- Deploy bead: `ga-kidl6r`
- Review bead: `ga-5g48xu`
- Reviewed source: `1e2f4ec59a8ed53f1763b9ec61471c4d55989329`
- Evaluated source after bounded self-rebase: `70b7853e6ce8a62792d41dd3db9357ea9b64ad50`
- Base: `origin/main@a4e4cc2bfac251b65116d536addbb4a7be9d95cd`
- Deploy branch: `deploy/ga-kidl6r-gate`
- Verdict: **PASS**

`docs/PROJECT_MANIFEST.md` is absent from this repository, so this checklist
uses the seven release criteria defined by the active deployer protocol.

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | `ga-5g48xu` records round-2 `verdict: pass` for the exact reviewed source. The review reports no blockers, majors, or minors. |
| 2 | Acceptance criteria met | **PASS** | Independent diff read confirmed one integration-test isolation change: pinned `bd` subprocesses receive the exact test-owned environment and keep stderr separate from JSON stdout; `newIsolatedToolEnv` pins `HOME` while the supervisor-capable root preserves ambient `HOME`; the `bdstore` shard owns the five intended tests; the stale conformance skip is removed; and the session fixture uses the production bead type `session`. The resource-census baseline is internally consistent (`TestRepositoryLedgerMatchesCensusAndDocumentation` PASS). |
| 3 | Tests pass | **PASS** | See the detailed evidence below. Required commands were run once on `70b7853e6c` with the rootless Podman socket configured. All eight diff-owned top-level tests executed and passed; no diff-owned test skipped. `waiver_ref: none (no skip)`. |
| 4 | No high-severity review findings open | **PASS** | Round-2 review records zero blocking, major, or minor findings. Unresolved HIGH count: `0`. |
| 5 | Final branch is clean | **PASS** | `git status --short --branch` reported only `## deploy/ga-kidl6r-gate` before this checklist was added. `git diff --check` and `gofmt -l` on every changed Go file produced no output. |
| 6 | Branch diverges cleanly from main | **PASS** | No target PR already existed for the reviewed SHA. The canonical bounded helper rebased the isolated deploy branch from `1e2f4ec59a` to `70b7853e6c` and returned `0`; `origin/main` is now an ancestor of the evaluated head and the merge base is exactly `a4e4cc2bfa`. The helper pushed with `--force-with-lease`; the before/after SHAs are recorded on `ga-kidl6r`. |
| 7 | Single feature theme | **PASS** | The 11-file diff is confined to integration-test environment isolation, its shard/resource ledger, and the conformance fixture exposed when that coverage was restored. No independent production feature is bundled. |

## Test evidence

Environment:

- `DOCKER_HOST=unix:///run/user/$(id -u)/podman/podman.sock`
- `TESTCONTAINERS_RYUK_DISABLED=true`
- Rootless Podman socket confirmed present before the integration run.

Commands and counts:

- `LOCAL_TEST_JOBS=16 make test-fast-parallel`: **10 PASS jobs, 0 FAIL jobs**; all six `cmd/gc` unit shards, `unit-core`, filesystem cross-compile, and both concurrency/guard self-tests passed.
- `GOFLAGS=-v GO_TEST_TIMEOUT=30m ./scripts/test-integration-shard bdstore`: **52 PASS result lines, 0 FAIL, 0 SKIP** — five top-level tests plus all 47 `TestBdStoreConformance` subtests. The package passed in 684.588s.
- `go test -count=1 -v ./scripts/... -run '^(TestProviderOverridesAndSuiteContractsCrossMakeIsolation|TestBdStoreIntegrationShardOwnsRunnerIsolationAndAllCallSites)$'`: **8 PASS result lines, 0 FAIL, 0 SKIP** — two top-level tests and all six provider/shard-contract subtests. The unrelated `scripts/cipolicy` package had no name matching the anchored selector and is not part of the diff-owned tally.
- `go test -count=1 -tags integration -v ./test/integration -run '^TestNewIsolatedEnvRootPreservesAmbientHOME$'`: **1 PASS, 0 FAIL, 0 SKIP**.
- `go test -count=1 -v ./internal/testpolicy/resourcecensus/... -run '^TestRepositoryLedgerMatchesCensusAndDocumentation$'`: **1 PASS, 0 FAIL, 0 SKIP**.
- `go build ./...`: PASS.
- `go vet ./...`: PASS.

`diff_tests_executed`:

- `TestProviderOverridesAndSuiteContractsCrossMakeIsolation`: PASS (all 6 subtests PASS)
- `TestBdStoreIntegrationShardOwnsRunnerIsolationAndAllCallSites`: PASS
- `TestBdStoreDeleteBatchOrphansExternalDependents`: PASS
- `TestPinnedBdStoreCommandRunnerUsesExactEnvironmentAndKeepsStdoutJSON`: PASS
- `TestNewIsolatedToolEnvPinsHomeAwayFromAmbientBeadsConfig`: PASS
- `TestNewIsolatedEnvRootPreservesAmbientHOME`: PASS
- `TestBdStoreConformance`: PASS (all 47 subtests PASS, including `CreateEchoMatchesGetOnMetadata` and the previously load-sensitive `ReadyEmptyStore`)
- `TestBdStoreMailWispInsert`: PASS

`test_counts`: fast baseline `10 PASS jobs / 0 FAIL`; named Go results
`62 PASS / 0 FAIL / 0 SKIP` across the CI-equivalent shard, supplementary
diff-owned checks, and resource-census consistency check.

`skip_justification`: not applicable — zero skips.

`waiver_ref`: none required.
