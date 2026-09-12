# Pre-Deploy Scaffold Residue Cleanup

Date: 2026-07-04

## Goal

Remove old scaffold-only residue found by the post-PR #3859 scan without
touching real work products, registered git worktrees, or ambiguous designer
artifacts.

Source bead:

- `ga-odlifn.4.1` - Clean pre-deploy scaffold-only residue found by post-3859
  scan

Tracker import: no external tracker skill was installed for this rig, so no
tracker issues were imported.

## Context

The validator closed the post-PR #3859 recurrence risk after confirming the
running scaffolder had the shipped fix and no new post-deploy scaffold-only
residue was observed. The remaining work is cleanup debt: eleven generated
scaffold-only directories predate the fixed supervisor start, while two
designer directories were explicitly left ambiguous and must not be deleted by
this cleanup plan unless separately triaged.

## Work Packages

| Bead | Route | Purpose | Acceptance summary |
| --- | --- | --- | --- |
| `ga-odlifn.4.1.1` | `gascity/builder` | Re-verify the pre-deploy scaffold residue cleanup set. | Check every path listed by `ga-odlifn.4.1` for no `.git`, absence from `git worktree list --porcelain`, and scaffold-only contents; record before counts and exact verified, skipped, already-gone, and unsafe paths; perform no deletion. |
| `ga-odlifn.4.1.2` | `gascity/builder` | Delete only verified pre-deploy scaffold-only residue. | Delete only paths verified by `ga-odlifn.4.1.1`, use exact paths rather than broad globs, leave ambiguous designer paths and registered worktrees untouched, and record after counts plus exact removed paths. |
| `ga-odlifn.4.1.3` | `gascity/validator` | Verify cleanup evidence independently. | Rerun the residue scans, confirm removed paths are gone, confirm ambiguous designer paths remain untouched unless separately triaged, run the focused doc sync check or record the current equivalent, and file follow-ups for any remaining risk. |

## Dependency Order

1. `ga-odlifn.4.1.1` runs first to produce current inventory evidence.
2. `ga-odlifn.4.1.2` depends on `ga-odlifn.4.1.1` and may remove only paths
   verified by that inventory.
3. `ga-odlifn.4.1.3` depends on `ga-odlifn.4.1.2` and validates the final
   state.

## Risks

- Deleting ambiguous designer output would risk data loss, so ambiguous paths
  stay in place unless a separate triage bead proves they are safe to remove.
- Broad glob deletion is out of scope. Cleanup must operate on exact,
  evidence-backed paths.
- This plan does not reopen the recurrence risk. New post-deploy residue should
  be filed as a separate bug with fresh scan evidence.
