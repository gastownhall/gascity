# Release gate: bounded liveness observation

- Re-gate bead: `ga-966mj8`
- Deploy bead: `ga-08v565`
- Build bead: `ga-ix9kx5`
- Review bead: `ga-tzjc3i`
- Originally reviewed source: `02a16e523ab08bb9dafa41b26e8749e66587e80f`
- Review-carryover source: `c209321aa8929c1c81535675030ce06d0765981c`
- Bounded self-rebase parent: `57d3229915296aceb6c3decfd8f3b7d728084c90`
- Final candidate: `772b71ee75095c28fa32ea49214d98e37590c2a6`
- Candidate merge base: `80eebf4f18e5a693fb1628dcf49cb28993305b97`
- Current base checked: `origin/main@435ed03be7d2d7818e31edfe6dc248488b952fcb`
- Decision: **PASS**

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This exact-head
re-gate applies the seven release criteria from the active deployer protocol
and the full-suite command documented in `TESTING.md`.

PR #6091 was already open when `ga-966mj8` was assigned. Its old clearance
was tied to superseded head `8d4b49c6bff7347ec0bc2b921c9589450899ff2e`.
The deployer resolved the one-hunk import conflict by bounded self-rebase to
`57d3229915296aceb6c3decfd8f3b7d728084c90`; the maintainer review then applied
the shared-observer fixup, producing final candidate
`772b71ee75095c28fa32ea49214d98e37590c2a6`. This record supersedes the prior
gate evidence and no clearance is carried across those head changes.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-tzjc3i` is closed with verdict `pass` on `02a16e523ab08bb9dafa41b26e8749e66587e80f`; the original deploy gate independently verified patch-ID carryover to `c209321aa8929c1c81535675030ce06d0765981c`. Maintainer `quad341` then reviewed PR #6091 as `fix-merge`, identified the exact duplicate-interface/shared-helper delta, and applied that reviewed delta as final commit `772b71ee75095c28fa32ea49214d98e37590c2a6`; the mayor's re-gate note identifies that exact final head. |
| 2 | Acceptance criteria met | PASS | `ObservationStatus`, `ObservationComplete`, `ObservationIncomplete`, `BoundedLivenessObserver`, and `ObserveLivenessBounded` remain additive in `internal/runtime/liveness.go`. The final fix aliases `BoundedLivenessObserver` to the already-landed `LivenessObserverWithError`, delegates through `ObserveLivenessWithError` so nil/blank guards and normalization are shared, preserves legacy fallback and error semantics, and rejects an already-canceled context without spawning an observation. Seven named tests cover the bounded API, including the two maintainer-added cases. |
| 3 | Tests pass | PASS | A fresh exact-head `make test-local-full-parallel` completed all 40 jobs: 35 PASS, 5 attributed FAIL, 0 SKIP. Every raw failure stopped in `bd init` on the predating shared-schema condition tracked by `ga-esyijp`, before candidate behavior ran; exact attribution is below. `unit-core` reports `internal/runtime` PASS. A fresh named run passes all seven diff-owned bounded-liveness tests, and a broad `cmd/gc` `(Liveness|Observation)` regression run passes. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`, `go build ./...`, `go vet ./...`, tracked `make fmt-check-changed`, merge-base-scoped `make lint-affected`, and `git diff --check origin/main...HEAD` all PASS; lint reports `0 issues`. |
| 3c | CI-config lane | PASS | `ci_lane_run: n/a (no CI job, matrix, timeout, or required-check configuration changed)`. |
| 4 | No high-severity review findings open | PASS | The formal reviewer recorded no blocker, major, or HIGH finding. The maintainer review found only the duplicate error-bearing interface/shared-helper divergence and resolved it in the final candidate. |
| 5 | Final branch is clean | PASS | The isolated deploy branch was clean before this record was refreshed. This record is the only evidence-only change after the tested candidate and is committed with the gate result. |
| 6 | Branch diverges cleanly from main | PASS | After the test run, `git fetch origin main` resolved `origin/main` to `435ed03be7d2d7818e31edfe6dc248488b952fcb`. `git merge-tree --write-tree origin/main 772b71ee75095c28fa32ea49214d98e37590c2a6` exited 0 and produced tree `cca62bbb1b93a809f1a32d332abde92f169a13a2`; divergence is 3 commits behind and 4 ahead. GitHub reports PR #6091 `MERGEABLE`. |
| 7 | Single feature theme | PASS | The candidate range contains the bounded-liveness implementation/tests, the prior gate record, and the maintainer's shared-observer fixup. All code changes are confined to `internal/runtime/liveness.go` and its test and implement one feature theme. |

