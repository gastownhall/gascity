# Release gate: runtime-provider waiver expiry ownership

- Deploy bead: `ga-rt0f61`
- Build bead: `ga-glwaz5.1`
- Review bead: `ga-wadtbn`
- Reviewed source: `e10dcfcb7c84f725981ecd473b020e972983c495`
- Base: `origin/main@3363c31b5dbe4b174295c93727fb4125098fafcd`
- Deploy mode: remote
- Push remote: `origin`
- Gate result: **PASS**

See "Post-gate amendment" below: criterion 2's ownership premise is corrected.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Closed review bead `ga-wadtbn` records `verdict: pass` for exact source `e10dcfcb7c84f725981ecd473b020e972983c495`. |
| 2 | Acceptance criteria met | **PASS** | The generated runtime-provider ledger is in sync; the dead `ga-80po0c` owner is absent; `ga-uz5t3a` owns all eight runtime waivers; and each `waivedRuntime` call carries its own `time.Date(2026, September 7, ...)` literal rather than a shared expiry variable. Details below. |
| 3 | Tests pass | **PASS with attributed failures** | Fresh full-scope CI-equivalent run completed 28/40 jobs PASS, 12/40 jobs FAIL, with 14 failing functions and 0 observed SKIP. Every raw failure is predating-tracked or covered by the signature-scoped `ga-6bnc42` standing authorization, has no diff path overlap, and cannot import or execute the isolated `internal/testutil/providerledger` package. The affected package independently passed 37 PASS, 0 FAIL, 0 SKIP. |
| 3b | Policy/lint lane | **PASS with attributed baseline failure** | Policy, docs, build, vet, native-DoltLite, changed formatting, and affected lint all pass. The native dependency surface remains above its tracked host baseline (`270281232 > 270000000`, `ga-5flk3r`); providerledger is not imported by the `gc` binary and cannot affect its dependency graph. |
| 4 | No unresolved HIGH review findings | **PASS** | Review bead `ga-wadtbn` records no style, security, spec, or HIGH-severity finding. |
| 5 | Final branch clean | **PASS** | Exact reviewed source was clean before this gate record; `git diff --check origin/main...HEAD` passed. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree --messages origin/main HEAD` returned zero conflicts and tree `b4ca93774562ea8ddddd99419d2b8d2c82d578c3`. The source is 7 commits behind and 1 ahead of current main. |
| 7 | Single feature theme | **PASS** | One commit updates the provider-contract ledger implementation, its tests, and the generated `TESTING.md` table for the same waiver-ownership/expiry change. |

The mandatory source-ancestry guard also passes:

```text
assert_deploy_ancestry_scope origin/main e10dcfcb7c84f725981ecd473b020e972983c495 ga-rt0f61 ga-glwaz5.1 ga-wadtbn
ANCESTRY_SCOPE=PASS
```

## Acceptance evidence

1. `go test -count=1 -run '^TestCatalogMatchesProductionWiringAndDocumentation$' -v ./internal/testutil/providerledger/...` passes.
2. `git grep 'ga-80po0c' HEAD -- internal/testutil/providerledger` returns no match.
3. `runtimeWaiverExpiry` is absent. The eight production waiver claims each pass
   an expiry literal directly to `waivedRuntime`, and the generated table names
   `ga-uz5t3a` through `2026-09-07` for all eight runtime constructors.

## Test evidence

- `test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 make test-local-full-parallel`
- `test_cmd_scope: full-suite`
- `test_counts: 28 PASS jobs, 12 FAIL jobs, 0 observed SKIP jobs`
- `failing_test_functions: 14`
- `full_log: /var/tmp/ga-rt0f61-full-suite-rerun2-20260825.log`
- `shard_logs: /var/tmp/gc-local-tests.6k2JJM`
- `affected_package_cmd: go test -count=1 -v ./internal/testutil/providerledger/...`
- `affected_package_counts: 37 PASS, 0 FAIL, 0 SKIP`
- `affected_package_log: /var/tmp/ga-rt0f61-providerledger-verbose-rerun2.log`
- `diff_tests_executed: TestValidateRejectsInvalidContractClaims PASS; TestCatalogBindsFakeAndBothSubprocessConstructors PASS; TestCatalogBindsACPWithDirAndDefersDefaultConstructor PASS; TestCatalogBindsExecCompositionToSeamBackedContract PASS; TestCatalogReturnsIndependentEntries PASS; TestCatalogMatchesProductionWiringAndDocumentation PASS; all other top-level tests in the modified ledger_test.go PASS`
- `waiver_ref: ga-6bnc42` for the exact beads#4566 dirty-table schema-migration signature

The candidate changes only `TESTING.md` and the self-contained
`internal/testutil/providerledger` package. `git grep` finds no import of that
package anywhere else in the repository. Therefore none of the failing test
packages can execute changed production code, satisfying attribution clause
3(a), and every row also has no path overlap. The diff adds no test target and
does not change resource-census baselines.

| Raw failing test(s) | Tracker / authority | Disposition and proof |
|---|---|---|
| `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` | `ga-ukteq1`; `ga-6bnc42` | Exact beads#4566 dirty-table signature (`events`). **FAIL-WAIVED** under the standing signature authorization; providerledger is unreachable. |
| `TestBdStoreMailWispInsert` | `ga-sxtkmu` (successor deploy `ga-piz22t`) | Exact test-local Dolt readiness timeout, predating this run; providerledger is unreachable. |
| `TestSessionReconcilerTraceGH1654WorkRequestedStartCandidates/named_session_post-kill` | `ga-hgjlhi` | Exact async-start timeout under shard contention; providerledger is unreachable. |
| `TestCompactScriptRealDoltRemotePush` | `ga-ok3q3c` | Predating real-Dolt lifecycle/readiness flake in this exact test; providerledger is unreachable. |
| `TestBdFlagManifestCurrent` | `ga-f0uceo` | Installed-`bd` flag-manifest drift; providerledger is unreachable. |
| `TestGetKeyBinding_CapturesDefaultBinding`, `TestGetKeyBinding_CapturesDefaultBindingWithArgs` | `ga-afqddr` | Exact host-tmux empty-default-binding class; providerledger is unreachable. |
| `TestRunSetupCommandActivityStreamingSurvivesIdleWindow` | `ga-886gn1` | Exact progress-producing command killed by idle timeout; providerledger is unreachable. |
| `TestE2E_SuspendResume_City` | `ga-yc0e3a` | Exact missing `citysus.report` lifecycle signature; providerledger is unreachable. |
| `TestAdoptPRFormulaCompileAndRun`, `TestPersonalWorkFormulaCompileAndRun`, `TestAdoptPRFormulaRetriesTransientReviewerStep`, `TestGraphWorkflowSuccessPath`, `TestCleanInstallTutorialPath` | `ga-lpfjhc`; standing authorization `ga-6bnc42` | Each failed during fixture `gc init` with the exact beads#4566 dirty-table schema-migration signature. Preserved as **FAIL-WAIVED**, not converted to green. Providerledger is unreachable. |

`failure_attribution`: every non-waived row uses clause 3(a), structural
mechanism/import reachability, with a tracker that predates the run. The
beads#4566 rows use the mayor's prior signature-scoped standing authorization
recorded on `ga-6bnc42`, whose conditions are satisfied because this diff
cannot reach store bootstrap or schema migration.

## Policy and static evidence

```text
make test-ci-policy                                      PASS
make check-gomod-replace                                 PASS
make check-eventexport-isolation                         PASS
make check-core-boundary                                 PASS
make check-docs                                          PASS
go build ./...                                           PASS
go vet ./...                                             PASS
make test-native-doltlite-beads                          PASS
LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=HEAD^ make fmt-check-changed  PASS
LINT_CHANGED_REF=HEAD^ make lint-affected                PASS (0 issues)
make check-native-dependency-surface                     FAIL-ATTRIBUTED: 270281232 > 270000000 (`ga-5flk3r`)
```

## Release disposition

**Gate PASS.** Cut the isolated `deploy/ga-rt0f61-gate` branch from the exact
reviewed source, commit this checklist, push that isolated branch, open the PR,
publish `release-gate/deploy-clearance=success` on the exact PR head, and route
the merge-request to the merge authority. The deployer does not merge.

## Post-gate amendment — the ownership swap was backwards (ga-lgizl)

The fable review council on PR #5610 found criterion 2's premise inverted, and
`bd` confirms it against this repository's store:

```text
bd show ga-uz5t3a   Error fetching ga-uz5t3a: no issue found matching "ga-uz5t3a"
bd show ga-zzzzzzz  Error fetching ga-zzzzzzz: no issue found matching "ga-zzzzzzz"   (control)
bd show ga-80po0c.3 ○ ga-80po0c.3 · H5: contract production runtime compositions once  [P2 · OPEN]
```

**Correction to criterion 2.** `ga-80po0c` is not dead. It is an OPEN epic whose
child `ga-80po0c.3` — "contract production runtime compositions once" — is
exactly the work these eight waivers wait on, and 36 of the 37 rows in
`test/test-resources.toml` still name it. `ga-uz5t3a` resolves to nothing here;
it appears to live in the maintainer city's separate ledger. The swap moved the
runtime waivers from a live owner to a string, and the accompanying test change
replaced three literal-owner assertions with comparisons against the constant
that populates the field, so the check became self-referential and could no
longer fail.

The gate's other criteria stand: the ledger was in sync, the expiry literals
were correctly de-shared, and the package passed. Only the ownership premise was
wrong.

**Repaired by `ga-lgizl`.** `runtimeContractWaiverOwner` is back to
`ga-80po0c.3`, and `TestRuntimeWaiverOwnerIsPinnedAndWellFormed` pins that
literal in one place so re-owning all eight waivers is a reviewed edit rather
than a constant rename. The mutation the review council used to expose the hole
— renaming the constant to `ga-DEADBEEF` and updating the eight `TESTING.md`
rows to match — now fails the package.
