# Plan: Pack v1 → v2 migration guide (`ga-6wrr` family)

> Owner: `gascity/pm-1` · Created: 2026-04-30
> Source: architecture decision `ga-6wrr` (closed)
> Designer addendum: in `ga-6wrr.1` notes
> Sibling plan (cross-link target): `pack-v1-v2-collision-detection`

## Why this work exists

Pack v2 splits each agent's definition out of `pack.toml`'s flat
`[[agent]]` list into a per-agent directory tree
(`agents/<name>/agent.toml` + `prompt.template.md`). Maintainers
upgrading from v1 need a single page that walks them through the
mechanical edit, the auto-import semantics of system packs, and
the `fallback = true` resolution order. The migration error from
`ga-9ogb` (`pack-v1-v2-collision-detection`) needs a stable URL to
point at.

## Goal

A pack maintainer with a v1 pack on disk can complete the
migration in under 10 minutes by following one Mintlify page.
The page is the canonical reference for v2 layout, auto-import
list, and fallback semantics — referenced from godoc, CLI errors,
and existing migration docs.

## Work breakdown

| Bead         | Title                                | Priority | Routes to | Gate           |
|--------------|--------------------------------------|----------|-----------|----------------|
| `ga-6wrr.1`  | Author pack v1 → v2 migration guide (Mintlify doc) | P2       | builder   | ready-to-build |

Single docs deliverable. Architect+designer have specified:

- The page slug (`docs/packv2/migration.mdx`), title, frontmatter,
  and Mintlify component palette.
- The 9-section outline with per-section word budgets summing to
  1100–1500 words within the FR's 800–1500 budget.
- Code-block style guide (language tags, `${PACK_NAME}`-shape
  placeholders, copy-pasteability rule).
- Cross-link strategy for godoc, CLI error, and sibling docs.
- The accessibility audit (WCAG 2.1 AA pass; one open builder
  item — mobile-width table verification).

No further PM decomposition is needed; the bead body IS the
authoring brief.

## Coordination

`ga-6wrr.1` and `ga-9ogb.1` (`pack-v1-v2-collision-detection`)
**share a URL contract**. Once the migration page is published at
`docs/packv2/migration.mdx`, the migration error in
`formatDuplicateAgentError` (built by `ga-9ogb.1`) must reference
its absolute URL.

The builder for whichever lands second should:

1. Confirm the URL of the migration page (or the URL the error
   message uses) is the same.
2. Patch the other side if the published URL differs from what
   was assumed.

The migration-link path discrepancy in `duplicate-name-error-source-paths.md`'s plan
(which cites `docs/guides/pack-v1-to-v2.mdx`) resolves here in
favor of `docs/packv2/migration.mdx` — the path the architect and
designer agreed on for `ga-6wrr.1`. `ga-tpfc.1`'s builder should
emit `docs/packv2/migration.mdx` and flag the discrepancy in
their PR description.

## Routing rationale

`source:actual-architect`, `source:actual-designer`,
`source:actual-planner` already on the bead. Routed to **builder**
with `ready-to-build` — the architect made the architectural
decisions a writer cannot; the designer planned the writing pass;
the builder authors the prose.

`kind:docs` label preserved so docs-only filters work.

## PM decisions on designer's open questions

1. **Keep both pages.** `docs/guides/migrating-to-pack-vnext.md`
   stays as the broader walkthrough; `docs/packv2/migration.mdx`
   is the focused procedural front door for the v1→v2 split. Add
   a "See also" link in each direction.
2. **`.mdx` extension** as the architect specified. If the docs
   build system rejects `.mdx` (favors `.md`), builder switches
   without impacting the structure or content.
3. **Navigation: Option A** — add the new page to the existing
   "Guides" group between `migrating-to-pack-vnext` and
   `shareable-packs`. Reconsider Option B (dedicated PackV2 group)
   only if `docs/packv2/` accumulates ≥3 user-facing pages.
4. **`<CodeGroup>` widget for the v1/v2 diff** — use if Mintlify
   supports it, fall back to two consecutive blocks with `# v1` /
   `# v2` comment headers if not.
5. **Worked-example pin in frontmatter
   (`worked_example_pinned_to: "gastown layout v0.15.0"`)** —
   keep as advisory annotation. No formal versioning convention
   yet; this is the first such pin and we'll see if it needs to
   become a project pattern.

## Acceptance criteria (rolled up)

1. **Page renders.** `mint.sh dev` shows
   `localhost:3000/packv2/migration` with all `<Note>`,
   `<Warning>`, code blocks rendered correctly.
2. **Word budget.** Prose between 800 and 1500 words excluding
   code blocks.
3. **Migration time target.** A maintainer using the guide
   (smoke test: PM or designer dry-runs against a scratch v1
   pack) completes the v1→v2 transformation in under 10 minutes.
4. **Code blocks compile / run.** Every shell command and TOML
   block in the guide is copy-pasteable into an operator's
   terminal or editor; fenced with the correct language tag.
5. **Auto-import table matches code.** The §4 auto-imports table
   matches `internal/config/config.go:2676-2682` `Imports` map
   exactly. Future map edits trip a CONTRIBUTING note.
6. **Fallback prose matches code.** §5's `fallback = true` prose
   matches `internal/config/pack.go:860-923`
   `resolveFallbackAgents` semantics; a code excerpt is included
   inline as a pin.
7. **Godoc link added.** `internal/config/config.go:1701-1704`
   `Fallback` godoc references the page URL.
8. **Sibling docs cross-linked.**
   `docs/guides/migrating-to-pack-vnext.md` adds a "See also"
   link to the new page; the new page reciprocates.
9. **Nav entry added.** `docs/docs.json` includes the page in
   the "Guides" group between `migrating-to-pack-vnext` and
   `shareable-packs`.
10. **`make check-docs` clean.** Zero broken links.

## Risks and unknowns

- **Imports map drifts between code and doc.** Mitigation:
  CONTRIBUTING note enforces consistency. Future lint is out of
  scope.
- **Fallback semantics get reworded ambiguously.** Mitigation:
  the §5 prose is pinned to a code excerpt of
  `resolveFallbackAgents`'s signature with a line reference.
- **Mintlify config breaks** when adding the nav entry.
  Mitigation: builder runs `mint.sh dev` locally before merging.
- **`gastown` migration example bitrots** if the system pack
  evolves. Mitigation: frontmatter
  `worked_example_pinned_to: "gastown layout v0.15.0"` makes the
  staleness visible to the next maintainer.

## Out of scope (explicit)

- Migrating the `gastown` system pack itself — it's a worked
  example, not a code change.
- Lint to enforce `Imports` map ↔ doc consistency — future work.
- Translations to other languages — English-only.
- Mobile-width table verification at 375px — flagged as a builder
  task during the `mint dev` audit, not a separate PR.

## Validation gates

- `mint.sh dev` renders the page without warnings.
- `make check-docs` passes (zero broken links).
- Manual smoke: PM or designer dry-runs against a scratch v1 pack;
  walk takes ≤ 10 minutes.
- Word count per section sums within the FR's 800–1500 budget.
- Heading hierarchy: H1 → H2 → H3 only, no skipped levels.
- One `bd remember` entry from the builder when this lands so
  future maintainers know to keep the auto-import table in sync
  with the `Imports` map.
