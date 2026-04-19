---
title: "GC Import Launch Implementation Plan"
---

| Field | Value |
|---|---|
| Status | In Progress |
| Date | 2026-04-19 |
| Author(s) | Codex |
| Issue | `ga-nv693` |
| Scope | PackV2 `gc import` launch sweep |

## Summary

Ship the full `gc import` launch backlog in one patch train. The target
state is:

- `gc import` is the only supported schema-2 pack dependency surface
- normal load/start/config paths do not fetch remote imports implicitly
- `gc import install` is the single bootstrap and repair command
- internal packs are system packs, not implicit imports
- default rig composition is driven by
  `[defaults.rig.imports.<binding>]`
- Gastown init emits PackV2-native root `pack.toml`
- docs, migrate, doctor, loader, and runtime behavior converge on one
  story

## Fixed Decisions

- Remote import remediation is explicit. Runtime/config entrypoints do
  not fetch. They fail with a clear `gc import install` hint.
- `gc import install` is both bootstrap and repair. If `packs.lock` is
  absent, it resolves declared imports, writes the lockfile, and
  installs cache state.
- Package-registry and implicit-import launch stories are removed from
  this wave.
- Internal packs are materialized through `.gc/system/packs`.
- Canonical default rig syntax is `[defaults.rig.imports.<binding>]`.
- Gastown init must use the PackV2 shape and preserve the current
  product behavior where new rigs inherit Gastown defaults without
  needing `--include`.

## Slice Graph

```text
Slice A: import bootstrap/remediation contract
    ├── Slice B: system-pack / implicit-import / registry cleanup
    ├── Slice C: default-rig loader + gc rig add + init/gastown wizard
    │       └── Slice D: migrate/doctor/docs convergence
    └── Slice E: remaining import backlog surfaces
            ├── watch/revision
            ├── rig-scoped authoring
            ├── graph inspection
            ├── imported command UX
            ├── named_session imported agents
            └── unknown pack.toml field validation
```

Slices `A`, `B`, and discovery for `E` can run in parallel. `C`
depends on the PackV2 contract being stable enough to wire init/runtime
behavior. `D` lands after the code paths are real.

## Task List

### Phase 1: Contract Foundation

#### Task 1: Remove implicit remote fetch from runtime/config entrypoints

**Description:** Delete legacy auto-fetch behavior from `gc start`,
`gc config`, and supervisor paths so schema-2 cities never do network
work during load.

**Acceptance criteria:**
- `gc start`, `gc config show`, `gc config explain`, and supervisor city
  load do not fetch remote packs implicitly
- missing remote import state fails with actionable remediation text
- legacy `gc pack` compatibility does not silently reintroduce fetch on
  schema-2 code paths

**Likely files:**
- `cmd/gc/cmd_start.go`
- `cmd/gc/cmd_config.go`
- `cmd/gc/cmd_supervisor.go`
- `cmd/gc/cmd_supervisor_city.go`
- `internal/config/pack_include.go`

#### Task 2: Make `gc import install` bootstrap and repair

**Description:** Extend the install path so one command handles both
first-time bootstrap and stale/missing cache repair.

**Acceptance criteria:**
- existing `packs.lock` restores cache/install state
- missing `packs.lock` resolves declared remote imports, writes
  `packs.lock`, and installs them
- repeated `gc import install` runs are idempotent
- error text distinguishes offline/bootstrap failure cleanly

**Likely files:**
- `internal/packman/install.go`
- `internal/packman/lockfile.go`
- `cmd/gc/cmd_import.go`
- `internal/config/pack_include.go`

#### Checkpoint A

- targeted tests pass for `./cmd/gc`, `./internal/config`,
  `./internal/packman`
- run `$review-pr` against the branch diff
- fix blocker/major findings before starting the next checkpoint

### Phase 2: System Pack Cleanup

#### Task 3: Remove package-registry and implicit-import launch artifacts

**Description:** Remove the PackV2 package-registry and implicit-import
story from code, tests, and docs. Keep internal packs on the
`.gc/system/packs` path.

**Acceptance criteria:**
- bootstrap no longer seeds package-registry artifacts
- implicit-import state is not required for PackV2 load
- registry/import bootstrap artifacts are removed if unused
- docs stop presenting implicit imports as a public dependency model

**Likely files:**
- `internal/bootstrap/bootstrap.go`
- `internal/bootstrap/packs/registry/`
- `internal/bootstrap/packs/import/`
- `internal/config/implicit.go`
- `internal/config/compose.go`
- `cmd/gc/embed_builtin_packs.go`

### Phase 3: Default Rig PackV2 Support

#### Task 4: Honor `[defaults.rig.imports.<binding>]` at runtime

**Description:** Wire the root pack default-rig import model into load
and `gc rig add` behavior so newly added rigs inherit PackV2 defaults.

**Acceptance criteria:**
- loader/runtime model can parse and preserve
  `[defaults.rig.imports.<binding>]`
- `gc rig add` uses those defaults when `--include` is omitted
- schema-2 runtime no longer depends on
  `workspace.default_rig_includes` for the supported path

**Likely files:**
- `internal/config/config.go`
- `internal/config/import*.go`
- `cmd/gc/cmd_rig.go`
- `cmd/gc/cmd_rig_test.go`

