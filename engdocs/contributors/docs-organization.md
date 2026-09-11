---
title: Docs Organization
description: How docs/ is structured, named, registered, generated, and gated — the contributor guide to the published documentation tree.
---

`docs/` is the source tree for the published Mintlify site
(docs.gascityhall.com). `engdocs/` is GitHub-only contributor material —
architecture, design, and contributor guides — and is never published.
Normative **user-facing** specifications (pack and formula specs) live under
`docs/reference/specs/` and ship to readers; **internal** architecture
invariants and subsystem docs live under `engdocs/architecture/`. There is no
repo-root `specs/` tree. This page covers `docs/`: which section a page
belongs in, how to name it, how to register it, which pages are generated,
and which gates must stay green.

## Section Taxonomy

The sections follow a Diátaxis-ish split — learn, think, fix, look up.
One page, one purpose: a page that tries to teach and specify at once does
neither; split it and cross-link.

| Nav group | Directory | Purpose |
|---|---|---|
| Getting Started | `docs/getting-started/` (+ `docs/index.mdx`) | Installation, quickstart, how Gas City works (the six-primitive mental model), migration from Gas Town |
| Tutorials | `docs/tutorials/` | Learn by doing. Numbered, sequential, every command runnable |
| Guides | `docs/guides/` | How to think about a subsystem (`understanding-*`) and how to accomplish workflows |
| Troubleshooting | `docs/troubleshooting/` + `docs/runbooks/` | Fix things: diagnosis walkthroughs and operational runbooks |
| Reference | `docs/reference/` | Look things up: exact commands, fields, contracts |

The Reference nav group is divided into three named sub-sections, each
led by an `index.md` titled **Overview** that tables its section's pages.
Two sub-folders (`specs/`, `internal/`) carry the latter two; `schema/`
surfaces as a single page inside the first:

| Nav sub-section | Sub-folder | What goes there |
|---|---|---|
| Reference | `docs/reference/` root | The flat lookup pages (CLI, config, API, events, schemas, providers, trust boundaries, system packs, the Gas Town command map), led by `reference/index.md` |
| Specifications | `docs/reference/specs/` | Authoritative specifications (`pack-spec.md`, `formula-spec-v1.md`, `formula-spec-v2.md`), led by `specs/index.md`. Specs ARE reference material: they live under Reference, keep their spec file names and titles, and new specs land here |
| Internal | `docs/reference/internal/` | Internals-grade reference (`beads-topology.md`), led by `internal/index.md` — implementation topology users need for operations and debugging, but not a public contract |

`docs/reference/schema/` holds generated, downloadable schema artifacts —
OpenAPI, `gc events` JSONL schema, city/pack JSON Schema — plus the
hand-written `index.md` that links them; no other hand-written pages go
there. When you add a page to a sub-section, add a row to that section's
Overview page as well as the `docs.json` nav entry.

## Naming And File Conventions

- Tutorials: `NN-name.md` (`05-formulas.md`); frontmatter
  `title: Tutorial NN - Name` plus `sidebarTitle: NN - Name`.
- Specs: `<topic>-spec[-vN].md` (`pack-spec.md`, `formula-spec-v2.md`).
  Versioned contracts get one file per version and both stay live (v1 and
  v2 are peer contracts — see Vocabulary below). Titles name the product
  and version: "Gas City Formula Specification — v2 (formula_compiler
  2.0)", "Gas City 1.0 Pack System (PackV2)".
- Mental-model guides: `understanding-<topic>.md` (`understanding-packs.md`,
  `understanding-formulas.md`). Task guides are named for the task
  (`shareable-packs.md`, `using-json-from-gc.md`).
- Every page: frontmatter `title` + `description`, both required.
- **No body H1, anywhere in docs/.** Mintlify renders the frontmatter
  title as the page header; a `# Heading` in the body doubles it. Start
  bodies at `##`. For generated pages this is pinned by tests in
  `internal/docgen/` (`cli_test.go`, `markdown_test.go`); hand-written
  pages follow the same rule.
- `.md` is the default; `.mdx` only when the page needs MDX
  (`index.mdx`, `troubleshooting/gc-start-walkthrough.mdx`).
- Links inside docs/ pages are root-relative without extension
  (`/reference/specs/pack-spec`). Links from engdocs/ into docs/ are
  relative paths with the `.md` extension; `test/docsync` enforces the
  per-tree style.

