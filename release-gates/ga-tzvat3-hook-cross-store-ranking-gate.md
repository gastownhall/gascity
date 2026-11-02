# Release gate: cross-store hook ranking

- Bead: `ga-tzvat3`
- Build bead: `ga-t922vm`
- Review bead: `ga-u7rfgy`
- Reviewed commit: `f0f427aa3f08c92af48ac51b710ffb31e386ae77`
- Base: `origin/main@dca75312bdf571e9be33752d10260ec5c3e30b2b`
- Merge base: `09bae7ad1706aced67a72775d2b1d11549002cd0`
- Deploy mode: `remote`
- Gate result: **PASS**

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-u7rfgy` is closed with `verdict: pass` at the exact reviewed commit. The SHA resolves to a commit in this repository. No review carryover was used. |
| 2 | Acceptance criteria met | **PASS** | The diff implements cross-directory comparison that treats assigned and routed tiers as tie-equivalent while retaining raw tier ranking within one store directory. `TestBestStoreWithWorkTreatsAssignedTierAsRoutedAcrossStores`, `TestBestStoreWithWorkRanksTierAheadOfPriority`, `TestBestStoreWithWorkShortCircuitsOwnInProgress`, and the existing tie-rotation tests all passed twice in the full suite. The repaired fleet checker (`packs/actual/all/scripts/hook-coverage.sh --check`, gc-management `42ecf64bc3da08b57d3dac9c71a9b7d59064c1d0`) reports `pack-author: 2 bead(s) exist, all 2 reachable`; its exit 2 is solely the unrelated `deep-investigator`/`ga-rv0rh9` condition, which exact `origin/main` reproduces and the candidate's direct hook call returns. `failure_attribution: hook-coverage deep-investigator/ga-rv0rh9 -> ga-f6brbc; proof: clause 3(d), exact BASE_REF reproduction; no path overlap or added test load.` |
| 3 | Tests pass | **PASS** | Required full-scope command: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' GOFLAGS=-v LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m LOCAL_TEST_LOG_DIR=/var/tmp/ga-tzvat3-full.5d6Qqa make test-local-full-parallel`. `test_cmd_scope: full-suite`. Raw counts: **46,977 PASS, 8 FAIL, 201 SKIP**. All eight raw failures are attributed below; no waiver was used. All six `cmd-gc-process` jobs passed, satisfying the required `cmd_gc_process` coverage for `cmd/gc/**`. `ci_lane_run: n/a (no CI-config change)`. |
| 3a | Pre-existing failures attributed | **PASS** | See the failure-attribution table below. Every failure is not diff-owned, has a pre-run open tracker for its root condition, has causal proof independent of this diff, and has no changed-path overlap. The candidate adds no test target or declared load. |
| 3b | Policy/lint lane | **PASS** | `policy_lane: make test-ci-policy — PASS` (runner policy, CI suite coverage, `scripts/cipolicy`, PR watchdog, and static-scope policy). Also PASS: `go build ./...`, `go vet ./...`, `make fmt-check-changed LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=09bae7ad1706aced67a72775d2b1d11549002cd0`, `make lint-changed` with the same scope/ref, `make lint-new LINT_BASE=09bae7ad1706aced67a72775d2b1d11549002cd0`, and `git diff --check`. `core.hooksPath=.githooks`. |
| 4 | No high-severity review findings open | **PASS** | Review records `style_findings: none`, `security_findings: none`, no blockers/majors/minors, and `uncovered_criteria: none`; unresolved HIGH count is 0. |
| 5 | Final branch is clean | **PASS** | Before adding this gate artifact, `git status --short` produced no output at the exact reviewed commit. The gate file is the only deployer-authored change and is committed separately on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | Checked first and rechecked after the full suite. `git merge-tree --write-tree origin/main f0f427aa3f08c92af48ac51b710ffb31e386ae77` exited 0 and produced tree `367e2db6a8da6c2bcef1f5333384e925474467b8`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | Two TDD commits touch only `cmd/gc` cross-store hook candidate ranking and its tests: 3 files, 157 insertions, 31 deletions. The same-directory invariant, cross-directory comparison, and fallback/claim tests are one coherent hook-selection subsystem. |

## Full-suite failure attribution

