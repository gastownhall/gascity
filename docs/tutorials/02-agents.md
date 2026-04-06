---
title: Tutorial 02 - Agents
description: Define agents with custom prompts and providers, interact through sessions, and configure scope and working directories.
---

# Tutorial 02: Agents

In [Tutorial 01](01-cities.md), you created a city, slung work to an implicit agent, and added a rig. The implicit agents (`claude`, `codex`, etc.) are convenient, but they have no custom prompt — they're just the raw provider. In this tutorial, you'll define your own agents with specific roles, interact with them through sessions, and see how scope and working directories keep things organized.

We'll pick up where Tutorial 01 left off. You should have `my-city` running with `my-project` rigged.

## Defining an agent

Open `city.toml`. You already have a `mayor` agent from the tutorial template. Let's add a `helper` agent next to it:

```toml
[workspace]
name = "my-city"
provider = "claude"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"

[[agent]]
name = "helper"
prompt_template = "prompts/helper.md"
```

Now create the prompt file:

```shell
~/my-city
$ cat > prompts/helper.md << 'EOF'
# Helper

You are a helpful coding assistant. When given a task, read the relevant
code, understand the context, and do the work. Use the tools available
to you — read files, write code, run tests.
EOF
```

Then restart the city to pick up the new agent:

```shell
~/my-city
$ gc restart
```

Now that your agent is available, it's time to sling some work to it:

```shell
~/my-city
$ gc sling helper "Review the auth module for security issues"
Created my-2 — "Review the auth module for security issues"
Slung my-2 → helper
```

The agent picks up the work automatically. Gas City started a Claude session (the default provider for the city), loaded the prompt from `prompts/helper.md`, and delivered the task. You can watch progress with `bd show`:

```shell
~/my-city
$ bd show my-2 --watch
⧖ my-2 · Review the auth module for security issues   [● P2 · OPEN]
Owner: you · Type: task

Watching for changes... (Press Ctrl+C to exit)
```

That's the fire-and-forget model — sling work and let the agent run. But sometimes you want to talk to an agent directly, often over multiple turns. That's where sessions come in.

## Talking to an agent

Every provider — Claude, Codex, Gemini — has its own way of managing conversations. Gas City normalizes all of that behind a single abstraction called a **session**. A session is a live process with its own terminal, state, and conversation history.

Let's create a session from the `helper` agent and give it an alias `hal` so you can refer to it easily:

```shell
~/my-city
$ gc session new helper --alias hal
Created session my-3 (helper) with alias 'hal'
Attaching...

> What does the auth middleware do?

I'll look at the auth middleware for you.

[reads the file]

The auth middleware in middleware/auth.go does three things:
1. Extracts the JWT from the Authorization header
2. Validates it against the signing key in the environment
3. Attaches the decoded claims to the request context
...

> Are there any security concerns?

Looking at it more carefully, I see two issues...
```

You're in a live conversation. The agent responds just like any chat-based coding assistant, but with the full context of its prompt template.

To detach without killing the session, press `Ctrl-b d` (the standard tmux detach). The session keeps running in the background. Reattach anytime:

```shell
~/my-city
$ gc session attach hal
```

You can also interact with running sessions without attaching. You vsn peek at the last few lines of output from your agent:

```shell
~/my-city
$ gc session peek hal --lines 3
[helper] Looking at middleware/auth.go...
[helper] The JWT validation uses HS256 with a static key.
[helper] Recommending migration to RS256 with key rotation.
```

Or you can nudge it, which types a new message into the session's terminal:

```shell
~/my-city
$ gc session nudge hal "Also check the session token storage"
Nudged hal
```

To get a feel for whats's happening in your city, you can see all running sessions:

```shell
~/my-city
$ gc session list
ID      ALIAS    TEMPLATE    STATE
my-2    —        helper      active
my-3    hal      helper      active
my-4    —        mayor       active
```

## Changing the provider