## Registering A Page

Every change to the page set touches `docs/docs.json`:

1. **New page** — add it to the right nav group (or sub-group). Two
   docsync tests enforce exact two-way correspondence:
   `TestMintNavigationPagesExist` (every nav entry points at a real file)
   and `TestEveryDocsPageIsPublished` (every md/mdx under docs/ appears in
   the nav; only `docs/README.md` is exempt).
2. **Moved or deleted page** — add a `redirects` entry mapping the old URL
   to the new one, and retarget any existing redirect that pointed at the
   old URL so no chains form.
3. **Index rows** — the hand-maintained index pages get an entry:
   `docs/reference/index.md` for reference pages, plus the group index
   pages (`tutorials/index.md` table, `guides/index.md` list,
   `reference/schema/index.md` for new schema artifacts).

## Generated Pages

`docs/reference/cli.md`, `docs/reference/config.md`, and everything under
`docs/reference/schema/` except `index.md` are generated. Never edit the
markdown — edit the Go source and regenerate:

| Artifact | Source to edit | Generator |
|---|---|---|
| `reference/cli.md` | cobra `Use`/`Short`/`Long`/`Example` strings in `cmd/gc/` | `go run ./cmd/genschema` (shells out to the hidden `gc gen-doc`) |
| `reference/config.md`, `reference/schema/city-schema.*`, `reference/schema/pack-schema.*` | doc comments on `internal/config` structs; renderers in `internal/docgen/` | `go run ./cmd/genschema` |
| `reference/schema/openapi.*`, `reference/schema/events.*` | Huma registrations in `internal/api/` (see [Huma Usage Notes](huma-usage.md)) | `go run ./cmd/genspec` |

Notes:

- `.githooks/pre-commit` regenerates and stages all of these on any staged
  Go change, so a clean commit cannot leave them stale. Freshness tests
  back it up: `TestSchemaFreshness` and `TestCLIDocsFreshness` (docs
  artifacts), `TestOpenAPISpecInSync` (spec).
- jsonschema **type** doc comments render first-sentence-only
  (invopop/jsonschema applies `go/doc.Synopsis` to type comments by
  default); **field** comments render in full. Put the load-bearing
  sentence first on type comments.
- `reference/api.md` and `reference/events.md` are hand-written contract
  pages that link the generated artifacts via GitHub raw URLs;
  `TestSchemaDownloadLinksUseGitHubRaw` checks the URL form and that each
  linked artifact is committed.

## Media: Prose, Code, and Images

A page teaches with three media, and the best pages use all three
deliberately: **prose** for the why and the judgment, **code/CLI** for the
exact, copy-pasteable truth (validated against the binary — see Gates), and
**images** for structure and flow that words describe poorly. Treat them as
complementary, not interchangeable. If a concept is a shape — a graph, a
tree, a fan-out, a lifecycle, a layering — prose is the wrong primary medium
for it, and a wall of prose or ASCII art is a signal that a diagram is owed.

**When a diagram earns its place.** Add one when at least one is true:

- The thing is inherently spatial or topological: bead graphs, the v1
  molecule tree vs the v2 flat workflow, drain fan-out into unit convoys,
  convoy `tracks` membership, the city/controller/store structure.
- A reader must hold several moving parts at once to follow the prose
  (a multi-actor flow, a multi-stage pipeline, a state lifecycle).
- The page is already carrying the picture as ASCII art or a long
  "first… then… which then…" paragraph. That is a diagram in disguise;
  replace it (or pair it) with a real one.

**When NOT to.** Skip the diagram when prose or a code block already conveys
it cleanly: a two-step sequence, a field table, a single command and its
output. A decorative diagram that restates a sentence adds maintenance cost
(it can drift from the code) without adding understanding. Reference pages
that are exhaustive field tables generally do not need diagrams; concept,
architecture, and "how the pieces fit" pages usually do.

**Why this matters here specifically.** Gas City's load-bearing concepts are
graph-shaped — formulas compile to bead graphs; convoys *are* graphs of
related work. Those are exactly the concepts that prose serves worst, so the
concept/architecture pages and the formula specs are where diagrams pay off
most. A diagram that drifts from the code is worse than none, so keep each
one anchored to a real, current shape (the same standard as a code example).

