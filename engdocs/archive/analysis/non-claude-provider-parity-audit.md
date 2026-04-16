---
title: "Non-Claude Provider Parity Audit"
date: 2026-04-12
status: findings
wasteland: w-7ed35a727f
---

Audit of hook/runtime parity for non-Claude providers in Gas City. Claude
is the reference (all features wired end-to-end); this catalogs where each
other built-in provider diverges, with concrete file pointers so maintainers
can convert items into bugs or patches quickly.

Providers reviewed: `claude`, `codex`, `gemini`, `cursor`, `copilot`, `amp`,
`opencode`, `auggie`, `pi`, `omp` (defined in
[`internal/config/provider.go`](../../../internal/config/provider.go) lines
~225-456, hook configs in [`internal/hooks/config/`](../../../internal/hooks/config/)).

## Severity legend

- **Show-stopper** — feature fails silently for the provider; users hit it on
  the golden path.
- **Friction** — works, but worse UX / inconsistent with Claude.
- **Polish** — minor inconsistency, low user impact.

---

## Gap 1: Session resume is a silent no-op for every non-Claude provider

**Files:** `internal/config/provider.go:225-456`, `cmd/gc/session_reconciler.go:980-1008`

**Severity:** Show-stopper

Only `claude` defines `ResumeFlag` / `ResumeStyle` / `SessionIDFlag`. Every
other builtin (`codex`, `gemini`, `cursor`, `copilot`, `amp`, `opencode`,
`auggie`, `pi`, `omp`) leaves them empty.

`resolveResumeCommand` at `cmd/gc/session_reconciler.go:989-1007` does this
when `ResumeFlag == ""`:

```go
if rp.ResumeFlag == "" {
    return command
}
```

Net effect: on crash or deacon-driven restart, the reconciler appears to
resume but actually launches a fresh process, losing the in-memory session.
No error, no warning — the session just silently becomes a new one.

**To fix:** populate `ResumeFlag` / `ResumeCommand` for each provider that
supports resumption (codex `resume <session-id>`, gemini, etc.), or emit a
diagnostic when `firstStart=false` but the provider has no resume
capability.

---

## Gap 2: `SessionIDFlag` only defined for Claude — breaks "Generate & Pass" strategy for all others

**Files:** `internal/config/provider.go:243` (claude has `SessionIDFlag: "--session-id"`), all other providers omit it. `cmd/gc/session_reconciler.go:980-982`:

```go
if (firstStart || forceFresh) && rp.SessionIDFlag != "" {
    return command + " " + rp.SessionIDFlag + " " + sessionKey
}
```

**Severity:** Show-stopper for providers that *do* have a session-id CLI
(Codex does: `codex --session-id <uuid>`), friction for those that don't.

Without `SessionIDFlag`, Gas City can't pre-assign a session key and has to
discover it after the fact. This matters whenever the reconciler or external
client (Mission Control) needs to address a session by a key it minted.

**To fix:** add `SessionIDFlag` for Codex at minimum; document which
providers genuinely lack this capability and use a fallback discovery
strategy for them.

---

## Gap 3: Missing PreCompact equivalent on Codex and Copilot

**Files:** `internal/hooks/config/claude.json` (has `PreCompact` → `gc handoff
"context cycle"`), `codex.json` (no equivalent), `copilot.json` (no
equivalent), `gemini.json` (has `PreCompress` — probably equivalent), `cursor.json`
(has `preCompact`), `opencode.js` (handles via `session.compacted` event).

**Severity:** Friction / show-stopper depending on runtime

The PreCompact hook is how Gas City cycles a session's context before the
provider auto-compacts (preserving handoff notes, flushing state). Claude,
Gemini, Cursor, and OpenCode all have equivalents wired; **Codex and
Copilot do not**. Long-running Codex/Copilot sessions will silently lose
handoff state on auto-compaction.

