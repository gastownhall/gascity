# Release gate: bounded liveness observation

- Deploy bead: `ga-08v565`
- Build bead: `ga-ix9kx5`
- Review bead: `ga-tzjc3i`
- Originally reviewed source: `02a16e523ab08bb9dafa41b26e8749e66587e80f`
- Gated source: `c209321aa8929c1c81535675030ce06d0765981c`
- Review-carryover patch ID: `f16865760343d5344aa2c351fe11ce4ebce79332`
- Candidate merge base: `2bace2ef89c11e260ca396f95441f805a01e7546`
- Current base checked: `origin/main@92dd4b61381d8d810936e7474d091c0c316b2e7c`
- Decision: **PASS**

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate applies
the seven release criteria from the active deployer protocol and the full-suite
command documented in `TESTING.md`.

The already-merged preflight found no PR carrying the gated source before the
deploy branch was pushed. The normal isolated-branch path therefore applies.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-tzjc3i` is closed with `verdict: pass` on `02a16e523ab08bb9dafa41b26e8749e66587e80f`. The builder recorded review carryover to `c209321aa8929c1c81535675030ce06d0765981c`; independent diffs against each commit's merge base both produce patch ID `f16865760343d5344aa2c351fe11ce4ebce79332`. The deploy bead records `review_carryover_verified`. |
| 2 | Acceptance criteria met | PASS | `runtime.ObservationStatus`, `ObservationComplete`, `ObservationIncomplete`, `BoundedLivenessObserver`, and `ObserveLivenessBounded` are additive in `internal/runtime/liveness.go`. The implementation uses `context.WithTimeout`, prefers the richer opt-in observer, preserves the existing fallback, maps wrapped `runtime.ErrRuntimeUnavailable` and deadline expiry to incomplete observations, preserves non-runtime errors as complete observations, and does not depend on `internal/runtime/worker.go`. Five named tests cover these obligations. |
| 3 | Tests pass | PASS | The documented full-scope `make test-local-full-parallel` run completed 37 PASS jobs / 3 attributed FAIL jobs / 0 SKIP jobs. Each failure is non-diff-owned, has a predating tracker, has a mechanism proof, and has no path overlap. `unit-core` reports `internal/runtime` PASS; a fresh named supplemental run maps all five diff-owned tests to PASS. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`, `go build ./...`, and `go vet ./...` PASS. `make fmt-check` and `make lint` report only tracked, non-diff-owned diagnostics that reproduce identically on current `origin/main` in the same worktree; attribution is recorded below. |
| 3c | CI-config lane | PASS | `ci_lane_run: n/a (no CI job, matrix, timeout, or required-check configuration changed)`. |
| 4 | No high-severity review findings open | PASS | The reviewer recorded no blocker, major, or HIGH finding. Its only observation was the disclosed goroutine lifetime inherited from the mirrored bounded-observation precedent. |
| 5 | Final branch is clean | PASS | The isolated branch was clean before the gate record; the gate record is committed as the only release-evidence addition, after which `git status --porcelain` is empty. |
| 6 | Branch diverges cleanly from main | PASS | After `origin/main` advanced during the test run, `git merge-tree --write-tree origin/main c209321aa8929c1c81535675030ce06d0765981c` was rerun against `92dd4b61381d8d810936e7474d091c0c316b2e7c`; it exited 0 and produced tree `42d748103729eec0215dd6247d94bfb1d53abaaf`. The candidate is one commit behind and two ahead. No self-rebase was needed or attempted. |
| 7 | Single feature theme | PASS | Two commits and two files in `internal/runtime` implement one theme: bounded, tri-state liveness observation and its tests. The ancestry-scope guard passes for deploy bead `ga-08v565` and confirmed build bead `ga-ix9kx5`. |

## Test evidence

`test_cmd_scope: full-suite`

Environment prepared before the run:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
TESTCONTAINERS_RYUK_DISABLED=true
EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true'
make test-local-full-parallel
```

The rootless Podman socket was listening, and the testcontainers Dolt module's
pinned image `dolthub/dolt-sql-server:1.32.4` was present and refreshed before
the run.

- `test_counts: 37 PASS jobs, 3 attributed FAIL jobs, 0 SKIP jobs`
- `top_level_failures: 3`
- `diff_tests_executed: TestObserveLivenessBoundedFallsBackToObserveLivenessWithoutRicherInterface PASS; TestObserveLivenessBoundedForwardsExistingLivenessObserver PASS; TestObserveLivenessBoundedMapsRuntimeUnavailableToIncomplete PASS; TestObserveLivenessBoundedCompleteWithNonRuntimeErrorStaysComplete PASS; TestObserveLivenessBoundedTimesOutToIncompleteWithZeroLiveness PASS`
- `skip_justification: no full-suite job reported SKIP`
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI-config change in this diff)`
- Full-suite logs: `/var/tmp/gc-local-tests.gruhkx`
- Gate logs: `/var/tmp/ga-08v565-gate.4LTpMi`

### Failure attribution

| Raw result | Tracker | Attribution |
|---|---|---|
| FAIL: `TestBdFlagManifestCurrent` | `ga-f0uceo` | Clause 3(a), mechanism: the installed `bd` flag surface and `internal/bdflags` manifest cannot be changed by the new runtime liveness symbols. The candidate has no `internal/bdflags` path overlap. |
| FAIL: `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` | `ga-esyijp` | Clause 3(a), mechanism: fixture initialization stopped on the tracked beads#4566 dirty `dependencies` schema-migration condition before formula behavior ran. The new runtime API has no production callers and cannot affect store bootstrap. The candidate has no `test/integration` path overlap. |
| FAIL: `TestE2E_SuspendResume_City` | `ga-dqd7gf` | Clause 3(a), mechanism: the test timed out after 94.98 seconds waiting for `citysus.report` under the full 40-job load. The new runtime API has no production callers and cannot affect this report path. The candidate has no `test/integration` path overlap. |

`failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a) mechanism — installed-bd manifest drift; candidate unreachable`

`failure_attribution: TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash -> ga-esyijp | clause 3(a) mechanism — dirty-schema migration during fixture initialization; candidate unreachable`

`failure_attribution: TestE2E_SuspendResume_City -> ga-dqd7gf | clause 3(a) mechanism — full-load report timeout; candidate's uncalled API is unreachable`

Each tracker predates this run, was opened and checked, and received a
peek-verified comment recording this occurrence.

## Required lanes

- `policy_lane: make test-ci-policy — PASS`
- `go build ./...` — PASS
- `go vet ./...` — PASS
- `make fmt-check` — raw FAIL; current `origin/main` reproduces the same four unchanged-file diffs.
- `make lint` — raw FAIL; current `origin/main` reproduces all 11 diagnostics identically in the same worktree.

`policy_attribution: gofumpt in three cmd/gc files and internal/storebinding/storebinding_test.go -> ga-d3m213 | origin/main reproduces; no candidate path overlap`

`policy_attribution: three ResolvedProvider.Kind SA1019 diagnostics -> ga-t88402 | origin/main reproduces; no candidate path overlap`

`policy_attribution: internal/api workDir SA4006 -> ga-egkp4x | origin/main reproduces; no candidate path overlap`

`policy_attribution: three node_modules/flatted govet/revive diagnostics -> ga-bvixfw | origin/main reproduces in the same worktree; no candidate path overlap`
