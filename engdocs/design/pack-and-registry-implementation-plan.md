---
title: "Pack CLI and Pack Registry Implementation Plan"
---

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-05-17 |
| Author(s) | Codex |
| Issue | TBD |
| Scope | Staged implementation plan for `gc pack` / `gc pack registry` |

This plan implements
[`pack-and-registry-cli-surface.md`](pack-and-registry-cli-surface.md).
It treats that design as the product contract and organizes the work into
waves with explicit dependencies, validation slices, and compatibility rules.

## Intended Contract

At the end of this rollout:

- `gc pack` is the canonical pack dependency surface.
- `gc pack registry` owns registry configuration, catalog refresh, search, and
  catalog inspection.
- `gc import` commands are thin deprecated wrappers where the surface maps
  cleanly.
- `gc import migrate` has no successor command; `gc doctor` / `gc doctor --fix`
  owns equivalent migration/remediation guidance before it is removed.
- Legacy `gc pack fetch` becomes a deprecated wrapper for `gc pack sync`.
- Legacy `gc pack list` remains the canonical `gc pack list`, but its old
  `[packs]`/`pack.lock` behavior is reported through compatibility diagnostics
  instead of continuing as a separate dependency model.
- Normal load/start/config paths remain network-free readers of manifests,
  `packs.lock`, and cache state.
- Registry-backed commands use registry handles as command-time selectors,
  then persist concrete durable import `source` values in manifests and
  registry/ref/commit/hash metadata in schema-2 `packs.lock`.

Intentionally preserved:

- Existing direct git source forms: `https://...`, `ssh://...`, `git@...`,
  bare `github.com/org/repo`, `file://...`, and explicit local paths.
- PackV2 import fields: `source`, `version`, `export`, `transitive`, `shadow`.
- Historical PackV2 launch docs, with successor markers at the top.
- Existing compat coverage for `gc import` and legacy `[packs]` until doctor
  parity and deprecation windows are complete.

Out of scope for the implementation waves below:

- Non-git package source kinds.
- Authenticated private registries.
- Signing, attestations, or TUF-style delegation.
- A pack publishing command surface.

## Inventory Classification

### Migrate Now

- `cmd/gc/cmd_pack.go`: replace the legacy two-command surface with the new
  dependency and registry command tree.
- `cmd/gc/cmd_import.go`: refactor command handlers into thin wrappers over
  the new `gc pack` implementation.
- `internal/packman/*`: registry resolution, schema-2 lockfile, cache
  verification, sync/check/why/outdated/upgrade behavior.
- `internal/config/pack.go`, `internal/config/compose.go`, and
  `internal/config/repo_cache_lock.go`: keep loader behavior as a lock/cache
  reader and prevent new registry/network work in load paths.
- `cmd/gc/cmd_doctor.go` and `internal/doctor/*`: registry and deprecation
  diagnostics.
- Current-facing PackV2 docs that tell users what command to run.

### Preserve For Compatibility

- `gc import migrate` tests and implementation until doctor dry-run/fix parity
  exists.
- Legacy direct git/path import tests.
- Bootstrap/internal `import` and `registry` pack compatibility tests.
- Legacy `[packs]` / `pack.lock` checks until the new doctor warnings and
  remediation are in place.

### Docs And Goldens Follow-Up Within This Plan

- `docs/reference/cli.md` after command generation updates.
- Tutorial goldens only when their command output changes.
- Historical docs keep their old narrative but get markers pointing at the new
  canonical surface.

## Stage Dependency Graph

```text
Wave 0: Contract and inventory cleanup
    |
    v
Wave 1: Registry configuration and catalog operations
    |
    v
Wave 2: Registry resolution primitives
    |
    v
Wave 3: Schema-2 lockfile and cache verification
    |
    v
Wave 4a: gc pack command shape
    |
    v
Wave 4b: scope matrix and dependency semantics
    |
    +--> Wave 5: gc import thin wrappers and deprecation
    |
    +--> Wave 6: doctor diagnostics and migration parity
    |
    v
Wave 7: docs, generated CLI reference, and examples
    |
    v
Wave 8: acceptance hardening and removal gates
```

Wave 5 and Wave 6 can start once Wave 4a/4b have stable command handlers, but
Wave 6 must finish before `gc import migrate` removal. Wave 7 starts after
command help stabilizes. Wave 8 is the release-readiness sweep.

## Wave 0: Contract And Shared Helpers

