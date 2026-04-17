---
title: "Skill Materialization v0.15.1 — Session Handoff"
---

**For future Claude sessions resuming this work.**

This file is the resume-point after a session checkpoint at the end of
Phase 1. If you are picking up this task, read this document first,
then the spec and implementation plan it references.

---

## Where you are

- **Branch:** `release/v0.15.1` (off `v0.15.0` tag).
- **Worktree:** `/data/projects/gascity/.claude/worktrees/skills-mcp`.
- **State:** Phase 1 complete + its review loop approved. Changes are
  **uncommitted** in the worktree — the user reviews and commits
  themselves.
- **Build:** `go build ./...` clean.
- **Tests:** `go test ./...` — all green.

### Sanity check commands on resume

```bash
cd /data/projects/gascity/.claude/worktrees/skills-mcp
git status                              # should show ~30 changes on release/v0.15.1
git log --oneline v0.15.0..HEAD          # should be empty (no commits yet)
go build ./... && go test ./... | tail
```

If any of those fail, stop and investigate before doing more work.

---

## Read-order for context

1. `engdocs/proposals/skill-materialization.md` — the **approved spec**
   (703 lines). Six-pass reviewed by Claude + Codex. Every design
   decision lives here.
2. `engdocs/proposals/skill-materialization-implementation-plan.md` —
   the **phase-by-phase plan** (4 phases × 3-4 subagents each + review
   loops).
3. This file (you are here).
4. The task list (via `TaskList` tool) — 17 tasks, Phase 1 complete,
   tasks 6-17 pending with dependency chain intact.

Do **not** re-read Phase 1 source files unless you need to verify
something specific. The spec + plan capture everything you need.

---

## What Phase 1 delivered (don't re-do)

1. **Tombstone attachment-list fields** (task 1). `Agent.Skills`,
   `Agent.SharedSkills`, `AgentDefaults.Skills/MCP`, `AgentPatch.*`,
   `AgentOverride.*` field variants are kept TOML-parseable but
   completely inert. A one-time deprecation warning fires per load if
   any tombstone field is populated. All apply/consume paths deleted.
   All associated test fixtures updated. Surface: `cmd/gc/pool.go`,
   `cmd/gc/cmd_skill.go`, `cmd/gc/cmd_mcp.go`,
   `internal/config/{config,patch,pack,compose}.go`,
   `internal/migrate/migrate.go`, plus their `_test.go` files.

2. **`core` bootstrap pack** (task 2). New
   `internal/bootstrap/packs/core/` with `pack.toml` +
   `skills/gc-<topic>/SKILL.md` × 7. Content migrated from
   `cmd/gc/skills/*.md` with real YAML frontmatter. Registered in
   `BootstrapPacks` with `Source:
   "github.com/gastownhall/gc-core"`.

3. **Implicit-import collision detection** (task 3). New
   `internal/bootstrap/collision.go` with `CollidesWithBootstrapPack` +
   `BootstrapPackNames`. New `EnsureBootstrapForCity` function.
   Composer wired via duplicated unexported `collidesWithImplicitImports`
   helper (import cycle workaround) + `bootstrapManagedImportNames`
   list. **Important:** the compose-time gate is scoped to bootstrap
   names only — non-bootstrap implicit imports retain the pre-v0.15.1
   "explicit wins silently" contract. A sync test
   (`TestBootstrapManagedNames_MatchesBootstrapPacks`) asserts the two
   duplicated lists agree.

4. **Skill collision validator + doctor check** (task 4). New
   `internal/validation/skill_collision.go` with
   `ValidateSkillCollisions(cfg) []SkillCollision`. New
   `internal/doctor/skill_checks.go` registering
   `NewSkillCollisionCheck`. **Wiring into `gc start` /
   supervisor tick is deferred to Phase 4A.**

### Phase 1 review fixes applied

Five issues from the first Phase 1 review pass were fixed in-line (not
in a separate commit):
- Scoped compose-time hard-stop to bootstrap names only (was rejecting
  all implicit-import collisions).
