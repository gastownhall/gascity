---
title: Tutorial 02 - Agents
sidebarTitle: 02 - Agents
description: Define agents with custom prompts and providers, interact through sessions, and configure scope and working directories.
---

In [Tutorial 01](/tutorials/01-cities), you created a city, slung work to an
implicit agent, and added a rig. The implicit agents (`claude`, `codex`, etc.)
are convenient, but they have no custom prompt — they're just the raw provider.
In this tutorial, you'll define your own agents with specific roles, interact
with them through sessions, and see how scope and working directories keep
things organized.

We'll pick up where Tutorial 01 left off. You should have `my-city` running with
`my-project` rigged.

## Defining an agent

Open `city.toml`. You already have a `mayor` agent from the tutorial template.
Let's add a second agent that uses `codex` instead of `claude`:

```toml
[workspace]
name = "my-city"
provider = "claude"

... # context elided

[[agent]]
name = "reviewer"
provider = "codex"
prompt_template = "prompts/reviewer.md"
```

You'll want to create a prompt for the new agent. Let's take a look at the
default GC prompt if you don't provide one:

```shell
~/my-city
$ gc prime
# Gas City Agent

You are an agent in a Gas City workspace. Check for available work
and execute it.

## Your tools

- `bd ready` — see available work items
- `bd show <id>` — see details of a work item
- `bd close <id>` — mark work as done

## How to work

1. Check for available work: `bd ready`
2. Pick a bead and execute the work described in its title
3. When done, close it: `bd close <id>`
4. Check for more work. Repeat until the queue is empty.
```

The `gc prime` command let's an agent running in GC how to behave, specially how
to look for work that's been assigned to it. In [tutorial
01](/tutorials/01-cities), we learned that slinging work to an agent created a
bead. Looking here at the default prompt, it should be clear how the agent can
actually pick up work that was slung its way.

What we want to do is to preserve the instructions on how to be an agent in GC,
but also add the specifics for being a review agent. To do that, create the
reviewer prompt to look like the following:

```shell
~/my-city
$ cat > prompts/reviewer.md << 'EOF'
# Code Reviewer Agent
You are an agent in a Gas City workspace. Check for available work and execute it.

## Your tools
- `bd ready` — see available work items
- `bd show <id>` — see details of a work item
- `bd close <id>` — mark work as done

## How to work
1. Check for available work: `bd ready`
2. Pick a bead and execute the work described in its title
3. When done, close it: `bd close <id>`
4. Check for more work. Repeat until the queue is empty.

## Reviewing Code
Read the code and provide feedback on bugs, security issues, and style.
EOF
$ gc prime reviewer
# Code Reviewer Agent
You are an agent in a Gas City workspace. Check for available work and execute it.
... # contents elided as identical to the above
```

Notice that use of `gc prime <agent-name>` to get the contents of your custom
prompt for that agent. That's a handy way to check on how the built-in agents or
your own custom agents are configured as you build out more of them over time.

Now that your agent is available, it's time to sling some work to it:

```shell
~/my-city
$ cd ~/my-project
~/my-project
$ gc sling reviewer "Review hello.py and write review.md with feedback"
Created mc-p956 — "Review hello.py and write review.md with feedback"
Auto-convoy mc-4wdl
Slung mc-p956 → reviewer
```

Your new reviewer agent picks up the work automatically. Gas City started a
Codex session, loaded the prompt from `prompts/reviewer.md`, and delivered the
task. You can watch progress with `bd show` as you already know. And when the
work is done, you can check the file system for the review you requested:

```shell
~/my-project
$ ls
hello.py  review.md

~/my-project
$ cat review.md
# Review
No findings.

`hello.py` is a single `print("Hello, World!")` statement and does not present a meaningful bug, security, or style issue in its current form.
```

This is handy for fire-and-forget kind of work. However, if you'd like to see
the agent in action or even talk to one directly, you're going to need a
session.

## Looking in on polecats

Every provider — Claude, Codex, Gemini, etc. — has its own way of managing
conversations. Gas City normalizes all of that behind a single abstraction
called a **session**. A session is a live process with its own terminal, state,
and conversation history.

When you sling a bead, you're creating a session. You can peek at what's
happening in that session with the `gc session peek` command, passing in the
name of the agent you'd like to check in on:

```shell
~/my-project
$ gc session peek reviewer
› [my-city] reviewer • 2026-04-07T11:56:59

  Run `gc prime` to initialize your context.

  # Code Reviewer Agent
  You are an agent in a Gas City workspace. Check for available work and
  execute it.

  ## Your tools
  - `bd ready` — see available work items
  - `bd show <id>` — see details of a work item
  - `bd close <id>` — mark work as done

  ## How to work
  1. Check for available work: `bd ready`
  2. Pick a bead and execute the work described in its title
  3. When done, close it: `bd close <id>`
  4. Check for more work. Repeat until the queue is empty.

  ## Reviewing Code
  Read the code and provide feedback on bugs, security issues, and style.


• Using beads-br guidance for the ready-queue workflow. I’m priming the
  workspace context first, then I’ll inspect the ready beads and start reviewing
  the first actionable item.

• Explored
  └ Read SKILL.md

• Ran gc prime
  └ # Code Reviewer Agent
    You are an agent in a Gas City workspace. Check for available work and
    … +13 lines
    ## Reviewing Code
    Read the code and provide feedback on bugs, security issues, and style.

────────────────────────────────────────────────────────────────────────────────

• Context is primed. Next I’m querying the queue in structured form so I can
  take the first actionable review item and inspect its details before touching
  anything.

• Ran bd ready --json
  └ warning: beads.role not configured (GH#2950).
      Fix: git config beads.role maintainer
    … +282 lines
      }
    ]

────────────────────────────────────────────────────────────────────────────────

• The ready queue has one task that is explicitly routed to the reviewer agent:
  mc-p956, “Review hello.py and write review.md with feedback.” I’m pulling the
  bead details and the target file now, then I’ll write the review and close
  that item.

• Explored
  └ List rg --files -g hello.py -g review.md

• Ran bd show mc-p956 --json
  └ [
      {
    … +27 lines
      }
    ]

• Working (43s • esc to interrupt)


› Summarize recent commits

  gpt-5.4 xhigh fast · 84% left · ~/my-city
```

