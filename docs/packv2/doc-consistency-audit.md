# Consistency Audit: Directory Conventions and TOML Structure — RETIRED

> **Status: Retired.** All findings have been folded into the canonical spec docs:
> - Orders → top-level `orders/` (doc-directory-conventions.md, doc-pack-v2.md, doc-loader-v2.md)
> - `[formulas].dir` → fixed convention (doc-directory-conventions.md, doc-pack-v2.md)
> - `[agents]` alias → `[agent_defaults]` canonical (doc-agent-v2.md)
> - `overlay/` (singular) → implementation cleanup
> - Fallback agents → moot (fallback removed in V2)
> - Doctor/commands → convention-based dirs (doc-commands.md, doc-directory-conventions.md)
>
> This file is kept for historical reference only. Do not update it.

# Original Audit (historical)

**GitHub Issue:** *(to be filed)*

Title: `feat: Consistency audit — directory conventions and TOML structure`

The big-rock redesigns (pack/city model [#360](https://github.com/gastownhall/gascity/issues/360), agent definitions [#356](https://github.com/gastownhall/gascity/issues/356)) are tracked separately. This issue audits everything else for consistency, assuming the agent-as-directory model from #356 is adopted.

---BEGIN ISSUE---

## Context

Gas City has three models for named definitions:

1. **Table in a TOML file** — the definition is inline TOML (`[[named_session]]`, `[providers.claude]`)
2. **Directory of named files** — one file per definition, name from filename (`formulas/pancakes.formula.toml`)
3. **Directory of directories** — complex entity with multiple associated files (`agents/mayor/`)

The agent redesign (#356) moves agents from model 1 to model 3. This audit checks every other definition type: does it use the right model, and is it consistent?

## The full inventory

Every user-declarable definition in Gas City, with its current pattern:

### Singleton TOML blocks (city.toml)

These are single-instance settings. Pure TOML is the right pattern — no issues.

| Block | Purpose |
|---|---|
| `[workspace]` | City metadata, default provider |
| `[daemon]` | Patrol interval, restart policy, wisp GC |
| `[beads]` | Bead store provider |
| `[session]` | Session provider, timeouts, K8s/ACP sub-configs |
| `[mail]` | Mail provider |
| `[events]` | Events provider |
| `[dolt]` | Dolt host/port |
| `[api]` | API port, bind address |
| `[chat_sessions]` | Idle timeout |
| `[session_sleep]` | Sleep policies by session class |
| `[convergence]` | Max convergence per agent/total |
| `[orders]` | Skip list, max timeout |
| `[agent_defaults]` | Model, wake mode, overlay defaults |
| `[formulas]` | Formula directory path |

**One inconsistency:** `[formulas].dir` is configurable but every other convention-based directory (`scripts/`, `commands/`, `doctor/`) is discovered by fixed name. Should `formulas/` be a fixed convention too?

### Singleton TOML blocks (pack.toml)

| Block | Purpose |
|---|---|
| `[pack]` | Pack name, schema version, requirements |
| `[global]` | Pack-wide session_live commands |
| `[agents]` (v.next) | Pack-wide agent defaults |

No issues — these are metadata.

### Collections declared as TOML arrays-of-tables

| Definition | TOML | Also has files? | Pattern |
|---|---|---|---|
| **Agents** | `[[agent]]` | prompt, overlay, namepool | Hybrid — **redesigned in #356** |
| **Named sessions** | `[[named_session]]` | none | Pure TOML — fine as-is, they reference agents by name |
| **Rigs** | `[[rigs]]` | external project dir | Hybrid (TOML + path binding) — **addressed in #360** |
| **Services** | `[[service]]` | runtime state in `.gc/` | Pure TOML — fine as-is |
| **Providers** | `[providers.<name>]` | none | Pure TOML — fine as-is |
| **Patches** | `[[patches.agent]]` etc. | optional prompt file in `patches/` | Hybrid — **addressed in #356** |

### Convention-based directories

These are discovered by scanning a directory. No TOML declaration needed.

| Directory | File pattern | Provides identity from... | Consistent? |
|---|---|---|---|
| `agents/` (v.next) | `<name>/prompt.md` | Directory name | Yes — #356 |
| `formulas/` | `<name>.formula.toml` | Filename | **Yes** |
| `orders/` | `<name>/order.toml` | **Directory name** | **No — see below** |
| `scripts/` | `<path>.sh` | Path | Yes |
| `prompts/` | `<name>.md.tmpl` | Filename | Being replaced by `agents/<name>/prompt.md` |
| `overlays/` | directory tree | N/A (copied wholesale) | Being replaced by per-agent + per-provider |
| `namepools/` | `<name>.txt` | Filename | Being replaced by `agents/<name>/namepool.txt` |
| `template-fragments/` (v.next) | `<name>.md.tmpl` | Filename | Yes — #356 |
| `skills/` (v.next) | `<name>/SKILL.md` | Directory name | Yes — #356 |
| `mcp/` (v.next) | `<name>.toml` | Filename | Yes — #356 |
| `commands/` | See below | | **Hybrid — see below** |
| `doctor/` | See below | | **Hybrid — see below** |

## Issues found

### 1. Orders: wrong structure, wrong location

**Two problems in one.**

Orders use `formulas/orders/<name>/order.toml` — a subdirectory per order containing a single file. No order directory in the codebase contains anything besides `order.toml`. Meanwhile, formulas use flat files: `pancakes.formula.toml`.

Additionally, orders can live in `formulas/orders/` or top-level `orders/`, but only cities support both — packs only support `formulas/orders/`.

Orders aren't formulas — they *reference* formulas. They schedule dispatch; formulas define workflow.

**Suggestion:**
- Standardize on top-level `orders/` for both cities and packs
- Adopt flat files: `orders/<name>.order.toml` (matches formula convention)
- Deprecate `formulas/orders/` nesting

### 2. Doctor checks and commands: leave alone for now

Doctor checks (`[[doctor]]`) and commands (`[[commands]]`) are both hybrid — TOML metadata + script file references. They *could* become pure convention (model 2), but there's an open question: can two definitions share a script with different arguments? Today they don't, but the pattern is plausible (e.g., `check-provider.sh --provider claude` vs. `check-provider.sh --provider codex`). If sharing is needed, TOML is the right place to express it.

These are model 1 today and it works. Revisit if the shared-script question resolves.

### 4. `[formulas].dir` is the odd one out

Every convention-based directory is discovered by fixed name: `scripts/`, `commands/`, `doctor/`, `overlays/`, `agents/` (v.next). But `formulas/` has a configurable path via `[formulas].dir`.

**Suggestion:** Make `formulas/` a fixed convention. If someone needs formulas elsewhere, that's what packs and imports are for.

### 5. Gastown has dead `overlay/` directory

The gastown pack has both `overlay/` (singular) and `overlays/` (plural). Only `overlays/` is referenced in pack.toml. The `overlay/` directory appears unused but is still embedded via `embed.go`.

**Suggestion:** Remove `overlay/` from gastown pack and update embed.go.

### 6. Fallback agents: inconsistent prompt requirements

The dolt pack's fallback dog agent has no `prompt_template`. The maintenance pack's fallback dog has one. Both are `fallback = true`. It's unclear whether prompts are required, optional, or have different semantics for fallback agents.

**Suggestion:** Clarify and document. If prompts are optional for fallbacks (reasonable — they might just inherit), make that explicit.

## Summary

Three models for named definitions:

| Model | When to use | Examples |
|---|---|---|
| **1. TOML table** | Singleton settings, lightweight declarations, definitions that may share scripts | `[daemon]`, `[[named_session]]`, `[[doctor]]`, `[[commands]]` |
| **2. Directory of named files** | Collections where each definition is one file | `formulas/<name>.formula.toml`, `orders/<name>.order.toml` |
| **3. Directory of directories** | Complex entities with multiple associated files | `agents/<name>/`, `skills/<name>/` |

**What changes:**
- Orders move from model 3 (directory per order) to model 2 (flat files), and from `formulas/orders/` to top-level `orders/`
- Agents move from model 1 to model 3 (#356)
- `[formulas].dir` becomes a fixed convention
- Doctor checks and commands stay model 1 (revisit if shared-script pattern emerges)

---END ISSUE---
