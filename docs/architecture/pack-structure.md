# Pack Structure — Understanding Gas City Packs

This document explains how Gas City packs work, using the `examples/gastown/` reference
implementation as the primary example. Packs are how you express multi-agent orchestration
as pure configuration — no Go code required.

## Directory Layout

A pack follows this structure:

```
packs/
├── <pack-name>/
│   ├── pack.toml               # Pack manifest: agents, includes, commands, doctors
│   ├── prompts/                # Role-specific prompt templates (.md.tmpl)
│   │   ├── <role>.md.tmpl      # One per agent role
│   │   └── shared/             # Templates included across roles
│   ├── formulas/               # Multi-step workflow definitions (.formula.toml)
│   │   └── orders/             # Gate-based auto-dispatch orders
│   ├── namepools/              # Themed name lists for pooled agents
│   ├── scripts/                # Support scripts (worktree setup, theming)
│   ├── overlays/               # Role-specific environment overrides
│   │   └── <role>/             # Override files for a specific role
│   ├── commands/               # Custom gc commands
│   └── doctor/                 # Health check scripts
└── <infrastructure-pack>/      # Shared infrastructure layer (included by domain packs)
    ├── pack.toml
    ├── prompts/
    ├── formulas/orders/
    └── doctor/
```

## pack.toml — The Pack Manifest

The `pack.toml` file is the root of a pack. It defines:

- **Pack metadata** — name, schema version
- **Includes** — other packs to inherit from
- **Agents** — each role as an `[[agent]]` section
- **Formulas** — directory containing workflow definitions
- **Commands** — custom CLI commands
- **Doctors** — health check scripts

```toml
[pack]
name = "gastown"
schema = 1
includes = ["../maintenance"]    # Inherit infrastructure layer

[formulas]
dir = "formulas"

[[doctor]]
name = "check-scripts"
script = "doctor/check-scripts.sh"

[[commands]]
name = "status"
script = "commands/status.sh"
```

## Agent Definitions

Each role is defined as an `[[agent]]` section in `pack.toml`. The key fields are:

| Field | Purpose |
|-------|---------|
| `name` | Agent role name |
| `scope` | `"city"` (one per city) or `"rig"` (one per rig) |
| `work_dir` | Working directory (supports template variables) |
| `prompt_template` | Path to the role's prompt template |
| `nudge` | Default nudge message sent on wake |
| `overlay_dir` | Environment overlay directory |
| `idle_timeout` | How long before idle suspension |
| `wake_mode` | `"resume"` (default) or `"fresh"` (no session resume) |
| `pre_start` | Scripts to run before agent starts |
| `session_live` | Scripts to run in the live tmux session |
| `[agent.pool]` | Pool configuration for multi-instance agents |

### Singleton Agent Example (Mayor)

```toml
[[agent]]
name = "mayor"
scope = "city"
work_dir = ".gc/agents/mayor"
prompt_template = "prompts/mayor.md.tmpl"
nudge = "Check mail and hook status..."
overlay_dir = "overlays/default"
idle_timeout = "1h"
```

### Pooled Agent Example (Polecat)

```toml
[[agent]]
name = "polecat"
scope = "rig"
work_dir = ".gc/worktrees/{{.Rig}}/polecats/{{.AgentBase}}"
prompt_template = "prompts/polecat.md.tmpl"
nudge = "Check your hook for work assignments."
overlay_dir = "overlays/default"
idle_timeout = "2h"
pre_start = ["{{.ConfigDir}}/scripts/worktree-setup.sh ..."]

[agent.pool]
min = 0
max = 5
namepool = "namepools/mad-max.txt"
```

### Ephemeral Agent Example (Boot)

```toml
[[agent]]
name = "boot"
scope = "city"
wake_mode = "fresh"
work_dir = ".gc/agents/boot"
prompt_template = "prompts/boot.md.tmpl"
overlay_dir = "overlays/default"
```

## The Gas Town Roles

The `examples/gastown/` pack defines seven roles, demonstrating the full range of agent
patterns:

| Role | Scope | Pool | Worktree | Purpose |
|------|-------|------|----------|---------|
| **Mayor** | city | singleton | no | Global coordinator — plans, dispatches, strategic decisions |
| **Deacon** | city | singleton | no | Patrol loop — health checks, stuck agent detection, gate checks |
| **Boot** | city | ephemeral | no | Watchdog — triages whether deacon is stuck, fresh spawn each tick |
| **Witness** | rig | singleton | no | Per-rig monitor — orphaned bead recovery, stuck polecat detection |
| **Refinery** | rig | singleton | yes | Merge queue — sequential rebase, merge, close for work beads |
| **Polecat** | rig | 0–5 | yes | Workers — implement features, create branches, submit to refinery |
| **Dog** | city | 0–3 | no | Utility — shutdown dance, infrastructure orders, maintenance |

## Prompt Templates

Prompt templates are Go `text/template` files (`.md.tmpl`) that define agent behavior.
They are rendered at agent start time with variables from the city, rig, and agent config.

### Template Variables

| Variable | Description |
|----------|-------------|
| `{{ cmd }}` | CLI command name (`gc`) |
| `{{ .CityRoot }}` | City root directory |
| `{{ .RigName }}` | Current rig name |
| `{{ .AgentName }}` | Agent instance name |
| `{{ .WorkDir }}` | Agent working directory |
| `{{ .IssuePrefix }}` | Bead prefix for routing (e.g., `gt-`) |
| `{{ .ConfigDir }}` | Pack config directory |

### Shared Templates

