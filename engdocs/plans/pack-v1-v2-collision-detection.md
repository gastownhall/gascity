# Plan: Pack v1/v2 layout-collision migration error (`ga-9ogb` family)

> Owner: `gascity/pm-1` · Created: 2026-04-30
> Source: architecture decision `ga-9ogb` (closed)
> Designer addendum: `gascity/designer` (in `ga-9ogb.1` notes)
> Sibling plan (must coordinate): `duplicate-name-error-source-paths`
> Downstream dep (placeholder until landed): `ga-6wrr`
> (`docs/packv2/migration.mdx`)

## Why this work exists

`ValidateAgents` (`internal/config/config.go:2357`) emits an opaque
generic duplicate-name error when v1 inline `[[agent]]` blocks
collide with v2 convention-layout agents. Operators in the middle
of a v1→v2 migration get the same error message as someone with
two genuinely-conflicting v2 agents — but the two remediation
paths are completely different (delete the v1 `[[agent]]` block
vs. rename one of the v2 agents).

Architecture (`ga-9ogb`) chose to **stamp an unexported `layout`
field on each `Agent` at discovery time**, and emit a migration-
guidance error when the layout pair is exactly (V1Inline,
V2Convention). Otherwise keep the existing generic format.

## Goal

When a v1↔v2 layout collision occurs, surface a distinct,
operator-actionable error that points at both source paths and
links to the migration guide. Don't change behavior for any other
collision type.

## Work breakdown

| Bead         | Title                                                                | Priority | Routes to | Gate           |
|--------------|----------------------------------------------------------------------|----------|-----------|----------------|
| `ga-9ogb.1`  | Implement layout-version stamping + migration-guidance error          | P1       | builder   | ready-to-build |

The architect+designer broke this work down to a single coherent
implementation unit covering the `layout` field, discovery-site
stamping (loadPack + DiscoverPackAgents), the migration variant of
the shared `formatDuplicateAgentError` helper, and the layout-pair
matrix tests. The bead body is itself the implementation plan.

## Coordination

`ga-9ogb.1` and `ga-tpfc.1` (`duplicate-name-error-source-paths`
plan) **share a helper function**, `formatDuplicateAgentError`.
Either bead may land first; the second rebases. The shared test
file (per designer notes): a single
`internal/config/duplicate_agent_error_test.go` with table-driven
cases owned by both beads.

`ga-9ogb.1`'s migration-error variant references
`docs/packv2/migration.mdx`, which is `ga-6wrr`'s deliverable.
Until `ga-6wrr` lands, the builder emits the path verbatim
(treating it as a placeholder); a one-line follow-up patch keeps
them in sync if the path slips.

## Routing rationale

Designer addendum is present in the bead notes — covers operator
workflow comparison (today vs. fixed), wording review, the
migration-guide URL decision, accessibility audit, edge cases, and
the validator acceptance checklist. No more design hops needed.
Routed to **builder** with `ready-to-build`.

## Acceptance criteria (rolled up)

- **Bug repro test passes.** v1 `[[agent]] mayor` in user pack +
  v2 `agents/mayor/` in auto-imported system pack → migration
  error fires.
- **Headline pinned.** `agent "X": pack v1/v2 layout collision`
  is byte-stable across the test corpus.
- **Layout-pair matrix coverage.** All cells from architect §7
  have a table-driven test case asserting the correct error
  family fires (generic vs. migration). The two V1↔V2 cells fire
  migration; the rest fire generic.
- **`fallback = true` suppresses the error.**
  Pre-`ValidateAgents` removal of fallback losers is regression-
  tested for the v1+v2 case specifically.
- **Field-sync test updated:** `agent.layout` listed in
  `field_sync_test.go` expected fields.
- **Patch / override / pool deep-copy preserve `layout`.** Same
  propagation assertions as `ga-tpfc.1`'s `source` field.
- **Migration-guide URL emits `docs/packv2/migration.mdx`.**
  Single source of truth; matches `ga-6wrr`'s deliverable.
- **Existing same-version duplicate-name tests stay green.**
  Wording and format unchanged for non-migration cases.
- **No FS access in `ValidateAgents`.** Validator stays pure-data.

Full acceptance checklist is in the bead body's "Acceptance
checklist (for validator)" section.

## Risks and unknowns

- **`layout` cleared by a struct rebuild we miss.** Field-sync
  test plus propagation assertions are the safety net. CLAUDE.md
  flags this as a known gotcha — read the section "Adding agent
  config fields" before touching the struct.
- **`ga-tpfc.1` lands without coordination and clobbers the
  helper.** Mitigation: shared test file pins the format; whoever
  lands second rebases on the helper.
- **Auto-imported pack delivers agents without going through
  `DiscoverPackAgents`.** Round-trip unit test pins the
  invariant.
- **`fallback = true` semantics evolve and break the post-
  fallback invariant.** Pin via dedicated regression test.

## Out of scope (explicit)

- Side-table `map[*Agent]agentLayout`.
- Inferring layout via `Stat` inside the validator.
- `[pack].schema` as the signal.
- Generalising into a `SourceProvenance` interface.
- `pack-conflicts: ignore` config knob.
- Pre-emptive "consider migrating" warning when only v1 is
  present.

## Validation gates

- `go test ./...` green.
- `go vet ./...` clean.
- `TestAgentFieldSync` lists `layout`.
- Layout-pair matrix tests cover all cells from architect §7.
- Manual smoke: city with both v1 `[[agent]] mayor` in user pack
  and v2 `agents/mayor/` in auto-imported system pack; `gc start`
  produces the migration error with both source lines and the
  `To migrate, see: …` line.
- One `bd remember` entry from the builder when this lands.
