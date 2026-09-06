# Plan: Split doctor backlog-depth Ready-error deploy target (`ga-5gd6pa`)

> Owner: `gascity/pm` - Created: 2026-06-05
> Source: deployer fail-back `ga-5gd6pa`; source review `ga-nw0pck`; spec `ga-j5n5xr`

## Goal

Produce a deployable, single-feature release target for
`TestBacklogDepthCheckReadyErrorIsGraceful`.

The failed deploy candidate was branch
`fix/ga-671hz5-doctor-backlog-depth-dep-check` / PR #3133 at commit
`1d439f97a`. The intended doctor backlog-depth Ready-error coverage passed
review, but deployer rejected the release unit because the reviewer-visible
diff also bundled unrelated order-dispatch/config/schema changes, macOS
`icu4c` test-script changes, and prior release-gate artifacts.

## Work Breakdown

| Bead | Title | Routes to | Gate |
|------|-------|-----------|------|
| `ga-5gd6pa.1` | Isolate doctor backlog-depth Ready-error coverage branch | builder | ready-to-build |
| `ga-5gd6pa.2` | Deploy isolated doctor backlog-depth Ready-error coverage | deployer | needs-deploy |

## Dependency Graph

```text
ga-5gd6pa.1 -> ga-5gd6pa.2
```

The deploy re-check waits for builder to record the isolated branch, commit,
and PR evidence.

## Acceptance Summary

1. The builder produces or cleans a branch/PR whose reviewer-visible diff is
   limited to the doctor backlog-depth Ready-error test coverage required by
   `ga-j5n5xr`.
2. The deploy candidate excludes unrelated order-dispatch changes,
   config/schema changes, macOS `icu4c` test-script changes, prior
   release-gate artifacts, and internal planning docs.
3. The builder records the final branch, commit SHA, PR URL if available, diff
   scope summary, and targeted verification evidence.
4. The deployer runs the standard deploy gate only against the isolated target.
5. On deploy PASS, the deployer routes merge authority to mayor/mpr and does
   not merge to main directly.

## Out Of Scope

- Shipping unrelated PR #3133 changes as part of this deploy.
- Changing product behavior beyond the already-reviewed doctor test coverage.
- New UI, design, or architecture work.