Templates in `prompts/shared/` are included across roles:

| Template | Purpose |
|----------|---------|
| `propulsion.md.tmpl` | The Propulsion Principle — hook work means execute immediately |
| `capability-ledger.md.tmpl` | Why work creates permanent record |
| `architecture.md.tmpl` | ASCII diagram of the town structure |
| `following-mol.md.tmpl` | How to follow formula steps |
| `approval-fallacy.md.tmpl` | No approval step — execute immediately |
| `operational-awareness.md.tmpl` | Running state awareness |
| `tdd-discipline.md.tmpl` | Test-driven approach |
| `command-glossary.md.tmpl` | Command reference |

Role templates include shared templates via `{{ template "name" . }}`.

## Formulas — Multi-Step Workflows

Formulas are TOML files that define step-by-step workflows agents follow. They live in
the `formulas/` directory.

### Structure

```toml
description = """Polecat work lifecycle — feature-branch variant."""
formula = "mol-polecat-work"
extends = ["mol-polecat-base"]
version = 7

[vars]
[vars.event_timeout]
description = "Seconds to wait for events before re-checking"
default = "30"

[[steps]]
id = "workspace-setup"
title = "Set up worktree and feature branch"
needs = ["load-context"]
description = """
Ensure you have an isolated git worktree and a clean feature branch.
Every check is idempotent — safe to re-run after crash/restart.
"""

[[steps]]
id = "submit-and-exit"
title = "Submit work to refinery and exit"
needs = ["self-review"]
description = """
Instructions for final submission, metadata, cleanup.
"""
```

### Key Concepts

- **Steps are descriptions, not child beads** — agents read and follow them sequentially
- **`needs`** declares step dependencies (DAG ordering)
- **`extends`** inherits from base formulas
- **`[vars]`** provides configurable parameters with defaults
- **Crash recovery** — on restart, agents re-read steps and resume based on persistent
  state (git state, bead state). This is Nondeterministic Idempotence (NDI).

### Gas Town Formulas

| Formula | Used By | Purpose |
|---------|---------|---------|
| `mol-polecat-work` | Polecat | Feature branch implementation lifecycle |
| `mol-deacon-patrol` | Deacon | Health check patrol loop |
| `mol-witness-patrol` | Witness | Per-rig work monitoring loop |
| `mol-refinery-patrol` | Refinery | Merge queue processing loop |
| `mol-idea-to-plan` | Mayor | Idea intake and planning |
| `mol-personal-work-v2` | Crew | Persistent human collaborator workflow |
| `expansion-design-review` | Various | Design review expansion |
| `expansion-review-pr` | Various | PR review expansion |

## Orders — Gate-Based Auto-Dispatch

Orders are formulas or exec scripts that auto-dispatch when gate conditions are met.
They live in `formulas/orders/`.

```toml
# formulas/orders/digest-generate/order.toml
[order]
description = "Generate daily code digest across all rigs"
formula = "mol-digest-generate"
gate = "cooldown"
interval = "24h"
pool = "dog"
```

Gate types include `cooldown` (time-based), `cron` (schedule), `condition` (state check),
and `event` (reactive).

## Pack Inheritance

Packs can include other packs via `includes`:

```toml
[pack]
name = "gastown"
includes = ["../maintenance"]
```

The maintenance pack provides infrastructure agents and orders:

- **Dog pool** (fallback definition, overridden by gastown)
- **Exec orders** — gate-sweep, wisp-compact, prune-branches, orphan-sweep
- **Formulas** — mol-shutdown-dance, mol-dog-reaper, mol-dog-compactor

Agents in included packs marked `fallback = true` are overridden by the including pack's
definition of the same agent name.

## Overlays

Overlays provide role-specific environment configuration. Each overlay directory contains
files that are layered into the agent's working environment.

```
overlays/
├── default/            # Used by most agents
│   └── .claude/settings.json
├── witness/            # Witness-specific overrides
└── crew/               # Crew-specific overrides
```

## Namepools

Pooled agents draw instance names from themed text files:

- `namepools/mad-max.txt` — Fury Road characters for polecats
- `namepools/minerals.txt` — Element names for dogs

One name per line. Names are assigned sequentially as agents spawn.

## Design Principles

### Propulsion Principle
Every agent follows the same loop:
1. Check hook for work (`bd list --assignee=$GC_AGENT`)
2. Work found → execute immediately (no questions, no approval)
3. Hook empty → search pool for new work
4. Follow formula steps in order

### Zero Framework Cognition (ZFC)
Go handles transport only. Judgment calls belong in prompts. Decision logic like
"is this agent stuck?" or "should I reject this branch?" lives in the prompt template,
not in if-statements in Go.

### Nondeterministic Idempotence (NDI)
System converges through persistent state. Crash? Re-read formula steps, check bead
state, resume from where you left off. Multiple observers check the same state
idempotently.

### Self-Cleaning Model
Ephemeral agents clean up after themselves:
- Polecat: work → push → metadata → reassign → exit
- Dog: complete warrant → exit
- Witness cleans orphaned worktrees

## Creating a New Pack

To create a new pack, follow this pattern:

1. Create the directory structure under `packs/`
2. Write `pack.toml` with your agent definitions
3. Create prompt templates for each role in `prompts/`
4. Define workflows as formulas in `formulas/`
5. Add namepools for any pooled agents
6. Include `maintenance` (or another infrastructure pack) for shared orders
7. Add doctor checks for validation

The gastown example at `examples/gastown/packs/gastown/` is the canonical reference.
