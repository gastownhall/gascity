# Plan: `gc start` troubleshooting walkthrough page (`ga-x5v5` family)

> Owner: `gascity/pm-1` · Created: 2026-05-01
> Source: design `ga-x5v5` (FR-X2 from umbrella PRD `ga-r8hs`)
> Designer addendum: in `ga-x5v5` notes (verbatim `.mdx` contract,
> URL table, accessibility audit)
> Sibling plans:
> - `pack-v1-to-v2-migration-guide` (`ga-6wrr.1`) — cross-link target
> - `gc-start-warning-dedup-fatal-clarity` (`ga-q0bf.1`) — URL contract
>   consumer

## Why this work exists

The seven per-bug fixes in the 1.1.0 selfhost-UX cluster ship with
better error messages, FATAL-line clarity, and a published pack v2
migration guide. What is still missing is the single page an operator
lands on after seeing the FATAL line — the symptom-to-resolution
catalogue covering all seven canonical first-start failures.

The umbrella PRD (`ga-r8hs`) flagged this as FR-X2 with NFR-X2 of
"time-to-resolution after landing on the page < 5 min p95". The
designer (`ga-x5v5` notes) has produced a verbatim `.mdx` contract,
two reference render PNGs (terminal mockup + page wireframe), a
WCAG 2.1 AA accessibility audit, and an explicit URL contract that
the FATAL line emitted by `ga-q0bf.1` will reference.

## Goal

A `gc start` failure ends in a `FATAL:` line whose URL points at a
published Mintlify page. The operator finds their symptom in the
quick-lookup table, expands the matching section, and resolves the
failure in under five minutes p95.

## Work breakdown

| Bead         | Title                                           | Priority | Routes to | Gate           |
|--------------|-------------------------------------------------|----------|-----------|----------------|
| `ga-x5v5.1`  | Author `gc start` troubleshooting walkthrough page (Mintlify doc) | P2 | builder | ready-to-build |
| `ga-x5v5.2`  | Cross-link walkthrough into migration guide and pin `ga-q0bf.1` URL constants | P2 | builder | ready-to-build |

Two beads. The primary (`ga-x5v5.1`) is a fully self-contained docs
deliverable: it ships with no dependencies on the in-flight siblings.
The coordination bead (`ga-x5v5.2`) handles the two cross-cuts that
require sibling work to land first: a one-line cross-link in the
migration guide (FR-6) and a refactor of the FATAL-line URL strings
emitted by `ga-q0bf.1` into a single constants file (FR-7).

The split is taken because both `ga-6wrr.1` and `ga-q0bf.1` are still
ready-to-build at decomposition time. Splitting keeps the primary
unblocked and makes the cross-cut dependencies explicit in the bead
graph rather than burying them as "if-then" acceptance criteria.

## Routing rationale

The design carries `source:actual-designer` and `source:actual-planner`.
Both children inherit those plus `source:actual-pm`. Both route to
**builder** with `ready-to-build` — the architect made the
architectural call (page location, nav placement, anchor scheme), the
designer wrote the verbatim `.mdx` source and the URL contract, the
builder authors the prose and applies the patches.

The `kind:docs` label is preserved on `ga-x5v5.1`. `ga-x5v5.2` carries
both `kind:docs` and `kind:refactor` because it covers a Go constants
file refactor as well as a docs cross-link.

## PM decisions on designer's open questions

The designer's §2 already resolves the four open questions the PRD
raised. PM has nothing to add for those. The decisions left to PM:

1. **Single-bead vs split.** Split. See "Work breakdown" rationale
   above.
2. **FR-7 ownership (URL constants file).** The walkthrough's
   coordination bead (`ga-x5v5.2`) owns the refactor, applied AFTER
   `ga-q0bf.1` lands. `ga-q0bf.1`'s builder may inline the URL
   strings from §8 of the design when they ship; the coordination
   bead extracts them into `internal/logutil/walkthrough_urls.go` so
   future renames touch one place. To prevent the inline strings from
   drifting from the design, the URL contract is appended to
   `ga-q0bf.1`'s notes as a soft contract.
3. **Coordination bead vs follow-up issue.** A second bead (not a
   follow-up issue) so the dependency on `ga-6wrr.1` and `ga-q0bf.1`
   is encoded in `bd dep add` and the bead graph stays readable.

## Acceptance criteria (rolled up)

### Primary: `ga-x5v5.1`

Covers the design's FR-1..5, FR-8..10:

1. **Page renders.** `mint.sh dev` shows
   `localhost:3000/troubleshooting/gc-start-walkthrough` with all
   `<Note>`, `<Tip>`, `<CodeGroup>`, `<AccordionGroup>` (or the
   rendered equivalent if any of those component names need to
   change for compatibility) rendered correctly.
2. **Page contents match the design's §4 verbatim contract.** The
   seven sections appear in the order the design specifies, with
   the anchor IDs from the design's §2 anchor table. Every code
   block is copy-pasteable.
