---
name: gc-agents
description: Managing agents — list, peek, nudge, suspend, drain
---

# Agent Management

Agents are the workers in a Gas City workspace. Each runs in its own
session (tmux pane, container, etc).

## Adding agents

```
{{binary}} agent add --name <name>             # Scaffold agents/<name>/prompt.template.md
{{binary}} agent add --name <name> --dir <rig> # Scaffold a rig-scoped agent.toml
{{binary}} agent add --name <name> --prompt-template <file>
```

## Sessions from templates

Every configured template can now spawn sessions directly.

For cities migrating off the old multi-instance model, see
`engdocs/archive/migrations/remove-agent-multi-migration.md`.

Use the session commands directly:

```
{{binary}} session new <template>              # Create and attach to a new session
{{binary}} session new <template> --no-attach  # Create a detached background session
{{binary}} session suspend <id-or-template>    # Suspend a session
{{binary}} session close <id-or-template>      # Close a session permanently
{{binary}} session kill <name>                 # Force-kill an agent session
{{binary}} session nudge <name> <message...>   # Send text to a running agent session
{{binary}} session logs <name>                 # Show session logs for an agent
```

When multiple sessions exist for the same template, use the session ID.

## Pools

Pools still control controller-managed worker capacity. Pool `max`
limits pool-managed workers, not manually created interactive sessions.

## Lifecycle

```
{{binary}} agent suspend <name>                # Suspend agent (reconciler skips it)
{{binary}} agent resume <name>                 # Resume a suspended agent
```

## Runtime

```
{{binary}} runtime drain <name>                # Signal agent to wind down gracefully
{{binary}} runtime undrain <name>              # Cancel drain
{{binary}} runtime drain-check <name>          # Check if agent has been drained
{{binary}} runtime drain-ack <name>            # Acknowledge drain (agent confirms exit)
{{binary}} runtime request-restart             # Request graceful restart (reads GC_AGENT env)
```
