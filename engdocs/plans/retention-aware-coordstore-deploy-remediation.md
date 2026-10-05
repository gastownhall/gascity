# Retention-Aware Coordstore Deploy Remediation

Root bead: `ga-linjt0`

Source bug: `ga-oxtlqn`

Reviewed commit: `07a9fd70a` on `builder/ga-oxtlqn-retention-aware-import`

Review bead: `ga-sjaifi`

## Problem

The deploy gate rejected `ga-linjt0` before PR creation. The reviewed
commit is focused and passed review, but the branch is not a valid
single-bead release unit.

Gate failures:

- Tests were not run because preliminary release gates failed.
- The branch does not merge cleanly with `origin/main`.
- The branch contains a broad SQLite/HQStore coordstore stack, internal
  planning docs, and prior release-gate artifacts.

Conflict evidence from the deploy gate:

- `cmd/gc/main.go`
- `go.sum`
- `internal/beads/exec/exec_test.go`

Contaminated branch evidence:

- `origin/main...07a9fd70a` includes 62 files and 8319 insertions.
- `docs/coordination-store/**` is present in the deploy branch diff.
- `release-gates/**` artifacts are present in the deploy branch diff.

## Decision Needed

PM should not decide the release topology. Architecture must first decide
whether this fix can ship independently from `origin/main`, must become an
explicit rollup, or must wait for the underlying coordstore/SQLite stack.

## 2026-06-04 Update

The first architecture decision, `ga-linjt0.1`, chose the safe path of waiting
for PR #2738 to land before builder produced a clean single-commit branch. That
path is no longer sufficient: PM verified PR #2738 is now closed, still draft,
dirty, and unmerged (`mergedAt=null`, head
`ed51accf47ac7b92dd87c164fc0b4f7bdc1e9c4b`).

Validator recorded `ga-linjt0.3` as fail/blocked because no remediated builder
branch exists and the previously approved base did not land. PM created
`ga-linjt0.4` to re-open the release-path decision against current reality.

## Work Packages

### `ga-linjt0.1` - Decide Release Path

Route: `gascity/architect`

Label: `needs-architecture`

Acceptance:

- State whether the retention-aware import/shadow fix is independently
  shippable from `origin/main`, must become an explicit rollup, or must wait
  for the underlying coordstore/SQLite stack such as PR #2738.
- Identify the allowed base branch or PR stack, and the commits/paths that
  downstream work may include or must exclude.
- Define branch cleanliness gates for builder/validator, including merge-tree
  base, scope boundaries, and required test evidence.
- If no safe release path exists now, leave a blocked recommendation and
  notify PM/mayor with the reason.

Status: Closed. Decision was "must wait for PR #2738 to land."

### `ga-linjt0.4` - Re-decide Release Path After PR #2738 Closed Unmerged

Route: `gascity/architect`

Label: `needs-architecture`

Acceptance:

- Decide whether retention-aware coordstore import/shadow can still ship via a
  reopened #2738, a replacement coordstore substrate PR/base, an explicit
  rollup, or should be closed/deferred as superseded.
- Identify the exact approved base branch, commit, or PR stack downstream
  builder work may use, including dependency changes needed for `ga-linjt0.2`
  and `ga-linjt0.3`.
- Define allowed paths/commits, excluded paths/commits, merge-tree base, scope
  gates, and required test evidence for builder and validator.
- If no safe path exists now, record that explicitly and state which beads
  should be closed, deferred, or re-scoped, plus who must be notified.

### `ga-linjt0.2` - Produce Deployable Branch

Route: `gascity/builder`

Label: `ready-to-build`

Blocked by: `ga-linjt0.1`, `ga-linjt0.4`, and `ga-snab2n.3`

Acceptance:

- Follow the release topology and base selected by `ga-linjt0.1`.
- Produce a deployable branch/commit for `ga-oxtlqn` that contains only the
  approved retention-aware import/shadow scope: retention filtering,
  sweeper-race elimination, shadow `--json` contract, and batched dependency
  loading.
- Exclude `docs/coordination-store/**`, `release-gates/**`, unrelated
  HQStore/SQLite draft artifacts, and prior release-gate commits unless the
  architect explicitly approves a rollup.
- The branch merges cleanly with the approved base; specifically, there are no
  unresolved conflicts in `cmd/gc/main.go`, `go.sum`, or
  `internal/beads/exec/exec_test.go`.
- Run and record builder evidence for focused coordstore tests, shadow
  JSON/schema coverage, `go vet ./...`, and the repo test shard required by
  the architecture decision.
- Update the bead with branch name, commit SHA, diffstat summary, commands
  run, and any review/deploy handoff created.

### `ga-linjt0.3` - Validate Deploy Gate Readiness

Route: `gascity/validator`

Label: `needs-tests`

Blocked by: `ga-linjt0.2`

Acceptance:

- Use the builder child bead branch/commit and the architect-approved base.
- Confirm the release-scope gate: diff against base contains only approved
  paths/commits and excludes internal planning docs/release-gate artifacts
  unless explicitly approved as a rollup.
- Confirm merge-tree is clean against the approved base.
- Run or verify the retention-aware coordstore import/shadow tests, shadow
  `--json` contract/schema coverage, go vet, and any build/smoke gate required
  for deploy readiness.
- Record a PASS/FAIL with exact command evidence. On PASS, state the next
  handoff target for review/deploy; on FAIL, list the specific blocker and
  child remediation needed.

## Dependency Graph

Original graph: `ga-linjt0.1` -> `ga-linjt0.2` -> `ga-linjt0.3`

Current graph: `ga-linjt0.4` -> `ga-linjt0.2` -> `ga-linjt0.3`

## Handoff Notes

The original deploy bead should be closed as decomposed once all three child
beads are slung to their target agents. The deployer should not retry
`builder/ga-oxtlqn-retention-aware-import` directly; retry only after the
architecture decision and a remediated branch exist.

After the 2026-06-04 update, builder should not produce a replacement branch
until `ga-linjt0.4` closes with a current approved base or a close/defer
recommendation.
