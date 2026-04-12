---
title: "Nudge Delivery Modes"
---

> Last verified against code: 2026-04-12

## Summary

Gas City delivers nudges (text pushed into an agent's session to wake or
redirect it) through one of three modes. The right mode depends on the
provider's runtime: whether it exposes a turn-boundary hook, whether it can
report idle, and whether a separate drainer is needed.

This doc is for contributors adding a new provider or routing nudges. It
explains the three modes, which provider capability flag selects which, and
why Claude and Codex require different plumbing.

## Delivery modes

- **`immediate`** — `runtime.Provider.Nudge()` fires the moment the
  caller asks. No waiting, no queueing. Fine for user-invoked `gc nudge`
  against any provider that can accept mid-turn text, but risks fighting
  the model mid-generation.

- **`wait-idle`** — the default. Send blocks until the provider reports
  idle (`runtime.IdleWaitProvider.WaitForIdle()`), then delivers. Claude
  is the only runtime that currently implements `IdleWaitProvider`, so for
  non-Claude providers this mode falls back to enqueuing.

- **`queue`** — the nudge is written to the on-disk queue under
  `.gc/nudges/`. Delivery happens asynchronously via the runtime's drain
  path (see below). Guaranteed delivery across session restarts, retries
  on failure up to `defaultQueuedNudgeMaxAttempts`, bounded by
  `defaultQueuedNudgeTTL`.

Per-call override is the `--delivery` flag on `gc nudge`.
See `cmd/gc/cmd_nudge.go` (`nudgeDeliveryMode`).

## Draining the queue

Once a nudge is queued, it has to actually reach the session. Two paths:

### Hook-driven drain (Claude)

Claude's `UserPromptSubmit` hook fires at the start of every turn. Gas City
runs `gc nudge drain` from that hook, which pops pending items for the
session and pipes them to the provider as a single message. No background
process, no timer — the drain cost is absorbed into the next turn boundary.

This is the preferred model. It has no CPU overhead between turns, no
race between drain and generation, and no chance of delivering into a
dead session.

### Poll-driven drain (Codex today; Amp/Auggie in future)

Providers without a turn-boundary hook need a separate drainer. The
`startNudgePoller` background process periodically checks
`runtime.Provider.GetLastActivity()`, waits for quiescence
(`defaultNudgePollQuiescence`, 3s), and drains queued items when the
session looks idle.

## The `NeedsNudgePoller` capability

`ProviderSpec.NeedsNudgePoller` (and its mirror on `ResolvedProvider`)
controls whether Gas City starts the background poller for a given provider.
Set to `true` when:

- The provider has **no turn-boundary hook equivalent** to Claude's
  `UserPromptSubmit`, AND
- The provider has a working `GetLastActivity()` implementation so the
  poller can detect idle.

Set to `false` when the provider drains via hooks. This is the default.

**Current settings:**

| Provider | `SupportsHooks` | `NeedsNudgePoller` | Drain path                 |
|----------|-----------------|--------------------|----------------------------|
| claude   | true            | false              | `UserPromptSubmit` hook    |
| codex    | true            | **true**           | background poller          |
| gemini   | true            | false              | hook (`BeforeAgent`)       |
| cursor   | true            | false              | hook (`beforeSubmitPrompt`)|
| copilot  | true            | false              | hook (`userPromptSubmitted`)|
| opencode | true            | false              | plugin                     |
| pi       | false           | false              | plugin                     |
| omp      | false           | false              | plugin                     |
| amp      | false           | false              | *no drain today*           |
| auggie   | false           | false              | *no drain today*           |

Providers marked *no drain today* queue nudges that never deliver until a
drain path is added. See the non-Claude parity audit
(`engdocs/archive/analysis/non-claude-provider-parity-audit.md`, gap 4).

## Adding a new provider

1. Build the `ProviderSpec` entry in `internal/config/provider.go`.
2. Pick the drain path:
   - If the provider has a prompt-submit hook, wire `gc nudge drain` into
     the hook config under `internal/hooks/config/`. Leave
     `NeedsNudgePoller` at `false`.
   - If it doesn't have one but does report activity, set
     `NeedsNudgePoller: true`. No other wiring is needed — the poller
     auto-starts from `maybeStartNudgePoller()` at each `gc nudge`,
     `gc sling`, and `gc prime` call.
   - If it has neither, document the gap and consider whether the
     provider can accept immediate nudges mid-turn safely.
3. Add a test in `internal/config/provider_test.go` asserting the
   `NeedsNudgePoller` value.

## Related code

- Delivery dispatch: `cmd/gc/cmd_nudge.go` (`deliverSessionNudgeWithProvider`)
- Queue storage: `internal/nudgequeue/`
- Poller: `cmd/gc/cmd_nudge.go` (`maybeStartNudgePoller`, `ensureNudgePoller`)
- Idle detection: `runtime.Provider.GetLastActivity()`,
  `runtime.IdleWaitProvider.WaitForIdle()`
