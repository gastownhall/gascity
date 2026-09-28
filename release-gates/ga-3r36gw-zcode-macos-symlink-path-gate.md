# Release gate: zcode macOS symlink-path matching

- Deploy bead: `ga-3r36gw`
- Source defect bead: `ga-51umv4`
- Round-two build bead: `ga-qdjzjv`
- Review bead: `ga-feseu0`
- Reviewed and gated source: `7a6539ae0b35017fbe440030f3933c82bd85bae8`
- Candidate merge base: `018a60c81ffda8325b48cb2064062abe6ee83641`
- Current base checked: `origin/main@63abaf27dcd0d01c747ff5e50cfe19de0f5e2f1d`
- Decision: **PASS**

No matching feature work package or `docs/PROJECT_MANIFEST.md` exists in this
checkout. This record applies the active seven-criterion deploy protocol and
the explicit exit contract on `ga-51umv4`.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-feseu0` is closed with `verdict: pass` after independent review of the complete `018a60c8..7a6539ae` diff. The review explicitly resolves the stale metadata SHA: the authoritative reviewed tip is `7a6539ae0b35017fbe440030f3933c82bd85bae8`; its final commit changes only a stale test comment. |
| 2 | Acceptance criteria met | PASS | Both zcode work-directory comparisons now use the existing symlink-aware `pathutil.SamePath`. Production changes are confined to `internal/sessionlog/zcode_reader.go`; Linux-manufactured symlink tests cover both scope and ID lookup. The original adapter regressions also pass on Linux and in the exact-SHA macOS lane. `internal/sessionlog/opencode_reader.go` remains untouched as explicitly out of scope. |
| 3 | Tests pass | PASS with attributed non-diff-owned failures | The documented `make test-local-full-parallel` sweep ran all 40 jobs: 36 PASS / 4 attributed FAIL / 0 SKIP. `unit-core`, which contains `internal/sessionlog`, passed. Both diff-owned tests and both original adapter regressions pass in named reruns. The exact reviewed SHA was then checked out by macOS workflow run `34742169727`; job `Mac / make test` passed in 13m57s and its observable wrapper ended PASS. |
| 3b | Policy and static lanes | PASS | `make test-ci-policy`, `go build ./...`, `go vet ./...`, merge-base-scoped `make fmt-check-changed`, merge-base-scoped `make lint-affected`, and `git diff --check` pass. A conservative `origin/main`-scoped lint invocation widened to the full repository because this stale reviewed head lacks a newer main-only gate file; its only findings were the three known ignored `node_modules/flatted` diagnostics tracked by `ga-bvixfw`. |
| 3c | CI-config lane | PASS | `ci_lane_run: n/a (no CI job, matrix, timeout, or required-check configuration changed)`. The separate macOS platform run is acceptance evidence for this macOS-specific fix, not a CI-config lane. |
| 4 | No high-severity review findings open | PASS | `ga-feseu0` records no blocker, major, or security finding. The prior round's functional completeness gap was fixed and re-reviewed. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty before this gate record was created. The gate record is the only deploy-evidence addition. |
| 6 | Branch diverges cleanly from main | PASS | After a fresh fetch, `git merge-tree --write-tree origin/main 7a6539ae0b35017fbe440030f3933c82bd85bae8` exited 0 against `origin/main@63abaf27dcd0d01c747ff5e50cfe19de0f5e2f1d` and produced tree `a7e5967d5bf4ffb9c05c7742b6d63d54251f3582`. The candidate is four commits behind and five ahead; no rebase was required. |
| 7 | Single feature theme | PASS | Five TDD/review-fixup commits change only `internal/sessionlog/zcode_reader.go` and `internal/sessionlog/zcode_reader_test.go` (+89/-2) for one theme: symlink-insensitive zcode work-directory matching. The ancestry-scope guard passes when given the confirmed chain IDs `ga-3r36gw`, `ga-51umv4`, `ga-qdjzjv`, and `ga-feseu0`. |

## Test evidence

`test_cmd_scope: full-suite`

The full local suite ran with the documented rootless-Podman environment:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
TESTCONTAINERS_RYUK_DISABLED=true
EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true'
LOCAL_TEST_JOBS=4 LOCAL_TEST_LOG_DIR=/var/tmp/ga-3r36gw-gate-20260913T0524Z/jobs make test-local-full-parallel
```

