---
name: gascity-docs
description: >-
  Project conventions for writing, editing, restructuring, or reviewing the Gas
  City user documentation — the Mintlify site under docs/. Use this whenever you
  touch anything in docs/ (pages, tutorials, guides, reference, concept pages,
  diagrams, navigation) or write/edit prose about Gas City, even when the request
  is just "fix the docs", "write a docs page", "the docs are wrong/confusing",
  "rename X across the docs", or an edit to a file under docs/. It defines the
  canonical six-primitive model, required terminology (orchestrator not
  controller, platform not SDK, formulas v2 as the value), the prose / emphasis /
  diagram conventions, the information architecture, the rule that generated docs
  are edited at their source, and the gates to run before docs work is done.
---

# Writing Gas City docs

This is the house style for `docs/` — Gas City's public Mintlify site. The goal
is docs that motivate before they jargon, say the same thing the same way
everywhere, show concepts as pictures and snippets instead of walls of prose, and
never drift from the code. Apply it to any page you create, edit, or review.

The audience of `docs/` is a **user/operator** of Gas City. Contributor-facing
material lives in `engdocs/` and `AGENTS.md` and follows different rules (it may
describe the implementation literally). When this skill says "the docs," it means
`docs/`.

## 1. The canonical model

Gas City is built from **six primitives**. This is the user-facing model; teach it
consistently and link to it rather than re-explaining it.

| Primitive | Role | Derived terms that live *under* it (not co-equal) |
|---|---|---|
| **Agent** | WHO does the work | session, provider, pool |
| **Bead** | WHAT the work is | convoy, dependencies |
| **Formula** | HOW the work is done | run, sling, order (Health Patrol is one kind of order) |
| **Rig** | WHERE the work happens | repo, bead namespace, scope |
| **Pack** | what CONFIGURES the system | the City is the local (root) pack; imports pull in shared packs |
| **Event** | how you OBSERVE the system | (the "bus" is delivery machinery) |

The authoritative page is **`docs/getting-started/how-gas-city-works.md`**. Other
pages link to it; they do not re-derive the whole model.

Relationship backbone (state it where it helps, don't repeat it everywhere): packs
declare agents/formulas/orders → the local pack is the City → a Formula operates
over a convoy of Beads, fanning work to Agents that execute in a Rig → an Order
automates *when* a formula runs → Events fire so humans and agents can observe.

## 2. Required terminology

These are settled. Use the left column; never the right (except as noted).

| Use this | Not this | Notes |
|---|---|---|
| **orchestrator** | "controller" | The conceptual component that executes formulas. The *implementation* is still named `controller` and does more than orchestration, so keep "controller" only in literal program output (`Controller:` in `gc status`), the `controller` JSON field, and config keys — never in concept prose. |
| **platform** ("the platform for building software factories") | "SDK" | Gas City's positioning. Keep "SDK" only in literal output (the `Welcome to Gas City SDK!` banner) and when it means a *different* SDK ("provider SDKs", Claude Code's SDK). |
| **formulas v2** is the value | "graph.v2"; "legacy" | v2 = the orchestrator running a formula graph across many agents, out of your session. v1 is a supported single-agent, in-session **peer** — never call it "legacy". `graph.v2` only ever appears as the deprecated `contract` literal. |
| a **formula** is the *how* (a method over a convoy of beads) | "a formula is the work" | Beads are the work; the formula is the method applied over them. Never conflate formula with bead/convoy. |
| **Events are fired** so humans/agents can observe | "events observe"; "observation substrate" | Events are the outbound notification, not an active observer. |
| the **City is the local (root) pack** | (silence) | State this wherever pack and city both appear. |

**molecule / wisp** are the v1 *materialization* — an implementation detail, not a
user concept. They may appear only as a parenthetical naming the v1 mechanism,
never as a heading, a decision axis, or a list item the reader chooses among. The
v1/v2 formula **spec** files are the one exception (they normatively define the v1
materialization).

**No Gas Town design-philosophy jargon** in `docs/` prose: ZFC, NDI, GUPP, MEOW,
"Bitter Lesson". Keep the plain principle, drop the acronym. (These live in
`AGENTS.md` / `engdocs/` for contributors.)

**Role names:** use neutral examples (planner, reviewer, worker) on generic pages.
Keep the Gas Town role names (deacon, polecat, crew, witness, reaper) only on
Gas-Town-specific pages (`coming-from-gastown`, `gastown-*`). **`mayor` stays
everywhere.**

When you rename a concept across the corpus, see
[references/terminology.md](references/terminology.md) for the prose-vs-literal
discipline (rename the concept in prose; preserve literal program output, JSON
fields, config keys, and generated files).

## 3. Content stance

- **Docs are not the project's history.** No code archaeology on user pages — no
  `internal/*` package paths, no "the X subsystem was removed in the Y migration",
  no "the former Z". That belongs in `engdocs/`/`AGENTS.md`. Migration framing is
  allowed only on an explicit migration page (`coming-from-gastown`).
- **Motivate before you mechanize.** Lead a page with the problem it solves, then
  the solution, then the mechanics. Don't open on vocabulary.
- **Lead with the value.** Gas City's value is v2 orchestration — many agents,
  out of session — framed as a *software factory* that gets production quality at
  machine speed. Never frame orchestration as a "thin layer."
- **One concrete image beats three abstract sentences.** Where you assert a
  capability, show a tangible instance of it.

## 4. Information architecture

The nav sections are **Getting Started, Tutorials, Guides, Troubleshooting,
Reference** (the contributor map lives in `engdocs/contributors/docs-organization.md`).

