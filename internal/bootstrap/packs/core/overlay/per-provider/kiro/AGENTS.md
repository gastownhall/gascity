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

When you finish your current task or have no active work, run
`gc hook --claim` to find and claim one routed work item in a single step.
Plain `gc hook` only lists routed work; it claims nothing. If you instead
claim a bead `gc hook` returned with `gc bd update <id> --claim`, a failed
claim means another agent holds that bead: do not work it.

## Key commands

- `gc prime` — load/reload agent context
- `gc nudge drain --inject` — drain queued nudges
- `gc mail check --inject` — check for inter-agent messages
- `gc hook` — check for available routed work
- `gc hook --claim` — find and claim one routed work item
- `gc bd update <id> --claim` — claim one bead before working it; fails if another agent holds it
- `gc bd ready` — list ready beads (add `--include-ephemeral` only in bd 1.0.5+ cities)
- `gc bd show <id>` — show bead details
- `gc bd close <id>` — mark a bead as done
