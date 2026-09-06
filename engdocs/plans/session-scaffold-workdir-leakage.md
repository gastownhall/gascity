# Plan: Session Scaffold Workdir Leakage (`ga-ajw1no`)

> Owner: `gascity/pm` | Created: 2026-07-01
> Source: builder bug report `ga-ajw1no`

## Why this work exists

`ga-ajw1no` reports 47 untracked bead-slug directories under the shared
`gascity/builder` worktree. Each directory appears to contain only agent
session scaffold content such as `.claude/skills`, `.codex/hooks.json`, and
`.gc/settings.json`, and none are registered git worktrees.

The suspected user impact is accumulating untracked scaffold residue in a
shared worktree whenever session provisioning runs from one cwd but targets a
different `gc.work_dir`. This creates operator noise and risks hiding real
worktree dirtiness.

## Goal

Ensure session scaffold materialization always targets the resolved session
workdir, never the spawning process cwd, then verify the fix and remove the
already-created scaffold-only residue.

## Work Breakdown

| Bead | Title | Priority | Routes to | Gate |
| --- | --- | --- | --- | --- |
| `ga-ajw1no.1` | As a maintainer, I can reproduce stray session scaffold leakage | P2 | validator | needs-tests |
| `ga-ajw1no.2` | As an agent operator, session scaffold staging always targets the session workdir | P2 | builder | ready-to-build |
| `ga-ajw1no.3` | As a maintainer, I can verify the scaffold leakage fix end to end | P2 | validator | needs-tests |
| `ga-ajw1no.4` | As an operator, I can remove pre-existing stray scaffold directories after verification | P3 | builder | ready-to-build |

## Dependency Graph

```text
ga-ajw1no.1 (regression coverage)
  -> ga-ajw1no.2 (path-isolation fix)
  -> ga-ajw1no.3 (post-fix verification)
  -> ga-ajw1no.4 (cleanup existing residue)
```

The first bead creates failing coverage before implementation. Cleanup waits
until verification confirms the root cause is fixed so the stray directories
are not immediately recreated.

## Acceptance Rollup

1. A regression test exercises scaffold staging from a cwd that differs from
   the target WorkDir and proves no scaffold files land in the spawner cwd.
2. Session startup/reconciliation honors resolved `WorkDir`/`gc.work_dir` for
   singleton and pooled sessions without hardcoded role names.
3. Provider-specific staging behavior stays consistent, and staging failures
   include source/destination context.
4. Verification records exact commands and outcomes, including whether the
   reported builder-worktree residue is pre-existing rather than newly created.
5. Cleanup deletes only verified scaffold-only directories that are not git
   worktrees and records before/after evidence.

## Risks And Notes

- If downstream investigation shows the current behavior is provider-specific,
  the validator or builder should record the affected provider boundary in the
  relevant bead notes.
- If fixing the path contract requires changing session/workdir architecture,
  file a `needs-architecture` follow-up routed to `gascity/architect` before
  implementing that broader change.
- Existing directories under the shared builder worktree must be treated as
  residue until proven scaffold-only. Ambiguous content is not cleanup scope.

## Out Of Scope

- Deleting anything from another agent worktree before the fix is verified.
- Changing role behavior or adding role-specific safeguards.
- Revisiting the workdir isolation model beyond the scaffold leakage contract.

## Validation Gates

- New regression coverage passes.
- Focused tests for changed session/workdir provisioning packages pass.
- `go test ./...` and `go vet ./...` are green before final ship.
- The cleanup bead records a clean post-cleanup inventory for the reported
  shared builder worktree.
