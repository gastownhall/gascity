---
title: "Skill Materialization — Implementation Plan"
---

Companion to `engdocs/proposals/skill-materialization.md`. Breaks the v0.15.1
hotfix into parallelizable work units grouped into four phases. Each phase
ends with a `/review-pr` loop that must return no blockers or majors before
the next phase begins.

## Phase 1: Foundation (4 parallel subagents)

All four run in parallel with isolated worktrees, no shared file surfaces.
Merge back into `release/v0.15.1` after completion.

### 1A. Delete dead attachment-list code
**Files:** `internal/config/config.go` (Agent, AgentDefaults, AgentOverride fields + apply functions + `mergeAgentDefaults` Skills/MCP handling), `internal/config/patch.go` (AgentPatch fields + apply), `internal/config/field_sync_test.go`, `internal/migrate/migrate.go`, `internal/migrate/migrate_test.go`, `cmd/gc/pool.go`, `cmd/gc/pool_test.go`, `cmd/gc/cmd_skill.go` (remove `attachmentSet`/`filterEntriesByName` filter path), `cmd/gc/cmd_mcp.go` (same), `internal/config/compose_test.go` (remove attachment-defaults assertions), `internal/config/config_test.go` (delete `TestParseAgentSkillsAndMCP` and AgentDefaults.Skills/MCP parsing tests, update attachment-inheritance integration test).

**Scope:** Per the spec, fields are **tombstoned** (accepted + ignored with deprecation warning) rather than hard-removed in v0.15.1 — hard removal lands in v0.16. So this subagent keeps the struct fields but: (a) removes all apply/consume paths, (b) adds a deprecation warning emitter on load, (c) deletes tests that assert behavior, (d) updates `field_sync_test.go` allow-list.

### 1B. Create `core` bootstrap pack
**Files:** `internal/bootstrap/packs/core/pack.toml` (new), `internal/bootstrap/packs/core/skills/gc-<topic>/SKILL.md` for 7 topics (new), `internal/bootstrap/bootstrap.go` (add entry to `BootstrapPacks`).

**Content migration:** each `cmd/gc/skills/<topic>.md` becomes `internal/bootstrap/packs/core/skills/gc-<topic>/SKILL.md` with real YAML frontmatter (name + description from `cmd/gc/cmd_skills.go:19-31`) followed by the topic body. No `!` `gc skills …` `` shell-escape — content is first-class.

