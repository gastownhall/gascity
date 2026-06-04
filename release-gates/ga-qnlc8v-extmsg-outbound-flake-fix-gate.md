# Release Gate: extmsg outbound flake fix

Bead: ga-qnlc8v
Source implementation bead: ga-cu8z0x
Source review bead: ga-jwck43
PR: https://github.com/gastownhall/gascity/pull/3099
Branch: fix/ga-6d3g9c-drain-ack-poke
Reviewed commit: c52f6b8b969288b84f27b56fb1c62a60b49d68c8
Current main: c85caa45b32f02e7751dd58aca9690d276cde9f8
Gate worktree: /home/jaword/projects/gc-management/.gc/worktrees/gascity/builder
Gate date: 2026-06-04

Note: docs/PROJECT_MANIFEST.md is not present in this checkout. This gate uses
the release criteria from the deployer role prompt and TESTING.md.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | ga-jwck43 is closed with `REVIEW VERDICT: PASS`; reviewer recorded PR #3099, branch `fix/ga-6d3g9c-drain-ack-poke`, commit `c52f6b8b969288b84f27b56fb1c62a60b49d68c8`, and no blockers. |
| 2 | Acceptance criteria met | PASS | ga-cu8z0x required replacing the single-shot `IsRunning` assertion with a poll loop and reading fake runtime calls via a locked snapshot accessor. Commit `c52f6b8b9` touches only `internal/api/handler_extmsg_test.go` and `internal/runtime/fake.go`, adding the poll loop and `Fake.SnapshotCalls()`. Reviewer notes say the diff matches the validated investigator spec. |
| 3 | Tests pass | FAIL | Builder/reviewer evidence records `go vet ./internal/api ./internal/runtime`, race `-count=300`, 48x40 parallel no-race runs, and sibling package tests passing on the reviewed commit. GitHub CI on PR #3099 tip is green. This release gate still fails because no valid final branch exists against current `origin/main`: the dry merge conflicts in `internal/api/handler_extmsg_test.go`, so current-main-integrated tests were not run. |
| 4 | No high-severity review findings open | PASS | ga-jwck43 records `PASS - no blockers` with INFO-only findings. |
| 5 | Final branch is clean | PASS | Before writing this gate, `git status --short --branch` in the branch worktree reported no uncommitted changes. |
| 6 | Branch diverges cleanly from main | FAIL | After `git fetch origin main`, `git merge-tree --write-tree origin/main HEAD` exited 1 with a content conflict in `internal/api/handler_extmsg_test.go`. Merge base is `8ad393860a6dc5139a0786cf514d7c4677b26dd1`; current `origin/main` is `c85caa45b32f02e7751dd58aca9690d276cde9f8`. |
| 7 | Single feature theme | PASS | The four intermediate cherry-picked commits are patch-equivalent to current main: `git cherry -v origin/main HEAD` marks `9565551d`, `9f149768`, `900d9447`, and `86b86a51` with `-`. The remaining non-main changes are PR #3099's previously gated drain-ack fix, its gate file, and this test-only extmsg stabilization. |

## Commands

```text
bd show ga-qnlc8v
bd show ga-jwck43
bd show ga-cu8z0x
git fetch origin main
git status --short --branch
git rev-parse origin/main
git merge-base origin/main HEAD
git cherry -v origin/main HEAD
git merge-tree --write-tree origin/main HEAD
git diff --check origin/main...HEAD
gh pr view 3099 --json number,title,state,baseRefName,headRefName,headRepositoryOwner,url,commits,statusCheckRollup
```

## Cherry-pick no-op check

`git cherry -v origin/main HEAD` confirms the four intermediate commits called
out by the reviewer are already present on `origin/main` by patch identity:

```text
- 9565551d87bec72566d40a618ac9b842a49f5d6c Bead leaks: follow graph deps for order wisp checks (#2922)
- 9f149768d252f552f7157e360d4a5bd7ec2e21ff fix(dispatch): wait for retry attempts before scope close (#3083)
- 900d9447557e06866e2b96b4fb3b1a6b171b659b fix(reconciler): count assigned sessions for active scale slots (#3084)
- 86b86a51a207bae8f5b6a14c41edff3ebca720d0 fix(dispatch): convoy reopen pre-routes to gc.run_target atomically (#3060)
```

## Conflict evidence

```text
git merge-tree --write-tree origin/main HEAD

6dd70d4ed8c2ada038c06f3aab3687bff65ee64d
100644 a5a165352917538519c78da5a4172073ca2ea580 1 internal/api/handler_extmsg_test.go
100644 d64a2c7161668e18b8e81a142c0e2caf2358cbc2 2 internal/api/handler_extmsg_test.go
100644 f59390754a973c48cc8c7c4fd35936c4c1c2dfe1 3 internal/api/handler_extmsg_test.go

Auto-merging internal/api/handler_extmsg_test.go
CONFLICT (content): Merge conflict in internal/api/handler_extmsg_test.go
```

## Decision

FAIL. The reviewed extmsg test fix is acceptable, and the four cherry-picked
commits are clean no-ops against current main, but PR #3099 no longer merges
cleanly with `origin/main`. Route ga-qnlc8v back to builder for a rebase or
branch refresh; the deployer must not resolve this content conflict.
