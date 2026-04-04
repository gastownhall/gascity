# Sessions

The agents chapter covered the basics — creating sessions with `gc session new`, attaching and detaching, peeking, nudging. This chapter goes deeper into what happens once sessions are running: how they sleep and wake, how agents talk to each other, and how you manage sessions over time.

## Session lifecycle

A session isn't just "running" or "stopped." It moves through states as it's created, does work, goes idle, and eventually shuts down. Understanding the lifecycle helps you make sense of what `gc session list` is telling you and why a session is or isn't responding.

```
                          idle timeout
              ┌───────────────────────────────┐
              │                               ▼
creating ──► active ◄──────────────────── asleep
              │  ▲        wake/nudge/
              │  │        attach
              │  │
              │  └──── wake ──── suspended
              │                     ▲
              │                     │
              │               gc session suspend
              │
              ▼
           draining ──► archived
```

Every session is in exactly one of these states:

| State | What it means |
|---|---|
| **creating** | Bead exists but process hasn't started yet. Transient. |
| **active** | Running. Process is alive, accepting input. |
| **asleep** | Went dormant after idle timeout. No runtime resources. Wakes on attach, nudge, or slung work. |
| **suspended** | You explicitly paused it with `gc session suspend`. Stays down until you say otherwise. |
| **draining** | Gracefully stopping. Finishing in-flight work before shutdown. |
| **archived** | Done draining. Kept around for history. |
| **quarantined** | Hit the crash-loop threshold. Temporarily blocked from waking. |

In practice you'll mostly see `active`, `asleep`, and `suspended`. The others are transient or edge cases — you'll know when you hit them.

You can filter `gc session list` by state:

```
$ gc session list --state all
ID      ALIAS    TEMPLATE    STATE
gc-1    sky      mayor       active
gc-2    —        mayor       asleep
gc-5    rev      reviewer    suspended
gc-7    —        worker      archived
```

## Sleep and wake

Sessions don't run forever. If an agent sits idle, Gas City puts it to sleep — kills the process, frees the resources, keeps the state. The `idle_timeout` on the agent controls how long to wait:

```toml
[[agent]]
name = "mayor"
idle_timeout = "1h"
```

After an hour of no activity, the mayor's session goes to sleep. No process, no memory, no cost. But the session still exists — it's just not running.

Waking happens automatically. Attach to a sleeping session, nudge it, or sling work at it, and Gas City brings it back:

```
$ gc session attach sky
Waking session sky...
Attached.
```

When a session wakes, the provider needs to restore the conversation. There are two strategies:

- **`"resume"` (default)** — reuses the provider's session key to pick up the existing conversation. Claude does this with `--resume`.
- **`"fresh"`** — starts a brand new provider session every time. The agent gets its prompt again but loses conversation history.

Most agents use resume. Fresh is useful for stateless workers that don't need continuity.

## Suspend and resume

Sleep is automatic. Suspend is manual — you're telling Gas City "put this down, I'll come back to it later."

```
$ gc session suspend sky
Suspended session sky
```

A suspended session won't wake on its own. It stays down until you explicitly bring it back:

```
$ gc session wake sky
Waking session sky...

$ gc session attach sky
Attached.
```

Suspend is useful when you want an agent to stop doing work but don't want to lose the session. Maybe you're pausing a long-running task, or you've got too many sessions competing for rate limits.

The difference from sleep: a sleeping session wakes automatically when something needs it. A suspended session stays suspended until you say otherwise.

## Closing, killing, and pruning

When you're done with a session, close it:

```
$ gc session close sky
Closed session sky
```

Close is graceful — the session moves to `draining`, finishes in-flight work, then transitions to `archived`.

If a session is misbehaving and you need it gone now:

```
$ gc session kill sky
```

Kill terminates the process immediately. If the session is a named session with `mode = "always"`, the controller will restart it — you're killing the process, not the declaration. To stop a named session from restarting, either remove the `[[named_session]]` entry or suspend the agent template with `gc agent suspend`.

