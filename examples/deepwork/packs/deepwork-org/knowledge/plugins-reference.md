# Plugins Reference — Deacon Patrol Plugins

Plugins run during deacon patrol on cooldown gates. Each is a markdown file with shell commands the deacon executes.

## Active Plugins (14)

| Plugin | Cooldown | Purpose |
|--------|----------|---------|
| stuck-agent-dog | 5m | Detects crashed/stuck polecats and deacons, restarts them |
| dolt-backup | 15m | Smart Dolt backup with change detection |
| compactor-dog | 30m | Monitors Dolt commit growth, escalates when compaction needed |
| rebuild-gt | 1h | Rebuilds gt binary when source is newer than installed |
| dolt-archive | 1h | Offsite backup: JSONL snapshots + dolt push to remotes |
| submodule-commit | 2h | Auto-commits accumulated submodule changes |
| github-sheriff | 2h | Monitors GitHub CI on open PRs (BROKEN: GitHub suspended) |
| quality-review | 6h | Reviews merge quality, tracks per-worker trends |
| dolt-log-rotate | 6h | Rotates Dolt server log when exceeding size threshold |
| gitignore-reconcile | 6h | Auto-untracks files matching .gitignore |
| git-hygiene | 12h | Cleans stale branches, stashes, loose git objects |
| knowledge-evolve | 12h | Harvests lessons from closed beads into knowledge base |
| dolt-snapshots | event | Tags Dolt DBs at convoy boundaries for audit/rollback |
| tool-updater | 168h (weekly) | Upgrades bd and dolt via Homebrew |

## Plugin Format

```toml
+++
name = "plugin-name"
description = "What it does"
version = 1

[gate]
type = "cooldown"  # or "event"
duration = "6h"

[tracking]
labels = ["plugin:name", "category:infra"]
digest = true

[execution]
timeout = "5m"
notify_on_failure = true
severity = "medium"
+++

# Plugin Title

Step-by-step instructions with bash code blocks.
Deacon executes each step in order.
```

## Known Issues

- **github-sheriff** is broken (GitHub account suspended). Should be disabled or converted to Gitea.
- Plugins require LLM (deacon). The reference implementation has "exec orders" that run without LLM.
- If deacon is down, no plugins run. The cron-based knowledge-evolve provides a fallback path.