**Goal:** Make the implementation boundary boring before registry work starts.

### Tasks

- Add shared `GC_HOME` resolution in one package used by packman, config,
  bootstrap, registry config, and doctor. Name it `internal/gchome`.
- Normalize test isolation so packman no longer implicitly depends on
  `os.UserHomeDir()` while registry code uses `$GC_HOME`.
- Compose with Jasmine's CLI JSON platform from PR 2222 instead of adding a
  second JSON mode: use the root `--json` and `--json-schema` plumbing,
  `writeCLIJSONLine`, the shared failure shape, and embedded
  `schemas/<command path>/result.schema.json` files.
- Add package-level source/name classifier skeleton with table-driven tests.
  Name it `internal/packsource`; Wave 2 extends it with registry-specific
  semantics.
- Add manifest compatibility contract tests: when both legacy `[packs]` and
  `[imports]` are present, `[imports]` wins and a compatibility diagnostic is
  emitted; later doctor fix rewrites safe `[packs]` entries into `[imports]`.
- Add a loader import-boundary test that `internal/config` does not import
  network packages such as `net/http` or registry-fetch packages.
- Add top markers to historical `gc import` docs.

### Likely Files

- `internal/packman/cache.go`
- `internal/config/implicit.go`
- `internal/config/pack.go`
- `internal/config/compose.go`
- `internal/config/repo_cache_lock.go`
- `cmd/gc/json_schema.go` and `schemas_embed.go` from PR 2222
- new `internal/gchome`
- new `internal/packsource`
- `docs/packv2/doc-packman.md`
- `engdocs/design/gc-import-launch-implementation-plan.md`

### Validation

```sh
go test ./internal/packman ./internal/config ./internal/bootstrap
go test ./cmd/gc -run 'Test.*JSON|Test.*Home|TestDoImport'
```

## Wave 1: Registry Configuration And Catalog Operations

**Goal:** Implement registry operations first, before dependency mutation
depends on them.

### Tasks

- Add `internal/packregistry` package for:
  - `registries.toml` read/write
  - registry name validation
  - catalog source normalization
  - catalog fetch from HTTPS, `file:`, and local path
  - catalog cache snapshots under `$GC_HOME`
  - sidecar locking: `$GC_HOME/registries.toml.lock`
  - atomic writes
- Add catalog parser and validator:
  - schema validation
  - scoped pack names
  - full commit SHA validation
  - hash format validation
  - duplicate release rejection
  - withdrawn release parsing
  - remote catalog rejects `file:` or local-path pack sources
  - immutable release metadata check against previous cache
- Add `gc pack registry` commands:
  - `list`
  - `add`
  - `remove`
  - `refresh`
  - `search`
  - `show`
- Add `--refresh`, `--limit`, and `--all`; JSON is requested through the shared
  root `--json` flag, not command-local `--format json`.
- Add typed registry JSON structs, result schemas, and golden tests in the Wave
  1 PRs, not as a trailing docs-only cleanup.
- Add `gc init` seeding hook for `main` when `$GC_HOME/registries.toml` does
  not exist, guarded behind the final first-party URL constant.
- Add a release-blocking test such as `TestSeedRegistryURLNotPlaceholder` so
  the placeholder first-party registry URL cannot ship.

### Dependencies

- Wave 0 shared `GC_HOME` and JSON helpers.

### Validation

```sh
go test ./internal/packregistry
go test ./cmd/gc -run 'TestPackRegistry'
```

### Edge Tests

- TLS verification for HTTPS registries
- HTTPS-to-HTTP redirect rejection, with no cache write
- catalog fetch timeout
- catalog size cap and gzip/decompression limit
- invalid registry names and 65-character names
- scoped and unscoped pack names
- duplicate releases
- withdrawn releases with reason
- full commit SHA validation
- hash format validation
- HTTPS catalog with `file:` pack source rejected
- local catalog with `file:` pack source accepted
- local catalog with local-path pack source accepted
- immutable release metadata check against previous cache
- stale cache warning and `--refresh` behavior
- partial registry reachability during search
- `registry show lighthouse` resolves only when unambiguous
- concurrent `registry add` writes produce valid TOML
- atomic write preserves the previous config on write/rename failure
- removed registry snapshots ignored and pruned on refresh
- `GC_REGISTRY_FRESHNESS` valid and invalid parsing
- `--no-validate` skips add-time fetch only; first real use still validates
  schema and transport
