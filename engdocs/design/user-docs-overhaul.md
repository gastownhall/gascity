---
title: User Docs Overhaul
description: Plan to make `docs/` usable for end users — especially Gas Town users migrating to Gas City — rather than a half-contributor half-user mix.
status: Proposed
---

# User Docs Overhaul

**Status:** Proposed — first draft, expected to evolve before issues are cut.

**Audience this plan targets:** engineers who want to *use* Gas City (not contribute to it). The motivating persona is a Gas Town user evaluating or executing a switch to Gas City.

**Scope:** `docs/` (the Mintlify site). Contributor material in `engdocs/` is in scope only as a *source* — content that should be promoted, adapted, or referenced from `docs/`.

**Out of scope:** contributor-facing docs in `engdocs/`, CLAUDE/AGENTS files, internal architecture rewrites.

---

## 1. Premise

The current `docs/` tree is roughly 75% solid: installation, quickstart, and the seven tutorials work and are progressive. But the conceptual foundation lives in `engdocs/` (contributor docs), tutorials are split between PackV1 and PackV2 syntax, and several user-facing pages still send users into `engdocs/`. A new user — especially a Gas Town migrator — will bounce between conflicting sources.

The goal of this overhaul is to make `docs/` self-sufficient for users: no link to `engdocs/` from a user-facing page should be required to understand or operate Gas City.

---

## 2. Findings

Each finding is tagged **P0** (blocks user onboarding), **P1** (significant friction), or **P2** (polish / longer-term). Findings carry a shortcode (**F1**, **F2**, …, **F12**) used throughout the rest of this document — in particular, work-plan issues in §4 cite the findings they address by shortcode.

### 2.1 F1 — No top-down view of how Gas City works (P0)

`docs/` has no high-level explanation of Gas City as an orchestrator. An engineer arriving cold cannot answer "what is this system, how do its parts hang together, how does a piece of work flow through it, how are agents spawned and kept alive, how do they talk to each other" from user docs alone.

What's needed is two pages, both currently absent from `docs/`:

**(a) An architecture overview** — top-down prose plus at least two diagrams:

- **Structural diagram:** the components that exist (city → rigs → agents → sessions; beads store; event bus; controller) and how they connect.
- **Interactional / lifecycle diagram:** how a piece of work flows through the system — e.g. `gc sling` → bead created → agent selected → session spawned → prompt rendered → nudge → completion → events emitted. A health/keepalive flow (patrol → stall detection → restart with backoff) is a strong candidate for a third diagram.

This is the page a user needs *first*, both to evaluate fit and to form a working mental model.

**(b) A primitives reference** — the Nine Concepts (Session, Task Store, Event Bus, Config, Prompt Templates, Messaging, Formulas, Dispatch, Health Patrol) rewritten in user tone and positioned as a deeper reference *introduced by* the overview, not as the entry point. The Nine Concepts are bottom-up building blocks; they do not teach the system on their own.

Both currently live only in `engdocs/architecture/` (contributor tone), and [docs/index.mdx](../../docs/index.mdx) plus [coming-from-gastown.md](../../docs/getting-started/coming-from-gastown.md) send users there as required reading.

Relevant engdocs source material to adapt:

- `engdocs/architecture/index.md` — overview seed
- `engdocs/architecture/life-of-a-bead.md`, `engdocs/architecture/life-of-a-molecule.md` — interactional diagram
- `engdocs/architecture/session.md`, `controller.md`, `health-patrol.md` — "how agents are kept alive"
- `engdocs/architecture/nine-concepts.md`, `glossary.md` — primitives reference

Existing published asset to cross-reference from the overview:

