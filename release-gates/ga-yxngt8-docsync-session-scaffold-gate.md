# Release Gate: docsync session scaffold coverage skip

- Bead: `ga-yxngt8`
- Source bead: `ga-z7evh4.2`
- Branch: `deploy/ga-yxngt8-gate`
- Candidate before gate commit: `7ae5019665b0308efc094d0075bd761899ed69bf`
- Base: `origin/main` at `80e5166473033b9f2807dad048ddcb70dfc3b86e`
- Evaluated: `2026-07-24T15:06:09Z`

`docs/PROJECT_MANIFEST.md` is not present in this repository at the evaluated
commit, so this gate uses the deployer release criteria from the role contract.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Deploy bead description records reviewer PASS for `ga-z7evh4.2`; source bead notes include `REVIEWER VERDICT: PASS`. |
| 2 | Acceptance criteria met | PASS | Source acceptance required confirming the original `ga-4tjr99` fix had not already landed, preparing a clean candidate with only the `TestDocDirCoverage` session-scaffold skip, excluding stranded routed-demand/throttle content, and recording branch/commit/test evidence. Reviewer notes confirm those checks. Deployer re-checks found `origin/main` has no `isSessionScaffoldRoot`, and `git diff --name-only origin/main...HEAD` is exactly `test/docsync/docsync_test.go`. |
| 3 | Tests pass | PASS | Candidate checks passed: `gofmt -l test/docsync/docsync_test.go` produced no output; `git diff --check origin/main...HEAD` passed; `go build ./...` passed; `go vet ./...` passed; `go test -count=1 ./test/docsync/... -v` passed 11/11 tests, including `TestDocDirCoverage`. Broader `make test-fast-parallel` failed one unrelated `cmd/gc` shard at `TestErrorReturningSessionProviderFactoriesPreserveSuccessBehavior/default`; the exact test also fails on `origin/main` from this same nested worktree environment and is already tracked as `ga-y4se3w`, so this is not a regression from the one-file docsync diff. |
| 4 | No high-severity review findings open | PASS | Source review records no exploitable security issue; the only disclosed gap is direct unit coverage for the new test helper's skip branch, tracked separately as `ga-f9udbh` and accepted by reviewer as proportionate. No unresolved HIGH/CRITICAL finding is recorded on the deploy bead or source review notes. |
| 5 | Final branch is clean | PASS | Before adding this gate file, `git status --short --branch` showed only `## deploy/ga-yxngt8-gate`. This gate file is the only deployer-authored release commit. |
| 6 | Branch diverges cleanly from main | PASS | Checked first: `git merge-tree --write-tree origin/main 7ae5019665b0308efc094d0075bd761899ed69bf` exited 0 and produced tree `f4c3398ffa1cc0d49f79335f75796d6914a1c01b`. Re-checked on the deploy branch with the same clean result. `git merge-base --is-ancestor 7ae5019665b0308efc094d0075bd761899ed69bf origin/main` exited 1, confirming the candidate is not already landed. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem and one file: `test/docsync/docsync_test.go`. The behavior is limited to skipping per-session `.gc` scaffold directories during the documentation directory census. |
