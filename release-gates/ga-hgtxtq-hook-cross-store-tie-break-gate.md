# Release Gate: cross-store hook tie rotation

Deploy bead: `ga-hgtxtq`

Review bead: `ga-rdex4v`

Build bead: `ga-kbbg9a`

Reviewed source: `b8626685d1778b24eee014ec7be9c1e6ba770d47`

Base: `origin/main@187e53828754894096fc295cea4baca909fe9a96`

Gate date: 2026-08-21

**Verdict: PENDING MAYOR ADJUDICATION.** The reviewed behavior and every
diff-owned test pass, but the documented 40-job candidate sweep contains three
candidate-only wall-clock failures that one exact-base control did not
reproduce. Two failures are in the changed `cmd/gc` package, so the no-path-
overlap attribution clause is not satisfied; the third is a tracked Dolt
fixture timeout outside the diff, but this control did not prove it
pre-existing. Under mayor correction `gm-wisp-giaqmv`, a single clean base
observation is not enough to classify a wall-clock timeout as caused by the
candidate. No waiver has been self-granted. Nothing is pushed and no pull
request exists while criterion 3 is unresolved.

`docs/PROJECT_MANIFEST.md` is absent from this repository. This record uses the
seven release criteria in the deployer contract and the documented commands in
`TESTING.md` and
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Gate Results

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Closed review bead `ga-rdex4v` records `verdict: pass` for the exact reviewed source. |
| 2 | Acceptance criteria met | PASS | Exact ties rotate across distinct bead IDs; co-resident duplicate IDs remain deduplicated; strict rank improvements, primary-store in-progress short-circuiting, and class-escalation behavior remain intact. All focused and repetition checks pass. |
| 3 | Tests pass | **PENDING** | The candidate 40-job union reports 34 PASS jobs, 6 FAIL jobs, 0 job-level SKIP. Three deterministic failures reproduce on the exact base and are attributable below. `TestTutorial01`, `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix`, and `TestDoltConfigWiringExternalHost` fail only on the candidate in this A/B sample. They are not diff-owned, but their wall-clock nature and the two `cmd/gc` path overlaps prevent either a branch-regression verdict or a deployer-authored waiver from this single comparison. |
| 3a | Pre-existing failures attributed | **PENDING** | `TestBdFlagManifestCurrent` and both tmux key-binding tests reproduce on the exact base with active trackers and no path overlap. The other three candidate failures do not satisfy every attribution clause. Mayor adjudication is required; no builder bounce is justified from one timing observation. |
| 3b | Policy/lint lane | PASS | Required policy lane `make test-ci-policy` passes. `go build ./...`, `go vet ./...`, changed formatting, and `git diff --check` pass. An auxiliary affected-lint run selected the full repository because the reviewed source predates a base-only release-gate file and failed on 182 unrelated repository/cache diagnostics; no diagnostic names either changed `cmd/gc` file. |
| 4 | No high-severity review findings open | PASS | The reviewer recorded PASS and no open HIGH finding. Unresolved HIGH count: 0. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty at the exact reviewed source before this checklist was written. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree --messages origin/main b8626685...` exited 0 against current `origin/main`, producing tree `bd3b73056d40beaab7c6d7374fe040f14895a599`. `assert_deploy_ancestry_scope` passed for `ga-hgtxtq`, `ga-kbbg9a`, and `ga-rdex4v`. No self-rebase was needed. |
| 7 | Single feature theme | PASS | Three commits modify only cross-store hook selection and its tests in `cmd/gc`; all changes serve one starvation fix. |

## Acceptance Evidence

- `bestStoreWithWork` accumulates every distinct candidate tied at the best
  rank and selects among them with a clock-derived index, so repeated fresh
  `gc hook` processes do not permanently prefer `stores[0]`.
- Equal-ranked rows sharing one bead ID are deduplicated before rotation. A
  copied/migrated bead therefore does not masquerade as two pieces of work or
  invert the rig-first-city-last fan-out order.
- An empty/unidentifiable ID is not deduplicated, preserving the existing
  fallback behavior for unrankable fixtures.
- A primary-store in-progress candidate still returns immediately; a better
  tier or priority still beats every worse candidate before tie-breaking.

## Test Evidence

The rootless Podman socket was active before the full run:
`DOCKER_HOST=unix:///run/user/1000/podman/podman.sock` and
`TESTCONTAINERS_RYUK_DISABLED=true`. The cached pinned image
`docker.io/dolthub/dolt-sql-server:2.1.7` was present.

### Diff-owned and acceptance tests

