# Plan: Dolt origin reappearance root cause (`ga-673qo6`)

> Owner: `gascity/pm` - Created: 2026-05-28
> Source: builder follow-up `ga-673qo6`; closeout source `ga-o8pmyw.3`

## Goal

Find and fix why the live gascity Beads Dolt `origin` remote reappeared
after the local-only configuration and one-time SQL removal flow had
already run.

The desired user-visible outcome is durable local-only behavior: after
`origin` is removed once, later `bd` or `gc` activity must not re-create
it in SQL `dolt_remotes`.

## Context

- `ga-o8pmyw.2` removed `origin` once from the live gascity Dolt SQL
  server on port `28231`.
- `ga-o8pmyw.3` found `origin` present again during closeout
  verification and correctly filed this follow-up instead of repeating
  the SQL removal.
- The observed row was `origin` pointing at
  `git+https://github.com/gastownhall/gascity.git`.
- `backup_export` was absent in this database. An empty `dolt_remotes`
  table remains the expected gascity result after cleanup.
- gascity Beads is local-only. Do not run `bd dolt push`.

Tracker import no-op: this session only had the local `actual` skill
materialized under `.claude/skills`; no `tracker-to-beads` or sibling
tracker skill was present.

## Work breakdown

| Bead | Title | Routes to | Gate |
|------|-------|-----------|------|
| `ga-673qo6.1` | Write regression coverage for local-only Dolt remote reappearance | validator | needs-tests |
| `ga-673qo6.2` | Fix the source that recreates gascity origin under local-only mode | builder | ready-to-build |
| `ga-673qo6.3` | Verify live gascity Dolt remotes stay durable after the fix | validator | needs-tests |

## Dependency graph

```text
ga-673qo6
  -> ga-673qo6.1
  -> ga-673qo6.2
  -> ga-673qo6.3
```

`ga-673qo6.2` depends on `ga-673qo6.1`. `ga-673qo6.3` depends on
`ga-673qo6.2`.

## Acceptance summary

1. A deterministic regression covers the reappearance failure before the
   fix closes.
2. Builder records the exact code or configuration path that re-created
   `origin`.
3. With `dolt.local-only:true`, `dolt.auto-push:false`, and
   `no-push:true`, later `bd` or `gc` activity does not add CLI
   `origin` to SQL `dolt_remotes`.
4. Default non-local behavior is preserved.
5. SQL-only remotes such as any future `backup_export` are preserved.
6. If the live gascity database still has `origin` after the fixed path
   is active, at most one controlled post-fix removal is performed and
   recorded.
7. Final live verification reads the current port from
   `/home/jaword/projects/gascity/.beads/dolt-server.port`, triggers the
   durability path that previously failed, and confirms `origin_count=0`.
8. `go test ./...` and `go vet ./...` pass before builder closure.

## Risks

- The failure may be a missed call path rather than a bad config value;
  builder must avoid gascity-specific branches and hardcoded role names.
- If root cause requires a broader product or architecture decision,
  builder should file a `needs-architecture` bead routed to
  `gascity/architect` before expanding the scope.
- Existing live `origin` state may need one final cleanup after the fix
  is active; this is allowed once and must be recorded, not repeated in a
  loop.

## Out of scope

- Re-running SQL removal before the root cause is fixed.
- Removing or modifying any `backup_export` remote.
- Pushing the gascity Beads Dolt store.
