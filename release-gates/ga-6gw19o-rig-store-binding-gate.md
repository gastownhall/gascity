# Release gate: detect missing and collapsed rig stores

**Deploy bead:** `ga-6gw19o`

**Implementation bead:** `ga-e5lyfu`

**Review bead:** `ga-f7kdwt`

**Reviewed source:** `b71bff88a53e14948a2ff36536a764770b0d3634`

**Base checked:** `origin/main@2c4780ccf6dc444b5c50b0de9a5fc4931604d82e`

**Deploy mode:** remote

**Overall result:** **PASS**

The mandatory already-merged pre-flight found no pull request carrying the
reviewed commit, so the normal gate proceeded. `docs/PROJECT_MANIFEST.md` is
absent from this repository; the active deployer criteria and `TESTING.md`
therefore define the release gate.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Reviewer re-review recorded `Review verdict: PASS` at the exact reviewed source after the gate-rebase round. SHA resolution and the remote source tip both matched `b71bff88a53e14948a2ff36536a764770b0d3634`. |
| 2 | Acceptance criteria met | **PASS** | Missing-database, collapsed-store, frozen-store, restore, and `started_at` behavior are exercised by named tests below. The candidate was also run read-only against the live city: active `MCDClient` and `beads` stores produced blocking below-80%-of-export diagnostics, while `gascity` passed. The originally recorded `tincan` (0 rows) and `ProjectWrenUnity` (4 rows) observations remain in review evidence; both rigs are currently suspended and are intentionally skipped by doctor. |
| 3 | Tests pass | **PASS** | CI-equivalent coverage is a composite of the documented `make test-local-full-parallel` 40-job sweep plus corrected reruns of three host-contaminated jobs: **40 job scopes PASS, 0 FAIL**. Nine diff-owned tests passed by name, 0 failed, 0 skipped. `diff_tests_executed` is listed below; `waiver_ref: none`. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected` (fail-safe full-repository selection, 0 issues), `make fmt-check-changed`, and `make check-docs` all passed. `go build ./...` and `go vet ./...` also passed. |
| 4 | No high-severity review findings open | **PASS** | Reviewer re-review reported no security, specification, or correctness findings; unresolved HIGH count = 0. |
| 5 | Final branch is clean | **PASS** | The reviewed candidate checkout was clean before the gate artifact was written. The gate file is the only intended deploy-only change and is committed on the isolated branch. |
| 6 | Branch diverges cleanly from main | **PASS** | Final refresh: `git merge-tree --write-tree origin/main b71bff88a53e14948a2ff36536a764770b0d3634` exited 0 against `origin/main@2c4780ccf6dc444b5c50b0de9a5fc4931604d82e` and produced tree `17cf846612e36680d839f7a9f29eaa2126867368`. Merge base: `6e7fb75304bbf930d194ab6c4b5fdb0ef4180cb1`. No self-rebase was required. |
| 7 | Single feature theme | **PASS** | The commit set is one store-integrity theme: `gc doctor` validates each active managed rig's real database and export baseline, the Dolt adoption timestamp stops fabricating evidence, and the resource-census changes account for the new integration tests. |

## Acceptance evidence

1. A missing configured database fails and identifies rig, database, and
   endpoint: `TestRigStoreBindingCheck_DatabaseAbsentFails` PASS.
2. A severe row-count collapse fails against the most recent accepted export,
   and a non-advancing store warns as frozen:
   `TestRigStoreBindingCheck_EmptyStoreUnderExportBaselineFails` PASS and
   `TestRigStoreBindingCheck_FrozenStoreWarns` PASS.
3. Restoring the database turns the same check green:
   `TestRigStoreBindingCheck_DatabaseRestoredPasses` PASS; the healthy baseline
   case `TestRigStoreBindingCheck_HealthyStorePasses` also passed.
4. Read-only live candidate run produced the intended diagnostics on active
   collapsed stores:
   - `MCDClient`: 1,629 live issues vs 5,149 in the accepted export — ERROR.
   - `beads`: 2,535 live issues vs 3,282 in the accepted export — ERROR.
   - `gascity`: 18,232 live issues vs 18,280 in the accepted export — OK.
   The acceptance record's earlier `tincan` and `ProjectWrenUnity` observations
   cannot be repeated while those rigs are suspended; the real-Dolt tests cover
   their missing/empty shapes without changing live suspension state.
5. Adopted Dolt state preserves a real prior timestamp and leaves an unknown
   timestamp empty rather than inventing one:
   `TestRepairedManagedDoltRuntimeStatePreservesStartedAt` PASS, including both
   subtests. Symlink-equivalent data directories also passed both subtests in
   `TestRepairedManagedDoltRuntimeStateAcceptsSymlinkEquivalentDataDir`.

## Test evidence

Environment prepared before tests:

- rootless Podman socket at `/run/user/1000/podman/podman.sock` with
  `TESTCONTAINERS_RYUK_DISABLED=true`;
- cached `dolthub/dolt-sql-server:2.1.7` image and local Dolt 2.2.3;
- repository-pinned `bd` v1.1.0 (`8e4e59d39f34`) and tmux 3.4 for CI parity.

Documented full lane:

```text
LOCAL_TEST_JOBS=1 GO_TEST_TIMEOUT=30m make test-local-full-parallel
composite result: 40 job scopes PASS, 0 FAIL
```

The serialized runner passed 37 jobs directly. Its remaining three job results
were contaminated by host-global state, and each failing scope was rerun rather
than waived:

- `unit-core` was rerun in a non-login, sanitized CI environment with pinned
  `bd`; every package passed.
- `scripts/test-integration-shard packages-core-4-of-4` was rerun in the same
  environment; all 46 packages passed.
- `TestCleanInstallTutorialPath` was rerun after the global circuit-breaker
  cleanup noise and passed (it also passed an earlier isolated rerun).

The CI-shaped unit run has one unrelated, documented optional skip:
`TestProviderLiveClaudeKindPath` skips when `herdr` or `claude` is unavailable,
as they are in CI. This diff does not touch Herdr. The same test was additionally
run with both provider binaries present and passed when its shared pane was
available.

Diff-owned named results (`9 PASS, 0 FAIL, 0 SKIP`):

- `TestBuildDoctorChecksRegistersRigStoreBindingCheck` — PASS
- `TestBuildDoctorChecksSkipsRigStoreBindingCheckWithSkipRigDoltChecks` — PASS
- `TestRepairedManagedDoltRuntimeStateAcceptsSymlinkEquivalentDataDir` — PASS
- `TestRepairedManagedDoltRuntimeStatePreservesStartedAt` — PASS
- `TestRigStoreBindingCheck_DatabaseAbsentFails` — PASS
- `TestRigStoreBindingCheck_DatabaseRestoredPasses` — PASS
- `TestRigStoreBindingCheck_EmptyStoreUnderExportBaselineFails` — PASS
- `TestRigStoreBindingCheck_FrozenStoreWarns` — PASS
- `TestRigStoreBindingCheck_HealthyStorePasses` — PASS

Additional changed-ledger contract:
`TestRepositoryLedgerMatchesCensusAndDocumentation` — PASS.

## Scope and disposition

`assert_deploy_ancestry_scope` passed for the deploy and implementation bead
IDs (`ga-6gw19o`, `ga-e5lyfu`): no denylisted `.claude/**` paths and no unrelated
commit ancestry. Gate PASS; prepare the isolated `deploy/ga-6gw19o-gate`
branch, push it, open a pull request, publish deploy clearance on the exact PR
head, and route merge authority to mayor. The deployer does not merge.
