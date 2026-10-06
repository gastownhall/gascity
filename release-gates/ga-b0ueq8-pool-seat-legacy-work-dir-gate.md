# Release gate: legacy work-dir pool seats

- Deploy bead: `ga-b0ueq8`
- Build bead: `ga-uw9mcd`
- Review bead: `ga-9gscz2`
- Reviewed commit: `83185183ea523427edc9efa078b96edc2981beb4`
- Provenance branch: `builder/ga-uw9mcd`
- Base: `origin/main@c7a92b25ebb100ccfd0f3a31cf2e865a5d7bfb1c`
- Deploy mode: remote
- Deploy branch: `deploy/ga-b0ueq8-gate`
- Evaluated: 2026-08-30
- Verdict: **PASS PENDING** -- criterion 3c (`Mac / make test`) is open

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-9gscz2` records PASS for exact commit `83185183ea523427edc9efa078b96edc2981beb4`; the provenance branch resolves to that SHA. |
| 2 | Acceptance criteria met | **PASS** | A `gc.work_dir` stamp with none of the nine ownership keys now returns a nil spec and nil error, preserving unmanaged pool-seat spawning. Partial ownership evidence still returns the existing missing-key error, including the eight-of-nine boundary case. Both work_dir spellings are covered. The four exact behavior tests passed. |
| 3 | Tests pass (local canonical run) | **PASS WITH ATTRIBUTED FAILURES** | The canonical 40-job run completed: 37 jobs passed and 3 jobs failed on four non-diff-owned tests. All four failures satisfy the project's attribution convention below. The exact changed behavior tests recorded 4 PASS, 0 FAIL, 0 SKIP. |
| 3a | Pre-existing failures may be attributed | **PASS** | Every raw failure has a predating consolidated tracker plus exact-signature and causal disproof or exact-base evidence. No path-name-only or inconclusive attribution is used. |
| 3b | Policy/lint lane | **PASS** | CI policy, module policy, native dependency surface, event-export isolation, core boundary, native DoltLite beads, docs sync, build, vet, formatting, and fresh-cache full lint passed; lint reported 0 issues. `waiver_ref: none`; `ci_lane_run: n/a`. |
| 3c | `Mac / make test` CI lane | **FAIL -- OPEN** | The lane is red on the PR head (run `33339960602`, job `99336852642`), and `Mac regression summary` is red as a consequence. 17 tests failed, none in a package this diff touches. The merge-base comparison this criterion needs has **not** been performed -- see "Mac lane" below. This criterion is not signed off by the local 40-job run. |
| 4 | No high-severity review findings open | **PASS** | Review records no findings. The change is an internal read-path classification with fixed-format logging and no auth, network, serialization, or secret surface. |
| 5 | Final branch is clean | **PASS** | Exact reviewed SHA was evaluated in a clean detached worktree; hooks resolve to `/home/jaword/projects/gascity/.githooks`; `git diff --check` passed. |
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first and refreshed after the full suite. Candidate is 2 commits ahead and 5 behind current main. `git merge-tree --write-tree origin/main 83185183ea523427edc9efa078b96edc2981beb4` exited 0 with tree `9944d7b8fcea8c7d26f67fcece14844cd81feb97`. The mandatory ancestry guard passed for `ga-b0ueq8`, `ga-uw9mcd`, and `ga-9gscz2`; no PR already carries the reviewed SHA. |
| 7 | Single feature theme | **PASS** | Three files: the pool desired-state classifier, its TDD coverage, and this gate record. One theme -- classifying a work_dir stamp that carries no ownership evidence. All commits cite build bead `ga-uw9mcd`. |

## Criterion 3 evidence

```text
test_cmd_scope: full-suite
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=2 LOCAL_TEST_LOG_DIR=/var/tmp/gc-deploy-ga-b0ueq8-eval-logs make test-local-full-parallel
test_counts: 37/40 jobs PASS, 3/40 jobs raw FAIL; 4 top-level failing tests
job_logs: /var/tmp/gc-deploy-ga-b0ueq8-eval-logs
skip_justification: none claimed
waiver_ref: none
ci_lane_run: n/a (no CI configuration change)
```

The runner reports job outcomes rather than an exhaustive test-event tally, so
no total PASS/SKIP count is inferred. All 40 expected job logs exist.

Exact changed behavior execution:

```text
go test ./cmd/gc/... -run 'TestWorktreeSpecForBead' -count=1 -v

