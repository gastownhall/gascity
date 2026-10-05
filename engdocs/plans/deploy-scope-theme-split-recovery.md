# Deploy Scope Theme Split Recovery

Owner: `gascity/pm`
Created: 2026-07-24
Source beads: `ga-z7evh4`, `ga-nnjcuc`
Priority: P2

## Goal

Recover two deploy gate failures by replacing mixed-theme release candidates
with clean, single-theme work packages. The deployer rejected both roots before
opening PRs because each candidate bundled an independently shippable fix.

This plan does not change product scope. It separates already-reviewed work
into release units that builder, reviewer, and deployer can validate without
merging unrelated behavior.

Tracker import was checked during PM intake. No `tracker-to-beads` or sibling
tracker skill is installed in this worktree, so the import step was a no-op.

## Work Packages

| Bead | Route | Scope |
| --- | --- | --- |
| `ga-z7evh4.1` | `gascity/builder` | Repackage stranded routed-demand throttle/dedup from `builder/ga-hlvv1n` as a single-theme deploy candidate. |
| `ga-z7evh4.2` | `gascity/builder` | Release the independent `ga-4tjr99` docsync session-scaffold coverage fix separately, or close with evidence that it already landed. |
| `ga-nnjcuc.1` | `gascity/builder` | Repackage dead-assignee pool-wake fallback from `builder/ga-o3ko1j.4.3` as a single-theme deploy candidate. |
| `ga-nnjcuc.2` | `gascity/builder` | Release the independent `ga-glzz6h` push-ownership guard multi-level bead-id regex fix separately, or close with evidence that it already landed. |

## Split Boundaries

`ga-z7evh4.1` must include only the stranded routed-demand throttle/dedup work
for `ga-bhabhv` / `ga-o3ko1j.4.4` and mechanically coupled generated outputs.
It must exclude `ga-4tjr99` unless that work already exists on `origin/main`.

`ga-z7evh4.2` owns the separate `TestDocDirCoverage` session-scaffold skip
release path. It must exclude stranded routed-demand policy and throttle work.

`ga-nnjcuc.1` must include only the FR-1/FR-2/FR-3 dead-assignee demand fallback
from `ga-o3ko1j.4.3`. It must exclude `ga-glzz6h` unless that work already
exists on `origin/main`.

`ga-nnjcuc.2` owns the separate `scripts/push-ownership-guard.sh` branch-id
regex fix and its regression coverage. It must exclude dead-assignee demand
fallback behavior.

## Dependencies

Each split bead depends on its already-completed source implementation bead for
traceability:

- `ga-z7evh4.1` depends on `ga-bhabhv`
- `ga-z7evh4.2` depends on `ga-4tjr99`
- `ga-nnjcuc.1` depends on `ga-o3ko1j.4.3`
- `ga-nnjcuc.2` depends on `ga-glzz6h`

The four split beads are otherwise independent. The builder can work them in
parallel, and each resulting review/deploy handoff should stand on its own
branch, changed-file list, and gate evidence.

## PM Notes

PM intake found the long-lived PM worktree on the wrong branch identity:
`gc-pm-1-b8e04a1ccdb4` instead of `gc-pm-b8e04a1ccdb4`. The expected branch was
an ancestor of the current branch, so PM advanced the expected branch to the
current HEAD, switched to it, and cleared `.worktree-stale`. A freshen rebase was
not attempted over pre-existing tracked edits in `AGENTS.md` and
`schemas/convoy/target/result.schema.json`; those paths were left untouched.
