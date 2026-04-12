# Dispatching Work

`gc sling` routes work to session configs. **Multi-session configs are valid
targets** — sling to the config and any eligible session can claim the work.
You do NOT need to find or create an individual session first.

## Quick reference

```
gc sling <bead-id>                     # Auto-target via rig's default_sling_target
gc sling <session-config> <bead-id>     # Route to a specific session config
gc sling <session-config> -f <formula>  # Instantiate formula, route wisp root
gc sling <session-config> <bead-id> --on <formula>  # Attach wisp to existing bead
```

## Targeting

The `<session-config>` is a qualified config name from `gc session list`:
- **Single-session config:** `mayor`, `hello-world/refinery`
- **Multi-session config:** `hello-world/polecat` — routes to the config's shared work queue

**1-arg shorthand:** When target is omitted, sling derives it from the
bead's rig prefix. The rig's `default_sling_target` in city.toml determines
where work goes. Example: bead `hw-42` → rig `hello-world` → target
`hello-world/polecat`.

**Rig-scoped beads:** `gc sling` automatically resolves the rig directory
for rig-scoped bead IDs (e.g. `hw-abc`) and runs `bd update` from there,
so the rig's `.beads` database is found without manual intervention.

**Beads must be in the agent's rig database.** Sling operates on the
target agent's rig database — formula cooking, labeling, and convoy
creation all happen there. Create beads with `--rig` so they land in
the right database:

```
bd create "fix the bug" --rig frontend   # Creates fe-xxx in frontend's db
gc sling frontend/polecat fe-xxx         # Works — bead is in the right db
```

If the bead is in the wrong database (e.g. `gc-xxx` in HQ but targeting
a frontend agent), sling's cross-rig guard will block the route.

## Direct dispatch (bead to session config)

```
gc sling <session-config> <bead-id>    # Route a bead to a session config
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
for merge review. Production default for multi-agent setups. The polecat's
`base_branch` comes from `metadata.target` on the work bead if present,
otherwise from a parent convoy with `metadata.target`, otherwise from
the rig repo's default branch.

```
gc sling <agent> <bead-id> --on mol-polecat-work
```

**mol-idea-to-plan** — Planning workflow for a coordinator session. Turns a
rough idea into a PRD, reviewed design doc, and beads DAG using Gas City's
existing primitives: repo-local artifact files, review task beads, `gc sling`,
and mail. Best run from a crew worker in the target rig.

```
gc sling <coordinator-agent> -f mol-idea-to-plan --var problem="..." --var review_target=<rig>/polecat
```

**mol-review-leg** — Helper formula used by `mol-idea-to-plan` review tasks.
Persists the full report to bead notes, mails the coordinator, closes the bead,
and drains the session. Usually not slung by hand.

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
gc convoy create <name> <bead-ids...>                 # Group beads into a convoy
gc convoy create <name> --owned --target integration/<slug>  # Long-lived initiative convoy
gc convoy target <id> <branch>                        # Set/update convoy target branch
gc convoy list                                        # List active convoys
gc convoy status <id>                                 # Show convoy progress + metadata
gc convoy add <id> <bead-ids...>                      # Add beads to convoy
gc convoy close <id>                                  # Close convoy
gc convoy check <id>                                  # Check if all beads done
gc convoy stranded                                    # Find convoys with no progress
gc convoy autoclose                                   # Close convoys where all beads done
```

Migration note:
- Existing epic beads are no longer first-class containers. Migrate open epics to convoys before relying on convoy-only tooling such as `gc convoy target`, `gc sling <convoy>`, or the Gastown refinery convoy flow.

## Orders

```
gc order list                     # List order rules
gc order show <name>              # Show order definition
gc order run <name>               # Manually trigger an order
gc order check <name>             # Check if gate conditions are met
gc order history <name>           # Show order run history
```