- [`docs/tutorials/06-beads#bead-lifecycle`](../../docs/tutorials/06-beads.md#bead-lifecycle) — already shows how a unit of work moves through states; link as "dig deeper" from the interactional diagram section rather than duplicating the content.

### 2.2 F2 — PackV1 / PackV2 contradiction in the tutorial path (P0)

[Tutorial 02 — Agents](../../docs/tutorials/02-agents.md) and [Tutorial 03 — Sessions](../../docs/tutorials/03-sessions.md) still use the legacy `[[agent]]` TOML blocks, while [coming-from-gastown.md](../../docs/getting-started/coming-from-gastown.md) and [guides/shareable-packs.md](../../docs/guides/shareable-packs.md) teach the PackV2 `agents/<name>/` directory layout. A user following tutorials in order will write config that contradicts the migration guide they hit next.

The project is currently at **v1.2.1**, well past the PackV2 cutover. PackV1 syntax in tutorials is unambiguous staleness, not a pre-release hedge.

### 2.3 F3 — Gas Town role recap missing (P1)

[coming-from-gastown.md](../../docs/getting-started/coming-from-gastown.md) maps Gas Town roles to Gas City equivalents without explaining what each role *did* operationally in Gas Town. A user who hasn't touched Gas Town recently — or who is *evaluating* whether to migrate at all — cannot recover operational meaning from a mapping alone. One paragraph per role (mayor, deacon, witness, refinery, polecat, crew, dog) describing its function in Gas Town is the minimum.

### 2.4 F4 — Migration Concept Map mixes domains (P1)

The Concept Map table in `coming-from-gastown.md` jumbles at least four distinct domains into a single mapping:

- **Roles** — Mayor, Deacon, Witness, Refinery, Polecat, Crew, Dog (things that *act*).
- **Mechanisms / behaviors** — "Deacon watchdog logic", "Witness lifecycle logic" (what those things *do*).
- **Filesystem / state layout** — "Directory tree under `~/gt`" (where state *lives*).
- **Likely more** — commands, runtime artifacts, config file shapes.

Forcing the reader to disentangle domains while simultaneously learning the new system multiplies cognitive load. The fix is structural, not editorial: split into separate single-domain tables, each presented in a deliberate order (role recap → roles → mechanisms → filesystem/state → commands → workflows).

### 2.5 F5 — Workflow mapping missing (P1)

The current migration guide is heavy on nouns (what *was* a mayor, what *is* an agent) and light on verbs (how do I *do* X in Gas City if I used to do it in Gas Town?). A workflow mapping table — e.g. "spin up a worker", "send a task", "inspect what's stuck", "restart a stalled agent", "share a config across teams" — is what a real migrator reaches for, and it does not exist today.

Distinct from F9 (workflow gaps in the *general* user docs): F5 is about *translation* of existing Gas Town habits; F9 is about *operation* once on Gas City.

### 2.6 F6 — Migration guide points at files that aren't in the docs (P1)

The "Fast Ramp Checklist" in [coming-from-gastown.md](../../docs/getting-started/coming-from-gastown.md) references `examples/gastown/city.toml` and similar repo paths. These are not embedded in the rendered docs, nor linked as downloadable assets, nor guaranteed to exist at those paths. Users following the rendered site dead-end.

### 2.7 F7 — Contributor material leaking into user docs (P1)

- [docs/packv2/](../../docs/packv2/) holds 9 files; only `migration.mdx` is in the user nav. The rest (`doc-pack-v2.md`, `doc-agent-v2.md`, `doc-loader-v2.md`, `doc-rig-binding-phases.md`, `doc-conformance-matrix.md`, `doc-consistency-audit.md`, `doc-packman.md`, `doc-commands.md`, `skew-analysis.md`) read as contributor scratch.
- [docs/internals/](../../docs/internals/) is labeled "not required reading" but `beads-topology.md` is exactly what an operator needs.
- [docs/index.mdx](../../docs/index.mdx) literally states the tree is "organized for external contributors first" — wrong primary audience for `docs/`.

### 2.8 F8 — Reference docs are complete but not navigable (P1)

- [reference/config.md](../../docs/reference/config.md): flat auto-generated field dump, no grouped "Common Patterns".
- [reference/formula.md](../../docs/reference/formula.md): mentions conditions/loops/checks but never shows a working example.
- [reference/api.md](../../docs/reference/api.md): points at OpenAPI with no "how do I query my city" narrative.
- [reference/cli.md](../../docs/reference/cli.md): comprehensive but flat; no task-oriented entry points.

### 2.9 F9 — Significant workflow gaps in operating Gas City (P1)

Things a real user will hit and not find:

- How to write a first agent prompt from scratch (template variables, prompt design).
- Debugging a stuck or failing session (`gc session peek` output, detection, kill/restart).
- Choosing between crew (persistent) and polecats (on-demand).
- Hooks and external integrations.
- Multi-machine / Kubernetes deployment.
- Health monitoring; what `gc doctor [--fix]` actually does.

Source material exists in `engdocs/architecture/` (e.g. `session.md`, `health-patrol.md`, `dispatch.md`, `prompt-templates.md`) and `engdocs/design/machine-wide-supervisor-v0.md` for scaling. None is promoted to user docs.

### 2.10 F10 — No full end-to-end example (P1)

There is no canonical "minimal complete city" showing `pack.toml` + `city.toml` + `agents/<name>/agent.toml` + `prompt.md` + a formula + an order, together. Each piece is shown in isolation across different tutorials; assembly is left to the reader.

### 2.11 F11 — Single-runbook troubleshooting catalog (P2)

[docs/troubleshooting/](../../docs/troubleshooting/) holds one runbook (`dolt-bloat-recovery.md`). The Oh-My-Zsh `gc`-alias trap — a real blocker for affected users — is in [getting-started/troubleshooting.md](../../docs/getting-started/troubleshooting.md) but should be a prerequisite check at the top of Quickstart.

### 2.12 F12 — Staleness signals (P2)

- [Tutorial 01 line ~26](../../docs/tutorials/01-cities-and-rigs.md) shows version `v0.13.4` in sample output. Current tag is **v1.2.1** — eight minor versions behind.
- [docs/index.mdx](../../docs/index.mdx) opens with "organized for external contributors first" — contradicts the goal of `docs/`.
- General sweep needed for stale concept names (the former Agent Protocol primitive was removed `dd90ac0a` on 2026-03-08; user docs appear clean but worth a check).

---

## 3. Cross-cutting principles for the overhaul

Before issues are cut, the following are non-negotiable so individual PRs stay coherent:

1. **`docs/` is user-first.** No user-facing page may link into `engdocs/` as required reading. If `engdocs/` content is needed, promote it (with rewrite) into `docs/`.
2. **One pack syntax in tutorials.** PackV2 (`agents/<name>/`) is canonical. PackV1 lives only in a clearly-labeled migration appendix.
3. **Every concept page has a runnable example.** No conceptual page ships without at least one snippet a user can copy-paste.
4. **Examples are versioned with the docs.** Reference output uses the current release (v1.2.1 at time of writing).
5. **`engdocs/` stays the source of truth for contributors.** When content is promoted, the original stays as a deeper contributor reference; the user-doc version links *back* (one-way).

---

## 4. Work plan (candidate GitHub issues)

This is the first cut. Each item below is sized roughly to be a single PR / issue. Grouped by milestone so we can sequence cleanly. Each issue cites the finding(s) it addresses by shortcode (F1, F2, …); the mapping is not strictly 1:1.

### 4.1 Milestone 1 — Stop the bleeding (P0)

**Issue 1 — Architecture overview page + diagrams.** Addresses F1(a). **Recorded:** [#2101](https://github.com/gastownhall/gascity/issues/2101). **Implemented:** [PR #2981](https://github.com/gastownhall/gascity/pull/2981).
- New page: `docs/concepts/architecture-overview.md`.
- Top-down prose: what Gas City is, how the parts hang together, how work flows, how agents are kept alive and communicate.
- At least two diagrams (Excalidraw — see [`engdocs/proposals/excalidraw-diagrams.md`](../proposals/excalidraw-diagrams.md) for toolchain and authoring workflow): structural and interactional. Health/keepalive a strong third.
- Update `docs/index.mdx` and `docs/getting-started/coming-from-gastown.md` to link to this page instead of `engdocs/`.
- Add to `docs/docs.json` nav.

**Issue 2 — Primitives reference page.** Addresses F1(b). Lands in lockstep with Issue 1. **Recorded:** [#3014](https://github.com/gastownhall/gascity/issues/3014). **Implemented:** [PR #3034](https://github.com/gastownhall/gascity/pull/3034).
- New page: `docs/concepts/primitives.md`.
- Promote and rewrite `engdocs/architecture/nine-concepts.md` and `glossary.md` for a user audience.
- Linked *from* the architecture overview, not the entry point.

**Issue 3 — Migrate Tutorials 02 and 03 to PackV2 syntax.** Addresses F2. **Recorded:** [#3013](https://github.com/gastownhall/gascity/issues/3013). **Implemented:** Tutorials 02/03 were already on the PackV2 `agents/<name>/` layout on `main` (legacy `[[agent]]` authoring deprecated in [#2117](https://github.com/gastownhall/gascity/pull/2117)); a cross-reference audit confirmed the full 01→07 tutorial path plus `coming-from-gastown.md` and `shareable-packs.md` are PackV1-free, and fixed the one remaining PackV1-as-canonical example in `docs/concepts/primitives.md`. [#3013](https://github.com/gastownhall/gascity/issues/3013) closed as completed.
- Rewrite the `agents/<name>/agent.toml` + `prompt.md` flow.
- Verify all cross-references (Tutorial 04+, guides) stay consistent.
- Move existing PackV1 examples to a clearly-labeled appendix inside `guides/migrating-to-pack-vnext.md` (or remove if redundant with that guide).

**Issue 4 — Refresh stale version strings and "contributors first" framing.** Addresses F12. **Recorded:** [#3015](https://github.com/gastownhall/gascity/issues/3015). **Implemented:** [PR #3044](https://github.com/gastownhall/gascity/pull/3044).
- Update Tutorial 01 sample output (and any others) to v1.2.1 .
- Update `docs/index.mdx` opening framing to "user-first".
- Sweep for any other stale concept names.

### 4.2 Milestone 2 — Migration realism (P1)

**Issue 5 — Restructure `coming-from-gastown.md`.** Addresses F3, F4, F5 in one coherent rewrite of the same file.
- Add "Gas Town role recap" — one paragraph per role.
- Split the single Concept Map into domain-scoped tables: roles, mechanisms, filesystem/state, commands (existing), workflows (new).
- Add the workflow mapping table ("how I used to do X → how I do X now").

**Issue 6 — Embed or link real example assets.** Addresses F6.
- Replace dangling `examples/gastown/city.toml` references with inline TOML blocks **or** downloadable links served by Mintlify.
- Ensure offline / PDF rendering works.

**Issue 7 — Publish a "Complete Minimal City" example.** Addresses F10.
- New page under `docs/guides/` (or top-level `docs/examples/minimal-city.md`).
- Full tree: `pack.toml`, `city.toml`, `agents/<name>/`, one formula, one order — assembled, runnable end-to-end.
- Link from index, quickstart, and Tutorial 07.

### 4.3 Milestone 3 — Reference & workflow gaps (P1)

**Issue 8 — Add "Common Config Patterns" to `reference/config.md`.** Addresses F8.
- Hand-written grouped patterns above the auto-generated field dump: changing beads provider, adding a rig, configuring pools, overriding an agent's provider, etc.

**Issue 9 — Expand `reference/formula.md` with conditions, loops, checks.** Addresses F8.
- Working examples of each. Source: `engdocs/architecture/formulas.md`, `engdocs/design/formula-v2-transient-retries.md`.

**Issue 10 — Narrative API guide in `reference/api.md`.** Addresses F8.
- "How to query your city's state" walkthrough with curl + a tiny client snippet. Keep the OpenAPI link.

**Issue 11 — "Debugging Sessions" guide.** Addresses F9.
- `gc session peek` output, stuck-session detection, restart flow.
- Source: `engdocs/architecture/session.md`, `engdocs/contributors/reconciler-debugging.md` (sanitized — the trace artifact workflow stays contributor-only).

**Issue 12 — "Choosing crew vs. polecats" guide.** Addresses F9.
- Decision criteria and config snippets.
- Source: `engdocs/architecture/session.md`, `engdocs/design/agent-pools.md`.

**Issue 13 — "Writing your first agent prompt" guide.** Addresses F9.
- Template variables, GUPP principle in user terms, examples.
- Source: `engdocs/architecture/prompt-templates.md`.

### 4.4 Milestone 4 — IA cleanup (P1/P2)

**Issue 14 — Audit and reclassify `docs/packv2/`.** Addresses F7.
- For each of the unlinked files: promote into Guides/Reference with a proper nav entry, or move to `engdocs/`.
- Update `docs/docs.json`.

**Issue 15 — Rename `docs/internals/` → "How It Works" (or similar), promote `beads-topology.md` into the main IA.** Addresses F7.
- Adjust framing from "not required" to operator-relevant.

**Issue 16 — Move OMZ alias warning to top of Quickstart and expand troubleshooting catalog.** Addresses F11.
- New runbooks (initial set): recovering from dolt corruption, resetting a rig, debugging order triggers.

### 4.5 Milestone 5 — Scaling and integrations (P2)

**Issue 17 — Multi-machine / Kubernetes deployment guide.** Addresses F9.
- Source: `engdocs/design/machine-wide-supervisor-v0.md`.

**Issue 18 — Document hooks and external integrations.** Addresses F9.
- Source: `engdocs/architecture/messaging.md`, `engdocs/design/external-messaging-fabric.md`.

---

## 5. Process

1. Land this design doc as `Proposed`.
2. Iterate on §2 (findings) and §4 (issue list) in this file until the work plan is agreed.
3. Cut the issues from §4. Each issue links back here for context.
4. Flip status to `Accepted` once issues are filed.
5. Flip individual items in §4 to checkboxes as their issues land.
6. Flip the whole doc to `Implemented` when Milestones 1–3 are done. Milestones 4–5 may continue independently.

---

## 6. Open questions

- **IA placement of the architecture overview and primitives reference.** ~~Top-level `docs/concepts/`? Under `getting-started/` so they're encountered before tutorials? Some hybrid (overview in getting-started, primitives in concepts)?~~ **Resolved: both pages land under top-level `docs/concepts/`.** Final paths: `docs/concepts/architecture-overview.md` (Issue 1) and `docs/concepts/primitives.md` (Issue 2).
