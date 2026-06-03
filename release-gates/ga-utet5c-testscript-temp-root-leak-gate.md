# Release Gate: testscript temp root leak fix

- Deploy bead: ga-utet5c
- Source bug: ga-lh1k9
- Review bead: ga-wdz76d
- PR: https://github.com/gastownhall/gascity/pull/2996
- Feature branch: builder/ga-lh1k9-testscript-temp-leak
- Source commit: 6161bb8ec3c2698351fb59a06d7fa2ccd72f7301
- Base checked: origin/main at 5a9a2e26061495ab2d133e62b755d2409f82c744

## Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-wdz76d` is closed with reason `pass`; notes contain `VERDICT: PASS` for commit `6161bb8ec3c2698351fb59a06d7fa2ccd72f7301`. |
| 2 | Acceptance criteria met | PASS | Source bug `ga-lh1k9` identified leaked `/tmp/gct<PID>-*` roots from testscript re-exec paths. The commit moves `testscript.Main` behind an early return before temp-root setup, adds stale-root sweep coverage, and adds `TestTestscriptCommandInvocationDoesNotLeakTempRoot` to assert re-exec invocations do not leave `/tmp/gct<PID>-*` roots. |
| 3 | Tests pass | PASS | `make test-fast-parallel` passed: fsys darwin compile, unit-core, and all six `cmd/gc` unit shards. `go vet ./...` passed. PR #2996 GitHub checks are green for required CI, CodeQL, integration shards, dashboard, and worker phase 2. |
| 4 | No high-severity review findings open | PASS | Reviewer notes for `ga-wdz76d` report `BLOCKERS: None`; no unresolved high-severity findings were listed. |
| 5 | Final branch is clean | PASS | Clean deploy worktree was created from `origin/builder/ga-lh1k9-testscript-temp-leak`; `git status --short --branch` was clean before writing this gate. Final cleanliness was rechecked after committing the gate file. |
| 6 | Branch diverges cleanly from main | PASS | PR #2996 reports `mergeStateStatus: CLEAN`; `git merge-tree --write-tree --quiet origin/main HEAD` exited 0 before the gate commit. |
| 7 | Single feature theme | PASS | One source commit touches only `cmd/gc/main_test.go` and `cmd/gc/test_orphan_sweep_branches_test.go`; both changes address the same testscript temp-root leak in test infrastructure. |

## Commands Run

```text
gh pr view 2996 --json number,title,state,isDraft,url,baseRefName,headRefName,headRepositoryOwner,headRepository,mergeStateStatus,statusCheckRollup,commits,files,body
bd show ga-wdz76d
bd show ga-lh1k9
git diff --stat origin/main...HEAD
git log --oneline --decorate origin/main..HEAD
git merge-tree --write-tree --quiet origin/main HEAD
make test-fast-parallel
go vet ./...
```

## Notes

`docs/PROJECT_MANIFEST.md` is not present in this repository checkout; this gate applies the deployer release criteria supplied in the agent instructions.
