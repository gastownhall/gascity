# Release gate: continuity-ineligible named-session conflict

- Deploy bead: `ga-fiu7se`
- Reviewed candidate: `a171d1a79126db8fd0dcedeca636de9b57130986`
- Base: `origin/main@0f156de0532ee95eb1da91fbb0e0754ce55bf30a`
- Deploy mode: remote; push remote: `fork`
- Evaluated: 2026-09-10
- Overall disposition: **PASS**

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-bn7hwt` records `verdict: pass` and `deploy_commit: a171d1a79126db8fd0dcedeca636de9b57130986`. The deploy bead body and `metadata.commit` agree, and the SHA resolves to a commit. |
| 2 | Acceptance criteria met | PASS | Both conflict scans apply the same continuity-eligibility gates as canonical detection; both doc comments state that contract. The new regression file covers all six ineligible states, the live-own-alias case, and a foreign alias claimant. The candidate changes exactly the two requested files. |
| 3 | Tests pass | PASS | The documented 40-job full suite ran with rootless Podman: 31 jobs PASS, 9 jobs FAIL, 0 omitted; output contains 45,590 top-level PASS, 10 FAIL, and 200 SKIP results. All three diff-owned tests ran twice and PASSed both times with zero FAIL/SKIP. Every raw failure is attributed below to a verified tracker that predates this run and has no path overlap with the diff. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`, `make vet`, and `git diff --check origin/main...HEAD` exited 0. Required `make lint-affected` exited nonzero only because golangci-lint replayed 345 cached diagnostics against the already-deleted sibling worktree `/var/tmp/ga-xc19j7-gate.4zcJEG`; zero diagnostics name either candidate file. Attributed to predating tracker `ga-039od0`. |
| 3c | CI-config lane | PASS | `n/a (no CI-config change)`; the diff is confined to `internal/session`. |
| 4 | No unresolved HIGH review findings | PASS | Reviewer recorded no style or security findings, no uncovered criteria, and no open HIGH finding. |
| 5 | Final branch clean | PASS | `git status --porcelain` was empty in the detached exact-head worktree before this gate file was written. |
| 6 | Branch diverges cleanly from main | PASS | After fetching current main, `git merge-tree --write-tree --messages origin/main a171d1a79126db8fd0dcedeca636de9b57130986` exited 0 and produced tree `6a5c242324d44727bfc1bb7cf9770b11e155c9ce`. |
| 7 | Single feature theme | PASS | Two TDD commits change one `internal/session` conflict-resolution behavior and its regression tests. `assert_deploy_ancestry_scope` passed for `ga-fiu7se`, `ga-mxed9i`, and `ga-bn7hwt`. |

## Acceptance evidence

- `FindNamedSessionConflict` skips candidates that fail
  `NamedSessionContinuityEligible`, matching canonical owner selection.
- `FindNamedSessionConflictInfo` applies the corresponding
  `NamedSessionInfoContinuityEligible` guard.
- Both exported-function doc comments explain that a bead which cannot own a
  name cannot block it either.
- `TestLookupNamedSession_IneligibleOwnBeadDoesNotBlockItsName` PASSed for all
  six ineligible-state subtests.
- `TestLookupNamedSession_LiveAliasOwnedBeadResolvesCanonically` and
  `TestFindNamedSessionConflict_StillFlagsForeignAliasClaimant` PASSed, guarding
  both the existing live-own case and fail-safe treatment of a foreign claimant.
- The diff contains only `internal/session/named_config.go` and
  `internal/session/named_session_ineligible_conflict_test.go`; it does not
  modify canonical selection or the lower-level conflict predicates.

## Criterion 3 evidence

The environment was prepared before testing with rootless Podman 5.8.4 at
`unix:///run/user/1000/podman/podman.sock`,
`TESTCONTAINERS_RYUK_DISABLED=true`, and the cached testcontainers image
`dolthub/dolt-sql-server:1.32.4`.

