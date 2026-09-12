# SetMetadata Pack-Pin Scope Reset

Root deploy bead: `ga-jqvmde.3`
Parent release split: `ga-jqvmde`
Owner: `gascity/pm`
Created: 2026-06-14

## Goal

Recover the SetMetadata deploy retry after PR #3498 picked up an independent
Gastown pack-pin update and failed the SetMetadata-only scope gate.

The deploy candidate may include:

- `internal/beads/native_dolt_store_integration_test.go` for the SetMetadata
  event-recording integration test.
- `cmd/gc/city_discovery.go` for the already-approved hermetic gate
  remediation.

The deploy candidate must not include the pack-pin update from `592d55c1c`
unless those changes have independently landed on `origin/main`.

## Context

Previous PM work split the original mixed SetMetadata and worktree-cleanup
release into a clean SetMetadata path. Builder and validator completed the
first standard-gate remediation:

- `ga-jqvmde.3.1` fixed the ambient `/tmp/.gc` city-discovery test failure.
- `ga-jqvmde.3.2` verified `make test`, `go vet ./...`, and the focused
  SetMetadata integration test at
  `047860f002e5ca0b24e80332fa9705f22474cb94`.

Deployer then tested head `592d55c1ca1f2eb0e9d355596857ec4eebf25ed1` and
failed the gate. The new commit `592d55c1c` added a public Gastown pack-pin
update on top of the validated candidate:

- `docs/guides/gastown-config-recipes.md`
- `examples/gastown/city.toml`
- `examples/gastown/pack.toml`
- `examples/gastown/packs.lock`
- `go.mod`
- `go.sum`
- `internal/config/public_packs.go`

The deployer gate artifact is available from commit
`0a6dcc27ce42f2ffef0e94270bbd79bf0d4aa1ee` at
`release-gates/ga-jqvmde-3-setmetadata-gate.md`.

Tracker import was a no-op for this PM pass because no `tracker-to-beads`
skill or command is present in the worktree or rig path.

## Work Packages

1. `ga-jqvmde.4` - Builder: reset SetMetadata candidate scope
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Parent: `ga-jqvmde`
   - Source deploy bead: `ga-jqvmde.3`
   - Acceptance: start from current `origin/main` or the last
     validator-approved candidate
     `047860f002e5ca0b24e80332fa9705f22474cb94`; produce or force-update the
     deploy candidate branch so `git log origin/main..HEAD` contains only the
     SetMetadata integration test commit and the approved `city_discovery`
     gate-remediation commit, or rebased equivalents; confirm the diff excludes
     all `592d55c1c` pack-pin surfaces unless they are already present on
     `origin/main`; record branch name, base SHA, head SHA, exact log, exact
     diff name-status, and whether PR #3498 was force-updated or replaced; if
     the standard gate cannot pass without the pack-pin update, route back to
     PM with evidence instead of bundling it.

2. `ga-jqvmde.5` - Validator: verify reset candidate before deploy
   - Route: `gascity/validator`
   - Label: `needs-tests`
   - Parent: `ga-jqvmde`
   - Source deploy bead: `ga-jqvmde.3`
   - Depends on: `ga-jqvmde.4`
   - Acceptance: verify the branch and SHA recorded by `ga-jqvmde.4`; run
     `make test`, `go vet ./...`, and
     `go test -tags=integration ./internal/beads -run '^TestNativeDoltStoreRegularUpdateEventRecording$' -count=1 -v`;
     confirm `git log origin/main..HEAD` and
     `git diff --name-status origin/main...HEAD` include only the SetMetadata
     test and approved `city_discovery` gate-remediation surfaces, unless one
     is already on `origin/main`; explicitly confirm the `592d55c1c` pack-pin
     files are absent from the deploy diff unless already merged; record
     commands, PASS/SKIP/FAIL, branch, base SHA, head SHA, log, diff
     name-status, and PR URL/state; route back to PM if scope leakage or
     unresolved gate failure remains.

3. `ga-jqvmde.3` - Deployer: existing deploy retry
   - Route: `gascity/deployer`
   - Label: `needs-deploy`
   - Existing bead: `ga-jqvmde.3`
   - Depends on: `ga-jqvmde.4` and `ga-jqvmde.5`
   - Acceptance: retry only after the builder scope reset and validator pass;
     use the branch/SHA recorded by `ga-jqvmde.5`, not stale invalid head
     `592d55c1c`; rerun the standard deploy gate; route merge request to
     mayor/mpr on pass; route back to PM with exact evidence on fail.

## Dependency Graph

- `ga-jqvmde.5` depends on `ga-jqvmde.4`.
- Existing deploy bead `ga-jqvmde.3` depends on `ga-jqvmde.4`.
- Existing deploy bead `ga-jqvmde.3` depends on `ga-jqvmde.5`.

Both new blocker beads are children of the closed split bead `ga-jqvmde`, not
children of `ga-jqvmde.3`, so the deploy bead can depend on them without
creating a parent-child dependency cycle.

## Risks

- The pack-pin update may need its own reviewed release path. It must not be
  folded into the SetMetadata deploy candidate just to get this PR moving.
- PR #3498 may need another force-update or replacement if it still points at
  `592d55c1c`.
- Previous `gc sling` and `gc mail send` attempts have hit
  `HY000 Field id has no default value` while recording events. Durable
  routing metadata on beads is the source of truth if wakeup transport fails.

## Out Of Scope

- Shipping the Gastown pack-pin update as part of the SetMetadata deploy.
- Agent-home worktree cleanup behavior, tracked separately under `ga-oo656x`
  and PR #3496.
- PM-authored implementation or test changes.
