# Release Gate: Shared nudge backstop engine

Date: 2026-07-19T19:28:13Z
Deployer: gascity/deployer
Deploy bead: `ga-4an0nc`
Source review bead: `ga-3zz4m2`
Parent feature bead: `ga-zogqc1.2.3`

## Candidate

- PR branch: `release/ga-4an0nc-backstop-engine`
- Reviewed source commit: `8016aa6f91bb8bcc79cacbc5972b92140e69f8bf`
- Base checked for mergeability: `origin/main` at `2b9a7d9263e2c3309a0e2997f5c5fd2adca849b9`
- Reviewer-visible delta:
  - `cmd/gc/idle_nudge.go`
  - `cmd/gc/idle_nudge_test.go`
  - `cmd/gc/nudge_backstop.go`

## Gate Inputs

- `docs/PROJECT_MANIFEST.md` is not present in this worktree; release criteria were evaluated against the deployer role's explicit seven-point release gate.
- `TESTING.md` was read before selecting the broad local runner; it names `make test-fast-parallel` as the default broad fast-unit baseline.
- The deploy instruction requested the standard deploy gate: build, smoke, and fast tests.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Source review bead `ga-3zz4m2` is closed with `REVIEW VERDICT: PASS` for commit `8016aa6f9`. The deploy bead records the same reviewed branch and commit. |
| 2 | Acceptance criteria met | PASS | The pool idle-claim pacing state machine is extracted into package-private `backstopPredicate`, `runNudgeBackstop`, and `decideBackstopAction` in `cmd/gc/nudge_backstop.go`. `poolClaimBackstop` in `cmd/gc/idle_nudge.go` preserves the pool-specific eligibility, metadata keys, nudge content, and the 90s grace / 3m backoff / 3-attempt cap. The runtime provider API is unchanged; the engine only calls existing `IsRunning` and `Nudge`. Focused tests cover nudge-after-grace, no nudge for working slots, give-up at cap, and non-pool skip. |
| 3 | Tests pass | PASS | `go build ./cmd/gc/...` passed. `go test ./cmd/gc -run '^TestNudgeStalledPoolClaims_(NudgesAfterGrace|NeverTouchesWorkingSlot|GivesUpAtCap|SkipsNonPool)$' -count=1` passed. `go vet ./...` passed. `gofmt -l cmd/gc/idle_nudge.go cmd/gc/idle_nudge_test.go cmd/gc/nudge_backstop.go` produced no output. `make test-fast-parallel` passed with all fast shards green and `All fast jobs passed`. |
| 4 | No high-severity review findings open | PASS | Source review notes list no HIGH findings or blockers. A targeted scan of the deploy and review bead text for `HIGH`, high-severity markers, and request-changes markers produced no open finding evidence. |
| 5 | Final branch is clean | PASS | Before writing this checklist, `git status --short --branch` reported only `## release/ga-4an0nc-backstop-engine`. This checklist is the only deployer change and is committed as the final branch tip before push. |
| 6 | Branch diverges cleanly from main | PASS | Checked first, then rechecked after `origin/main` advanced during deploy. `git merge-tree --write-tree origin/main HEAD` completed successfully with no conflicts against `2b9a7d926`, producing tree `4eb241663f4fb956a83292cc86a9f666b141c78e`. No bounded self-rebase was needed. |
| 7 | Single feature theme | PASS | The commit set contains one feature theme: generalizing pool idle-nudge pacing into a shared in-process backstop engine under `cmd/gc`. The diff is limited to `cmd/gc` idle-nudge code and tests; it does not bundle an unrelated user-facing behavior or package. |

## Validation

- `go build ./cmd/gc/...` - PASS.
- `go test ./cmd/gc -run '^TestNudgeStalledPoolClaims_(NudgesAfterGrace|NeverTouchesWorkingSlot|GivesUpAtCap|SkipsNonPool)$' -count=1` - PASS.
- `go vet ./...` - PASS.
- `gofmt -l cmd/gc/idle_nudge.go cmd/gc/idle_nudge_test.go cmd/gc/nudge_backstop.go` - PASS, no output.
- `make test-fast-parallel` - PASS, all fast jobs passed.

## Deploy Decision

PASS. Commit this gate checklist to `release/ga-4an0nc-backstop-engine`, push the branch, open a PR, record the PR URL on `ga-4an0nc`, close the deploy bead, and route a merge-request to mayor. Deployer does not merge.
