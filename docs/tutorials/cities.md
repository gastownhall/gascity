# Cities, Rigs, and Packs

A city is the environment where agents live, projects connect, and work happens. Each city is an independent workspace with its own agents, sessions, and work — you might have one for your team's backend services and another for a side project. 

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
$ gc sling claude "Generate hello, world in python to the file ~/hello.py"
Dispatched gc-1 → claude
```

> **Issue:** gc sling on a new city fails to dispatch — [details](issues.md#sling-after-init) · [#286](https://github.com/gastownhall/gascity/issues/286), [#287](https://github.com/gastownhall/gascity/issues/287)

The work is dispatched to the agent. In a few moments, the file will be written to your home directory:

```
$ cat ~/hello.py
print("Hello, world!")
```

## City Definitions

When you ran `gc init`, Gas City populated the new directory to support both you defining what the city is and how it should work, and for Gas City to write out its own state needed to keep the city running.

Your city directory looks like this:
```
my-city/
├── city.toml           # City definition and local bindings
├── packs/              # Packages of reusable definitions
├── prompts/            # Prompts that initialize agents
├── formulas/           # Workflow definitions
├── scripts/            # Utility scripts
├── .gc/                # Managed state (gitignored)
└── .beads/             # Managed state (gitignored)
```

There are three categories of files in a city:

| Category | Contents | Version? |
|---|---|---|
| **Definitions** | `city.toml`*, `packs/`, `prompts/`, `formulas/`, `scripts/` — the blueprint for what the city is and how it behaves | Yes |
| **Local bindings** | `city.toml`* —  configuration that wires the city up to the specific machine and directory it's registered in (e.g., paths to project directories, network ports) | No |
| **Managed state** | `.gc/`, `.beads/` — opaque state that Gas City needs to keep your city running properly | No |

*`city.toml` currently contains both definitions and local bindings. See [#159](https://github.com/gastownhall/gascity/issues/159) for the plan to separate them cleanly via `city.local.toml`.

If you're versioning your city (and you should), you'll want to keep local bindings and managed state out of source control:

```gitignore
.gc/
.beads/
```

> **Issue:** gc init generates an incomplete .gitignore — [details](issues.md#init-incomplete-gitignore) · [#301](https://github.com/gastownhall/gascity/issues/301)

## city.toml

This is the city's primary definition file. A minimal city.toml looks like:

```toml
[workspace]
name = "my-city"
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

One request went to Claude, the other to Codex. You didn't have to think about which CLI to invoke, how to pass the prompt, or where the session state lives. Providers let you work with multiple model backends without changing your workflow.

The `[[agent]]` entries define agents inline — the agents chapter covers all the definition fields. As things grow further, you'll compose reusable *packs* into the config rather than defining everything inline. Packs are covered later in this chapter.

## Running Your Cities

Gas City has two layers of infrastructure working behind the scenes: the *supervisor* and the *controller*.

The **supervisor** is a machine-wide daemon that manages all your cities. It starts when you first run `gc init` or `gc start` and stays running in the background. It's responsible for starting and stopping city controllers, tracking registration, and restarting cities after a reboot.

You can see the list of registered cities at any time:

```
$ gc cities
NAME                          PATH
my-city                       /Users/you/my-city
project-management            /Users/you/pmc
develop-and-deploy            /Users/you/dev
```

Each city is tracked by its path on disk — two cities can't share the same directory. The city name must also be unique; if you initialize two cities with the same name, the second will fail to register.

When you called `gc init`, your city was registered automatically. To remove your city, deleting the directory isn't sufficient — you must unregister it from the supervisor:

```
$ gc unregister ~/my-city
```

To register an existing city directory (one that already has a `city.toml`) with the supervisor:

```
$ gc register ~/my-city
```

### City Controllers

Each city gets its own **controller** — a background process launched by the supervisor. The controller is what keeps the city alive. It manages sessions, handles scaling, enforces timeouts, and watches `city.toml` for changes, applying them live.

`gc init` starts your city automatically. But you'll want to start and stop cities manually — after a reboot, when making config changes, or when you want to free resources.