- `gc init` registry seeding is idempotent when the real first-party registry
  URL is available

### PR Boundary

- 1a: `internal/gchome`, `internal/packregistry` config structs, TOML I/O,
  locking, and atomic writes.
- 1b: catalog parser, validator, immutable metadata checks, HTTPS catalog
  rejection of `file:` and local-path pack sources.
- 1c: `gc pack registry list/add/remove/refresh`.
- 1d: registry JSON structs, result schemas under
  `schemas/pack/registry/.../result.schema.json`, `--json-schema` tests,
  output-vs-schema tests, failure-schema tests, and goldens. This slice is
  blocked until PR #2222's shared JSON platform is present locally.
- 1e: `gc init` seeding behind the real first-party registry URL constant. If
  the URL is not available, keep this slice blocked rather than landing a
  placeholder.

## Wave 2: Registry Resolution Primitives

**Goal:** Add command-time registry selector resolution without teaching
manifest sync/check paths to treat `registry:` as durable import sources.

### Tasks

- Add registry selector parsing for command-time inputs.
- Add registry-backed candidate selection:
  - reuse valid locked release first
  - highest non-withdrawn semver release within constraint
  - exact version beats range
  - `sha:[0-9a-f]{40}` bypasses semver
- Add direct git default behavior:
  - no `--version` locks remote default branch HEAD
  - semver constraints inspect tags
- Add canonical pack tree hash implementation.
- Add immutable-release mismatch errors and remediation text.
- Add source classifier support for:
  - `registry:main:lighthouse`
  - `main:lighthouse`
  - `main:acme/lighthouse`
  - bare names
  - direct git locators
  - explicit paths
  - path/bare-name collision hints.
- Extend `internal/packsource` from Wave 0 rather than introducing a second
  classifier.

### Dependencies

- Wave 1 catalog parser/cache.

### Validation

```sh
go test ./internal/packman -run 'TestResolve|TestRegistry|TestTreeHash|TestSource'
```

### Edge Tests

- withdrawn release skipped unless SHA-pinned
- stale cached catalog used with warning
- unreachable registry fails closed for unqualified names
- unqualified resolution fails closed per command: `add`, `sync`, `outdated`,
  `upgrade`, and `registry show`
- ref resolves to different commit
- hash mismatch
- symlink, executable bit, ignored dirs, and submodule tree-hash cases
- symlink escape, absolute symlink, parent-ref escape, and unreadable file cases
- cross-platform tree-hash golden fixture
- tree-hash digest includes the `gc-pack-tree-sha256-v1` algorithm prefix
- prerelease/build-metadata resolver policy
- strict SHA pins: reject short, uppercase, and mixed-case pins
- invalid `--version` rejected before manifest writes
- bare `lighthouse` with `./lighthouse` present suggests `./lighthouse`

## Wave 3: Schema-2 `packs.lock` And Cache Verification

**Goal:** Extend the root-city lockfile without forking lock semantics.

### Tasks

- Introduce `packs.lock` schema 2 in `internal/packman`.
- Preserve schema-1 read compatibility and fence schema-2 writes: keep writing
  schema 1 unless a schema-2-only field is required, such as registry origin,
  withdrawn state, graph-node scope namespace, or non-legacy source kind.
- Write graph-node entries with:
  - node identity
  - parent node
  - scope namespace
  - local import name
  - registry name/source when applicable
  - manifest constraint
  - selected version
  - source kind/source/ref/commit/hash
  - fetched/synced and withdrawn state
- Keep city-root lockfile as the only lockfile for city and rig scopes.
- Reject `sync`, `outdated`, `upgrade`, and `list --transitive` for standalone
  packs outside a city.
- Keep current cache layout as implementation detail while verifying by logical
  source/source-kind/commit/hash.
- Split ownership so schema types, read/write migration, and cache verification
  can be reviewed independently.

### Dependencies

- Wave 2 resolution primitives.

### Validation

```sh
go test ./internal/packman -run 'TestLock|TestSync|TestCheck|TestInstall'
go test ./internal/config -run 'Test.*Pack|Test.*Import'
```

### Edge Tests

- unknown/future schema versions rejected for `packs.lock`, `registry.toml`,
  and `registries.toml`
