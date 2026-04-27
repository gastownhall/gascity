---
name: gc-city
description: City lifecycle — status, start, stop, init
---

# City Lifecycle

A city is a directory containing `city.toml` and `.gc/` runtime state.

## Initialization

```
{{binary}} init                                # Initialize city in current directory
{{binary}} init <path>                         # Initialize city at path
```

## Starting and stopping

```
{{binary}} start                               # Start city under the supervisor
{{binary}} start <path>                        # Start city at path under the supervisor
{{binary}} supervisor run                      # Run the supervisor in the foreground
{{binary}} start --dry-run                     # Preview what would start
{{binary}} stop                                # Stop the current city
{{binary}} restart                             # Stop then start
```

`{{binary}} init` and `{{binary}} start` register the city with the machine supervisor,
ensure it is running, and trigger an immediate reconcile. Interactive
sessions are created separately with `{{binary}} session new <template>`.

## Status

```
{{binary}} status                              # City-wide overview
{{binary}} session list                        # Session / agent status
{{binary}} rig status <name>                   # Rig status
```

## Suspending

```
{{binary}} suspend                             # Suspend entire city
{{binary}} resume                              # Resume suspended city
```

## Configuration

```
{{binary}} config show                         # Show resolved configuration
{{binary}} config explain                      # Show config layering and provenance
{{binary}} doctor                              # Run health checks
```

## Events

```
{{binary}} events                              # Tail the event log
{{binary}} event emit <type> [data]            # Emit a custom event
```

## Dashboard

See `{{binary}} skills dashboard` for full dashboard reference.

## Packs

Packs extend Gas City with additional commands, prompts, formulas, and
doctor checks. Pack commands appear as top-level `{{binary}} <pack> <command>`
subcommands.

```
{{binary}} pack list                           # List installed packs
{{binary}} pack fetch                          # Fetch remote packs
```
