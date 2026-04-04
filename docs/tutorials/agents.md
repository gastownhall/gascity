# Agents

In Gas City, work gets dispatched to *agents*, who take whatever work is slung at them and produce results based on how they were defined.

Part of the definition of a city is a list of agents that can be instantiated to perform work. Each agent definition in `city.toml` says which provider to use (e.g., claude, codex, etc), what prompt to give it when it begins, whether it should work on a specific rig or across the entire city, and other definition fields.

A city typically has several agents with different roles — a coordinator that plans work, workers that write code, a reviewer that checks PRs. They don't talk to each other directly. Instead, they coordinate through work items (beads) and messages (mail) in a shared store. One agent creates a task, another picks it up. This shared-nothing model means agents can come and go independently — crash, restart, scale up — without breaking the system.

## A simple agent

Agents live in `city.toml` as `[[agent]]` entries:


```toml
[[agent]]
name = "helper"
provider = "claude"
prompt_template = "prompts/helper.md"
```

And here's what `prompts/helper.md` looks like:

```markdown
# Helper

You are a helpful coding assistant. When given a task, read the relevant
code, understand the context, and do the work. Use the tools available
to you — read files, write code, run tests.
```

That's enough to sling work at it:

```
$ gc sling helper "Review the auth module for security issues"
Dispatched gc-5 → helper
```

The agent picks up the work automatically. Gas City started a Claude session, loaded the prompt from `prompts/helper.md`, and delivered the task. Under the hood, the resulting command looks something like:

```
claude --print 'You are a helpful assistant...' \
       -p 'Review the auth module for security issues'
```

You didn't have to assemble that command, manage sessions, or even know what provider is running. You defined the agent, slung the work, and Gas City handled the rest.

That's the simplest way to use an agent — sling work at it and let it run. But sometimes you want to talk to an agent directly. That's where sessions come in.

## Sessions

Every provider — Claude, Codex, Gemini — has its own way of managing conversations: session keys, chat IDs, thread state. Gas City normalizes all of that behind a single abstraction called a **session**. A session is a live process with its own terminal, state, and conversation history, regardless of which provider is underneath.

When you ran `gc sling` above, Gas City created a session behind the scenes to do the work. That's the fire-and-forget model — like submitting a batch job. But you can also create a session and interact with it yourself:

```
$ gc session new helper --alias hal
Created session gc-1 (helper) with alias 'hal'
Attaching...

> What does the auth middleware do?

I'll look at the auth middleware for you.

[reads the file]

The auth middleware in `middleware/auth.go` does three things:
1. Extracts the JWT from the Authorization header
2. Validates it against the signing key in the environment
3. Attaches the decoded claims to the request context
...

> Are there any security concerns?

Looking at it more carefully, I see two issues...
```

When you're attached to a session, you're in a live conversation. Type naturally — the agent responds just like any chat-based coding assistant, but with the full context of its prompt template and whatever work has been assigned to it.

To detach, press `Ctrl-b d` (the standard tmux detach). The session keeps running in the background — the agent continues working on whatever it was doing. You can reattach later with `gc session attach hal` and pick up right where you left off. This is one of the nice things about sessions: you can check in, give direction, detach, and come back later to see what happened.

You can also interact with running sessions without attaching. `gc session peek` shows the last few lines of a session's terminal output:

```
$ gc session peek hal --lines 3
[helper] Looking at middleware/auth.go...
[helper] The JWT validation uses HS256 with a static key.
[helper] Recommending migration to RS256 with key rotation.
```

`gc session nudge` types a message into a session's terminal without you having to attach:

```
$ gc session nudge hal "Also check the session token storage"
Nudged hal
```

To see all the sessions that are running:

```
$ gc session list
ID      ALIAS    TEMPLATE    STATE
gc-1    hal      helper      active
gc-2    —        mayor       active
gc-3    —        worker      asleep
```

Now that you know how to define agents and interact with them through sessions, let's look at how to configure agents in more detail.

## Providers

The provider is what actually runs when a session starts — Claude, Codex, Gemini, or whatever else you've configured. Most of the time, you set a workspace default for your city and don't think about it:

```toml
[workspace]
provider = "claude"
```

Agents pick up the city's provider by default, but can override it explicitly.

```toml
[[agent]]
name = "helper"
provider = "codex"
prompt_template = "prompts/helper.md"
```

Providers expose configurable options like model selection or permission mode. You can override the defaults per agent:

```toml
[[agent]]
name = "helper"
option_defaults = { model = "sonnet", permission_mode = "plan" }
```

Agent-level `option_defaults` override whatever the provider would normally use.

When a session starts, Gas City needs to get the prompt to the provider's CLI. There are three ways this can work, controlled by `prompt_mode`:

