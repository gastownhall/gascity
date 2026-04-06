# Work Items (Beads)

Everything in Gas City is a bead — tasks, messages, molecules, convoys.
The `bd` CLI is the primary interface for bead CRUD.

## Rig-scoped beads

Each rig has its own `.beads/` database with its own ID prefix (e.g.
`fe-` for frontend, `be-` for beads). **A bead must live in the
same database as the agent that will work on it.** When you sling a bead
to a rig-scoped agent, sling operates on the agent's rig database — so
the bead must already exist there. The bead ID prefix tells you which
rig it belongs to.

Use `gc rig list` to see rig names, paths, and prefixes.

## Creating work

**Use `--rig` to create beads in the right database.** If the work will
be dispatched to a rig-scoped agent, create the bead in that agent's rig:

```
bd create "title" --rig frontend         # Create in frontend's db (fe- prefix)
bd create "title" --rig beads            # Create in beads db (be- prefix)
bd create "title"                        # Create in current directory's .beads/
bd create "title" -t bug                 # Create with type
bd create "title" --label priority=high  # Create with labels
```

## Finding work

```
bd list                                # List beads in current .beads/
bd list --dir /path/to/rig             # List beads in a specific rig
bd ready                               # List beads available for claiming
bd ready --label role:worker           # Filter by label
bd show <id>                           # Show bead details
```

## Claiming and updating

```
bd update <id> --claim                 # Claim a bead (sets assignee + in_progress)
bd update <id> --status in_progress    # Update status
bd update <id> --label <key>=<value>   # Add/update labels
bd update <id> --note "progress..."    # Add a note
```

## Closing work

```
bd close <id>                          # Close a completed bead
bd close <id> --reason "done"          # Close with reason
```

## Hooks

```
gc hook show <agent>                   # Show what's on an agent's hook
gc agent claim <agent> <id>            # Put a bead on an agent's hook
```