| Failing test | Raw occurrences | Tracker | Proof and path-overlap check |
|---|---:|---|---|
| `TestCatalogMatchesProductionWiringAndDocumentation` | 2 | `ga-cojd80` | The reviewed ancestry carries expired provider-waiver dates. The feature commits do not touch `internal/testutil/providerledger`, its catalog, provider constructors, or waiver dates. Current main passes after a later date refresh; attribution is by the explicit date-expiry mechanism, not a claimed base reproduction. No path overlap. |
| `TestBdFlagManifestCurrent` | 1 | `ga-f0uceo` | The installed `bd` executable exposes flags absent from the inherited source manifest. This diff cannot change the installed executable and does not touch `internal/bdflags`. No path overlap. |
| `TestGetKeyBinding_CapturesDefaultBinding`; `TestGetKeyBinding_CapturesDefaultBindingWithArgs` | 2 | `ga-k3fxvj` | Both return the tracker’s known empty ambient tmux default binding. The diff does not touch `internal/runtime/tmux` or host tmux configuration. No path overlap. |
| `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash`; `TestGraphWorkflowSuccessPath` | 2 | `ga-esyijp` | Both fixtures fail during `gc init` on the tracked beads#4566 dirty-schema migration refusal, before either test scenario executes. The diff does not touch Beads schema/bootstrap behavior and adds no test target. No path overlap. |
| `TestE2E_SuspendResume_City` | 1 | `ga-dc9utn` | Exact proven ~94-second missing `citysus.report` signature from the reconciler suspend/wake bug. The diff does not touch that reconciler path. No path overlap. |

`skip_justification: 201 non-diff-owned tests intentionally skipped behind explicit opt-in, platform, runtime, fixture, or live-infrastructure guards; the rootless Podman socket was configured before the run, so no container-backed coverage was silently lost. None of the 23 diff-owned tests skipped.`

## Diff-owned tests

`diff_tests_executed: 23 tests, each observed twice as PASS; 46 PASS observations, 0 FAIL, 0 SKIP.`

- `TestClaimHookWorkAssignedTierUnresolvableBeadDoesNotStrandLaterStore` — PASS x2
- `TestClaimHookWorkAssignedTierUnresolvableBeadDrainsClaimsErrored` — PASS x2
- `TestClaimHookWorkAssignedTierOperationalErrorStaysTerminal` — PASS x2
- `TestRigScopedHookRig` — PASS x2
- `TestAppendOneRigHookStoreSkipsUnknownInput` — PASS x2
- `TestBestStoreWithWorkReturnsTheOnlyStoreThatHasWork` — PASS x2
- `TestBestStoreWithWorkPrefersHigherPriorityInALaterStore` — PASS x2
- `TestBestStoreWithWorkDoesNotInvertTheBug` — PASS x2
- `TestBestStoreWithWorkRotatesExactTies` — PASS x2
- `TestBestStoreWithWorkRepeatedTiesVisitEveryStoreOverTime` — PASS x2
- `TestHookTieBreakIndex` — PASS x2
- `TestBestStoreWithWorkRanksTierAheadOfPriority` — PASS x2
- `TestBestStoreWithWorkTreatsAssignedTierAsRoutedAcrossStores` — PASS x2
- `TestBestStoreWithWorkShortCircuitsOwnInProgress` — PASS x2
- `TestBestStoreWithWorkDegradesToFirstHitOnUnrankableOutput` — PASS x2
- `TestBestHookCandidateRank` — PASS x2
- `TestBestStoreWithWorkReturnsLastWhenNoneHasWork` — PASS x2
- `TestBestStoreWithWorkSurfacesOwnStoreErrorWhenNoWork` — PASS x2
- `TestBestStoreWithWorkIgnoresRigStoreErrorWhenOwnStoreHasNoWork` — PASS x2
- `TestBestStoreWithWorkSkipsStoreWithOnlyUnreadyRows` — PASS x2
- `TestClaimStoreWithFallbackFallsBackWhenSelectedStoreRerunsEmpty` — PASS x2
- `TestClaimStoreWithFallbackUsesSelectedStoreWhenStillReady` — PASS x2
- `TestBestStoreWithWorkDoesNotRotateOnACoResidentDuplicateID` — PASS x2

`waiver_ref: none`
