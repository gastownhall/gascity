# Plan: beads v1.0.5 release and gascity pin update (`ga-vw1yb2`)

> Owner: `gascity/pm` - Created: 2026-05-28
> Source: architecture `ga-om4nvp`; design handoff `ga-vw1yb2`

## Goal

Merge the unreleased beads local-only fixes, publish `bd` `v1.0.5`, and
update gascity to consume the release through the existing pinned archive
installer. This replaces the temporary source-build bridge from
`ga-peo3rm` with a formal release binary.

The user-visible result is durable local-only behavior in live gascity and
CI: `bd dolt status` reports remote sync disabled when
`dolt.local-only:true`, and gascity no longer depends on an untagged
source build.

Tracker import no-op: only the local `actual` skill is materialized in
this worktree; no `tracker-to-beads` or sibling tracker skill is present.

## Work breakdown

| Bead | Title | Routes to | Gate |
|------|-------|-----------|------|
| `ga-vw1yb2.1` | Merge beads local-only fix branches in guarded order | builder | ready-to-build |
| `ga-vw1yb2.2` | Cut and publish the beads v1.0.5 release artifacts | builder | ready-to-build |
| `ga-vw1yb2.3` | Update gascity BD_VERSION and v1.0.5 release pins | builder | ready-to-build |
| `ga-vw1yb2.4` | Reinstall live gascity bd from the v1.0.5 release and close the bridge | builder | ready-to-build |

## Dependency graph

```text
ga-vw1yb2.1 -> ga-vw1yb2.2 -> ga-vw1yb2.3 -> ga-vw1yb2.4
                                                ^
                                                |
                                        ga-peo3rm.3
```

The bridge verification dependency means the final reinstall explicitly
closes the temporary source-build path instead of leaving it implicit.

## Acceptance summary

1. Beads merges happen in the safe order:
   `cbaa555bf` first, `06c631d25` second, `75c5eff1b` last.
2. The `isDoltLocalOnly()` duplicate is resolved by keeping the shared
   helper in `dolt_local_only.go`; after the final merge only one helper
   definition remains.
3. `go test ./...` and `go vet ./...` pass in the beads repo after the
   merge set lands.
4. The `v1.0.5` tag is cut from merged beads main and release tarballs
   exist for linux/amd64, linux/arm64, darwin/amd64, and darwin/arm64.
5. SHA256 values for all four release tarballs are generated from the
   published artifacts and recorded before editing gascity pins.
6. gascity `deps.env` sets `BD_VERSION=v1.0.5`.
7. `.github/scripts/install-bd-archive.sh` includes matching `v1.0.5`
   SHA256 pins for all four platforms.
8. `.github/actions/setup-gascity-ubuntu/action.yml` is updated only if
   the beads `v1.0.5` `go.mod` floor is higher than `1.25.10`.
9. The gascity pin update lands as one commit or PR titled
   `deps: bump bd to v1.0.5, add SHA256 pins, conditionally bump go-version`.
10. Live gascity reinstall from the release reports exact `bd` `v1.0.5`
    and `bd dolt status` still includes
    `Remote sync: disabled (dolt.local-only=true)`.

## Constraints

- Do not update gascity pins before the `v1.0.5` release artifacts are
  published.
- Do not run `bd dolt push`, `bd dolt pull`, or `gc dolt sync`; gascity
  Beads is intentionally local-only.
- Do not split `deps.env`, install-script pins, and the conditional
  composite-action Go version check across unrelated gascity changes.