Over time, suspended and archived sessions pile up. Prune cleans them out:

```
$ gc session prune --before 7d
```

This closes any suspended session older than the specified duration. Useful as periodic maintenance — either run it manually or let a formula handle it.

## Communicating with sessions

Agents in Gas City don't call each other directly — no function calls, no shared memory, no direct references. They're independent processes that coordinate through messages and work items in a shared store. One agent creates a task, another picks it up. This shared-nothing model is what lets agents crash, restart, and scale independently without breaking each other.

There are two ways sessions communicate:

- **Mail** — persistent, structured messages tracked as beads. A mayor can mail a worker a review request. The worker picks it up on its next turn via hooks. Messages have subjects, stay unread until processed, and survive crashes.
- **Nudge** — immediate terminal input. Fast and direct, but ephemeral. Good for quick pokes, not for anything that needs to survive a restart.

There's also a third path that doesn't look like communication but is: **slung work**. When the mayor runs `gc sling worker "fix the auth bug"`, it's not talking to a specific session — it's routing a bead to an agent template. Gas City figures out which session picks it up. This indirection is what lets you scale workers without the coordinator knowing or caring how many are running.

The agents chapter covered basic nudge and peek. Here's the rest.

### Nudge delivery modes

The agents chapter showed the basic nudge — type a message into a session's terminal. But nudge has delivery modes that matter for automation:

```
$ gc session nudge sky "New PR on the demo rig" --delivery immediate
Nudged sky
```

Three modes:

- **`immediate` (default)** — sends now. If the session is asleep, wakes it first.
- **`wait-idle`** — waits until the agent finishes its current turn, then delivers. Avoids interrupting mid-thought.
- **`queue`** — queues the message. A poller delivers it later. Survives crashes — queued nudges are retried until delivered or expired.

Immediate is fine for human use. For agent-to-agent coordination, `wait-idle` and `queue` prevent messages from getting lost or garbled mid-response.

### Mail

Nudge is terminal input — ephemeral, best-effort, one line at a time. Mail is the structured messaging system. It creates persistent message beads that agents can check, read, and act on.

Send mail to an agent:

```
$ gc mail send mayor "Review needed" "Please look at the auth module changes in the demo rig"
Sent message gc-15 to mayor
```

Check for unread mail:

```
$ gc mail check mayor
1 unread message(s)
```

Read the inbox:

```
$ gc mail inbox mayor
ID      FROM    SUBJECT          STATE
gc-15   human   Review needed    unread
```

Mail vs. nudge: nudge is like tapping someone on the shoulder. Mail is like dropping a memo in their inbox. Nudge is instant but can get lost if the agent is busy or the session crashes. Mail persists — it's tracked as a bead, has a subject, and stays unread until the agent processes it.

### How agents check mail

Agents don't poll their inbox manually. The typical pattern is a provider hook that runs `gc mail check --inject` on each turn. If there's unread mail, the hook surfaces it as a system reminder in the agent's context — the agent sees its mail without having to do anything.

```toml
[[agent]]
name = "mayor"
install_agent_hooks = ["claude"]
```

The `install_agent_hooks` field controls which hooks get installed. Hooks are how Gas City integrates with agent sessions — mail checking, work delivery, nudge draining. Without hooks, the agent is just a plain provider session with no Gas City awareness.

### Logs

Sessions record their full conversation history to JSONL files. You can read them back with `gc session logs`:

```
$ gc session logs sky --tail 3
[2026-03-30 14:22:01] user: Check the status of BL-12
[2026-03-30 14:22:03] assistant: BL-12 is currently open, assigned to rev...
[2026-03-30 14:22:15] assistant: The reviewer has posted 3 comments so far.
```

Follow live output as it arrives:

```
$ gc session logs sky -f
```

This is useful for watching what a background agent is doing without attaching to it. Peek shows the terminal — logs show the conversation.

## Implicit Sessions