**How — the toolchain (do not improvise a medium).** Excalidraw is the
adopted standard; the decision and the rationale are in
[Adopt Excalidraw](../design/excalidraw-diagrams.md), and the end-to-end
mechanics (MCP authoring, the shared pastel palette, `make
diagrams-excalidraw`, committing source + rendered SVG) are in
[Excalidraw Setup](excalidraw-setup.md). Use the shared palette so recurring
roles (entry, compute, persistence, success, error) read consistently across
the corpus. Mermaid inline remains an acceptable fallback only for simple or
sequence diagrams where the no-build-step advantage outweighs the aesthetic.
Both the `.excalidraw` source and the rendered `.svg` are committed; pages
reference the SVG (`/diagrams/excalidraw-rendered/<name>.svg`) with
descriptive alt text. Screenshots are for UI/terminal output that genuinely
needs to be shown; keep them current.

## Gates

| Gate | When it runs | What it checks |
|---|---|---|
| `make check-docs` (`go test ./test/docsync`) | per-PR CI; pre-commit on staged docs | Tutorial command/txtar sync (today: tutorial 01 vs `cmd/gc/testdata/01-hello-gas-city.txtar`); schema download-link integrity; generated-page freshness; nav↔file correspondence; file-level link resolution across all doc trees (docs, engdocs, contrib, release-gates); bans on known-stale references |
| Tutorial goldens (`make test-tutorial-goldens`; `//go:build acceptance_c`; `test/acceptance/tutorial_goldens/`) | RC-gate CI (sharded); before each release | Executes the tutorial pages end-to-end with real inference. `manifests_test.go` pins the exact command sequence of every tutorial page; the per-tutorial tests assert outcomes. Tutorial edits and golden assertions move in lockstep — change a tutorial command and you must update the manifest and the matching `tutorialNN_test.go` |
| Anchor hygiene | manual / review | docsync resolves links at file level only (fragments are stripped) — when citing a spec section anchor (`#3-runtime`), verify the heading exists |
| Local preview | manual | `./mint.sh dev` from the repo root. Mintlify requires Node ≤ 24; the wrapper falls back to Homebrew `node@22` when the ambient Node is too new |

## Vocabulary

Formula naming rules, applied corpus-wide:

- The two formula contracts are **formulas v1** and **formulas v2** ("v1"
  / "v2" after first use). They are peer contracts and v1 is the default —
  never call v1 "legacy".
- `graph.v2` appears only as the deprecated contract-key literal
  (`contract = "graph.v2"`) or inside quoted CLI output, never as the name
  of the v2 system.
- "Deprecated" is reserved for surfaces the specs enumerate as deprecated
  (the `contract = "graph.v2"` opt-in, the `{{issue}}` alias, the
  `.formula.toml` infix, `gc.output_json_required`, and similar). It never
  applies to the v1 contract itself.

Spec register, set by `pack-spec.md` and followed by both formula specs:

- A header table directly under the frontmatter: Status / Last verified /
  contract or schema version / Primary implementation / User-facing guide
  (formula specs add a Tutorial row).
- Lowercase normative keywords — "must", "must not", "should", "may" —
  declared normative in a leading paragraph (not RFC-2119 capitals).
- §-numbered H2 sections starting at `## 0.` with `N.M` subsections, so
  other pages can cite "§3.1".
- An "Accepted But Inert" section for constructs the parser accepts but no
  runtime component consumes. Specs are normative for **implemented**
  behavior and say so honestly instead of describing intent.

## The Full-Maturity Pattern

A topic at full documentation maturity has all three registers plus
reference, cross-linked:

- **Tutorial** — learn by doing: `tutorials/05-formulas.md`.
- **Guide** — how to think: `guides/understanding-formulas.md`,
  `guides/understanding-packs.md` (plus task guides such as
  `guides/shareable-packs.md`).
- **Spec(s)** — the normative contract, under Reference:
  `reference/specs/formula-spec-v1.md` / `formula-spec-v2.md`,
  `reference/specs/pack-spec.md`.
- **Generated reference** rounds it out: `reference/cli.md`,
  `reference/config.md`, the schema artifacts.

Packs and formulas are the worked examples of this shape. The spec header
table names its guide and tutorial; the guide links the specs; the
tutorial points onward. When documenting a new topic, aim for this shape —
and until a topic earns all the registers, land its page in the single
section matching its purpose rather than writing one hybrid page.