- schema-1 migration
- schema-1 file remains schema 1 after no-op sync with only legacy fields
- schema-2 written when registry imports or scoped graph-node fields appear
- diamond graph
- same source under two bindings
- same pack in city and rig scopes
- `transitive=false`
- stale and missing lock entries
- deterministic write order
- byte-stable lockfile writes across repeated runs
- cross-process `packs.lock` writer serialization
- stale/dead lock recovery if lock sidecars are used
- lock conflict diagnostics
- dirty/missing cache
- hash mismatch on fetch
- tampered cache content rejection on verification paths
- atomic writes preserve previous files on simulated `ENOSPC`/`EACCES`

## Wave 4a: Canonical `gc pack` Command Shape

**Goal:** Land the additive command shape and aliases without temporarily
removing existing commands.

### Tasks

- Replace `cmd/gc/cmd_pack.go` with:
  - `add`
  - `remove`
  - `sync`
  - `check`
  - `list`
  - `show`
  - `outdated`
  - `upgrade`
  - `why`
  - nested `registry`
- Convert `gc pack fetch` into a deprecated alias for `gc pack sync` in this
  same wave so it never disappears between PRs.
- Preserve existing `gc pack list` flags that still map cleanly, while routing
  through the new handler.
- Add JSON output structs, result schemas, `--json-schema` tests, and goldens
  for pack read verbs in the same wave as the verbs they describe.

Current workstream boundary: the registry/gc pack PR includes the registry
commands and low-risk dependency wrappers for `add`, `remove`, `sync`,
`check`, `upgrade`, and `why`. Dependency `show`, `outdated`, and canonical
dependency `list` remain in Wave 4a/4b follow-up work. `gc pack list` stays on
the legacy `[packs]` status surface until that compatibility collision is
resolved intentionally.

### Dependencies

- Wave 3 lockfile/cache behavior.

### Validation

```sh
go test ./cmd/gc -run 'TestPack|TestDoPack|TestPackFetchAlias|TestPackList'
```

### Edge Tests

- `gc pack fetch` executes sync path exactly once
- legacy `gc pack list` text output has an explicit golden during transition
- this workstream asserts that dependency `show` and `outdated` are absent, and
  that `list` is still legacy `[packs]` status
- JSON no-scope failure uses the PR #2222 shared failure schema
- pack read-command `--json` stdout validates against `--json-schema=result`
- failing pack read-command `--json` validates against `schemas/failure.schema.json`
- result schemas have per-field descriptions for every public and nested public
  field
- schema enums match Go constants
- result schemas reject unexpected public fields unless explicitly extensible
- `show` does not refresh catalog unless asked through refresh-capable command

## Wave 4b: Scope Matrix And Dependency Semantics

**Goal:** Complete pack dependency behavior after the command tree exists.

### Tasks

- Implement scope targeting:
  - ambient pack
  - city pack
  - explicit `--pack`
  - explicit `--city`
  - explicit `--rig`
  - no implicit mutation from CWD inside a rig
  - standalone pack `add` implies `--no-sync`
- Map import fields:
  - `--export`
  - `--transitive`
  - `--no-transitive`
  - `--transitive-default`
  - `--shadow`
  - `--version`
- Complete command semantics behind the JSON structs and goldens added in Wave
  4a.
- Preserve text defaults.
- Make `gc pack list` canonical for both old and new expectations; legacy
  `[packs]` status appears as compatibility diagnostics until removed.
- Cross-check the scope matrix against `docs/packv2/doc-rig-binding-phases.md`
  and add a rig-binding acceptance case.

### Dependencies

- Wave 4a command handlers.

### Validation

```sh
go test ./cmd/gc -run 'TestPack|TestDoPack|TestPackV2|TestImportState'
go test ./test/acceptance -tags acceptance_a -run 'Test(ConfigShowCommands|PackListCommands|GastownPackMaterialization)'
```

### Edge Tests

- CWD inside rig without flags errors for every mutating verb
- CWD inside pack nested under rig targets pack
- `--pack` plus non-city `--rig` errors
- symlinked city/pack paths, tilde expansion, relative paths, and absolute paths
  resolve to the same scope
- rig-name collisions across registered cities error with `--city` guidance
- standalone `gc pack add` writes manifest and skips sync
- standalone `gc pack sync`, `outdated`, `upgrade`, and `list --transitive`
  reject with city-context remediation
- `gc pack add --version` rejects plain local directories and leaves manifests
  unchanged
