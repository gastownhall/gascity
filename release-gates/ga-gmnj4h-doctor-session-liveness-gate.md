# Release gate: doctor session-liveness reporting while the controller runs

- Deploy bead: `ga-gmnj4h`
- Build bead: `ga-bq9vdi`
- Review bead: `ga-muq5i1`
- Reviewed source: `c9649b9acb5c8d06bd3c8e978f9ec30e17b39964`
- Base checked: `origin/main@fcbd34178b1c86ec14f5b88ebc40dbe805f224ed`
- Deploy branch: `deploy/ga-gmnj4h-gate`
- Deploy mode: remote; push target: `fork`
- Evaluated: 2026-09-02
- Verdict: **PASS with three attributed, non-diff-owned test failures and one attributed local lint condition**

## Gate checklist

The target pre-flight ran before criterion 6. The reviewed source resolves to
the full SHA above and is not carried by an existing pull request. Criterion 6
was checked first and passed against the current fetched base, so the remaining
criteria were evaluated.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review-of-record `ga-muq5i1` is closed with an explicit final PASS on the exact reviewed source. It independently records build, vet, test, security, coverage, and scope-compliance evidence. No review carryover is involved. |
| 2 | Acceptance criteria met | **PASS** | `buildDoctorChecks` now registers the agent-session, zombie-session, and orphan-session checks whenever config loaded, regardless of controller state. Both destructive `Fix` paths re-check `IsControllerRunning` and return a documented refusal while the controller owns reconciliation. The registration, zombie refusal, orphan refusal, and concurrent reconciler tests cover all required branches. Startup-health episode code is untouched. The broader bounded tri-state observation work is explicitly split into follow-up `ga-xiukll`, consistent with this bead's recorded scope decision. |
| 3 | Tests pass | **PASS with attributed failures** | The documented full-suite command completed all 40 jobs: **38 PASS / 2 raw FAIL / 0 SKIP jobs**. The two red jobs contain three top-level test failures, all non-diff-owned and attributed under criterion 3a. A fresh JSON-mode named probe reported **4 PASS / 0 FAIL / 0 SKIP** for every diff-owned test. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3a | Pre-existing failures attributed | **PASS** | `TestBdFlagManifestCurrent` is tracked by `ga-f0uceo`; `TestE2E_SuspendResume_City` by `ga-dqd7gf`; and `TestCleanInstallTutorialPath`'s beads#4566 dirty-schema condition by `ga-esyijp`. Each tracker predates this run and was opened before attribution. Each failing command path is structurally unable to invoke the changed doctor code, and no failing test path overlaps the diff. Current sightings were appended and read back. |
| 3b | Policy and static lanes | **PASS with one attributed local condition** | `make test-ci-policy` PASS; `make vet` PASS; `make fmt-check` PASS; `make build` PASS; `git diff --check origin/main...HEAD` PASS. `make lint` reported only three diagnostics in ignored, untracked dashboard `node_modules/flatted` Go code that is absent from `origin/main`; exact pre-existing tracker `ga-bvixfw` covers the condition. The candidate does not touch dashboard dependencies, and the sighting was appended and read back. |
| 3c | CI-config lane | **PASS — n/a** | `ci_lane_run: n/a (no CI job, matrix, timeout, workflow, or required-check change)`. |
| 4 | No high-severity review findings open | **PASS** | Unresolved HIGH findings: 0. The review records no blocking security, correctness, style, or coverage finding. |
| 5 | Final branch is clean | **PASS** | The exact reviewed source was clean before branch creation. The gate checklist is the only deployer-authored file and will be committed separately on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | After a fresh fetch, `git merge-tree --write-tree origin/main c9649b9acb5c8d06bd3c8e978f9ec30e17b39964` returned tree `e78e3899d43b7969e06804a70ba62171b8a8c4c3` with exit 0 and no conflict messages against `origin/main@fcbd34178b1c86ec14f5b88ebc40dbe805f224ed`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | Two TDD commits and four files implement one doctor/session-liveness theme: restore passive visibility while preserving the controller's exclusive remediation ownership. |

## Full-suite test evidence

`test_cmd_scope: full-suite`