- **Every section has an Overview page** (frontmatter `title: Overview`) that
  introduces the section in a sentence or two and then lists every page beneath it
  with a one-line, accurate summary and a link. The home page (`docs/index.mdx`)
  is the Getting Started Overview.
- One page, one purpose. A page that tries to teach *and* specify does neither —
  split it and cross-link.
- The **repository/codebase map belongs in the README**, not in `docs/`.
- Concept material is unified on `how-gas-city-works`; don't reintroduce a separate
  "concepts" section.

## 5. Prose doctrine — cut words, sharpen points

Most bloat is information stored in the wrong medium. Move it to a cheaper carrier,
then delete what isn't pulling weight. Every page must **stand alone** — a reader
landing cold needs no previous page.

**Convert** (move load off prose):
- A relationship or sequence → a **diagram** (reuse an existing one or author a new
  one — see §6).
- A set of parallel options/fields/comparisons → a **table**.
- "you do X, which does Y" narration → an **annotated CLI/TOML snippet** (show the
  artifact, comment it).
- Edge cases and deep mechanics → an `<Accordion>`, or move them to a spec/reference
  page. Keep the 80% on the page.

**Delete** (the load was fake): throat-clearing openers ("In this section we'll…"),
hedge chains ("generally / typically / in most cases"), restatement (said →
explained → summarized: keep the sharpest one), narrating an artifact a snippet or
table already shows, and adjectives standing in for evidence.

**Write for the reader, not about the edit.** No meta-commentary, no asides that
only make sense relative to text you removed, and no negative "this is X, not Y —
go read something else." Use a positive invitation instead ("If you'd like a
step-by-step walkthrough, see the [Tutorials](/tutorials/index)").

**Standalone, not sequential.** Use a consistent example cast across pages (same
city/rig/feature names; `mayor` is a fine recurring name), but never depend on
reading order — give each page a one-line self-contained setup, and dedup the
six-primitive model by *linking* to `how-gas-city-works`, not by re-explaining it.

When you're running a deliberate **simplification pass** over a page or a whole
section — not just applying these moves ad hoc — follow
[references/simplification.md](references/simplification.md): the per-page loop
(measure → convert/delete → verify), and the two guardrails that keep a trim
honest — a **loss-check** (don't drop a fact that lives nowhere else) and a
**fact-check** (don't let trimmed or rewritten prose drift from the code).

## 6. Emphasis and formatting

- **Bold** *names a term*, on first mention only — the thing the reader should
  anchor on. *Italic* marks a *property or contrast* (the `*who/what/how*` role
  words, `*outside your session*`). Keep it to ~1–2 marks per paragraph, never give
  the same phrase both treatments, and never re-emphasize a term already
  introduced. The six-primitive bullet list (one bold term + one italic role word
  each) is the model.
- No body `# H1` — the frontmatter `title` is the H1. Use `##`/`###` in the body.
- Mintlify markdown: root-relative links without the file extension
  (`/tutorials/index`), `<Note>`/`<Tip>`/`<Accordion>` sparingly, valid MDX.

## 7. Diagrams

The project standard is **`engdocs/design/excalidraw-diagrams.md`** — read it
before authoring. In short: author the source, render with **`make
diagrams-excalidraw`** (source `.excalidraw` + rendered `.svg` both get committed),
use the shared pastel palette, and embed from a docs page as
`/diagrams/excalidraw-rendered/<name>.svg` with **descriptive alt text**.

Two non-negotiables learned the hard way:
- **Keep labels short and let them fit** — a two-line "Name / role" beats a long
  sentence crammed in a box. After rendering, **rasterize the SVG and look at it**
  (text overflow and layout problems are invisible in a text diff).
- Prefer reusing an existing rendered diagram over authoring a new one.

## 8. Generated content is edited at its source

Never hand-edit a generated file. In `docs/reference/` these are **`cli.md`,
`config.md`, and `schema/*`** (emitted by `cmd/genschema` from Cobra `Long:`
strings and Go struct doc comments), plus the OpenAPI spec and the dashboard's
generated TS types. To change their wording, edit the Go source and regenerate
(`go run ./cmd/genschema`), then commit source + regenerated output together. A
freshness test (`TestCLIDocsFreshness`) fails if they drift.

## 9. Verify before you call it done

Run the gates in [references/verification.md](references/verification.md). The
durable repo gates are **`make check-docs`** (nav↔file + local markdown links),
**`make diagrams-excalidraw`** (if you touched diagrams), `go run ./cmd/genschema`
(if you touched generated docs), and **`make dashboard-check`** (if you touched
`internal/api/`, the OpenAPI spec, or the dashboard). Beyond the gates: every TOML
fence must parse, every internal link and anchor must resolve, no page is orphaned
from the nav, and no body H1 was introduced. Preview on the live site with `make
docs-dev` (or `./mint.sh dev`) at `localhost:3000`.

When you **move or remove a page**: add a Mintlify redirect in `docs/docs.json`
from the old path, rewrite inbound links (including `engdocs/` links that use the
`../../docs/...` form), and update any nav/IA references.

## 10. Review and commit discipline

- **Author → the user reviews → commit only on explicit approval.** Push nothing
  without a clear go-ahead. This matters most for **diagrams and images**, which
  can't be reviewed in a text diff — render them and show the user before
  committing.
- Group commits by audience for reviewability: contributor (`AGENTS.md` +
  `engdocs/`) separate from user docs (`docs/`).
- Docs-only commits hit a lightweight `docsync` pre-commit gate; a change touching
  Go source escalates to the full fast test suite, which can flake under load — if
  it does, confirm the change is doc/comment-only and the relevant gates pass, and
  disclose that in the commit message.