- `test_counts: 36 PASS jobs, 4 attributed FAIL jobs, 0 SKIP jobs`
- `top_level_failures: 4`
- `diff_tests_executed: TestFindZCodeSessionFileByIDResolvesSymlinkedWorkDir PASS; TestFindZCodeSessionFileByScopeResolvesSymlinkedWorkDir PASS`
- `acceptance_tests_executed: TestNonASCIISessionNameSanitizesByteWiseOnBothSides PASS; TestFreshSeatDoesNotAdoptAClosedSiblingsNameOnlyState PASS`
- `skip_justification: no full-suite job reported SKIP; neither diff-owned test skipped`
- `waiver_ref: none`
- Full-suite log: `/var/tmp/ga-3r36gw-gate-20260913T0524Z/full-suite.log`
- Per-job logs: `/var/tmp/ga-3r36gw-gate-20260913T0524Z/jobs`
- Focused sessionlog log: `/var/tmp/ga-3r36gw-gate-20260913T0524Z/focused-sessionlog.log`
- Focused adapter log: `/var/tmp/ga-3r36gw-gate-20260913T0524Z/focused-zcode-adapter.log`

### macOS confirmation

- Workflow: `Mac Regression`, manual `suite=needs-mac` run
  [34742169727](https://github.com/gastownhall/gascity/actions/runs/34742169727)
- Required job: [Mac / make test](https://github.com/gastownhall/gascity/actions/runs/34742169727/job/103683723590) — **PASS** in 13m57s.
- Checkout evidence in the job log shows `ref: 7a6539ae0b35017fbe440030f3933c82bd85bae8`, fetch and checkout of that exact object, and `git log -1 --format=%H` returning the same SHA.
- The observable test log names both original regressions as executed and ends `observable go test: PASS`.
- An earlier dispatch `34742152844` contained a mistyped SHA and was canceled before execution; it is not gate evidence.

### Failure attribution

| Raw result | Tracker | Attribution |
|---|---|---|
| FAIL: `TestGraphWorkflowSuccessPath` | `ga-esyijp` | Clause 3(a), mechanism: fixture setup stopped on the installed `bd` shared-server schema refusal (`v54 -> v66`) before sessionlog discovery could run. No path overlap. |
| FAIL: `TestHumaBinary_SessionMessageAsync` | `ga-esyijp` | Clause 3(a), mechanism: city initialization stopped on the same shared-server schema refusal (`v47 -> v66`). No path overlap. |
| FAIL: `TestCleanInstallTutorialPath` | `ga-esyijp` | Clause 3(a), mechanism: city initialization stopped on the same shared-server schema refusal (`v49 -> v66`). No path overlap. |
| FAIL: `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` | `ga-j88sfp` | Clause 3(a), mechanism: the exact predating tracker covers the missing `review.attempt.2` signature. The fixture pins `session.provider = "subprocess"`; transcript discovery dispatches the changed functions only for provider family `zcode`, so the candidate is unreachable. No path overlap. |

`failure_attribution: TestGraphWorkflowSuccessPath -> ga-esyijp | clause 3(a) mechanism — bd schema refusal during fixture initialization; candidate unreachable`

`failure_attribution: TestHumaBinary_SessionMessageAsync -> ga-esyijp | clause 3(a) mechanism — bd schema refusal during city initialization; candidate unreachable`

`failure_attribution: TestCleanInstallTutorialPath -> ga-esyijp | clause 3(a) mechanism — bd schema refusal during city initialization; candidate unreachable`

`failure_attribution: TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash -> ga-j88sfp | clause 3(a) mechanism — subprocess fixture cannot dispatch zcode-only reader paths`

Both trackers predate this run, were opened, and received read-back-verified
comments for these occurrences.

## Required lanes

- `policy_lane: make test-ci-policy — PASS`
- `go build ./...` — PASS
- `go vet ./...` — PASS
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=018a60c81ffda8325b48cb2064062abe6ee83641 make fmt-check-changed` — PASS
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=018a60c81ffda8325b48cb2064062abe6ee83641 make lint-affected` — PASS, 0 issues
- `git diff --check origin/main...HEAD` — PASS
- `LINT_CHANGED_REF=origin/main make lint-affected` — raw FAIL after conservative full-repository expansion; only the exact two `govet` and one `revive` diagnostics in ignored `internal/api/dashboardspa/web/node_modules/flatted/golang/pkg/flatted/flatted.go`.

`policy_attribution: dashboard node_modules/flatted findings -> ga-bvixfw | predating clean-main reproduction and repeated exact sightings; ignored third-party path is absent from the candidate diff`

