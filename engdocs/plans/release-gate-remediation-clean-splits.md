# Release Gate Remediation Clean Splits

Owner: `gascity/pm`
Created: 2026-06-03
Source beads: `ga-7z0hku`, `ga-mtgtv7.2`, `ga-3u2e95.2`

## Goal

Recover three release-gate failures without reopening the same failed scopes.
The fixes are PM acceptance corrections only; no new architecture decision is
needed.

## Context

`ga-7z0hku` failed because the deploy branch for the modernc restart-init
adoption fix included unrelated modernc/order-dispatch work and conflicted
with current `origin/main`. The corrected package is a clean cherry-pick of
only the reviewed red/green commits from `ga-spy4rw`.

`ga-mtgtv7.2` failed because the SQLite split acceptance was too strict. The
source commit intentionally touches `cmd/gc/main.go` to document the
`sqlite-cgo` compatibility alias, and `go.sum` may retain a transitive
`github.com/mattn/go-sqlite3` checksum even when `go.mod` and direct imports
are clean.

`ga-3u2e95.2` failed because the worktree-reaper split whitelist omitted
generated API and config artifacts required by the typed event and
`DaemonConfig` changes.

## Work Packages

| Bead | Route | Label | Acceptance focus |
|------|-------|-------|------------------|
| `ga-7z0hku.1` | `gascity/deployer` | `needs-deploy` | Fresh branch from current `origin/main`; cherry-pick only `f7dff357b` and `c94a02794`; diff limited to `cmd/gc/beads_provider_lifecycle.go` and its test. |
| `ga-mtgtv7.2.1` | `gascity/deployer` | `needs-deploy` | Fresh SQLite split branch; allow `cmd/gc/main.go` compatibility comment and transitive `go.sum` checksum residue while requiring no direct mattn import or `go.mod` dependency. |
| `ga-3u2e95.2.1` | `gascity/deployer` | `needs-deploy` | Fresh worktree-reaper split branch; allow generated OpenAPI, generated API client, config reference, and city-schema artifacts. |

## Dependency Graph

Each remediation bead is a child of the failed PM item that produced it:

- `ga-7z0hku.1` parent: `ga-7z0hku`
- `ga-mtgtv7.2.1` parent: `ga-mtgtv7.2`
- `ga-3u2e95.2.1` parent: `ga-3u2e95.2`

There is no blocker edge between these remediation beads. Unit A and Unit B
remain independent per architecture decision `ga-4q6sgc.1`; deployer may work
them sequentially or in parallel depending on capacity.

## Handoff

All three remediation beads carry `needs-deploy`, `source:actual-pm`, and
`gc.routed_to=gascity/deployer`. Deployer must record PR URL and gate evidence
on pass, or exact failure evidence and route back to PM on fail.
