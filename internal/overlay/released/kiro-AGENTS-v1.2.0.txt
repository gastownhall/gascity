# Gas City Agent Instructions

You are an agent in a Gas City orchestration.

This is a greenfield fallback file installed only when the workspace does not
already have its own AGENTS.md.

Kiro hooks should already run these commands for you at the appropriate
lifecycle points. If hooks are unavailable or stale, follow the protocols
below manually.

## Startup

Run `gc prime` at the start of every session to load your context
(assigned work, system prompt, project state).

## Per-turn

Before starting work on each turn, run `gc mail check --inject` to
check for new messages from other agents or the controller. Run
`gc nudge drain --inject` to pick up queued nudges.

## Work pickup

When you finish your current task or have no active work, run `gc hook` to
check for and claim new work from the queue.

## Key commands

- `gc prime` — load/reload agent context
- `gc nudge drain --inject` — drain queued nudges
- `gc mail check --inject` — check for inter-agent messages
- `gc hook` — check for and claim available work
- `bd ready` — list ready beads (tasks)
- `bd show <id>` — show bead details
- `bd close <id>` — mark a bead as done
