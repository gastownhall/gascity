# Running a simplification pass

Read this when you're trimming a page or a whole section — "simplify the
guides", "this page is a wall of text", "continue the trim + pictures pattern",
"cut this down". SKILL.md §5–§7 define the *moves* (convert to a cheaper
carrier, delete dead load, emphasis, diagrams). This file is the *process*: how
to run those moves across real pages without dropping facts or drifting from the
code.

## The goal

Remove **redundancy and unnecessary words**, and make the point *sharper*, not
thinner. **Word count is the output, not the target** — never trim toward a
number. The win is almost never deleting information; it's moving it to a cheaper
carrier (a table, an accordion, a diagram, or a link to the page that already
owns it) and deleting what was only restatement. A reader should land on a leaner
page and understand *more*, faster.

How much a page sheds depends entirely on how much *genuine redundancy* it
carries — not on a target percentage. Measured against `main@head`, the same pass
produced wildly different cuts, and that's correct:

- A page that **restates what a sibling page owns** sheds a lot. A registry
  showcase re-teaching the pack model dropped ~20% by dedup-by-link; a packs
  guide whose "Names" section re-taught import binding dropped a section.
- A page whose length is **earned teaching** (worked examples plus the runtime
  behavior they illustrate) barely moves — forcing it toward a number guts the
  teaching. The formulas guide cut ~1%: remove its real redundancy and stop.
- A page that is **already lean** gets left alone entirely.

Two corollaries that bite if you forget them:

- **Accordions and tables fold content for scannability without deleting words.**
  A page can read dramatically lighter with almost no word-count change. Don't
  measure that work by the word delta, and don't reach for aggressive cuts just
  because the number didn't move.
- **A close trim surfaces stale facts.** Reading every line to decide what's
  redundant is also a fact-check — expect to find model drift the last sweep
  missed (e.g. two guides still claiming built-in packs compose through
  `workspace.includes` after that model was retired). Fix them as you go.

## The per-page loop

1. **Measure.** `wc -w <page>`; note the current count and a target (~70–75% of
   it).
2. **Find opportunities, by carrier.** Read the page and tag each heavy passage
   with the move that fits:
   - **dedup-by-link** — prose that re-teaches what a linked spec/reference/
     concept page already owns. This is the biggest lever in a reference-heavy
     corpus: replace the restatement with one sentence + the link. Keep a fact
     when the page is *applying* the spec to a decision; cut it when the page is
     *re-teaching* the spec.
   - **prose→table** — a set of parallel options, fields, verbs, or comparisons.
   - **prose→accordion** — deep mechanics, power-user detail, "in the wild"
     digressions. Keep the 80% inline; fold the 20%.
   - **add / reuse a diagram** — a relationship asserted in prose two or more
     times with no picture. Reuse an existing rendered diagram before drawing a
     new one (`ls docs/diagrams/excalidraw-rendered/`); author a new one only
     for a genuine relationship none of them covers (§7).
   - **delete** — throat-clearing openers, hedge chains, said-then-explained-
     then-summarized restatement, narration of an artifact a snippet or table
     already shows.
3. **Apply** the moves in the page's own voice. Never reflow prose you are *not*
   simplifying — it buries the real change in the diff.
4. **Loss-check** (below) — did you drop anything important?
5. **Fact-check** (below) — does everything you kept or rewrote still match the
   code?
6. **Gates** (§9) — `make check-docs`, link/anchor/orphan check, every TOML
   fence parses, no body H1; render and eyeball any diagram you added.
7. **Preview, then commit.** Show the user the diff (and rendered diagrams) for
   one page or section at a time, and let them steer before the next batch.

## Loss-check: simplification must not lose facts

Trimming is where real details quietly disappear. After a pass, diff the page
against its pre-trim version (or `origin/main` on a branch) and walk the removed
lines:

> For each removed block: is it an important **fact, command, flag, config key,
> caveat, behavior, or worked example** — and is it **preserved nowhere else**
> (`rg` across `docs/` to check for relocation)?

Most removals are *not* losses. Separate them honestly:

- **Intentional cuts** (leave them gone): terminology migration, hedge/
  restatement deletion, a page or section relocated to a better carrier (with a
  redirect), and deliberately-retired content — e.g. a design doc whose feature
  has shipped.
- **Genuine losses** (restore): a real detail dropped with no home elsewhere.

Restore a genuine loss as a **sharpened clause, not restored bulk** — fold the
missing *why* back into a sentence you kept rather than re-adding the paragraph
you cut. Condensing a three-term distinction into a two-row table is good; if the
table drops *the reason the distinction matters*, put that reason back in the
lead sentence, not the table.

At section scale, run the loss-check as a fan-out: one agent per page or cluster
reads its `git diff origin/main HEAD -- <files>` and flags unpreserved removals,
then a second, independent agent re-checks each flag against the whole corpus to
kill false positives. **Tell the confirm pass to judge *intent*, not just
absence** — a checker that only asks "is it absent from `docs/`?" will re-flag
every intentional cut (the retired design doc, the migrated terminology) as a
loss.

## Fact-check: simplified prose drifts from code

Authoring and trimming both introduce drift — a version number goes stale, a
renamed flag survives in prose, a rewritten sentence overstates the
implementation. Before pushing, verify every **checkable claim** on the touched
pages against the current code (and, for pack claims, the packs): CLI commands /
subcommands / flags, config keys and defaults, environment variables, file and
directory paths, event-type names, error strings, named formulas / orders /
agents / packs, pack source URLs, and numeric defaults.

This is not optional polish. In practice, carefully-authored doc prose is wrong
often enough that a verification pass routinely catches several errors per
rewrite — stale versions, a flag that moved, a "designed" feature that already
shipped. Verify **adversarially**: have a checker try to prove each claim *false*
against a `file:line`, defaulting to "unverified" rather than "fine". For a
single new or rewritten page, one thorough pass; for a corpus sweep, fan out per
cluster with an adversarial confirm to filter false positives. Generated
reference (`cli.md`, `config.md`, `schema/*`) is exempt — it is correct by
regeneration (§8), not by reading, so fact-checking it against its own source is
circular.

## Cadence

One page (or one guide) per batch: simplify → loss-check → fact-check → gates →
preview → commit on approval → next. Resist simplifying a whole section in a
single unreviewed sweep; the per-page checkpoint is what lets the user catch a
cut they disagree with before it compounds across the section.
