---
name: gc-rigs
description: Managing rigs — add, list, status, suspend, resume
---

# Rig Management

A rig is a project directory registered with the city. Agents can be
scoped to rigs via the `dir` field.

## Beads

Each rig has its own `.beads/` database with a unique prefix (e.g.
`hw-` for hello-world). To create or query beads for a rig, run `bd`
from the rig directory or pass `--dir`:

```
bd create "title" --dir /path/to/rig   # Create in rig's database
bd list --dir /path/to/rig             # List rig's beads
```

Running `bd` from the city root hits the city-level `.beads/`, not
the rig's. Use `gc rig list` to find rig paths.

## Rig-Local Custom Workers

Custom workers that combine `start_command`, `work_query`, and `work_dir`
must query the same bead store where routed work is created. A common failure
looks like "No routed work found" even though a bead has
`gc.routed_to=<worker>`: the worker is running `bd` from the city root, so it is
looking in the city `.beads/` database instead of the rig `.beads/` database.

Prefer one of these patterns for rig-scoped workers:

```
# Run the worker from the rig root so plain `bd ...` discovers the rig DB.
dir = "frontend"
work_dir = "{{.RigRoot}}"
start_command = "scripts/custom-worker.sh"
work_query = "bd ready --metadata-field gc.routed_to={{.Rig}}/{{.AgentBase}} --unassigned --json --limit=1"
```

```
# Or keep another working directory but force bd to the rig path.
dir = "frontend"
work_dir = ".gc/worktrees/{{.Rig}}/{{.AgentBase}}"
start_command = "scripts/custom-worker.sh"
work_query = "bd ready --dir {{.RigRoot}} --metadata-field gc.routed_to={{.Rig}}/{{.AgentBase}} --unassigned --json --limit=1"
```

When debugging a custom worker lane:

- run `gc rig list` to confirm the rig path and prefix
- create or inspect rig work with `gc bd --rig <rig> ...` or `bd --dir <rig-path> ...`
- make `work_query` use `{{.RigRoot}}` when it shells out from a city or worktree path
- keep `sling_query` and `work_query` pointed at the same rig database

## Convention

The canonical location for rigs is `<city-root>/rigs/<rig-name>`. Always
use this path unless the user explicitly provides an alternative. Do not
create rigs at the city root or as siblings of the city directory.

If the user asks to create a rig but does not specify where, **ask them**
before proceeding: confirm the `rigs/` convention and offer the choice of
a custom path. Do not silently pick a location.

## Adding and listing

```
gc rig add <path>                      # Register a directory as a rig
gc rig list                            # List all registered rigs
```

## Status and inspection

```
gc rig status <name>                   # Show rig status, agents, health
gc status                              # City-wide overview (includes rigs)
```

## Suspending and resuming

```
gc rig suspend <name>                  # Suspend rig (all its agents stop)
gc rig resume <name>                   # Resume a suspended rig
```

## Restarting

```
gc rig restart <name>                  # Restart all agents in a rig
gc restart                             # Restart entire city
```
