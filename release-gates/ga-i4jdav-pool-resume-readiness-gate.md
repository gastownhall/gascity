**Verdict:** **PASS**

# Release gate: pool resume requires ready assigned work

- Deploy bead: `ga-i4jdav`
- Build bead: `ga-b630bn.1`
- Review bead: `ga-9ot8xl`
- Reviewed source: `f7859f8c078893bc4b32b94f87aaba49518cd813`
- Base evaluated: `origin/main@a76bff41c79473e646600e8ad42dab53f3dac067`
- Merge base: `c814d5a5c349617e36ec85c345b99a4aaa9fbe75`
- Deploy mode: remote; push target: `fork`
- Evaluated: 2026-09-18

GitHub's commit-to-pull-request lookup returned no pull request carrying the
reviewed source, so there is no already-merged or closed target to reconcile.
`docs/PROJECT_MANIFEST.md` is absent from this source and current main; this
record applies the deploy protocol's current seven release criteria and the
source bead's acceptance contract.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Closed review bead `ga-9ot8xl` records `verdict: PASS`, `tests_green: true`, no uncovered criteria, and the exact reviewed commit above. No review carryover was used. |
| 2 | Acceptance criteria met | **PASS** | The existing store-scoped readiness map is reduced while bead/store slices are aligned, preserved through pool filtering, and threaded into the resume-tier decision. An open assigned bead resumes only when its readiness flag is true; `in_progress` work remains actionable. The implementation adds no fabricated blocked status, no second store-ready query, and no `pool_session_name.go` change. Both negative and positive control tests passed twice in the full suite. |
| 3 | Tests pass | **PASS with attribution** | The documented 40-job full local suite completed 37 jobs green and 3 red, with **51,412 PASS / 3 FAIL / 227 SKIP** top-level results. Every red test stopped during external `bd init` on the predating shared-server schema-migration condition tracked by `ga-esyijp`; none reached candidate behavior. Both diff-owned tests passed twice with zero FAIL/SKIP. Full details are below. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, `go vet ./...`, `go build ./...`, and `LINT_BASE=origin/main make lint-new` exited 0; lint reported `0 issues`. `git diff --check` and `gofmt -l` were also clean. |
| 3c | CI-config lane | **PASS / n/a** | No workflow, CI job, matrix, timeout, or required-check configuration changed. |
| 4 | No high-severity review findings open | **PASS** | Reviewer recorded no style or security findings, no blocker, and no uncovered acceptance criterion; unresolved HIGH count is 0. |
| 5 | Final branch clean | **PASS** | The sanctioned deployer worktree was clean at the detached reviewed SHA before this checklist was written. `.githooks` owns `core.hooksPath`, verified by `make check-hooks`. |
| 6 | Branch diverges cleanly from main | **PASS** | After fetching current main, `git merge-tree --write-tree --messages origin/main f7859f8c078893bc4b32b94f87aaba49518cd813` exited 0 and produced tree `4025c82730f28baeed1f3fc5e2615ca53caecd38`. The candidate is 20 commits behind and 2 ahead of the base; no bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | Two TDD commits and three `cmd/gc` files form one pool desired-state readiness theme. `assert_deploy_ancestry_scope` passed for `ga-i4jdav`, `ga-b630bn.1`, and `ga-9ot8xl`; no unrelated ancestry or `.claude/**` path is present. |

## Acceptance evidence

- `build_desired_state.go` derives bead-ID readiness from the existing
  `map[storeScopedBeadKey]bool` while `assignedWorkBeads` and
  `assignedWorkStoreRefs` are still index-aligned, then carries the aligned
  flags through pool-demand filtering.
- `pool_desired_state.go` gates only the open-work case on the supplied
  readiness flag and keeps the established unconditional `in_progress` path.
- The not-ready regression covers both resume subtiers by asserting that no
  desired-state entry is emitted; its ready positive control still emits one
  resume request for the assigned session.
- The diff is confined to `cmd/gc/build_desired_state.go`,
  `cmd/gc/pool_desired_state.go`, and
  `cmd/gc/pool_desired_state_test.go`.

## Criterion 3 evidence

```text
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/ga-i4jdav-full.kNN1mx/shards isolated-test-run.sh -- bash -c 'make test-local-full-parallel'
test_cmd_scope: full-suite
job_counts: PASS=37 FAIL=3 TOTAL=40
test_counts: PASS=51412 FAIL=3 SKIP=227
waiver_ref: none
ci_lane_run: n/a (no CI-config change)
wrapper_tripwire: none
shard_log_manifest_sha256: c87eb56b8f5488100ec11710232a238e43e441b1ced43748d7fda2275c9b00d5
```

The rootless Podman socket was active before the run. The cached Dolt image
tags matched `deps.env`'s `DOLT_VERSION=2.1.7` pin, and Ryuk was disabled per
the host isolation contract. The 227 skips are suite-declared platform,
live-provider, optional integration, helper-sentinel, or fast-lane skips from
unchanged tests. No diff-owned test skipped.

`diff_tests_executed` (each PASS twice; zero FAIL/SKIP):

- `TestComputePoolDesiredStates_OpenAssignedWorkNotReadyDoesNotResume`
- `TestComputePoolDesiredStates_OpenAssignedWorkReadyResumes`

### Failure attribution

All three failures reproduce the exact external `bd init` refusal tracked by
open gate tracker `ga-esyijp`, created 2026-08-29, before this run. The suite
was not rerun. Each sighting was appended to the tracker and read back.

| Failure | Job | Criterion 3a evidence |
|---|---|---|
| `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` | `cmd-gc-process-6-of-6` | Not diff-owned. Cross-PR proof: the tracker records this exact test and refusal on unrelated candidates including `ga-wlwk8p` and `ga-em8g4o`. Same-package clause 4 is satisfied by that external proof; the candidate has no census bump, new test target, or new test file. The test stopped before metadata was written. |
| `TestAdoptPRFormulaRetriesTransientReviewerStep` | `integration-review-formulas-retries-1-of-2` | Not diff-owned, no path overlap, and repeated exact cross-PR sightings predate this run. External store initialization failed before formula execution. |
| `TestHumaBinary_CityCreateAsync` | `integration-rest-full-1-of-8` | Not diff-owned, no path overlap, and repeated exact cross-PR sightings predate this run. Asynchronous city creation reached external store initialization, which failed before API scenario behavior. |

Raw logs are retained under `/var/tmp/ga-i4jdav-full.kNN1mx`; the initial
setup-only invocation under `/var/tmp/ga-i4jdav-full.SbsEH1` executed no tests
because its shard-log directory had not been created and is not gate evidence.

## Disposition

All seven release criteria pass. Cut `deploy/ga-i4jdav-gate` from the exact
reviewed source, commit this checklist, push only that isolated branch, open a
pull request that states it replaces obsolete PR #3860, publish exact-head
deploy clearance, and route the merge request to mayor for MPR. The deployer
does not merge.
