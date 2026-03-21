---
title: Non-Claude Provider Parity Audit
description: Concrete gaps in hook installation, startup detection, and session management for Codex, Copilot, Gemini, Cursor, and other non-Claude providers.
---

> **Status**: Launch-hardening audit conducted 2026-03-20.
> High-severity items are tracked as follow-up issues linked below.
> Update this document when gaps close.

This audit compares Gas City's behavior for non-Claude providers against the
Claude baseline. It distinguishes confirmed gaps from already-landed behavior
so maintainers know what still needs work.

## Summary Punch List

| # | Gap | Severity | Affected providers |
|---|-----|----------|--------------------|
| [1](#1-readypromptprefix-missing-for-most-providers) | No `ReadyPromptPrefix` — ready-state detection fails at startup | **HIGH** | Codex, Gemini, Cursor, OpenCode, Pi, OMP, AMP, Auggie |
| [2](#2-non-claude-hooks-not-auto-installed-at-init) | Non-Claude hooks not auto-installed at `gc init` / `gc start` | **HIGH** | All non-Claude providers |
| [3](#3-resume-support-is-claude-only) | Resume (`--resume` flag / `ResumeFlag`) is Claude-only | **HIGH** | All non-Claude providers |
| [4](#4-sessionidflag-is-claude-only) | `SessionIDFlag` and session-ID tracking are Claude-only | **HIGH** | All non-Claude providers |
| [5](#5-provider-readiness-probes-incomplete) | Provider readiness probes missing for most providers | **MEDIUM** | Copilot, Cursor, OpenCode, Auggie, Pi, OMP, AMP |
| [6](#6-codex-nudge-poller-not-generalized) | Codex nudge poller not generalized to other turn-hookless providers | **MEDIUM** | Gemini, Cursor, OpenCode, Auggie, Pi, OMP, AMP |
| [7](#7-permission-modes-and-optionsschema-sparse) | `PermissionModes` and `OptionsSchema` defined only for Claude/Codex/Gemini | **MEDIUM** | All others |
| [8](#8-acp-support-sparse) | `SupportsACP` only for Claude and OpenCode | **LOW** | All others |

---

## Confirmed Gaps

### 1. ReadyPromptPrefix missing for most providers

**Severity**: HIGH
**File**: `internal/config/provider.go` (provider preset definitions)

`ReadyPromptPrefix` tells the reconciler what prompt text to wait for before
declaring a session ready for input. Only Claude and Copilot have this set:

| Provider | `ReadyPromptPrefix` | `ProcessNames` |
|----------|--------------------|--------------------|
| Claude | `"❯ "` (U+276F) | `["node", "claude"]` |
| Copilot | `"❯ "` (U+276F) | `["copilot"]` |
| Codex | *(empty)* | `["codex"]` |
| Gemini | *(empty)* | `["gemini"]` |
| Cursor | *(empty)* | `["cursor-agent"]` |
| OpenCode | *(empty)* | `["opencode", "node", "bun"]` |
| Auggie | *(empty)* | `["auggie"]` |
| Pi | *(empty)* | `["pi", "node", "bun"]` |
| OMP | *(empty)* | `["omp", "node", "bun"]` |
| AMP | *(empty)* | `["amp"]` |

**Impact**: For all providers with an empty `ReadyPromptPrefix`, the reconciler
cannot detect when the session is ready for input. It falls back to
`ReadyDelayMs` (a fixed wait). This causes launch-week "agent never noticed
the work" failures — the nudge is delivered before the agent is ready.

**Fix**: Identify the prompt string each CLI emits when waiting for input and
add it to the provider preset. If the CLI has no stable ready indicator, set a
`ReadyDelayMs` that covers the typical cold-start time.

---

### 2. Non-Claude hooks not auto-installed at `gc init`

**Severity**: HIGH
**Files**: `cmd/gc/init_artifacts.go:14-29`, `cmd/gc/cmd_init.go:544-550`

During `gc init` and `gc start`, `ensureInitArtifacts` calls
`installClaudeHooks` which installs only the Claude hook files:

```go
// cmd/gc/init_artifacts.go
func ensureInitArtifacts(...) {
    installClaudeHooks(fsys.OSFS{}, cityPath, stderr)  // Claude only
    ...
}
```

Hook installation is complete for all providers — the `internal/hooks` package
can install Codex, Gemini, Copilot, Cursor, OpenCode, Pi, OMP, and AMP hook
files. But `ensureInitArtifacts` never calls `hooks.Install` for non-Claude
providers. Users who configure a non-Claude provider must either:
- Manually trigger hook installation, or
- Have the provider preset listed in `install_agent_hooks` in `city.toml`

Neither of these is documented clearly for launch-week users.

**Fix**: `ensureInitArtifacts` should call `hooks.Install` for the full set of
providers configured in the workspace (or at minimum install all known hook
files on init since they are additive and harmless to co-install).

---

### 3. Resume support is Claude-only

**Severity**: HIGH
**File**: `internal/config/provider.go` (provider preset definitions, fields `ResumeFlag` and `ResumeStyle`)

`ResumeFlag = "--resume"` and `ResumeStyle = "flag"` are set only for Claude.
All other providers have empty values. When `gc wake` or `gc sling` needs to
resume an existing conversation, it uses these fields to construct the resume
invocation.

**Impact**: Resume only works for Claude. Non-Claude providers always start
fresh sessions even when a prior conversation should continue.

**Fix**: Investigate what each CLI offers for conversation continuation:
- Codex: `--continuation` or conversation ID via `CODEX_CONVERSATION_ID`?
- Gemini: `--resume` or session context file?
- Copilot: no documented resume; may need `WakeMode = "new"` as default
- Document findings per provider; add `ResumeFlag` / `ResumeStyle` where
  supported, or explicitly set `WakeMode = "new"` to make the lack of resume
  explicit rather than silently broken.

---

### 4. SessionIDFlag is Claude-only

**Severity**: HIGH
**File**: `internal/config/provider.go` (field `SessionIDFlag`)

`SessionIDFlag = "--session-id"` is set only for Claude. The session ID is
used to correlate sessions across restarts and for event attribution.

**Impact**: For non-Claude providers, session-ID-based features (event
correlation, `gc session list --id`, handoff recovery) don't work.

**Fix**: Identify whether each CLI exposes a session/conversation ID flag or
env var. For CLIs that don't, document the gap explicitly.

---

### 5. Provider readiness probes incomplete

**Severity**: MEDIUM
**File**: `internal/api/handler_provider_readiness.go` lines 31-33

The provider readiness probe handler handles `claude`, `codex`, and `gemini`.
All other providers (Copilot, Cursor, OpenCode, Auggie, Pi, OMP, AMP) return a
default (unprobed) state.

**Fix**: Add readiness checks for providers that have detectable startup state,
or explicitly return `"unknown"` with a message so callers know probing is not
supported.

---

### 6. Codex nudge poller not generalized

**Severity**: MEDIUM
**Files**: `cmd/gc/cmd_nudge.go` (`maybeStartCodexNudgePoller`), `cmd/gc/cmd_prime.go:137-146`

Codex has no turn-based hook mechanism, so it needs a poller to detect queued
nudges and deliver them at the next safe moment. This poller is Codex-specific.
Other providers with similar characteristics (no `UserPromptSubmit` hook, or
similar turn-boundary behavior) — Gemini, Cursor, OpenCode — have the same
limitation but no poller.

**Fix**: Generalize the nudge poller. The trigger condition should be
configurable per provider preset (e.g., a `NudgePollMode` field) rather than
hardcoded to Codex. The mechanism is already correct; just needs to be
provider-agnostic.

---

### 7. PermissionModes and OptionsSchema sparse

**Severity**: MEDIUM
**File**: `internal/config/provider.go`

`PermissionModes` (available permission level flags) and `OptionsSchema`
(feature selection UI) are only defined for Claude, Codex, and Gemini.

**Impact**: The Mission Control UI cannot show configurable options for other
providers. Not a launch blocker — these are progressive-enhancement features.

---

### 8. ACP support sparse

**Severity**: LOW
**File**: `internal/config/provider.go` (field `SupportsACP`)

`SupportsACP = true` only for Claude and OpenCode. ACP (Agent Client Protocol)
is a structured JSON-RPC over HTTP interface. Most CLIs do not expose it, so
this gap is expected and not actionable until those CLIs add ACP support.

---

## Already-Landed (Not Gaps)

The following were flagged in the upstream audit but are now confirmed complete:

| Item | Status |
|------|--------|
| Hook file installation for Codex, Gemini, Copilot, Cursor, OpenCode, Pi, OMP, AMP | ✓ Complete — `internal/hooks/hooks.go` |
| `InstructionsFile` defaults for all providers | ✓ Complete — all presets set explicitly; fallback is `AGENTS.md` |
| `AgentHasHooks` logic handles non-Claude via `install_agent_hooks` list | ✓ Complete — documented in `internal/config/resolve.go:192-216` |

---

## Follow-Up Issues Filed

Issues have been filed in the gascity repo for the high-severity gaps:

- `ReadyPromptPrefix` coverage: see issue tracker
- Non-Claude hook auto-install at `gc init`: see issue tracker
- Resume/SessionIDFlag for non-Claude: see issue tracker
