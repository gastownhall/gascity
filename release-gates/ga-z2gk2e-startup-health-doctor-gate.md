# Release gate: startup-health episodes in `gc doctor`

- Bead: `ga-z2gk2e`
- Review bead: `ga-49yu7c`
- Reviewed commit: `32d76330045cf35c6e934f2f4b43c705708c734e`
- Base: `origin/main@171f24319ab7a3c004b561ff734d0feb924fa129`
- Deploy mode: `remote`; push remote resolved to `fork`
- Result: **PASS** — criterion 3 uses independently granted waiver `mayor-2026-09-05-ga-z2gk2e-c3` for the tracked external Dolt schema failure.

## Pre-flight

- `git rev-parse --verify --quiet 32d76330045cf35c6e934f2f4b43c705708c734e^{commit}` resolved the recorded value to the same full commit SHA.
- GitHub reports no pull request carrying the reviewed commit, so neither already-merged nor closed-without-merging reconciliation applies.
- The source is internally authored. No contributor PR or contributor interaction is involved.
- The commit range is one coupled feature: the typed startup-health episode listing feeds the read-only `gc doctor` diagnostic that consumes it.
- `assert_deploy_ancestry_scope` passed for the deploy bead and the confirmed feature-chain bead IDs `ga-pbakj9`, `ga-1gq8wz`, and `ga-o04bfr.1.4`; no unrelated or `.claude/**` content is present.

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-49yu7c` records `Review verdict: PASS` for the exact reviewed commit, with no review carryover. |
| 2 | Acceptance criteria met | **PASS** | Source inspection and the 20 diff-owned tests verify registration independent of controller state; no-episode and below-threshold OK results; threshold, quarantine, startup-death, stalled-reset, malformed-data, duplicate-session, store-error, deterministic-rendering, and secret-exclusion behavior; typed listing including closed beads; and `CanFix=false` / `WarmupEligible=false`. |
| 3 | Tests pass | **PASS** | The documented full command `make test-local-full-parallel` ran with the rootless Podman socket configured. Current run: 37/40 jobs PASS, 3 FAIL, 0 SKIP; logs: `/var/tmp/gc-gate-ga-z2gk2e.UUtZGD`. The runner enumerated 20,226 named shard tests: at least 20,223 PASS, 3 top-level FAIL, 0 explicit SKIP, in addition to passing unenumerated core package tests. All 20 diff-owned tests reported PASS through their owning full-suite shards/packages. `TestBdFlagManifestCurrent` is attributed to pre-existing tracker `ga-f0uceo` under clause 3(a): the diff has no `internal/bdflags` overlap and that package reaches neither changed production package. `TestE2E_SuspendResume_City` is attributed to tracker `ga-dqd7gf` under clause 3(b): the identical full-suite timeout is recorded on unrelated candidates and this diff has no `test/integration` path overlap. `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` hit tracker `ga-esyijp`'s beads#4566 dirty-schema condition. Because its package overlaps the candidate, deployer attribution stopped at clause 4; mayor independently verified the failure originates outside this read-and-format diff and granted waiver `mayor-2026-09-05-ga-z2gk2e-c3`. An earlier full run on this exact SHA (`/var/tmp/ga-z2gk2e-full.rhLdHm`) independently reproduced the same dirty-schema condition and suspend/resume timeout. `test_cmd_scope: full-suite`; `waiver_ref: mayor-2026-09-05-ga-z2gk2e-c3`; `ci_lane_run: n/a (no CI-config change)`; `policy_lane: make test-ci-policy — PASS`; `go vet ./...` — PASS. |
| 4 | No high-severity review findings open | **PASS** | Review bead `ga-49yu7c` records no unresolved HIGH finding. |
| 5 | Final branch is clean | **PASS** | `git status --short` was empty before this gate artifact was written. |
| 6 | Branch diverges cleanly from main | **PASS** | `origin/main...32d76330045cf35c6e934f2f4b43c705708c734e` is 1 behind / 7 ahead, and `git merge-tree --write-tree 32d76330045cf35c6e934f2f4b43c705708c734e origin/main` returned 0 with tree `19b4c7fde5441b2626dd750da1bd974535622cad`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The commit set is confined to startup-health episode persistence/query and its `gc doctor` observation surface. |

## Diff-owned test evidence

All tests below were selected by the full-suite command and passed; none skipped or failed:

- `TestBuildDoctorChecks_StartupHealthEpisodesRegisteredRegardlessOfController_GH5742`
- `TestStartupHealthEpisodesCheck_NoEpisodesIsOK`
- `TestStartupHealthEpisodesCheck_BelowThresholdIsOK`
- `TestStartupHealthEpisodesCheck_AtThresholdIsError`
- `TestStartupHealthEpisodesCheck_StalledResetKindIsDistinguished`
- `TestStartupHealthEpisodesCheck_MixedKindsBothDistinguished`
- `TestStartupHealthEpisodesCheck_ActiveQuarantineBelowThresholdIsError`
- `TestStartupHealthEpisodesCheck_RendersRequiredFields`
- `TestStartupHealthEpisodesCheck_RecoveredEpisodeIsOK`
- `TestStartupHealthEpisodesCheck_MalformedEpisodeIsError`
- `TestStartupHealthEpisodesCheck_DuplicateSessionNamesBothReported`
- `TestStartupHealthEpisodesCheck_StoreErrorIsGraceful`
- `TestStartupHealthEpisodesCheck_DeterministicOrdering`
- `TestStartupHealthEpisodesCheck_CanFixIsFalse`
- `TestStartupHealthEpisodesCheck_WarmupEligibleIsFalse`
- `TestStartupHealthEpisodesCheck_NeverLeaksLastDetail`
- `TestStoreListStartupHealthEpisodesEmptyStoreReturnsEmpty`
- `TestStoreListStartupHealthEpisodesReturnsAllSaved`
- `TestStoreListStartupHealthEpisodesIncludesClosedBeads`
- `TestStoreListStartupHealthEpisodesPropagatesStoreError`

`diff_tests_executed: 20 PASS, 0 FAIL, 0 SKIP`

## Failure attribution and waiver

- `failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a): unreachable changed packages; clause 4: no path overlap`
- `failure_attribution: TestE2E_SuspendResume_City -> ga-dqd7gf | clause 3(b): identical cross-run timeout on unrelated candidates; clause 4: no path overlap`
- `failure_attribution: TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix -> ga-esyijp | clause 4 stopped deployer self-attribution because the candidate also changes cmd/gc`
- `waiver_ref: mayor-2026-09-05-ga-z2gk2e-c3`
- Independent waiver evidence: the candidate's only final-round change is pure episode filtering and timestamp/string rendering with no beads schema, initialization, or migration write path; the failing signature is byte-identical to the tracked gastownhall/beads#4566 condition; and tracker `ga-esyijp` contains seven or more sightings predating this candidate.

## Disposition

All seven criteria pass. Create the isolated `deploy/ga-z2gk2e-gate` branch from the reviewed SHA, commit this checklist, push it to the fork, open the pull request, publish deploy clearance on the exact PR head, and route the merge-request to mayor. The deployer does not merge.
