# Release Gate: nested-worktree-safe composition-root censuses

Gate date: 2026-08-20

Deploy bead: ga-dmha63
Review bead: ga-d3h2oo
Source bead: ga-5em
Candidate branch: builder/ga-z0738h-f2322-resubmit (provenance only)
Reviewed commit: 442ece82560dbe93df789a2632598cb0acf1a13b
Base checked: origin/main @ 75b12a0461254034effb319db9b1509258a899f6
Merge base: 7c817e0640fae801631043005f1d54b17ce3e97c
Clean merge tree: bfdb82d89637267bc49850a02d7bd413ba7f2df9

Overall result: **FAIL — criterion 3**. The release evidence itself passed or
was attributable, but the required pre-push fast rerun exposed a second known
same-package timing failure that this deployer cannot self-attribute under the
four-clause rule. Nothing was pushed and no PR was opened.

`docs/PROJECT_MANIFEST.md` and `PROJECT_MANIFEST.md` are not present in this
checkout. This gate therefore applies the seven release criteria from the
active deployer role, the acceptance criteria on ga-5em, and the required-lane
guidance in `TESTING.md` and
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Summary

The composition-root and storage-boundary test censuses now enumerate
git-tracked Go files instead of walking every file below the checkout. This
prevents an untracked nested sibling worktree from being counted as duplicate
production source while preserving the existing invariants for genuine,
tracked duplicates.

The reviewed range is one feature theme: two nested-worktree regression tests,
the shared tracked-Go-file helper, the two census consumers, and the matching
resource-census ledger/documentation updates.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-d3h2oo` is closed with `verdict: pass` and pins reviewed commit `442ece82560dbe93df789a2632598cb0acf1a13b`. |
| 2 | Acceptance criteria met | PASS | Both synthetic nested-worktree regressions pass; all six originally failing census/boundary tests pass; `TestCensusSeesEveryKnownEvasion` preserves true-positive detection; the live resource-census ledger/documentation check passes; `go vet ./...` is clean. |
| 3 | Tests pass | FAIL | The evaluation lanes passed or were validly attributed: initial `make test-fast-parallel` had 9 PASS jobs plus one attributable cross-package failure; `make test-cmd-gc-process-parallel` had 7 PASS jobs; the focused acceptance run had 43 PASS events; policy and vet passed. However, the required pre-push fast rerun failed `TestCityRuntimeForceShutdownTearsDownAfterLateAsyncSweep`. It is tracked and not diff-owned, but this candidate also touches `cmd/gc` test files, so the required no-package-overlap clause is false. No mayor/operator waiver exists; bypass is forbidden. |
| 4 | No high-severity review findings open | PASS | Review notes contain no HIGH finding; unresolved HIGH count is 0. The only review observation is a non-blocking six-line helper duplication across two test packages. |
| 5 | Final branch is clean | PASS | Before this checklist was written, `git status --short` produced no output. `git diff --check origin/main...HEAD` and `gofmt -l` on every changed Go file also produced no output. This gate file is the deployer-authored change. |
| 6 | Branch diverges cleanly from main | PASS | After `git fetch origin main`, `git merge-tree --write-tree origin/main HEAD` exited 0 and produced `bfdb82d89637267bc49850a02d7bd413ba7f2df9`. The candidate is 2 commits ahead and 1 commit behind with no conflicts, so no bounded self-rebase was needed. |
| 7 | Single feature theme | PASS | The two commits and seven changed paths all serve tracked-file scoping for test censuses and its checked resource ledger. No independent product behavior is bundled. |

## Acceptance Checklist

- [x] `TestNoRuntimeRegistryAccess` passes.
- [x] `TestOSSProjectsNoUnregisteredBackendEnv`, including `registry`,
  `source`, and `projection`, passes.
- [x] `TestSingleCompositionRoot` passes.
- [x] `TestStorageProviderBundleHasOneConstructionSite` passes.
- [x] `TestStoreSetHasOneProducer` and `TestStoreSetPublicationSites` pass.
- [x] `TestModuleGoFilesExcludesUntrackedNestedWorktree` and
  `TestCensusSourcesFromExcludesUntrackedNestedWorktree` pass against synthetic
  nested-worktree fixtures.
- [x] `TestCensusSeesEveryKnownEvasion` passes, preserving failures for genuine
  tracked duplicate/evasion cases.
- [x] `TestRepositoryLedgerMatchesCensusAndDocumentation` passes without an
  update flag, proving the committed ledger and documentation match a live
  repository scan.
- [x] `go vet ./...` is clean.

## Test Evidence

### Required lanes

```text
make test-fast-parallel
10 jobs selected
9 PASS jobs, 1 FAIL job, 0 SKIP jobs
PASS: fsys-darwin-compile, local-concurrency-selftest,
      push-gate-lock-selftest, unit-cmd-gc-1-of-6 through 6-of-6
FAIL: unit-core (one attributed test; all other reported packages passed)

