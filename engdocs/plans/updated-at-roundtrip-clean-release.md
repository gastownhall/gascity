# Plan: Clean updated_at round-trip release (`ga-t40ugi`)

> Owner: `gascity/pm` - Created: 2026-06-01
> Source: reviewer/deployer handoff `ga-t40ugi`; reviewed fix `ga-suon4b`

## Why this work exists

The updated_at round-trip fix passed review on the builder branch
`builder/ga-34q3ss-updated-at-roundtrip`, but deploy rejected that branch
as a release unit. The branch is 34 commits / 63 files ahead of
`origin/main`, includes internal coordination-store planning docs and a
wider coordstore/HQStore/SQLite stack, and conflicts with `origin/main` in
`cmd/gc/main.go`, `go.sum`, and `internal/beads/exec/exec_test.go`.

The reviewed behavior is narrower: preserve `updated_at` through the exec
store, BdStore, and coordstore import/shadow paths.

Tracker import no-op: only the local `actual` skill is materialized in this
worktree; no `tracker-to-beads` or sibling tracker skill is present.

## Goal

Ship only the reviewed updated_at round-trip fix through a clean release
branch, with fresh validation and deploy evidence. The failed broad branch is
context only and must not become the PR source.

## Work Breakdown

| Bead | Title | Routes to | Gate |
|------|-------|-----------|------|
| `ga-t40ugi.1` | Extract a clean updated_at round-trip release branch | builder | ready-to-build |
| `ga-t40ugi.2` | Prove the clean updated_at branch is release-ready | validator | needs-tests |
| `ga-t40ugi.3` | Ship the clean updated_at round-trip branch via PR/MPR | deployer | needs-deploy |

## Dependency Graph

```text
ga-t40ugi.1 -> ga-t40ugi.2 -> ga-t40ugi.3
```

The builder bead creates the clean branch. The validator bead proves scope,
merge cleanliness, and regression coverage on that branch. The deployer bead
uses only that validated branch for PR/MPR handoff.

## Acceptance Summary

1. The clean branch is based on `origin/main`.
2. The clean branch contains only `c92a0e7fd`, `b92b1bce6`, or equivalent
   updated_at round-trip changes.
3. The clean branch diff is limited to the exec store, BdStore, and
   coordstore import/shadow regression files.
4. `docs/coordination-store`, provider/config/sqlite rollout files,
   release-gate artifacts, and unrelated coordstore/HQStore stack changes
   are absent from the clean branch.
5. `git merge-tree --write-tree origin/main <branch>` exits clean.
6. Focused updated_at regressions pass on the clean branch.
7. Deploy opens a PR from the clean branch, records release-gate evidence,
   and routes the merge request to `mayor/mpr`.

## Risks

- The reviewed commits may need conflict resolution when replayed on current
  `origin/main`; any behavior change beyond the reviewed updated_at fix must
  go back through review.
- Reusing `origin/builder/ga-34q3ss-updated-at-roundtrip` would recreate the
  failed scope/theme and merge-cleanliness gate.
- The validator must record fresh evidence for the new branch rather than
  relying only on the prior reviewer pass.

## Out Of Scope

- Shipping the broader coordstore/HQStore/SQLite stack.
- Publishing the internal coordination-store planning docs.
- Reworking updated_at product behavior beyond the reviewed round-trip fix.
