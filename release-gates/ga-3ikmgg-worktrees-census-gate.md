# Release gate: skip sibling `worktrees/` in source-census tests

- Deploy bead: `ga-3ikmgg`
- Build bead: `ga-c72bnw`
- Review bead: `ga-dogxrx`
- Reviewed commit: `38ea36688ce230ccb8230c82787a7ce1c411e275`
- Deploy mode: `remote`
- Base: `origin/main@aafe756142e70a54995b412de4c0adfad984fe9a`
- Gate result: **PASS**

The repository does not contain `docs/PROJECT_MANIFEST.md`; this gate applies
the deployer release criteria together with
`engdocs/contributors/release-gate-criteria-conventions.md` and the build bead's
acceptance criteria.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-dogxrx` is closed with `verdict: pass` for the exact reviewed commit. |
| 2 | Acceptance criteria met | **PASS** | A temporary `worktrees/ga-deploy-gate-sibling/` tree containing duplicate census-triggering Go sources was present while every affected helper consumer ran. All 13 top-level tests passed: 7 in `internal/storebinding` and 6 in `cmd/gc`; 0 failed and 0 skipped. The fixture was moved out of the checkout afterward. |
| 3 | Tests pass | **PASS WITH ATTRIBUTED FAILURES** | `make test-local-full-parallel` completed all 40 required jobs: 33 PASS, 7 FAIL, 0 SKIP. Every failure is outside the changed packages and structurally unreachable from this test-only diff; each is tied to an open tracker below. The three beads#4566 failures are preserved as **FAIL — WAIVED** under the mayor standing authorization recorded on `ga-lpfjhc`. `make test-ci-policy`, `go vet ./...`, formatting, all six `cmd/gc` process shards, all six integration `cmd/gc` shards, and all diff-relevant focused tests passed. |
| 4 | No high-severity review findings open | **PASS** | Reviewer recorded `style_findings: none`, `security_findings: none`, `uncovered_criteria: none`, and final verdict PASS. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | **PASS** | `git status --short --branch` at the reviewed commit was clean before this checklist was written. The synthetic fixture was removed from the checkout. |
| 6 | Branch diverges cleanly from main | **PASS** | After a fresh fetch, `git merge-tree --write-tree origin/main 38ea36688ce230ccb8230c82787a7ce1c411e275` exited 0 and produced tree `628c2902e0289f9f026ae79f124e023264d693fc`. No self-rebase was needed. The reviewed commit has no associated GitHub PR. |
| 7 | Single feature theme | **PASS** | One test-infrastructure theme across two files: module-root census walkers skip sibling agent `worktrees/` directories. No production code changes. |

## Test evidence

### Diff-relevant acceptance run

The synthetic sibling tree contained copies of production files that would
produce duplicate construction/publication findings if a module-root walker
entered it.

Commands:

```text
GOFLAGS= GOENV=off GOWORK=off go test -count=1 -json ./internal/storebinding/... -run '^(TestStoreSetPublicationSites|TestStoreSetHasOneProducer|TestNoLinknameOrUnsafeEscape|TestCensusSeesEveryKnownEvasion|TestSingleCompositionRoot|TestNoRuntimeRegistryAccess|TestUnpublishedStoreSetUnreachable)$'
GOFLAGS= GOENV=off GOWORK=off go test -count=1 -json ./cmd/gc/... -run '^(TestStorageProviderBundleHasOneConstructionSite|TestCompiledStorageProviderRegistryIsFrozenAndExplicit|TestStorageSurfaceDeclaresOnlySanctionedProviderIDs|TestModuleGraphCarriesNoReplaceDirective|TestOSSProjectsNoUnregisteredBackendEnv|TestStorageRegistryConstructorHasOneCaller)$'
```

Counts: **13 PASS, 0 FAIL, 0 SKIP**.

`diff_tests_executed`:

- `TestStoreSetPublicationSites` — PASS
- `TestStoreSetHasOneProducer` — PASS
- `TestNoLinknameOrUnsafeEscape` — PASS
- `TestCensusSeesEveryKnownEvasion` — PASS
- `TestSingleCompositionRoot` — PASS
- `TestNoRuntimeRegistryAccess` — PASS
- `TestUnpublishedStoreSetUnreachable` — PASS
- `TestStorageProviderBundleHasOneConstructionSite` — PASS
- `TestCompiledStorageProviderRegistryIsFrozenAndExplicit` — PASS
- `TestStorageSurfaceDeclaresOnlySanctionedProviderIDs` — PASS
- `TestModuleGraphCarriesNoReplaceDirective` — PASS
- `TestOSSProjectsNoUnregisteredBackendEnv` — PASS
- `TestStorageRegistryConstructorHasOneCaller` — PASS

The diff modifies helper functions in two existing `_test.go` files; it adds or
renames no `Test*` function. The list above covers every top-level consumer of
the modified helpers.

Logs:

- `/var/tmp/ga-3ikmgg-storebinding-focused.json`
- `/var/tmp/ga-3ikmgg-cmdgc-focused.json`

### Required full union and policy lanes

- `make test-local-full-parallel` — **33 PASS jobs, 7 FAIL jobs, 0 SKIP jobs**
- `make test-ci-policy` — **PASS**
- `go vet ./...` — **PASS**
- `gofmt -l cmd/gc/storage_provider_bundle_boundary_test.go internal/storebinding/builder_publication_census_test.go` — **PASS**, no output
- Rootless Podman configured through `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock` with `TESTCONTAINERS_RYUK_DISABLED=true`; socket and rootless runtime verified before testing.

Full-union log: `/var/tmp/ga-3ikmgg-test-local-full.log`; per-job logs:
`/var/tmp/gc-local-tests.dlWrLT/`.

`skip_justification`: none required; focused tests and full-union jobs reported
zero skips.

`waiver_ref`: `ga-lpfjhc` notes, **MAYOR RULING 2026-08-18**, for the exact
gastownhall/beads#4566 dirty-schema signature only. The occurrence was logged
on `ga-lpfjhc` before this gate was signed.

### Failure attribution

The candidate changes only
`cmd/gc/storage_provider_bundle_boundary_test.go` and
`internal/storebinding/builder_publication_census_test.go`. Both are test-only
files. They cannot enter the production `gc` binary launched by
`test/integration`, cannot affect `internal/bdflags`, and cannot affect
`internal/runtime/tmux`. This structural mechanism proof establishes that the
failures are pre-existing without a probabilistic base-ref rerun. None of the
failing packages overlaps either changed package except that the candidate's
`cmd/gc` test package itself was exercised by twelve clean required shards.

- `TestBdFlagManifestCurrent` — **FAIL, ATTRIBUTED** to `ga-f0uceo`; installed
  `bd` flag-manifest skew; no diff/package overlap.
- `TestGetKeyBinding_CapturesDefaultBinding` — **FAIL, ATTRIBUTED** to
  `ga-afqddr`; host tmux 3.7b default-key lookup; no diff/package overlap.
- `TestGetKeyBinding_CapturesDefaultBindingWithArgs` — **FAIL, ATTRIBUTED** to
  `ga-afqddr`; same exact host-tmux signature; no diff/package overlap.
- `TestAdoptPRFormulaCompileAndRun` — **FAIL — WAIVED** under `ga-lpfjhc`;
  beads#4566 dirty `custom_statuses`/`custom_types` during fixture bootstrap;
  no schema-migration/store-bootstrap mechanism in the diff.
- `TestAdoptPRFormulaRetriesTransientReviewerStep` — **FAIL — WAIVED** under
  `ga-lpfjhc`; beads#4566 dirty `compaction_snapshots` during fixture bootstrap;
  no schema-migration/store-bootstrap mechanism in the diff.
- `TestCleanInstallTutorialPath` — **FAIL, ATTRIBUTED** to `ga-hrdd3h`;
  circuit-breaker cleanup diagnostics polluted `bd config get issue_prefix`
  output; no diff/package overlap.
- `TestGraphWorkflowFailureRunsCleanup` — **FAIL — WAIVED** under `ga-lpfjhc`;
  beads#4566 dirty `comments`/`issues` during fixture bootstrap; no
  schema-migration/store-bootstrap mechanism in the diff.