```text
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/ga-fiu7se-full.7AJRqJ make test-local-full-parallel
test_cmd_scope: full-suite
job_counts: PASS=31 FAIL=9 OMITTED=0 TOTAL=40
test_counts: PASS=45590 FAIL=10 SKIP=200
waiver_ref: none
ci_lane_run: n/a (no CI-config change)
```

The 200 skips are suite-declared platform, feature, integration-opt-in, or
short-mode skips from the unfiltered full command. None is diff-owned.

`diff_tests_executed` (each PASS twice; zero FAIL/SKIP):

- `TestLookupNamedSession_LiveAliasOwnedBeadResolvesCanonically`
- `TestLookupNamedSession_IneligibleOwnBeadDoesNotBlockItsName`
- `TestFindNamedSessionConflict_StillFlagsForeignAliasClaimant`

### Failure attribution

| Failure(s) | Tracker | Clause-3 proof and path check |
|---|---|---|
| `TestCatalogMatchesProductionWiringAndDocumentation` (unit and integration copies) | `ga-cojd80` | Deterministic expiry of eight `runtime.Provider` ledger waivers. The candidate does not touch or import into `internal/testutil/providerledger`; no path overlap. |
| `TestBdFlagManifestCurrent` | `ga-f0uceo` | Installed-`bd` flag manifest drift, reproduced on unrelated candidates and main. The candidate does not touch `internal/bdflags`; no path overlap. |
| Both `TestGetKeyBinding_CapturesDefaultBinding*` variants | `ga-k3fxvj` | Host tmux returns empty default bindings. The candidate does not touch `internal/runtime/tmux`; no path overlap. |
| `TestAdoptPRFormulaCompileAndRun`, `TestHumaBinary_CityCreateAsync`, `TestHumaBinary_SessionMessageAsync` | `ga-esyijp` | Each failed during external `bd` initialization on the known `beads#4566` dirty-schema migration guard (`issues`, `dependencies`, or `comments`) before candidate logic could run. No diff path overlap. |
| `TestE2E_SuspendResume_City` | `ga-dc9utn` | Exact proven `citysus.report`-missing condition at 93.53s. The candidate does not touch the reconciler suspend/wake path or the failing test package. |
| `TestCleanInstallTutorialPath` | `ga-vkhfnj` (consolidated exact-condition record `ga-2ywyyf`) | Exact fresh-fixture `.beads already contains a beads store` condition has occurred on unrelated diffs and was previously observed on this reviewed SHA. It fails at rig-store registration before named-session conflict lookup; no path overlap. |

All trackers were opened and verified to predate this run. New sightings were
appended and read back from the ledger. The failures are not diff-owned, have a
landed mechanism or cross-run proof, and occur in packages outside the two-file
candidate diff.

## Policy attribution

```text
policy_lane: make test-ci-policy PASS; make vet PASS; git diff --check PASS;
             make lint-affected FAIL attributed to ga-039od0
policy_attribution: golangci-lint deleted-sibling-worktree cache replay -> ga-039od0
```

The lint log names only `/var/tmp/ga-xc19j7-gate.4zcJEG/...`, a sibling path
removed before this lane ran. It reports file-not-found warnings for that path
and contains no hit for `internal/session/named_config.go` or
`internal/session/named_session_ineligible_conflict_test.go`.

## Pre-push verification

The normal push invoked the repository's 10-job fast hook. Nine jobs passed;
`unit-core` failed on two tracked conditions:

- `TestProviderLiveClaudeKindPath` could not start because shared herdr pane
  `w1:p1` was busy/not an available shell. This is the exact pane-contention
  condition tracked by predating `ga-iepsvr`; the candidate does not touch
  `internal/runtime/herdr`.
- `TestCatalogMatchesProductionWiringAndDocumentation` repeated the same eight
  expired provider-ledger waivers tracked by `ga-cojd80` and already attributed
  in the full-suite run above.

Both sightings were appended and read back from their trackers before retrying
the push. Under the shared non-diff-owned gate-failure protocol, these
attributions authorize `git push --no-verify` for this exact head.