Everything we've looked at so far has been about sessions you create manually with `gc session new`. 

A city may declare that one or more agents need to be in running sessions as part of the initialization of the city.  The `city.toml` file allows one or more named sessions to be declared. 

```toml
[[named_session]]
template = "mayor"
scope = "city"
mode = "always"
```

This tells the controller that there should always be a running session for the mayor. If it dies, restart it. If the city starts, start it. You don't create it yourself — the controller handles the lifecycle.

There are two modes for named sessions:

- **`always`** — the controller keeps this session running. If it dies, it comes back. If the city starts, it starts.
- **`on_demand`** — reserves the identity but doesn't auto-start. The session gets created when something needs it — slung work, a nudge, an attach — and goes away when idle.

Named sessions are for agents that should be persistently available: coordinators, monitors, patrol agents. For temporary or human-initiated work, `gc session new` is still the right call.

### Example: always-on coordinator with on-demand workers

```toml
[[agent]]
name = "mayor"
scope = "city"
prompt_template = "prompts/mayor.md"
idle_timeout = "1h"

[[named_session]]
template = "mayor"
scope = "city"
mode = "always"

[[agent]]
name = "worker"
scope = "rig"
prompt_template = "prompts/worker.md"

[[named_session]]
template = "worker"
scope = "rig"
mode = "on_demand"
```

The mayor starts with the city and stays running. Workers spin up when work arrives and sleep after their idle timeout.

## Dependencies

When agents rely on each other, you don't want the worker spinning up before the coordinator it reports to is ready. Dependencies let you express that:

```toml
[[agent]]
name = "worker"
depends_on = ["mayor"]
```

The controller won't start a worker session until the mayor is running. If the mayor goes down and comes back, the dependency is satisfied again — it's about startup ordering, not a permanent link.

This matters most with named sessions. If your mayor is `mode = "always"` and your workers are `mode = "on_demand"`, the dependency ensures the mayor is up before any worker sessions get created. Without it, a worker could start, try to report to the mayor, and find nobody home.

## Putting it together: the Gastown mayor

Here's a real-world example that ties most of this chapter together — the mayor from the Gastown reference city:

```toml
[[agent]]
name = "mayor"
scope = "city"
work_dir = ".gc/agents/mayor"
prompt_template = "prompts/mayor.md.tmpl"
nudge = "Check mail and hook status, then act accordingly."
overlay_dir = "overlays/default"
idle_timeout = "1h"
install_agent_hooks = ["claude"]
session_live = [
    "scripts/tmux-theme.sh {{.Session}} {{.Agent}}",
    "scripts/tmux-keybindings.sh",
]

[[named_session]]
template = "mayor"
scope = "city"
mode = "always"
```

This is a city-scoped coordinator that works in an isolated directory, gets its prompt from a template, has an overlay copied into its workspace, sleeps after an hour of inactivity, gets Gas City hooks for mail and work delivery, has tmux theming applied on startup and config reload, and has a named session that keeps it always running. If it crashes, the controller restarts it. If it sleeps, any nudge or slung work wakes it back up.

## Command reference

| Command | What it does |
|---|---|
| `gc session list` | List sessions (filter with `--state`, `--template`) |
| `gc session suspend <id-or-alias>` | Manually pause a session |
| `gc session wake <id-or-alias>` | Wake a suspended or sleeping session |
| `gc session close <id-or-alias>` | Gracefully end a session |
| `gc session kill <id-or-alias>` | Force-terminate a session process |
| `gc session logs <id-or-alias>` | View conversation history |
| `gc session prune --before <duration>` | Clean up old suspended sessions |
| `gc session nudge <id> <msg> --delivery <mode>` | Send with delivery mode |
| `gc mail send <target> <subject> <body>` | Send mail to an agent |
| `gc mail check <target>` | Check for unread mail |
| `gc mail inbox <target>` | List messages |
| `gc agent suspend <name>` | Suspend an agent template |
| `gc agent resume <name>` | Resume a suspended agent template |

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
