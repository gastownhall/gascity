# Release gate: ctime-aware pack content fingerprints

- Deploy bead: `ga-8cce0j`
- Review bead: `ga-kf5zn8`
- Reviewed commit: `84f0af4833b3e59a178b0d4952913fb971f90bbe`
- Base: `origin/main@0b10e4e4d9648cdaf913193b3eed207e71bbdbb9`
- Deploy mode: remote (`gastownhall/gascity`)
- Gate result: **PASS**

The pre-flight lookup found no pull request associated with the reviewed commit,
so the normal release gate applied. Criterion 6 was evaluated first and required
no bounded self-rebase.

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-kf5zn8` is closed with `verdict: pass` on the exact reviewed commit. |
| 2 | Acceptance criteria met | PASS | The fingerprint compares size, mtime, and ctime on Unix; reuses the existing `os.Stat` result; gives the fake filesystem an independent ctime; supplies an explicit Windows approximation; narrows the residual-collision comment; and adds the exact same-size, restored-mtime regression. No wire, schema, OpenAPI, or dashboard surface changes. |
| 3 | Tests pass | PASS | The corrected documented sharded lanes completed 40 PASS / 6 FAIL / 0 SKIP jobs. Five failure signatures satisfy all four pre-existing-failure clauses. `TestCleanInstallTutorialPath` is attributed by the explicit mayor ruling `gm-wisp-feey5w`: its trigger is legacy circuit-breaker cleanup machine state, making a same-state base reproduction unsatisfiable. The diff-owned regression test passed by name with no waiver. |
| 3a | Pre-existing failures attributable | PASS | Four tracked failure classes reproduce on `origin/main` with no path overlap. The tracked tutorial stdout-pollution failure is attributed narrowly to `ga-rsktma` by mayor ruling `gm-wisp-feey5w`, not by base reproduction; the ruling does not waive clause 3 for any other signature. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`, `make fmt-check-changed`, and `go vet ./...` all exited 0. |
| 4 | No high-severity review findings open | PASS | Reviewer recorded no style, security, specification, or uncovered-criteria findings; unresolved HIGH count is 0. |
| 5 | Final branch is clean | PASS | `git status --short` was empty at the reviewed commit before this gate record was created. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main 84f0af4833b3e59a178b0d4952913fb971f90bbe` produced tree `76eb545010645d8d85cf792346d0b2beb1301c37` with no conflicts. |
| 7 | Single feature theme | PASS | The ancestry-scope guard passed for `ga-8cce0j` and its reviewed source bead `ga-kf5zn8`. All five changed files implement or test one ctime-aware pack-fingerprint behavior under `internal/config` and `internal/fsys`. |

## Test evidence

The gate used checksum-pinned `bd v1.1.0` and Dolt `2.1.7`, an isolated
rootless Podman service, `DOCKER_HOST` pointed at that service, and
`TESTCONTAINERS_RYUK_DISABLED=true`.

- `go test -count=1 -run '^TestPackContentHashRecursiveDetectsMtimePreservingEdit$' -v ./internal/config`
  - 1 PASS / 0 FAIL / 0 SKIP.
  - `diff_tests_executed: TestPackContentHashRecursiveDetectsMtimePreservingEdit PASS`
  - `waiver_ref: none`
- `LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-fast-parallel`
  - 8 PASS / 2 FAIL / 0 SKIP jobs.
- `LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m make test-cmd-gc-process-parallel`
  - 6 PASS / 1 FAIL / 0 SKIP jobs.
- `LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-integration-shards-parallel`
  - 26 PASS / 3 FAIL / 0 SKIP jobs.
- `make test-ci-policy`
  - PASS: 5 runner-policy tests, 15 CI-suite-coverage tests, `scripts/cipolicy`, and the focused static-scope contracts.
- `make fmt-check-changed`
  - PASS.
- `go vet ./...`
  - PASS.

## Failure attribution

The diff changes only:

- `internal/config/fingerprint_ctime_unix.go`
- `internal/config/fingerprint_ctime_windows.go`
- `internal/config/pack.go`
- `internal/config/pack_test.go`
- `internal/fsys/fake.go`

The following failures are not diff-owned, have tracked beads, reproduce on the
base ref, and have no package/path overlap with the diff:

- `TestOSSProjectsNoUnregisteredBackendEnv/source` -> `ga-5em`; base-ref
  reproduction fails when the observed nested-worktree condition is present.
- `TestProviderLiveClaudeKindPath` -> `ga-cqq3hs.1`; base-ref reproduction fails
  with the identical `agent_pane_busy` / `w1:p1` signature.
- `TestGetKeyBinding_CapturesDefaultBinding` and
  `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-afqddr`; both exact
  base-ref tests fail because the host tmux default key table is empty.
- `TestE2E_SuspendResume_City` -> `ga-rntpsh`; the exact base-ref test fails with
  the same missing `citysus.report` timeout.

The remaining failure has a narrow merge-authority attribution because its
trigger is machine state rather than chance:

- `failure_attribution: TestCleanInstallTutorialPath -> ga-rsktma + mayor ruling
  (state-dependent trigger; clause 3 unsatisfiable, attributed by ruling not by
  base repro)`

Mayor ruling `gm-wisp-feey5w` identifies the defect as a real `bd config get`
stdout-contract violation caused only when legacy closed circuit-breaker files
exist for cleanup. The clean `origin/main` run lacked that machine state, so it
could not reproduce or disprove the defect. The ruling authorizes attribution
for this signature only and explicitly forbids quarantining the test.

The pre-push hook also encountered one teardown-only failure after the test
body passed:

- `failure_attribution: TestCustomTypesCheck_TableDrift -> ga-t33q83 + mayor
  ruling gm-wisp-xnkthw (teardown-only race; test body passed; clause 3 absent,
  attributed by ruling)`

The diff does not own or overlap this test. Its assertions passed, then
`t.TempDir` cleanup reported a lingering eventkit-store lock. The exact
`make test-fast-parallel` lane on
`origin/main@0b10e4e4d9648cdaf913193b3eed207e71bbdbb9` passed all 10 jobs, so a
same-signature base failure was absent. The merge authority authorized this
narrow attribution because the cleanup race carries no information about the
ctime fingerprint change; `ga-t33q83` tracks the underlying fix.

## Re-clear after bounded self-rebase — 2026-09-01

Mayor decision `ga-wmips8` authorized rebasing PR #5367 and re-clearing its
new head after the original CI run remained pinned before the deflake in
#5366. `attempt_bounded_self_rebase` moved the PR from
`567c91009da37533192b2af12087dc18a9782ddf` to
`3ef9158c76f869f31901299042b65079cc286e5a` and pushed with
`--force-with-lease`. This section independently re-evaluates the release gate
on that rebased head. The commit carrying this section is the gate-record-only
successor and must be the head that receives deploy clearance.

### Re-clear checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-kf5zn8` records `verdict: pass` on `84f0af4833b3e59a178b0d4952913fb971f90bbe`. Stable patch IDs independently recomputed across each commit's merge base are identical: `ab12c852d9231644f31f5ba1cd4257486ab78606` for the reviewed feature and rebased feature tip `3423fa0e9c3dbe1d6128734e81dfdcc7e7a982ec`. `review_carryover_verified: 84f0af4833b3e59a178b0d4952913fb971f90bbe -> 3423fa0e9c3dbe1d6128734e81dfdcc7e7a982ec`. |
| 2 | Acceptance criteria met | PASS | Re-read against the rebased diff: Unix fingerprints compare size, mtime, and ctime without an added stat call; fake-FS ctime is independently mutable; Windows has an explicit approximation; the residual-collision comment is narrowed; and the same-size/restored-mtime regression remains present. No wire, schema, OpenAPI, or dashboard surface changed. |
| 3 | Tests pass | PASS with attribution | `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel`; `test_cmd_scope: full-suite`; **37/40 jobs PASS, 3/40 raw FAIL, 0 skipped jobs**. All three raw failures meet clauses 1-4 of the non-diff-owned failure protocol and are attributed below. The full run's `unit-core` job reports `internal/config` green; the diff-owned test was then resolved explicitly by name and PASSed. `waiver_ref: none`. |
| 3a | Pre-existing failures attributable | PASS | `TestBdFlagManifestCurrent` is tracked by `ga-f0uceo`; the two beads#4566 fixture-init failures are tracked by `ga-esyijp`. Both trackers predate this run, this sighting was appended to each, unrelated candidate runs reproduce the same conditions, and no failing test path overlaps this diff. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy` PASS; merge-base-scoped `make lint-affected` PASS with 0 issues; `make fmt-check-changed` PASS; `go vet ./...` PASS; `go build ./...` PASS. The affected/static comparison ref was the actual PR merge base `a4361e58228b82b668609c19159031baa0d6928c`, yielding exactly the six PR paths. |
| 3c | CI-config lane | PASS | `ci_lane_run: n/a (no CI job, matrix, timeout, workflow, or required-check change)`. Before this gate-record-only commit, GitHub reported every applicable check on `3ef9158c76f869f31901299042b65079cc286e5a` complete and successful; suite-controlled non-applicable jobs were skipped. |
| 4 | No high-severity review findings open | PASS | The reviewer recorded no style, security, specification, or uncovered-criteria findings; unresolved HIGH count remains 0. |
| 5 | Final branch is clean | PASS | The unrelated `ga-sqv77i` untracked gate draft was preserved outside the worktree at `/var/tmp/ga-sqv77i-orphan-sweep-live-claims-gate.untracked-20260901T0820.md`; `git status --short` was empty before this gate record was edited. |
| 6 | Branch diverges cleanly from main | PASS | After fetching `origin/main@28ddd183f2e2f0b224474a64861ba9a7539284f2`, `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree `eef99757cbf1c4cfa6c4159ed2b923bc94b9c237`. Merge base: `a4361e58228b82b668609c19159031baa0d6928c`. GitHub also reported the PR `MERGEABLE` / `CLEAN`. |
| 7 | Single feature theme | PASS | The rebased range changes only ctime-aware pack fingerprinting and its existing gate record: five implementation/test files under `internal/config` and `internal/fsys`, plus this checklist. |

### Re-clear test evidence

- Container environment:
  - Rootless Podman 5.8.4 socket returned `OK` before the run.
  - Repository-pinned `docker.io/dolthub/dolt:2.1.7` was pulled and resolved to digest `sha256:22319531c51c2fb2ca3639ad284d0ff9a98b55c25c6ba4ebeefbf7769e663916`.
  - Current testcontainers image `docker.io/dolthub/dolt-sql-server:2.2.0` was pulled and resolved to digest `sha256:85232ce3343b1a8bcf409dd7bd97bb500690d37ba07115ece75ec0102fe2b268`.
- Full suite:
  - `test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel`
  - `test_cmd_scope: full-suite`
  - `test_counts: 37 PASS / 3 raw FAIL / 0 SKIP jobs`
  - Raw logs: `/var/tmp/gc-local-tests.sQzqiv`
- Diff-owned test:
  - `TestPackContentHashRecursiveDetectsMtimePreservingEdit`: PASS in the full run's green `internal/config` package and PASS by explicit name with `go test ./internal/config -run '^TestPackContentHashRecursiveDetectsMtimePreservingEdit$' -count=1 -v`.
  - `diff_tests_executed: TestPackContentHashRecursiveDetectsMtimePreservingEdit PASS`
  - `waiver_ref: none`
- Required non-test lanes:
  - `policy_lane: make test-ci-policy — PASS`
  - `GOLANGCI_LINT_CACHE=/var/tmp/ga-wmips8-golangci-cache LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=a4361e58228b82b668609c19159031baa0d6928c make lint-affected` — PASS, 0 issues.
  - Merge-base-scoped `make fmt-check-changed`, `go vet ./...`, and `go build ./...` — PASS.

### Re-clear failure attribution

- `failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(b), cross-candidate proof — identical installed-bd manifest drift is recorded on unrelated candidates and exact origin/main; the host CLI exposes flags absent from internal/bdflags.`
  - Clauses 1 and 4: the candidate does not add or modify `internal/bdflags/freshness_test.go` or any `internal/bdflags` path.
  - Clause 2: `ga-f0uceo` was created 2026-08-15 and names this exact test; this run's sighting is comment-verified.
- `failure_attribution: TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash -> ga-esyijp | clause 3(b), cross-candidate proof — unrelated PR #5841's gate hit the identical beads#4566 dirty-dependencies migration during fixture gc init.`
- `failure_attribution: TestCleanInstallTutorialPath -> ga-esyijp | clause 3(b), cross-candidate proof — unrelated deploy gates recorded the identical beads#4566 dirty-schema fixture-init condition before feature assertions.`
  - Clauses 1 and 4 for both integration tests: neither `test/integration/review_formula_test.go` nor `test/integration/tutorial_path_test.go` is in the diff.
  - Clause 2: canonical root-condition tracker `ga-esyijp` was created 2026-08-29 and explicitly consolidates dirty-schema fan-out; this run's paired sighting is comment-verified.
  - Added-test-load guard: no census, test-resource manifest, Makefile, workflow, new test target, or other test-load path changed.
