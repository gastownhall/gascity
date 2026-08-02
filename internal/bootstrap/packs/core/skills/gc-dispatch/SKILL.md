---
name: gc-dispatch
description: Routing work to agents with gc sling and formulas
---

# Dispatching Work

`gc sling` routes work to session configs. **Multi-session configs are valid
targets** — sling to the config and any eligible session can claim the work.
You do NOT need to find or create an individual session first.

## Quick reference

```
gc sling <bead-id>                     # Auto-target via rig's default_sling_target
gc sling <session-config> <bead-id>     # Route to a specific session config
gc sling <session-config> -f <formula>  # Instantiate formula, route its root (v2 → workflow)
gc sling <session-config> <bead-id> --on <formula>  # Attach formula to existing bead (v2 → workflow)
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
creation all happen there. Create the bead in a specific rig's database
with `gc bd create --rig <rig>`, which resolves the rig from city config
and uses its database and prefix:

```
gc bd create "fix the bug" --rig frontend   # Creates fe-xxx in frontend's db
gc sling frontend/polecat fe-xxx            # Works — bead is in the right db
```

If the bead is in the wrong database (e.g. `gc-xxx` in HQ but targeting
a frontend agent), sling's cross-rig guard will block the route.

## Direct dispatch (bead to session config)

```
gc sling <session-config> <bead-id>    # Route a bead to a session config
gc sling <bead-id>                     # Use rig's default_sling_target
```

The agent receives the bead on its hook and runs it per GUPP.

## Formula dispatch (`-f`, formula creates its own root bead)

```
gc sling <agent> -f <formula>          # Instantiate a formula, route its root bead
```

Instantiates the formula and routes its **root bead** to the target. What the
instantiation produces depends on the formula's compiler contract: a **v2**
formula (one declaring `[requires] formula_compiler = ">=2.0.0"`) starts a
**workflow**; a **v1** formula instantiates a **wisp** (an ephemeral molecule).
Use `-f` when the formula defines the work itself — when you already have a work
bead, use `--on` (below). A formula that references `{{convoy_id}}` or contains
a drain step cannot be launched bare with `-f`; route it onto a bead with `--on`
so a target convoy is created.

## Formula-on-bead dispatch (`--on`, formula runs against an existing bead)

```
gc sling <agent> <bead-id> --on <formula>  # Attach a formula to an existing bead
```

`--on` attaches a formula to a bead that **already exists** — the bead is the
work; the formula supplies the method. What gets created depends on the
formula's compiler contract:

- **v2 formula** (`[requires] formula_compiler = ">=2.0.0"`) — starts a
  **workflow**. The sling creates a workflow-root bead **and** an auto-convoy,
  with your bead tracked as the work member (root + convoy + work bead). The
  step DAG and handoff metadata (`branch`, `target`, …) are persisted as beads,
  so they survive a session recycle — a recycled agent resumes the same branch
  instead of stranding it.
- **v1 formula** — instantiates a **wisp** (an ephemeral molecule) in place on
  the bead.

**Convoy-referencing formulas require a target convoy.** A v2 formula that
references `{{convoy_id}}` or contains a drain step must launch onto a convoy —
routing with `--on` satisfies that, because the sling auto-creates the convoy
(pass `--no-convoy` to suppress it, or `gc formula cook --attach` to target an
existing one). Launching such a formula bare with `-f` is rejected.

## Formulas

```
gc formula list                        # List available formulas
gc formula show <name>                 # Show formula definition
```

### Choosing a work formula

Work formulas differ by **isolation** (does the agent get its own worktree and
branch?) and **handoff** (does the agent land the change itself, or hand off to
a separate merge-review step?). Reach for the lightest one that fits:

| Formula | Isolation | Lands the change | Use when |
|---------|-----------|------------------|----------|
| `mol-do-work` | none — works in the CWD | agent commits, then **closes** the bead | demos, throwaway, or a trivial single-agent fix where isolation and review are overkill |
| `mol-scoped-work` | worktree + explicit setup/teardown | agent-managed, no refinery — work modeled as a routable **step-bead DAG** | multi-step work you want decomposed into independently-routable steps under one owner, without a merge-review gate |
| `mol-polecat-work` | worktree + feature branch | pushes the branch and **reassigns to the refinery** for merge review | production multi-agent work that must be reviewed before landing on a shared branch — the default for pooled polecats |

Two narrower siblings trade a stage away from `mol-polecat-work`:

- **`mol-polecat-commit`** — worktree + quality gates, but commits directly to
  the base branch (no feature branch, no refinery). For small installs where
  merge review is unnecessary.
- **`mol-polecat-report`** — no checkout; the agent investigates and writes
  findings to bead notes. For analysis/investigation beads whose output is a
  report, not a code change.

Rule of thumb: choose **`mol-polecat-work` for anything that must land through
review or survive a session recycle** — its workflow state and branch/target
metadata live in beads, so a recycled agent resumes the branch instead of
stranding it. Use **`mol-scoped-work`** when you want worktree isolation and
step-level routing but own the work end-to-end and need no merge-review handoff.
Drop to **`mol-do-work`** only for the trivial single-agent case.

### Built-in formulas

**mol-do-work** — Simple work lifecycle. Agent reads the bead, implements
the solution in the current working directory, and closes the bead.
No git branching, no worktree isolation, no refinery handoff. Good for
demos and simple single-agent workflows.

```
gc sling <agent> <bead-id> --on mol-do-work
```

**mol-scoped-work** — Graph-first worktree lifecycle (v2 workflow). Models the
work as an explicit DAG — a durable scope bead, explicit worktree setup and
teardown, and first-class step beads that can be routed independently, with
continuation metadata for same-session execution. The opt-in replacement for
hierarchy-first single-session formulas; agent-managed, with no refinery handoff.

```
gc sling <agent> <bead-id> --on mol-scoped-work
```

**mol-polecat-commit** — Direct-commit variant. Creates a worktree but
commits directly to base_branch with no feature branch or refinery step.
Includes preflight tests, implementation, and self-review quality gates.
For small installations where merge review is unnecessary.

```
gc sling <agent> <bead-id> --on mol-polecat-commit
```

**mol-polecat-report** — Report-only variant. No git checkout, no feature
branch, no push, no PR. The agent investigates, writes findings as bead
notes, and exits. Use for analysis or investigation tasks where the output
is a written report, not a code change.

```
gc sling <agent> <bead-id> --on mol-polecat-report
```

**mol-polecat-base** — Shared base for polecat work formulas. Defines
the common steps (load context, preflight, implement, self-review) that
variant formulas extend. Not typically used directly — use a variant
like mol-polecat-commit, mol-polecat-report, or mol-polecat-work instead.

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
- **mol-shutdown-dance** — Due process for stuck agents (interrogate → execute → epitaph)

`mol-digest-generate` (the periodic activity digest mailed to the mayor) is
**not** a startup patrol pour: it is driven by an `order` on a schedule (the
`digest-generate` order — a 24h cooldown trigger). Run or inspect it through
its order (`gc order run digest-generate`, `gc order show digest-generate`), not
as a manual sling or a patrol pour.

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
gc order check <name>             # Check if trigger conditions are met
gc order history <name>           # Show order run history
```
