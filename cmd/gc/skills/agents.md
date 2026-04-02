# Agent Management

Agents are the workers in a Gas City workspace. Each runs in its own
session (tmux pane, container, etc).

## Adding agents

```
gc agent add --name <name>             # Add agent to city root
gc agent add --name <name> --dir <rig> # Add agent scoped to a rig
```

## Sessions from templates

Every configured template can now spawn sessions directly.

For cities migrating off the old multi-instance model, see
`engdocs/archive/migrations/remove-agent-multi-migration.md`.

Use the session commands directly:

```
gc session new <template>              # Create and attach to a new session
gc session new <template> --no-attach  # Create a detached background session
gc session suspend <id-or-template>    # Suspend a session
gc session close <id-or-template>      # Close a session permanently
gc session kill <name>                 # Force-kill an agent session
gc session nudge <name> <message...>   # Send text to a running agent session
gc session logs <name>                 # Show session logs for an agent
```

When multiple sessions exist for the same template, use the session ID.

## Pools

Pools still control controller-managed worker capacity. Pool `max`
limits pool-managed workers, not manually created interactive sessions.

## Lifecycle

```
gc agent suspend <name>                # Suspend agent (reconciler skips it)
gc agent resume <name>                 # Resume a suspended agent
```

## Runtime

```
gc runtime drain <name>                # Signal agent to wind down gracefully
gc runtime undrain <name>              # Cancel drain
gc runtime drain-check <name>          # Check if agent has been drained
gc runtime drain-ack <name>            # Acknowledge drain (agent confirms exit)
gc runtime request-restart             # Request graceful restart (reads GC_AGENT env)
```
