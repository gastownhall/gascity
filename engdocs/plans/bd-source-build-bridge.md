# Plan: bd source-build bridge (`ga-peo3rm`)

> Owner: `gascity/pm` - Created: 2026-05-28
> Source: architecture `ga-om4nvp`; design handoff `ga-peo3rm`

## Goal

Install a temporary `bd` binary from the local beads source tree so the
live gascity rig immediately gets the unreleased store-open
`dolt.local-only` guard. This is a bridge only; the formal release path in
`ga-vw1yb2` replaces it with the `v1.0.5` release binary.

The user-visible result is that normal `bd` activity no longer recreates
the SQL `origin` remote while gascity is configured with
`dolt.local-only:true`.

Tracker import no-op: only the local `actual` skill is materialized in
this worktree; no `tracker-to-beads` or sibling tracker skill is present.

## Work breakdown

| Bead | Title | Routes to | Gate |
|------|-------|-----------|------|
| `ga-peo3rm.1` | Build the temporary bd bridge from local beads HEAD | builder | ready-to-build |
| `ga-peo3rm.2` | Verify the bridge bd binary is selected on PATH | builder | ready-to-build |
| `ga-peo3rm.3` | Confirm gascity local-only remote sync is disabled under the bridge | builder | ready-to-build |

## Dependency graph

```text
ga-peo3rm.1 -> ga-peo3rm.2 -> ga-peo3rm.3
```

`ga-vw1yb2.4` also depends on `ga-peo3rm.3` because the formal release
reinstall is the step that closes this temporary bridge.

## Acceptance summary

1. `/home/jaword/projects/beads` contains commit `06c631d25` or a
   descendant with the store-open local-only fix.
2. `GOBIN=/home/jaword/.local/bin go install ./cmd/bd/...` succeeds.
3. `/home/jaword/.local/bin/bd` has a fresh timestamp after install.
4. `which bd` resolves to the bridge binary, or another path proven to
   be the same freshly installed binary.
5. `bd version` reports a post-`v1.0.4` build, not exact `v1.0.4`.
6. `bd dolt status` includes
   `Remote sync: disabled (dolt.local-only=true)`.
7. Failure at any verification step stops the bridge and is mailed to
   mayor with command output.

## Constraints

- Do not write gascity implementation code for this bridge.
- Do not run `bd dolt push`, `bd dolt pull`, or `gc dolt sync`.
- Do not repeat live SQL remote removal as a workaround if verification
  fails; the fix belongs in beads.
