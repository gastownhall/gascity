# Plan: Dolt local-only lock-in execution (`ga-o8pmyw`)

> Owner: `gascity/pm` - Created: 2026-05-28
> Source: architecture `ga-kom0cp`; designer handoff `ga-o8pmyw`

## Goal

Finish the live gascity Dolt local-only lock-in now that the two guard
beads have shipped:

- `ga-vjefud.1`: `bd init` remote wiring guard
- `ga-acrtc9.1`: CLI remote sync guard

The remaining user-visible outcome is that the gascity Beads store has
`dolt.local-only:true`, the existing SQL `origin` remote is removed once,
and the removal survives a reinit/restart check.

## Critical corrections from design

- Use Dolt SQL port `28231`. The earlier `28232` value is incorrect for
  the live gascity server.
- `backup_export` is absent in the gascity database. The expected
  `dolt_remotes` result after removal is an empty table, not "only
  backup_export".
- Do not run `bd dolt push`; gascity Beads is intentionally local-only.

## Work breakdown

| Bead | Title | Routes to | Gate |
|------|-------|-----------|------|
| `ga-o8pmyw.1` | Add dolt.local-only to gascity Beads config | builder | ready-to-build |
| `ga-o8pmyw.2` | Remove gascity origin from live Dolt remotes | builder | ready-to-build |
| `ga-o8pmyw.3` | Verify gascity Dolt remotes stay empty after reinit | builder | ready-to-build |

All children route to builder because the designer handoff is complete
and this is operator execution work, not new UX design.

## Dependency graph

```text
ga-vjefud.1 ----\
                 > ga-o8pmyw -> ga-o8pmyw.1 -> ga-o8pmyw.2 -> ga-o8pmyw.3
ga-acrtc9.1 ----/
```

## Acceptance summary

1. `.beads/config.yaml` contains `dolt.local-only: true` while preserving
   existing local-only safety settings.
2. The live SQL removal runs once against `gascity` on port `28231`.
3. `origin` is removed from `dolt_remotes`.
4. `dolt_remotes` is empty after removal.
5. After a `bd init` re-run or equivalent city/rig reattach, `origin`
   does not reappear.
6. If `origin` reappears, builder files a follow-up bug instead of
   repeating the SQL removal loop.

## Out of scope

- Code changes to the local-only guards.
- Removing or modifying any future `backup_export` remote.
- Pushing the gascity Beads Dolt store.
