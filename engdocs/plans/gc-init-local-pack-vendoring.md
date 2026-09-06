# Plan: gc init local pack vendoring

> **Status:** decomposed - 2026-05-28
> **Parent architecture:** `ga-6mtzij` - `gc init --from` vendors external
> local pack imports into the initialized city.
> **Designer specs:** `ga-8atzuw` for `vendorExternalLocalPackDeps`;
> `ga-ki3dbz` for init wiring and tests.
> **Designer handoff mail:** `gm-wisp-4yxa68`.
> **Tracker import:** no tracker skill was installed in this session, so this
> was a no-op.

## Context

`gc init --from <srcDir>` copies only the source city directory. When the
source city's TOML imports a local pack outside that directory, for example
`source = "../dolt"`, the initialized city keeps a relative source that no
longer resolves. Remote imports already use the existing lock/cache path and
are out of scope.

The accepted product outcome is transparent init behavior: external local pack
dependencies are copied into `packs/vendor/<name>/`, copied TOML files are
rewritten to `//packs/vendor/<name>`, and `config.LoadWithIncludes` can resolve
the initialized city without requiring a new command or flag.

## Scope

This plan turns the architecture and designer handoff into three sequential
builder work packages:

| Builder bead | Scope | Blocked by |
|---|---|---|
| `ga-8atzuw.1` | Direct local pack vendoring and TOML rewrite helpers | none |
| `ga-8atzuw.2` | Transitive, deduped, and cycle-safe vendoring | `ga-8atzuw.1` |
| `ga-ki3dbz.1` | `doInitFromDirWithOptionsFS` wiring and integration coverage | `ga-8atzuw.2` |

## Acceptance Themes

- `vendorExternalLocalPackDeps(fs, srcDir, cityPath)` is implemented in
  `cmd/gc/cmd_init_vendor.go` using the existing `fsys.FS` abstraction.
- Direct external local imports in `city.toml` and `pack.toml` are copied into
  `packs/vendor/<name>/` and rewritten to city-root-relative `//` sources.
- Transitive graphs, diamond graphs, duplicate basenames, and cycles terminate
  deterministically without source-tree writes.
- Remote import sources and already city-root-relative sources are ignored.
- `gc init --from` calls vendoring after `.gitignore` setup and before any
  config load that requires imports to resolve.
- Unit and integration tests cover the designer's named cases, including the
  full `doInitFromDir` path.
- Quality gates for the final slice include `make test`, `go vet ./cmd/gc/...`,
  all vendoring unit tests, and the integration test.

## Sequencing

1. Build the direct vendoring surface and TOML rewriting first. This establishes
   the helper file, function signatures, filesystem discipline, and direct
   import behavior without yet depending on graph complexity.
2. Extend the queue algorithm for transitive dependencies, diamond dedup,
   basename conflict handling, and cycle/depth safety.
3. Wire the completed helper into `doInitFromDirWithOptionsFS` and add the
   end-to-end init test.

The PM-created dependency graph enforces this order with `bd dep add` so only
the first builder bead is ready immediately.

## UX Follow-up From `ga-zyyp4l`

Designer handoff `ga-zyyp4l` arrived after the initial builder sequence was
already decomposed and completed. It adds visible CLI UX requirements for the
vendoring step and asks whether the helper should return vendored pack names
so `cmd_init.go` can print a useful summary.

PM scope decisions:

- Transitive annotations in success output are v2, not MVP.
- Declaration hints in error output are v2, not MVP, unless architecture
  decides they are required by the contract.
- The helper signature and graph-depth behavior are architecture decisions,
  not PM decisions.

Follow-up work:

| Bead | Route | Scope | Blocked by |
|---|---|---|---|
| `ga-zyyp4l.1` | architect | Decide the approved helper/output contract and runaway graph behavior | none |
| `ga-zyyp4l.2` | validator | Define CLI UX regression coverage against the approved contract | `ga-zyyp4l.1` |
| `ga-zyyp4l.3` | builder | Implement the approved CLI summary output delta | `ga-zyyp4l.1`, `ga-zyyp4l.2`, `ga-ki3dbz.1` |

The follow-up should build on the latest completed stack from
`ga-ki3dbz.1`, not recreate the original vendoring implementation beads.

## Out Of Scope

- New user-facing CLI flags or commands.
- Changes to remote import resolution, `packs.lock`, or global import caches.
- Modifying `config.LoadWithIncludes` semantics.
- Preserving TOML comments or exact formatting after rewrite.
- Writing implementation code in the PM session.

## Handoff Notes

The builder should treat the designer bead bodies as the detailed spec. The
architecture guardrails that matter most are:

- Source trees are read-only; all writes go under `cityPath`.
- Vendor paths are user-visible `packs/vendor/<name>/`, not `.gc/`.
- Existing `initFromSkip` behavior applies when copying vendor packs.
- No hardcoded roles or orchestration behavior are involved in this feature.
