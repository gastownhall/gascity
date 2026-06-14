# Hook Event Vocabulary

Gas City wires per-provider hook configs into a small set of coordination
commands (`gc prime --hook`, `gc handoff --auto`, `gc nudge drain --inject`,
`gc mail check --inject`). Each provider names its hook events differently;
this document maps Gas City's canonical events to the provider's native
name for each, plus where the wiring lives on disk.

The mapping exists primarily so future contributors can audit coverage
gaps at a glance — see [`gastownhall/gascity#672`](https://github.com/gastownhall/gascity/issues/672)
("non-Claude provider parity") for the audit that motivated it.

## File layout

Provider hook configs live in two places:

- `internal/hooks/config/claude.json` — Claude-specific settings (this directory).
- `internal/bootstrap/packs/core/overlay/per-provider/<provider>/…` — every other
  provider, scoped under that provider's expected dotfile path
  (e.g. `codex/.codex/hooks.json`, `cursor/.cursor/hooks.json`).

Installation walks the pack overlay during `gc start` / `gc rig boot`,
materializing the per-provider files into each agent's working directory.

## Event mapping

✓ = wired today. — = not wired (either the provider does not expose
the event, or it does but Gas City has not opted in yet).

| Canonical event | claude | codex | cursor | copilot | gemini | antigravity | opencode | omp | pi | kimi |
|---|---|---|---|---|---|---|---|---|---|---|
| session start    | `SessionStart` ✓ | `SessionStart` ✓ | `sessionStart` ✓ | `sessionStart` ✓ | `SessionStart` ✓ | `PreInvocation` ✓ | `session.created` ✓ | `session_start` ✓ | `session_start` ✓ | `SessionStart` ✓ |
| pre-compaction   | `PreCompact` ✓   | `PreCompact` ✓   | `preCompact` ✓   | `preCompact` ✓   | `PreCompress` ✓  | — | `session.compacted` ✓ | `session_compact` ✓ | `session_compact` ✓ | — |
| user prompt submit | `UserPromptSubmit` ✓ | `UserPromptSubmit` ✓ | `beforeSubmitPrompt` ✓ | `userPromptSubmitted` ✓ | — | — | — | — | — | — |
| before agent run | —                | —                | —                | —                | `BeforeAgent` ✓  | `PreInvocation` ✓ | —                | `before_agent_start` ✓ | `before_agent_start` ✓ | — |

### Gas City command bindings

For each provider where a row above is ✓, the wired command is one of:

- **session start** → `gc prime --hook` (loads context, drains hooks).
- **pre-compaction** → `gc handoff --auto "context cycle"` (capture state
  before the provider compacts the conversation).
- **user prompt submit** / **before agent run** → `gc nudge drain --inject`
  and/or `gc mail check --inject` (inject pending agent-to-agent messages
  into the upcoming prompt).

Some providers fold both injection commands into a single hook entry;
others split them. The exact wiring lives in the per-provider config —
this README only documents the event vocabulary, not the command shape.

Antigravity currently exposes `PreInvocation` for before-model-call
injection. Gas City wires prime, nudge-drain, and mail-check through
separate named `PreInvocation` hooks in `.agents/hooks.json`; no
pre-compaction hook is installed because Antigravity does not expose one.

## Adding a new provider hook

1. Find the provider's native event name in its documentation. Do not
   guess — wiring a non-existent event silently no-ops and looks fine in
   review.
2. Add an entry to the provider's hook config file under the right path
   (see "File layout" above). For new providers, create the directory
   under `internal/bootstrap/packs/core/overlay/per-provider/<provider>/`.
3. Update the event table above with the new row or column.
4. If the provider supports `BuiltinProviderSpec.SupportsHooks` in
   `internal/worker/builtin/profiles.go`, flip it to `true` for that
   provider.

## Known gaps

- **kiro pre-compaction** — Kiro's hook config (under
  `.kiro/agents/gascity.json`) wires `agentSpawn` and `userPromptSubmit`
  but has no pre-compaction event. Kiro does not currently document a
  hook fired before context compaction; add a row here and wire
  `gc handoff --auto` if/when Kiro exposes one. Tracked under the parent
  audit (#672 gap 3).
