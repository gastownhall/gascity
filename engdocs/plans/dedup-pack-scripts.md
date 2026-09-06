# Plan: dedup pack scripts (ga-x0pq6s)

> **Status:** decomposing — 2026-05-11
> **Parent architecture:** `ga-o5t309` — Option C (move byte-identical
> shared scripts into `<city>/scripts/` referenced via the
> `{{.CityRoot}}` template).
> **Designer spec:** `ga-x0pq6s` (~1200-line design body) pins the
> doctor-check widening contract, the two-PR shape, file modes, the
> `<city>/scripts/README.md` content, the audited 6-caller list for
> `sync-actual-skill.sh` (all human-run), the verification sequence,
> and the test names for PR1.
> **Designer handoff mail:** `gm-r49n3y` (2026-05-11).
> **Decomposed into:** 2 builder beads — `ga-qsmaw2` (PR1, ready) and
> `ga-3b1eba` (PR2, blocked-by PR1).
> **Closes alongside:** `ga-omxbk3` — the rejected `{{.SystemPack:core}}`
> approach (architecture §6 Option B was explicitly REJECTED in favor of
> Option C).

## Context

Twenty packs under `packs/actual/*/` each ship a byte-identical
`worktree-setup.sh` (~250 LOC × 20). Six of those packs additionally ship a
byte-identical `sync-actual-skill.sh`. Total: ~5,090 LOC of shell
duplication. Architecture `ga-o5t309` chose **Option C** — move both scripts
to `<city>/scripts/` and reference them via the existing `{{.CityRoot}}`
template variable.

The architecture's §3.3 risk is **load-bearing** and verified by the
designer: PR #1778's `pre-start-scripts` doctor check
(`internal/doctor/pre_start_scripts_check.go`) hardcodes `{{.ConfigDir}}`
and would SILENTLY SKIP any `{{.CityRoot}}`-shaped pre_start command.
Switching the 20 pack pre_start lines without widening the check first
trades the real bug (drift) for a false-negative on the regression guard.

The fix is two PRs, in strict order:

1. **PR1** — widen `resolvePreStartScript` to recognize both
   `{{.ConfigDir}}` AND `{{.CityRoot}}`, with `cityPath` plumbed in via
   `CheckContext.CityPath`. New tests. ~105 LOC.
2. **PR2** — move canonical scripts to `<city>/scripts/`, sed-edit 20
   pack.toml files, delete 26 in-pack copies, update 6 README invocations
   plus `AGENT_PACK.md`. Two commits inside one PR: pilot-architect-edit,
   sweep-remaining-and-delete. ~50 LOC sed + 3 new files + 27 edited +
   26 deleted.

## Why two builder beads (not one)

Architecture §10 calls for one-PR-per-concern. The designer's refinement
(§"PR split"): bundle the two cleanups together in PR2 (both depend on the
same PR1 doctor-check widening, and the moves are mechanical enough that
splitting them would yield two near-identical PRs with no revertibility
gain). PR1 stays standalone so it is independently reviewable and
revertible.

## Plan

| Builder bead | PR | Files | Blocked by |
|---|---|---|---|
| **`ga-qsmaw2`** | PR1 — doctor-check widening | `internal/doctor/pre_start_scripts_check.go`, `internal/doctor/pre_start_scripts_check_test.go` | (none — ready) |
| **`ga-3b1eba`** | PR2 — script moves + pack edits | `scripts/{worktree-setup.sh,sync-actual-skill.sh,README.md}` (new), 20 `packs/actual/*/pack.toml`, 20 `packs/actual/*/scripts/worktree-setup.sh` (DEL), 6 `packs/actual/*/scripts/sync-actual-skill.sh` (DEL), 6 `packs/actual/*/README.md`, `packs/actual/AGENT_PACK.md` | `ga-qsmaw2` (must merge to main with CI green first) |

### `ga-qsmaw2` — PR1 doctor-check widening (P2, `ready-to-build`)

**Acceptance criteria summary** (full list in the bead body):

- `resolvePreStartScript(cmd, sourceDir, cityPath string) (string, bool)`
  recognizes BOTH `{{.ConfigDir}}` and `{{.CityRoot}}`. When both are
  present, both substitute (order-independent).
- `PreStartScriptsCheck.Run(ctx)` passes `ctx.CityPath` to the helper.
- The success message wording is widened to mention both templates.
- The FixHint mentions both options (pack-shipped or `<city>/scripts/`).
- 4 new test cases: `TestPreStartScriptsCheck_CityRoot_ScriptExists`,
  `*_ScriptMissing`, `*_BothTemplates_OneMissing`,
  `*_CityRoot_OtherTemplateInPath`.
- `TestResolvePreStartScript_TableDriven` with the 7 cases from the design.
- All existing `TestPreStartScriptsCheck_*` cases continue to pass.
- `go test ./...` passes; `go vet ./...` clean.
- PR description references this design bead (`ga-x0pq6s`).

**HARD RULES:**

- **`{{.RigRoot}}` / `{{.WorkDir}}` / `{{.AgentBase}}` recognition is OUT OF
  SCOPE.** Those carry runtime context the static check cannot resolve;
  the existing check correctly skips them in trailing tokens.
- **Error wording unchanged.** `agent %q: pre_start script %q not found`.
- **Removing `{{.ConfigDir}}` recognition is OUT OF SCOPE.** Both
  templates are supported forever.