- Aggregated all colliding names in both error messages.
- Extended deprecation warning scan to `rig.RigPatches`.
- Expanded deprecation warning wording to name all fields and surfaces.
- Added `TestBootstrapManagedNames_MatchesBootstrapPacks` sync test.

---

## What's next

### Phase 2 (3 subagents) — the materializer core

| Task | Subject | Key files |
|------|---------|-----------|
| 6 | Materializer core library | `internal/materialize/skills.go` (new), vendor map, source discovery, cleanup, legacy-stub migration |
| 7 | Delete `gc skills` command + stub materializer | Delete `cmd/gc/skill_stubs.go`, `cmd_skills.go`, `cmd/gc/skills/*.md`; remove call sites |
| 8 | `gc doctor --fix` rule for deprecated attachment fields | `internal/doctor/autofix_skills.go` (new) |

Task 9 is the Phase 2 review boundary — run `/review-pr` and iterate.

**Run order:** tasks 7 and 8 can run in parallel after task 6 lands
(task 7's deletions don't conflict with 6 because 6 creates a new
package; task 8's doctor rule is also disjoint). If running serially,
do 6 → 7 → 8.

### Phase 3 (3 subagents)

| Task | Subject |
|------|---------|
| 10 | `gc internal materialize-skills` CLI (thin cobra wrapper over task 6's library) |
| 11 | `BuildDesiredState` integration + `FingerprintExtra["skills:<name>"]` population |
| 12 | Update `gc skill list` to include bootstrap catalogs |

Task 13 = Phase 3 review boundary.

### Phase 4 (3 subagents + final review)

| Task | Subject |
|------|---------|
| 14 | Supervisor tick reordering + start-time collision gate |
| 15 | Acceptance + integration tests |
| 16 | Schema + reference docs + migration guide updates |
| 17 | Final `/review-pr` + handoff for manual testing |

---

## Operational notes from Phase 1

Non-obvious lessons the future you should know:

1. **Import cycle: `bootstrap` already imports `config`.** So
   `config/compose.go` cannot call `bootstrap.CollidesWithBootstrapPack`
   or reference `BootstrapPackNames()`. Phase 1 solved this by
   duplicating the predicate inside `config` and keeping the two name
   lists in sync via `TestBootstrapManagedNames_MatchesBootstrapPacks`.
   If future phases need another bootstrap↔config shared surface,
   repeat that pattern or add a third shared package to break the
   cycle.

2. **`overlay.CopyFileOrDir` follows symlinks via `os.Stat`.**
   `runtime.CopyFiles` is staged unconditionally by every runtime
   (subprocess/tmux/acp/k8s). That's why the spec routes per-skill
   fingerprints through `FingerprintExtra`, not `CopyEntry`. Do **not**
   try to route skill entries through `CopyEntry` in Phase 3B — the
   runtime will try to copy the symlinks into workdirs and shadow the
   materializer.

3. **Eight providers, four with sinks.** `internal/hooks/hooks.go:80-96`
   enumerates `claude`, `codex`, `gemini`, `opencode`, `copilot`,
   `cursor`, `pi`, `omp`. Only the first four get skill sinks in
   v0.15.1 (see spec "Vendor mapping" table). The other four are
   explicit no-ops with an informational log line. Don't rediscover this.

4. **Bootstrap pack cache path.** Bootstrap packs resolve to
   `<gcHome>/cache/repos/<GlobalRepoCacheDirName(source, commit)>/` — a
   **single** directory component keyed on source+commit hash (not
   `<gcHome>/cache/packs/<name>/` as an earlier spec draft said). Use
   `config.ReadImplicitImports` + `config.GlobalRepoCachePath` from
   the materializer (Phase 2A).

5. **Deletion-surface table in the spec is authoritative.** When
   removing code, trust the table in the spec's "No attachment
   filtering" section. Phase 1 verified every row exists at the cited
   line range — they do, but future edits to the codebase may shift
   line numbers. Use `grep` with field names, not line numbers.

6. **`gc doctor` registration.** The doctor-check registration happens
   in `cmd/gc/cmd_doctor.go` inside the `cfgErr == nil` block. Phase 4A
   will need to add the skill collision check as a blocking gate at
   `gc start`, which is a **different** integration point — the
   doctor check surface doesn't block start today. Don't confuse these.

7. **Agent-collision validator: rig-scoped expansion.** A rig-scoped
   agent runs on multiple rigs. `ValidateSkillCollisions` needs to
   check collisions **per (rig, vendor)** pair, not just per scope
   class. Phase 1's implementation handles this via an expanded scope
   root list. Phase 4A wires it up.

8. **`rig.RigPatches` vs `rig.Overrides`.** PackV2 renames the
   overrides surface to `patches`. Both currently coexist in the
   config. Phase 1's deprecation warning scan covers both; future work
   on patches/overrides surfaces should keep both in mind.

9. **The review loop is non-trivial.** Each `/review-pr` pass I ran
   cost ~20-60K tokens. Budget for 2-3 passes per phase review
   boundary (Phase 1 converged in 2 passes). Use `--skip-gemini` (dual
   Claude + Codex) unless you have a reason.

10. **Review-pass reviewers need the spec file visible.** Phase 1 pass
    1 review was partially blocked because the reviewers couldn't find
    `engdocs/proposals/skill-materialization.md` (it's untracked).
    Include the spec file in the diff by dropping the `:(exclude)`
    path spec, or copy it into the post-change worktree before running
    the review.

---

## How to resume

### Option A: fresh conversation, continue from here

```
The handoff is in engdocs/proposals/skill-materialization-handoff.md.
Read it, then resume Phase 2 starting with task 6 (materializer core).
```

### Option B: continue the existing session

Just ask me to proceed with Phase 2. I'll read this file, call
`TaskList`, mark task 6 `in_progress`, and spawn the subagent.

### Option C: user wants to commit Phase 1 first

Phase 1 is ~30 files of uncommitted changes. A user-initiated commit
would be a good checkpoint. Suggested commit message:

```
feat: skill-materialization Phase 1 — tombstone fields, core bootstrap pack,
collision detection, validator

Part of v0.15.1 hotfix per engdocs/proposals/skill-materialization.md.

- Tombstone v0.15.0 attachment-list fields (Agent.Skills, SharedSkills,
  AgentDefaults.Skills/MCP, AgentPatch/AgentOverride Skills/MCP/*_Append).
  Fields parse but are inert; one-time deprecation warning per load.
  Hard parse error lands in v0.16.
- New core bootstrap pack at internal/bootstrap/packs/core/ with 7
  gc-topic SKILL.md files. Implicit-imported into every city.
- Implicit-import collision detection for bootstrap pack names (core,
  import, registry). Non-bootstrap implicit imports retain silent-shadow
  semantics.
- Skill collision validator (internal/validation) + doctor check. Not
  yet wired into gc start / supervisor tick (Phase 4A).
```

Don't commit on behalf of the user unless asked.

---

## File-level change manifest

**New files (8):**
- `engdocs/proposals/skill-materialization.md` (spec, untracked)
- `engdocs/proposals/skill-materialization-implementation-plan.md`
- `engdocs/proposals/skill-materialization-handoff.md` (this file)
- `internal/bootstrap/collision.go`
- `internal/bootstrap/collision_test.go`
- `internal/bootstrap/packs/core/pack.toml`
- `internal/bootstrap/packs/core/skills/gc-<7 topics>/SKILL.md`
- `internal/doctor/skill_checks.go`
- `internal/doctor/skill_checks_test.go`
- `internal/validation/skill_collision.go`
- `internal/validation/skill_collision_test.go`

**Modified files (22):**
- `cmd/gc/cmd_doctor.go`, `cmd_mcp.go`, `cmd_skill.go`,
  `cmd_skill_test.go`, `init_provider_readiness.go`, `pool.go`,
  `pool_test.go`
- `docs/reference/cli.md`, `config.md`, `docs/schema/city-schema.json`
- `internal/bootstrap/bootstrap.go`, `bootstrap_test.go`
- `internal/config/compose.go`, `compose_test.go`, `config.go`,
  `config_test.go`, `field_sync_test.go`, `implicit_test.go`, `pack.go`,
  `patch.go`
- `internal/migrate/migrate.go`, `migrate_test.go`

---

End of handoff.
