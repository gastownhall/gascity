# Plan — Migrate `packs/actual/` into `gc-actual-packs` repo

**PM bead:** ga-371q (this plan)
**Architecture decision:** ga-ejwh (Option A — new dedicated git repo)
**Designer runbook:** see `bd show ga-371q` DESIGN section, plus four
diagrams under
`/home/jaword/projects/gc-management/.gc/worktrees/gascity/designer/ga-371q/`
(`arch-before-after.png`, `migration-phases.png`, `edit-workflow.png`,
`rollback-tree.png`).
**Outcome:** every edit to the Actual pack family lands as a PR on
`github.com/gastownhall/gc-actual-packs`, reviewed under CODEOWNERS,
before reaching any running city. The ga-kc5s class of in-place
prompt edit becomes impossible.

## Scope

This plan tracks the migration **only** — moving 24+ packs out of the
local `gc-management/packs/actual/` directory into a versioned remote
pack registered via `gc pack`. The designer wrote the runbook; this
plan turns it into builder-executable work.

## Out of scope (per architecture / designer)

- Workshop installs at `~/Projects/factory/{workshop_w1,workshop_w2,lab_l1}/`
- PRs upstream into SFI
- Schema V1 → V2 migration (separate plan: `pack-v1-to-v2-migration-guide.md`)
- Splitting the `wren-*` family into its own pack
- `gc-management/` itself becoming a git repo (filed as follow-up)
- `gc lint <pack>` CLI command (filed as follow-up; a dependency for
  the optional pre-merge CI)

## Open questions to resolve in flight

These are flagged in the designer's §10 and §11; the builder
confirms each with the mayor on the relevant phase. None block kick-off.

1. **Repo visibility** — bootstrap as `--private` (designer default);
   public flip is a one-line `gh repo edit --visibility public`
   later. Confirmed with mayor before tagging `v0.1.0`.
2. **CODEOWNERS team / mayor handle** — placeholders
   `@gastownhall/factory-maintainers` and `@<mayor-handle>` need real
   values. Stop-gap: explicit usernames. Builder confirms during B-1.
3. **`[packs.actual]` field names** — the V1 pack-source schema
   may use `url`/`git_ref` or `source`/`ref`. Builder verifies via
   `gc config explain --section packs` before staging.
4. **Cache layout sibling-preservation** — `all/pack.toml` uses
   relative `../<pack>` includes; builder spot-checks during Phase 4
   that `gc pack fetch` lays the cache out with siblings intact.
   Designer flagged this as the highest architecture-vs-reality risk.

## Work breakdown

The five migration beads are sequential — each depends on the
previous. Follow-ups run in parallel and never block migration.

### Migration chain

| Bead | Pri | Phases | Blocks on | Title |
|------|-----|--------|-----------|-------|
| B-1  | P1  | 0 + 1  | —         | Bootstrap `gc-actual-packs` repo |
| B-2  | P1  | 2      | B-1       | Stage and validate `city.toml.next` |
| B-3  | P1  | 3+4+5  | B-2       | Cutover + smoke verify |
| B-4  | P2  | 6 + 7  | B-3       | 7-day soak, archive, delete |
| B-5  | P2  | (doc)  | B-3       | Documentation rollups & operator announcement |

All five carry `--label ready-to-build --metadata-field gc.routed_to=gascity/builder`.

### Follow-ups (filed separately, do not block migration)

| Bead | Pri | Title |
|------|-----|-------|
| F-1  | P3  | Add `gc lint <pack>` CLI for pre-merge validation |
| F-2  | P3  | `git init` `gc-management/` with appropriate `.gitignore` |
| F-3  | P3  | `gc doctor` check warning if `packs/actual/` resurfaces |
| F-4  | P3  | Pre-merge CI on `gc-actual-packs` (blocks on F-1) |

## Acceptance criteria — per bead

Detailed acceptance lives in each bead's notes. Each migration bead's
success criteria match the designer's per-phase checklist (§1.5,
§2.4, §4.1, §5.4, etc.). The **runbook is authoritative** — beads
point at it rather than restate it.

## Risks

1. **Cache layout drift.** If `gc pack fetch` does not preserve the
   sibling layout, every `../<pack>` include in `all/pack.toml`
   breaks at once. Caught early at §4.1; rollback C documented.
2. **Quiet-window coordination.** Phase 3 (suspend) and Phase 4
   (cutover) need an arranged window. B-3 must mail mayor before
   starting and confirm no human operators are mid-flow.
3. **CODEOWNERS gaps.** If the team handle doesn't exist, the
   stop-gap (explicit usernames) leaves the door open to a
   self-approved merge. Builder must list 2+ usernames.
4. **`packs/actual/` re-creation.** Any agent unaware of the
   migration could `mkdir -p packs/actual` and break the audit
   trail. F-3 (`gc doctor` check) is the long-term mitigation;
   short-term, B-5 documents the prohibition prominently.

## Coordination

- **Quiet window for B-3.** Builder mails mayor when ready to start
  Phase 3 so any active human operators are paused.
- **Soak monitoring (B-4).** Builder schedules the 7-day soak end
  date in their session notes; PM does not poll.
- **Follow-ups intake.** F-1..F-4 enter the standard ready-to-build
  queue and are picked up alongside other builder work.
