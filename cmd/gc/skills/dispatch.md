# Dispatching Work

`gc sling` routes work to agents. **Pools are valid targets** — sling to
the pool and any idle member can claim the work. You do NOT need to find
or create an individual agent first.

## Quick reference

```
gc sling <bead-id>                     # Auto-target via rig's default_sling_target
gc sling <agent-or-pool> <bead-id>     # Route to a specific agent or pool
gc sling <agent-or-pool> -f <formula>  # Instantiate formula, route wisp root
gc sling <agent-or-pool> <bead-id> --on <formula>  # Attach wisp to existing bead
```

## Targeting

The `<agent-or-pool>` is a qualified name from `gc session list`:
- **Fixed agent:** `mayor`, `hello-world/refinery`
- **Pool:** `hello-world/polecat` — routes to the pool's shared work queue

**1-arg shorthand:** When target is omitted, sling derives it from the
bead's rig prefix. The rig's `default_sling_target` in city.toml determines
where work goes. Example: bead `hw-42` → rig `hello-world` → target
`hello-world/polecat`.

**Rig-scoped beads:** `gc sling` automatically resolves the rig directory
for rig-scoped bead IDs (e.g. `hw-abc`) and runs `bd update` from there,
so the rig's `.beads` database is found without manual intervention.

**Creating beads for a rig:** When creating beads with `bd create`, you
must run from the rig directory (or pass `--dir`) so the bead lands in
the rig's `.beads/` database, not the city-level one. Use `gc rig list`
to find rig paths.

## Direct dispatch (bead to agent or pool)

```
gc sling <agent-or-pool> <bead-id>     # Route a bead to an agent's hook
gc sling <bead-id>                     # Use rig's default_sling_target
```

The agent receives the bead on its hook and runs it per GUPP.

## Formula dispatch (formula on agent)

```
gc sling <agent> -f <formula>          # Run a formula, creating a molecule
```

Creates a molecule from the formula and hooks the root bead to the agent.

## Wisp dispatch (formula + existing bead)

```
gc sling <agent> <bead-id> --on <formula>  # Attach formula wisp to bead
```

Creates a molecule wisp on the bead and routes to the agent.

## Formulas

```
gc formula list                        # List available formulas
gc formula show <name>                 # Show formula definition
```

### Built-in formulas

**mol-do-work** — Simple work lifecycle. Agent reads the bead, implements
the solution in the current working directory, and closes the bead.
No git branching, no worktree isolation, no refinery handoff. Good for
demos and simple single-agent workflows.

```
gc sling <agent> <bead-id> --on mol-do-work
```

**mol-polecat-commit** — Direct-commit variant. Creates a worktree but
commits directly to base_branch with no feature branch or refinery step.
Includes preflight tests, implementation, and self-review quality gates.
For small installations where merge review is unnecessary.

```
gc sling <agent> <bead-id> --on mol-polecat-commit
```

**mol-polecat-base** — Shared base for polecat work formulas. Defines
the common steps (load context, preflight, implement, self-review) that
variant formulas extend. Not typically used directly — use a variant
like mol-polecat-commit or mol-polecat-work instead.

### Gastown pack formulas (work variants)

These require the gastown pack. They extend the built-in
`mol-polecat-base`.

**mol-polecat-work** — Feature-branch variant. Creates a worktree and
feature branch, implements, then pushes and reassigns to the refinery
for merge review. Production default for multi-agent setups.

```
gc sling <agent> <bead-id> --on mol-polecat-work
```

**mol-polecat-work-reviewed** — Human-reviewed variant. Like
mol-polecat-work but adds investigation, planning, and a human
checkpoint before implementation. The polecat writes a plan to bead
notes, notifies the mayor, and blocks until approved. Use for
high-risk or complex work that needs human sign-off before coding.

```
gc sling <agent> <bead-id> --on mol-polecat-work-reviewed
```

### Gastown pack formulas (patrol loops)

Patrol formulas are auto-poured by agent startup prompts — you typically
don't sling these manually:

- **mol-refinery-patrol** — Refinery merge loop (check for work, merge one branch, repeat)
- **mol-witness-patrol** — Rig work-health monitor (orphan recovery, stuck polecats, help mail)
- **mol-deacon-patrol** — Controller sidekick (work-layer health, system diagnostics)
- **mol-digest-generate** — Periodic activity digest mailed to the mayor
- **mol-shutdown-dance** — Due process for stuck agents (interrogate → execute → epitaph)

## Convoys (grouped work)

```
gc convoy create <name> <bead-ids...>  # Group beads into a convoy
gc convoy list                         # List active convoys
gc convoy status <id>                  # Show convoy progress
gc convoy add <id> <bead-ids...>       # Add beads to convoy
gc convoy close <id>                   # Close convoy
gc convoy check <id>                   # Check if all beads done
gc convoy stranded                     # Find convoys with no progress
gc convoy autoclose                    # Close convoys where all beads done
```

## Automations

```
gc automation list                     # List automation rules
gc automation show <name>              # Show automation definition
gc automation run <name>               # Manually trigger an automation
gc automation check <name>             # Check if gate conditions are met
gc automation history <name>           # Show automation run history
```