```
$ gc start
City started under supervisor.
```

Once started, a city can be suspended and resumed. Suspending pauses all agent activity — sessions stop accepting work and go quiet — but the controller stays alive, watching for config changes and ready to bring everything back:

```
$ gc suspend
City suspended.

$ gc resume
City resumed.
```

Suspend is useful when you need agents to stop temporarily — during infrastructure work, or to free up rate limits — without tearing down the city. The controller is still running, so resuming is instant.

`gc stop` is more drastic. It shuts down the controller entirely, terminates all sessions, and unregisters the city from the supervisor:

```
$ gc stop
City stopped.
```

The difference: suspend is a pause button, stop is a full shutdown. After stop, you need `gc start` to bring the city back.

You can check the status of the city using `gc status`:

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

## Rigs

So far our city has agents but no project code to work on. Your projects are just directories on your filesystem, in whatever location works for you. Your project directories exist independently of Gas City, and Gas City doesn't move them, copy them, or contain them. What Gas City does need to know is which directories your city's agents should work in, which is what rigs are for.

A *rig* is what connects your city to a path. To bring a project into a city, you *rig* it — registering the directory so the city's agents can reach into it, track work against it, and install the hooks that let agents integrate with the project's tooling.

When you rig a project, Gas City creates a rig entry in the city, sets up work tracking for it, installs provider hooks, and — if your city uses packs — stamps rig-scoped agents for that project.

```
$ gc rig add ~/path/to/my-app
Added rig 'my-app' to city 'my-city'
  Prefix: ma
  Beads:  initialized
  Hooks:  installed (claude)
```

Gas City derives the rig name from the directory basename and creates a short *prefix* for bead IDs. You can override the name:

```
$ gc rig add ~/path/to/my-app --name frontend
```

After adding a rig, city.toml has a new `[[rigs]]` entry:

```toml
[[rigs]]
name = "my-app"
path = "/Users/you/path/to/my-app"
```

### Rig context

Gas City automatically figures out which rig you're working in based on your current directory. If you're in `~/path/to/my-app/src/main`, it knows you're in the `my-app` rig. Commands like `gc sling` use this to resolve targets:

```
$ cd ~/path/to/my-app/src
$ gc sling worker BL-42
# Resolves to my-app/worker
```

### Managing rigs

Like cities, rigs are managed by the supervisor and can be enumerated using `gc rig list`:

```
$ gc rig list
NAME       PATH                    PREFIX  SUSPENDED
my-app     /path/to/my-app         ma      no
backend    /path/to/backend        ba      no
```

Rigs can be suspended and resumed when you're doing potentially disruptive work that you don't want agents interfering with:

```
# pause all agent work on the project my-app
$ gc rig suspend my-app
Suspended rig 'my-app'

# do your infrastructure work while agents are paused...

# bring the agents back
$ gc rig resume my-app
Resumed rig 'my-app'
```

Because rigs are managed by the supervisor, removing their entry from your TOML file doesn't actually make the rig go away. To remove a rig, use `gc rig remove`:

```
$ gc rig remove my-app
Removed rig 'my-app' from city 'my-city'
```

This removes the `[[rigs]]` entry from `city.toml` and cleans up the internal registry that the supervisor uses to manage the system.

When you remove a rig, you are just removing the binding from this city to the rig's directory. The rig's directory itself is untouched.

> **Managed fields:** Some fields in `city.toml` are written by `gc` commands and shouldn't be edited by hand. The `[[rigs]]` entries are the main example — `gc rig add` writes them and sets up the associated work tracking and hooks. When in doubt, use the CLI.

## Packs

In Gas City, a *pack* is a reusable set of definitions that can be shared across multiple cities.

A *city* definition is a directory with a `city.toml` at the root.

A *pack* definition is a directory with a `pack.toml` at the root.

Both define agents, formulas, prompts, scripts, and other assets. The difference: a city is a running workspace you start and stop. A pack is a reusable module that gets composed into one or more cities.

