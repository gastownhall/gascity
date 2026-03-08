# CLI Reference

> **Auto-generated** — do not edit. Run `go run ./cmd/genschema` to regenerate.

## Global Flags

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--city` | string |  | path to the city directory (default: walk up from cwd) |

## gc

Gas City CLI — orchestration-builder for multi-agent workflows

```
gc [flags]
```

| Subcommand | Description |
|------------|-------------|
| [gc agent](#gc-agent) | Manage agents |
| [gc automation](#gc-automation) | Manage automations (periodic formula dispatch) |
| [gc beads](#gc-beads) | Manage the beads provider |
| [gc build-image](#gc-build-image) | Build a prebaked agent container image |
| [gc cities](#gc-cities) | List registered cities |
| [gc config](#gc-config) | Inspect and validate city configuration |
| [gc converge](#gc-converge) | Manage convergence loops (bounded iterative refinement) |
| [gc convoy](#gc-convoy) | Manage convoys (batch work tracking) |
| [gc daemon](#gc-daemon) | Manage the city daemon (background controller) |
| [gc dashboard](#gc-dashboard) | Web dashboard for monitoring the city |
| [gc doctor](#gc-doctor) | Check workspace health |
| [gc event](#gc-event) | Event operations |
| [gc events](#gc-events) | Show the event log |
| [gc graph](#gc-graph) | Show dependency graph for beads |
| [gc handoff](#gc-handoff) | Send handoff mail and restart agent session |
| [gc help](#gc-help) | Help about any command |
| [gc hook](#gc-hook) | Check for available work (use --inject for Stop hook output) |
| [gc init](#gc-init) | Initialize a new city |
| [gc mail](#gc-mail) | Send and receive messages between agents and humans |
| [gc migration](#gc-migration) | Migration tools for the unified session model |
| [gc pack](#gc-pack) | Manage remote pack sources |
| [gc prime](#gc-prime) | Output the behavioral prompt for an agent |
| [gc register](#gc-register) | Register a city with the machine-wide supervisor |
| [gc restart](#gc-restart) | Restart all agent sessions in the city |
| [gc resume](#gc-resume) | Resume a suspended city |
| [gc rig](#gc-rig) | Manage rigs (projects) |
| [gc session](#gc-session) | Manage interactive chat sessions |
| [gc skill](#gc-skill) | Show command reference for a topic |
| [gc sling](#gc-sling) | Route work to an agent or pool |
| [gc start](#gc-start) | Start the city (auto-initializes if needed) |
| [gc status](#gc-status) | Show city-wide status overview |
| [gc stop](#gc-stop) | Stop all agent sessions in the city |
| [gc supervisor](#gc-supervisor) | Manage the machine-wide supervisor |
| [gc suspend](#gc-suspend) | Suspend the city (all agents effectively suspended) |
| [gc unregister](#gc-unregister) | Remove a city from the machine-wide supervisor |
| [gc version](#gc-version) | Print gc version information |

## gc agent

Manage agents in the city workspace.

Agents are the autonomous workers that execute tasks. Each agent runs
in its own tmux session with a configured provider (Claude, Codex, etc).
Agents can be fixed (single instance) or pooled (multiple instances
scaled by demand).

```
gc agent
```

| Subcommand | Description |
|------------|-------------|
| [gc agent add](#gc-agent-add) | Add an agent to the workspace |
| [gc agent attach](#gc-agent-attach) | Removed, use `gc session attach` |
| [gc agent destroy](#gc-agent-destroy) | Removed, use `gc session close` |
| [gc agent drain](#gc-agent-drain) | Removed, use `gc runtime drain` |
| [gc agent drain-ack](#gc-agent-drain-ack) | Removed, use `gc runtime drain-ack` |
| [gc agent drain-check](#gc-agent-drain-check) | Removed, use `gc runtime drain-check` |
| [gc agent kill](#gc-agent-kill) | Removed, use `gc session kill` |
| [gc agent list](#gc-agent-list) | Removed, use `gc session list` |
| [gc agent logs](#gc-agent-logs) | Removed, use `gc session logs` |
| [gc agent nudge](#gc-agent-nudge) | Removed, use `gc session wake` |
| [gc agent peek](#gc-agent-peek) | Removed, use `gc session peek` |
| [gc agent request-restart](#gc-agent-request-restart) | Removed, use `gc runtime request-restart` |
| [gc agent resume](#gc-agent-resume) | Resume a suspended agent |
| [gc agent start](#gc-agent-start) | Removed, use `gc session new` |
| [gc agent status](#gc-agent-status) | Removed, use `gc session list` |
| [gc agent stop](#gc-agent-stop) | Removed, use `gc session suspend` |
| [gc agent suspend](#gc-agent-suspend) | Suspend an agent (reconciler will skip it) |
| [gc agent undrain](#gc-agent-undrain) | Removed, use `gc runtime undrain` |

## gc agent add

Add a new agent to the workspace configuration.

Appends an [[agents]] block to city.toml. The agent will be started
on the next "gc start" or controller reconcile tick. Use --dir to
scope the agent to a rig's working directory.

```
gc agent add --name <name> [flags]
```

**Example:**

```
gc agent add --name mayor
  gc agent add --name polecat --dir my-project
  gc agent add --name worker --prompt-template prompts/worker.md --suspended
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--dir` | string |  | Working directory for the agent (relative to city root) |
| `--name` | string |  | Name of the agent |
| `--prompt-template` | string |  | Path to prompt template file (relative to city root) |
| `--suspended` | bool |  | Register the agent in suspended state |

## gc agent attach

Removed, use `gc session attach`.

```
gc session attach <name>
```

## gc agent destroy

Removed, use `gc session close`.

```
gc session close <session-id-or-name>
```

## gc agent drain

Removed, use `gc runtime drain`.

```
gc runtime drain <name>
```

## gc agent drain-ack

Removed, use `gc runtime drain-ack`.

```
gc runtime drain-ack [name]
```

## gc agent drain-check

Removed, use `gc runtime drain-check`.

```
gc runtime drain-check [name]
```

## gc agent kill

Removed, use `gc session kill`.

```
gc session kill <name>
```

## gc agent list

Removed, use `gc session list`.

```
gc session list [flags]
```

## gc agent logs

Removed, use `gc session logs`.

```
gc session logs <agent-name> [flags]
```

## gc agent nudge

Removed, use `gc session wake`.

```
gc session message <agent-name> <message>
```

## gc agent peek

Removed, use `gc session peek`.

```
gc session peek <agent-name> [flags]
```

## gc agent request-restart

Removed, use `gc runtime request-restart`.

```
gc runtime request-restart
```

## gc agent resume

Resume a suspended agent by clearing suspended in city.toml.

The reconciler will start the agent on its next tick. Supports bare
names (resolved via rig context) and qualified names (e.g. "myrig/worker").

```
gc agent resume <name>
```

## gc agent start

Removed, use `gc session new`.

```
gc session new <template> [flags]
```

## gc agent status

Removed, use `gc session list`.

```
gc session list
```

## gc agent stop

Removed, use `gc session suspend`.

```
gc session suspend <session-id-or-name>
```

## gc agent suspend

Suspend an agent by setting suspended=true in city.toml.

Suspended agents are skipped by the reconciler — their sessions are not
started or restarted. Existing sessions continue running but won't be
replaced if they exit. Use "gc agent resume" to restore.

```
gc agent suspend <name>
```

## gc agent undrain

Removed, use `gc runtime undrain`.

```
gc runtime undrain <name>
```

## gc automation

Manage automations — formulas with gate conditions for periodic dispatch.

Automations are formulas annotated with scheduling gates (interval, cron
schedule, or shell check commands). The controller evaluates gates
periodically and dispatches automation formulas when they are due.

```
gc automation
```

| Subcommand | Description |
|------------|-------------|
| [gc automation check](#gc-automation-check) | Check which automations are due to run |
| [gc automation history](#gc-automation-history) | Show automation execution history |
| [gc automation list](#gc-automation-list) | List available automations |
| [gc automation run](#gc-automation-run) | Execute an automation manually |
| [gc automation show](#gc-automation-show) | Show details of an automation |

## gc automation check

Evaluate gate conditions for all automations and show which are due.

Prints a table with each automation's gate, due status, and reason. Returns
exit code 0 if any automation is due, 1 if none are due.

```
gc automation check
```

## gc automation history

Show execution history for automations.

Queries bead history for past automation runs. Optionally filter by automation
name. Use --rig to filter by rig.

```
gc automation history [name] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--rig` | string |  | rig name to filter automation history |

## gc automation list

List all available automations with their gate type, schedule, and target pool.

Scans formula layers for formulas that have automation metadata
(gate, interval, schedule, check, pool).

```
gc automation list
```

## gc automation run

Execute an automation manually, bypassing its gate conditions.

Instantiates a wisp from the automation's formula and routes it to the
target pool (if configured). Useful for testing automations or triggering
them outside their normal schedule.
Use --rig to disambiguate same-name automations in different rigs.

```
gc automation run <name> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--rig` | string |  | rig name to disambiguate same-name automations |

## gc automation show

Display detailed information about a named automation.

Shows the automation name, description, formula reference, gate type,
scheduling parameters, check command, target pool, and source file.
Use --rig to disambiguate same-name automations in different rigs.

```
gc automation show <name> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--rig` | string |  | rig name to disambiguate same-name automations |

## gc beads

Manage the beads provider (backing store for issue tracking).

Subcommands for health checking and diagnostics.

```
gc beads
```

| Subcommand | Description |
|------------|-------------|
| [gc beads health](#gc-beads-health) | Check beads provider health |

## gc beads health

Check beads provider health and attempt recovery on failure.

Delegates to the provider's lifecycle health operation. For exec
providers (including bd/dolt), the script handles multi-tier checking
and recovery internally. For the file provider, always succeeds (no-op).

Also used by the beads-health system automation for periodic monitoring.

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
| [gc config explain](#gc-config-explain) | Show resolved agent config with provenance annotations |
| [gc config show](#gc-config-show) | Dump the resolved city configuration as TOML |

## gc config explain

Show the resolved configuration for each agent with provenance.

Displays every resolved field with an annotation showing which config
file provided the value. Use --rig and --agent to filter the output.
Useful for debugging config composition and understanding override
resolution.

```
gc config explain [flags]
```

**Example:**

```
gc config explain
  gc config explain --agent mayor
  gc config explain --rig my-project
  gc config explain -f overlay.toml --agent polecat
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--agent` | string |  | filter to a specific agent name |
| `-f`, `--file` | stringArray |  | additional config files to layer (can be repeated) |
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
| `--gate-timeout` | string | `30s` | Gate execution timeout |
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

Manage convoys — batch work tracking containers.

A convoy is a bead that groups related issues. Issues are linked to a
convoy via parent-child relationships. Convoys track completion progress
and can be auto-closed when all their issues are resolved.

```
gc convoy
```

| Subcommand | Description |
|------------|-------------|
| [gc convoy add](#gc-convoy-add) | Add an issue to a convoy |
| [gc convoy check](#gc-convoy-check) | Auto-close convoys where all issues are closed |
| [gc convoy close](#gc-convoy-close) | Close a convoy |
| [gc convoy create](#gc-convoy-create) | Create a convoy and optionally track issues |
| [gc convoy land](#gc-convoy-land) | Land an owned convoy (terminate + cleanup) |
| [gc convoy list](#gc-convoy-list) | List open convoys with progress |
| [gc convoy status](#gc-convoy-status) | Show detailed convoy status |
| [gc convoy stranded](#gc-convoy-stranded) | Find convoys with ready work but no workers |

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
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--merge` | string |  | merge strategy: direct, mr, local |
| `--notify` | string |  | notification target on completion |
| `--owner` | string |  | convoy owner (who manages it) |

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

