# Release Gate: Pool worker claim identity

Bead: ga-h9lsg4
Source bead: ga-aq6xfs
Implementation bead: ga-0cymgz
Branch under review: builder/ga-0cymgz
Reviewed commit: 6ee9ba04f49df788a5d7ec134c4b02d44c9d9d6c
Gate date: 2026-07-01

Note: docs/PROJECT_MANIFEST.md is not present in this worktree. This gate uses
the deployer release criteria and the repo testing guidance in TESTING.md.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-aq6xfs is closed with `REVIEW VERDICT: PASS`; deploy bead ga-h9lsg4 was created by reviewer-gm-u4aay with reviewed commit 6ee9ba04f49df788a5d7ec134c4b02d44c9d9d6c. |
| 2 | Acceptance criteria met | PASS | `cmd/gc/cmd_hook.go` removes `resolvedAgentName` from `IdentityCandidates` while retaining it in `RouteTargets`. The branch adds the required suffixed-worker acceptance test, named-holder over-fix guard, and mechanism-level identity-candidate test. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestCmdHookClaimSuffixedPoolWorkerDoesNotAdoptBareTemplateInProgressWork|TestCmdHookClaimNamedHolderStillAdoptsOwnInProgressWork|TestPoolWorkerIdentityCandidatesExcludeBareTemplate' -count=1` passed. `make test-fast-parallel` passed all 8 fast shards. `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list no unresolved HIGH findings; the review classifies the change as an access-control correctness improvement with no new attack surface. |
| 5 | Final branch is clean | PASS | No uncommitted changes before gate file creation; this gate file is committed as the branch tip. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` succeeded and produced tree d4889ccb4dd55e295788597b4a03757ed70f77a1. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem: `gc hook --claim` identity adoption behavior in `cmd/gc`, plus tests for that behavior. |

## Acceptance Checks

- PASS: Suffixed pool workers no longer include the bare pool template in
  adoption identity candidates.
- PASS: Fresh-claim routing still includes the resolved template through
  `RouteTargets`.
- PASS: Named holders and canonical slots still adopt their own in-progress
  work.
- PASS: The change is scoped to `cmd/gc/cmd_hook.go` and
  `cmd/gc/cmd_hook_test.go`.

## Commands

```text
gofmt -l cmd/gc/cmd_hook.go cmd/gc/cmd_hook_test.go
go test ./cmd/gc -run 'TestCmdHookClaimSuffixedPoolWorkerDoesNotAdoptBareTemplateInProgressWork|TestCmdHookClaimNamedHolderStillAdoptsOwnInProgressWork|TestPoolWorkerIdentityCandidatesExcludeBareTemplate' -count=1
make test-fast-parallel
go vet ./...
git diff --check origin/main...HEAD
git merge-tree --write-tree origin/main HEAD
```
