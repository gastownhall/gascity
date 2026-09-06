# Plan: Durable `dolt.local-only` remote suppression (`ga-kom0cp` family)

> Owner: `gascity/pm` · Created: 2026-05-28
> Source: architecture `ga-kom0cp`; design handoff `ga-acrtc9`, `ga-vjefud`

## Why this work exists

`dolt.local-only:true` is intended to keep a Gas City Beads store local
and prevent accidental Dolt remote propagation. The first pass covered
push/pull/remote-add behavior, but two origin re-creation paths remain:

- every `bd` command open can call `syncCLIRemotesToSQL`, re-registering
  CLI remotes into SQL `dolt_remotes`;
- `bd init` can call `configureInitDoltRemote`, wiring git `origin` as a
  Dolt remote during city or rig reinitialization.

Until both paths are guarded, the one-time origin removal in `ga-o8pmyw`
will not be durable.

## Goal

When `dolt.local-only:true` is set, neither ordinary `bd` command opens
nor `bd init` re-create `origin` in Dolt SQL remotes. Existing non-local
behavior remains unchanged, and default CLI output stays quiet.

## Work breakdown

| Bead | Title | Priority | Routes to | Gate |
|------|-------|----------|-----------|------|
| `ga-acrtc9.1` | Implement dolt.local-only guard for CLI remote sync | P2 | builder | ready-to-build |
| `ga-vjefud.1` | Implement dolt.local-only guard for bd init remote wiring | P2 | builder | ready-to-build |

Both source beads have completed architecture and design handoff. No UX
design hop is needed.

## Dependency graph

```text
ga-acrtc9.1 ─┐
             ├──> ga-o8pmyw (set local-only config and remove origin once)
ga-vjefud.1 ─┘
```

The original PM/design source beads (`ga-acrtc9`, `ga-vjefud`) remain as
parent context. The downstream removal bead now also depends directly on
the build children so it cannot run before the guards ship.

## Routing rationale

Both child beads are routed to **builder** with `ready-to-build` because
the designer handoff already resolved the UX behavior: both skips are
silent by default, and the `bd init` guard must happen at the call sites
rather than inside `shouldWireInitRemote`.

## Acceptance criteria

1. With `dolt.local-only:true`, store opens do not re-register CLI remotes
   into SQL `dolt_remotes`.
2. With `dolt.local-only:true`, `bd init` and `bd init --reinit-local` do
   not wire git `origin` as a Dolt remote.
3. With `dolt.local-only` unset or false, current remote-sync and
   remote-wiring behavior is preserved.
4. `sync.remote` still persists when the user provides `--remote` under
   local-only mode.
5. Agent render options do not advertise remote instructions under
   local-only mode.
6. The default CLI path stays quiet: no new skip warning and no false
   success line for local-only remote wiring.
7. Regression coverage proves the local-only and default-mode cases for
   both guard paths.
8. `go test ./...` and `go vet ./...` pass before builder handoff closes.

## Risks and unknowns

- The `bd init` predicate is shared by persistence and display paths.
  Builder must preserve the designer constraint: guard the actions at
  the call sites, not the `shouldWireInitRemote` predicate.
- `backup_export` must survive all changes. This work suppresses CLI to
  SQL remote perpetuation; it must not remove SQL-only backup remotes.
- `ga-o8pmyw` should remain blocked until both child build beads close.

## Out of scope

- Running the one-time SQL removal for gascity; that remains `ga-o8pmyw`.
- Removing `backup_export`.
- Adding new user-facing local-only status output beyond the existing
  config/help surface.
