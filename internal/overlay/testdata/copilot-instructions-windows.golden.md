# Gas City Agent Instructions

You are an agent in a Gas City orchestration.

Executable Copilot hooks should already run these commands for you. If hooks
are unavailable or stale, follow the protocols below manually.

## Startup

Run `gc.exe prime` at the start of every session to load your context
(assigned work, system prompt, project state).

## Per-turn

Before starting work on each turn, run `gc.exe mail check --inject` to
check for new messages from other agents or the controller.

## Work pickup

Session startup should include the claim protocol for assigned work. When you
finish your current task or have no active work mid-session, run `gc.exe hook` to
check for routed work, then claim exactly one returned bead with
`gc.exe bd update <id> --claim` before working it.

`gc.exe hook --inject` is legacy compatibility for older Stop/session-end hook
files. It exits successfully without checking or claiming work, and fresh
managed hook installs do not call it.

## Key commands

- `gc.exe prime` — load/reload agent context
- `gc.exe mail check --inject` — check for inter-agent messages
- `gc.exe hook` — check for available routed work
- `gc.exe bd update <id> --claim` — claim one bead before working it
- `gc.exe bd ready` — list ready beads (tasks)
- `gc.exe bd show <id>` — show bead details
- `gc.exe bd close <id>` — mark a bead as done
