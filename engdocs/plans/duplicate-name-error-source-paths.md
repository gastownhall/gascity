# Plan: Render non-empty source paths in duplicate-name errors (`ga-tpfc` family)

> Owner: `gascity/pm-1` · Created: 2026-04-30
> Source: architecture decision `ga-tpfc` (closed)
> Designer addendum: `gascity/designer` (in `ga-tpfc.1` notes)
> Sibling plan (must coordinate): `pack-v1-v2-collision-detection`

## Why this work exists

`internal/config/config.go:ValidateAgents` (~line 2371-2375) renders
duplicate-name errors as:

```
agent "mayor": duplicate name (from "packs/gastown" and "")
```

The empty `""` makes the error untriageable — operators can't tell
which two definitions collided. Three categories produce empty
`SourceDir` today: auto-imported system packs, inline `[[agent]]`
declarations in `city.toml`, and any future origin that doesn't
flow through `DiscoverPackAgents`.

Architecture (`ga-tpfc`) chose **proposal: add an unexported
`agent.source` enum field**, populated at the same discovery sites
that already stamp `SourceDir`/`BindingName`, and render a useful
descriptor for each category. Empty string is never user-visible.

## Goal

Render every duplicate-name error with two non-empty, navigable
source descriptors so operators can locate both definitions
without grepping the codebase.

## Work breakdown

| Bead         | Title                                              | Priority | Routes to | Gate           |
|--------------|----------------------------------------------------|----------|-----------|----------------|
| `ga-tpfc.1`  | Implement source-provenance rendering              | P1       | builder   | ready-to-build |

The architect+designer broke this work down to a single coherent
implementation unit covering the field, discovery-site stamping,
the `describeSource` method, the shared `formatDuplicateAgentError`
helper, and the test matrix. No further PM decomposition is needed
— the bead body is itself the implementation plan, including
table-driven test specifications and propagation assertions.

## Coordination

`ga-tpfc.1` and `ga-9ogb.1` (`pack-v1-v2-collision-detection` plan)
**share a helper function**, `formatDuplicateAgentError`. Either
bead may land first; the second rebases. The shared test file
recommendation (per the designer notes): a single
`internal/config/duplicate_agent_error_test.go` containing
table-driven cases owned by both beads.

The builder for whichever lands second should:

1. Pull the helper from the first PR.
2. Add the second variant's switch arm to it.
3. Add their test rows to the shared test file.

## Routing rationale

Designer addendum is present in the bead notes — covers operator
UX, accessibility audit, edge cases, and the validator acceptance
checklist. No more design hops needed. Routed to **builder** with
`ready-to-build`. Implementation includes its own test matrix per
the design (TDD: tests authored alongside code).

## Acceptance criteria (rolled up)

- **Bug repro test passes.** Duplicate `mayor` across a user pack
  and the auto-imported system pack renders
  `<auto-import: .gc/system/packs/gastown>` verbatim.
- **No empty quoted strings** anywhere in the rendered duplicate-
  agent error corpus. Grep-test golden output for `and ""`; zero
  matches required.
- **Existing string-pinned tests stay green** for non-empty
  `SourceDir` cases.
- **`describeSource` honors empty `cityFile`** (renders `<inline>`
  with no trailing colon, per designer note edge case 2).
- **Field-sync test updated:** `agent.source` listed in
  `field_sync_test.go` expected fields.
- **Patch / override / pool deep-copy preserve `source`.**
  Propagation assertions live alongside whichever test covers
  `applyAgentPatch`, `applyAgentOverride`, and the
  `cmd/gc/pool.go` deep-copy.

Full acceptance checklist is in the bead body's "Acceptance
checklist (for validator)" section.

## Risks and unknowns

- **`source` cleared by a struct rebuild we miss.** Field-sync test
  plus propagation assertions are the safety net. CLAUDE.md flags
  this as a known gotcha — read the section "Adding agent config
  fields" before touching the struct.
- **Auto-import set diverges from `tc.Imports`.** Make
  `defaultBindings` immutable and computed once per city load;
  pass by value into pack loaders.
- **City file path unknown at validate time in test fixtures.**
  `describeSource` must handle empty `cityFile` (fall back to
  `<inline>` with no path suffix).
- **Migration-link path inconsistency.** This plan's parent body
  (`ga-tpfc` §7) cites `docs/guides/pack-v1-to-v2.mdx`, but
  `ga-9ogb` and `ga-6wrr` agree on `docs/packv2/migration.mdx`.
  Builder should emit `docs/packv2/migration.mdx` (matches
  `ga-6wrr`'s actual deliverable) and flag the discrepancy in the
  PR description so the architect can decide whether to update the
  `ga-tpfc` body.

## Out of scope (explicit)

- Filesystem stat in `ValidateAgents`. Validator stays pure-data.
- A `SourceProvenance` interface (one consumer; revisit if a second
  appears).
- Exposing `agent.source` via TOML (`toml:"-" json:"-"`).
- A `<system-pack: …>` descriptor variant — rejected; "system" is
  a deployment fact, not a configuration fact.

## Validation gates

- `go test ./...` green.
- `go vet ./...` clean.
- `TestAgentFieldSync` lists `source`.
- Manual smoke: city with both a user pack `mayor` and the
  auto-imported `gastown` system pack `mayor`; `gc start` produces
  an error pointing at both files.
- One `bd remember` entry from the builder when this lands so
  future maintainers learn the format from `bd prime`, not from
  archaeology.