## gc daemon

Manage the city daemon — a persistent background controller.

The daemon runs "gc start --foreground" as a background process,
continuously reconciling agent state. It can be managed as a system
service via launchd (macOS) or systemd (Linux).

```
gc daemon
```

| Subcommand | Description |
|------------|-------------|
| [gc daemon install](#gc-daemon-install) | Install the daemon as a platform service (launchd/systemd) |
| [gc daemon logs](#gc-daemon-logs) | Tail the daemon log file |
| [gc daemon run](#gc-daemon-run) | Run the controller in the foreground (with log file) |
| [gc daemon start](#gc-daemon-start) | Start the daemon in the background |
| [gc daemon status](#gc-daemon-status) | Show daemon status (PID, uptime) |
| [gc daemon stop](#gc-daemon-stop) | Stop the running daemon |
| [gc daemon uninstall](#gc-daemon-uninstall) | Remove the platform service (launchd/systemd) |

## gc daemon install

Install the daemon as a platform service that starts on login.

Generates and loads a launchd plist (macOS) or systemd user unit
(Linux) that runs "gc daemon run" automatically.

```
gc daemon install [path]
```

## gc daemon logs

Tail the daemon log file (.gc/daemon.log).

Shows recent log output with optional follow mode. Equivalent to
"tail -n 50 .gc/daemon.log" (or "tail -f" with --follow).

```
gc daemon logs [path] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-f`, `--follow` | bool |  | follow log output |
| `-n`, `--lines` | int | `50` | number of lines to show |

## gc daemon run

Run the controller in the foreground with log file output.

Starts the persistent reconciliation loop, writing output to both
stdout and .gc/daemon.log. This is the command that "gc daemon start"
forks in the background.

```
gc daemon run [path] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-f`, `--file` | stringArray |  | additional config files to layer (can be repeated) |
| `--no-strict` | bool |  | disable strict config collision checking (strict is on by default) |

## gc daemon start

Fork the daemon as a background process.

Spawns "gc daemon run" as a detached child process and verifies it
acquired the controller lock. Only one daemon can run per city.

```
gc daemon start [path]
```

**Example:**

```
gc daemon start
  gc daemon start ~/my-city
```

## gc daemon status

Show whether the daemon is running, its PID, and uptime.

Reads the PID file and verifies the process is alive. Derives uptime
from the most recent controller.started event in the event log.

```
gc daemon status [path]
```

## gc daemon stop

Signal the running daemon to shut down gracefully.

Connects to the controller's unix socket and sends a stop command.
The daemon performs graceful agent shutdown before exiting.

```
gc daemon stop [path]
```

## gc daemon uninstall

Remove the platform service and stop the daemon.

Unloads and deletes the launchd plist (macOS) or systemd unit (Linux)
created by "gc daemon install".

```
gc daemon uninstall [path]
```

## gc dashboard

Web dashboard for monitoring the city

```
gc dashboard
```

| Subcommand | Description |
|------------|-------------|
| [gc dashboard serve](#gc-dashboard-serve) | Start the web dashboard |

## gc dashboard serve

Start the web dashboard

```
gc dashboard serve [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--api` | string |  | GC API server URL (e.g. http://127.0.0.1:8080) |
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
| `--actor` | string |  | Actor name (default: GC_AGENT or "human") |
| `--message` | string |  | Event message |
| `--payload` | string |  | JSON payload to attach to the event |
| `--subject` | string |  | Event subject (e.g. bead ID) |

## gc events

Show the city event log with optional filtering.

Events are recorded to .gc/events.jsonl by the controller, agent
lifecycle operations, and bead mutations. Use --type and --since to
filter. Use --watch to block until matching events arrive (useful for
scripting and automation).

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
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--after` | uint64 |  | Resume watching from this sequence number (0 = current head) |
| `--follow` | bool |  | Continuously stream events as they arrive |
| `--json` | bool |  | Output in JSON format (list mode only) |
| `--payload-match` | stringArray |  | Filter by payload field (key=value, repeatable) |
| `--seq` | bool |  | Print the current head sequence number and exit |
| `--since` | string |  | Show events since duration ago (e.g. 1h, 30m) |
| `--timeout` | string | `30s` | Max wait duration for --watch (e.g. 30s, 5m) |
| `--type` | string |  | Filter by event type (e.g. bead.created) |
| `--watch` | bool |  | Block until matching events arrive (exits after first match) |

## gc graph

Show the dependency graph for a set of beads, a convoy, or an epic.

Resolves dependencies via the bead store and prints each bead with its
status and what blocks it. Convoys and epics are expanded to their
children automatically. Readiness is computed within the displayed set.

By default prints a table. Use --mermaid for a Mermaid.js flowchart
you can paste into Markdown.

```
gc graph <bead-ids|convoy-id|epic-id...> [flags]
```

**Example:**

```
gc graph gc-42               # expand convoy or epic children
  gc graph gc-1 gc-2 gc-3     # arbitrary beads
  gc graph gc-42 --mermaid     # Mermaid.js diagram
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--mermaid` | bool |  | output Mermaid.js flowchart |

## gc handoff

Convenience command for context handoff.

Self-handoff (default): sends mail to self and blocks until controller
restarts the session. Equivalent to:

  gc mail send $GC_AGENT <subject> [message]
  gc runtime request-restart

Remote handoff (--target): sends mail to target agent and kills its
session. The reconciler restarts it with the handoff mail waiting.
Returns immediately. Equivalent to:

  gc mail send <target> <subject> [message]
  gc session kill <target>

Self-handoff requires agent context (GC_AGENT/GC_CITY env vars).
Remote handoff can be run from any context with access to the city.

```
gc handoff <subject> [message] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--target` | string |  | Remote agent to handoff (sends mail + kills session) |

## gc help

Help provides help for any command in the application.
Simply type gc help [path to command] for full details.

```
gc help [command]
```

## gc hook

Checks for available work using the agent's work_query config.

Without --inject: prints raw output, exits 0 if work exists, 1 if empty.
With --inject: wraps output in <system-reminder> for hook injection, always exits 0.

The agent is determined from $GC_AGENT or a positional argument.

```
gc hook [agent] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--inject` | bool |  | output <system-reminder> block for hook injection |

## gc init

Create a new Gas City workspace in the given directory (or cwd).

Runs an interactive wizard to choose a config template and coding agent
provider. Creates the .gc/ runtime directory, default
prompts and formulas, and writes city.toml. Use --file to skip the
wizard and initialize from an existing TOML config file.

```
gc init [path] [flags]
```

**Example:**

```
gc init
  gc init ~/my-city
  gc init --file examples/gastown.toml ~/bright-lights
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--file` | string |  | path to a TOML file to use as city.toml |
| `--from` | string |  | path to an example city directory to copy |

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
| [gc mail archive](#gc-mail-archive) | Archive a message without reading it |
| [gc mail check](#gc-mail-check) | Check for unread mail (use --inject for hook output) |
| [gc mail count](#gc-mail-count) | Show total/unread message count |
| [gc mail delete](#gc-mail-delete) | Delete a message (closes the bead) |
| [gc mail inbox](#gc-mail-inbox) | List unread messages (defaults to your inbox) |
| [gc mail mark-read](#gc-mail-mark-read) | Mark a message as read |
| [gc mail mark-unread](#gc-mail-mark-unread) | Mark a message as unread |
| [gc mail peek](#gc-mail-peek) | Show a message without marking it as read |
| [gc mail read](#gc-mail-read) | Read a message and mark it as read |
| [gc mail reply](#gc-mail-reply) | Reply to a message |
| [gc mail send](#gc-mail-send) | Send a message to an agent or human |
| [gc mail thread](#gc-mail-thread) | List all messages in a thread |

## gc mail archive

Close a message bead without displaying its contents.

Use this to dismiss a message without reading it. The message is marked
as closed and will no longer appear in mail check or inbox results.

```
gc mail archive <id>
```

## gc mail check

Check for unread mail addressed to an agent.

Without --inject: prints the count and exits 0 if mail exists, 1 if
empty. With --inject: outputs a <system-reminder> block suitable for
hook injection (always exits 0). The recipient defaults to $GC_AGENT
or "human".

```
gc mail check [agent] [flags]
```

**Example:**

```
gc mail check
  gc mail check --inject
  gc mail check mayor
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--inject` | bool |  | output <system-reminder> block for hook injection |

## gc mail count

Show total and unread message counts for an agent or human.
The recipient defaults to $GC_AGENT or "human".

```
gc mail count [agent]
```

## gc mail delete

Delete a message by closing the bead. Same effect as archive but with different user intent.

```
gc mail delete <id>
```

## gc mail inbox

List all unread messages for an agent or human.

Shows message ID, sender, subject, and body in a table. The recipient defaults
to $GC_AGENT or "human". Pass an agent name to view another agent's inbox.

```
gc mail inbox [agent]
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

Send a message to an agent or human.

Creates a message bead addressed to the recipient. The sender defaults
to $GC_AGENT (in agent sessions) or "human". Use --notify to nudge
the recipient after sending. Use --from to override the sender identity.
Use --to as an alternative to the positional <to> argument.
Use -s/--subject for the summary line and -m/--message for the body text.
Use --all to broadcast to all agents (excluding sender and "human").

```
gc mail send [<to>] [<body>] [flags]
```

**Example:**

```
gc mail send mayor "Build is green"
  gc mail send mayor -s "Build is green"
  gc mail send mayor/ -s "ESCALATION: Auth broken" -m "Token refresh fails after 30min"
  gc mail send --to mayor "Build is green"
  gc mail send human "Review needed for PR #42"
  gc mail send polecat "Priority task" --notify
  gc mail send --all "Status update: tests passing"
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--all` | bool |  | broadcast to all agents (excludes sender and human) |
| `--from` | string |  | sender identity (default: $GC_AGENT or "human") |
| `-m`, `--message` | string |  | message body text |
| `--notify` | bool |  | nudge the recipient after sending |
| `-s`, `--subject` | string |  | message subject line |
| `--to` | string |  | recipient address (alternative to positional argument) |

## gc mail thread

Show all messages sharing a thread ID, ordered by time.

```
gc mail thread <thread-id>
```

## gc migration

Migration tools for the unified session model

```
gc migration
```

| Subcommand | Description |
|------------|-------------|
| [gc migration plan](#gc-migration-plan) | Show what the adoption barrier would do without making changes |

## gc migration plan

Dry-run the adoption barrier: list running sessions, check which ones
already have session beads, and report what beads would be created.

No changes are made. Run "gc start" to execute the actual adoption.

```
gc migration plan
```

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

If agent-name matches a configured agent with a prompt_template,
that template is output. Otherwise outputs a default worker prompt.

```
gc prime [agent-name]
```

## gc register

Register a city directory with the machine-wide supervisor.

If no path is given, registers the current city (discovered from cwd).
Registration is idempotent — registering the same city twice is a no-op.
City names (derived from directory basename or workspace.name) must be
unique across all registered cities.

```
gc register [path]
```

## gc restart

Restart the city by stopping all agents then starting them again.

Equivalent to running "gc stop" followed by "gc start". Performs a
full one-shot reconciliation after stopping, which re-reads city.toml
and starts all configured agents.

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
| [gc rig restart](#gc-rig-restart) | Restart all agents in a rig |
| [gc rig resume](#gc-rig-resume) | Resume a suspended rig |
| [gc rig status](#gc-rig-status) | Show rig status and agent running state |
| [gc rig suspend](#gc-rig-suspend) | Suspend a rig (reconciler will skip its agents) |

## gc rig add

Register an external project directory as a rig.

Initializes beads database, installs agent hooks if configured,
generates cross-rig routes, and appends the rig to city.toml.
If the target directory doesn't exist, it is created. Use --include
to apply a pack directory that defines the rig's agent configuration.

Use --start-suspended to add the rig in a suspended state (dormant-by-default).
The rig's agents won't spawn until explicitly resumed with "gc rig resume".

```
gc rig add <path> [flags]
```

**Example:**

```
gc rig add /path/to/project
  gc rig add ./my-project --include packs/gastown
  gc rig add ./my-project --include packs/gastown --start-suspended
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--include` | string |  | pack directory for rig agents |
| `--start-suspended` | bool |  | add rig in suspended state (dormant-by-default) |

## gc rig list

List all registered rigs with their paths, prefixes, and beads status.

Shows the HQ rig (the city itself) and all configured rigs. Each rig
displays its bead ID prefix and whether its beads database is initialized.

```
gc rig list
```

## gc rig restart

Kill all agent sessions belonging to a rig.

The reconciler will restart the agents on its next tick. This is a
quick way to force-refresh all agents working on a particular project.

```
gc rig restart <name>
```

## gc rig resume

Resume a suspended rig by clearing suspended in city.toml.

The reconciler will start the rig's agents on its next tick.

```
gc rig resume <name>
```

## gc rig status

Show rig status and agent running state

```
gc rig status <name>
```

## gc rig suspend

Suspend a rig by setting suspended=true in city.toml.

All agents scoped to the suspended rig are effectively suspended —
the reconciler skips them and gc hook returns empty. The rig's beads
database remains accessible. Use "gc rig resume" to restore.

```
gc rig suspend <name>
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
| [gc session list](#gc-session-list) | List chat sessions |
| [gc session new](#gc-session-new) | Create a new chat session from an agent template |
| [gc session peek](#gc-session-peek) | View session output without attaching |
| [gc session prune](#gc-session-prune) | Close old suspended sessions |
| [gc session rename](#gc-session-rename) | Rename a session |
| [gc session suspend](#gc-session-suspend) | Suspend a session (save state, free resources) |
| [gc session wake](#gc-session-wake) | Wake a session (clear hold and quarantine) |

## gc session attach

Attach to a running session or resume a suspended one.

If the session is active with a live tmux session, reattaches.
If the session is suspended or the tmux session died, resumes
using the provider's resume mechanism (if supported) or restarts.

Accepts a session ID (e.g., gc-42) or template name (e.g., overseer).

```
gc session attach <session-id-or-name>
```

## gc session close

End a conversation. Stops the runtime if active and closes the bead.

Accepts a session ID (e.g., gc-42) or template name (e.g., overseer).

```
gc session close <session-id-or-name>
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

## gc session new

Create a new persistent conversation from an agent template defined in
city.toml. By default, attaches the terminal after creation.

```
gc session new <template> [flags]
```

**Example:**

```
gc session new helper
  gc session new helper --title "debugging auth"
  gc session new helper --no-attach
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--no-attach` | bool |  | create session without attaching |
| `--title` | string |  | human-readable session title |

## gc session peek

View session output without attaching

```
gc session peek <session-id-or-name> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--lines` | int | `50` | number of lines to capture |

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
gc session rename <session-id-or-name> <title>
```

## gc session suspend

Suspend an active session by stopping its runtime process.
The session bead persists and can be resumed later.

Accepts a session ID (e.g., gc-42) or template name (e.g., overseer).

```
gc session suspend <session-id-or-name>
```

## gc session wake

Release a user hold and/or crash-loop quarantine on a session.

After waking, the reconciler will start the session on its next tick
if it has wake reasons (e.g., a matching config agent). If the session
has no wake reasons, it remains asleep.

Accepts a session ID (e.g., gc-42) or template name (e.g., overseer).

```
gc session wake <session-id-or-name>
```

**Example:**

```
gc session wake gc-42
  gc session wake overseer
```

## gc skill

Show curated command reference for a Gas City topic.

Without arguments, lists available topics. With a topic name,
prints the full command reference for that topic.

```
gc skill [topic]
```

**Example:**

```
gc skill work       # beads command reference
  gc skill dispatch   # sling and formula reference
  gc skill            # list all topics
```

## gc sling

Route a bead to an agent or pool using the target's sling_query.

The target is an agent qualified name (e.g. "mayor" or "hello-world/polecat").
The second argument is a bead ID, or a formula name when --formula is set.

When target is omitted, the bead's rig prefix is used to look up the rig's
default_sling_target from config. Requires --formula to have an explicit target.

With --formula, a wisp (ephemeral molecule) is instantiated from the formula
and its root bead is routed to the target.

```
gc sling [target] <bead-or-formula> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-n`, `--dry-run` | bool |  | show what would be done without executing |
| `--force` | bool |  | suppress warnings and allow cross-rig routing |
| `-f`, `--formula` | bool |  | treat argument as formula name |
| `--merge` | string |  | merge strategy: direct, mr, or local |
| `--no-convoy` | bool |  | skip auto-convoy creation |
| `--no-formula` | bool |  | suppress default formula (route raw bead) |
| `--nudge` | bool |  | nudge target after routing |
| `--on` | string |  | attach wisp from formula to bead before routing |
| `--owned` | bool |  | mark auto-convoy as owned (skip auto-close) |
| `-t`, `--title` | string |  | wisp root bead title (with --formula or --on) |
| `--var` | stringArray |  | variable substitution for formula (key=value, repeatable) |

## gc start

Start the city by launching all configured agent sessions.

Auto-initializes the city if no .gc/ directory exists. Fetches remote
packs, resolves providers, installs hooks, and starts agent sessions
via one-shot reconciliation. Use --foreground for a persistent controller
that continuously reconciles agent state.

```
gc start [path] [flags]
```

**Example:**

```
gc start
  gc start ~/my-city
  gc start --foreground
  gc start -f overlay.toml --no-strict
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-n`, `--dry-run` | bool |  | preview what agents would start without starting them |
| `-f`, `--file` | stringArray |  | additional config files to layer (can be repeated) |
| `--foreground` | bool |  | run as a persistent controller (reconcile loop) |
| `--no-strict` | bool |  | disable strict config collision checking (strict is on by default) |

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

```
gc stop [path]
```

## gc supervisor

Manage the machine-wide supervisor daemon.

The supervisor manages all registered cities from a single process,
hosting a unified API server. Use "gc register" to add cities.

```
gc supervisor
```

| Subcommand | Description |
|------------|-------------|
| [gc supervisor start](#gc-supervisor-start) | Start the machine-wide supervisor (foreground) |
| [gc supervisor status](#gc-supervisor-status) | Check if the supervisor is running |
| [gc supervisor stop](#gc-supervisor-stop) | Stop the machine-wide supervisor |

## gc supervisor start

Start the machine-wide supervisor in the foreground.

The supervisor reads ~/.gc/cities.toml for registered cities and
~/.gc/supervisor.toml for configuration. It starts a CityRuntime
for each registered city and hosts a single API server.

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

```
gc supervisor stop
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

## gc unregister

Remove a city from the machine-wide supervisor registry.

If no path is given, unregisters the current city (discovered from cwd).

```
gc unregister [path]
```

## gc version

Print gc version, git commit, and build date.

Version information is injected via ldflags at build time.
When built with go install, VCS metadata is read from the binary.

```
gc version
```