3. **Image embedded.** `gc-start-fatal.png` (rendered from the
   designer's `term-fatal.svg` scratch source) lives at
   `docs/images/troubleshooting/gc-start-fatal.png` and renders on
   both light and dark Mintlify themes. Alt text matches the design's
   §4 verbatim.
4. **Sidebar updated.** `docs/docs.json`'s "Troubleshooting" group
   lists `troubleshooting/gc-start-walkthrough` first, then
   `troubleshooting/dolt-bloat-recovery`.
5. **Cross-link callout added.** `docs/getting-started/troubleshooting.md`
   gains a near-the-top callout linking to
   `/troubleshooting/gc-start-walkthrough` for first-start runtime
   failures. The existing install content is unchanged.
6. **Image alt text** carries both visible-content summary and
   accessibility purpose per the design's §4.
7. **Anchors are stable.** The seven anchors match the design's §2
   table: `#bd-op-init-timeout`, `#pack-schema-mismatch`,
   `#duplicate-name`, `#unknown-field-agent-pool`,
   `#rig-path-required`, `#template-not-found`,
   `#duplicate-identity`. No `#section-1` style anchors.
8. **Word budget.** Prose between 800 and 1500 words excluding code
   blocks (matches the design's NFR-1).
9. **`make check-docs` clean.** Zero broken links.

### Coordination: `ga-x5v5.2`

Covers the design's FR-6, FR-7:

1. **Migration-guide cross-link added.** `docs/packv2/migration.mdx`
   (shipped by `ga-6wrr.1`) gains a final-section "Hit a different
   `gc start` error?" link to
   `/troubleshooting/gc-start-walkthrough`. One line; surrounding
   prose unchanged.
2. **URL constants file extracted.** `internal/logutil/walkthrough_urls.go`
   exists, exports a single map (or eight named constants) keyed by
   failure mode, with values matching the design's §8 URL table
   verbatim.
3. **`ga-q0bf.1`'s emission code consumes the constants.** Whatever
   call-site `ga-q0bf.1`'s builder used to attach the per-symptom URL
   to the FATAL line is updated to read from the new constants file.
   No URL string is hardcoded outside `walkthrough_urls.go`.
4. **Test pinning the URL strings.** A unit test in
   `internal/logutil/walkthrough_urls_test.go` asserts that each
   constant matches the design's §8 table. Future renames trip the
   test.
5. **`make check-docs` clean** after the migration-guide patch.
6. **`go test ./...` and `go vet ./...` clean** after the constants
   refactor.

## Side action: pin `ga-q0bf.1`'s URL contract

To prevent inline URLs in `ga-q0bf.1` from drifting from the design's
§8 table before `ga-x5v5.2` runs, this PM appends the §8 URL contract
to `ga-q0bf.1`'s notes as a soft contract. The note instructs that
builder to copy the strings from §8 verbatim and adds a
`relates_to` link to `ga-x5v5.2`'s eventual constants-file refactor.
No work is added to `ga-q0bf.1`'s scope; the URL strings are part of
the design they already need to consume.

## Coordination

- `ga-x5v5.1` ships standalone. It can land before, after, or
  alongside `ga-6wrr.1` and `ga-q0bf.1`; no ordering constraint.
- `ga-x5v5.2` is blocked by `ga-6wrr.1` (need migration.mdx in tree)
  AND `ga-q0bf.1` (need the inline URL emissions to refactor) AND
  `ga-x5v5.1` (need the page anchors live so the cross-link is not
  broken). Encoded via three `bd dep add` edges.
- If `ga-q0bf.1`'s builder ends up writing the constants file
  themselves while shipping that bead, `ga-x5v5.2`'s scope shrinks to
  only the migration-guide cross-link. That is acceptable — the
  test (acceptance #4 above) still applies; it just lands as a no-op
  delta on the constants file.

## Risks and unknowns

- **Mintlify component support varies.** `AccordionGroup`, `CodeGroup`,
  `Note`, `Tip` are all standard but the local docs build may pin
  older versions. Builder runs `./mint.sh dev` first; if a component
  is unavailable, fall back to flat sections + plain code blocks
  without renumbering anchors.
- **Anchor drift between page and FATAL emitter.** Mitigated by the
  side action above (pin the URL strings into `ga-q0bf.1`'s notes)
  plus `ga-x5v5.2`'s pinning test.
- **Image asset path collision.** `docs/images/troubleshooting/` does
  not yet exist. Builder creates it; the PNG name
  (`gc-start-fatal.png`) is unique within that directory.
- **Mintlify nav refactor while we add an entry.** Low probability;
  the `docs.json` "Troubleshooting" group has been single-entry for
  a while. Builder rebases on `main` before merging.
- **Symptom strings evolve as further fixes ship.** The design's
  quick-lookup table absorbs that drift — anchor IDs are semantic
  (cause-based), not error-string-based, so renaming the on-screen
  text does not break URLs. The page's lookup table maps current
  symptom strings to anchors and is the only place that needs
  updates when error text changes.

## Out of scope (explicit)

- A `gc stop` walkthrough page — separate PRD if the symptoms cluster
  warrants it.
- An auto-fix command (`gc fix --packs`, `gc fix --orders`) — separate
  PRD.
- Translations to other languages — English-only.
- `docs/troubleshooting/index.mdx` — not needed while the group has
  two entries; reconsider if the group grows past five.
- Tone / copy edits to `dolt-bloat-recovery.md` — leave existing page
  alone.
- Search-engine SEO tuning beyond Mintlify defaults.

## Validation gates

- `./mint.sh dev` renders the walkthrough page without warnings; the
  embedded PNG renders on light + dark themes; the seven anchors
  resolve.
- `make check-docs` passes (zero broken links) on both children.
- `go test ./...` and `go vet ./...` clean on `ga-x5v5.2`.
- Manual smoke (during `ga-x5v5.1` review): a maintainer following
  the page from a `FATAL:` URL resolves their failure in under
  five minutes (NFR-X2).
- The seven anchor IDs in `ga-x5v5.1`'s rendered page match the §2
  table verbatim — verified by clicking each lookup-table entry and
  confirming the section scrolls into view.
- One `bd remember` entry from each builder when each lands so future
  maintainers learn the URL-contract pattern (`ga-x5v5.2`) and the
  per-runbook page convention (`ga-x5v5.1`) from `bd prime`, not from
  archaeology.
