# Release Gate: CI push concurrency groups

- Deploy bead: `ga-h46avo`
- Source review bead: `ga-3fw26n`
- Reviewed source SHA: `b337159a6a99d378e083372c4def8e0c875581b8`
- Base checked: `origin/main` at `d2142785fdab11831110fb090eadda28d2d59d96`
- Gate date: `2026-07-30`
- Release criteria source: `docs/PROJECT_MANIFEST.md` is absent from this
  checkout, so this checklist uses the active deployer role's seven release
  criteria and the repository testing policy in `TESTING.md`.

## Release criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | The deploy bead records `Reviewed + PASSED by reviewer gascity/reviewer` for the exact reviewed SHA. The source review bead records `REVIEWER VERDICT (gascity/reviewer)` and `Verdict: PASS. No blockers.` |
| 2 | Acceptance criteria met | PASS | Push-event concurrency groups now include `github.sha`; the pull-request fallback remains `pull_request.number \|\| ref \|\| run_id`; `cancel-in-progress` remains limited to pull requests; the CI execution-policy hash was updated; and `TestPushRunsGetPerCommitConcurrencyGroup` directly locks all three expression requirements. |
| 3 | Tests pass | PASS | On a fresh isolated worktree at the reviewed SHA: `go build ./...` passed; `make lint` passed with `0 issues`; `make fmt-check` passed; `make vet` passed; `make test-ci-policy` passed (20 Python tests plus its focused Go policy checks); `go test -json -count=1 ./scripts/...` recorded 339 test/subtest PASS, 0 FAIL, 0 SKIP; and `make test-fast-parallel` passed 10 jobs, 0 failed jobs, 0 skipped jobs. The fast profile intentionally omits `GC_FAST_UNIT=0` process cases; `TESTING.md` assigns those to the separate process suite, and this diff changes only CI workflow/policy files. |
| 4 | No high-severity review findings open | PASS | Reviewer notes say `No blockers`, `style_findings: none`, and `security_findings: none`; unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | Both the role checkout and fresh isolated gate worktree were clean after the gate. This checklist is the sole deployer-authored file and will be committed on the mechanically derived isolated deploy branch. |
| 6 | Branch diverges cleanly from main | PASS | `origin/main` is the merge base and direct parent-side base of the reviewed commit set. `git merge-tree --write-tree origin/main b337159a6a99d378e083372c4def8e0c875581b8` exited 0 and produced tree `2052c7191cd88aafe568691898f497e623c4a0d3`. No self-rebase was needed. |
| 7 | Single feature theme | PASS | The two commits touch only `.github/workflows/ci.yml` and its `scripts/cipolicy` hash/test guard. The commit set is one CI-run concurrency-correctness theme with no independent feature bundled. |

## Acceptance evidence

- Push events use the commit SHA in the concurrency group, so a newer push to
  the same branch does not cancel the earlier commit's run.
- Pull-request runs keep their prior grouping fallback and remain the only
  event type with `cancel-in-progress` enabled.
- The pinned execution hash recognizes the intentional workflow change.
- The new regression test reads the checked workflow and asserts the complete
  group and cancellation expressions verbatim.
- The net diff is three files, 26 insertions, and 2 deletions.

## Test evidence

```text
go build ./...                                 PASS
make lint                                      PASS, 0 issues
make fmt-check                                 PASS
make vet                                       PASS
make test-ci-policy                            PASS
  Python runner-policy tests                   5 PASS, 0 FAIL, 0 SKIP
  Python suite-coverage tests                  15 PASS, 0 FAIL, 0 SKIP
  focused Go policy packages                   PASS
go test -json -count=1 ./scripts/...           339 PASS, 0 FAIL, 0 SKIP
make test-fast-parallel                        10 PASS jobs, 0 FAIL, 0 SKIP jobs
git diff --check origin/main...HEAD            PASS
```

An initial lint invocation in the long-lived role worktree found three
diagnostics under ignored
`internal/api/dashboardspa/web/node_modules/flatted/golang/`. That directory is
not tracked, is absent from the reviewed diff, and did not exist in the fresh
isolated gate worktree. The clean isolated run above is the candidate result;
the contaminated output was retained as environment evidence and was not
reported as a product-test failure.

## Verdict

PASS. The reviewed SHA is eligible for an isolated deploy branch, pull request,
and merge-authority handoff.
