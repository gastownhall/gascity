# Account Registry & Quota Rotation

**Status:** Implemented (PR #130)
**Issue:** #19
**Date:** 2026-03-26

## Problem

Gas City agents running sustained multi-agent workloads hit provider rate
limits. When one account is rate-limited, all agents using it stall until
the limit resets. Gas City needs per-session account isolation and rotation
so work continues on a different account.

Gastown solves this with `gt account` + `gt quota`. Gas City needs an
equivalent that is provider-agnostic and fits the city-as-directory model.

## Design Decisions

### Accounts in `.gc/accounts.json`, not `city.toml`

Accounts map handles to **machine-local credential paths** (e.g.,
`~/.claude-work/`). These paths differ per machine and must not be
committed to version control. The portable part — the handle name — lives
in `city.toml` as the `account` field on agents. The registry mapping
handles to paths is local runtime state under `.gc/`.

This mirrors Gastown's `mayor/accounts.json` adapted to city-as-directory.

### Account field as string handle with startup resolution

The `account` field on `Agent` is a short string (e.g., `"work"`) resolved
at session startup via registry lookup → `CLAUDE_CONFIG_DIR` env var. This
is the same indirection pattern as `provider` (string name → resolved
spec). It keeps `city.toml` portable: `account = "work"` means the same
thing on any machine with a "work" account registered.

Resolution chain: agent `env["CLAUDE_CONFIG_DIR"]` > `agent.account` >
`agent_defaults.account` > no account.

### Flock on `quota.json` despite "no status files" principle

CLAUDE.md says "never write status files to track running processes."
Quota state is different — it tracks **external provider state** (rate
limits) that cannot be queried live. There is no API to ask a provider
"am I rate limited?" The only signal is observing session output. This
makes `quota.json` a cache of external reality, not a process tracking
file.

The flock + atomic write pattern follows `nudgequeue.WithState` exactly.

### LRU rotation algorithm

Simplest correct algorithm. The account with the oldest `LastUsed`
timestamp is assigned next. This naturally distributes load and matches
Gastown's proven approach. More sophisticated strategies (weighted,
predicted-reset-aware) can layer on top without changing the interface.

### Rate-limit patterns on ProviderSpec

Provider-agnostic by design. Each provider preset carries its own
`rate_limit_patterns` (regex strings). Claude's built-in preset includes
patterns for known rate-limit messages. Users override per-provider in
`city.toml`. The scanner compiles patterns per-provider and checks session
output against the correct set.

### Progressive activation

Following Gas City's capability model:

- **Level 0-1**: `account` field on agent config, `gc account` commands,
  `CLAUDE_CONFIG_DIR` wiring at session startup
- **Level 6+**: `[quota]` config section, patrol scanning, automatic
  rate-limit detection, `gc quota` commands

A city with no `[quota]` section has zero overhead — the scanner is nil
and `quotaPatrolTick` returns immediately.

## Architecture

```
city.toml                    .gc/accounts.json         .gc/quota.json
┌─────────────┐              ┌──────────────┐          ┌─────────────┐
│ [[agent]]   │              │ accounts: [  │          │ accounts: [ │
│ account =   │──resolve──→  │   {handle,   │          │   {handle,  │
│   "work"    │              │    config_dir}│          │    status,  │
└─────────────┘              │ ]            │          │    limited} │
                             │ default: ... │          │ ]           │
                             └──────────────┘          └─────────────┘
                                    │                        ↑
                                    ▼                        │
                          CLAUDE_CONFIG_DIR           quota patrol
                          set in tmux -e              scans sessions
```

## Files

### New packages
- `internal/account/` — Account, Registry, flock store
- `internal/quota/` — QuotaState, Scanner, rotation logic, flock store

### Modified
- `internal/config/config.go` — Agent.Account, AgentDefaults.Account, QuotaConfig
- `internal/config/patch.go` — AgentPatch.Account
- `internal/config/pack.go` — applyAgentOverride (Account)
- `internal/config/provider.go` — ProviderSpec.RateLimitPatterns
- `cmd/gc/template_resolve.go` — Account → CLAUDE_CONFIG_DIR resolution
- `cmd/gc/city_runtime.go` — quotaPatrolTick, initQuotaScanner
- `cmd/gc/cmd_sling.go` — --account flag

## Not Yet Implemented

- `gc quota scan` / `gc quota rotate` CLI (library logic exists, CLI deferred)
- Auto-rotation in patrol (detects + marks, does not restart sessions yet)
- `gc account switch` (interactive account change, not needed for automated rotation)
