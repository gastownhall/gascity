# Cities, Rigs, and Packs

A city is the environment where agents live, projects connect, and work happens. Each city is an independent workspace with its own agents, sessions, and work — you might have one for your team's backend services and another for a side project. 

Gas City knows about all the cities on your machine, and monitors their health, resource usage, and other runtime characteristics. You can see the list of registered cities at any time:

```
$ gc cities
NAME                          PATH
my-city                       /Users/you/my-city
project-management            /Users/you/pmc
develop-and-deploy            /Users/you/dev
```

Each city is tracked by its path on disk — two cities can't share the same directory. The city name must also be unique; if you initialize two cities with the same name, the second will fail to register.

If you're just getting started, this list is empty. Let's fix that.

You create cities using the `gc init` command, specifying a directory for your city and a default provider to be used by agents:

```
$ gc init ~/my-city --provider claude
[1/8] Creating runtime scaffold
[2/8] Installing hooks (Claude Code)
[3/8] Writing default prompts
[4/8] Writing default formulas
[5/8] Writing city configuration
Welcome to Gas City!
Initialized city "my-city" with default provider "claude".
[6/8] Checking provider readiness
[7/8] Registering city with supervisor
Registered city 'my-city' (/Users/you/my-city)
[8/8] Waiting for supervisor to start city
```

`gc init` creates the city directory and registers it with the supervisor. By the time it finishes, your city is running.

That means you can use `gc sling` to dispatch work to an agent right away. The command takes two arguments — the agent name and what you want it to do:

```
$ cd ~/my-city
$ gc sling claude "What files are in this directory?"
Dispatched gc-1 → claude
```