- local git directories with `--version` normalize to absolute `file:` sources
- `gc pack add` rejects unrelated derived-name collisions
- re-adding an existing import preserves omitted fields
- normal `gc pack remove` blocks active references from pack config, city/rig
  config, formulas, commands, and agent definitions
- `remove --force` leaves dangling refs and warns
- `outdated` flags locked withdrawn releases
- `--rig` binding behavior matches the rig-binding phases design

## Wave 5: `gc import` Thin Wrappers

**Goal:** Make old commands route through the new surface without retaining a
second implementation.

Implementation note: the current workstream takes the low-risk first step by
sharing command handlers between `gc import` and the new `gc pack` dependency
commands while keeping `gc import` output stable. Flipping `gc import` into
explicit deprecated aliases remains gated on doctor parity and deprecation
coordination.

### Tasks

- Refactor shared command handlers so `gc import` wraps `gc pack` operations:
  - `gc import add` -> `gc pack add`
  - `gc import remove` -> `gc pack remove`
  - `gc import check` -> `gc pack check`
  - `gc import install` -> `gc pack sync`
  - `gc import upgrade` -> `gc pack upgrade`
  - `gc import list` -> `gc pack list`
  - `gc import why` -> `gc pack why`
- Keep `gc import migrate` until doctor parity exists; mark it removed from the
  successor surface.
- Ensure legacy `gc pack list` semantics are not a second code path; surface
  legacy `[packs]` data through diagnostics or doctor.
- Add deprecation warnings:
  - two minor releases warning
  - one minor release hard error with remediation
  - removal after that window.
- Gate warning release on Wave 6 deprecated-command-reference doctor checks, or
  ship the two in the same release, so users get both the warning and the
  remediation surface together.

### Dependencies

- Wave 4a/4b command handlers.

### Validation

```sh
go test ./cmd/gc -run 'TestDoImport|TestImportAlias|TestPackFetchAlias|TestPackList'
go test ./test/acceptance -tags acceptance_a -run 'TestPackListCommands'
```

### Compat Tests

- wrapper preserves old flags where mapped
- wrapper emits deprecation warning on stderr only
- wrapper warning names the canonical successor command
- wrapper warning never pollutes JSON stdout
- JSON-capable wrappers validate stdout against the target command result schema
- `gc import migrate` remains available until doctor parity
- text output remains stable unless explicitly changed by alias warning tests
- text-output goldens for every `gc import` alias at cutover
- deprecation release constants are not placeholders
- `gc import migrate` emits no forward-looking replacement command; it points
  to `gc doctor --fix` once parity exists

## Wave 6: Doctor Diagnostics And Migration Parity

**Goal:** Shift migration and registry-health pressure into doctor.

### Tasks

- Add doctor checks for:
  - deprecated command references in parsed command/doctor hooks
  - deprecated command references in `.gc` generated files
  - deprecated command references in Gas City-controlled templates
  - registry config parse failures
  - unreachable/stale catalogs
  - removed registry references
  - invalid durable imports that still contain command-time registry selectors
  - schema-1 lock entries
  - stale/missing lock entries
  - cache entries missing for lock nodes
  - ref/commit/hash mismatches
  - withdrawn locked releases
  - legacy `[packs]` / `pack.lock` state
- Build `gc doctor --fix` parity for `gc import migrate --dry-run` and safe
  mechanical fixes.
- Prune cached catalog snapshots for removed registries when running
  `gc doctor --fix`.
- Only after parity lands, move `gc import migrate` into the deprecation/removal
  window.

### Dependencies

- Wave 5 wrappers and Wave 3 lock schema.

### Validation

```sh
go test ./internal/doctor ./cmd/gc -run 'TestDoDoctor|TestImportMigrate|TestRegistryDoctor'
go test ./test/acceptance -tags acceptance_a -run 'Test.*Doctor|TestMigrationRegression'
```

### Edge Tests

- removed registry still referenced by manifest
- stale registry cache warning
- schema-1 lockfile diagnostic
- withdrawn release remediation
- `gc import migrate` parity dry-run text
- mixed `[packs]`/`[imports]` manifest where `[imports]` wins and fix rewrites
  safe legacy state
- doctor parity against every existing migrate golden corpus case
- doctor report-only and `--fix` behavior for `[packs]` only, mixed manifests,
  schema-1 lockfiles, and stale/missing lock/cache
- immutable metadata changes
- removed registry cache pruning under `--fix`
- doctor does not scan arbitrary free-form repo docs/scripts

## Wave 7: Docs, Generated Reference, And Examples