You'll notice that the result of `gc prime` for our reviewer agent as the first
input to the `codex` CLI. That's how GC lets Codex know how to act. Then you'll
notice Codex acting on those instructions by looking for the beads that are
ready for it to act on. It finds one, executes it and out comes our `review.md`
file.

When an agent session is created to do work and the work is complete, the agent
will go idle. And when it's been idle for a little while, it will be cleanly
shutdown by the GC supervisor process. These transient sessions are often used
by one-and-done agents know as "polecats". While you can talk to one
interactively, they're configured to execute beads and then return resources to
the system.

If you want an agent to to talk to, you'll want an agent configured for chatting
called a "crew" member.

## Chatting with crew

TODO:STARTHERE

Let's create a session from the `helper` agent and give it an alias `hal` so you
can refer to it easily:

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

You're in a live conversation. The agent responds just like any chat-based
coding assistant, but with the full context of its prompt template.

To detach without killing the session, press `Ctrl-b d` (the standard tmux
detach). The session keeps running in the background. Reattach anytime:

```shell
~/my-city
$ gc session attach hal
```

You can also interact with running sessions without attaching. You vsn peek at
the last few lines of output from your agent:

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

To get a feel for whats's happening in your city, you can see all running
sessions:

```shell
~/my-city
$ gc session list
ID      ALIAS    TEMPLATE    STATE
my-2    —        helper      active
my-3    hal      helper      active
my-4    —        mayor       active
```

## Changing the provider

By default, agents use the city's provider (set in `[workspace]`). But an agent
can use a different one. Let's make the reviewer from Tutorial 01 use Codex:

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

One request went to Claude, the other to Codex. You don't have to think about
which CLI to invoke or how each provider wants its arguments.

You can also override provider options per agent. For example, to pin a specific
model and permission mode:

```toml
[[agent]]
name = "helper"
prompt_template = "prompts/helper.md"
option_defaults = { model = "sonnet", permission_mode = "plan" }
```

## Nudge vs. prompt

You've seen `prompt_template` — it tells the agent what it is at startup.
There's a related concept called `nudge` — text typed into the session's
terminal after the agent is up and running.

The difference: the prompt sets the agent's _intrinsic identity_. The nudge
tells it _what to do right now_.

```toml
[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
nudge = "Check mail and hook status, then act accordingly."
```

This is useful for long-lived agents that need a kick after waking up. The
mayor's prompt defines its role and capabilities. The nudge says "go — start by
checking what needs attention."

## City agents and rig agents

In Tutorial 01, when you slung work from inside `my-project`, the target showed
up as `my-project/claude` — the agent was scoped to that rig. That happened
automatically with the implicit provider agents. You can control this explicitly
with the `scope` field.

Think about what happens as your city grows. You add a second project — say,
`my-api`. Now you have two rigs with code to work on. A coordinator agent only
needs one instance — it plans work across the whole city. But a coding agent
needs to work in a specific project's directory, with that project's files and
context. You don't want one worker trying to juggle two codebases.

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

The default scope is `"rig"`. Let's see what that means. Add a `worker` agent
and a second rig, then restart:

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

The `worker` was automatically stamped for each rig — `my-project/worker` and
`my-api/worker`. Each has its own working directory, its own beads, and its own
identity. When you sling work to `my-api/worker`, it lands in the right project
context automatically.

## Working directory

By default, a rig-scoped agent starts in the rig's root directory and a
city-scoped agent starts in the city root. You can override this with
`work_dir`:

```toml
[[agent]]
name = "mayor"
scope = "city"
work_dir = "agents/mayor"
prompt_template = "prompts/mayor.md"
```

Why override? File isolation. If two agents are both editing code in the same
directory, they'll step on each other's changes. Giving each agent its own
`work_dir` prevents that.

This becomes especially important when you have multiple sessions from the same
agent. Template variables let you give each session a unique directory:

```toml
[[agent]]
name = "polecat"
work_dir = "worktrees/{{.Rig}}/polecats/{{.AgentBase}}"
```

Gas City expands `{{.Rig}}`, `{{.AgentBase}}`, and other variables at session
creation time, so each session gets its own isolated workspace.

## What's next

You've defined agents with custom prompts, interacted with them through
sessions, configured different providers, and set up scope and working
directories. From here:

- **[Sessions](/tutorials/03-sessions)** — session lifecycle, sleep/wake,
  suspension, named sessions
- **[Formulas](/tutorials/04-formulas)** — multi-step workflow templates with
  dependencies and variables
- **[Beads](/tutorials/05-beads)** — the work tracking system underneath it all