**`"arg"` (default)** — the prompt is passed as a positional argument. This is what Claude and most providers expect:

```
# prompt_mode = "arg" (the default)
claude --print 'You are a helpful coding assistant...'
```

**`"flag"`** — the prompt is passed via a specific CLI flag. Some providers want their prompt behind a named flag rather than as a bare argument:

```toml
[[agent]]
name = "kiro-worker"
prompt_mode = "flag"
prompt_flag = "--prompt"
# resulting command: kiro --prompt 'You are a...'
```

**`"none"`** — no prompt is passed on the command line at all. The provider starts up bare, and the agent gets its instructions some other way. This is where `nudge` comes in — it types text directly into the session's terminal after the provider is running:

```toml
[[agent]]
name = "vim-agent"
prompt_mode = "none"
nudge = "You are a code reviewer. Check the latest PR and review it."
```

Nudge and prompt delivery are different mechanisms. The prompt goes through the provider's CLI argument system at launch time. The nudge is raw terminal input typed *after* the provider is up. They're not interchangeable — `prompt_mode = "arg"` passes structured startup context that the provider can handle specially, while nudge is just keystrokes in the terminal.

That said, nudge is useful even with `prompt_mode = "arg"`. Long-lived agents like a mayor get their identity and capabilities from the prompt, but need a kick after waking up:

```toml
[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
nudge = "Check mail and hook status, then act accordingly."
```

The prompt says *what the agent is*. The nudge says *what to do right now*.

Agents can also set environment variables that get passed to the provider process:

```toml
[[agent]]
name = "helper"
env = { CUSTOM_VAR = "value", DEBUG = "true" }
```

These are available in the session's shell environment — useful for passing configuration to scripts, tools, or the provider itself without baking values into the prompt.

Finally, if you need absolute control over what gets run, `start_command` bypasses the provider system entirely:

```toml
[[agent]]
name = "custom"
start_command = "/usr/local/bin/my-agent --config /etc/agent.toml"
```

## Identity, scope, and working directory

So far we've been looking at a single agent in isolation. In practice, a city has multiple agents with different roles, often working across multiple projects. Gas City needs to know whether an agent is a singleton that oversees the whole city, or whether it should exist once per project — and where each agent's files should live.

That's what `scope` and `work_dir` control.

### Scope

Some agents — like a coordinator — make sense as singletons. Others — like a code worker — should exist once per project (rig), so each project gets its own dedicated instance.

```toml
# City-scoped — one for the whole city
[[agent]]
name = "mayor"
scope = "city"
prompt_template = "prompts/mayor.md"

# Rig-scoped — one per rig (the default)
[[agent]]
name = "worker"
prompt_template = "prompts/worker.md"
```

A city-scoped agent like `mayor` has a bare name — there's exactly one, and you sling work to it as `gc sling mayor "..."`. A rig-scoped agent like `worker` gets *stamped* once per rig: if your city has three rigs, you get `rig-a/worker`, `rig-b/worker`, and `rig-c/worker`.

When you sling work to `rig-a/worker`, it lands in the right project context automatically — the agent's working directory, beads, and identity are all scoped to that rig. If you made all your workers city-scoped, you'd have to manually tell each one which project to work in and keep them from stepping on each other's files. Scope gives you that isolation for free.

### Rig overrides

Sometimes you want the same agents but with different settings per rig. Overrides let you customize pack-stamped agents for a specific rig without redefining the whole agent:

```toml
[[rigs]]
name = "my-app"
path = "/path/to/my-app"
includes = ["packs/gastown"]

[[rigs.overrides]]
agent = "polecat"
max_active_sessions = 10    # This rig gets more workers

[[rigs.overrides]]
agent = "witness"
idle_timeout = "2h"         # This rig's witness stays awake longer
```

Overrides are applied after packs are expanded — they're the final word on how an agent behaves in a specific rig.

You can also set a default sling target per rig, so `gc sling BL-42` from within that directory routes to the right agent automatically:

```toml
[[rigs]]
name = "backend"
path = "/path/to/backend"
default_sling_target = "backend/polecat"
```

### Working directory

By default, a rig-scoped agent starts in the rig's root directory, and a city-scoped agent starts in the city root. You can override this with `work_dir`:

```toml
[[agent]]
name = "mayor"
work_dir = "agents/mayor"
```

Why override? File isolation. If two agents are both editing code in the same directory, they'll step on each other's changes. Giving each agent its own `work_dir` prevents that.

This becomes especially important when you have multiple sessions from the same agent definition. You don't want five workers all editing `main.go` in the same checkout. Template variables let you give each session a unique directory:

```toml
[[agent]]
name = "polecat"
work_dir = "worktrees/{{.Rig}}/polecats/{{.AgentBase}}"
```