```text
TMPDIR=/var/tmp \
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
TESTCONTAINERS_RYUK_DISABLED=true \
LOCAL_TEST_LOG_DIR=/var/tmp/gc-gate-ga-gmnj4h-20260903/full \
LOCAL_TEST_JOBS=4 \
GO_TEST_TIMEOUT=30m \
make test-local-full-parallel
```

The rootless Podman socket was present and `podman info` reported rootless
operation before the run. Gas City's suite contains no testcontainers-backed
test in this diff. All 40 documented full-runner jobs reached a terminal state;
raw logs are retained at the path above.

- `test_counts: 38 PASS jobs, 2 attributed raw FAIL jobs, 0 SKIP jobs`
- `raw_test_failures: 3 attributed FAIL, none diff-owned`
- `diff_tests_executed: 4 PASS, 0 FAIL, 0 SKIP`
- `skip_justification: n/a for job results; no diff-owned test skipped`
- `waiver_ref: none`

Named diff-owned results from the fresh JSON-mode supplemental probe:

- `TestBuildDoctorChecks_SessionLivenessChecksRegisteredRegardlessOfController_GH5742` — PASS
- `TestZombieSessionsCheck_FixSkipsWhenControllerRunning` — PASS
- `TestZombieSessionsCheck_FixDoesNotRaceControllerReconciliation` — PASS
- `TestOrphanSessionsCheck_FixSkipsWhenControllerRunning` — PASS

## Raw failures and attribution

| Raw result | Test | Tracker / proof |
|---|---|---|
| **FAIL — ATTRIBUTED** | `TestBdFlagManifestCurrent` | Open tracker `ga-f0uceo`, created 2026-08-15. Clause 3(a): the test compares the host-installed `bd` help surface to `internal/bdflags`; neither the installed binary nor that package is changed. `internal/bdflags` cannot import `cmd/gc` or `internal/doctor`, and there is no path overlap. |
| **FAIL — ATTRIBUTED** | `TestE2E_SuspendResume_City` | Open tracker `ga-dqd7gf`, created 2026-09-02 before this run. Clause 3(a): the exact tracked full-suite condition timed out waiting for `citysus.report`; the scenario invokes `suspend`, `session kill`, and `resume`, never `doctor`, and no test path overlaps. |
| **FAIL — ATTRIBUTED** | `TestCleanInstallTutorialPath` | Open root-condition tracker `ga-esyijp`, created 2026-08-29. Clause 3(a): the log has the tracked beads#4566 dirty-schema migration signature during `gc rig add`, followed by a missing `leases` table. The changed doctor paths are not invoked by rig-add/schema bootstrap, and no test path overlaps. |

`failure_attribution`:

- `TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a) mechanism — installed-binary/source-manifest skew; candidate unreachable`
- `TestE2E_SuspendResume_City -> ga-dqd7gf | clause 3(a) mechanism — tracked report timeout in suspend/resume path; doctor code unreachable`
- `TestCleanInstallTutorialPath -> ga-esyijp | clause 3(a) mechanism — tracked dirty-schema bootstrap condition; doctor code unreachable`

`inconclusive-guard: n/a — all three attributions have decisive mechanism
proof.` The diff changes no resource-census baseline and adds no suite target.

## Policy-lane attribution

`policy_lane: make test-ci-policy — PASS; make lint — raw FAIL attributed to ga-bvixfw; make vet — PASS; make fmt-check — PASS`

`make lint` found two `govet` constant-inline diagnostics and one `revive`
package-comment diagnostic in
`internal/api/dashboardspa/web/node_modules/flatted/golang/pkg/flatted/flatted.go`.
That ignored, untracked file is absent from `origin/main`, and the candidate
changes neither dashboard code nor dependencies. Open tracker `ga-bvixfw`
predates this run and names this exact full-lint condition; the current sighting
was appended and verified.

## Pre-flight and ancestry evidence

- The recorded reviewed SHA passed a hex-only guard and resolved to the full
  commit above.
- `gh api repos/gastownhall/gascity/commits/c9649b9acb5c8d06bd3c8e978f9ec30e17b39964/pulls` returned no
  pull request, so no already-merged or closed-PR reconciliation applies.
- `assert_deploy_ancestry_scope origin/main c9649b9acb5c8d06bd3c8e978f9ec30e17b39964 ga-gmnj4h ga-bq9vdi`
  passed. The accepted sibling ID is the confirmed build bead cited by both
  source commits; no unrelated commit or `.claude/**` path rides in the range.