By default, agents use the city's provider (set in `[workspace]`). But an agent can use a different one. Let's make the reviewer from Tutorial 01 use Codex:

```toml
[[agent]]
name = "reviewer"
prompt_template = "prompts/reviewer.md"
provider = "codex"
```

Restart the city to pick up the change:

```shell
~/my-city
$ gc restart
```

Now sling to both agents — same command, different providers handling it:

```shell
~/my-project
$ gc sling helper "Add input validation to the API"
Slung mp-2 → my-project/helper

~/my-project
$ gc sling reviewer "Review the latest changes"
Slung mp-3 → my-project/reviewer
```

One request went to Claude, the other to Codex. You don't have to think about which CLI to invoke or how each provider wants its arguments.

You can also override provider options per agent. For example, to pin a specific model and permission mode:

```toml
[[agent]]
name = "helper"
prompt_template = "prompts/helper.md"
option_defaults = { model = "sonnet", permission_mode = "plan" }
```

## Nudge vs. prompt

You've seen `prompt_template` — it tells the agent what it is at startup. There's a related concept called `nudge` — text typed into the session's terminal after the agent is up and running.

The difference: the prompt sets the agent's *intrinsic identity*. The nudge tells it *what to do right now*.

```toml
[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
nudge = "Check mail and hook status, then act accordingly."
```

This is useful for long-lived agents that need a kick after waking up. The mayor's prompt defines its role and capabilities. The nudge says "go — start by checking what needs attention."

## City agents and rig agents

In Tutorial 01, when you slung work from inside `my-project`, the target showed up as `my-project/claude` — the agent was scoped to that rig. That happened automatically with the implicit provider agents. You can control this explicitly with the `scope` field.

Think about what happens as your city grows. You add a second project — say, `my-api`. Now you have two rigs with code to work on. A coordinator agent only needs one instance — it plans work across the whole city. But a coding agent needs to work in a specific project's directory, with that project's files and context. You don't want one worker trying to juggle two codebases.

That's what `scope` controls:

```toml
[[agent]]
name = "mayor"
scope = "city"
prompt_template = "prompts/mayor.md"

[[agent]]
name = "worker"
prompt_template = "prompts/worker.md"
```

The default scope is `"rig"`. Let's see what that means. Add a `worker` agent and a second rig, then restart:

```shell
~/my-city
$ cat > prompts/worker.md << 'EOF'
# Worker

You are a coding agent. When given a task, implement it carefully.
Read the existing code first, write tests, then implement.
EOF
```

```shell
~/my-city
$ gc rig add ~/my-api
Added rig 'my-api' to city 'my-city'
  Prefix: ma
  Beads:  initialized
  Hooks:  installed (claude)
```

```shell
~/my-city
$ gc restart
```

```shell
~/my-city
$ gc status
my-city  /Users/you/my-city
  Controller: running (PID 12345)

Agents:
  mayor                      running
  helper                     running
  my-project/worker          running
  my-project/reviewer        running
  my-api/worker              running
  my-api/reviewer            running
```

The `worker` was automatically stamped for each rig — `my-project/worker` and `my-api/worker`. Each has its own working directory, its own beads, and its own identity. When you sling work to `my-api/worker`, it lands in the right project context automatically.

## Working directory

By default, a rig-scoped agent starts in the rig's root directory and a city-scoped agent starts in the city root. You can override this with `work_dir`:

```toml
[[agent]]
name = "mayor"
scope = "city"
work_dir = "agents/mayor"
prompt_template = "prompts/mayor.md"
```

Why override? File isolation. If two agents are both editing code in the same directory, they'll step on each other's changes. Giving each agent its own `work_dir` prevents that.

This becomes especially important when you have multiple sessions from the same agent. Template variables let you give each session a unique directory:

```toml
[[agent]]
name = "polecat"
work_dir = "worktrees/{{.Rig}}/polecats/{{.AgentBase}}"
```

Gas City expands `{{.Rig}}`, `{{.AgentBase}}`, and other variables at session creation time, so each session gets its own isolated workspace.