- **No `cmd/gc/cmd_doctor.go` changes.** Only the helper widens.

### `ga-3b1eba` — PR2 script moves + pack edits (P2, `ready-to-build`)

**Blocked by:** `ga-qsmaw2` (PR1). Wired via `bd dep add ga-3b1eba ga-qsmaw2`.

**Acceptance criteria summary** (full list in the bead body):

- PR1 has merged to main (CI green) — verified before this PR opens.
- `<city>/scripts/worktree-setup.sh` exists (mode `0o755`, md5
  `686f93fa708ca2014161b79bb81a3606`).
- `<city>/scripts/sync-actual-skill.sh` exists (mode `0o755`, md5
  `a8d6985e60393f9561d076efad90d94b`).
- `<city>/scripts/README.md` exists with the pinned content.
- All 20 `packs/actual/*/pack.toml` `pre_start` lines reference
  `{{.CityRoot}}/scripts/worktree-setup.sh` — verified by `grep -rln
  '{{.ConfigDir}}/scripts/worktree-setup' packs/` → 0 hits.
- All 20 `packs/actual/*/scripts/worktree-setup.sh` copies are DELETED.
- All 6 `packs/actual/*/scripts/sync-actual-skill.sh` copies are DELETED.
- 6 `packs/actual/*/README.md` files updated; `AGENT_PACK.md:73-77` block
  updated to single-line form.
- `gc doctor` output (in PR description) shows `pre-start-scripts` at
  `StatusOK`.
- `gc city reload` succeeds; `go test ./...` passes.
- PR contains exactly TWO commits: `pilot-architect-edit`,
  `sweep-remaining-and-delete`.

**HARD RULES:**

- **PR1 must be merged to main BEFORE PR2 opens.** Reviewer audits.
- **TWO COMMITS in PR2.** Per architecture §9 verification sequence: a
  pilot edit (one pack + canonical scripts) followed by the sweep. Allows
  `git revert HEAD~1` to recover cleanly if the sweep is wrong.
- **Move, don't copy.** No symlinks left behind.
- **Mode preservation.** Both `.sh` files MUST ship `0o755`.
- **No `{{.SystemPack:core}}` template syntax** (ga-omxbk3 approach was
  REJECTED by architecture §6 Option B).
- **No changes to `.gc/system/packs/`** (architecture guardrail — auto-sync
  hazard).

## Sequence

`bd dep add ga-3b1eba ga-qsmaw2` is in place so `bd ready` gates PR2 until
PR1 lands. PM slings PR1 to builder now; PR2 will surface to the builder's
Tier-3 query automatically once PR1 closes.

PM closes `ga-omxbk3` (the `{{.SystemPack:core}}` parallel approach,
rejected by architecture §6 Option B) with a reason pointing at this
decomposition and the source architecture.

## Out of scope (deferred follow-ups)

- **`gc internal worktree-setup` subcommand (architecture §7.2.1 / Option F)**
  — upstream gascity SDK change to replace the 212-line shell with a typed
  Go subcommand. File against `gastownhall/gascity`.
- **Upstream actual-factory adopting worktrees** — architecture §7.2.2.
- **Pack-specific `scripts/` cleanup** — many packs still have per-pack
  `scripts/` directories with pack-specific scripts. Those stay.
- **Generalizing the doctor check to `{{.RigRoot}}` / `{{.WorkDir}}`** —
  those require runtime context.
- **Removing `{{.ConfigDir}}` template recognition.** Both supported
  forever.
- **Configurable `<city>/scripts/` location.** Hardcoded `scripts/` matches
  `citylayout.ScriptsRoot`.
- **Detecting drift between identical-shape pre_start lines across packs**
  — architecture §6 Option G, rejected for being reactive.

## Verification (after both PRs land)

```bash
# PR1 verification
grep -n 'func resolvePreStartScript' internal/doctor/pre_start_scripts_check.go
# Expect: signature with (cmd, sourceDir, cityPath string).
grep -c '^func TestPreStartScriptsCheck_CityRoot' internal/doctor/pre_start_scripts_check_test.go
# Expect: >= 3.

# PR2 verification
ls scripts/worktree-setup.sh scripts/sync-actual-skill.sh scripts/README.md
# Expect: all three exist.
md5sum scripts/worktree-setup.sh scripts/sync-actual-skill.sh
# Expect: 686f93fa708ca2014161b79bb81a3606 and a8d6985e60393f9561d076efad90d94b respectively.
grep -rln '{{.ConfigDir}}/scripts/worktree-setup' packs/
# Expect: 0 hits.
find packs/actual -name worktree-setup.sh | wc -l
# Expect: 0.
find packs/actual -name sync-actual-skill.sh | wc -l
# Expect: 0.
gc doctor 2>&1 | grep -A2 'pre-start-scripts'
# Expect: StatusOK.
```

## Builder beads

- **`ga-qsmaw2`** — PR1 doctor-check widening (P2, `ready-to-build`,
  `source:actual-pm`). Routed to `gascity/builder`. No blockers.
- **`ga-3b1eba`** — PR2 script moves + pack edits (P2, `ready-to-build`,
  `source:actual-pm`). Routed to `gascity/builder`. Blocked by `ga-qsmaw2`.

PM also closes `ga-omxbk3` (`{{.SystemPack:core}}` parallel approach) as
superseded by this decomposition.
