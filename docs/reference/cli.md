# CLI Reference

> **Auto-generated** — do not edit. Run `go run ./cmd/genschema` to regenerate.

## Global Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--city` | string |  | path to the city directory (default: walk up from cwd) |
| `--rig` | string |  | rig name or path (default: discover from cwd) |

## gc

Gas City CLI — orchestration-builder for multi-agent workflows

```
gc [flags]
```

| Subcommand | Description |
|------------|-------------|
| [gc agent](#gc-agent) | Manage agent configuration |
| [gc bd](#gc-bd) | Run bd in the correct rig directory |
| [gc beads](#gc-beads) | Manage the beads provider |
| [gc build-image](#gc-build-image) | Build a prebaked agent container image |
| [gc cities](#gc-cities) | List registered cities |
| [gc completion](#gc-completion) | Generate the autocompletion script for the specified shell |
| [gc config](#gc-config) | Inspect and validate city configuration |
| [gc converge](#gc-converge) | Manage convergence loops (bounded iterative refinement) |
| [gc convoy](#gc-convoy) | Manage convoys — graphs of related work |
| [gc dashboard](#gc-dashboard) | Web dashboard for monitoring the supervisor and managed cities |
| [gc doctor](#gc-doctor) | Check workspace health |
| [gc dolt-cleanup](#gc-dolt-cleanup) | Find and remove orphaned Dolt databases (Go-side core) |
| [gc event](#gc-event) | Event operations |
| [gc events](#gc-events) | Show events from the GC API |
| [gc formula](#gc-formula) | Manage and inspect formulas |
| [gc graph](#gc-graph) | Show dependency graph for beads |
| [gc handoff](#gc-handoff) | Send handoff mail and restart controller-managed sessions |
| [gc help](#gc-help) | Help about any command |
| [gc hook](#gc-hook) | Check for available work |
| [gc import](#gc-import) | Manage pack imports |
| [gc init](#gc-init) | Initialize a new city |
| [gc mail](#gc-mail) | Send and receive messages between agents and humans |
| [gc mcp](#gc-mcp) | Inspect projected MCP config |
| [gc nudge](#gc-nudge) | Inspect and deliver deferred nudges |
| [gc order](#gc-order) | Manage orders (scheduled and event-driven dispatch) |
| [gc pack](#gc-pack) | Manage remote pack sources |
| [gc prime](#gc-prime) | Output the behavioral prompt for an agent |
| [gc register](#gc-register) | Register a city with the machine-wide supervisor |
| [gc reload](#gc-reload) | Reload the current city's config without restarting the city/controller |
| [gc restart](#gc-restart) | Restart all agent sessions in the city |
| [gc resume](#gc-resume) | Resume a suspended city |
| [gc rig](#gc-rig) | Manage rigs (projects) |
| [gc runtime](#gc-runtime) | Process-intrinsic runtime operations |
| [gc service](#gc-service) | Inspect workspace services |
| [gc session](#gc-session) | Manage interactive chat sessions |
| [gc shell](#gc-shell) | Manage the Gas City shell integration hook |
| [gc skill](#gc-skill) | List visible skills |
| [gc sling](#gc-sling) | Route work to a session config or agent |
| [gc start](#gc-start) | Start the city under the machine-wide supervisor |
| [gc status](#gc-status) | Show city-wide status overview |
| [gc stop](#gc-stop) | Stop all agent sessions in the city |
| [gc supervisor](#gc-supervisor) | Manage the machine-wide supervisor |
| [gc suspend](#gc-suspend) | Suspend the city (all agents effectively suspended) |
| [gc trace](#gc-trace) | Inspect and control session reconciler tracing |
| [gc unregister](#gc-unregister) | Remove a city from the machine-wide supervisor |
| [gc version](#gc-version) | Print gc version |
| [gc wait](#gc-wait) | Inspect and manage durable session waits |

## gc agent

Manage agent configuration in city.toml.

Runtime operations (attach, list, peek, nudge, kill, start, stop, destroy)
have moved to "gc session" and "gc runtime".

```
gc agent
```

| Subcommand | Description |
|------------|-------------|
| [gc agent add](#gc-agent-add) | Add an agent scaffold |
| [gc agent resume](#gc-agent-resume) | Resume a suspended agent |
| [gc agent suspend](#gc-agent-suspend) | Suspend an agent (reconciler will skip it) |

## gc agent add

Add a new agent scaffold under agents/&lt;name&gt;/.

Creates agents/&lt;name&gt;/prompt.template.md and, when needed,
agents/&lt;name&gt;/agent.toml. These files live in the city directory and do
not append [[agent]] blocks to city.toml.

Use --prompt-template to copy prompt content from an existing file into
the canonical prompt.template.md location. Use --dir to record a rig or
working-directory prefix in agent.toml. Use --suspended to scaffold the
agent in a suspended state.

```
gc agent add --name <name> [flags]
```

**Example:**

```
gc agent add --name mayor
  gc agent add --name polecat --dir my-project
  gc agent add --name worker --prompt-template ./worker.md --suspended
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--dir` | string |  | Working directory for the agent (relative to city root) |
| `--name` | string |  | Name of the agent |
| `--prompt-template` | string |  | Path to prompt template file (relative to city root) |
| `--suspended` | bool |  | Register the agent in suspended state |

## gc agent resume

Resume a suspended agent by clearing suspended in its durable config.

The reconciler will start the agent on its next tick. Supports bare
names (resolved via rig context) and qualified names (e.g. "myrig/worker").

```
gc agent resume <name>
```

## gc agent suspend

Suspend an agent by setting suspended=true in its durable config.

Suspended agents are skipped by the reconciler — their sessions are not
started or restarted. Existing sessions continue running but won't be
replaced if they exit. Use "gc agent resume" to restore.

```
gc agent suspend <name>
```

## gc bd

Run a bd command routed to the correct rig directory.

When beads belong to a rig (not the city root), bd must run from the
rig directory to find the correct .beads database. This command resolves
the rig automatically from the --rig flag or by detecting the bead prefix
in the arguments.

All arguments after "gc bd" are forwarded to bd unchanged.

gc bd forces BD_EXPORT_AUTO=false to prevent bd's git auto-export hook
from wedging the wrapper after printing command output. If you need
auto-export behavior, invoke bd directly.

```
gc bd [bd-args...]
```

**Example:**

```
gc bd --rig my-project list
  gc bd --rig my-project create "New task"
  gc bd show my-project-abc          # auto-detects rig from bead prefix
  gc bd list --rig my-project -s open
```

## gc beads

Manage the beads provider (backing store for issue tracking).

Subcommands for topology operations, health checking, and diagnostics.

```
gc beads
```

| Subcommand | Description |
|------------|-------------|
| [gc beads city](#gc-beads-city) | Manage canonical city endpoint topology |
| [gc beads health](#gc-beads-health) | Check beads provider health |

## gc beads city

Manage the canonical city endpoint topology for bd-backed beads stores.

Use use-managed to make the city GC-managed again. Use use-external to pin the
city to an external Dolt endpoint and rewrite inherited rig mirrors.

```
gc beads city
```

| Subcommand | Description |
|------------|-------------|
| [gc beads city use-external](#gc-beads-city-use-external) | Set the city endpoint to an external Dolt server |
| [gc beads city use-managed](#gc-beads-city-use-managed) | Set the city endpoint to GC-managed |

## gc beads city use-external

Set the city endpoint to an external Dolt server

```
gc beads city use-external [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--adopt-unverified` | bool |  | record the endpoint without live validation |
| `--dry-run` | bool |  | show the canonical changes without writing files |
| `--host` | string |  | external Dolt host |
| `--port` | string |  | external Dolt port |
| `--user` | string |  | external Dolt user |

## gc beads city use-managed

Set the city endpoint to GC-managed

```
gc beads city use-managed [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--dry-run` | bool |  | show the canonical changes without writing files |

## gc beads health

Check beads provider health and attempt recovery on failure.

Delegates to the provider's lifecycle health operation. For exec
providers (including bd/dolt), the script handles multi-tier checking
and recovery internally. For the file provider, always succeeds (no-op).

Also used by the beads-health system order for periodic monitoring.

```
gc beads health [flags]
```

**Example:**

```
gc beads health
  gc beads health --quiet
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--quiet` | bool |  | silent on success, stderr on failure |

## gc build-image

Assemble a Docker build context from city config, prompts, formulas,
and rig content, then build a container image with everything pre-staged.

Pods using the prebaked image skip init containers and file staging,
reducing startup from 30-60s to seconds. Configure with prebaked = true
in [session.k8s].

Secrets (Claude credentials) are never baked — they stay as K8s Secret
volume mounts at runtime.

```
gc build-image [city-path] [flags]
```

**Example:**

```
# Build context only (no docker build)
  gc build-image ~/bright-lights --context-only

  # Build and tag image
  gc build-image ~/bright-lights --tag my-city:latest

  # Build with rig content baked in
  gc build-image ~/bright-lights --tag my-city:latest --rig-path demo:/path/to/demo

  # Build and push to registry
  gc build-image ~/bright-lights --tag registry.io/my-city:latest --push
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--base-image` | string | `gc-agent:latest` | base Docker image |
| `--context-only` | bool |  | write build context without running docker build |
| `--push` | bool |  | push image after building |
| `--rig-path` | stringSlice |  | rig name:path pairs (repeatable) |
| `--tag` | string |  | image tag (required unless --context-only) |

## gc cities

List all cities registered with the machine-wide supervisor.

```
gc cities
```

| Subcommand | Description |
|------------|-------------|
| [gc cities list](#gc-cities-list) | List registered cities |

## gc cities list

List registered cities

```
gc cities list
```

## gc completion

Generate the autocompletion script for gc for the specified shell.
See each sub-command's help for details on how to use the generated script.

```
gc completion
```

| Subcommand | Description |
|------------|-------------|
| [gc completion bash](#gc-completion-bash) | Generate the autocompletion script for bash |
| [gc completion fish](#gc-completion-fish) | Generate the autocompletion script for fish |
| [gc completion powershell](#gc-completion-powershell) | Generate the autocompletion script for powershell |
| [gc completion zsh](#gc-completion-zsh) | Generate the autocompletion script for zsh |

## gc completion bash

Generate the autocompletion script for the bash shell.

This script depends on the 'bash-completion' package.
If it is not installed already, you can install it via your OS's package manager.

To load completions in your current shell session:

	source &lt;(gc completion bash)

To load completions for every new session, execute once:

#### Linux:

	gc completion bash &gt; /etc/bash_completion.d/gc

#### macOS:

	gc completion bash &gt; $(brew --prefix)/etc/bash_completion.d/gc

You will need to start a new shell for this setup to take effect.

```
gc completion bash
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--no-descriptions` | bool |  | disable completion descriptions |

## gc completion fish

Generate the autocompletion script for the fish shell.

To load completions in your current shell session:

	gc completion fish | source

To load completions for every new session, execute once:

	gc completion fish &gt; ~/.config/fish/completions/gc.fish

You will need to start a new shell for this setup to take effect.

```
gc completion fish [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--no-descriptions` | bool |  | disable completion descriptions |

## gc completion powershell

Generate the autocompletion script for powershell.

To load completions in your current shell session:

	gc completion powershell | Out-String | Invoke-Expression

To load completions for every new session, add the output of the above command
to your powershell profile.

```
gc completion powershell [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--no-descriptions` | bool |  | disable completion descriptions |

## gc completion zsh

Generate the autocompletion script for the zsh shell.

If shell completion is not already enabled in your environment you will need
to enable it.  You can execute the following once:

	echo "autoload -U compinit; compinit" &gt;&gt; ~/.zshrc

To load completions in your current shell session:

	source &lt;(gc completion zsh)

To load completions for every new session, execute once:

#### Linux:

	gc completion zsh &gt; "$&#123;fpath[1]&#125;/_gc"

#### macOS:

	gc completion zsh &gt; $(brew --prefix)/share/zsh/site-functions/_gc

You will need to start a new shell for this setup to take effect.

```
gc completion zsh [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--no-descriptions` | bool |  | disable completion descriptions |

## gc config

Inspect, validate, and debug the resolved city configuration.

The config system supports multi-file composition with includes,
packs, patches, and overrides. Use "show" to dump the resolved
config and "explain" to see where each value originated.

```
gc config
```

| Subcommand | Description |
|------------|-------------|
| [gc config explain](#gc-config-explain) | Show resolved config with provenance annotations |
| [gc config show](#gc-config-show) | Dump the resolved city configuration as TOML |

## gc config explain

Show the resolved configuration with provenance.

For agents (default): displays every resolved field with an annotation
showing which config file provided the value. Use --rig and --agent to
filter.

For providers (--provider): displays the resolved ProviderSpec along
with per-field and per-map-key attribution — which chain layer
(builtin:X or providers.Y) contributed each value. Useful for
debugging base-chain inheritance.

Use --json to emit machine-readable output (providers only).

```
gc config explain [flags]
```

**Example:**

```
gc config explain
  gc config explain --agent mayor
  gc config explain --rig my-project
  gc config explain --provider codex-max
  gc config explain --provider codex-max --json
  gc config explain -f overlay.toml --agent polecat
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--agent` | string |  | filter to a specific agent name |
| `-f`, `--file` | stringArray |  | additional config files to layer (can be repeated) |
| `--json` | bool |  | emit JSON (requires --provider) |
| `--provider` | string |  | explain a provider's resolved chain instead of agents |
| `--rig` | string |  | filter to agents in this rig |

## gc config show

Dump the fully resolved city configuration as TOML.

Loads city.toml with all includes, packs, patches, and overrides,
then outputs the merged result. Use --validate to check for errors
without printing. Use --provenance to see which file contributed each
config element. Use -f to layer additional config files.

```
gc config show [flags]
```

**Example:**

```
gc config show
  gc config show --validate
  gc config show --provenance
  gc config show -f overlay.toml
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-f`, `--file` | stringArray |  | additional config files to layer (can be repeated) |
| `--provenance` | bool |  | show where each config element originated |
| `--validate` | bool |  | validate config and exit (0 = valid, 1 = errors) |

## gc converge

Convergence loops are bounded multi-step refinement cycles.

A root bead + formula + gate = repeat until the gate passes or max
iterations are reached. The controller processes wisp_closed events
and drives the loop automatically.

```
gc converge
```

| Subcommand | Description |
|------------|-------------|
| [gc converge approve](#gc-converge-approve) | Approve and close a convergence loop (manual gate) |
| [gc converge create](#gc-converge-create) | Create a convergence loop |
| [gc converge iterate](#gc-converge-iterate) | Force next iteration (manual gate) |
| [gc converge list](#gc-converge-list) | List convergence loops |
| [gc converge retry](#gc-converge-retry) | Retry a terminated convergence loop |
| [gc converge status](#gc-converge-status) | Show convergence loop status |
| [gc converge stop](#gc-converge-stop) | Stop a convergence loop |
| [gc converge test-gate](#gc-converge-test-gate) | Dry-run the gate condition (no state changes) |

## gc converge approve

Approve and close a convergence loop (manual gate)

```
gc converge approve <bead-id>
```

## gc converge create

Create a convergence loop

```
gc converge create [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--evaluate-prompt` | string |  | Custom evaluate prompt (overrides formula default) |
| `--formula` | string |  | Formula to use (required) |
| `--gate` | string | `manual` | Gate mode: manual, condition, hybrid |
| `--gate-condition` | string |  | Path to gate condition script |
| `--gate-timeout` | string | `5m0s` | Gate execution timeout |
| `--gate-timeout-action` | string | `iterate` | Action on gate timeout: iterate, retry, manual, terminate |
| `--max-iterations` | int | `5` | Maximum iterations |
| `--target` | string |  | Target agent (required) |
| `--title` | string |  | Convergence loop title |
| `--var` | stringArray |  | Template variable (key=value, repeatable) |

## gc converge iterate

Force next iteration (manual gate)

```
gc converge iterate <bead-id>
```

## gc converge list

List convergence loops

```
gc converge list [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--all` | bool |  | Include closed/terminated loops |
| `--json` | bool |  | Output as JSON |
| `--state` | string |  | Filter by state (active, waiting_manual, terminated) |

## gc converge retry

Retry a terminated convergence loop

```
gc converge retry <bead-id> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--max-iterations` | int |  | Override max iterations (default: inherit from source) |

## gc converge status

Show convergence loop status

```
gc converge status <bead-id> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | bool |  | Output as JSON |

## gc converge stop

Stop a convergence loop

```
gc converge stop <bead-id>
```

## gc converge test-gate

Dry-run the gate condition (no state changes)

```
gc converge test-gate <bead-id>
```

## gc convoy

Manage convoys — graphs of related work beads.

A convoy is a named graph of beads with dependencies. Simple convoys
group related issues via parent-child relationships. Complex convoys
use formula-compiled DAGs with control beads for orchestration.

```
gc convoy
```

| Subcommand | Description |
|------------|-------------|
| [gc convoy add](#gc-convoy-add) | Add an issue to a convoy |
| [gc convoy check](#gc-convoy-check) | Auto-close convoys where all issues are closed |
| [gc convoy close](#gc-convoy-close) | Close a convoy |
| [gc convoy control](#gc-convoy-control) | Execute control beads or run the control-dispatcher loop |
| [gc convoy create](#gc-convoy-create) | Create a convoy and optionally track issues |
| [gc convoy delete](#gc-convoy-delete) | Close or delete a convoy and all its beads |
| [gc convoy delete-source](#gc-convoy-delete-source) | Close workflows sourced from a bead |
| [gc convoy land](#gc-convoy-land) | Land an owned convoy (terminate + cleanup) |
| [gc convoy list](#gc-convoy-list) | List open convoys with progress |
| [gc convoy reopen-source](#gc-convoy-reopen-source) | Reopen a source bead after workflow cleanup |
| [gc convoy status](#gc-convoy-status) | Show detailed convoy status |
| [gc convoy stranded](#gc-convoy-stranded) | Find convoys with ready work but no workers |
| [gc convoy target](#gc-convoy-target) | Set the target branch on a convoy |

## gc convoy add

Link an existing issue bead to a convoy.

Sets the issue's parent to the convoy ID, making it appear in the
convoy's progress tracking.

```
gc convoy add <convoy-id> <issue-id>
```

## gc convoy check

Scan open convoys and auto-close any where all child issues are resolved.

Evaluates each open convoy's children. If all children have status
"closed", the convoy is automatically closed and an event is recorded.

```
gc convoy check
```

## gc convoy close

Close a convoy bead manually.

Marks the convoy as closed regardless of child issue status. Use
"gc convoy check" to auto-close convoys where all issues are resolved.

```
gc convoy close <id>
```

## gc convoy control

Process a single control bead, or run the control-dispatcher loop
with --serve to continuously process ready control beads.
Use --follow &lt;agent&gt; to filter the serve loop to a specific agent template.

```
gc convoy control [bead-id] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--follow` | string |  | Run serve loop filtered to a specific agent template |
| `--serve` | bool |  | Run the control-dispatcher loop (continuous) |

## gc convoy create

Create a convoy and optionally link existing issues to it.

Creates a convoy bead and sets the parent of any provided issue IDs to
the new convoy. Issues can also be added later with "gc convoy add".

```
gc convoy create <name> [issue-ids...] [flags]
```

**Example:**

```
gc convoy create sprint-42
  gc convoy create sprint-42 issue-1 issue-2 issue-3
  gc convoy create deploy --owner mayor --notify mayor --merge mr
  gc convoy create auth-rewrite --owned --target integration/auth-rewrite
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--merge` | string |  | merge strategy: direct, mr, local |
| `--notify` | string |  | notification target on completion |
| `--owned` | bool |  | mark convoy as owned (manual lifecycle, no auto-close) |
| `--owner` | string |  | convoy owner (who manages it) |
| `--target` | string |  | target branch inherited by child work beads |

## gc convoy delete

Close all open beads in a convoy, or delete them.

Searches all stores (city + rigs) for the convoy root and all beads
with matching gc.root_bead_id. Without --force, shows a preview.

By default, beads are closed with gc.outcome=skipped. Use --delete to
remove them from the store via bd delete --cascade --force.

```
gc convoy delete <convoy-id> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--delete` | bool |  | Delete beads from the store instead of closing |
| `-f`, `--force` | bool |  | Actually close/delete (without this, shows preview) |

## gc convoy delete-source

Find every live workflow root sourced from the given bead and close
its subtree. By default this is a preview. Use --apply to mutate.
Use --delete with --apply to also delete closed beads.

```
gc convoy delete-source <source-bead-id> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--apply` | bool |  | Actually close/delete matched workflows |
| `--delete` | bool |  | Also delete beads from the store after closing |
| `--rig` | string |  | Select the rig store for the source bead |
| `--store-ref` | string |  | Select the source bead store (city:&lt;name&gt; or rig:&lt;name&gt;) |

## gc convoy land

Land an owned convoy, verifying all children are closed.

Landing is the natural lifecycle termination for owned convoys created
via "gc sling --owned". It verifies all children are closed (or uses
--force), closes the convoy bead, and records a ConvoyClosed event.

```
gc convoy land <convoy-id> [flags]
```

**Example:**

```
gc convoy land gc-42
  gc convoy land gc-42 --force
  gc convoy land gc-42 --dry-run
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--dry-run` | bool |  | preview what would happen |
| `--force` | bool |  | land even with open children |

## gc convoy list

List all open convoys with completion progress.

Shows each convoy's ID, title, and the number of closed vs total
child issues.

```
gc convoy list
```

## gc convoy reopen-source

Reopen a source bead after workflow cleanup

```
gc convoy reopen-source <source-bead-id> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--rig` | string |  | Select the rig store for the source bead |
| `--store-ref` | string |  | Select the source bead store (city:&lt;name&gt; or rig:&lt;name&gt;) |

## gc convoy status

Show detailed status of a convoy and all its child issues.

Displays the convoy's ID, title, status, completion progress, and a
table of all child issues with their status and assignee.

```
gc convoy status <id>
```

## gc convoy stranded

Find open issues in convoys that have no assignee.

Lists issues that are ready for work but not claimed by any agent.
Useful for identifying bottlenecks in convoy processing.

```
gc convoy stranded
```

## gc convoy target

Set the target branch metadata on a convoy.

Child work beads can inherit this target branch when slung with
feature-branch formulas such as mol-polecat-work.

```
gc convoy target <convoy-id> <branch>
```

## gc dashboard

Open the static GC dashboard against the machine-wide supervisor API.

Without a city in scope, the dashboard shows supervisor-level state and managed
city tabs. From a city directory or with --city, city-specific panels and action
forms are enabled for that city.

```
gc dashboard [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--api` | string |  | GC API server URL override (auto-discovered by default) |
| `--port` | int | `8080` | HTTP port |

| Subcommand | Description |
|------------|-------------|
| [gc dashboard serve](#gc-dashboard-serve) | Start the web dashboard |

## gc dashboard serve

Start the static GC dashboard against the machine-wide supervisor API.

Without a city in scope, the dashboard shows supervisor-level state and managed
city tabs. From a city directory or with --city, city-specific panels and action
forms are enabled for that city.

```
gc dashboard serve [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--api` | string |  | GC API server URL override (auto-discovered by default) |
| `--port` | int | `8080` | HTTP port |

## gc doctor

Run diagnostic health checks on the city workspace.

Checks city structure, config validity, binary dependencies (tmux, git,
bd, dolt), controller status, agent sessions, zombie/orphan sessions,
bead stores, Dolt server health, event log integrity, and per-rig
health. Use --fix to attempt automatic repairs.

```
gc doctor [flags]
```

**Example:**

```
gc doctor
  gc doctor --fix
  gc doctor --verbose
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--fix` | bool |  | attempt to fix issues automatically |
| `-v`, `--verbose` | bool |  | show extra diagnostic details |

## gc dolt-cleanup

gc dolt-cleanup is the Go-side implementation of the operational Dolt
cleanup tool. It resolves the Dolt server port via the AD-04 chain
(--port &gt; city dolt.port &gt; &lt;rigRoot&gt;/.beads/dolt-server.port &gt; 3307),
drops stale test/agent databases, calls DOLT_PURGE_DROPPED_DATABASES
to reclaim disk, and reaps orphaned dolt sql-server processes left
over from leaked test harnesses. Invalid explicit ports and unreadable
or invalid city/rig port settings fail closed before cleanup stages run;
only absent rig port files can reach the legacy default. The legacy
default is a connection fallback only; it does not protect port 3307
from orphan-process reaping.

Dry-run by default. Pass --force to actually drop, purge, and kill.
Pass --max-orphan-dbs with --force to refuse all destructive cleanup
stages if the live apply-time stale database count exceeds the
scan-time threshold. The default 0 disables this guard; negative values
are rejected before any city lookup or cleanup stage runs.
Active rig dolt servers, registered rig databases, active test temp roots,
and processes outside the test-config-path allowlist (/tmp/Test*,
os.TempDir()/Test*, known Gas City test prefixes, ~/.gotmp/Test*) are always
protected — see the PROTECTED section of the
report. Destructive drops are limited to known stale test database name
shapes and conservative SQL identifier characters; skipped stale matches
are reported in dropped.skipped. Rig dolt_database names used for purge
must use the same identifier shape: ASCII letters, digits, underscores,
and non-leading hyphens. Missing or silent rig metadata disables forced
drop/purge because the live database name cannot be proven safe.

JSON envelope schema is stable: gc.dolt.cleanup.v1. Automation that
uses --json must inspect summary.errors_total and errors, and must also
refuse to invoke --force when dry-run force_blockers is non-empty.
force_blockers reports conditions that would block forced cleanup without
incrementing errors_total. The rig-protection blocker is intentionally
global: missing or silent rig metadata prevents forced drop/purge because
the command cannot prove all registered rig databases are protected.
Cleanup stage errors are reported in the envelope even when the command
can still return successfully after emitting the report.

```
gc dolt-cleanup [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--force` | bool |  | actually drop, purge, and kill orphaned resources (default: dry-run) |
| `--json` | bool |  | emit JSON envelope (gc.dolt.cleanup.v1) |
| `--max-orphan-dbs` | int |  | with --force, refuse cleanup when live stale database count exceeds this limit |
| `--port` | string |  | override the resolved Dolt port |
| `--probe` | bool |  | TCP-probe the resolved port; fail if unreachable |

## gc event

Event operations

```
gc event
```

| Subcommand | Description |
|------------|-------------|
| [gc event emit](#gc-event-emit) | Emit an event to the city event log |

## gc event emit

Record a custom event to the city event log.

Best-effort: always exits 0 so bead hooks never fail. Supports
attaching arbitrary JSON payloads.

```
gc event emit <type> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--actor` | string |  | Actor name (default: $GC_ALIAS, else $GC_AGENT, else $GC_SESSION_ID, else "human") |
| `--message` | string |  | Event message |
| `--payload` | string |  | JSON payload to attach to the event |
| `--subject` | string |  | Event subject (e.g. bead ID) |

## gc events

Show events from the GC API with optional filtering.

The API is the source of truth for both city-scoped and supervisor-scoped
events. In a city directory (or with --city), this command reflects the
city's /v0/city/&#123;cityName&#125;/events and /stream endpoints. Without a city in
scope, it reflects the supervisor's /v0/events and /stream endpoints.

List, watch, and follow output are always JSON Lines. Each line is one API
DTO or SSE envelope.

```
gc events [flags]
```

**Example:**

```
gc events
  gc events --type bead.created --since 1h
  gc events --watch --type convoy.closed --timeout 5m
  gc events --follow
  gc events --seq
  gc events --follow --after-cursor city-a:12,city-b:9
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--after` | uint64 |  | Resume from this city event sequence number (city scope only) |
| `--after-cursor` | string |  | Resume from this supervisor event cursor (supervisor scope only) |
| `--api` | string |  | GC API server URL override (auto-discovered by default) |
| `--follow` | bool |  | Continuously stream events as they arrive |
| `--payload-match` | stringArray |  | Filter by payload field (key=value or key.subkey=value, repeatable) |
| `--seq` | bool |  | Print the current head cursor and exit |
| `--since` | string |  | Show events since duration ago (e.g. 1h, 30m) |
| `--timeout` | string | `30s` | Max wait duration for --watch (e.g. 30s, 5m) |
| `--type` | string |  | Filter by event type (e.g. bead.created) |
| `--watch` | bool |  | Block until matching events arrive (exits after first match or buffered replay) |

## gc formula

Manage and inspect formulas

```
gc formula
```

| Subcommand | Description |
|------------|-------------|
| [gc formula cook](#gc-formula-cook) | Instantiate a formula into the current bead store |
| [gc formula list](#gc-formula-list) | List available formulas |
| [gc formula show](#gc-formula-show) | Show a compiled formula recipe |

## gc formula cook

Compile and instantiate a formula as real beads in the current store.

This is a low-level workflow construction tool. It creates the formula root
and all compiled step beads without routing any work.

With --attach=&lt;bead-id&gt;, the sub-DAG is created as children of the given
bead. The bead gains a blocking dependency on the sub-DAG root, so it won't
close until the sub-DAG completes. This is the core primitive for late-bound
DAG expansion — any agent, script, or workflow step can call it to expand a
bead into a sub-workflow at runtime.

```
gc formula cook <formula-name> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--attach` | string |  | attach sub-DAG to existing bead (bead gains blocking dep on sub-DAG root) |
| `--meta` | stringArray |  | set root bead metadata after cook (key=value, repeatable) |
| `-t`, `--title` | string |  | override root bead title |
| `--var` | stringArray |  | variable substitution for formula (key=value, repeatable) |

## gc formula list

List all formulas available in the city's formula search paths.

Formulas are discovered from city-level and rig-level formula directories
configured via packs and formulas_dir settings.

```
gc formula list
```

## gc formula show

Compile and display a formula recipe.

By default, shows the recipe with &#123;&#123;variable&#125;&#125; placeholders intact.
Use --var to substitute variables and preview the resolved output.

Examples:
  gc formula show mol-feature
  gc formula show mol-feature --var title="Auth system" --var branch=main

```
gc formula show <formula-name> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--var` | stringArray |  | variable substitution for preview (key=value) |

## gc graph

Show the dependency graph for a set of beads or a convoy.

Resolves dependencies via the bead store and prints each bead with its
status and what blocks it. Convoys are expanded to their children
automatically. Readiness is computed within the displayed set.

By default prints a table. Use --tree for a Unicode tree view or
--mermaid for a Mermaid.js flowchart you can paste into Markdown.

```
gc graph <bead-ids|convoy-id...> [flags]
```

**Example:**

```
gc graph gc-42               # expand convoy children
  gc graph gc-1 gc-2 gc-3     # arbitrary beads
  gc graph gc-42 --tree        # dependency tree
  gc graph gc-42 --mermaid     # Mermaid.js diagram
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--mermaid` | bool |  | output Mermaid.js flowchart |
| `--tree` | bool |  | output Unicode dependency tree |

## gc handoff

Convenience command for context handoff.

Self-handoff (default): sends mail to self. If the current session is
controller-restartable, requests a restart and blocks until the controller
stops the session. For on-demand configured named sessions, sends mail and
returns without requesting restart because the controller cannot restart the
user-attended process.

For controller-restartable sessions, equivalent to:

  gc mail send $GC_ALIAS &lt;subject&gt; [message]
  gc runtime request-restart

Under normal operation the controller stops controller-restartable
self-handoff sessions before this command returns. If the controller does not
act within a bounded timeout, gc handoff exits 1 with a diagnostic instead of
blocking indefinitely. If interrupted, the restart request remains set for the
controller to process on its next reconcile tick.

Auto handoff (--auto): sends mail to self and returns without requesting a
restart. This is for PreCompact hooks, where the provider is already managing
the context compaction lifecycle.

Remote handoff (--target): sends mail to a target session. If the target is
controller-restartable, kills it so the reconciler restarts it with the handoff
mail waiting. For on-demand configured named targets, sends mail and returns
without killing the session.

For controller-restartable targets, equivalent to:

  gc mail send &lt;target&gt; &lt;subject&gt; [message]
  gc session kill &lt;target&gt;

Self-handoff requires session context (GC_ALIAS or GC_SESSION_ID, plus
GC_SESSION_NAME and city context env). Remote handoff accepts a session alias
or ID. Subject is required unless --auto is set.

```
gc handoff [subject] [message] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--auto` | bool |  | Send handoff mail without requesting restart (for PreCompact hooks) |
| `--hook-format` | string |  | format hook output for a provider |
| `--target` | string |  | Remote session alias or ID to handoff (kills only controller-restartable sessions) |

## gc help

Help provides help for any command in the application.
Simply type gc help [path to command] for full details.

```
gc help [command]
```

## gc hook

Checks for available work using the agent's work_query config.

Without --inject: prints raw output, exits 0 if work exists, 1 if empty.
With --inject: silent legacy Stop-hook compatibility; skips the work query and always exits 0.

		The agent is determined from $GC_AGENT or a positional argument.

```
gc hook [agent] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--inject` | bool |  | silent legacy Stop-hook compatibility; skip work query and exit 0 |

## gc import

Manage pack imports

```
gc import
```

| Subcommand | Description |
|------------|-------------|
| [gc import add](#gc-import-add) | Add a pack import |
| [gc import check](#gc-import-check) | Validate installed pack import state |
| [gc import install](#gc-import-install) | Install imports from pack.toml and packs.lock |
| [gc import list](#gc-import-list) | List imported packs |
| [gc import migrate](#gc-import-migrate) | Migrate a V1 city layout to the V2 pack shape |
| [gc import remove](#gc-import-remove) | Remove a pack import |
| [gc import upgrade](#gc-import-upgrade) | Upgrade imported packs within their constraints |
| [gc import why](#gc-import-why) | Explain why an import is present |

## gc import add

Add a pack import

```
gc import add <source> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--name` | string |  | Local binding name override |
| `--version` | string |  | Version constraint for git-backed imports |

## gc import check

Validate installed pack import state

```
gc import check
```

## gc import install

Install imports from pack.toml and packs.lock

```
gc import install
```

## gc import list

List imported packs

```
gc import list [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--tree` | bool |  | Show the import dependency tree |

## gc import migrate

Rewrite a legacy city into the V2 migration shape.

Moves workspace.includes into pack imports, converts [[agent]] tables
into agents/&lt;name&gt;/ directories, and stages prompt/overlay/namepool
assets into their V2 locations.

```
gc import migrate [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--dry-run` | bool |  | print what would change without writing |

## gc import remove

Remove a pack import

```
gc import remove <name>
```

## gc import upgrade

Upgrade imported packs within their constraints

```
gc import upgrade [name]
```

## gc import why

Explain why an import is present

```
gc import why <name-or-source>
```

## gc init

Create a new Gas City workspace in the given directory (or cwd).

Runs an interactive wizard to choose a config template and coding agent
provider. Creates the .gc/ runtime directory plus pack.toml, city.toml,
the standard top-level directories, and .template.md prompt templates, then
materializes builtin packs under .gc/system/packs. Use --provider to create the default minimal city
non-interactively, or --file to initialize from an existing TOML config file.

Pass --preserve-existing to keep any pre-authored pack.toml, city.toml, or
agent prompt files in the target directory (useful when bootstrapping a
committed workspace — e.g. from a bootstrap.sh shipped in the repo).

```
gc init [path] [flags]
```

**Example:**

```
gc init
  gc init ~/my-city
  gc init --provider codex ~/my-city
  gc init --provider codex --bootstrap-profile k8s-cell /city
  gc init --name my-city
  gc init --from ~/elan --name elan /city
  gc init --file examples/gastown.toml ~/bright-lights
  gc init --file city.toml --preserve-existing .
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--bootstrap-profile` | string |  | bootstrap profile to apply for hosted/container defaults |
| `--file` | string |  | path to a TOML file to use as city.toml |
| `--from` | string |  | path to an example city directory to copy |
| `--name` | string |  | workspace name (default: target directory basename) |
| `--preserve-existing` | bool |  | keep any pre-authored pack.toml, city.toml, or agent prompt files instead of overwriting them |
| `--provider` | string |  | built-in workspace provider to use for the default mayor config |
| `--skip-provider-readiness` | bool |  | skip provider login/readiness checks during init and continue startup |

## gc mail

Send and receive messages between agents and humans.

Mail is implemented as beads with type="message". Messages have a
sender, recipient, subject, and body. Use "gc mail check --inject" in agent
hooks to deliver mail notifications into agent prompts.

```
gc mail
```

| Subcommand | Description |
|------------|-------------|
| [gc mail archive](#gc-mail-archive) | Archive one or more messages without reading them |
| [gc mail check](#gc-mail-check) | Check for unread mail (use --inject for hook output) |
| [gc mail count](#gc-mail-count) | Show total/unread message count |
| [gc mail delete](#gc-mail-delete) | Delete one or more messages (closes the beads) |
| [gc mail inbox](#gc-mail-inbox) | List unread messages (defaults to your inbox) |
| [gc mail mark-read](#gc-mail-mark-read) | Mark a message as read |
| [gc mail mark-unread](#gc-mail-mark-unread) | Mark a message as unread |
| [gc mail peek](#gc-mail-peek) | Show a message without marking it as read |
| [gc mail read](#gc-mail-read) | Read a message and mark it as read |
| [gc mail reply](#gc-mail-reply) | Reply to a message |
| [gc mail send](#gc-mail-send) | Send a message to a session alias or human |
| [gc mail thread](#gc-mail-thread) | List all messages in a thread |

## gc mail archive

Close one or more message beads without displaying their contents.

Use this to dismiss messages without reading them. Each message is marked
as closed and will no longer appear in mail check or inbox results. When
multiple IDs are passed, they are archived in a single batch round-trip.

```
gc mail archive <id>...
```

## gc mail check

Check for unread mail addressed to a session alias or mailbox.

Without --inject: prints the count and exits 0 if mail exists, 1 if
empty. With --inject: outputs a &lt;system-reminder&gt; block suitable for
hook injection (always exits 0). The recipient defaults to $GC_SESSION_ID,
$GC_ALIAS, $GC_AGENT, or "human".

```
gc mail check [session] [flags]
```

**Example:**

```
gc mail check
  gc mail check --inject
  gc mail check mayor
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--hook-format` | string |  | format hook output for a provider |
| `--inject` | bool |  | output &lt;system-reminder&gt; block for hook injection |

## gc mail count

Show total and unread message counts for a session alias or human.
The recipient defaults to $GC_SESSION_ID, $GC_ALIAS, $GC_AGENT, or "human".

```
gc mail count [session]
```

## gc mail delete

Delete one or more messages by closing the beads. Same effect as archive
but with different user intent. When multiple IDs are passed, they are
deleted in a single batch round-trip.

```
gc mail delete <id>...
```

## gc mail inbox

List all unread messages for a session alias or human.

Shows message ID, sender, subject, and body in a table. The recipient defaults
to $GC_SESSION_ID, $GC_ALIAS, $GC_AGENT, or "human". Pass a session alias to view another inbox.

```
gc mail inbox [session]
```

## gc mail mark-read

Mark a message as read without displaying it. The message will no longer appear in inbox results.

```
gc mail mark-read <id>
```

## gc mail mark-unread

Mark a message as unread. The message will appear again in inbox results.

```
gc mail mark-unread <id>
```

## gc mail peek

Display a message without marking it as read.

Same output as "gc mail read" but does not change the message's read status.
The message will continue to appear in inbox results.

```
gc mail peek <id>
```

## gc mail read

Display a message and mark it as read.

Shows the full message details (ID, sender, recipient, subject, date, body).
The message stays in the store — use "gc mail archive" to permanently close it.

```
gc mail read <id>
```

## gc mail reply

Reply to a message. The reply is addressed to the original sender.

Inherits the thread ID from the original message for conversation tracking.
Use --notify to nudge the recipient after replying.
Use -s/--subject for the reply subject and -m/--message for the reply body.

```
gc mail reply <id> [-s subject] [-m body] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-m`, `--message` | string |  | reply body text |
| `--notify` | bool |  | nudge the recipient after replying |
| `-s`, `--subject` | string |  | reply subject line |

## gc mail send

Send a message to a session alias or human.

Creates a message bead addressed to the recipient. The sender defaults
to $GC_SESSION_ID, $GC_ALIAS, $GC_AGENT, or "human". Use --notify to nudge
the recipient after sending. Use --from to override the sender identity.
Use --to as an alternative to the positional &lt;to&gt; argument.
Use -s/--subject for the summary line and -m/--message for the body text.
Use --all to broadcast to all live sessions (excluding sender and "human").

```
gc mail send [<to>] [<body>] [flags]
```

**Example:**

```
gc mail send mayor "Build is green"
  gc mail send mayor -s "Build is green"
  gc mail send myrig/witness -s "Need investigation" -m "Attach logs from the last failed run"
  gc mail send --to mayor "Build is green"
  gc mail send human "Review needed for PR #42"
  gc mail send polecat "Priority task" --notify
  gc mail send --all "Status update: tests passing"
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--all` | bool |  | broadcast to all live sessions (excludes sender and human) |
| `--from` | string |  | sender identity (default: $GC_SESSION_ID, $GC_ALIAS, $GC_AGENT, or "human") |
| `-m`, `--message` | string |  | message body text |
| `--notify` | bool |  | nudge the recipient after sending |
| `-s`, `--subject` | string |  | message subject line |
| `--to` | string |  | recipient address (alternative to positional argument) |

## gc mail thread

Show all messages sharing a thread ID or message ID, ordered by time.

```
gc mail thread <id>
```

## gc mcp

Inspect the projected MCP catalog for a concrete target.

Projected MCP is target-specific. Use "gc mcp list --agent &lt;name&gt;" when
the agent has a single deterministic projection target from config, or
"gc mcp list --session &lt;id&gt;" for a live session target.

```
gc mcp
```

| Subcommand | Description |
|------------|-------------|
| [gc mcp list](#gc-mcp-list) | Show projected MCP servers |

## gc mcp list

Show the precedence-resolved MCP servers that Gas City would project into the provider-native config for one agent or session target.

```
gc mcp list [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--agent` | string |  | show the projected MCP config for this agent |
| `--session` | string |  | show the projected MCP config for this session |

## gc nudge

Inspect and deliver deferred nudges.

Deferred nudges are reminders that were queued because the target agent
was asleep or was not at a safe interactive boundary yet.

```
gc nudge
```

| Subcommand | Description |
|------------|-------------|
| [gc nudge status](#gc-nudge-status) | Show queued and dead-letter nudges for a session |

## gc nudge status

Show queued and dead-letter nudges for a session.

Defaults to $GC_ALIAS or $GC_SESSION_ID when run inside a session.

```
gc nudge status [session]
```

## gc order

Manage orders — scheduled or event-driven dispatch of formulas and scripts.

Orders live in flat orders/&lt;name&gt;.toml files. Each order pairs a trigger
condition (cooldown, cron, condition, event, or manual) with an action
(a formula or an exec script). The controller evaluates triggers on each
tick and dispatches work when a trigger opens.

```
gc order
```

| Subcommand | Description |
|------------|-------------|
| [gc order check](#gc-order-check) | Check which orders are due to run |
| [gc order history](#gc-order-history) | Show order execution history |
| [gc order list](#gc-order-list) | List available orders |
| [gc order run](#gc-order-run) | Execute an order manually |
| [gc order show](#gc-order-show) | Show details of an order |
| [gc order sweep-tracking](#gc-order-sweep-tracking) | Close stale order-tracking beads |

## gc order check

Evaluate trigger conditions for all orders and show which are due.

Prints a table with each order's trigger, due status, and reason. Returns
exit code 0 if any order is due, 1 if none are due.

```
gc order check
```

## gc order history

Show execution history for orders.

Queries bead history for past order runs. Optionally filter by order
name. Use --rig to filter by rig.

```
gc order history [name] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--rig` | string |  | rig name to filter order history |

## gc order list

List all available orders with their trigger type, schedule, and target.

Scans orders/ directories for flat .toml files defining trigger conditions,
scheduling parameters, and target pools.

```
gc order list
```

## gc order run

Execute an order manually, bypassing its trigger conditions.

Instantiates a wisp from the order's formula and routes it to the
configured target (if any). Useful for testing orders or triggering
them outside their normal schedule.
Use --rig to disambiguate same-name orders in different rigs.

```
gc order run <name> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--rig` | string |  | rig name to disambiguate same-name orders |

## gc order show

Display detailed information about a named order.

Shows the order name, description, formula reference, trigger type,
scheduling parameters, check command, target, and source file.
Use --rig to disambiguate same-name orders in different rigs.

```
gc order show <name> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--rig` | string |  | rig name to disambiguate same-name orders |

## gc order sweep-tracking

Close stale open order-tracking beads.

This is intended for maintenance exec orders. It only closes tracking beads
older than --stale-after so a fresh in-flight order is not interrupted.

```
gc order sweep-tracking [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--quiet` | bool |  | suppress success output |
| `--stale-after` | duration | `10m0s` | minimum age for an open tracking bead to be closed |

## gc pack

Manage remote pack sources that provide agent configurations.

Packs are git repositories containing pack.toml files that
define agent configurations for rigs. They are cached locally and
can be pinned to specific git refs.

```
gc pack
```

| Subcommand | Description |
|------------|-------------|
| [gc pack fetch](#gc-pack-fetch) | Clone missing and update existing remote packs |
| [gc pack list](#gc-pack-list) | Show remote pack sources and cache status |

## gc pack fetch

Clone missing and update existing remote pack caches.

Fetches all configured pack sources from their git repositories,
updates the local cache, and writes a lockfile with commit hashes
for reproducibility. Automatically called during "gc start".

```
gc pack fetch
```

## gc pack list

Show configured pack sources with their cache status.

Displays each pack's name, source URL, git ref, cache status,
and locked commit hash (if available).

```
gc pack list
```

## gc prime

Outputs the behavioral prompt for an agent.

Use it to prime any CLI coding agent with city-aware instructions:
  claude "$(gc prime mayor)"
  codex --prompt "$(gc prime worker)"

Runtime hook profiles may call `gc prime --hook`.
When agent-name is omitted, `GC_ALIAS` is used (falling back to `GC_AGENT`).

If agent-name matches a configured agent with a prompt_template,
that template is output. Otherwise outputs a default worker prompt.

Pass --strict to fail on debugging mistakes instead of silently falling
back to the default prompt. Strict errors on:

  - no city config found
  - city config fails to load
  - no agent name given (from args, GC_ALIAS, or GC_AGENT)
  - agent name not in city config (typo detection — the main use case)
  - agent's prompt_template points at a file that cannot be read

Strict does NOT error on agents whose config intentionally lacks a
prompt_template (a supported minimal config), on templates that render
to empty output from valid conditional logic, or on suspended states
(city or agent) — those are legitimate quiet states, not mistakes.

```
gc prime [agent-name] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--hook` | bool |  | compatibility mode for runtime hook invocations |
| `--hook-format` | string |  | format hook output for a provider |
| `--strict` | bool |  | fail on missing city, missing or unknown agent, or unreadable prompt_template instead of falling back to the default prompt |

## gc register

Register a city directory with the machine-wide supervisor.

If no path is given, registers the current city (discovered from cwd).
Use --name to set the machine-local registration alias. The alias is stored
in the machine-local supervisor registry and never written back to city.toml.
When --name is omitted, the current effective city identity is used
(site-bound workspace name if present, otherwise legacy workspace.name,
otherwise the directory basename) — in every case city.toml is not modified.
Registration is idempotent — registering the same city twice is a no-op.
The supervisor is started if needed and immediately reconciles the city.

```
gc register [path] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--name` | string |  | machine-local alias for this city registration |

## gc reload

Force the current city controller to re-read effective config and
process one reload tick without restarting the city/controller.

Reload may fetch configured remote packs before recomputing effective
config. Existing per-session restarts may still happen if normal config
drift rules require them.

```
gc reload [path] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--async` | bool |  | Return after the controller accepts the reload request |
| `--timeout` | string | `5m` | How long to wait for reload completion |

## gc restart

Restart the city by stopping it then starting it again.

Equivalent to running "gc stop" followed by "gc start". Under supervisor
mode this unregisters the city, then re-registers it and triggers an
immediate reconcile.

```
gc restart [path]
```

## gc resume

Resume a suspended city by clearing workspace.suspended in city.toml.

Restores normal operation: the reconciler will spawn agents again and
gc hook/prime will return work. Use "gc agent resume" to resume
individual agents, or "gc rig resume" for rigs.

```
gc resume [path]
```

## gc rig

Manage rigs (external project directories) registered with the city.

Rigs are project directories that the city orchestrates. Each rig gets
its own beads database, agent hooks, and cross-rig routing. Agents
are scoped to rigs via their "dir" field.

```
gc rig
```

| Subcommand | Description |
|------------|-------------|
| [gc rig add](#gc-rig-add) | Register a project as a rig |
| [gc rig list](#gc-rig-list) | List registered rigs |
| [gc rig remove](#gc-rig-remove) | Remove a rig from the city |
| [gc rig restart](#gc-rig-restart) | Restart all agents in a rig |
| [gc rig resume](#gc-rig-resume) | Resume a suspended rig |
| [gc rig set-endpoint](#gc-rig-set-endpoint) | Set the canonical endpoint ownership for a rig |
| [gc rig status](#gc-rig-status) | Show rig status and agent running state |
| [gc rig suspend](#gc-rig-suspend) | Suspend a rig (reconciler will skip its agents) |

## gc rig add

Register an external project directory as a rig.

Initializes beads database, installs agent hooks if configured,
generates cross-rig routes, and appends the rig to city.toml.
If the target directory doesn't exist, it is created. Use --include
to apply a pack directory that defines the rig's agent configuration;
repeat the flag to compose multiple packs for one rig.

Use --name to set the rig name explicitly (default: directory basename).
Use --prefix to set the bead ID prefix explicitly (default: derived from name).
Use --default-branch to set the rig's mainline branch explicitly. By default,
gc rig add probes the repo's origin/HEAD (and falls back to the currently
checked-out branch) and stores the result in city.toml so polecats and the
refinery target the right branch without manual metadata patching.
Use --start-suspended to add the rig in a suspended state (dormant-by-default).
The rig's agents won't spawn until explicitly resumed with "gc rig resume".

Use --adopt to register a directory that already has a fully initialized
.beads/ directory (must include both metadata.json and config.yaml).
Skips beads init; the git repo check remains informational.

```
gc rig add <path> [flags]
```

**Example:**

```
gc rig add /path/to/project
  gc rig add /path/to/project --name myrig
  gc rig add /path/to/project --prefix r1
  gc rig add /path/to/master-repo --default-branch master
  gc rig add ./my-project --include packs/gastown
  gc rig add ./my-project --include packs/planner --include packs/architect
  gc rig add ./my-project --include packs/gastown --start-suspended
  gc rig add /path/to/existing --adopt
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--adopt` | bool |  | adopt existing .beads/ directory (skip init) |
| `--default-branch` | string |  | mainline branch (default: auto-detect from origin/HEAD or current branch) |
| `--include` | stringArray |  | pack directory for rig agents (repeatable) |
| `--name` | string |  | rig name (default: directory basename) |
| `--prefix` | string |  | bead ID prefix (default: derived from name) |
| `--start-suspended` | bool |  | add rig in suspended state (dormant-by-default) |

## gc rig list

List all registered rigs with their paths, prefixes, default branches, and beads status.

Shows the HQ rig (the city itself) and all configured rigs. Each rig
displays its bead ID prefix, recorded default branch when set, and whether
its beads database is initialized.

```
gc rig list [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | bool |  | Output in JSON format |

## gc rig remove

Remove a rig from the current city's configuration.

Removes the rig entry from city.toml and removes its machine-local path
binding from .gc/site.toml.

```
gc rig remove <name>
```

**Example:**

```
gc rig remove myrig
```

## gc rig restart

Kill all agent sessions belonging to a rig.

The reconciler will restart the agents on its next tick. This is a
quick way to force-refresh all agents working on a particular project.

```
gc rig restart [name]
```

## gc rig resume

Resume a suspended rig by clearing suspended in city.toml.

The reconciler will start the rig's agents on its next tick.

```
gc rig resume [name]
```

## gc rig set-endpoint

Set the canonical endpoint ownership for a rig.

Use --inherit to make a rig derive its endpoint from the current city
topology. Use --external to pin the rig to its own external Dolt endpoint.
Use --self to mark the rig as running its own local Dolt server on
127.0.0.1 at the given --port; while the city is in managed_city mode the
command requires --force because the rig's .beads/dolt-server.port mirror
will no longer track the managed city Dolt.

This command owns the rig's canonical .beads/config.yaml topology state.

```
gc rig set-endpoint <rig> [flags]
```

**Example:**

```
gc rig set-endpoint frontend --inherit
  gc rig set-endpoint frontend --external --host db.example.com --port 3307
  gc rig set-endpoint frontend --external --host db.example.com --port 3307 --user agent --adopt-unverified
  gc rig set-endpoint frontend --self --port 28232 --force
  gc rig set-endpoint frontend --inherit --dry-run
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--adopt-unverified` | bool |  | record the endpoint without live validation |
| `--dry-run` | bool |  | show the canonical changes without writing files |
| `--external` | bool |  | set an explicit external endpoint for the rig |
| `--force` | bool |  | acknowledge conflicting managed-city state when using --self |
| `--host` | string |  | external Dolt host |
| `--inherit` | bool |  | inherit the city endpoint |
| `--port` | string |  | external Dolt port (required with --external or --self) |
| `--self` | bool |  | mark the rig as running its own local Dolt on 127.0.0.1 |
| `--user` | string |  | external Dolt user |

## gc rig status

Show rig status and agent running state

```
gc rig status [name]
```

## gc rig suspend

Suspend a rig by setting suspended=true in city.toml.

All agents scoped to the suspended rig are effectively suspended —
the reconciler skips them and gc hook returns empty. The rig's beads
database remains accessible. Use "gc rig resume" to restore.

```
gc rig suspend [name]
```

## gc runtime

Process-intrinsic runtime operations called by agent code from within sessions.

These commands read and write session metadata to coordinate lifecycle
events (drain, restart) between agents and the controller. They are
designed to be called from within running agent sessions, not by humans.

```
gc runtime
```

| Subcommand | Description |
|------------|-------------|
| [gc runtime drain](#gc-runtime-drain) | Signal a session to drain (wind down gracefully) |
| [gc runtime drain-ack](#gc-runtime-drain-ack) | Acknowledge drain — signal the controller to stop this session |
| [gc runtime drain-check](#gc-runtime-drain-check) | Check if a session is draining (exit 0 = draining) |
| [gc runtime request-restart](#gc-runtime-request-restart) | Request controller restart this session (waits to be killed) |
| [gc runtime undrain](#gc-runtime-undrain) | Cancel drain on a session |

## gc runtime drain

Signal a session to drain — wind down its current work gracefully.

Sets a GC_DRAIN metadata flag on the session. The agent should check
for drain status periodically (via "gc runtime drain-check") and finish
its current task before exiting. Pass a session alias or ID. Use
"gc runtime undrain" to cancel.

```
gc runtime drain <name>
```

## gc runtime drain-ack

Acknowledge a drain signal — tell the controller to stop this session.

Sets GC_DRAIN_ACK metadata on the session. The controller will stop
the session on its next reconcile tick. Call this after the session has
finished its current work in response to a drain signal.

```
gc runtime drain-ack [name]
```

## gc runtime drain-check

Check if a session is currently draining.

Returns exit code 0 if draining, 1 if not. Designed for use in
conditionals: "if gc runtime drain-check; then finish-up; fi". Without
arguments, uses the current session context.

```
gc runtime drain-check [name]
```

## gc runtime request-restart

Signal the controller to stop and restart this session.

Sets GC_RESTART_REQUESTED metadata on the session, then waits while the
controller stops the session on its next reconcile tick and restarts it
fresh. The wait keeps the agent idle so it does not consume more context
in the interim.

Under normal operation the controller SIGKILLs the process tree before
this command returns. If the controller accepts the stop handoff, the
runtime is already gone, or a SIGINT/SIGTERM is received, the command
exits 0 cleanly. If the controller has not acted within a bounded
timeout (max(5*PatrolInterval, 5min), capped at 30min) the command exits
1 with a diagnostic pointing at controller health.

For on-demand configured named sessions, the controller cannot restart
the user-attended process. In that case this command reports that
restart was skipped and returns immediately. No session.draining event
is emitted when restart is skipped.

This command is designed to be called from within a session context.
It emits a session.draining event before waiting.

```
gc runtime request-restart
```

## gc runtime undrain

Cancel a pending drain signal on a session.

Clears the GC_DRAIN and GC_DRAIN_ACK metadata flags, allowing the
session to continue normal operation. Pass a session alias or ID.

```
gc runtime undrain <name>
```

## gc service

Inspect workspace services

```
gc service
```

| Subcommand | Description |
|------------|-------------|
| [gc service doctor](#gc-service-doctor) | Show detailed workspace service status |
| [gc service list](#gc-service-list) | List workspace services |
| [gc service restart](#gc-service-restart) | Restart a workspace service |

## gc service doctor

Show detailed workspace service status

```
gc service doctor <name>
```

## gc service list

List workspace services

```
gc service list
```

## gc service restart

Stop and restart a workspace service by name.

The controller closes the current service process and starts a fresh one.
Useful after updating pack scripts without a full city restart.

```
gc service restart <name>
```

## gc session

Create, resume, suspend, and close persistent conversations with agents.

Sessions are conversations backed by agent templates. They can be
suspended to free resources and resumed later with full conversation
continuity.

```
gc session
```

| Subcommand | Description |
|------------|-------------|
| [gc session attach](#gc-session-attach) | Attach to (or resume) a chat session |
| [gc session close](#gc-session-close) | Close a session permanently |
| [gc session kill](#gc-session-kill) | Force-kill session runtime (reconciler restarts) |
| [gc session list](#gc-session-list) | List chat sessions |
| [gc session logs](#gc-session-logs) | Show session logs for a session |
| [gc session new](#gc-session-new) | Create a new chat session from an agent template |
| [gc session nudge](#gc-session-nudge) | Send a text message to a running session |
| [gc session peek](#gc-session-peek) | View session output without attaching |
| [gc session pin](#gc-session-pin) | Keep a session awake |
| [gc session prune](#gc-session-prune) | Close old suspended sessions |
| [gc session rename](#gc-session-rename) | Rename a session |
| [gc session reset](#gc-session-reset) | Restart a session fresh while preserving the bead |
| [gc session submit](#gc-session-submit) | Submit a message with semantic delivery intent |
| [gc session suspend](#gc-session-suspend) | Suspend a session (save state, free resources) |
| [gc session unpin](#gc-session-unpin) | Remove a session awake pin |
| [gc session wait](#gc-session-wait) | Register a dependency wait for a session |
| [gc session wake](#gc-session-wake) | Wake a session (request start and clear holds) |

## gc session attach

Attach to a running session or resume a suspended one.

If the session is active with a live tmux session, reattaches.
If the session is suspended or the tmux session died, resumes
using the provider's resume mechanism (if supported) or restarts.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).

```
gc session attach <session-id-or-alias>
```

## gc session close

End a conversation. Stops the runtime if active and closes the bead.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).

```
gc session close <session-id-or-alias>
```

## gc session kill

Force-kill the runtime process for a session without changing its bead state.

The session remains marked as active, so the reconciler will detect the dead
process and restart it according to the session's lifecycle rules. This is
useful for unsticking a session without losing its conversation history.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).

```
gc session kill <session-id-or-alias>
```

## gc session list

List all chat sessions. By default shows active and suspended sessions.

```
gc session list [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | bool |  | JSON output |
| `--state` | string |  | filter by state: "active", "suspended", "closed", "all" |
| `--template` | string |  | filter by template name |

## gc session logs

Show structured session log messages from a session's JSONL file.

Reads the session log, resolves the conversation DAG, and prints
messages in chronological order. Searches default paths (~/.claude/projects/)
and any extra paths from [daemon] observe_paths in city.toml.

Use --tail to print only the last N transcript entries (0 = all).
Semantics match Unix 'tail -n': '--tail 5' prints the final 5 entries,
not the first 5. A single assistant turn with multiple tool-use blocks
still counts as one entry. Compact-boundary dividers count as entries
when they fall inside the final window.

Compatibility note: before 1.0, --tail mapped to compaction segments.
As of 1.0, --tail trims the displayed transcript entry window instead.
The HTTP API's tail query parameter still uses compaction-segment
semantics.
Use -f to follow new messages as they arrive.

```
gc session logs <session> [flags]
```

**Example:**

```
gc session logs mayor
  gc session logs mayor --tail 2
  gc session logs gc-123 --tail 20
  gc session logs gc-123 --tail 0
  gc session logs s-gc-123 -f
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-f`, `--follow` | bool |  | Follow new messages as they arrive |
| `--tail` | int | `10` | Number of most recent transcript entries to show (0 = all; compact dividers count as entries) |

## gc session new

Create a new persistent conversation from an agent template defined
in the loaded city configuration. By default, attaches the terminal
after creation.

When --title-hint is provided without --title, the session title is
auto-generated from the hint text: a short version is set immediately
and refined by the title model in the background.

```
gc session new <template> [flags]
```

**Example:**

```
gc session new helper
  gc session new helper --alias sky
  gc session new helper --title "debugging auth"
  gc session new helper --title-hint "fix the login redirect loop"
  gc session new helper --no-attach
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--alias` | string |  | human-friendly session identifier for commands and mail |
| `--no-attach` | bool |  | create session without attaching |
| `--title` | string |  | human-readable session title |
| `--title-hint` | string |  | text to auto-generate a session title from |

## gc session nudge

Send text input to a running session via the runtime provider.

The message is delivered as text content to the session's input. This is
equivalent to typing the message into the session's terminal.

Accepts a session ID or session alias. Multi-word messages are
joined automatically.

```
gc session nudge <id-or-alias> <message...> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--delivery` | string | `wait-idle` | delivery mode: immediate, wait-idle, or queue |

## gc session peek

View session output without attaching

```
gc session peek <session-id-or-alias> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--lines` | int | `50` | number of lines to capture |

## gc session pin

Keep a session awake by setting its durable pin override.

Pinning does not clear suspend holds or other hard blockers. If the target is
a configured named session that has not been materialized yet, pin creates its
canonical bead so the reconciler can start it when unblocked.

```
gc session pin <session-id-or-alias>
```

## gc session prune

Close suspended sessions older than a given age. Only suspended
sessions are affected — active sessions are never pruned.

```
gc session prune [flags]
```

**Example:**

```
gc session prune --before 7d
  gc session prune --before 24h
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--before` | string | `7d` | prune sessions older than this duration (e.g., 7d, 24h) |

## gc session rename

Rename a session

```
gc session rename <session-id-or-alias> <title>
```

## gc session reset

Request a fresh restart for an existing session without closing its bead.

The controller stops the current runtime and starts the same session again with
fresh provider conversation state. Session identity, alias, mail, and queued
work remain attached to the existing session bead. For named sessions, reset
also clears any tripped named-session respawn circuit breaker before requesting
the fresh restart.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).

```
gc session reset <session-id-or-alias>
```

## gc session submit

Submit a user message to a session without choosing provider transport details.

The runtime decides whether to wake, inject immediately, or queue the message
according to the selected semantic intent.

```
gc session submit <id-or-alias> <message...> [flags]
```

**Example:**

```
gc session submit mayor "status update"
  gc session submit mayor "after this run, handle docs" --intent follow_up
  gc session submit mayor "stop and do this instead" --intent interrupt_now
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--intent` | string | `default` | submit intent: default, follow_up, or interrupt_now |

## gc session suspend

Suspend an active session by stopping its runtime process.
The session bead persists and can be resumed later.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).

```
gc session suspend <session-id-or-alias>
```

## gc session unpin

Remove only the durable pin override from a session.

Unpinning does not force an immediate stop. The reconciler will apply the
normal wake/sleep rules on its next pass.

```
gc session unpin <session-id-or-alias>
```

## gc session wait

Register a dependency wait for a session

```
gc session wait [session-id-or-alias] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--any` | bool |  | wake when any watched bead closes (default: all) |
| `--note` | string |  | reminder text delivered when the wait is satisfied |
| `--on-beads` | stringSlice |  | bead IDs to watch |
| `--sleep` | bool |  | set wait hold so the session can drain to sleep |

## gc session wake

Request wake for a session and release user hold or crash-loop quarantine metadata.

After waking, the reconciler will start the session on its next tick
if it has wake reasons (e.g., a matching config agent). If the session
has no wake reasons, it remains asleep.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).

```
gc session wake <session-id-or-alias>
```

**Example:**

```
gc session wake gc-42
  gc session wake mayor
```

## gc shell

The shell integration adds a completion hook to your shell RC file that
provides tab-completion for gc commands and flags.

Subcommands: install, remove, status.

```
gc shell
```

| Subcommand | Description |
|------------|-------------|
| [gc shell install](#gc-shell-install) | Install or update shell integration |
| [gc shell remove](#gc-shell-remove) | Remove shell integration |
| [gc shell status](#gc-shell-status) | Show shell integration status |

## gc shell install

Install or update the gc shell completion hook.

If no shell is specified, the shell is detected from $SHELL.
The completion script is written to ~/.gc/completions/ and a source line
is added to your shell RC file.

```
gc shell install [bash|zsh|fish]
```

## gc shell remove

Remove the gc shell completion hook from your shell RC file and delete the completion script.

```
gc shell remove
```

## gc shell status

Show shell integration status

```
gc shell status
```

## gc skill

List skills visible to the current city.

Output includes:
  - City pack skills (skills/&lt;name&gt;/SKILL.md under the city root)
  - Imported pack shared skills (binding-qualified, e.g. ops.code-review)
  - Compatibility bootstrap skills, when legacy implicit imports still exist
  - With --agent/--session: that agent's agents/&lt;name&gt;/skills/ catalog

The listing is a diagnostic view of what's *available*. It does not
collapse precedence, filter to agents whose provider has a vendor
sink, or predict exactly which entries the materializer will pick on
name collision. For the materialized set, inspect the
&lt;scope-root&gt;/.&lt;vendor&gt;/skills/ sink after "gc start" or run
"gc doctor" to surface collisions.

```
gc skill
```

| Subcommand | Description |
|------------|-------------|
| [gc skill list](#gc-skill-list) | List visible skills |

## gc skill list

List the current shared and agent-local visible skills, optionally scoped to an agent or session.

```
gc skill list [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--agent` | string |  | show the effective skill view for this agent |
| `--session` | string |  | show the effective skill view for this session |

## gc sling

Route a bead to a session config or agent using the target's sling_query.

The target is an agent qualified name (e.g. "mayor" or "hello-world/polecat").
The second argument is a bead ID, a formula name when --formula is set, or
arbitrary text (which auto-creates a task bead).

When target is omitted, the bead's rig prefix is used to look up the rig's
default_sling_target from config. Requires --formula to have an explicit target.
Inline text also requires an explicit target.

With --formula, a wisp (ephemeral molecule) is instantiated from the formula
and its root bead is routed to the target.

Examples:
  gc sling my-rig/claude BL-42              # route existing bead
  gc sling my-rig/claude "write a README"   # create bead from text, then route
  gc sling mayor code-review --formula      # instantiate formula, route wisp
  echo "fix login" | gc sling mayor --stdin # read bead text from stdin

```
gc sling [target] <bead-or-formula-or-text> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-n`, `--dry-run` | bool |  | show what would be done without executing |
| `--force` | bool |  | suppress warnings, allow cross-rig routing, allow graph workflow replacement, and for direct bead routes dispatch even if the bead does not resolve in the local store |
| `-f`, `--formula` | bool |  | treat argument as formula name |
| `--merge` | string |  | merge strategy: direct, mr, or local |
| `--no-convoy` | bool |  | skip auto-convoy creation |
| `--no-formula` | bool |  | suppress default formula (route raw bead) |
| `--nudge` | bool |  | nudge target after routing |
| `--on` | string |  | attach wisp from formula to bead before routing |
| `--owned` | bool |  | mark auto-convoy as owned (skip auto-close) |
| `--scope-kind` | string |  | logical workflow scope kind for graph.v2 launches |
| `--scope-ref` | string |  | logical workflow scope ref for graph.v2 launches |
| `--stdin` | bool |  | read bead text from stdin (first line = title, rest = description) |
| `-t`, `--title` | string |  | wisp root bead title (with --formula or --on) |
| `--var` | stringArray |  | variable substitution for formula (key=value, repeatable) |

## gc start

Start the city under the machine-wide supervisor.

Requires an existing city bootstrapped by "gc init". Fetches remote
packs as needed, registers the city with the machine-wide supervisor,
ensures the supervisor is running, and triggers immediate reconciliation.
Use "gc supervisor run" for foreground operation.

```
gc start [path] [flags]
```

**Example:**

```
gc start
  gc start ~/my-city
  gc start --dry-run
  gc supervisor run
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-n`, `--dry-run` | bool |  | preview what agents would start without starting them |

## gc status

Shows a city-wide overview: controller state, suspension,
all agents with running status, rigs, and a summary count.

```
gc status [path] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | bool |  | Output in JSON format |

## gc stop

Stop all agent sessions in the city with graceful shutdown.

Sends interrupt signals to running agents, waits for the configured
shutdown timeout, then force-kills any remaining sessions. Also stops
the Dolt server and cleans up orphan sessions. If a controller is
running, delegates shutdown to it.

Use --timeout=DURATION to cap the wall-clock time gc stop will spend
before giving up; the default budgets configured session interrupt and
stop waves, the configured shutdown grace wait, and a second orphan
cleanup pass. Use --force to skip the interrupt grace period and go
straight to kill.

```
gc stop [path] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--force` | bool |  | skip the interrupt grace period and force-kill all sessions immediately |
| `--timeout` | duration | `0s` | wall-clock cap for the stop sequence (0 = derive from city config) |

## gc supervisor

Manage the machine-wide supervisor.

The supervisor manages all registered cities from a single process,
hosting a unified API server. Use "gc init", "gc start", or "gc register"
to add cities.

```
gc supervisor
```

| Subcommand | Description |
|------------|-------------|
| [gc supervisor install](#gc-supervisor-install) | Install the supervisor as a platform service |
| [gc supervisor logs](#gc-supervisor-logs) | Tail the supervisor log file |
| [gc supervisor reload](#gc-supervisor-reload) | Trigger immediate reconciliation of all cities |
| [gc supervisor run](#gc-supervisor-run) | Run the machine-wide supervisor in the foreground |
| [gc supervisor start](#gc-supervisor-start) | Start the machine-wide supervisor in the background |
| [gc supervisor status](#gc-supervisor-status) | Check if the supervisor is running |
| [gc supervisor stop](#gc-supervisor-stop) | Stop the machine-wide supervisor |
| [gc supervisor uninstall](#gc-supervisor-uninstall) | Remove the platform service |

## gc supervisor install

Install the machine-wide supervisor as a platform service that
starts on login.

```
gc supervisor install
```

## gc supervisor logs

Tail the machine-wide supervisor log file.

Shows recent log output from background and service-managed supervisor runs.

```
gc supervisor logs [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-f`, `--follow` | bool |  | follow log output |
| `-n`, `--lines` | int | `50` | number of lines to show |

## gc supervisor reload

Send a reload signal to the running supervisor, causing it to
immediately re-read the registry and reconcile all cities. Use this
after killing a child process to force the supervisor to detect the
change and restart it without waiting for the next patrol tick.

```
gc supervisor reload
```

## gc supervisor run

Run the machine-wide supervisor in the foreground.

This is the canonical long-running control loop. It reads ~/.gc/cities.toml
for registered cities, manages them from one process, and hosts the shared
API server.

```
gc supervisor run
```

## gc supervisor start

Start the machine-wide supervisor in the background.

This forks "gc supervisor run", verifies it became ready, and returns.

```
gc supervisor start
```

## gc supervisor status

Check if the supervisor is running

```
gc supervisor status
```

## gc supervisor stop

Stop the running machine-wide supervisor and all its cities.

By default, returns as soon as the supervisor acknowledges the stop
request — shutdown continues asynchronously. Pass --wait to block
until the supervisor socket is no longer answering, which is what
most callers that need deterministic cleanup want (e.g., integration
tests that then expect to remove temp directories without racing
against lingering supervisor / controller subprocesses).

```
gc supervisor stop [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--wait` | bool |  | Wait for the supervisor to finish stopping all managed cities and release its socket before returning |
| `--wait-timeout` | duration | `30s` | Maximum time to wait when --wait is set |

## gc supervisor uninstall

Remove the platform service and stop the machine-wide supervisor.

On systemd, uninstall refuses to remove an active unit when the supervisor
control socket is unavailable. Start the supervisor first so it can re-adopt
preserved sessions, then retry uninstall.

```
gc supervisor uninstall
```

## gc suspend

Suspends the city by setting workspace.suspended = true in city.toml.

This inherits downward — when the city is suspended, all agents are
effectively suspended regardless of their individual suspended fields.
The reconciler won't spawn agents, gc hook/prime return empty.

Use "gc resume" to restore.

```
gc suspend [path]
```

## gc trace

Inspect and control the session reconciler trace stream.

Trace state is persisted locally under .gc/runtime/session-reconciler-trace
and can be managed even when the controller is offline.

```
gc trace
```

| Subcommand | Description |
|------------|-------------|
| [gc trace cycle](#gc-trace-cycle) | Show a cycle by tick id |
| [gc trace reasons](#gc-trace-reasons) | Show reason codes observed in trace records |
| [gc trace show](#gc-trace-show) | Show trace records |
| [gc trace start](#gc-trace-start) | Start or extend tracing for a template |
| [gc trace status](#gc-trace-status) | Show trace arms and stream state |
| [gc trace stop](#gc-trace-stop) | Stop tracing for a template |
| [gc trace tail](#gc-trace-tail) | Follow trace records |

## gc trace cycle

Show a cycle by tick id

```
gc trace cycle [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--tick` | string |  | tick id to display |

## gc trace reasons

Show reason codes observed in trace records

```
gc trace reasons [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--since` | string |  | show reasons since duration ago |
| `--template` | string |  | exact normalized template selector |

## gc trace show

Show trace records

```
gc trace show [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--json` | bool | `true` | emit JSON array |
| `--reason` | string |  | filter by reason code |
| `--since` | string |  | show records since duration ago |
| `--template` | string |  | exact normalized template selector |
| `--tick` | string |  | filter by tick id |
| `--trace-id` | string |  | filter by trace id |
| `--type` | string |  | filter by record type |

## gc trace start

Start or extend tracing for a template

```
gc trace start [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--auto` | bool |  | mark the arm as auto-triggered |
| `--for` | string | `15m` | trace arm duration (e.g. 15m) |
| `--level` | string | `detail` | trace level: baseline or detail |
| `--template` | string |  | exact normalized template selector |

## gc trace status

Show trace arms and stream state

```
gc trace status
```

## gc trace stop

Stop tracing for a template

```
gc trace stop [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--all` | bool |  | remove both manual and auto arms |
| `--template` | string |  | exact normalized template selector |

## gc trace tail

Follow trace records

```
gc trace tail [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--since` | string |  | follow from duration ago |
| `--template` | string |  | exact normalized template selector |

## gc unregister

Remove a city from the machine-wide supervisor registry.

If no path is given, unregisters the current city (discovered from cwd).
If the supervisor is running, it immediately stops managing the city.

```
gc unregister [path]
```

## gc version

Print the gc version string.

Use --long to include git commit and build date metadata.

```
gc version [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-l`, `--long` | bool |  | Include git commit and build date metadata |

## gc wait

Inspect and manage durable session waits

```
gc wait
```

| Subcommand | Description |
|------------|-------------|
| [gc wait cancel](#gc-wait-cancel) | Cancel a wait |
| [gc wait inspect](#gc-wait-inspect) | Show details for a wait |
| [gc wait list](#gc-wait-list) | List durable waits |
| [gc wait ready](#gc-wait-ready) | Manually mark a wait ready |

## gc wait cancel

Cancel a wait

```
gc wait cancel <wait-id>
```

## gc wait inspect

Show details for a wait

```
gc wait inspect <wait-id>
```

## gc wait list

List durable waits

```
gc wait list [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--session` | string |  | filter by session ID |
| `--state` | string |  | filter by wait state |

## gc wait ready

Manually mark a wait ready

```
gc wait ready <wait-id>
```
