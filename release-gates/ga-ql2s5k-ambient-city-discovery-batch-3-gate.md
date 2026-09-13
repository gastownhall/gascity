# Release Gate: Explicit city paths for command tests, batch 3

- Deploy bead: `ga-ql2s5k`
- Source review bead: `ga-xy2gbc`
- Reviewed commit: `48a40cd9ab4fa5d25b2eca14dd8826238faa4718`
- Gated commit after bounded self-rebase: `eabb4f7c771b464f5b09864f71671d5b2e69bc12`
- Base: `origin/main@1bc642727251a84a33d0316a0d3d26e3e9c11ffe`
- Deploy branch: `deploy/ga-ql2s5k-gate`
- Gate date: 2026-07-25
- Release criteria source: `docs/PROJECT_MANIFEST.md` is not present in this
  checkout, so this checklist uses the active deployer criteria and `TESTING.md`.

## Gate Results

Criterion 6 was evaluated first as required.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Source review bead `ga-xy2gbc` is closed with `REVIEW VERDICT: PASS`. The independent review covered the complete diff, build, vet, focused tests, security, coverage, CI integrity, and the parent-bead sequencing constraint. |
| 2 | Acceptance criteria met | PASS | The patch explicitly pins `GC_CITY_PATH` at 26 `main_test.go` sites, one `cmd_nudge_test.go` site, and five `cmd_formula_test.go` sites. `SetupHermeticCookEnv` now separates its working directory from the exact city root, and both repo-wide callers pass the correct pair. All 36 directly affected, indirectly affected, and guard-blind control tests passed. No production code changed. |
| 3 | Tests pass | PASS | On gated commit `eabb4f7c7`, `go build ./...` and `go vet ./...` passed; all 36 focused tests passed; and `make test-fast-parallel` passed all nine jobs (`unit-core`, six `unit-cmd-gc` shards, `fsys-darwin-compile`, and `push-gate-lock-selftest`). |
| 4 | No high-severity review findings open | PASS | The source review records only two non-blocking bookkeeping nits: the original base attribution and a verification-count typo. It records no unresolved HIGH, security, coverage, or CI-integrity finding. Deployer inspection found no additional high-severity concern in this test-only change. |
| 5 | Final branch is clean | PASS | Before this checklist was added, `git status --porcelain` produced no output, `git diff --check origin/main..HEAD` passed, and `gofmt -l` produced no output for all four changed files. This checklist is committed as the isolated deploy branch tip. |
| 6 | Branch diverges cleanly from main | PASS | The reviewed commit did not contain current main, so the canonical bounded helper replayed it from `48a40cd9ab` to `eabb4f7c77` on `origin/main@1bc6427272`. `git range-diff` reported the patch as identical, the lease-protected push to the isolated deploy branch succeeded, `git ls-remote` verified the remote at `eabb4f7c771b464f5b09864f71671d5b2e69bc12`, and a final fetch confirmed current main is its ancestor. |
| 7 | Single feature theme | PASS | The commit changes three `cmd/gc` test files and their shared test helper for one purpose: make city selection explicit instead of relying on ambient upward discovery. |

## Acceptance Checks

- PASS: Twenty-six prime/session-hook test sites in `cmd/gc/main_test.go` pin
  their temporary city explicitly.
- PASS: `TestCmdSessionNudgeQueueResolvesSessionName` pins its temporary city
  explicitly.
- PASS: Three direct formula-cook sites pin their city explicitly.
- PASS: Both formula-cook helper call sites preserve cwd-based rig selection
  while passing the exact city root through `GC_CITY_PATH`.
- PASS: `SetupHermeticCookEnv` has exactly two repo-wide callers, and both use
  the new `chdirDir, cityRootDir` contract.
- PASS: The change remains test-only: 48 insertions and 7 deletions across
  three test files and `internal/formulatest/env.go`.
- PASS: Parent bead `ga-klo4gz` remains sequenced after the full migration;
  this batch does not clear the ambient-discovery guard to land.

## Commands

```text
git range-diff 2c4c43b270d72365a0080530b0c3e3503f898e7d..48a40cd9ab4fa5d25b2eca14dd8826238faa4718 1bc642727251a84a33d0316a0d3d26e3e9c11ffe..eabb4f7c771b464f5b09864f71671d5b2e69bc12
git diff --check origin/main..HEAD
gofmt -l cmd/gc/main_test.go cmd/gc/cmd_nudge_test.go cmd/gc/cmd_formula_test.go internal/formulatest/env.go
go build ./...
go vet ./...
go test -count=1 -run '^(TestCmdSessionNudgeQueueResolvesSessionName|TestDoPrimeBareName|TestDoPrimeCodexHookPersistsProviderSessionKeyFromHookStdin|TestDoPrimeFallsBackToGCAliasWhenGCAgentUnresolvable|TestDoPrimeFormulaV2GraphWorkerPromptClaimsRoutedWork|TestDoPrimeGeminiHookPersistsProviderSessionKey|TestDoPrimeHookDoesNotCreateRuntimeSessionSidecar|TestDoPrimeHookFallsBackToGCTemplateForManualSessionAlias|TestDoPrimeHookFallsBackToSessionTemplateForManualSessionAlias|TestDoPrimeHookIgnoresProviderSessionKeyFromHookStdinForNonCodex|TestDoPrimeHookPersistsGenericProviderSessionKey|TestDoPrimeHookWarnsWhenRequiredProviderSessionKeyMissing|TestDoPrimeNoArgs|TestDoPrimePoolAgentFallback|TestDoPrimeStrictAbsoluteTemplatePath|TestDoPrimeStrictAgentWithEmptyPromptTemplate|TestDoPrimeStrictHookModeDoesNotCreateRuntimeSidecarOnSuccess|TestDoPrimeStrictHookModeMissingTemplateDoesNotUpdateProviderMetadataOnFailure|TestDoPrimeStrictHookModeOnSuspendedAgentDoesNotCreateRuntimeSidecar|TestDoPrimeStrictHookModeUnknownAgentDoesNotCreateRuntimeSidecar|TestDoPrimeStrictKnownAgent|TestDoPrimeStrictMissingTemplateFile|TestDoPrimeStrictNoAgentName|TestDoPrimeStrictNoCity|TestDoPrimeStrictTemplateRendersLegitimatelyEmpty|TestDoPrimeStrictUnknownAgent|TestDoPrimeStrictUnreadableTemplateFile|TestDoPrimeUsesGCAgentEnv|TestDoPrimeWithDiscoveredCityAgent|TestDoPrimeWithKnownAgent|TestDoPrimeWithUnknownAgent|TestFormulaCookAttachGraphV2AllowsDifferentLiveBareBeadRoots|TestFormulaCookAttachGraphV2CreatesFreshRootForBareBeadTarget|TestFormulaCookAttachGraphV2RejectsLiveLegacySourceWorkflow|TestFormulaCookStandaloneGraphV2StampsRunRootStoreScope|TestFormulaCookStandaloneGraphV2StampsRunRootStoreScopeForRig)$' -v ./cmd/gc
make test-fast-parallel
```