```text
test_cmd: go test -json -count=1 ./cmd/gc -run '^(TestBestStoreWithWorkDoesNotInvertTheBug|TestBestStoreWithWorkRotatesExactTies|TestBestStoreWithWorkRepeatedTiesVisitEveryStoreOverTime|TestBestStoreWithWorkDoesNotRotateOnACoResidentDuplicateID|TestHookTieBreakIndex|TestBestHookCandidateRank|TestBestStoreWithWorkShortCircuitsOwnInProgress|TestBestStoreWithWorkPrefersHigherPriorityInALaterStore|TestBestStoreWithWorkRanksTierAheadOfPriority)$'
test_counts: all named tests and subtests PASS, 0 FAIL, 0 SKIP
focused_log: /var/tmp/ga-hgtxtq-focused.json

repeat_cmd: go test -json -count=30 ./cmd/gc -run '^(TestClassEscalationStillReachesABindingOnlyBead|TestClassEscalationWaitsForEveryWorkLeg)$'
repeat_counts: 60 PASS, 0 FAIL, 0 SKIP
repeat_log: /var/tmp/ga-hgtxtq-class-escalation-30.json

diff_tests_executed:
  TestBestStoreWithWorkDoesNotInvertTheBug PASS
  TestBestStoreWithWorkRotatesExactTies PASS
  TestBestStoreWithWorkRepeatedTiesVisitEveryStoreOverTime PASS
  TestBestStoreWithWorkDoesNotRotateOnACoResidentDuplicateID PASS
  TestHookTieBreakIndex PASS
  TestBestHookCandidateRank and all subtests PASS
waiver_ref: none for diff-owned tests
```

### Candidate and exact-base full unions

```text
candidate_ref: b8626685d1778b24eee014ec7be9c1e6ba770d47
candidate_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m make test-local-full-parallel
candidate_counts: 34 PASS jobs, 6 FAIL jobs, 0 job-level SKIP (40 total)
candidate_logs: /var/tmp/gc-local-tests.l3r6cX
candidate_transcript: /var/tmp/ga-hgtxtq-full.out

base_ref: 187e53828754894096fc295cea4baca909fe9a96
base_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m make test-local-full-parallel
base_counts: 34 PASS jobs, 6 FAIL jobs, 0 job-level SKIP (40 total)
base_logs: /var/tmp/gc-local-tests.5SMCnj
base_transcript: /var/tmp/ga-hgtxtq-base.AlrVxt/base-full.out

policy_lane: make test-ci-policy -- PASS
build_lane: go build ./... -- PASS
static_lane: go vet ./... -- PASS
format_lane: LINT_CHANGED_REF=origin/main make fmt-check-changed -- PASS
diff_check: git diff --check origin/main...HEAD -- PASS
```

### Candidate failures and disposition

| Failing test | Candidate job | Base result | Disposition |
|---|---|---|---|
| `TestBdFlagManifestCurrent` | `integration-packages-core-1-of-4` | FAIL with the same signature in `integration-packages-core-4-of-4` | Attributed to `ga-f0uceo`; not diff-owned and no path overlap. |
| `TestGetKeyBinding_CapturesDefaultBinding` | `integration-packages-runtime-tmux-2-of-3` | FAIL with the same empty-default signature | Attributed to `ga-afqddr`; not diff-owned and no path overlap. |
| `TestGetKeyBinding_CapturesDefaultBindingWithArgs` | `integration-packages-runtime-tmux-3-of-3` | FAIL with the same empty-default signature | Attributed to `ga-afqddr`; not diff-owned and no path overlap. |
| `TestTutorial01/controller` | `cmd-gc-process-3-of-6` | PASS job | **UNRESOLVED**: controller socket wall-clock timeout; not diff-owned, but package overlaps the diff and one clean base run cannot establish causation. |
| `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` | `cmd-gc-process-4-of-6` | PASS job | **UNRESOLVED**: dirty-schema / init timing signature tracked by `ga-qyh9cb`; not diff-owned, but package overlaps the diff and did not reproduce in this base run. |
| `TestDoltConfigWiringExternalHost` | `integration-rest-full-2-of-8` | PASS job | **UNRESOLVED**: 36-second `bd init` timeout with fix lineage `ga-gajll3` / `ga-thuouz`; no path overlap, but this control did not prove the failure pre-existing. |

The exact base has its own six failing jobs, all outside this diff:
`TestFileRecorderWatchAfterLatestStartsAtEOF`,
`TestProviderLiveClaudeKindPath`, both tmux tests,
`TestE2E_SuspendResume_City`, `TestPersonalWorkFormulaCompileAndRun`, and
`TestBdFlagManifestCurrent`. Those failures demonstrate substantial host/load
noise but do not themselves waive a different candidate failure.

## Pre-flight and Disposition

GitHub's base-repository commit-to-PR lookup returned no PR for the reviewed
source, and the bead contains no prior PR URL. There is no merged, closed, or
superseded target to reconcile.

Deploy mode is remote. `origin` is the base/fetch remote and its dry-run push
is unavailable, so a future PASS would push the isolated branch to `fork`.
GitHub authentication is active.

Criterion 3 remains unresolved under correction `gm-wisp-giaqmv`. This gate
record stays local and unpushed; no PR, deploy-clearance status, or merge
request is permitted until mayor adjudication supplies a waiver or directs
the corrected repeat protocol.
