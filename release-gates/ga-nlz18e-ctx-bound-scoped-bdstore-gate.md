# Release gate: ga-nlz18e ctx-bound scoped BdStore

Date: 2026-07-04
Result: FAIL

## Candidate

- Deploy bead: ga-nlz18e
- Source implementation bead: ga-cdmx6x
- Review bead: ga-bytd3q
- Reviewed commit: 5e8459b3893fdc2e18f127c8200c98fc1fac1cf2 on gc-builder-3-dad840a7d698
- Clean release branch tested: release/ga-nlz18e-ctx-bound-scoped-bdstore
- Base: origin/main at d82074594d7594eea890e5300d7936540f30bd9e
- Tested commit: 4e45acbd4 (clean cherry-pick of 5e8459b38)

The reviewed builder branch was stacked on deploy/ga-oz3ow5.1-graphonlyready-clean,
which is not in origin/main and carries a separate graph-only readiness feature.
For the single-bead gate, I tested the reviewed status patch by itself on current
origin/main. The cherry-pick applied without conflicts.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | ga-bytd3q is closed with `REVIEW VERDICT: PASS` for commit 5e8459b3893fdc2e18f127c8200c98fc1fac1cf2. |
| 2 | Acceptance criteria met | FAIL | Cannot verify acceptance on the final clean branch because the branch does not compile under the fast unit gate. |
| 3 | Tests pass | FAIL | `TMPDIR=/var/tmp/gc-nlz18e-test make test-fast-parallel` failed in `unit-core`: `internal/api/store_health_test.go:171:9: not enough arguments in call to s.computeStoreHealth; have (); want ("context".Context)`. All six `cmd/gc` fast shards completed ok after the core failure. |
| 4 | No high-severity review findings open | PASS | Reviewer notes report no blockers and only one non-blocking security observation; unresolved HIGH finding count is 0. |
| 5 | Final branch is clean | PASS | Worktree was clean after the clean cherry-pick and before writing this gate file. |
| 6 | Branch diverges cleanly from main | PASS | Clean branch was cut from origin/main and the reviewed commit cherry-picked with no merge conflicts. |
| 7 | Single feature theme | PASS | The clean branch contains only the status/API scoped BdStore cancellation change, not the unrelated graph-only readiness parent stack. |

## Failure diagnosis

The isolated status patch needs a rebase/update against current origin/main.
`computeStoreHealth` now requires a `context.Context`, but
`TestComputeStoreHealthUsesDoltlitePathFromMetadata` still calls it with no
argument after the clean cherry-pick. This is a technical gate failure; no PR was
opened.