## What's next

You've defined agents with custom prompts, interacted with them through sessions, configured different providers, and set up scope and working directories. From here:

- **[Sessions](03-sessions.md)** — session lifecycle, sleep/wake, suspension, named sessions
- **[Formulas](04-formulas.md)** — multi-step workflow templates with dependencies and variables
- **[Beads](05-beads.md)** — the work tracking system underneath it all

<!--
BONEYARD — draft material for future tutorials or reference docs.

See fodder/agents.md for the previous version.

### Prompt delivery modes

prompt_mode controls how the prompt gets to the provider CLI:
- "arg" (default) — positional argument: `claude --print 'prompt...'`
- "flag" — named flag: `kiro --prompt 'prompt...'`
- "none" — no prompt on CLI. Use nudge for instructions.

### Environment variables

```toml
[[agent]]
name = "helper"
env = { CUSTOM_VAR = "value", DEBUG = "true" }
```

### start_command

Bypasses the provider system entirely:

```toml
[[agent]]
name = "custom"
start_command = "/usr/local/bin/my-agent --config /etc/agent.toml"
```

### Rig overrides

Customize pack-stamped agents per rig:

```toml
[[rigs.overrides]]
agent = "polecat"
max_active_sessions = 10

[[rigs.overrides]]
agent = "witness"
idle_timeout = "2h"
```

### Default sling target

```toml
[[rigs]]
name = "backend"
path = "/path/to/backend"
default_sling_target = "backend/polecat"
```

### Overlays

```toml
[[agent]]
name = "polecat"
overlay_dir = "overlays/default"
```

overlay_dir is recursively copied into the agent's working directory at startup.
Existing files aren't overwritten. This is how packs inject CLAUDE.md, .cursorrules, etc.

### Startup lifecycle

1. pre_start — shell commands before session creation
2. Session creation — provider starts in tmux
3. session_setup — shell commands after creation
4. session_setup_script — script with GC_SESSION env var
5. session_live — idempotent commands re-applied on config change
6. overlay_dir — directory copied into workspace

### Pools

A single agent definition can run multiple sessions in parallel. A *pool* is an agent template configured with `max_active_sessions` greater than one — the controller spins up or winds down instances based on demand. When work is routed to a pool, any available instance can pick it up.

Pools are how Gas City scales: instead of one `worker` agent handling tasks sequentially, you can have up to five `polecat` instances working in parallel, each with its own session and workspace. The controller manages the pool size automatically — starting new instances when work is waiting, letting idle ones sleep when the queue is empty.

```toml
[[agent]]
name = "polecat"
min_active_sessions = 0
max_active_sessions = 5
namepool = "namepools/names.txt"
work_query = "gc beads list --state open --routed-to {{.Agent}} --limit 1"
sling_query = "gc sling {{.Agent}} {}"
```

The `namepool` gives each instance a unique name drawn from a text file (one name per line). `work_query` is the command each instance runs to check for available work. `min_active_sessions` controls how many instances stay warm even when there's no work — set it to 0 for fully demand-driven scaling.

### Dependencies

```toml
[[agent]]
name = "worker"
depends_on = ["mayor"]
```

### Command reference

| Command | What it does |
|---|---|
| gc sling <agent> <work> | Route work to an agent |
| gc session new <template> | Create a session from an agent |
| gc session new <template> --alias <name> | Create with a friendly name |
| gc session list | List all sessions |
| gc session attach <id-or-alias> | Attach to a session |
| gc session peek <id-or-alias> | See last lines without attaching |
| gc session nudge <id-or-alias> <msg> | Type a message into a session |
| gc session suspend <id-or-alias> | Put a session to sleep |
| gc session wake <id-or-alias> | Wake a sleeping session |
| gc session close <id-or-alias> | Close a session permanently |
| gc agent add --name <n> | Add an agent to city.toml |
| gc agent suspend <name> | Suspend an agent template |
| gc agent resume <name> | Resume a suspended agent |
-->
