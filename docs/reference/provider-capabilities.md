---
title: Provider Capability Matrix
description: What each Gas City session provider supports — core operations, optional extensions, and known gaps.
---

Gas City supports several session providers selectable via the `GC_SESSION`
environment variable or the `session` field on a provider preset. This page
documents what each provider can reliably do so contributors can avoid
trial-and-error discovery of gaps.

## How to read this matrix

Each row is a capability. Each column is a provider. Values:

| Symbol | Meaning |
|--------|---------|
| ✓ | Supported and reliable |
| ~ | Best-effort or partial (details in footnotes) |
| — | Not supported; returns zero/nil/false without error |
| ✗ | Not applicable (the interface method itself is absent) |

## Session Providers

| `GC_SESSION` value | Description |
|--------------------|-------------|
| `tmux` | tmux multiplexer (production default) |
| `subprocess` | In-process subprocess (lightweight, no tmux) |
| `exec:<script>` | Script-delegated backend (bring-your-own multiplexer) |
| `acp` | ACP protocol (structured I/O over HTTP) |
| `auto` | Wraps tmux + ACP; routes by session type |
| `hybrid` | Wraps local + remote provider pair |
| `k8s` | Kubernetes pod sessions |
| `fake` | In-memory test double |

## Core `Provider` Interface

All providers implement the full `runtime.Provider` interface. The table below
shows which methods return meaningful results vs. silent no-ops.

| Capability | tmux | subprocess | exec | acp | auto | hybrid | k8s |
|------------|:----:|:----------:|:----:|:---:|:----:|:------:|:---:|
| `Start` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `Stop` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `Interrupt` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `IsRunning` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `IsAttached` ¹ | ✓ | — | — | — | — | — | — |
| `Attach` | ✓ | — | — | — | — | — | — |
| `ProcessAlive` | ✓ | ✓ | ~ ² | — | ✓ | ✓ | — |
| `Nudge` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `Peek` | ✓ | ✓ | ~ ³ | ✓ | ✓ | ✓ | ✓ |
| `SetMeta / GetMeta / RemoveMeta` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `ListRunning` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `GetLastActivity` ⁴ | ✓ | — | ~ ³ | — | — | — | ✓ |
| `ClearScrollback` | ✓ | — | ~ ³ | — | — | — | — |
| `CopyTo` | ✓ | — | ~ ³ | — | — | — | ✓ |
| `SendKeys` | ✓ | — | ~ ³ | — | ✓ | ✓ | — |
| `RunLive` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |

## `ProviderCapabilities` Flags

These flags are read by the reconciler to skip wake-reason checks that a
provider cannot support.

| Flag | tmux | subprocess | exec | acp | auto | hybrid | k8s |
|------|:----:|:----------:|:----:|:---:|:----:|:------:|:---:|
| `CanReportAttachment` | ✓ | — | — | — | ✓ ⁵ | ✓ ⁵ | — |
| `CanReportActivity` | ✓ | — | — | — | ✓ ⁵ | ✓ ⁵ | ✓ |

## Optional Extension Interfaces

Providers implement these interfaces when the capability is available.
The reconciler uses type assertions to check at runtime.

| Interface | Method | tmux | subprocess | exec | acp | auto | hybrid | k8s |
|-----------|--------|:----:|:----------:|:----:|:---:|:----:|:------:|:---:|
| `IdleWaitProvider` | `WaitForIdle` | ✓ | ✗ | ✗ | ✗ | ✓ | ✓ | ✗ |
| `InteractionProvider` | `Pending` / `Respond` | ✗ | ✗ | ✗ | ✓ | ✓ | ✓ | ✗ |

## Footnotes

¹ `IsAttached` returns `false` for all providers except tmux. The reconciler
skips attachment-gated wake reasons when `CanReportAttachment` is false.

² `exec` delegates `ProcessAlive` to the backing script via the
`process-alive` operation. Behavior depends on the script implementation;
the built-in `gc-session-screen` script supports it but custom scripts may
return `false` unconditionally.

³ `exec` delegates all operations to the backing script. The operations
`peek`, `get-last-activity`, `clear-scrollback`, `copy-to`, and `send-keys`
are defined in the exec script protocol — see
[Exec Session Provider](exec-session-provider.md) — but support depends on
the script. Gas City's built-in tmux-backend script supports all of them.

⁴ `GetLastActivity` returns meaningful timestamps from tmux's
`display-message` and from Kubernetes pod activity, but returns zero time for
subprocess and ACP providers (no scrollback / no activity observable).

⁵ `auto` and `hybrid` compute capabilities as the logical AND of their
component providers. If both components support a capability, the composite
does too. In practice: `auto` (tmux + ACP) inherits tmux's attachment and
activity reporting; `hybrid` (local + remote) depends on what each side is.

## Choosing a Provider

| Scenario | Recommended provider |
|----------|---------------------|
| Production: interactive agents with tmux | `tmux` |
| Production: ACP-native agents (structured I/O) | `acp` or `auto` |
| Production: agents in Kubernetes | `k8s` |
| Development: fast, no tmux required | `subprocess` |
| Development: custom multiplexer or terminal | `exec:<script>` |
| CI / unit tests | `fake` (via `GC_SESSION=fake`) |
| CI / acceptance tests | `subprocess` or `fake` |
| Multi-machine or remote execution | `hybrid` |

## Gap Summary

These capabilities are the most common sources of unexpected behavior when
switching providers:

- **`IsAttached` is tmux-only.** Code that gates on `IsAttached` silently
  falls back to `false` on all other providers.
- **`WaitForIdle` is tmux / auto / hybrid only.** Providers without
  `IdleWaitProvider` cannot wait for a safe injection window; nudges are
  delivered immediately.
- **`Pending` / `Respond` is ACP-based only.** Structured approval flows
  require ACP, auto, or hybrid.
- **`Peek` is best-effort on exec.** Scripts that don't implement the `peek`
  operation return an empty string.
- **`GetLastActivity` is meaningful on tmux and k8s only.** Activity-gated
  wake reasons are skipped on other providers.
