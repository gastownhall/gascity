# Plan 0001 — Docs quality improvements

**Status:** in progress (branch `docs/quality-followups-20260710`)
— items 1, 2, 5 implemented on the branch (uncommitted, pending review);
items 3, 4, 6 not started.
**Source:** fresh comparative assessment of the Gas City docs vs the beads docs
(2026-07-10). The gaps below were validated against the live corpus; the
back-ported items come from beads' docs machinery.

## Context

The docs program (PRs #3168, #3461, #3539) gave `docs/` disciplined IA, prose
doctrine, 16 concept diagrams, generated-at-source reference, and CI gates. A
side-by-side assessment against the beads project surfaced the remaining gaps
(items 1–5) and two beads strengths worth adopting (items 4 and 6 draw on them).

**Settled decisions (recorded so they are not re-litigated):**

- **Hosting stays Mintlify.** The Starter tier covers everything the site uses
  today; beads is being ported to Mintlify separately (in the beads repo), so
  both projects converge on the same hosting and authoring conventions.
- **Deferred: versioning.** Wait for 1.0 — a version switcher on a pre-1.0
  product is noise. Mintlify supports it when we need it.
- **Deferred: splitting `understanding-formulas.md`.** The page is dense
  because it is earning its density; revisit only on reader complaints.
- **Deferred: more terminal screenshots.** The diagram-first stance is right;
  add screenshots only where a specific flow demonstrably confuses readers.

## Work items

### 1. FAQ page (do first — cheapest high-value item)

A comparison-first FAQ in Getting Started, modeled on the beads FAQ's
newcomer-conversion framing. Questions grounded in existing pages (link, don't
re-explain): why not just an interactive coding-agent session; why not a bash
loop or CI; relation to Gas Town; do I need tmux/dolt; which coding-agent CLIs
work; do I have to write Go; what survives a crash; license.

- New page `docs/getting-started/faq.md`; add to `docs.json` nav (Getting
  Started group) and to the page list on `docs/index.mdx` (the section
  Overview).
- Every claim fact-checked against the page it links to.
- **Acceptance:** `make check-docs` passes; page renders in `./mint.sh dev`.

### 2. AI-friendliness config (quick win)

Verified live: `https://docs.gascity.com/llms.txt` already serves (~80 pages
indexed), so Mintlify's llms generation is working — that is the AI-friendliness
surface, and it needs no further work.

- **Decision (2026-07-10): no `contextual` menu.** It was added on this
  branch and rejected on review — redundant with `llms.txt` (agents ingest
  the corpus or fetch any page as markdown by URL) and visual clutter on
  every page. Do not re-propose the copy-page/open-in-X menu.
- Fix `logo.href` in `docs.json`: it points at `https://docs.gascityhall.com`,
  which 301-redirects to `https://docs.gascity.com` — use the canonical
  domain. (Done on this branch.)
- **Acceptance:** `docs.json` still validates (check-docs nav tests pass).

### 3. Dashboard screenshot pass (needs Chris in the loop)

`docs/getting-started/dashboard.md` (63 lines) documents a visual surface with
zero images. Run the dashboard against a live city, capture the main views,
embed annotated screenshots. Per the docs skill, images cannot be reviewed in a
text diff — capture, show Chris each one, commit only on approval.

- **Acceptance:** dashboard.md shows the primary views; images approved by
  Chris before commit.

### 4. Troubleshooting expansion (ongoing)

Adopt beads' pattern-coded runbook shape. The issue tracker was mined
(2026-07-10, most-discussed issues) and five recurring failure themes fell
out — these are the candidate Diagnose pages, in rough frequency order:

1. **Fresh city won't boot / `issue_prefix` not seeded** — `gc init` +
   `gc start` leaving a non-functional install; `gc sling` and
   `gc session attach` failing on clean installs.
2. **Dolt/bd resource usage and connection failures** — idle cities running
   bd subprocesses continuously, battery/CPU drain, dolt server port drift
   and stale runtime state breaking bd connections.
3. **Stuck sessions and reconciler loops** — drain-log loops from orphaned
   in-progress beads, sessions ignoring unread mail, config-drift draining
   active sessions; distill the user-facing half of
   `engdocs/contributors/reconciler-debugging.md`.
4. **Pool dispatch anomalies** — two sessions executing the same bead,
   spawn-without-assign, implicit pool agents clobbering a shared worktree.
5. **Idle city consuming resources / bead leaks** — excess agent activity
   with no work pending.

Write user-facing Diagnose pages for the top 3–5, each following the
existing walkthrough shape (symptom → confirm → cause → fix → verify).

- **Acceptance:** each new page follows the existing Diagnose walkthrough
  shape; `make check-docs` passes.

### 5. Docs-autofix bot for generated docs (port from beads)

Beads' `.github/workflows/docs-autofix.yml` pushes the regenerated-docs commit
to a stale same-repo PR instead of just failing CI. Port the pattern for this
repo's `cmd/genschema` outputs (`docs/reference/cli.md`, `config.md`,
`schema/*`). The security model is the non-negotiable part: never execute PR
code, anchored path allowlist, fork PRs get a comment with the regen recipe
instead of a push.

- **Acceptance:** workflow lints (`actionlint` if available); dry-run
  reasoning documented in the PR description; behavior verified on a real
  stale PR after merge.

### 6. Ecosystem page (low priority)

A "Related projects / articles" page modeled on beads' `RELATED_PROJECTS.md` /
`ARTICLES.md`. Wait until the beads Mintlify port lands so the two sites can
cross-reference each other.

## Cross-repo coordination

Once the beads repo has its `beads-docs` skill, do a one-time terminology sync
with `.claude/skills/gascity-docs/` for the shared vocabulary (molecule,
formula, wisp, gate) — the two projects must not define the same word
differently. Gas City treats molecule/wisp as v1 implementation detail; beads
exposes them as user concepts. Each skill documents its own usage and notes the
other's.

## Completion

On completion: archive to `specs/plans/archive/` via `git mv`; distill anything
permanently true (e.g. the contextual-menu config convention, the autofix-bot
security rules) into the gascity-docs skill or AGENTS.md; sweep for
unimplemented items per the spec-lifecycle conventions.