> **Issue:** gc sling on a new city fails to dispatch — [details](issues.md#sling-after-init) · [#286](https://github.com/gastownhall/gascity/issues/286), [#287](https://github.com/gastownhall/gascity/issues/287)

The work is dispatched — the agent picks it up in the background. To see what it's doing, peek at its session:

```
$ gc session peek claude
[mayor] Looking at the city directory...
[mayor] I can see city.toml, prompts/, formulas/, scripts/, and packs/.
```

The agents chapter goes deep on sessions, peeking, and other ways to interact with agents. For now, the important thing is that a city is ready to use the moment it's created.

## What's inside

Your city directory looks like this:

```
my-city/
├── city.toml           # City definition
├── packs/              # Packages of reusable definitions
├── prompts/            # Prompts that initialize agents
├── formulas/           # Workflow definitions
├── scripts/            # Utility scripts
├── .gc/                # Local state (gitignored)
└── .beads/             # Local state (gitignored)
```

There are two categories of files in a city:

- **Definition** — `city.toml`, `packs/`, `prompts/`, `formulas/`, `scripts/`. The blueprint for what the city is and how it behaves. Shareable, version-controllable — this is what you'd commit to a repo.
- **Local state** — `.gc/` and `.beads/`. Controller state, event logs, caches, and work items. Machine-local, generated at runtime — not something you'd share or version.

If you're versioning your city (and you should), you'll want to keep local state out of source control:

```gitignore
.gc/
.beads/
hooks/
```

> **Issue:** `gc init` doesn't generate a `.gitignore` for the city root — [details](issues.md#init-no-gitignore)

## city.toml

This is the city's primary definition file. A minimal city.toml looks like:

```toml
[workspace]
provider = "claude"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
```

That gives you a city with one agent called `mayor`. The city name defaults to the directory basename — `my-city` in this case. This is roughly what the default template generates.

As your city grows, you add more agents. The `workspace.provider` sets the default, but each agent can override it. Providers normalize access to different model backends — Claude, Codex, Gemini — behind a single interface, so you can mix and match within the same city without changing your workflow:

```toml
[workspace]
provider = "claude"

[[agent]]
name = "helper"
prompt_template = "prompts/helper.md"

[[agent]]
name = "reviewer"
prompt_template = "prompts/reviewer.md"
provider = "codex"
```

Now you can sling work at either one. Same command, same workflow — Gas City handles the provider differences:

```
$ gc sling helper "Refactor the auth module to use dependency injection"
Dispatched gc-2 → helper

$ gc sling reviewer "Check the latest PR for security issues"
Dispatched gc-3 → reviewer
```

One request went to Claude, the other to Codex. You didn't have to think about which CLI to invoke, how to pass the prompt, or where the session state lives. Gas City providers simplify doing development with multiple agents.

The `[[agent]]` entries define agents inline — the agents chapter covers all the definition fields. As things grow further, you'll compose reusable *packs* into the config rather than defining everything inline. Packs are covered later in this chapter.

## Starting and stopping

`gc init` starts your city automatically. But you'll also need to start and stop cities manually — after a reboot, when making config changes, or when you want to free resources.

```
$ gc start
City started under supervisor.
```

This starts the city controller under the machine-wide *supervisor*. The controller watches your config, reconciles agent sessions, handles scaling, and enforces idle timeouts.

Check what's running:

```
$ gc status
my-city  /Users/you/my-city
  Controller: supervisor (PID 12345)
  Suspended:  no

Agents:
  helper                     running
  reviewer                   running

2/2 agents running

Sessions: 2 active, 0 suspended
```

Stop everything:

```
$ gc stop
City stopped.
```

Stop sends a graceful shutdown signal, waits for the configured timeout, then force-kills anything still running.

Restart is just stop + start:

```
$ gc restart
```

If you want to see what *would* happen without actually starting anything:

```
$ gc start --dry-run
Dry-run: 2 agent(s) would start in city 'my-city'

  helper                 session=my-city-helper
  reviewer               session=my-city-reviewer

No side effects executed (--dry-run).
```

## Rigs

So far the city has agents but no project code to work on. Your projects are just directories on your filesystem — they exist independently of Gas City, and Gas City doesn't move them, copy them, or contain them. From Gas City's perspective, a project is nothing but a path on disk.

A *rig* is what connects your city to that path. To bring a project into a city, you *rig* it — registering the directory so the city's agents can reach into it, track work against it, and install the hooks that let agents integrate with the project's tooling.

When you rig a project, Gas City creates a rig entry in the city, sets up work tracking for it, installs provider hooks, and — if your city uses packs — stamps rig-scoped agents for that project.

```
$ gc rig add /path/to/my-app
Added rig 'my-app' to city 'my-city'
  Prefix: ma
  Beads:  initialized
  Hooks:  installed (claude)
```

Gas City derives the rig name from the directory basename and creates a short *prefix* for bead IDs. You can override the name:

```
$ gc rig add /path/to/my-app --name frontend
```

After adding a rig, city.toml has a new `[[rigs]]` entry:

```toml
[[rigs]]
name = "my-app"
path = "/path/to/my-app"
```

### Rig context

Gas City automatically figures out which rig you're working in based on your current directory. If you're in `/path/to/my-app/src/main`, it knows you're in the `my-app` rig. Commands like `gc sling` use this to resolve targets:

```
$ cd /path/to/my-app/src
$ gc sling worker BL-42
# Resolves to my-app/worker
```

### Managing rigs

```
$ gc rig list
NAME       PATH                    PREFIX  SUSPENDED
my-app     /path/to/my-app         ma      no
backend    /path/to/backend        ba      no
```

Suspend a rig to stop all its agents without removing it — useful when you're doing infrastructure work and don't want agents interfering:

```
$ gc rig suspend my-app
Suspended rig 'my-app'

$ gc rig resume my-app
Resumed rig 'my-app'
```

Remove a rig when you're done with it:

```
$ gc rig remove my-app
Removed rig 'my-app' from city 'my-city'
```

This removes the `[[rigs]]` entry from `city.toml` and cleans up the registry. The rig's directory is untouched — Gas City never modifies your project files.

> **Managed fields:** Some fields in `city.toml` are written by `gc` commands and shouldn't be edited by hand. The `[[rigs]]` entries are the main example — `gc rig add` writes them and sets up the associated work tracking and hooks. When in doubt, use the CLI.

## Packs

A *city* is a directory with a `city.toml` at the root.
A *pack* is a directory with a `pack.toml` at the root.

Both define agents, formulas, prompts, scripts, and other assets. The difference: a city is a running workspace you start and stop. A pack is a reusable module that gets composed into one or more cities.

If you used `gc init` with the gastown template, you're already using packs — everything from the mayor coordinator to the polecat workers is pure configuration in a pack. Gas City doesn't hardcode any behavior into the binary. Packs are how it ships defaults.

### Composing packs into a city

Packs are referenced from `city.toml` via `includes`:

```toml
[workspace]
provider = "claude"
includes = ["packs/gastown"]          # City-level: city-scoped agents

[[rigs]]
name = "my-app"
path = "/path/to/my-app"
includes = ["packs/gastown"]          # Rig-level: rig-scoped agents
```

The same pack can appear in both places, and Gas City handles it correctly:

- `workspace.includes` pulls in agents with `scope = "city"` — things like the mayor and deacon that exist once for the whole city
- `rigs.includes` pulls in agents with `scope = "rig"` — things like workers and witnesses that get stamped per rig

You can set a default so new rigs get packs automatically:

```toml
[workspace]
default_rig_includes = ["packs/gastown"]
```

### The composition pipeline

When Gas City loads the city definition, packs are processed through a specific pipeline:

1. Load `city.toml` and merge any definition fragments
2. Expand city-level packs (`workspace.includes`) — pull in city-scoped agents
3. Apply city-level patches
4. Expand rig-level packs (`rigs.includes`) — stamp rig-scoped agents per rig
5. Apply rig overrides
6. Apply pack globals
7. Compute formula and script layers

The order matters because each stage can modify what the previous stage produced. City patches can target pack-provided agents. Rig overrides (covered in the agents chapter) customize pack-stamped agents for a specific project. Globals append to everything at the end.

### A real pack: Gastown

The Gastown pack is the reference implementation. Here's a simplified view:

```toml
# packs/gastown/pack.toml
[pack]
name = "gastown"
schema = 1
includes = ["../maintenance"]

# City-scoped agents (one each for the city)
[[agent]]
name = "mayor"           # Coordinator — plans and dispatches
scope = "city"

[[agent]]
name = "deacon"          # Patrol executor — monitors health
scope = "city"

[[agent]]
name = "boot"            # Watchdog — monitors the deacon
scope = "city"
wake_mode = "fresh"

# Rig-scoped agents (one set per registered rig)
[[agent]]
name = "witness"         # Worker monitor — per rig
scope = "rig"

[[agent]]
name = "refinery"        # Merge queue — per rig
scope = "rig"

[[agent]]
name = "polecat"         # Transient workers — pool of up to 5 per rig
scope = "rig"
max_active_sessions = 5
```

A city that includes Gastown gets all of this with two lines:

```toml
[workspace]
includes = ["packs/gastown"]
```

The maintenance pack (included by Gastown) adds infrastructure agents and operational orders — the housekeeping that runs in the background.

### Where packs live

Packs can come from three places:

**Embedded** — inside the city directory. This is the most common setup. The conventional location is `packs/`:

```
my-city/
├── city.toml
├── packs/
│   ├── gastown/
│   │   └── pack.toml
│   └── maintenance/
│       └── pack.toml
└── ...
```

**External directory** — a path outside the city, useful for sharing a pack across multiple cities on the same machine:

```toml
[workspace]
includes = ["/Users/shared/packs/common"]
```

**Remote git** — fetched on first access and cached in `.gc/cache/includes/`:

```toml
[workspace]
includes = ["git@github.com:org/shared-pack.git//packs/base#v1.0"]
```

`packs/` is just a convention — Gas City doesn't auto-load everything in there. Only packs explicitly referenced from `includes` participate in the assembled definition.

### Pack includes

Packs can include other packs:

```toml
# gastown/pack.toml
[pack]
name = "gastown"
schema = 1
includes = ["../maintenance"]
```

This is how Gastown brings in the maintenance pack — which provides infrastructure agents and operational orders. The include is resolved relative to the pack's own directory, and includes are recursive with cycle detection.

---

## Health checks

When something isn't working right, `gc doctor` runs a suite of diagnostics:

```
$ gc doctor
[OK]   City structure
[OK]   City configuration
[OK]   Infrastructure binaries
[FAIL] Agent sessions: 1 session missing
       agent 'mayor' not running
[OK]   Bead store
[OK]   Event log

Passed: 5/6  Failed: 1  Warnings: 0
```

Add `--fix` to attempt automatic repairs:

```
$ gc doctor --fix
```

## The controller

When you run `gc start`, the supervisor launches a *controller* for your city. The controller is the background process that keeps everything running. It:

- Watches `city.toml` for changes and applies them live
- Starts missing agent sessions, drains excess ones
- Enforces idle timeouts and pool scaling
- Restarts crashed sessions (with backoff)
- Listens on a Unix socket for commands from the CLI

You don't interact with the controller directly — the `gc` commands talk to it for you. But it's useful to know it exists, because when you edit `city.toml` while the city is running, the controller picks up the changes automatically on its next patrol tick (every 30 seconds by default).

## Command reference

| Command | What it does |
|---|---|
| `gc init <path>` | Create a new city |
| `gc init <path> --provider <name>` | (*skip the wizard*) |
| `gc init <path> --from <example>` | (*clone from a template*) |
| `gc start` | Start the city controller |
| `gc start --dry-run` | Preview what would start |
| `gc stop` | Stop the city |
| `gc restart` | Stop and restart the city |
| `gc status` | Show city status and running agents |
| `gc cities` | List all registered cities |
| `gc register <path>` | Manually register a city |
| `gc unregister <path>` | Manually unregister a city |
| `gc doctor` | Run health checks |
| `gc doctor --fix` | (*attempt automatic repairs*) |
| `gc rig add <path>` | Register a project directory as a rig |
| `gc rig add <path> --name <name>` | (*with explicit name*) |
| `gc rig list` | List registered rigs |
| `gc rig suspend <name>` | Suspend all agents in a rig |
| `gc rig resume <name>` | Resume a suspended rig |
| `gc rig restart <name>` | Kill and restart all rig sessions |
| `gc rig remove <name>` | Unregister a rig |
| `gc pack list` | List configured packs |
| `gc pack fetch` | Clone or update remote packs |

<!--
BONEYARD — draft material for future sections. Not part of the published tutorial.

### Local overrides (city.local.toml)

Some things in city.toml aren't static across machines — rig paths, API ports, provider settings. city.local.toml is an overlay file: when both files define the same field, the local file wins. Gitignored by default.

```toml
# city.local.toml — machine-specific bindings
[[rigs]]
name = "my-app"
path = "/Users/me/src/my-app"

[api]
port = 19443
```

TODO: Consider a "City vs. path differences" section that addresses the broader question of how city.toml handles machine-specific values (rig paths are the main one).

### The daemon section

The [daemon] section has several knobs beyond patrol_interval and shutdown_timeout:

```toml
[daemon]
patrol_interval = "30s"       # Controller reconciliation interval
shutdown_timeout = "5s"       # Grace period on gc stop
max_restarts = 5              # Restarts before quarantine
restart_window = "1h"         # Sliding window for restart counting
drift_drain_timeout = "2m"    # Drain timeout for config-drift restarts
wisp_gc_interval = "5m"       # Wisp garbage collection interval
wisp_ttl = "1h"               # How long closed wisps survive
```

### The API server

Cities can expose an API server for programmatic access:

```toml
[api]
port = 19443
```

This enables the web dashboard and REST endpoints for session management, bead operations, and status queries.

### Workspace-level agent defaults

Fields set in [agent_defaults] apply to every agent in the city unless overridden:

```toml
[agent_defaults]
idle_timeout = "30m"
install_agent_hooks = ["claude"]
```

### Session sleep policy

The [session_sleep] section controls city-wide defaults for when idle sessions go to sleep vs. staying active:

```toml
[session_sleep]
enabled = true
after = "30m"
```

### Suspended workspace

You can suspend the entire city without stopping the controller:

```toml
[workspace]
suspended = true
```

This prevents any agents from running while keeping the controller alive and watching for config changes.

### Multi-city rigs

A single project directory can be registered as a rig in multiple cities. When this happens, you need to set a default city for the rig:

```
$ gc rig default my-app --city gastown-dev
```

This controls which city's hooks and routing files are active in the rig directory.

### Rig-local formulas

Rigs can have their own formula directory that layers on top of city and pack formulas:

```toml
[[rigs]]
name = "my-app"
path = "/path/to/my-app"
formulas_dir = "formulas"
```

Rig-local formulas shadow city formulas with the same name.

### Pack commands

Packs can ship CLI commands that show up as `gc <pack-name> <command>`:

```toml
[[commands]]
name = "status"
description = "Show orchestration overview"
script = "commands/status.sh"
```

```
$ gc gastown status
Mayor:    running (idle 5m)
Deacon:   running (patrol active)
Workers:  3/5 active
```

### Pack doctor checks

Diagnostic scripts that run as part of `gc doctor`:

```toml
[[doctor]]
name = "check-binaries"
script = "doctor/check-binaries.sh"
description = "Verify required binaries are available"
```

### Pack globals

Commands applied to all agents in the pack's scope:

```toml
[global]
session_live = [
    "{{.ConfigDir}}/scripts/tmux-theme.sh {{.Session}} {{.Agent}}",
    "{{.ConfigDir}}/scripts/tmux-keybindings.sh {{.ConfigDir}}",
]
```

### Pack patches

Modifications to agents after they've been assembled:

```toml
[[patches.agent]]
name = "dog"
idle_timeout = "30m"
```

### Fallback agents

When multiple packs define an agent with the same name, the `fallback` field controls precedence. A non-fallback always wins over a fallback. If both are fallback, first loaded wins. If both are non-fallback, it's a collision error.

### Remote pack includes (format)

Format: `<source>//<subpath>#<ref>`

Remote packs are fetched on first access and cached in `.gc/cache/includes/`. Lock files pin specific commits for reproducibility.

### Providers in packs

Packs can define custom provider definitions that merge into the city's provider registry.

### JSON overlay merging

When overlays encounter an existing .json file in the target directory, they perform an intelligent merge rather than skipping it.

### What's in a pack (directory details)

```
packs/gastown/
├── pack.toml              # Pack definition — agents and config
├── prompts/               # Prompt templates
├── formulas/              # Workflow templates
├── scripts/               # Utility scripts
├── overlays/              # Files copied into agent workspaces
│   └── default/
│       └── CLAUDE.md
└── namepools/             # Name lists for pool agents
    └── names.txt
```

A minimal pack:

```toml
# pack.toml
[pack]
name = "my-pack"
schema = 1

[[agent]]
name = "helper"
scope = "rig"
prompt_template = "prompts/helper.md.tmpl"
```
-->