**Goal:** Make current docs say `gc pack`, while preserving historical docs as
history.

### Tasks

- Update current-facing PackV2 docs:
  - `docs/packv2/doc-pack-v2.md`
  - `docs/packv2/doc-loader-v2.md`
  - `docs/packv2/doc-conformance-matrix.md`
  - `docs/packv2/doc-commands.md`
- Audit the rest of `docs/packv2/` for user-facing references to `gc import`,
  `gc pack fetch`, legacy `[packs]`, and schema-1 lockfile behavior; update or
  add historical markers as appropriate.
- Preserve historical docs with top markers:
  - `docs/packv2/doc-packman.md`
  - `engdocs/design/gc-import-launch-implementation-plan.md`
- Regenerate CLI reference after command help changes:
  - `docs/reference/cli.md`
- Add registry examples:
  - local file catalog
  - remote HTTPS catalog
  - scoped pack names
  - withdrawn release
- Update `engdocs/design/index.md` when the design/plan status changes.

### Dependencies

- Wave 4a/4b command help shape.
- Wave 5 alias policy.
- Wave 6 doctor wording.

### Validation

```sh
go test ./internal/docgen ./test/docsync
go test ./cmd/gc -run 'Test.*Gendoc|Test.*Completion|TestScripts'
```

Generated/reference docs must consume JSON Schema `description` fields when
presenting `--json` output. Checked-in generated docs must stay clean after
docgen, and examples must include both successful and failure JSON shapes.

## Wave 8: Acceptance Hardening And Release Gates

**Goal:** Prove the new surface works under realistic operator workflows.

### Scenario Tests

- Fresh city, add remote registry, search, show, add pack, sync, start.
- Fresh clone with committed schema-2 `packs.lock`, no cache: `gc pack sync`
  restores cache without rewriting manifest.
- Offline with fresh lock/cache: `list`, `show`, `sync --verify-only`, and
  startup succeed without network.
- Offline with missing lock: fails with `gc pack sync --refresh` or registry
  remediation guidance.
- Registry withdraws release: `outdated` and `doctor` flag it, existing lock
  still loads.
- Registry mutates immutable release metadata: refresh hard-fails.
- Removed registry with existing import: sync from lock still works when cache
  verifies, `outdated`/`upgrade` and doctor report missing origin.
- Rig-scoped import add/sync/list, including no-flag rig-CWD error.
- Standalone pack authoring: `add` writes manifest and does not auto-sync.
- Deprecated aliases: `gc import install`, `gc import check`, `gc pack fetch`
  route to new implementations and warn.
- Legacy migration paths: `[packs]` only, mixed `[packs]`/`[imports]`, partial
  doctor report-only, and doctor fix parity with the former
  `gc import migrate` golden corpus.
- First-party registry seeding uses the real URL, enforced by
  `TestSeedRegistryURLNotPlaceholder`.
- Two-clone reproducibility: clone A adds/syncs/commits, clone B pulls/syncs,
  and lockfile bytes plus verified cache content match.
- Runtime network boundary: load/start/config paths do not perform registry or
  network fetches.
- Stable exit-code policy for usage errors, network errors, integrity failures,
  lock contention, and unsupported JSON.

### Validation

```sh
make test-fast-parallel
make test-cmd-gc-process
make test-acceptance
go test ./...
```

Release-blocking gate bundle:

- `TestSeedRegistryURLNotPlaceholder`
- `TestDeprecationReleasesNotPlaceholder`
- `TestDoctorFixCoversEveryMigrateGoldenCase`
- `TestRegistryFetchVerifiesTLS`
- `TestRegistryFetchRejectsHTTPSToHTTPRedirect`
- `TestRegistryFetchRejectsOversizedCatalog`
- `TestRegistryFetchRespectsDeadline`
- `TestPackTreeHashRejectsSymlinkEscape`
- `TestPackTreeHashCrossPlatformGolden`
- `TestPacksLockRejectsUnknownSchemaVersion`
- `TestEmbeddedResultSchemasHaveDescriptions`
- `TestJSONFailuresMatchFailureSchema`
- `TestLoadAndStartMakeNoNetworkCalls`
- `TestPackCommandExitCodesAreStable`

## JSON And JSON Schema Implementation Checklist

The design doc defines result shapes, while PR 2222 defines the platform
plumbing. Implementation should add typed structs, embedded JSON Schema files,
schema-discovery tests, output-versus-schema validation tests, and golden tests
for:

