# Plan: Reviewer Scaffold Residue Cleanup (`ga-odlifn`)

> Owner: `gascity/pm` | Created: 2026-07-01
> Source: reviewer bug report `ga-odlifn`

## Why This Work Exists

`ga-odlifn` reports 16 untracked bead-slug directories directly under the
shared `gascity/reviewer` worktree. The directories match the scaffold leak
signature from `ga-ajw1no`: no `.git`, no registered git worktree, and only
generated agent session scaffold/runtime files.

The root-cause fix from `ga-ajw1no.2` has been reviewed, but deploy work is
still tracked separately by `ga-m9rkmi`. Until that fix ships, cleanup can
remove known residue but cannot prove the leak class is gone.

## Goal

Remove verified scaffold-only residue from the reviewer worktree, check sibling
Gas City worktrees for the same leak pattern, and independently verify cleanup
evidence plus recurrence risk.

## Work Breakdown

| Bead | Title | Priority | Routes to | Gate |
| --- | --- | --- | --- | --- |
| `ga-odlifn.1` | As an operator, I can remove scaffold-only residue from the reviewer worktree | P2 | builder | ready-to-build |
| `ga-odlifn.2` | As an operator, I can audit sibling Gas City worktrees for scaffold-only residue | P2 | builder | ready-to-build |
| `ga-odlifn.3` | As a maintainer, I can verify scaffold residue cleanup and recurrence risk | P2 | validator | needs-tests |

## Dependency Graph

```text
ga-odlifn.1 (reviewer worktree cleanup)
  -> ga-odlifn.2 (sibling worktree audit/sweep)
  -> ga-odlifn.3 (independent verification)

ga-m9rkmi (deploy reviewed root-cause fix)
  -> ga-odlifn.3 (recurrence-risk verification)
```

The broad audit waits for the targeted reviewer cleanup so the operator has a
fresh evidence standard. The validator waits for both cleanup beads and the
existing deploy bead before checking whether recurrence risk is still open.

## Acceptance Rollup

1. Cleanup deletes only directories proven to be scaffold-only residue: no
   `.git`, not in `git worktree list`, and no non-scaffold work product.
2. Reviewer cleanup records before/after inventory, exact removed paths,
   status/hash evidence, and any delta from the 16 paths reported in
   `ga-odlifn`.
3. Sibling worktree audit classifies each inspected worktree as clean, cleaned,
   or ambiguous with follow-up filed.
4. Ambiguous directories are left in place and tracked rather than deleted.
5. Independent validation confirms cleanup evidence, reruns the focused
   residue-sensitive check (`TestDocDirCoverage` or current equivalent), and
   records the current status of `ga-m9rkmi`.
6. If the root-cause fix has not shipped when validation runs, the validator
   files a follow-up recurrence-check bead instead of claiming the leak class is
   fully closed.

## Risks And Notes

- The existing deploy bead `ga-m9rkmi` is outside this cleanup plan but blocks
  recurrence confidence.
- Cleanup must be evidence-gated. Deleting a real task worktree, registered git
  worktree, or ambiguous directory is out of scope.
- The plan intentionally avoids new architecture decisions. If the sibling
  audit finds a broader workdir-isolation gap, file a `needs-architecture`
  follow-up routed to `gascity/architect`.

## Out Of Scope

- Re-implementing the `ga-ajw1no.2` root-cause fix.
- Merging or deploying the fix tracked by `ga-m9rkmi`.
- Writing new cleanup automation before the one-time residue sweep is verified.
