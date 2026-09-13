# Release Gate: Wake known pool identities with assigned work (`ga-wf6b3`)

Date: 2026-05-24
Branch: `builder/ga-6do4y.2-clean`
Source: `fork/builder/ga-6do4y.2-clean`
Feature commits:

- `a8072e913 test(pool): add wake known identity coverage`
- `3bff00217 fix(pool): wake known pool identities with assigned work`

`docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate uses
the release criteria from the deployer instructions.

## Scope

The final branch contains two feature commits above `origin/main`. The diff is
limited to pool desired-state recovery logic, wake-known-identity tests, and
trace type registration:

- `cmd/gc/pool_desired_state.go`
- `cmd/gc/pool_desired_state_wake_test.go`
- `cmd/gc/session_reconciler_trace_types.go`

## Gate Criteria

| # | Criterion | Status | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead notes contain `Review verdict: PASS` and clean-branch verification for `fork/builder/ga-6do4y.2-clean` at `a8072e913` and `3bff00217`. |
| 2 | Acceptance criteria met | PASS | The reconciler now emits a `wake-known-identity` request when in-progress work is assigned to a configured, non-suspended pool template and no live session owns that work. Duplicate work for the same template deduplicates to one request, unknown assignees do not wake, and live sessions continue through the existing resume tier. The new trace site is registered. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestComputePoolDesiredStates_WakeKnownIdentity|TestApplyNestedCaps_WakeKnownIdentity|TestComputePoolDesiredStates_LiveSessionContinuesAsResumeTier|TestTrace|TestResumeTier' -count=1` PASS. `go vet ./...` PASS. `make test` PASS from detached clean worktree `/home/jaword/tmp/gascity-ga-wf6b3-test-1779616606` with `TMPDIR=/tmp/gct-wf6b3`. |
| 4 | No high-severity review findings open | PASS | Reviewer listed only LOW/informational findings and concluded PASS; no unresolved HIGH findings are present in bead notes. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before adding this gate file. The release-gate commit is the only deployer change. |
| 6 | Branch diverges cleanly from main | PASS | `git diff --check origin/main...HEAD` PASS. `git merge-tree --write-tree HEAD origin/main` exited 0, indicating no merge conflicts with current `origin/main`. |

## Acceptance Evidence

- Wake-known-identity coverage verifies closed/missing live sessions can be
  woken when assigned work names the configured pool template.
- Unknown assignee coverage verifies unrelated work does not create a wake
  request.
- Deduplication coverage verifies multiple beads for the same template create
  one request.
- Resume-tier regression coverage verifies an existing live session keeps the
  existing resume behavior.
- Nested-cap coverage verifies wake-known-identity ranks before new session
  creation at the same bead priority.

## Result

PASS. This branch is ready for PR creation.