#### Task 5: Make Gastown init emit PackV2-native imports/defaults

**Description:** Change `gc init` Gastown generation to write the root
`pack.toml` import/default-rig shape instead of relying on legacy city
fields.

**Acceptance criteria:**
- Gastown wizard/init writes PackV2 imports in `pack.toml`
- new Gastown cities still get default rig composition without
  `--include`
- init artifact materialization follows the new source of truth

**Likely files:**
- `cmd/gc/cmd_init.go`
- `cmd/gc/init_artifacts.go`
- `internal/config/config.go`
- `cmd/gc/main_test.go`
- `test/acceptance/init_lifecycle_test.go`

#### Task 6: Align migrate and doctor with the chosen default-rig syntax

**Description:** Make migration, warnings, and tests converge on
`[defaults.rig.imports.<binding>]`.

**Acceptance criteria:**
- migrate emits loadable runtime shape
- doctor text points to the canonical syntax
- stale references to `[rig_defaults]` are removed

**Likely files:**
- `internal/migrate/migrate.go`
- `cmd/gc/doctor_v2_checks.go`
- related tests and txtar fixtures

#### Checkpoint B

- targeted tests pass for `./cmd/gc`, `./internal/config`,
  `./internal/migrate`, `./internal/doctor`
- Gastown init/default-rig behavior is covered end to end
- run `$review-pr` against the branch diff
- fix blocker/major findings before continuing

### Phase 4: Remaining Launch Backlog

#### Task 7: Finish remaining import/runtime surfaces

**Description:** Land the remaining backlog items that affect supported
behavior or sharp edges.

**Acceptance criteria:**
- watch/revision account for PackV2 imports
- rig-scoped import authoring exists on `gc import`
- graph inspection commands exist for import debugging
- imported/discovered subcommands are first-class in help UX
- root-city `named_session` can target imported PackV2 agents
- unknown `pack.toml` fields fail with actionable errors

**Likely files:**
- `internal/config/revision.go`
- `cmd/gc/cmd_import.go`
- `cmd/gc/cmd_commands.go`
- `internal/config/config.go`
- pack parsing/validation tests

### Phase 5: Doc Convergence

#### Task 8: Publish one authoritative PackV2 import doc set

**Description:** Rewrite docs so launch guidance matches shipped code,
including one remediation command and one default-rig syntax.

**Acceptance criteria:**
- `docs/packv2/` reflects shipped behavior, not aspirational behavior
- `docs/guides/` no longer advertises legacy `[packs]` / implicit-import
  guidance as current for schema-2
- conformance/skew docs are updated to remove known false statements

**Likely files:**
- `docs/packv2/doc-pack-v2.md`
- `docs/packv2/doc-packman.md`
- `docs/packv2/doc-loader-v2.md`
- `docs/packv2/doc-conformance-matrix.md`
- `docs/packv2/skew-analysis.md`
- `docs/guides/migrating-to-pack-vnext.md`
- `docs/guides/shareable-packs.md`

#### Checkpoint C

- targeted tests pass for all touched packages
- focused acceptance/integration tests pass
- run `$review-pr` against the branch diff
- fix blocker/major findings

## Parallel Ownership

These are the intended worker boundaries. Write sets should stay
disjoint until integration:

- Worker A: bootstrap/install/remediation in `cmd/gc` +
  `internal/packman` + import error plumbing
- Worker B: default-rig/runtime/init path in `cmd/gc/cmd_rig.go`,
  `cmd/gc/cmd_init.go`, `cmd/gc/init_artifacts.go`,
  `internal/config/*default-rig*`
- Worker C: registry/implicit-import cleanup and associated docs/tests
- Worker D: remaining `cmd_import` user-surface items and related tests
- Main rollout: plan, integration, cross-slice conflict resolution,
  review/fix loop, final verification, PR, and CI

## Verification Matrix

Run targeted suites after each relevant slice, then a full final sweep.

### Targeted

```bash
go test ./cmd/gc ./internal/config ./internal/packman ./internal/migrate ./internal/doctor
go test ./test/acceptance/... ./test/integration/...
```

### Final

```bash
go test ./...
```

If full `./...` is too slow or flaky, capture the failing packages and
rerun until the branch is green locally before pushing.

## Review Protocol

At each natural checkpoint:

1. run local tests for the current slice
2. run `$review-pr` on the local branch diff
3. fix blocker/major findings
4. rerun tests for touched packages
5. proceed to the next slice only after the review loop converges

## Risks and Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Legacy and PackV2 paths share code and regress old cities | High | keep schema-2 behavior gated where needed; preserve legacy tests |
| Default-rig changes break Gastown init expectations | High | add end-to-end init + `gc rig add` coverage before removing fallback behavior |
| Registry cleanup removes something still used indirectly | Medium | trace bootstrap pack usage first; keep system-pack path intact |
| Backlog breadth causes merge conflicts across slices | Medium | use disjoint worker ownership and integrate in checkpoints |
| Docs drift behind code again | Medium | land doc convergence as a required slice, not postscript |

## Definition of Done

- all `ga-nv693.*` backlog items are implemented or closed by the branch
- local review checkpoints converge without blocker/major findings
- branch tests are green
- PR is open
- CI is green