If you used `gc init` to create your city, you already have two packs available to you: `gastown` and `maintenance`, both in the city's `packs/` directory. Gas City bakes as little behavior into the binary as possible. Packs are how it ships defaults.

> **Issue:** Fresh city has defaults in both packs and top-level directories — [details](issues.md#pack-vs-toplevel-defaults)

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

Packs can also include other packs:

```toml
# gastown/pack.toml
[pack]
name = "gastown"
schema = 1
includes = ["../maintenance"]
```

This is how Gastown brings in the maintenance pack — which provides infrastructure agents and operational orders. The include is resolved relative to the pack's own directory, and includes are recursive with cycle detection.

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

### The Gastown and Maintenance packs

Gas City ships two packs that work together:

**Gastown** is the orchestration pack — it provides the agents that coordinate and execute work across your city. If you want multi-agent workflows with a coordinator that plans, workers that code, and monitors that watch, Gastown is what gives you that. It defines:

- **mayor** (city-scoped) — the coordinator. Plans work, dispatches to workers, tracks progress. Always running.
- **deacon** (city-scoped) — the patrol executor. Monitors city health, runs periodic checks. Always running.
- **boot** (city-scoped) — the watchdog. Monitors the deacon itself. Ephemeral — starts fresh each time.
- **witness** (rig-scoped) — monitors workers within a single rig. One per project.
- **refinery** (rig-scoped) — processes the merge queue for a rig.
- **polecat** (rig-scoped) — the transient workers that actually write code. Pools of up to 5 per rig.

**Maintenance** is the infrastructure pack — it provides utility agents and operational formulas for housekeeping tasks like orphan sweeps and gate checks. Gastown includes maintenance automatically, so you don't need to reference it separately.

You don't have to use either pack. A city with inline `[[agent]]` entries and no `includes` works fine — that's how the tutorial template starts you off. Packs become valuable when you want a proven orchestration pattern without defining every agent yourself.

A city that includes Gastown gets all of the above with two lines:

```toml
[workspace]
includes = ["packs/gastown"]
```

## Command reference

| Command | What it does |
|---|---|
| `gc init <path>` | Create a new city |
| `gc init <path> --provider <name>` | (*skip the wizard*) |
| `gc init <path> --from <example>` | (*clone from a template*) |
| `gc start` | Start the city controller |
| `gc stop` | Stop the city |
| `gc suspend` | Pause all agent activity |
| `gc resume` | Resume a suspended city |
| `gc restart` | Stop and restart the city |
| `gc status` | Show city status and running agents |
| `gc cities` | List all registered cities |
| `gc register <path>` | Manually register a city |
| `gc unregister <path>` | Manually unregister a city |
| `gc doctor` | Run health checks |
| `gc doctor --fix` | (*attempt automatic repairs*) |
| `gc sling <agent> <work>` | Dispatch work to an agent |
| `gc rig add <path>` | Register a project directory as a rig |
| `gc rig add <path> --name <name>` | (*with explicit name*) |
| `gc rig list` | List registered rigs |
| `gc rig status <name>` | Show rig details and agent status |
| `gc rig suspend <name>` | Suspend all agents in a rig |
| `gc rig resume <name>` | Resume a suspended rig |
| `gc rig restart <name>` | Kill and restart all rig sessions |
| `gc rig remove <name>` | Unregister a rig |
| `gc rig default <name>` | Set the default rig |
| `gc pack list` | List configured packs |
| `gc pack fetch` | Clone or update remote packs |

<!--
BONEYARD — draft material for future sections. Not part of the published tutorial.

### Health checks

`gc doctor` runs a suite of diagnostics that check the structural integrity of your city — is the config valid, are the expected binaries installed, are agent sessions running, is the bead store healthy. It's the first thing to run when something isn't working right.

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

Each check reports OK, WARN, or FAIL. Packs can ship their own doctor checks (e.g., Gastown checks that its scripts are executable, Maintenance checks that `jq` and `gh` are on PATH).

Add `--fix` to attempt automatic repairs — doctor will try to restart missing sessions, reinitialize stores, and fix what it can:

```
$ gc doctor --fix
```

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