- `gc pack list --json`
- `gc pack show --json`
- `gc pack outdated --json`
- `gc pack why --json`
- `gc pack registry list --json`
- `gc pack registry search --json`
- `gc pack registry show --json`

Each response must include:

- `schema_version: "1"` for these pack/registry result payloads
- `diagnostics: []` when no diagnostics are present
- stable field names
- per-field `description` text in every result schema for each public field and
  nested public field; generated/reference presentations of JSON output should
  consume these descriptions rather than duplicating field docs by hand
- result diagnostics in stdout; command failures follow the PR 2222 shared
  failure payload on stdout and may still write human diagnostics to stderr
- golden tests covering empty, normal, stale-warning, and error-ish results
- tests that execute each supported command with `--json` and validate stdout
  against that command's `--json-schema=result`; a command cannot be called done
  until real output conforms to its declared schema
- per-command schema-version policy: an incompatible shape bump affects that
  command's result schema and golden, not unrelated command schemas
- a golden-diff guard in CI or the closest existing doc/test target

Registry command structs and goldens belong in Wave 1d. Pack command structs
and goldens belong in Wave 4a/4b.

Schema files must follow PR 2222's embedded layout:

- `schemas/pack/list/result.schema.json`
- `schemas/pack/show/result.schema.json`
- `schemas/pack/outdated/result.schema.json`
- `schemas/pack/why/result.schema.json`
- `schemas/pack/registry/list/result.schema.json`
- `schemas/pack/registry/search/result.schema.json`
- `schemas/pack/registry/show/result.schema.json`

Validation must cover `gc pack list --json-schema`, role-specific
`--json-schema=result`, the shared `--json-schema=failure`, and `--json` on an
unsupported command path returning the platform `json_unsupported` failure. It
must also cover schema description completeness and output/schema conformance
for every pack and registry JSON command listed above.

Failure cases for JSON-supported commands must validate stdout against
`schemas/failure.schema.json`. Deprecation warnings and diagnostics must never
make JSON stdout unparsable.

## Deprecation Gates

Use release placeholders until the real release train is named:

| Surface | T+0 | T+1 | T+2 | T+3 |
|---|---|---|---|---|
| `gc import add/remove/check/install/upgrade/list/why` | alias warning, only after doctor references land | warning | hard error with remediation | removal |
| `gc pack fetch` | alias warning in Wave 4a | warning | hard error with remediation | removal |
| `gc import migrate` | keep available until `gc doctor --fix` parity passes the golden corpus | warning after parity | hard error with doctor remediation | removal |

`gc import migrate` is parity-gated, not time-gated: do not start its warning
window until `gc doctor --fix` covers the safe mechanical cases and dry-run text
that `gc import migrate` previously owned.

## Stop-Loss Points

- If Wave 1 cannot keep registry operations isolated from dependency mutation,
  stop and split registry config/catalog work into its own PR before touching
  `gc pack add`.
- If schema-2 lockfile migration forces broad loader changes, stop after
  schema read/write and land loader integration separately.
- If `gc import` wrappers require preserving large chunks of duplicate command
  logic, stop and extract shared handlers before continuing.
- If doctor parity for `gc import migrate` is larger than expected, keep
  `gc import migrate` longer and land doctor diagnostics first.
- If acceptance failures span unrelated PackV2 behavior, revisit PR boundaries
  instead of patching across the repo.

## Parallelization Plan

Safe parallel work:

- Wave 1 registry config/catalog package and CLI commands can be split between
  package implementation and CLI wiring once the package API is sketched.
- Wave 2 source classification and tree hashing can run in parallel.
- Wave 3 lockfile schema and cache verification can run in parallel after the
  schema structs are agreed: one owner for schema types, one for read/write and
  migration behavior, and one for cache verification tests.
- Wave 6 doctor checks can split by family: registry config, lock/cache drift,
  deprecated commands.
- Wave 7 docs can run while Wave 6 doctor implementation proceeds, after
  command names and help text stabilize.

Avoid parallel edits:

- `cmd/gc/cmd_pack.go` and `cmd/gc/cmd_import.go` should have one owner during
  Wave 4/5 handler extraction.
- `internal/packman/lockfile.go` and `install.go` should have one owner until
  schema-2 graph identity is stable.
- Generated docs should be regenerated after code settles, not by multiple
  workers in separate branches.
