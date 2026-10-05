# Plan: Clean deploy for adopted-rig types.custom registration (`ga-pftmco`)

> Owner: `gascity/pm` - Created: 2026-06-07
> Source: deployer fail-back `ga-pftmco`; reviewed fix `ga-bcip1f`

## Goal

Ship the reviewed `gc rig add --adopt` fix without bundling unrelated
status-degrade changes.

The reviewed target was commit
`255fae84175e8e287b53c3959b7667e8954f9aa3` on
`work/ga-stllj9-rebase`. Review passed, but deploy rejected the release unit
because the PR diff would include unrelated files:

- `internal/api/handler_status_test.go`
- `release-gates/fix-status-degrade-wall-bounds-gate.md`

The deployer also reported that local `work/ga-stllj9-rebase` is divergent
from `origin/work/ga-stllj9-rebase` and checked out in the builder worktree.

## Work Breakdown

| Bead | Title | Routes to | Gate |
|------|-------|-----------|------|
| `ga-pftmco.1` | Isolate clean adopt-only release branch for types.custom registration | builder | ready-to-build |
| `ga-pftmco.2` | Deploy clean adopt-only types.custom registration release | deployer | needs-deploy |

## Dependency Graph

```text
ga-pftmco.1 -> ga-pftmco.2
```

The deploy retry waits for builder to record the clean branch, commit SHA,
diff scope summary, and verification evidence.

## Acceptance Summary

1. Builder produces a fresh branch from current `origin/main`, or documents a
   current-main equivalent, containing only the reviewed adopt fix or an
   equivalent minimal transplant.
2. The release branch diff excludes `internal/api/handler_status_test.go` and
   `release-gates/fix-status-degrade-wall-bounds-gate.md` unless those files
   landed on `main` independently before deploy.
3. Builder records final branch, commit SHA, diff scope, and verification
   evidence on `ga-pftmco.1`.
4. Deployer runs the standard build, smoke, and diff-scope release gate only
   against the clean target recorded on `ga-pftmco.1`.
5. On deploy PASS, deployer opens the PR and routes merge authority to
   mayor/mpr. No rig agent merges directly to `main`.
6. On deploy FAIL, deployer records the exact gate artifact and routes back to
   PM.

## Out Of Scope

- Shipping unrelated status-degrade work in the adopt fix PR.
- Retrying the divergent local `work/ga-stllj9-rebase` branch unless it is
  cleaned first.
- New architecture, design, or product behavior changes.

## Tracker Import

No tracker-to-beads skill is installed in this PM worktree, so tracker import
is a no-op for this package.
