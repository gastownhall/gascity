# Release gate: idle pool members wake for shared-template work

- Deploy bead: `ga-z1axa6`
- Build bead: `ga-8vz95k.6`
- Review bead: `ga-ofn9oc`
- Reviewed commit: `f60f2f936dda31d798bc6e1e4e2dd5c57944b481`
- Base checked: `origin/main@3b6ab2351615c95d6b2f00e63911a14dd55fe67c`
- Gate result: **PASS**

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-ofn9oc` is closed with verdict `pass` for the exact reviewed commit. |
| 2 | Acceptance criteria met | **PASS** | Ready open work assigned to a pool template wakes an eligible configured pool member. Blocked, deferred, terminal, and otherwise-not-ready work does not. In-progress work still requires a concrete holder identity. Membership comes from typed `pool_managed` metadata; numeric suffixes, configured named sessions, manual sessions, and members of other pools cannot impersonate membership. |
| 3 | Tests pass | **PASS with attributed raw failures** | The documented full local CI union ran all 40 jobs with rootless Podman enabled: **46,976 PASS / 9 FAIL / 189 SKIP**. All five diff-owned tests passed in both required `cmd/gc` executions; none skipped. All nine raw failures are preserved and attributed below to open trackers that predate the run. |
| 4 | No high-severity review findings open | **PASS** | Reviewer reported no security, style, or specification findings and no unresolved HIGH findings. |
| 5 | Final branch is clean | **PASS** | `git diff --check origin/main...HEAD` passed. The gate file is the only deploy-only addition and is committed on the isolated branch. |
| 6 | Branch diverges cleanly from main | **PASS** | After fetching `origin/main@3b6ab2351615c95d6b2f00e63911a14dd55fe67c`, `git merge-tree --write-tree origin/main f60f2f936dda31d798bc6e1e4e2dd5c57944b481` completed without conflict and produced tree `5d26a061343628b185479b8207e73b6192091b78`. No bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The two reviewed commits and three changed files form one awake-set behavior change: distinguish pool-template serviceability for ready work from concrete ownership of claimed work. |

## Test evidence

`test_cmd`:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/ga-z1axa6-full-gate make test-local-full-parallel
```

- `test_cmd_scope: full-suite`
- `test_counts: 46,976 PASS / 9 FAIL / 189 SKIP`
- `diff_tests_executed:`
  - `TestAwakeSetReadyPoolTemplateAssignmentWakesEligibleMember` — PASS in `cmd-gc-process-3-of-6` and `integration-packages-cmd-gc-5-of-6`
  - `TestAwakeSetPoolTemplateAssignmentRequiresReadyDemand` — PASS in `cmd-gc-process-4-of-6` and `integration-packages-cmd-gc-6-of-6`
  - `TestAwakeSetPoolInProgressOwnershipRemainsConcrete` — PASS in `cmd-gc-process-5-of-6` and `integration-packages-cmd-gc-1-of-6`
  - `TestAwakeSetPoolTemplateServiceabilityRequiresConfiguredMembership` — PASS in `cmd-gc-process-6-of-6` and `integration-packages-cmd-gc-2-of-6`
  - `TestBuildAwakeInputFromReconcilerCarriesConfiguredPoolMembership` — PASS in `cmd-gc-process-1-of-6` and `integration-packages-cmd-gc-3-of-6`
- `waiver_ref: none` for diff-owned tests
- `skip_justification:` all skips are existing platform/build-tag, explicit opt-in, credential, real-tmux, or unavailable-integration guards. No test added or modified by this diff skipped.

### Raw failure attribution

The suite's non-zero exit is preserved. Criterion 3 passes only because every raw failure satisfies the repository's non-diff-owned failure rule.

| Raw failure | Disposition | Evidence |
|---|---|---|
| `TestCatalogMatchesProductionWiringAndDocumentation` (two executions) | FAIL — ATTRIBUTED to `ga-1s16pf` | Clause 3(a), mechanism: the result is determined by the provider-ledger waiver catalog's date (`2026-08-26`). This diff has no providerledger path or mechanism overlap. |
| `TestBdFlagManifestCurrent` | FAIL — ATTRIBUTED to `ga-f0uceo` | Clause 3(a), mechanism: installed `bd` flags exceed the checked manifest. The diff changes neither the installed binary nor `internal/bdflags`. |
| `TestGetKeyBinding_CapturesDefaultBinding` and `TestGetKeyBinding_CapturesDefaultBindingWithArgs` | FAIL — ATTRIBUTED to `ga-afqddr` | Clause 3(a), mechanism: host tmux returns an empty default binding. The diff does not touch `internal/runtime/tmux` or host tmux configuration. |
| `TestHumaBinary_CityCreateAsync`, `TestCleanInstallTutorialPath`, and `TestGCLiveContract_BeadsAndEvents` | FAIL — WAIVED under `ga-6bnc42`, occurrence logged on `ga-lpfjhc` | Clause 3(a), mechanism: each exposes the exact `gastownhall/beads#4566` dirty-table schema-migration failure during fixture city or rig-store initialization. The awake-set diff cannot alter Dolt schema migration/store bootstrap. The standing authorization requires the raw failures to remain visible as FAIL-WAIVED, as they do here. |
| `TestE2E_SuspendResume_City` | FAIL — ATTRIBUTED to `ga-yc0e3a` | Clause 3(a), structural mechanism: the fixture renders `citysus` as an always named session with `Pool=nil`. The new behavior requires an open work bead and `PoolManaged=true`; configured named sessions return through the unchanged named-identity branch before the new pool fallback. The failure matches the tracker's missing-report contention signature. |

For all nine failures: clause 1 passes (not diff-owned), clause 2 passes (the cited tracker predates this run and was opened during evaluation), and clause 4 passes (no failing test file/package overlaps the candidate files). The candidate adds tests to an existing `cmd/gc` target but adds no suite target or resource-census load. Tracker occurrence notes and complete attribution were recorded on the beads before any push.

## Additional required lanes

- `policy_lane: make test-ci-policy — PASS`
- `go vet ./...` — PASS
- `lint-affected` with a fresh on-disk cache and `LINT_CHANGED_SCOPE=tracked` — PASS, 0 issues
- `fmt-check-changed` — PASS
- `git diff --check origin/main...HEAD` — PASS

## Acceptance audit

- The reconciler projects typed `session.Info.PoolManaged` into the pure awake-set input.
- Ready/open template-assigned work can wake any eligible member of that configured pool.
- In-progress work cannot match by template and remains bound to bead ID, runtime session name, or named-session identity.
- Existing readiness/blocker filtering runs before matching, so not-ready work creates no wake demand.
- Named sessions, manual sessions, numeric-suffix lookalikes, and members of another pool are explicitly covered and rejected.
- The change introduces no role-name logic, wire/API change, dependency, or migration.
