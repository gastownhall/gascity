# Release Gate: Reconciler namedWorkReady guard

Bead: ga-ghhxvv
Source bead: ga-uxzzey
Implementation bead: ga-n2szjj
Branch under review: builder/ga-n2szjj
Reviewed commit: 615d85b1475e604573d8aa8c525f93a16395a8ec
Gate date: 2026-07-01

Note: docs/PROJECT_MANIFEST.md is not present in this worktree. This gate uses
the deployer release criteria and the repo testing guidance in TESTING.md.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-uxzzey is closed with `REVIEW VERDICT: PASS`; deploy bead ga-ghhxvv was created by reviewer-gm-u4aay with reviewed commit 615d85b1475e604573d8aa8c525f93a16395a8ec. |
| 2 | Acceptance criteria met | PASS | `cmd/gc/build_desired_state.go` skips bare-template assigned work for templates that support expanded session identities. The branch adds `TestBuildDesiredState_MultiSlotPoolNamedSession_OneRoutedBeadProvisionsTwoWorkers` and retains `TestSharedTemplateAssignee_Tier1CrashRecoveryCrossAdopts`. |
| 3 | Tests pass | PASS | Required repro tests passed. Broader desired-state/pool regression family passed. `make test-fast-parallel` passed all 8 fast shards. `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list no unresolved HIGH findings; the change is described as a defense-in-depth correctness guard with no new attack surface. |
| 5 | Final branch is clean | PASS | No uncommitted changes before gate file creation; this gate file is committed as the branch tip. |
| 6 | Branch diverges cleanly from main | PASS | After refreshing `origin/main` to 87bbc7b36be171d6e2271eb0b887d547e0db0cf6, `git merge-tree --write-tree origin/main HEAD` succeeded and produced tree 9019c2a3caa1b79b95d8704aff3b136fb5826f43. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem: reconciler desired-state demand calculation for named sessions and tests for that behavior. |

## Acceptance Checks

- PASS: Bare-template assigned work no longer counts as named-session demand
  when the agent supports expanded session identities.
- PASS: Plain named-only and canonical singleton pool behavior remains covered
  by the existing `SupportsExpandedSessionIdentities` contract.
- PASS: The complementary prompt-level failure surface remains documented by
  `TestSharedTemplateAssignee_Tier1CrashRecoveryCrossAdopts`.
- PASS: The change is scoped to `cmd/gc/build_desired_state.go` and
  `cmd/gc/build_desired_state_ga80pen8_test.go`.

## Commands

```text
gofmt -l cmd/gc/build_desired_state.go cmd/gc/build_desired_state_ga80pen8_test.go
go test ./cmd/gc -run 'TestBuildDesiredState_MultiSlotPoolNamedSession_OneRoutedBeadProvisionsTwoWorkers|TestSharedTemplateAssignee_Tier1CrashRecoveryCrossAdopts' -count=1
go test ./cmd/gc -run 'TestBuildDesiredState|TestComputePoolDesiredStates|TestBuildAwakeInputFromReconciler|TestNamedWorkReady' -count=1
make test-fast-parallel
go vet ./...
git diff --check origin/main...HEAD
git merge-tree --write-tree origin/main HEAD
```
