# Release Gate: nested-worktree-safe composition-root censuses

Gate date: 2026-08-20

Deploy bead: ga-dmha63
Review bead: ga-d3h2oo
Source bead: ga-5em
Deploy branch: deploy/ga-dmha63-gate
Source branch: builder/ga-z0738h-f2322-resubmit (provenance only)
Reviewed commit: 442ece82560dbe93df789a2632598cb0acf1a13b
Re-gated candidate: b0dda74dd7477cdc9b5f2c324e3a4e7a45d65d52
Base checked: origin/main @ 75b12a0461254034effb319db9b1509258a899f6
Merge base: 7c817e0640fae801631043005f1d54b17ce3e97c
Clean merge tree: 60b479ed10aecdda538c2879f5f7df46142d6488

Overall result: **PASS — 7/7 criteria under explicit merge-authority
waivers**. The direct re-gate passed every required lane. Two guarded push
attempts later encountered unrelated, tracked scheduling failures; the mayor
reviewed each exact candidate and granted the scoped dispositions recorded
below. The final push is authorized without retrying those environmental
signatures to green.

`docs/PROJECT_MANIFEST.md` and `PROJECT_MANIFEST.md` are not present in this
checkout. This gate applies the seven release criteria from the active deployer
role, the acceptance criteria on ga-5em, and the required-lane guidance in
`TESTING.md` and
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Summary

The composition-root and storage-boundary test censuses now enumerate
git-tracked Go files instead of walking every file below the checkout. This
prevents an untracked nested sibling worktree from being counted as duplicate
production source while preserving the existing invariants for genuine,
tracked duplicates.

The reviewed range is one feature theme: two nested-worktree regression tests,
the shared tracked-Go-file helper, the census consumers, and the matching
resource-census ledger and test documentation.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-d3h2oo` is closed with `verdict: pass` and pins reviewed commit `442ece82560dbe93df789a2632598cb0acf1a13b`. |
| 2 | Acceptance criteria met | PASS | Both synthetic nested-worktree regressions pass; all census and storage-boundary acceptance tests pass; the live resource-census ledger agrees with the repository; `go vet ./...` is clean. |
| 3 | Tests pass | PASS | Direct re-gate: `make test-fast-parallel` passed 10/10 jobs; `make test-cmd-gc-process-parallel` passed 7/7 jobs; 43/43 focused acceptance and diff-owned test events passed; policy passed. The later guarded-push `TestProviderLiveClaudeKindPath` / `agent_pane_busy` failure is covered by mayor waiver `mayor-2026-08-20-ga-dmha63-c3b`, scoped to candidate `831e729eca4f81ac64bc8a6d62376b8fdb933fc7`. |
| 4 | No high-severity review findings open | PASS | Review notes contain no HIGH finding; unresolved HIGH count is 0. The only review observation is a non-blocking six-line helper duplication across two test packages. |
| 5 | Final branch is clean | PASS | Before this checklist edit, `git status --short`, `git diff --check`, and `gofmt -d` on every changed Go file produced no output. The checklist is the only deployer-authored change and is committed separately below. |
| 6 | Branch diverges cleanly from main | PASS | After `git fetch origin main`, `git merge-tree --write-tree origin/main b0dda74dd7477cdc9b5f2c324e3a4e7a45d65d52` exited 0 and produced `60b479ed10aecdda538c2879f5f7df46142d6488`. The candidate is 3 commits ahead and 1 behind with no conflicts; no bounded self-rebase was needed. |
| 7 | Single feature theme | PASS | The reviewed commits and changed paths all serve tracked-file scoping for test censuses and its checked resource ledger. No independent product behavior is bundled. |

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
  tracked duplicate and evasion cases.
- [x] `TestRepositoryLedgerMatchesCensusAndDocumentation` passes without an
  update flag, proving the committed ledger and documentation match a live
  repository scan.
- [x] `go vet ./...` is clean.

## Test Evidence

### Required lanes

```text
make test-fast-parallel
10 PASS jobs, 0 FAIL, 0 SKIP
  - fsys-darwin-compile
  - push-gate-lock-selftest
  - local-concurrency-selftest
  - unit-core
  - unit-cmd-gc-1-of-6 through unit-cmd-gc-6-of-6

make test-cmd-gc-process-parallel
7 PASS jobs, 0 FAIL, 0 SKIP
  - cmd-gc-process-1-of-6 through cmd-gc-process-6-of-6
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

Focused `go test -count=1 -v` runs over `./cmd/gc`,
`./internal/storebinding`, and `./internal/testpolicy/resourcecensus` reported
43 PASS events, 0 FAIL, 0 SKIP.

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

`skip_justification`: none; no test or required-lane job skipped.

`waiver_ref`:

- `mayor-2026-08-20-ga-dmha63-c3` covers the prior occurrence of
`TestCityRuntimeForceShutdownTearsDownAfterLateAsyncSweep` on candidate
`b0dda74dd7477cdc9b5f2c324e3a4e7a45d65d52`. The re-gate itself passed that
test.
- `mayor-2026-08-20-ga-dmha63-c3b` covers
  `TestProviderLiveClaudeKindPath` failing with `agent_pane_busy` on candidate
  `831e729eca4f81ac64bc8a6d62376b8fdb933fc7`. The mayor measured 24 live tmux
  panes on the host and ruled that this live-pane test measured fleet load,
  not the candidate's test-policy census change.

## Prior blocked attempt

The first push attempt was correctly stopped before updating the remote when
the pre-push fast lane hit the known
`TestCityRuntimeForceShutdownTearsDownAfterLateAsyncSweep` scheduling race.
Because this candidate also changes tests in package `cmd/gc`, the deployer
could not satisfy the no-package-overlap clause and did not self-attribute or
bypass the failure. The mayor reviewed the evidence, recorded the external
waiver above, lifted `hold:mayor`, and directed this clean re-gate. No branch or
PR was created remotely during the blocked attempt.

The re-gate passed locally, but the subsequent guarded fork push stopped on a
different pre-existing failure:

```text
make test-fast-parallel (pre-push)
9 PASS jobs, 1 FAIL job, 0 SKIP jobs
FAIL: unit-core
internal/runtime/herdr/TestProviderLiveClaudeKindPath:
  agent target pane w1:p1 is not an available shell (agent_pane_busy)
```

`failure_attribution` evidence:

- Not diff-owned: the reviewed range changes no file under
  `internal/runtime/herdr`.
- Tracked before this run: `ga-fh1flg` records this exact live-pane failure;
  `ga-nqlb8q` root-causes the server/socket mismatch that can cascade into a
  redundant server and `agent_pane_busy`.
- Structurally pre-existing: `go list -deps ./internal/runtime/herdr` contains
  none of the candidate's changed production package
  `internal/testpolicy/resourcecensus`; the remaining changed Go files are
  tests in other packages.
- No path overlap: the candidate changes `cmd/gc`, `internal/storebinding`,
  `internal/testpolicy/resourcecensus`, `TESTING.md`, and the test-resource
  ledger, not `internal/runtime/herdr`.

All four attribution facts are present. Because the same signature had blocked
multiple pushes after tracking, the deployer escalated instead of retrying or
self-authorizing a bypass. The mayor then granted
`mayor-2026-08-20-ga-dmha63-c3b` on the exact blocked candidate and directed
the deployer to proceed without retry-to-green. The corrective chain remains
in flight independently: PR #5437 is open for the name-isolation fix, and
deploy bead `ga-nsaq0t` carries the reviewed socket-path fix.
