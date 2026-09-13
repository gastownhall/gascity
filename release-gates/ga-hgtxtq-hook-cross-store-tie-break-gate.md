# Release Gate: cross-store hook tie rotation

Deploy bead: `ga-hgtxtq`

Review bead: `ga-rdex4v`

Build bead: `ga-kbbg9a`

Reviewed source: `b8626685d1778b24eee014ec7be9c1e6ba770d47`

Base: `origin/main@08461bb390c8720cde505ae769638c8ccbcb2e53`

Gate date: 2026-08-21

**Verdict: PASS.** The reviewed behavior and every diff-owned test pass. The
documented candidate and exact-base 40-job unions each report 34 PASS / 6 FAIL
/ 0 SKIP, with different failure membership. Mayor ruling `gm-wisp-cwndyl`
clears criterion 3 as PASS-with-attribution: this equal-count, disjoint-set
result is two samples from the suite's documented timing-noise distribution,
not evidence that the candidate added failures. The ruling explicitly directs
the deployer not to spend another repetition sweep. No waiver was self-granted.

`docs/PROJECT_MANIFEST.md` is absent from this repository. This record uses the
seven release criteria in the deployer contract and the documented commands in
`TESTING.md` and
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Gate Results

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Closed review bead `ga-rdex4v` records `verdict: pass` for the exact reviewed source. |
| 2 | Acceptance criteria met | PASS | Exact ties rotate across distinct bead IDs; co-resident duplicate IDs remain deduplicated; strict rank improvements, primary-store in-progress short-circuiting, and class-escalation behavior remain intact. All focused and repetition checks pass. |
| 3 | Tests pass | PASS | Candidate and exact-base full unions each report 34 PASS jobs, 6 FAIL jobs, 0 job-level SKIP, with disjoint failure membership. Every diff-owned and acceptance test passes, including 60/60 class-escalation controls. Mayor ruling `gm-wisp-cwndyl` clears the three candidate-only wall-clock failures by non-attribution and directs no further resampling. Raw failures remain recorded below. |
| 3a | Pre-existing failures attributed | PASS | `TestBdFlagManifestCurrent` and both tmux key-binding tests reproduce on the exact base with active trackers and no path overlap. The remaining timing-only failures carry documented flake lineage (`ga-qyh9cb`, `ga-gajll3`/`ga-thuouz`, and tutorial-path beads `ga-hrdd3h`/`ga-io7xwr`) and are covered by the mayor's explicit non-attribution ruling `gm-wisp-cwndyl`; this is a merge-authority decision, not a deployer-authored waiver. |
| 3b | Policy/lint lane | PASS | Required policy lane `make test-ci-policy` passes. `go build ./...`, `go vet ./...`, changed formatting, and `git diff --check` pass. An auxiliary affected-lint run selected the full repository because the reviewed source predates a base-only release-gate file and failed on 182 unrelated repository/cache diagnostics; no diagnostic names either changed `cmd/gc` file. |
| 4 | No high-severity review findings open | PASS | The reviewer recorded PASS and no open HIGH finding. Unresolved HIGH count: 0. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty at the exact reviewed source before this checklist was written. |
| 6 | Branch diverges cleanly from main | PASS | After the guarded push, `git merge-tree --write-tree origin/main b8626685d1778b24eee014ec7be9c1e6ba770d47` exited 0 against `origin/main@08461bb390c8720cde505ae769638c8ccbcb2e53`, producing tree `bf32180957642c3e9e826cce0eccfb7b654f3394`. The newer main changes are confined to credential-provider tests, named-session alias handling, and their gate records; they do not touch `cmd/gc/hook_cross_store.go` or its test. `assert_deploy_ancestry_scope` passed for `ga-hgtxtq`, `ga-kbbg9a`, and `ga-rdex4v`. No self-rebase was needed. |
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
pre_push_lane: LOCAL_TEST_JOBS=2 CMD_GC_PROCESS_TOTAL=6 ./scripts/test-local-parallel fast -- 10 PASS jobs, 0 FAIL, 0 SKIP
```

### Candidate failures and disposition

| Failing test | Candidate job | Base result | Disposition |
|---|---|---|---|
| `TestBdFlagManifestCurrent` | `integration-packages-core-1-of-4` | FAIL with the same signature in `integration-packages-core-4-of-4` | Attributed to `ga-f0uceo`; not diff-owned and no path overlap. |
| `TestGetKeyBinding_CapturesDefaultBinding` | `integration-packages-runtime-tmux-2-of-3` | FAIL with the same empty-default signature | Attributed to `ga-afqddr`; not diff-owned and no path overlap. |
| `TestGetKeyBinding_CapturesDefaultBindingWithArgs` | `integration-packages-runtime-tmux-3-of-3` | FAIL with the same empty-default signature | Attributed to `ga-afqddr`; not diff-owned and no path overlap. |
| `TestTutorial01/controller` | `cmd-gc-process-3-of-6` | PASS job | FAIL — ATTRIBUTION CLEARED by `gm-wisp-cwndyl`: wall-clock controller timeout with tutorial-path flake lineage `ga-hrdd3h` / `ga-io7xwr`; weaker tracker link is recorded as such. |
| `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` | `cmd-gc-process-4-of-6` | PASS job | FAIL — ATTRIBUTION CLEARED by `gm-wisp-cwndyl`: same tracked wall-clock failure previously adjudicated on another gate; tracker `ga-qyh9cb`. |
| `TestDoltConfigWiringExternalHost` | `integration-rest-full-2-of-8` | PASS job | FAIL — ATTRIBUTION CLEARED by `gm-wisp-cwndyl`: hardcoded 15-second `bd init` timeout lineage `ga-gajll3` / `ga-thuouz`; no path overlap. |

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

Mayor ruling `gm-wisp-cwndyl` clears criterion 3 by non-attribution and directs
the deployer to push and proceed without another repetition sweep. The ruling
is based on identical 34/6/0 candidate and base counts with disjoint failure
sets, documented timing-flake history for the candidate-only failures, and a
fully green diff-owned surface. Normal isolated-branch deployment now applies.
