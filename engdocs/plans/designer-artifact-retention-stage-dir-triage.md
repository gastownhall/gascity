# Plan: Designer Artifact Retention And Stage Dir Triage (`ga-9fvvqd`)

> Owner: `gascity/pm` | Created: 2026-07-01
> Source: builder triage bead `ga-9fvvqd`

## Why This Work Exists

`ga-9fvvqd` was filed after the sibling Gas City worktree audit for
scaffold-only residue. The audit found two classes of residue that were outside
the evidence gate for immediate cleanup:

1. Twenty-eight untracked `ga-*` directories under `gascity/designer/` that
   appear to contain real design deliverables rather than scaffold-only files.
2. Seventeen hidden `.gascity-worktree-stage.*` directories under the Gas City
   worktree root, including at least one non-empty directory and one sample with
   a nested Gemini provider staging path.

The immediate goal is not deletion. The goal is to preserve real work, make the
retention and staging lifecycle decisions explicit, then allow cleanup only
where evidence and policy support it.

## Goal

Inventory and protect existing designer deliverables, define the retention and
worktree-stage lifecycle contracts, execute only approved cleanup, and validate
that no real design work or ambiguous staging artifact was silently discarded.

## Work Breakdown

| Bead | Title | Priority | Routes to | Gate |
| --- | --- | --- | --- | --- |
| `ga-9fvvqd.1` | As a maintainer, I can inventory and recover designer worktree deliverables before cleanup | P3 | designer | needs-design |
| `ga-9fvvqd.2` | As a maintainer, I have a retention policy for designer scratch deliverables | P3 | architect | needs-architecture |
| `ga-9fvvqd.3` | As a maintainer, I know the lifecycle contract for gascity worktree staging directories | P3 | architect | needs-architecture |
| `ga-9fvvqd.4` | As an operator, I can apply approved retention decisions to current designer and stage-dir residue | P3 | builder | ready-to-build |
| `ga-9fvvqd.5` | As a maintainer, I can verify designer artifact preservation and stage-dir cleanup safety | P3 | validator | needs-tests |

## Inventory Result

Designer inventory `ga-9fvvqd.1` completed on 2026-07-01. The current
designer worktree contains 28 top-level `ga-*` artifact directories totaling
9.5 MB. The inventory found that prose design writeups are reliably duplicated
into bd, but rendered diagrams are not embedded in bd and are instead
path-referenced back into the designer worktree.

Current classification:

- 11 directories are eligible for cleanup because their prose is duplicated in
  bd and no diagram asset is at risk.
- 16 directories must be relocated or attached before cleanup because their
  rendered diagrams are currently sole-copy artifacts referenced only by
  filesystem path.
- `ga-extmsg-connectedclient/` is ambiguous and must not be deleted until an
  architect or maintainer verifies whether its orphaned critical design review
  was applied or attaches that review to the related external-messaging beads.

The retention decision in `ga-9fvvqd.2` should treat this as a systemic artifact
durability policy question, not a per-directory cleanup judgment. The same
policy should cover the related loose top-level designer files noted in the
inventory.

## Dependency Graph

```text
ga-9fvvqd.1 (designer artifact inventory/recovery)
  -> ga-9fvvqd.2 (designer artifact retention policy)

ga-9fvvqd.2 (designer artifact retention policy)
  -> ga-9fvvqd.4 (apply approved cleanup)

ga-9fvvqd.3 (stage-dir lifecycle contract)
  -> ga-9fvvqd.4 (apply approved cleanup)

ga-9fvvqd.4 (apply approved cleanup)
  -> ga-9fvvqd.5 (independent validation)
```

The designer inventory gates the retention policy because architecture needs to
know whether any directory is the only durable copy of design work. Builder
cleanup waits for both architecture decisions. Validator work waits until the
cleanup execution bead records before/after evidence.

## Acceptance Rollup

1. All 28 designer `ga-*` directories from `ga-9fvvqd` are inventoried and
   classified as preserve, relocate/attach, eligible for cleanup, or ambiguous.
2. The `ga-7n7vth...` Excalidraw activity is explicitly investigated before any
   cleanup can touch that directory.
3. Architecture documents a durable artifact destination and cleanup trigger for
   designer deliverables.
4. Architecture defines the lifecycle and cleanup evidence standard for
   `.gascity-worktree-stage.*` directories, including Gemini/provider staging
   residue.
5. Builder cleanup deletes only paths that satisfy the approved evidence
   standard and records exact before/after paths.
6. If stage-dir residue indicates a code bug, the builder either implements the
   explicitly scoped fix with tests or files a follow-up build bead if the fix is
   larger than this cleanup bead.
7. Validator independently confirms that real design work was preserved or
   attached and that ambiguous stage directories remain tracked.

## Risks And Notes

- The designer directories are explicitly not scaffold-only residue until proven
  otherwise. Treating them as cleanup targets before inventory is data loss risk.
- The inventory found that diagram artifacts are path-referenced rather than
  embedded in bd. Deleting the worktree copies before relocation or attachment
  would make those references dangling.
- `ga-extmsg-connectedclient/` contains an apparently orphaned critical design
  review for external-messaging work packages. It is the highest-priority
  ambiguous artifact and should remain protected until architecture verifies the
  C1/C2 findings or preserves the review somewhere durable.
- The `.gascity-worktree-stage.*` directories may be harmless failed staging
  residue or evidence of another provider-specific worktree leak. Architecture
  owns that distinction.
- The PM tracker-import step was a no-op because no `tracker-to-beads` skill is
  materialized in this worktree.

## Out Of Scope

- Deleting any designer deliverable before retention policy exists.
- Making architecture decisions inside builder cleanup.
- Routing this as a direct implementation task without designer inventory and
  stage-dir lifecycle guidance.