**To fix:** audit Codex and Copilot hook APIs for a pre-compaction event
(Codex has `--hooks` docs; Copilot's hook API is still evolving). If the
event exists, wire it. If it doesn't, document the limitation and add a
`gc doctor` warning for long-running sessions on these providers.

---

## Gap 4: `Amp` and `Auggie` cannot receive hook-driven coordination at all

**Files:** `internal/config/provider.go:410-417, 430-437` (both omit
`SupportsHooks`), no hook config files in `internal/hooks/config/`.

**Severity:** Show-stopper for inter-agent coordination

Amp and Auggie are `SupportsHooks: false` (implicit default). That means
none of the Gas City coordination primitives fire for them:

- No `SessionStart` → no `gc prime --hook` → agents start with no context
  or assigned work.
- No `UserPromptSubmit` → `gc mail check` and `gc nudge drain` never run →
  mail and nudges queue forever.
- No `Stop` → `gc hook --inject` never runs → agents sit idle after
  completing work instead of picking up the next item.

Claude's compiled-in assumption (hooks do the work) means Amp/Auggie users
silently get a much worse product.

**To fix options:**
1. Investigate whether Amp/Auggie have any hook-like mechanism (lifecycle
   scripts, startup commands) we can piggy-back on.
2. Add a runtime-side fallback that periodically polls for queued work
   when `!SupportsHooks`. Pi/OMP already use plugin-file mechanisms
   (`.pi/extensions/gc-hooks.js`, `.omp/hooks/gc-hook.ts`); consider a
   similar pattern for Amp/Auggie if their CLIs accept init scripts.
3. If neither is possible, make the limitation visible in `gc doctor` and
   `gc rig add --provider=amp|auggie`.

Copilot is marked `SupportsHooks: true` but ships a fallback `copilot.md`
(manual prompt-level instructions) alongside `copilot.json` — acknowledging
that Copilot hooks may not fire reliably. That "belt and suspenders" pattern
is the right model for Amp/Auggie too.

---

## Gap 5: Hook event vocabulary is inconsistent across providers

Each provider's hook config uses a different event naming:

| Provider | Session start | Per-prompt | Session end | Compact |
|----------|---------------|------------|-------------|---------|
| claude | `SessionStart` | `UserPromptSubmit` | `Stop` | `PreCompact` |
| codex | `SessionStart` | `UserPromptSubmit` | `Stop` | *(missing)* |
| gemini | `SessionStart` | `BeforeAgent` | `SessionEnd` | `PreCompress` |
| cursor | `sessionStart` | `beforeSubmitPrompt` | `stop` | `preCompact` |
| copilot | `sessionStart` | `userPromptSubmitted` | `sessionEnd` | *(missing)* |
| opencode | `session.created` | `experimental.chat.system.transform` | `session.deleted` | `session.compacted` |
| pi | (plugin file) | (plugin file) | (plugin file) | (plugin file) |
| omp | (plugin file) | (plugin file) | (plugin file) | (plugin file) |

**Severity:** Polish (developer/maintenance friction)

This is inherent to each provider's API, not a bug. But there is no single
place in the codebase that documents the mapping, which makes it easy to
forget a hook when adding a new provider (as likely happened with Codex's
missing PreCompact equivalent).

**To fix:** add a maintainer-facing mapping table in
`internal/hooks/config/README.md` (doesn't exist yet) that lists each
logical Gas City event and the per-provider binding, so omissions are
visible at review time.

---

## Gap 6: `PrintArgs` is defined per-provider but has no consumer

**Files:** `internal/config/provider.go:99-103` (field), set for claude, codex,
gemini. No references anywhere under `cmd/gc/`.

**Severity:** Polish

Comment on the field says "Examples: `[-p]` (claude, gemini), `[exec]`
(codex)" — implying one-shot / print mode. Nothing consumes it. Either
it's a planned feature (in which case wire it into a `gc session run
--one-shot` or similar) or dead code. The comment at
`internal/config/provider.go:86-88` on `PermissionModes` has a similar
problem ("no runtime code reads this field"), which suggests this is a
known pattern of staging config ahead of consumers.

**To fix:** wire it, delete it, or leave it as-is with a TODO stating the
intended consumer so the next reader isn't confused.

---

## Gap 7: `InstructionsFile` is used for content lookup but never as a CLI flag or file copy

**Files:** `internal/config/provider.go:62-64` (field), `resolve.go:181-183,
303, 327-330` (set/defaulted), consumed at `cmd/gc/rig_beads.go` (one
reference) and in the `w-d4dba7b056` quality-gate fallback (PR #78).

**Severity:** Friction

`InstructionsFile` is a hint (`"CLAUDE.md"` vs `"AGENTS.md"`) used when
*generating* quality-gate hints for agents. It is **not** used to copy or
generate an actual instructions file in the agent's workdir — if a provider
expects `AGENTS.md` and the repo only ships `CLAUDE.md` (gastown's
convention), non-Claude agents start with no project instructions.

Workaround today: polecats and other agents symlink `AGENTS.md → CLAUDE.md`
at rig setup (see `/home/rome/gt/CLAUDE.md` link target). That's a
gastown-pack convention, not a Gas City primitive.

**To fix:** either document the expected workspace convention (users must
provide the right filename for their provider) or add a Gas City-level
mechanism that reads `InstructionsFile` and stages the content at session
start.

---

## Gap 8: No healthcheck or doctor coverage for per-provider capabilities

**Files:** `cmd/gc/cmd_doctor.go` (doesn't currently surface provider gaps).

**Severity:** Friction

`gc doctor` and `gc doctor --verbose` today check that required binaries
exist, runtime deps are present, and city config resolves. It does *not*
flag:
- `SupportsHooks: false` on a provider the user just added a rig for.
- `ResumeFlag == ""` when the rig's provider would need it for the
  reconciler's resume path.
- Missing hook config files if the provider is `SupportsHooks: true` but
  no file exists for it in `internal/hooks/config/` (would catch future
  drift).

**To fix:** add a `doctor/provider-parity` check that emits warnings for
the above, gated behind `--verbose` or a dedicated `gc doctor providers`
subcommand.

---

## Summary punch list (priority order)

| # | Gap | Severity | Affected providers |
|---|-----|----------|---------------------|
| 1 | Session resume silent no-op | **Show-stopper** | All non-Claude |
| 2 | `SessionIDFlag` missing | **Show-stopper** (Codex) / Friction | All non-Claude |
| 4 | Amp / Auggie have no hook mechanism | **Show-stopper** | amp, auggie |
| 3 | Missing PreCompact equivalent | Friction → Show-stopper long-session | codex, copilot |
| 7 | `InstructionsFile` not materialized in workdir | Friction | All non-Claude |
| 5 | Hook event vocabulary undocumented | Polish | All non-Claude (maint) |
| 6 | `PrintArgs` unused | Polish | codex, gemini (claude) |
| 8 | `gc doctor` misses provider capability gaps | Friction | All non-Claude |

## Not gaps (verified intentional)

- **Claude having the most wiring** is by design; it was first and is the
  reference. The audit is about bringing others *up*, not cutting Claude
  down.
- **`SupportsACP` differing** across providers is correct — ACP genuinely
  isn't supported by most.
- **OpenCode using a plugin file instead of a JSON hook config** is
  intentional: OpenCode's hook API is JS/ESM, not JSON.

## Next steps for maintainers

Ship in this order for biggest user-visible impact:
1. Fix resume for Codex (has a documented `resume` subcommand) — closes
   the most-hit show-stopper.
2. Decide Amp/Auggie strategy (polling fallback vs. first-class "no hooks"
   mode with clear doctor warning).
3. Wire Codex's missing PreCompact equivalent.
4. Stage per-provider instructions files automatically.

Each of these is a self-contained change and a good candidate for its own
wasteland bounty.