Gas City expands `{{.Rig}}`, `{{.AgentBase}}`, and other variables at session creation time, so each session gets its own workspace.

### Overlays

When an agent declares `overlay_dir`, that directory is recursively copied into the agent's working directory at startup:

```toml
[[agent]]
name = "polecat"
overlay_dir = "overlays/default"
```

If `overlays/default/` contains a `CLAUDE.md` file, every polecat session gets that file in its workspace. Existing files aren't overwritten — overlays add to the workspace without stomping on project files.

This is how packs inject agent instructions, tool configurations, and dotfiles into agent environments. A pack ships an `overlays/` directory, agents reference it, and every session gets the right context without anyone manually copying files around.

## Putting it together

Here's a minimal agent that does something useful:

```toml
# city.toml
[workspace]
name = "my-city"
provider = "claude"

[[agent]]
name = "reviewer"
prompt_template = "prompts/reviewer.md"
idle_timeout = "30m"
```

And a simple prompt to go with it:

```markdown
# Code Reviewer

You review pull requests. When given a PR URL or branch name, check out
the code, read the changes, and provide feedback.

Use `gh pr view` and `gh pr diff` to inspect pull requests.
```

Fire it up:

```
$ gc start
City 'my-city' started

$ gc session new reviewer --alias rev
Created session gc-1 (reviewer) with alias 'rev'
Attaching...
```

You're now talking to a code reviewer.


## Command reference

| Command | What it does |
|---|---|
| `gc sling <agent> <work>` | Route work to an agent |
| `gc session new <template>` | Create a session from an agent |
| `gc session new <template> --alias <name>` | Create with a friendly name |
| `gc session list` | List all sessions |
| `gc session attach <id-or-alias>` | Attach to a session |
| `gc session peek <id-or-alias>` | See last lines without attaching |
| `gc session nudge <id-or-alias> <msg>` | Type a message into a session |
| `gc session suspend <id-or-alias>` | Put a session to sleep |
| `gc session wake <id-or-alias>` | Wake a sleeping session |
| `gc session close <id-or-alias>` | Close a session permanently |
| `gc agent add --name <n>` | Add an agent to city.toml |
| `gc agent suspend <name>` | Suspend an agent template |
| `gc agent resume <name>` | Resume a suspended agent |

<!--
BONEYARD — draft material for future sections. Not part of the published tutorial.

### Startup lifecycle

When a session is created from an agent, several things happen in order:

```
  pre_start          session_setup       session_live
     |                    |                   |
     v                    v                   v
 +--------+  +--------------+  +------+  +----------+  +---------+
 | Setup  |--| Start provider|--| Setup|--| Live     |--| Ready   |
 | dirs   |  | in tmux       |  |script|  | commands |  |         |
 +--------+  +--------------+  +------+  +----------+  +---------+
                                               ^
                                               |
                                       re-applied on
                                       config change
```

1. **`pre_start`** — shell commands before the session is created.
2. **Session creation** — the provider process starts in tmux.
3. **`session_setup`** — shell commands after creation, in gc's process.
4. **`session_setup_script`** — a script run after setup. Gets `GC_SESSION` as an env var.
5. **`session_live`** — idempotent commands re-applied on config change without restart.
6. **`overlay_dir`** — a directory copied into the agent's workspace. Existing files aren't overwritten.

```toml
[[agent]]
name = "polecat"
pre_start = ["scripts/worktree-setup.sh {{.RigRoot}} {{.WorkDir}} {{.AgentBase}} --sync"]
overlay_dir = "overlays/default"
session_live = [
    "scripts/tmux-theme.sh {{.Session}} {{.Agent}}",
    "scripts/tmux-keybindings.sh",
]
```
### Pools

```toml
[[agent]]
name = "polecat"
min_active_sessions = 0
max_active_sessions = 5
namepool = "namepools/names.txt"
work_query = "gc beads list --state open --routed-to {{.Agent}} --limit 1"
sling_query = "gc sling {{.Agent}} {}"
```

Pools let an agent scale up and down based on demand. The controller creates sessions when work is available and drains them when idle.

### Resume command

For agents that need special resume behavior:

```toml
[[agent]]
name = "mayor"
resume_command = "claude --resume {{.SessionKey}} --dangerously-skip-permissions"
```

When set, this takes precedence over the provider's default resume mechanism.

### Drain timeout

When scaling down, sessions need time to finish their current work:

```toml
[[agent]]
name = "polecat"
drain_timeout = "30m"
```

The controller waits up to the drain timeout before force-killing a session during scale-down. Defaults to "5m".

### Boot and death hooks

```toml
[[agent]]
name = "helper"
on_boot = "scripts/init-workspace.sh"
on_death = "scripts/cleanup.sh"
```

`on_boot` runs once at controller startup. `on_death` runs when a session dies unexpectedly.
-->
