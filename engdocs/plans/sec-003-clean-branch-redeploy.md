# SEC-003 Clean Branch Redeploy

Owner: `gascity/pm`
Created: 2026-06-09
Source beads: `ga-x2i1lv`, `ga-mrpqg2`

## Goal

Recover the SEC-003 deploy by replacing the contaminated release branch with a
clean origin/main-based branch that contains only the reviewed
`isPathInSafeBoundary` home-directory fix.

## Context

Reviewer passed commit `c54c7bfa5` on `fix/sec-003-home-dir-resolution`.
The deploy gate failed because that branch was not a valid single-bead release
unit: it also carried unrelated commit `f288253ad`, was stale/diverged from
`origin/main`, had pre-existing untracked `.beads/formulas` files, and the
recorded gate exited in `cmd/bd` before opening a PR.

This is not a new architecture or UX decision. The PM action is to split the
cleanup from the deploy retry so deployer only gates a clean, reviewed release
candidate.

## Work Packages

| Bead | Route | Label | Acceptance focus |
| --- | --- | --- | --- |
| `ga-x2i1lv.1` | `gascity/builder` | `ready-to-build` | Prepare a fresh origin/main-based branch containing only the reviewed SEC-003 change from `c54c7bfa5`; record branch/head/base and keep the worktree clean. |
| `ga-x2i1lv.2` | `gascity/deployer` | `needs-deploy` | Run the standard deploy gate on the builder-provided clean branch; open a PR and route merge-request to mayor/mpr only on PASS. |

## Dependency Graph

`ga-x2i1lv.1` -> `ga-x2i1lv.2`

The deploy retry waits for the clean branch because the previous gate failure
was caused by release-unit contamination, not by a missing product requirement.

## Acceptance Notes

The builder handoff must prove the candidate is clean:

- Based on current `origin/main`.
- No unrelated `f288253ad` commit.
- No release-gate FAIL artifact or `.beads/formulas` dirt.
- Diff limited to the reviewed SEC-003 behavior in
  `internal/beads/context.go`.

The deployer handoff must record gate evidence and PR URL on PASS, or exact
failed criteria and artifact path on FAIL. Merge authority remains
operator, mayor, or mpr only.
