# Release gate: pool-seat freeability and explicit-wake retention

- Deploy bead: `ga-o2ixi9`
- Review bead: `ga-o7vpvk` — PASS
- Build bead: `ga-y1h63j`
- Reviewed source: `a87ba48e7455050c55928f3731d16679d54fb38c`
- Base: `origin/main@875028329c3288fae726b47824dc29bedebb2bd3`
- Deploy mode: `remote`; push remote: `fork`
- Pre-flight: the reviewed source has no associated pull request, so it has not already merged
- Overall verdict: **PASS**

## Criteria

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-o7vpvk` is closed with verdict `pass` and pins the exact deploy source `a87ba48e7455050c55928f3731d16679d54fb38c`. No review carryover is involved. |
| 2 | Acceptance criteria met | **PASS** | The prerequisite fix `516809154a` is an ancestor of the reviewed source. `session.Info.SleptAt` and the `slept_at` codec entry are present. Both freeability representations admit only an empty reason paired with a non-empty `slept_at`, while the quarantine control remains denied. `explicit-wake` joins the idle-sleep exemption beside the existing demand exemptions, with comments referencing issue #5739. All five named regression tests are present and passed. The candidate contains no out-of-scope lifecycle, wake-refusal, legacy-data, or adjacent PR changes. The deploy PR created from this gate will reference #5739. |
| 3 | Tests pass | **PASS with one attributed raw failure** | The documented full local CI union completed **39/40 jobs green**, with **50,901 PASS / 1 raw FAIL / 225 SKIP** top-level test executions. The only failure is attributed below under criterion 3a. All five diff-owned tests passed in both the process and integration package tiers: **10 PASS / 0 FAIL / 0 SKIP**. `test_cmd_scope: full-suite`; `waiver_ref: none`; `ci_lane_run: n/a (no CI-config change)`. The separate policy lane and vet both pass. |
| 4 | No high-severity review findings open | **PASS** | The review records no style, security, or specification blockers and no unresolved HIGH finding. |
| 5 | Final branch is clean | **PASS** | Before writing this checklist, `git status --porcelain=v1` was empty at the reviewed source; `git diff --check origin/main...a87ba48e7455050c55928f3731d16679d54fb38c` and `gofmt -l` on all three changed files were also empty. The checklist is committed on the isolated deploy branch in the next step. |
| 6 | Branch diverges cleanly from main | **PASS** | After fetching `origin/main`, `git merge-tree --write-tree origin/main a87ba48e7455050c55928f3731d16679d54fb38c` exited 0 and produced tree `92f23461836cfb9cd53d311a1e6a8586376045be`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The two feature commits touch only `cmd/gc` wake-demand/freeability behavior and their regression test. The ancestry scope guard passes for `ga-o2ixi9` plus its confirmed build bead `ga-y1h63j`; no unrelated commit or `.claude/**` path is present. |

## Acceptance checks

- `516809154a` is an ancestor of the reviewed source and current `origin/main`.
- `internal/session/manager.go` exposes `Info.SleptAt`; `internal/session/info_codec.go` maps `slept_at`.
- `isPoolSessionSlotFreeable` and `isPoolSessionSlotFreeableInfo` contain parallel empty-reason plus stamped-`slept_at` admission arms.
- `TestQuarantinedAsleepStaysUnfreeable` protects deliberate quarantine sleeps from becoming freeable.
- `ComputeAwakeSet` exempts durable `explicit-wake` demand from idle-sleep suppression and cites #5739.
- Diff-owned tests, each PASS twice in the full union:
  - `TestExplicitWakeSurvivesIdleSleep`
  - `TestExplicitWakeRefusalIsAttributable`
  - `TestWokenFromSuspendedIsInvisibleToStrandedLane`
  - `TestExplicitWakeIdleSleepScopeWithClaimedWork`
  - `TestQuarantinedAsleepStaysUnfreeable`
- Candidate paths are exactly:
  - `cmd/gc/compute_awake_set.go`
  - `cmd/gc/session_state_helpers.go`
  - `cmd/gc/wake_reprojection_repro_test.go`

## Criterion 3 evidence

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/ga-o2ixi9-test.LLanxd/jobs make test-local-full-parallel`, invoked through `packs/actual/all/scripts/isolated-test-run.sh`.
- `test_cmd_scope: full-suite`
- `test_counts: 50,901 PASS / 1 raw FAIL / 225 SKIP`; job result `39 PASS / 1 raw FAIL / 0 omitted`.
- `diff_tests_executed: 10 PASS / 0 FAIL / 0 SKIP` — each of the five named tests passed in both full-suite tiers that selected it.
- `skip_justification`: the 225 skips are existing suite-controlled platform, live-provider, helper-process, opt-in persistence, or fast-tier exclusions. The full union also ran the process-backed and integration tiers that own those boundaries. None is diff-owned.
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI-config change in this diff)`
- `policy_lane: make test-ci-policy — PASS`
- `make vet — PASS`
- `make check-hooks — PASS` (`core.hooksPath` is `.githooks`)
- Rootless Podman 5.8.4 was live before the run. The exact pinned images were refreshed successfully: testcontainers' `dolthub/dolt-sql-server:1.32.4` and the repository's `dolthub/dolt:2.1.7`. Ryuk remained disabled as required on this host.
- Full log: `/var/tmp/ga-o2ixi9-test.LLanxd/full.log`
- Per-job logs: `/var/tmp/ga-o2ixi9-test.LLanxd/jobs`
- Policy log: `/var/tmp/ga-o2ixi9-test.LLanxd/policy.log`
- Vet log: `/var/tmp/ga-o2ixi9-test.LLanxd/vet.log`

### Criterion 3a failure attribution

`failure_attribution: TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash -> ga-vkhfnj | clause 3(a): mechanism — PASS`

1. **Not diff-owned:** the failure is in `test/integration/review_formula_test.go`; the candidate changes only the three `cmd/gc` paths listed above.
2. **Tracked before the run:** `ga-vkhfnj` is an open `gate-tracker` created 2026-08-29 for whole-suite/shared-Dolt contention. It already records this exact test and the beads #5920 shared-server migration-refusal signature. This run's sighting was appended and read back from the tracker.
3. **Not caused by the candidate:** the test failed inside `setupReviewFormulaCity` because `gc init` invoked `bd init`, which refused 30 pending shared-HQ schema migrations (`v36 -> v66`). Fixture setup stopped before formula execution. The candidate's production symbols are called only from desired-state/session-reconciler paths after successful city initialization, so neither changed path executed before this failure.
4. **No path overlap:** the failing test package is `test/integration`; the candidate paths are under `cmd/gc`. The candidate changes no beads initialization, schema migration, Dolt provider, resource-census, Makefile, or CI target.

The full-suite raw failure is therefore attributable under all four non-diff-owned failure clauses. It does not weaken the independent evidence for the five diff-owned tests, which all executed and passed twice.
