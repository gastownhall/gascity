# Plan: bd actor identity-set resolution

Root: `ga-3t9lw7`  
Generated: 2026-09-11T19:16:03Z

## Outcome

Allow a live Gas City session to mutate a bead it owns when its managed alias
changes after process launch. `gc bd` will use the bead's exact recorded
assignee only when that value belongs to the calling session's known identity
set. Ownership checks remain unchanged for every other caller.

## Build increment

| Bead | Scope | Route |
| --- | --- | --- |
| `ga-cj0vli` | Add identity-set-aware actor resolution and regression coverage for `update --claim`, `close`, `reopen`, `delete`, and `heartbeat` | `gascity/builder` via `ready-to-build` |

This is one cohesive increment in `cmd/gc/cmd_bd.go`. Splitting it would make
multiple builders change the same dispatch seam and test matrix without an
independent deliverable boundary.

## Dependency graph

The build increment has no blocking dependencies. It consumes the completed
design in `ga-uvd6zy` and the architecture decision in `ga-joqi8c` as
references.

```text
ga-joqi8c (architecture, closed)
  -> ga-uvd6zy (implementation design, closed)
       -> ga-3t9lw7 (PM plan)
            -> ga-cj0vli (build increment, ready)
                 -> ga-mmscj2 (rollup ship, blocked)
```

`bd dep cycles` reported no dependency cycles on 2026-09-11.

## Acceptance summary

- Echo the target bead's recorded assignee only for an ambient-actor mismatch
  that resolves to another identity of the same calling session.
- Preserve explicit actor overrides, bd's unset-actor fallback, and refusals for
  beads owned by another session.
- Reuse the existing mutation guard read; add no persistent state, config, or
  role-specific behavior.
- Commit regression coverage for the positive identity-flip case, all negative
  and no-op cases, and all five recognized mutation verbs.

## Ship rail

`ga-mmscj2` records the architecture-rule rollup. It is blocked by
`ga-cj0vli` and intentionally has no `needs-deploy` label until its
`CHERRY_PICKS` placeholder is replaced with the reviewed implementation SHA.
