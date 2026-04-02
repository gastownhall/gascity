# Gas Town Architecture

## Overview

Gas Town is a multi-agent orchestration system where Claude Code instances coordinate work across product rigs. One town runs on a single machine, with agents spawned as tmux sessions.

## Agent Roles

| Role | Scope | Count | Purpose |
|------|-------|-------|---------|
| Mayor | City | 1 | Coordinator. Dispatches work, reviews, merges. Human-facing. |
| Deacon | City | 1 | Automated patrol. Spawns witnesses, runs plugins, monitors health. |
| Witness | Rig | 1/rig | Lifecycle manager. Monitors polecat health, recovers orphans. |
| Refinery | Rig | 1/rig | Merge processor. Rebases, tests, merges PRs. |
| Polecat | Rig | N/rig | Disposable workers. Spawned per-bead. Exit when done. |
| Crew | Rig | N/rig | Persistent workers. Domain expertise. |
| Dog | City | N | Short-lived helpers for deacon patrol tasks. |
| Boot | City | 1 | Ephemeral deacon watchdog. |

## Data Flow

```
Human → Mayor → gt sling → Polecat (writes code)
                         → Refinery (merges)
                         → Witness (monitors)
       Deacon → patrol → spawns witnesses/dogs → runs plugins
```

## Communication

| Method | Persistence | Use |
|--------|------------|-----|
| gt mail | Permanent (Dolt) | Work assignments, escalations |
| gt nudge | Ephemeral | Status checks, pings |
| DoltHub sync | Permanent | Cross-town (mesh) |

## Key Infrastructure

| Component | Port | Purpose |
|-----------|------|---------|
| Dolt | 3307 | SQL database for beads, mail, state |
| Gitea | 3300 | Git hosting (replaces GitHub for agents) |
| Daemon | — | Process manager, plugin scheduler |

## Automation Layers

1. **Hooks** (Claude Code) — SessionStart, Stop, PreCompact, UserPromptSubmit, PreToolUse
2. **Plugins** (Deacon patrol) — 14 plugins on cooldown gates
3. **Crons** (crontab) — Thread guardrail, log rotation, knowledge evolution
4. **Formulas** (Molecules) — Multi-step workflow templates for agents
