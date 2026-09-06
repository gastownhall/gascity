# Release gate: pool-template ready-work discovery

- Deploy bead: `ga-g716up`
- Build bead: `ga-8vz95k.1`
- Review bead: `ga-zol62r`
- Reviewed commit: `9c007d4aed3f612552a6a225153210b3682baca2`
- Base: `origin/main@7e72e01ab00974f43ebc7695767e2290deda3662`
- Deploy mode: remote (`origin` is GitHub and accepts a dry-run push)

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | `ga-zol62r` records `verdict: pass` on the exact reviewed commit, with no blocker, major, minor, or security findings. |
| 2 | Acceptance criteria met | **PASS** | The generated default queries now fall back from concrete session identities to the configured pool-template assignee only for ephemeral pool members. The four diff-owned tests cover both single-store `bd ready` and federated `gc ready`, concrete-identity precedence, exact-assignee behavior, custom-query preservation, ordinary/named/manual sessions, numeric-suffix lookalikes, and membership in another pool. |
| 3 | Tests pass | **PASS** | Full CI-equivalent local union: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/ga-g716up-full-gate make test-local-full-parallel`. Scope: `full-suite`. Result: 35/40 jobs green; 46,936 top-level PASS, 5 FAIL, 189 SKIP. All five failures are attributed below; no diff-owned test failed or skipped. All six `cmd-gc-process` jobs passed, satisfying the required process/Tutorial01 coverage for an `internal/**` change. The 189 skips are pre-existing OS-specific, live-provider, helper-process, or explicitly opt-in persistence tests; none is diff-owned. |
| 3a | Pre-existing failures attributed | **PASS** | `TestBdFlagManifestCurrent` -> `ga-f0uceo`, clause 3(a): host `bd` flag-manifest skew in `internal/bdflags`, outside and unreachable from this work-query change. `TestGetKeyBinding_CapturesDefaultBinding` and `...WithArgs` -> `ga-afqddr`, clause 3(a): host tmux 3.7b filtered-default-keytable behavior, outside the changed package and mechanism. `TestE2E_SuspendResume_City` -> `ga-yc0e3a`, clause 3(b): the identical missing-`citysus.report` contention signature is documented across unrelated diffs and exact-base runs. `TestPersonalWorkFormulaCompileAndRun` -> `ga-lpfjhc`, exact gastownhall/beads#4566 dirty-table schema-migration signature during fixture `gc init`; raw result retained as **FAIL-WAIVED** under mayor standing authorization `ga-6bnc42`, and this occurrence was logged on `ga-lpfjhc`. Clauses 1, 2, and 4 pass for every attribution: none of the failing tests is diff-owned; each tracker predates this run and was opened during evaluation; no failing test file overlaps the diff. `inconclusive-guard`: not used. `added_test_load=no`: no census baseline or suite-target change. |
| 3b | Policy and static lanes | **PASS** | `make test-ci-policy` PASS; clean-cache `make lint-affected LINT_CHANGED_REF=e736d74d0a84a129de47f9008c4560d3146c77ce LINT_CHANGED_SCOPE=tracked` PASS with 0 issues; `go vet ./...` PASS; `make fmt-check-changed` and `git diff --check origin/main...HEAD` PASS. An initial lint invocation using the shared analyzer cache was discarded because it returned stale absolute paths for already-deleted `/var/tmp` worktrees; the fresh on-disk cache rerun is the auditable result. |
| 4 | No high-severity review findings open | **PASS** | Reviewer security/spec review reports no blocker, major, minor, or HIGH findings. |
| 5 | Final branch is clean | **PASS** | `git status --short` was empty before the gate artifact was written; the candidate contains only its two reviewed commits and this gate file will be committed on the isolated deploy branch. Hooks path is `/home/jaword/projects/gascity/.githooks`. |
| 6 | Branch diverges cleanly from main | **PASS** | After `git fetch origin main`, `git merge-tree --write-tree origin/main 9c007d4aed3f612552a6a225153210b3682baca2` exited 0 and produced tree `9290dac489163c6e7f97951c8d3a2965dd8b088f`. Pre-flight `gh api repos/gastownhall/gascity/commits/9c007d4aed3f612552a6a225153210b3682baca2/pulls` returned `[]`; the target has not already merged or closed. |
| 7 | Single feature theme | **PASS** | Both commits touch one subsystem and one behavior: `internal/config` generation and tests for pool-template assigned ready-work discovery. |

## Diff-owned test resolution

- `TestPoolMemberDefaultQueriesDiscoverTemplateAssignedReadyWork`: PASS in `unit-core` and `integration-packages-core-3-of-4`, including all four topology/query-shape subtests.
- `TestPoolMemberConcreteReadyIdentityPrecedesTemplate`: PASS in both full-suite jobs, including all four subtests.
- `TestTemplateAssignedReadyFallbackRequiresConfiguredEphemeralPoolMembership`: PASS in both full-suite jobs, including all 20 boundary subtests.
- `TestPoolMemberCustomWorkQueryRemainsVerbatim`: PASS in both full-suite jobs.
- `waiver_ref`: `ga-6bnc42` applies only to the unrelated Beads #4566 fixture-bootstrap failure; no diff-owned test is waived.

## Gate decision

**PASS.** The reviewed feature is conflict-free against current main, its acceptance boundary is exercised in both supported ready-query topologies, every diff-owned test executed successfully, and the required process, policy, lint, formatting, and vet lanes are clean. The full-suite raw failures remain visible above and carry valid pre-existing attribution or the standing merge-authority waiver.
