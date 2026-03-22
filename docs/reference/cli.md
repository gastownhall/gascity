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
| [gc agent](#gc-agent) | Manage agent configuration |
| [gc beads](#gc-beads) | Manage the beads provider |
| [gc build-image](#gc-build-image) | Build a prebaked agent container image |
| [gc cities](#gc-cities) | List registered cities |
| [gc config](#gc-config) | Inspect and validate city configuration |
| [gc converge](#gc-converge) | Manage convergence loops (bounded iterative refinement) |
| [gc convoy](#gc-convoy) | Manage convoys (batch work tracking) |
| [gc dashboard](#gc-dashboard) | Web dashboard for monitoring the city |
| [gc doctor](#gc-doctor) | Check workspace health |
| [gc dolt](#gc-dolt) | Commands from the dolt pack |
| [gc event](#gc-event) | Event operations |
| [gc events](#gc-events) | Show the event log |
| [gc formula](#gc-formula) | Manage and inspect formulas |
| [gc gastown](#gc-gastown) | Commands from the gastown pack |
| [gc graph](#gc-graph) | Show dependency graph for beads |
| [gc handoff](#gc-handoff) | Send handoff mail and restart this session |
| [gc help](#gc-help) | Help about any command |
| [gc hook](#gc-hook) | Check for available work (use --inject for Stop hook output) |
| [gc init](#gc-init) | Initialize a new city |
| [gc mail](#gc-mail) | Send and receive messages between agents and humans |
| [gc nudge](#gc-nudge) | Inspect and deliver deferred nudges |
| [gc order](#gc-order) | Manage orders (periodic formula dispatch) |
| [gc pack](#gc-pack) | Manage remote pack sources |
| [gc prime](#gc-prime) | Output the behavioral prompt for an agent |
| [gc register](#gc-register) | Register a city with the machine-wide supervisor |
| [gc restart](#gc-restart) | Restart all agent sessions in the city |
| [gc resume](#gc-resume) | Resume a suspended city |
| [gc rig](#gc-rig) | Manage rigs (projects) |
| [gc runtime](#gc-runtime) | Process-intrinsic runtime operations |
| [gc service](#gc-service) | Inspect workspace services |
| [gc session](#gc-session) | Manage interactive chat sessions |
| [gc skill](#gc-skill) | Show command reference for a topic |
| [gc sling](#gc-sling) | Route work to an agent or pool |
| [gc start](#gc-start) | Start the city under the machine-wide supervisor |
| [gc status](#gc-status) | Show city-wide status overview |
| [gc stop](#gc-stop) | Stop all agent sessions in the city |
| [gc supervisor](#gc-supervisor) | Manage the machine-wide supervisor |
| [gc suspend](#gc-suspend) | Suspend the city (all agents effectively suspended) |
| [gc unregister](#gc-unregister) | Remove a city from the machine-wide supervisor |
| [gc version](#gc-version) | Print gc version |
| [gc wait](#gc-wait) | Inspect and manage durable session waits |
| [gc workflow](#gc-workflow) | Run explicit graph-first workflow control beads |

## gc agent

Manage agent configuration in city.toml.

Runtime operations (attach, list, peek, nudge, kill, start, stop, destroy)
have moved to "gc session" and "gc runtime".

```
gc agent
```

| Subcommand | Description |
|------------|-------------|
| [gc agent add](#gc-agent-add) | Add an agent to the workspace |
| [gc agent resume](#gc-agent-resume) | Resume a suspended agent |
| [gc agent suspend](#gc-agent-suspend) | Suspend an agent (reconciler will skip it) |

## gc agent add

Add a new agent to the workspace configuration.

Appends an [[agent]] block to city.toml. The agent will be started
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

## gc agent resume

Resume a suspended agent by clearing suspended in city.toml.

The reconciler will start the agent on its next tick. Supports bare
names (resolved via rig context) and qualified names (e.g. "myrig/worker").

```
gc agent resume <name>
```

## gc agent suspend

Suspend an agent by setting suspended=true in city.toml.

Suspended agents are skipped by the reconciler — their sessions are not
started or restarted. Existing sessions continue running but won't be
replaced if they exit. Use "gc agent resume" to restore.

```
gc agent suspend <name>
```

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

## gc convoy target

Set the target branch metadata on a convoy.

Child work beads can inherit this target branch when slung with
feature-branch formulas such as mol-polecat-work.

```
gc convoy target <convoy-id> <branch>
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
| `--api` | string |  | GC API server URL (e.g. standalone http://127.0.0.1:9443, supervisor http://127.0.0.1:8372) |
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

## gc dolt

Commands from the dolt pack

```
gc dolt
```

| Subcommand | Description |
|------------|-------------|
| [gc dolt cleanup](#gc-dolt-cleanup) | Find and remove orphaned Dolt databases |
| [gc dolt health](#gc-dolt-health) | Check Dolt data-plane health |
| [gc dolt list](#gc-dolt-list) | List Dolt databases |
| [gc dolt logs](#gc-dolt-logs) | Tail the Dolt server log file |
| [gc dolt recover](#gc-dolt-recover) | Recover Dolt from read-only state |
| [gc dolt rollback](#gc-dolt-rollback) | List or restore from migration backups |
| [gc dolt sql](#gc-dolt-sql) | Open an interactive Dolt SQL shell |
| [gc dolt start](#gc-dolt-start) | Start the Dolt server if not already running |
| [gc dolt status](#gc-dolt-status) | Check if the Dolt server is running |
| [gc dolt sync](#gc-dolt-sync) | Push databases to configured remotes |

## gc dolt cleanup

Find and remove orphaned Dolt databases

```
gc dolt cleanup
```

## gc dolt health

Check Dolt data-plane health

```
gc dolt health
```

## gc dolt list

List Dolt databases

```
gc dolt list
```

## gc dolt logs

Tail the Dolt server log file

```
gc dolt logs
```

## gc dolt recover

Recover Dolt from read-only state

```
gc dolt recover
```

## gc dolt rollback

List or restore from migration backups

```
gc dolt rollback
```

## gc dolt sql

Open an interactive Dolt SQL shell

```
gc dolt sql
```

## gc dolt start

Start the Dolt server if not already running

```
gc dolt start
```

## gc dolt status

Check if the Dolt server is running

```
gc dolt status
```

## gc dolt sync

Push databases to configured remotes

```
gc dolt sync
```

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

```
gc formula cook <formula-name> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
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

## gc gastown

Commands from the gastown pack

```
gc gastown
```

| Subcommand | Description |
|------------|-------------|
| [gc gastown status](#gc-gastown-status) | Show gastown orchestration overview |

## gc gastown status

Show a high-level overview of the gastown orchestration state.

Displays agent sessions, active molecules, and recent events.
Useful for quick health checks during development.

Environment variables set by gc:
  GC_CITY_PATH   Absolute path to the city root
  GC_PACK_DIR    Absolute path to this pack's directory
  GC_PACK_NAME   Pack name ("gastown")
  GC_CITY_NAME   City workspace name

```
gc gastown status
```

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

Self-handoff (default): sends mail to self and blocks until controller
restarts the session. Equivalent to:

  gc mail send $GC_ALIAS &lt;subject&gt; [message]
  gc runtime request-restart

Remote handoff (--target): sends mail to a target session and kills its
session. The reconciler restarts it with the handoff mail waiting.
Returns immediately. Equivalent to:

  gc mail send &lt;target&gt; &lt;subject&gt; [message]
  gc session kill &lt;target&gt;

Self-handoff requires session context (GC_ALIAS or GC_SESSION_ID, plus
GC_SESSION_NAME and GC_CITY). Remote handoff accepts a session alias or ID.

```
gc handoff <subject> [message] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--target` | string |  | Remote session alias or ID to handoff (sends mail + kills session) |

## gc help

Help provides help for any command in the application.
Simply type gc help [path to command] for full details.

```
gc help [command]
```

## gc hook

Checks for available work using the agent's work_query config.

Without --inject: prints raw output, exits 0 if work exists, 1 if empty.
With --inject: wraps output in &lt;system-reminder&gt; for hook injection, always exits 0.

The agent is determined from $GC_AGENT or a positional argument.

```
gc hook [agent] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--inject` | bool |  | output &lt;system-reminder&gt; block for hook injection |

## gc init

Create a new Gas City workspace in the given directory (or cwd).

Runs an interactive wizard to choose a config template and coding agent
provider. Creates the .gc/ runtime directory, default
prompts and formulas, and writes city.toml. Use --provider to create the
default mayor city non-interactively, or --file to initialize from an
existing TOML config file.

```
gc init [path] [flags]
```

**Example:**

```
gc init
  gc init ~/my-city
  gc init --provider codex ~/my-city
  gc init --provider codex --bootstrap-profile k8s-cell /city
  gc init --file examples/gastown.toml ~/bright-lights
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--bootstrap-profile` | string |  | bootstrap profile to apply for hosted/container defaults |
| `--file` | string |  | path to a TOML file to use as city.toml |
| `--from` | string |  | path to an example city directory to copy |
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
| [gc mail send](#gc-mail-send) | Send a message to a session alias or human |
| [gc mail thread](#gc-mail-thread) | List all messages in a thread |

## gc mail archive

Close a message bead without displaying its contents.

Use this to dismiss a message without reading it. The message is marked
as closed and will no longer appear in mail check or inbox results.

```
gc mail archive <id>
```

## gc mail check

Check for unread mail addressed to a session alias or mailbox.

Without --inject: prints the count and exits 0 if mail exists, 1 if
empty. With --inject: outputs a &lt;system-reminder&gt; block suitable for
hook injection (always exits 0). The recipient defaults to $GC_ALIAS,
$GC_SESSION_ID, or "human".

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
| `--inject` | bool |  | output &lt;system-reminder&gt; block for hook injection |

## gc mail count

Show total and unread message counts for a session alias or human.
The recipient defaults to $GC_ALIAS, $GC_SESSION_ID, or "human".

```
gc mail count [session]
```

## gc mail delete

Delete a message by closing the bead. Same effect as archive but with different user intent.

```
gc mail delete <id>
```

## gc mail inbox

List all unread messages for a session alias or human.

Shows message ID, sender, subject, and body in a table. The recipient defaults
to $GC_ALIAS, $GC_SESSION_ID, or "human". Pass a session alias to view another inbox.

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
to $GC_ALIAS or $GC_SESSION_ID (in sessions) or "human". Use --notify to nudge
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
| `--from` | string |  | sender identity (default: $GC_ALIAS, $GC_SESSION_ID, or "human") |
| `-m`, `--message` | string |  | message body text |
| `--notify` | bool |  | nudge the recipient after sending |
| `-s`, `--subject` | string |  | message subject line |
| `--to` | string |  | recipient address (alternative to positional argument) |

## gc mail thread

Show all messages sharing a thread ID, ordered by time.

```
gc mail thread <thread-id>
```

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

Manage orders — formulas with gate conditions for periodic dispatch.

Orders are formulas annotated with scheduling gates (interval, cron
schedule, or shell check commands). The controller evaluates gates
periodically and dispatches order formulas when they are due.

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

## gc order check

Evaluate gate conditions for all orders and show which are due.

Prints a table with each order's gate, due status, and reason. Returns
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

List all available orders with their gate type, schedule, and target pool.

Scans formula layers for formulas that have order metadata
(gate, interval, schedule, check, pool).

```
gc order list
```

## gc order run

Execute an order manually, bypassing its gate conditions.

Instantiates a wisp from the order's formula and routes it to the
target pool (if configured). Useful for testing orders or triggering
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

Shows the order name, description, formula reference, gate type,
scheduling parameters, check command, target pool, and source file.
Use --rig to disambiguate same-name orders in different rigs.

```
gc order show <name> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--rig` | string |  | rig name to disambiguate same-name orders |

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
When agent-name is omitted, `GC_AGENT` is used automatically.

If agent-name matches a configured agent with a prompt_template,
that template is output. Otherwise outputs a default worker prompt.

```
gc prime [agent-name] [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--hook` | bool |  | compatibility mode for runtime hook invocations |

## gc register

Register a city directory with the machine-wide supervisor.

If no path is given, registers the current city (discovered from cwd).
Registration is idempotent — registering the same city twice is a no-op.
The supervisor is started if needed and immediately reconciles the city.

```
gc register [path]
```

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
| [gc runtime request-restart](#gc-runtime-request-restart) | Request controller restart this session (blocks until killed) |
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

Sets GC_RESTART_REQUESTED metadata on the session, then blocks forever.
The controller will stop the session on its next reconcile tick and
restart it fresh. The blocking prevents the agent from consuming more
context while waiting.

This command is designed to be called from within a session context.
It emits a session.draining event before blocking.

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
| [gc session prune](#gc-session-prune) | Close old suspended sessions |
| [gc session rename](#gc-session-rename) | Rename a session |
| [gc session suspend](#gc-session-suspend) | Suspend a session (save state, free resources) |
| [gc session wait](#gc-session-wait) | Register a dependency wait for a session |
| [gc session wake](#gc-session-wake) | Wake a session (clear hold and quarantine) |

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

Use --tail to control how many compaction segments to show (0 = all).
Use -f to follow new messages as they arrive.

```
gc session logs <session> [flags]
```

**Example:**

```
gc session logs mayor
  gc session logs gc-123 --tail 0
  gc session logs s-gc-123 -f
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `-f`, `--follow` | bool |  | Follow new messages as they arrive |
| `--tail` | int | `1` | Number of compaction segments to show (0 = all) |

## gc session new

Create a new persistent conversation from an agent template defined in
city.toml. By default, attaches the terminal after creation.

```
gc session new <template> [flags]
```

**Example:**

```
gc session new helper
  gc session new helper --alias sky
  gc session new helper --title "debugging auth"
  gc session new helper --no-attach
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--alias` | string |  | human-friendly session identifier for commands and mail |
| `--no-attach` | bool |  | create session without attaching |
| `--title` | string |  | human-readable session title |

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

## gc session suspend

Suspend an active session by stopping its runtime process.
The session bead persists and can be resumed later.

Accepts a session ID (e.g., gc-42) or session alias (e.g., mayor).

```
gc session suspend <session-id-or-alias>
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

Release a user hold and/or crash-loop quarantine on a session.

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
| `--force` | bool |  | suppress warnings and allow cross-rig routing |
| `-f`, `--formula` | bool |  | treat argument as formula name |
| `--merge` | string |  | merge strategy: direct, mr, or local |
| `--no-convoy` | bool |  | skip auto-convoy creation |
| `--no-formula` | bool |  | suppress default formula (route raw bead) |
| `--nudge` | bool |  | nudge target after routing |
| `--on` | string |  | attach wisp from formula to bead before routing |
| `--owned` | bool |  | mark auto-convoy as owned (skip auto-close) |
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

```
gc stop [path]
```

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

```
gc supervisor stop
```

## gc supervisor uninstall

Remove the platform service and stop the machine-wide supervisor.

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

## gc workflow

Run explicit graph-first workflow control beads

```
gc workflow
```

| Subcommand | Description |
|------------|-------------|
| [gc workflow control](#gc-workflow-control) | Execute a graph.v2 control bead in the current city |

## gc workflow control

Execute a graph.v2 control bead in the current city

```
gc workflow control <bead-id>
```