## Test evidence

`test_cmd_scope: full-suite`

Environment prepared before the run:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
TESTCONTAINERS_RYUK_DISABLED=true
EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true'
LOCAL_TEST_JOBS=4
LOCAL_TEST_LOG_DIR=/var/tmp/ga-966mj8-final-20260913T0647Z/jobs
make test-local-full-parallel
```

The rootless Podman socket was active before the command.

- `test_counts: 35 PASS jobs, 5 attributed FAIL jobs, 0 SKIP jobs`
- `top_level_failures: 5`
- `diff_tests_executed: TestObserveLivenessBoundedFallsBackToObserveLivenessWithoutRicherInterface PASS; TestObserveLivenessBoundedForwardsExistingLivenessObserver PASS; TestObserveLivenessBoundedMapsRuntimeUnavailableToIncomplete PASS; TestObserveLivenessBoundedCompleteWithNonRuntimeErrorStaysComplete PASS; TestObserveLivenessBoundedTimesOutToIncompleteWithZeroLiveness PASS; TestObserveLivenessBoundedNormalizesObserverLiveness PASS; TestObserveLivenessBoundedCancelledParentContextIsIncomplete PASS`
- `skip_justification: no full-suite job reported SKIP`
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI-config change in this diff)`
- Full-suite log: `/var/tmp/ga-966mj8-final-20260913T0647Z/full-suite.log`
- Per-job logs: `/var/tmp/ga-966mj8-final-20260913T0647Z/jobs`
- Focused runtime log: `/var/tmp/ga-966mj8-final-20260913T0647Z/focused-liveness.log`
- Focused `cmd/gc` log: `/var/tmp/ga-966mj8-final-20260913T0647Z/focused-cmd-gc-liveness.log`

### Failure attribution

| Raw result | Tracker | Attribution |
|---|---|---|
| `TestAdoptPRFormulaCompileAndRun` | `ga-esyijp` | Clause 3(a), mechanism: `gc init` stopped when shared database `hq` refused pending schema migration v65 to v66, before formula behavior ran. |
| `TestPersonalWorkFormulaCompileAndRun` | `ga-esyijp` | Clause 3(a), mechanism: `gc init` stopped when shared database `hq` refused pending schema migrations v34 to v66, before formula behavior ran. |
| `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` | `ga-esyijp` | Clause 3(a), mechanism: `gc init` stopped when shared database `hq` refused pending schema migrations v40 to v66, before formula behavior ran. |
| `TestHumaBinary_CityCreateAsync` | `ga-esyijp` | Clause 3(a), mechanism: asynchronous city initialization emitted `city_init_failed` after shared database `hq` refused pending schema migrations v56 to v66. |
| `TestGCLiveContract_BeadsAndEvents` | `ga-esyijp` | Clause 3(a), mechanism: rig creation stopped when its shared database refused pending schema migrations v6 to v66. |

`failure_attribution: five bd-init schema refusals -> ga-esyijp | clause 3(a) mechanism — shared-server pending-schema refusal before candidate execution; candidate unreachable`

`ga-esyijp` predates this run, covers the root condition, was opened before
attribution, and received peek-verified comments for every occurrence. The
candidate touches only `internal/runtime/liveness.go`,
`internal/runtime/liveness_test.go`, and this release-gate record: none of the
five failing tests or bead initialization paths overlap the diff.

## Required lanes and remote corroboration

- `policy_lane: make test-ci-policy — PASS`
- `go build ./...` — PASS
- `go vet ./...` — PASS
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=80eebf4f18e5a693fb1628dcf49cb28993305b97 make fmt-check-changed` — PASS
- `LINT_CHANGED_REF=80eebf4f18e5a693fb1628dcf49cb28993305b97 make lint-affected` — PASS, `0 issues`
- `git diff --check origin/main...HEAD` — PASS
- Git hook path: `.githooks`

GitHub Actions run `34741373474` attempt 1 crashed inside the Go compiler
runtime while compiling Dolt's `sqle/dtablefunctions`, with no semantic source
diagnostic. That root condition is tracked as `ga-7omh22`. Attempt 2 on exact
candidate `772b71ee75095c28fa32ea49214d98e37590c2a6` completed successfully,
including `Integration / packages-core-4-of-4`, `Preflight / static checks`,
and `CI / required`:

`https://github.com/gastownhall/gascity/actions/runs/34741373474`