### 1C. Implicit-import collision detection
**Files:** `internal/bootstrap/collision.go` (new shared predicate), `internal/bootstrap/bootstrap.go` (wire the check before `EnsureBootstrap` writes the entry), `internal/config/compose.go` (wire the check during composition to emit a hard diagnostic when a user's `[imports.<bootstrap-name>]` shadows the splice), unit tests for both surfaces.

### 1D. Collision validator + `gc doctor` check
**Files:** `internal/validation/skill_collision.go` (new) — the shared validator function that groups agents by `(scope-root, vendor)` and detects duplicate agent-local skill names. `internal/doctor/skill_checks.go` (new) — exposes the validator as a doctor check. Unit tests.

**Note:** wiring into `gc start` / supervisor tick is deferred to Phase 4 (ordering must happen after the materializer lands in Phase 2).

**Phase 1 review boundary:** `/review-pr` on the cumulative diff against `v0.15.0`. Fix/iterate until no blockers or majors.

---

## Phase 2: Materializer (3 parallel subagents)

### 2A. Materializer core
**Files:** `internal/materialize/skills.go` (new) or similar — the core library:
- Vendor map (`claude`, `codex`, `gemini`, `opencode` → skill dirs).
- `SkillSourceDiscovery` function — enumerates union of current city pack `skills/` + bootstrap implicit-import pack `skills/` (via `ReadImplicitImports` + `GlobalRepoCachePath`) + per-agent `agents/<name>/skills/`.
- `MaterializeAgent(agent, workdir)` function — the core materialization with cleanup (ownership-by-target-prefix + atomic symlink rename via `write-temp + rename`).
- Legacy stub migration — detect v0.15.0 stub-shape regular directories and delete before first materialization.
- Unit tests for cleanup decision matrix (7-row table), legacy-stub detection (content-match preserves user content), vendor lookup, source discovery enumeration.

### 2B. Delete `gc skills` command + stub materializer
**Files:** delete `cmd/gc/skill_stubs.go`, `cmd/gc/skill_stubs_test.go`, `cmd/gc/cmd_skills.go`, `cmd/gc/cmd_skill_test.go` (the `TestSkillsAllTopicsReadable` test), `cmd/gc/skills/*.md`. Modify `cmd/gc/cmd_start.go:491` and `cmd/gc/cmd_supervisor.go:1443-1444` to remove the call sites.

### 2C. `gc doctor --fix` rule for deprecated fields
**Files:** `internal/doctor/autofix_skills.go` (new) — a `--fix` rule that strips `skills`, `mcp`, `skills_append`, `mcp_append`, `shared_skills` from user TOML when present (this is the migration helper that pairs with 1A's tombstone warnings). Unit tests.

**Phase 2 review boundary:** `/review-pr`. Fix/iterate until approved.

---

## Phase 3: Integration (3 parallel subagents)

### 3A. `gc internal materialize-skills` CLI
**Files:** `cmd/gc/cmd_internal_materialize_skills.go` (new). Thin cobra wrapper over Phase 2A's `MaterializeAgent`. Args: `--agent <qualified-name>`, `--workdir <path>`. Unit test.

### 3B. BuildDesiredState integration + FingerprintExtra
**Files:** `cmd/gc/build_desired_state.go` (or wherever runtime.Config is built). Per agent:
- Determine runtime eligibility (subprocess/tmux → eligible; k8s/acp → skip).
- If eligible AND WorkDir ≠ scope-root: append `gc internal materialize-skills --agent <name> --workdir <path>` to `PreStart`.
- If eligible (regardless of WorkDir): populate `FingerprintExtra["skills:<name>"] = hash` per skill.
- If ineligible: populate no `skills:*` entries.
Unit tests for all four branches (eligible-scope-root, eligible-worktree, k8s, acp).

### 3C. Update `gc skill list`
**Files:** `cmd/gc/cmd_skill.go`. Post-1A the filter is already removed; this subagent's remaining work is to extend source enumeration to include bootstrap implicit pack skills (the `core` catalog) so `gc skill list` reflects what the materializer delivers. Source column shows `city` / `core` (or pack name) / `agent`. Acceptance test updates.

**Phase 3 review boundary:** `/review-pr`. Fix/iterate until approved.

---

## Phase 4: Final integration + tests + docs (3 parallel subagents)

### 4A. Supervisor tick reordering + start-time gate
**Files:** `cmd/gc/cmd_supervisor.go`, `cmd/gc/cmd_start.go`. New tick order: ResolveFormulas → ValidateAgents → SkillCollisionValidator → MaterializeSkills → BuildDesiredState → Fingerprints → Drain. Block start on collision-validator error (wire 1D's validator as a gate). Unit + smoke tests.

### 4B. Acceptance + integration tests
**Files:** extend `test/acceptance/skill_test.go` with the full matrix from the spec (city skill delivered, agent-local delivered, mixed-provider sinks, user-placed content preserved, collision blocks start, k8s/ACP skipped with log line). New `test/integration/skill_lifecycle_test.go` — full add/edit/delete lifecycle with drain/restart observation.

### 4C. Schema + docs + migration guide updates
**Files:** `docs/schema/city-schema.json` (mark `skills`, `mcp`, `skills_append`, `mcp_append`, `shared_skills` as deprecated with description pointing to removal in v0.16), `docs/reference/config.md` (same), `docs/guides/migrating-to-pack-vnext.md` (update Skills/MCP section to describe the materialization semantics).

**Phase 4 review boundary:** final `/review-pr`. Fix/iterate until approved.

---

## Handoff

After Phase 4 approval, manual testing by user. No auto-commit or auto-push —
the branch stays at `release/v0.15.1` in this worktree for user verification.