TestWorktreeSpecForBeadRequiresCompletePublishedEvidence PASS
TestWorktreeSpecForBeadTreatsWorkDirOnlyAsLegacy PASS
TestWorktreeSpecForBeadTreatsWorkDirOnlyAsLegacy/canonical PASS
TestWorktreeSpecForBeadTreatsWorkDirOnlyAsLegacy/legacy PASS
TestWorktreeSpecForBeadRejectsSingleOwnershipKey PASS
TestWorktreeSpecForBeadPrefersCanonicalStoreRef PASS
```

Both work_dir spellings are covered, and `RejectsSingleOwnershipKey` pins the
eight-missing side of the boundary the unmanaged branch creates: nine missing
ownership keys is "never published", eight is partial evidence and still fails
closed.

### Raw failure attribution

| Raw failing test | Predating tracker | Attribution evidence |
|---|---|---|
| `TestSessionEventsLive` | `ga-vkhfnj` | Exact `events_live_test.go:77: getAgent evt-a: ok=false err=nil` contention signature. Candidate changes only `cmd/gc` pool worktree metadata classification and its test, with no herdr path or call overlap. |
| `TestBdFlagManifestCurrent` | `ga-he29xi` | Exact installed-`bd` flag-manifest drift signature. Candidate does not touch `internal/bdflags` and cannot change the installed `bd` binary queried by the test. |
| `TestE2E_SuspendResume_City` | `ga-vkhfnj` (consolidated from `ga-yc0e3a`) | Exact 93-second missing-`citysus.report` signature with prior exact-base reproduction. The fixture renders `citysus` as an always named session with no managed pool; it cannot reach the changed trigger-bead `worktreeSpecForBead` branch. |
| `TestCleanInstallTutorialPath` | `ga-esyijp` | Exact beads#4566 dirty-schema migration and missing `leases` table signature during `gc rig add`, before pool desired-state evaluation. Candidate does not touch store migration/bootstrap. |

Occurrences and log paths were appended to the consolidated trackers during
this gate. None of the four failures exercises a symbol or mechanism changed by
the candidate.

## Mac lane

The `Mac / make test` CI lane is **FAIL** on the PR head and is recorded here as
its own line item. The local 40-job run above is a different suite on a
different platform and does not stand in for it.

```text
lane: Mac / make test
run: https://github.com/gastownhall/gascity/actions/runs/33339960602/job/99336852642
head_result: FAIL (17 tests)
merge_base_result: NOT RUN
```

Failing tests and their owning packages:

| Package | Failing tests |
|---|---|
| `internal/worker/adapters/zcode` | `TestCancelledFirstTurnStaysVisibleAndIsAdopted`, `TestDrainKeepsPartialTrailingLine`, `TestExportMirrorAccumulatesTurns`, `TestExportMirrorPublishesTheUserTurnBeforeTheReply`, `TestFailedTurnClosesTheMirrorEntry`, `TestFailedTurnKeepsLoopingAndPreservesSid`, `TestFiveConsecutiveFailuresBailWithoutMarker`, `TestIdleSeparatedPromptsStaySeparate`, `TestInterruptedTurnClosesTheMirrorEntry`, `TestInterruptKillsASigintIgnoringTurn`, `TestInterruptMidTurnContinuesTheLoop`, `TestInterruptWhileIdleDoesNotExit` |
| `internal/runtime/herdr` | `TestSocketPathFallsBackToHomeConfigWhenXDGUnset`, `TestSocketPathHonorsXDGConfigHomeOverHome` |
| `internal/pathdurability` | `TestClassifyOnRealFilesystem` |
| `internal/doctor` | `TestCustomTypesCheck_TableDrift` |
| `internal/worktree` | `TestEnsureManagedWorktreePersistsAndVerifiesProvenance` |

**Reachability, not attribution.** The candidate changes exactly one production
symbol: `worktreeSpecForBead`, unexported in `package main` under `cmd/gc`. No
test above is in `cmd/gc`, so none can call it. That bounds the blast radius; it
is not the evidence this criterion requires.

**What is still owed.** `mac-regression.yml` is `workflow_call`-only and is
dispatched by label on pull requests, so it never runs on `main` and there is no
merge-base run to diff against. Before this gate can be signed off, the lane
must be re-run against both the PR head and the merge base, and the diff of
failures recorded here. Until then criterion 3c stands **OPEN** and the overall
verdict below is not final.

## Policy and static evidence

Passed on exact reviewed commit `83185183ea523427edc9efa078b96edc2981beb4`:

```text
make test-ci-policy check-gomod-replace check-native-dependency-surface \
  check-eventexport-isolation check-core-boundary \
  test-native-doltlite-beads check-docs
go build ./...
make vet
make fmt-check
GOLANGCI_LINT_CACHE=/var/tmp/golangci-ga-b0ueq8 make lint   # 0 issues
git diff --check origin/main...HEAD
```

No API, OpenAPI, dashboard, generated dashboard type, or CI workflow file is
changed, so the dashboard and CI-lane-specific gates do not apply.

## Decision

Gate **PASS PENDING**. Every criterion except 3c is satisfied. Criterion 3c is
open: the `Mac / make test` lane is red on the PR head and has no merge-base
comparison, which is the one piece of evidence this gate still owes.

Publish an isolated `deploy/ga-b0ueq8-gate` branch containing the reviewed TDD
pair plus this evidence record, open a PR against `main`, and route the merge
request to mayor/mpr. Merge is blocked until 3c is closed by a head-vs-merge-base
Mac lane comparison recorded above. The deployer does not merge or restart the
supervisor; post-merge redeployment remains an operator-present action.
