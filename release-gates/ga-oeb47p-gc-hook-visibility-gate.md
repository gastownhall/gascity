# Release Gate: consistent `gc hook` visibility

- Deploy bead: `ga-oeb47p`
- Review bead: `ga-dooipb`
- Build bead: `ga-1xaqgo.2`
- Reviewed source: `a4e9c3b40602c4787feaf23c5c29abe82fda4cca`
- Base evaluated: `origin/main@2cd07e018bf3680d24b037b509e6a4bad5e623ba`
- Deploy mode: remote
- Overall verdict: **PASS WITH ATTRIBUTED FAILURES**

The already-merged preflight found no pull request carrying the reviewed
source. Criterion 6 passed first, and the source ancestry passed the deploy
scope guard for `ga-oeb47p`, `ga-dooipb`, `ga-0t1lfm`, and `ga-1xaqgo.2`.
The full CI union reported six red jobs, but every failure is non-diff-owned,
predates this run under an exact tracker, has no path overlap with the diff,
and has a direct mechanism or coverage proof below. The required lint command
also hit a tracked full-fallback path leak rather than a finding in this diff.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-dooipb` is closed with a PASS verdict pinned to exact source `a4e9c3b40602c4787feaf23c5c29abe82fda4cca`. |
| 2 | Acceptance criteria met | PASS | Plain `gc hook` now applies the same assignee and route visibility facts as the claim path. Assigned work is visible only to a matching identity; foreign agent/rig routes are dropped; unassigned/unrouted legacy work remains visible; canonical slash and double-dash route spellings share one matcher. The implementation reuses `hookClaimHasIdentity` and `hookRouteIdentitiesEqual` across display and claim paths. All ten added top-level tests and 22 subtests pass. |
| 3 | Tests pass | PASS WITH ATTRIBUTED FAILURES | Documented CI union `make EXTRA_TEST_ENV="DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true" test-local-full-parallel`, at the fleet's two-job cap: **34 PASS jobs, 6 FAIL jobs, 0 SKIP jobs**. All six red jobs are attributed below. Focused diff-owned run: **32 PASS events, 0 FAIL, 0 SKIP** across ten top-level tests and 22 subtests. `waiver_ref: none`. Summary log: `/var/tmp/ga-oeb47p-test-local-full.log`; job logs: `/var/tmp/gc-local-tests.S9nil6`. |
| 3b | Policy/lint lane | PASS WITH ATTRIBUTED FAILURE | `make test-ci-policy`: PASS. `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make fmt-check-changed`: PASS. `go build ./...`: PASS. `go vet ./...`: PASS. `git diff --check`: PASS. `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected`: red only after the source's absence of base-only `.github/workflows/pr-evidence-watchdog.yml` forced a full-repository fallback that traversed stale `/var/tmp/ga-*-gate` worktrees and dashboard `node_modules`; all 623 findings are outside the five-file diff. Exact predating tracker: `ga-u8z8j6`. |
| 4 | No high-severity review findings open | PASS | Reviewer recorded no security, style, or specification blocker and no unresolved HIGH finding. |
| 5 | Final branch is clean | PASS | Exact reviewed source was checked out detached; `git status --porcelain=v1` was empty before this checklist was written. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main a4e9c3b40602c4787feaf23c5c29abe82fda4cca` exited 0 and produced tree `a6481e443285e8fffcec20e562ec5e9fed6c06ef`; branch counts were 2 behind / 1 ahead. No self-rebase was needed. |
| 7 | Single feature theme | PASS | One commit changes five `cmd/gc` hook files for a single behavior: keep plain hook display visibility consistent with hook claim eligibility. |

## Diff-owned test execution

Command:

```text
GC_FAST_UNIT=0 go test -json -count=1 ./cmd/gc -run '^(TestHookRouteIdentitiesEqual|TestHookClaimMatchesRouteToleratesSessionNameEncoding|TestHookCandidateVisible|TestHookCandidateVisibleWorkflowRunTargetFallback|TestFilterForeignHookCandidatesFailsOpen|TestFilterForeignHookCandidatesDropsForeignKeepsOwnAndUnrouted|TestDoHookVisibilityIgnoredWhenEmpty|TestDoHookDropsForeignAssigneeUnderVisibility|TestDoHookKeepsUnroutedUnassignedWorkUnderVisibility|TestDoHookGa1xaqgoRegression)$'
```

Results: **32 PASS events, 0 FAIL, 0 SKIP**.

- `TestHookRouteIdentitiesEqual` — PASS (7 subtests).
- `TestHookClaimMatchesRouteToleratesSessionNameEncoding` — PASS.
- `TestHookCandidateVisible` — PASS (9 subtests).
- `TestHookCandidateVisibleWorkflowRunTargetFallback` — PASS.
- `TestFilterForeignHookCandidatesFailsOpen` — PASS (6 subtests).
- `TestFilterForeignHookCandidatesDropsForeignKeepsOwnAndUnrouted` — PASS.
- `TestDoHookVisibilityIgnoredWhenEmpty` — PASS.
- `TestDoHookDropsForeignAssigneeUnderVisibility` — PASS.
- `TestDoHookKeepsUnroutedUnassignedWorkUnderVisibility` — PASS.
- `TestDoHookGa1xaqgoRegression` — PASS.

`diff_tests_executed`: every added test and subtest above executed and passed;
no diff-owned test skipped. The modified pre-existing hook tests also ran in
the full `cmd/gc` shards.

## Failure attribution

- `TestSendReloadControlRequestNoChange` -> `ga-movzgb` | clause 3(c),
  coverage: a focused run passed, and its coverage profile reports 0.0% for
  every changed hook function (`cmdHookWithOptions`, `workQueryEnvForDir`,
  `doHook`, `filterForeignHookCandidates`, `decodeHookCandidateBead`,
  `hookRouteIdentitiesEqual`, `hookClaimMatchesRoute`, and
  `hookCandidateVisible`). Profile: `/var/tmp/ga-oeb47p-reload.cover`.
- `TestBdFlagManifestCurrent` -> `ga-f0uceo` | clause 3(a), mechanism: the
  test compares the installed `bd` binary with `internal/bdflags`; neither is
  changed or reachable from the hook display/claim diff.
- `TestGetKeyBinding_CapturesDefaultBinding` and
  `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-afqddr` | clause
  3(a), mechanism: the host tmux 3.7b default key table is independent of the
  `cmd/gc` hook filter.
- `TestE2E_SuspendResume_City` -> `ga-yc0e3a` | clause 3(a), mechanism: the
  test invokes city suspend/resume and session kill, never `gc hook`; the
  tracked missing-`citysus.report` signature therefore cannot execute the
  changed path.
- `TestGCLiveContract_BeadsAndEvents` -> `ga-lpfjhc` (standing condition
  `ga-6bnc42`) | clause 3(a), mechanism: fixture initialization failed with
  the exact `gastownhall/beads#4566` dirty-table migration signature before
  any hook command or hook filter could execute.
- `lint-affected` full-fallback path leak -> `ga-u8z8j6` | mechanism: all 623
  findings name stale `/var/tmp` worktrees or dashboard `node_modules`; none
  names a changed file.

Every tracker above predates its cited run. Every failing test file/package is
path-disjoint from the five changed hook files. The coverage proof resolves
the only same-package failure.

## Release decision

The change is ready for an isolated deploy branch and pull request. The
attributed failures remain visible here; no failure was silently converted to
green and no waiver was self-granted.