make test-cmd-gc-process-parallel
All cmd-gc-process jobs passed
7 PASS jobs, 0 FAIL, 0 SKIP
  - cmd-gc-process-1-of-6 through 6-of-6
  - productmetrics-testhook

make test-ci-policy
PASS: 5 runner-policy Python tests
PASS: 15 suite-coverage Python tests
PASS: scripts/cipolicy Go package
PASS: focused scripts static-scope policy tests

go vet ./...
PASS (no output)
```

### Diff-owned and acceptance tests

The focused `go test -json -count=1` run over `./cmd/gc`,
`./internal/storebinding`, and `./internal/testpolicy/resourcecensus` selected
the acceptance tests by name and reported 43 PASS events, 0 FAIL, 0 SKIP.

`diff_tests_executed`:

- `cmd/gc/module_scan_nested_worktree_test.go`:
  `TestModuleGoFilesExcludesUntrackedNestedWorktree` PASS.
- `cmd/gc/storage_provider_bundle_boundary_test.go`:
  `TestStorageProviderBundleHasOneConstructionSite` PASS;
  `TestCompiledStorageProviderRegistryIsFrozenAndExplicit` PASS;
  `TestStorageSurfaceDeclaresOnlySanctionedProviderIDs` PASS;
  `TestStorageSurfaceCompilesIdenticallyWithAndWithoutCGO` PASS;
  `TestModuleGraphCarriesNoReplaceDirective` PASS.
- `internal/storebinding/census_sources_nested_worktree_test.go`:
  `TestCensusSourcesFromExcludesUntrackedNestedWorktree` PASS.
- `internal/storebinding/builder_publication_census_test.go`:
  `TestStoreSetPublicationSites` PASS; `TestStoreSetHasOneProducer` PASS;
  `TestNoLinknameOrUnsafeEscape` PASS; `TestCensusSeesEveryKnownEvasion` PASS;
  `TestSingleCompositionRoot` PASS; `TestNoRuntimeRegistryAccess` PASS;
  `TestUnpublishedStoreSetUnreachable` PASS.

Additional acceptance coverage:

- `TestOSSProjectsNoUnregisteredBackendEnv` and all three subtests PASS.
- `TestRepositoryLedgerMatchesCensusAndDocumentation` PASS.

`skip_justification`: none; no diff-owned or required-lane job skipped.

`waiver_ref`: none. This absence is blocking for the same-package pre-push
failure described below.

### Attributed non-diff-owned fast-lane failure

`failure_attribution`:

- `internal/api/TestStatusListUnderADeadlineIsUnchangedByTheUnreadStoreNotice/notice_active`
  -> `ga-2eekbp`. The test returned `context deadline exceeded` after
  250.181349 ms during the `unit-core` load wave.
  - Not diff-owned: `internal/api/handler_status_unread_store_test.go` is not
    in the candidate diff.
  - Independently tracked before this gate: `ga-2eekbp` was filed on
    2026-08-18 from the same test failing in `unit-core` on an unrelated
    `internal/beads` change.
  - Proven pre-existing/structurally unreachable: the prior occurrence is on
    unrelated code, and `go list -deps ./internal/api` does not include the
    candidate's only changed production package,
    `internal/testpolicy/resourcecensus`.
  - No path overlap: the candidate touches `cmd/gc` test census files,
    `internal/storebinding` tests, `internal/testpolicy/resourcecensus`, and
    the resource ledger/docs; it touches no `internal/api` path.

All four non-diff-owned attribution clauses are satisfied. The failure is
recorded rather than hidden, and it does not change criterion 3 to FAIL.

### Blocking pre-push failure

The guarded `git push -u fork deploy/ga-dmha63-gate` ran another
`make test-fast-parallel` and stopped before updating the remote:

```text
9 PASS jobs, 1 FAIL job, 0 SKIP jobs
FAIL: unit-cmd-gc-5-of-6
TestCityRuntimeForceShutdownTearsDownAfterLateAsyncSweep:
  city_runtime_server_lifecycle_test.go:354:
  force shutdown missed the late async-started runtime
```

This is the known race tracked by open beads `ga-tt3qwa` and `ga-remp51`;
`ga-tt3qwa` records multiple identical failures on unrelated diffs. It still
cannot be attributed away here:

- Not diff-owned: PASS. The candidate does not modify
  `cmd/gc/city_runtime_server_lifecycle_test.go`.
- Tracked before this run: PASS (`ga-tt3qwa`, `ga-remp51`).
- Pre-existing recurrence: PASS. Both trackers predate this run and document
  the identical assertion on unrelated candidate heads.
- No package/path overlap: **FAIL**. The candidate changes
  `cmd/gc/module_scan_nested_worktree_test.go` and
  `cmd/gc/storage_provider_bundle_boundary_test.go`, which share the failing
  test's `cmd/gc` package. The tracker itself documents that this exact shape
  blocks self-service attribution for same-package diffs.

Because all four clauses are mandatory, criterion 3 is FAIL. The recurrence
also happened after tracking, making it a quarantine/waiver decision for the
merge authority rather than permission for the deployer to retry or bypass.
