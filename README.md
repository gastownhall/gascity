# Gas City

You hit the wall at agent number three.

The first agent was magic. The second doubled your throughput. Then you added a third and everything collapsed — session conflicts, lost work, agents overwriting each other, no way to know what's running or why it stalled. You started writing glue code. Then more glue code. Then you realized the glue code *was* the project.

What if all that plumbing was already solved — and the only thing you had to write was a config file?

## What Gas City Is

Gas City is an **orchestration-builder SDK** for multi-agent AI systems. It gives you five infrastructure primitives, four composable mechanisms, and zero hardcoded roles. You define agents, packs, and behaviors in TOML. Gas City handles sessions, work tracking, communication, health monitoring, and scaling. The SDK is the plumbing between the model and the work — it doesn't care which model, which platform, or how many agents you run.

```toml
# 01-hello.toml — the simplest possible city

[workspace]
name = "my-city"

[[agents]]
name = "mayor"
prompt_template = "prompts/mayor.md"

[beads]
provider = "bd"
```

Fifteen lines. One agent. Full lifecycle management — session creation, work tracking, crash recovery, declarative reconciliation. Now scale to 200.

## Origin Story: Gas Town, Unplugged

Gas City was extracted from [Gas Town](https://github.com/steveyegge/gastown), Steve Yegge's multi-agent coding orchestrator. Gas Town runs 8+ specialized agents across multiple repos in production — polecats writing code, witnesses monitoring health, a refinery merging PRs, a deacon patrolling infrastructure, a mayor coordinating everything. It works. It has been working, through months of multi-agent production use.

But Gas Town's roles are hardwired in Go. The mayor is Go code. The witness is Go code. Every new role means new Go code. Steve realized that the primitives Gas Town had battle-tested — session management, work assignment, health patrol, communication, formulas — were powerful enough to express *any* orchestration pack. The roles weren't special. They were just configuration.

Gas City extracts those primitives into a standalone SDK where **Gas Town becomes one configuration among many**. You can build Gas Town in Gas City, or Claude Code Agent Teams, or a security audit pipeline, or a documentation factory, or something nobody has imagined yet — all from the same five primitives.

The success criterion: `gc topo start gastown` reads a set of role definitions and produces a working Gas Town — Witness, Refinery, Polecats, Mayor, Deacon, all of it — indistinguishable from the hardcoded version. Then someone else builds a completely different orchestrator from the same SDK. That proves it's general.

```
The Stack

Dolt (Tim Sehn — SQL database with git semantics)
  └── Beads (Steve Yegge — work tracking, identity, the ledger)
        └── Gas City (orchestration SDK — sessions, patrol, sling, config)
              ├── Gas Town (reference configuration — 8 roles, full orchestration)
              └── Your Thing (your pack, your roles, your rules)
```

Each layer is an independent project with independent ownership:

| Layer | Owner | What it provides |
|-------|-------|-----------------|
| **Dolt** | Tim Sehn / DoltHub | SQL database with git semantics — branches, merge, history, federation |
| **Beads** | Steve Yegge | Work tracking, mail, molecules, identity, the ledger — built on Dolt |
| **Gas City** | Community | Session management, patrol, sling, sandbox, daemon, role parser — built on Beads |
| **Gas Town** | Steve Yegge | The reference orchestrator — Gas City configured with standard roles |

## Show, Don't Tell

### One agent, one task

```toml
# The simplest city: one agent does work.
[workspace]
name = "my-city"

[[agents]]
name = "mayor"
prompt_template = "prompts/mayor.md"

[beads]
provider = "bd"
```

Create a bead with `bd create "Fix the login bug"`. The mayor claims it, works it, closes it. That's Level 1.

### Elastic pool scaling

```toml
# Workers scale based on queue depth.
# check returns desired count, clamped to [min, max].

[[agents]]
name = "worker"
prompt_template = "prompts/pool-worker.md"

[agents.pool]
min = 0
max = 5
check = "bd ready --unassigned --limit 0 --json | jq length"
```

No work in the queue? Zero workers. Five beads pile up? Five workers spin up, each in its own session with its own context. Work drains, workers stop. The `check` command is a shell command — it can query anything. Queue depth, CPU load, time of day, a coinflip. Gas City doesn't care what it returns. It cares that it returns a number.

### Provider swap: tmux to Kubernetes

Same city, different infrastructure. Change the provider, change where agents run:

```toml
# Local development — agents run in tmux sessions
[workspace]
name = "my-city"
provider = "tmux"
```

```toml
# Production — agents run in Kubernetes pods
[workspace]
name = "my-city"
provider = "k8s"
```

Two lines changed. Same agents, same prompts, same formulas, same work tracking. The `session.Provider` interface abstracts the runtime — tmux sessions, K8s pods, subprocesses, or a custom script that talks to your own infrastructure. Your city.toml doesn't know and doesn't care.

### Multi-project isolation with rigs

```toml
# Each rig gets its own .beads/ database, its own prefix, its own agents.

[[agents]]
name = "fe-worker"
dir = "projects/frontend"
prompt_template = "prompts/scoped-worker.md"

[[agents]]
name = "be-worker"
dir = "projects/backend"
prompt_template = "prompts/scoped-worker.md"

[[rigs]]
name = "frontend"
path = "/home/user/projects/my-frontend"

[[rigs]]
name = "backend"
path = "/home/user/projects/my-backend"
```

Two repos. Two agents. Isolated bead stores. Cross-rig references via routing. Agents can't step on each other's files.

### Full Gas Town pack

```toml
# A rig uses the gastown pack — all 8 roles, full orchestration.

[[rigs]]
name = "my-project"
path = "/home/user/my-project"
pack = "packs/gastown"
```

One line brings in witness, refinery, polecats, the entire Gas Town agent complement — configured, health-monitored, auto-scaled. The pack is a directory of agent definitions and prompt templates. Gas Town, packaged as a reusable pack that you can stamp onto any rig.

## Architecture: The Nine Concepts

Gas City has five irreducible primitives and four derived mechanisms. Removing any primitive makes it impossible to rebuild Gas Town. Every mechanism is provably composable from the primitives.

```
┌─────────────────────────────────────────────────────────┐
│  Layer 4: Dispatch / Coordination                       │
│           Sling + Convoys + Health Patrol                │
├─────────────────────────────────────────────────────────┤
│  Layer 3: Workflow Engine                               │
│           Formulas + Molecules + Plugins                 │
├─────────────────────────────────────────────────────────┤
│  Layer 2: Messaging                                     │
│           Mail + Nudge + Protocol Messages               │
├─────────────────────────────────────────────────────────┤
│  Layer 1: Rich Semantics                                │
│           Hooks, dependencies, labels, pools, templates  │
├─────────────────────────────────────────────────────────┤
│  Layer 0: Core Interfaces                               │
│           Agent Protocol · Task Store · Event Bus        │
│           Config · Prompt Templates                      │
└─────────────────────────────────────────────────────────┘
```

**Five invariants:**

1. No layer may import from a higher layer
2. Each layer exposes a clean API consumed by the layer above
3. Layer 0 has zero Gas City-specific logic (pure interfaces)
4. Removing any layer above 0 leaves the layers below fully functional
5. Side effects (I/O, process spawning) are confined to Layer 0 implementations

### Five Primitives

These are the irreducible core. Everything else is built from them.

| # | Primitive | What it does | If you remove it... |
|---|-----------|-------------|---------------------|
| 1 | **Agent Protocol** | Start, stop, prompt, observe, and health-check agents regardless of provider. Identity, pools, sandboxes, crash adoption. | Can't run agents. Nothing works. |
| 2 | **Task Store (Beads)** | CRUD + Hook + Dependencies + Labels + Query over work units. Everything is a bead: tasks, mail, molecules, convoys. | Can't track work. Agents run in a void. |
| 3 | **Event Bus** | Append-only pub/sub log of all system activity. Two tiers: critical (bounded queue) and optional (fire-and-forget). | Can't observe or self-heal. System goes blind. |
| 4 | **Config** | TOML parsing with progressive activation (Levels 0-8 from section presence) and multi-layer override resolution. | Roles are hardcoded. You have an app, not an SDK. |
| 5 | **Prompt Templates** | Go `text/template` in Markdown defining what each role does. The behavioral specification. | Agents start but have no instructions. |

The five primitives form a DAG with no circular dependencies. Agent Protocol and Event Bus depend on nothing. Task Store depends on Config and Event Bus. Config depends on nothing. Prompt Templates depend on Config.

### Four Mechanisms

Built from primitives. They provide real capability but introduce no new irreducible ideas.

| # | Mechanism | Built from | What it enables |
|---|-----------|-----------|-----------------|
| 6 | **Messaging** | Task Store + Agent Protocol + Config | Inter-agent communication: mail (async, persistent) + nudge (sync, immediate) |
| 7 | **Formulas & Molecules** | Config + Task Store + Prompt Templates | Reusable multi-step workflow DAGs. Formula (static TOML) → Molecule (instantiated beads). |
| 8 | **Dispatch (Sling)** | All primitives + Mechanisms 6-7 | Single-command work assignment: find/spawn agent → select formula → create molecule → hook → nudge → log |
| 9 | **Health Patrol** | Agent Protocol + Event Bus + Config + Task Store | Self-healing: ping agents, detect stalls, restart with backoff, re-hook work |

**Derivation proofs:**

- `Mail.Send(to, msg)` = `TaskStore.Create(bead{type: "message", to: to, body: msg})`
- `Nudge(agent, text)` = `AgentProtocol.SendPrompt(handle, text)`
- `Sling(issue, rig)` = `Pool.GetOrSpawn()` + `Formula.Bond(issue)` + `TaskStore.Hook(agent, mol)` + `Nudge(agent, "work assigned")` + `EventBus.Publish("sling", ...)`

Every mechanism is a function over the five primitives. No mechanism requires a sixth.

### How the Primitives Compose

The simplest flow — one agent, one task:

```
User                    TaskStore           Agent Protocol
  │                        │                      │
  ├─ bd create "fix bug" ──►                      │
  │                  [bead created]                │
  │                        ├── Hook(agent, bead) ──►
  │                  [agent claims work]           │
  │                        │    [agent executes]   │
  │                        ◄── Close(bead) ───────┤
  │                  [bead closed]                 │
```

The self-healing flow — patrol detects a stall and recovers:

```
Health Patrol      Agent Protocol       TaskStore         Event Bus
     │                  │                  │                 │
     ├─── Ping(agent) ──►                  │                 │
     │             [no response]           │                 │
     ├─── Ping(agent) ──►                  │                 │
     │             [timeout × 3]           │                 │
     ├──────────────────┼──────────────────┼── Publish ─────►│
     │                  │                  │          [stall event]
     ├─── Restart ──────►                  │                 │
     │             [new session]           │                 │
     │                  ├── GetHook ───────►                 │
     │                  │            [work still hooked]     │
     │                  ├── SendPrompt ────►                 │
     │                  │            [resume work]           │
     ├──────────────────┼──────────────────┼── Publish ─────►│
     │                  │                  │        [recovery event]
```

The agent died. The work didn't. A fresh session picks up where the last one left off.

## Key Capabilities

### Declarative Reconciliation

Gas City uses a controller loop. You edit `city.toml`, save, and the controller converges the running system to match. Add an agent? It starts. Remove one? It stops. Change a prompt template? The agent restarts with the new instructions.

```
Desired state (city.toml)  ──→  Controller  ──→  Actual state (running sessions)
         │                          │
         └── edit, save ───→ fsnotify ───→ reconcile ───→ converged
```

The controller watches config files, debounces changes (200ms coalesce window), and maintains last-known-good config on parse failure. If your editor half-writes a file, the controller waits. If the new config is invalid, the old config keeps running and the error is logged. This is the same pattern as Kubernetes controllers — declare what you want, let the system figure out how to get there.

Config fingerprinting drives intelligent restarts. The fingerprint is a SHA256 of the fully-resolved agent spec — command, args, env, pool config, isolation, provider, prompt content. Change anything that affects agent behavior, and Gas City restarts the agent. Change only observation hints (ready delay, process names), and it doesn't. K8s famously does *not* auto-roll pods when a ConfigMap changes. Gas City learned from that mistake.

### Provider Swapping

The `session.Provider` interface decouples agent lifecycle from infrastructure:

```go
type Provider interface {
    Start(name string, cfg Config) error    // create a new session
    Stop(name string) error                 // destroy session (idempotent)
    IsRunning(name string) bool             // liveness check
    Attach(name string) error               // connect user's terminal
    Nudge(name, message string) error       // wake or redirect agent
    Peek(name string, lines int) (string, error)  // capture recent output
    ProcessAlive(name string, processNames []string) bool  // deep health check
    SendKeys(name string, keys ...string) error  // raw key events
    // ... plus metadata, scrollback, file staging
}
```

Four implementations ship today:

| Provider | What it does | When to use it |
|----------|-------------|---------------|
| **tmux** | Local terminal multiplexer sessions | Development, single-machine deployments |
| **k8s** | Native Kubernetes pods via client-go | Production, multi-node clusters |
| **subprocess** | Local child processes with stdin/stdout | Testing, simple single-process agents |
| **exec** | Delegates to a custom shell script | Anything else — SSH, Docker, your cloud API |

The exec provider is the escape hatch. Write a script that understands `start`, `stop`, `is-running`, `nudge`, and `peek`. Gas City calls it as a shell command with JSON arguments. Your infrastructure, your rules. When platforms eventually provide native agent lifecycle APIs, the exec provider wraps them — the city.toml doesn't change.

Each primitive has pluggable backends:

| Primitive | Default | Alternatives |
|-----------|---------|-------------|
| Agent Protocol | tmux (claude, codex, gemini) | K8s pods, subprocess, exec script |
| Task Store | Beads (Dolt/SQLite) | Filesystem (JSONL files) |
| Event Bus | JSONL + flock | Extensible to Redis, NATS, etc. |
| Config | TOML parser | Standard, single implementation |
| Prompt Templates | Go `text/template` | Standard, single implementation |

**The pluggability invariant:** Swapping any implementation preserves all derived mechanisms. Replace Dolt with SQLite — mail still works (it's still beads). Replace tmux with K8s pods — nudge still works (it's still `SendPrompt`). The mechanisms are defined in terms of interfaces, not implementations.

### Pool Scaling

Agents can be fixed (always one running) or elastic (scale between min and max based on a check command):

```toml
[[agents]]
name = "worker"
prompt_template = "prompts/pool-worker.md"

[agents.pool]
min = 0
max = 5
check = "bd ready --unassigned --limit 0 --json | jq length"
```

The `check` command runs on a patrol interval. It returns a number — the desired agent count. Gas City clamps it to `[min, max]` and reconciles: if you need 3 workers and only 1 is running, 2 more start. If you need 0 and 3 are running, they drain and stop.

Pool workers get suffixed names (`worker-1`, `worker-2`, ...) and independent sessions. Each runs the same prompt template with its own context. The check command is an arbitrary shell command — query your bead queue, call an API, read a file, run a calculation. Gas City evaluates the result; it doesn't evaluate the logic.

### Self-Healing

Gas City's health patrol follows the Erlang/OTP supervision model:

| Erlang/OTP | Gas City |
|---|---|
| Supervisor | Controller / Health Patrol |
| Worker | Agent (any role) |
| Child spec | `[[agents]]` with health config |
| one_for_one restart | Restart dead agent only |
| max_restarts / max_seconds | `max_restarts_per_window` / `restart_window` |
| Links (death propagates) | `depends_on` (shutdown sequencing) |
| "Let it crash" | GUPP + beads: agent dies, hook persists, fresh session resumes |
| Process mailbox | Mail inbox (beads with type=message) |
| GenServer loop | Agent loop: check hook, execute, close, repeat |

When an agent stalls, the health patrol detects it (ping timeout), publishes a stall event, restarts the session, re-hooks the work, and nudges the fresh agent. The work is still there — beads persist across sessions. The agent picks up where it left off. This is "let it crash" for AI agents.

Circuit breaker semantics prevent restart storms: `max_restarts_per_window` limits how many times an agent can restart within `restart_window`. Hit the limit and the agent stays down until the window expires or a human intervenes. Same pattern as Erlang's `max_restarts` / `max_seconds`.

**Deterministic shutdown:** Agent shutdown uses Go state machines, not AI agents. You don't want an AI trying to shut down another AI. Infrastructure operations are mechanical, not intelligent.

### Composable Packs

A pack is a directory containing agent definitions and prompt templates — a reusable package of agents that can be stamped onto any rig:

```
packs/gastown/
    pack.toml           # metadata + agent definitions
    prompts/
        witness.md.tmpl
        refinery.md.tmpl
        polecat.md.tmpl
```

```toml
# packs/gastown/pack.toml

[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[[agents]]
name = "witness"
prompt_template = "prompts/witness.md.tmpl"

[[agents]]
name = "refinery"
isolation = "worktree"
prompt_template = "prompts/refinery.md.tmpl"

[[agents]]
name = "polecat"
isolation = "worktree"
prompt_template = "prompts/polecat.md.tmpl"
[agents.pool]
min = 0
max = 3
```

Import it per rig with overrides:

```toml
[[rigs]]
name = "my-project"
path = "/home/user/my-project"
pack = "packs/gastown"

# Override: more polecats for this rig
[[rigs.overrides]]
agent = "polecat"
pool = { max = 10 }

# Override: skip refinery on this rig
[[rigs.overrides]]
agent = "refinery"
suspended = true
```

This is the Kubernetes Kustomize pattern applied to agent orchestration. Base pack + per-rig patches. No Go-template-in-TOML. Explicit, reviewable overrides. You can always answer "what config does polecat on my-project get?" by reading two files — or running `gc config explain --rig my-project --agent polecat`.

Config composition supports three operations:

| Operation | Mechanism | Example |
|-----------|-----------|---------|
| **Add** | Array concatenation via `include` | Fragment adds new agents/rigs |
| **Patch** | Keyed `[[patches]]` blocks | Override pool.max on one agent |
| **Suspend** | `suspended = true` | Skip refinery on small rigs |

Overrides use sub-field patching: `pool = { max = 10 }` changes only `pool.max`; `pool.min` retains the pack's default. This is Kustomize, not Helm. Explicit patches beat template magic.

### The Exec Escape Hatch

Every integration point in Gas City is a shell command:

| Integration point | How it works |
|------------------|-------------|
| Pool check | Shell command returns desired agent count |
| Session provider (exec) | Shell script handles start/stop/nudge/peek |
| Pre-start hooks | Shell commands run before session creation |
| Session setup | Shell commands run after session creation |
| Session setup script | Script path receiving context via env vars |
| Formulas | Step prompts rendered from Go templates |

If it has a CLI, Gas City can use it. No plugin API, no adapter SDK, no registration protocol. Write a shell command, reference it in config. This is deliberate — shell commands are the universal integration layer. They work today, they'll work tomorrow, and they don't require Gas City to understand your infrastructure.

The exec session provider takes this to its extreme: every Provider method becomes a shell command invocation. `start` gets a JSON payload with the session config. `is-running` returns exit code 0 or 1. You can wrap SSH, Docker, AWS ECS, or anything else in a script and Gas City treats it as a first-class provider.

## Progressive Capability Model

Capabilities activate based on which config sections are present. Each level is independently useful — you don't need Level 8 to get value from Level 1.

| Level | Config adds | What you get |
|-------|------------|-------------|
| 0-1 | `[workspace]` + `[[agents]]` + `[beads]` | One agent doing work with tracked beads |
| 2 | `[agents.loop]` | Agent continuously polls for ready beads, clean context per task |
| 3 | Multiple `[[agents]]` + `[agents.pool]` | Agent teams with elastic scaling |
| 4 | Messaging config | Inter-agent mail and nudge |
| 5 | `[formulas]` | Structured multi-step workflow DAGs |
| 6 | Health monitoring config | Ping, stall detection, auto-restart |
| 7 | Automation/plugin config | Gate-conditioned automations (cron, event, cooldown) |
| 8 | Full pack + all mechanisms | Complete orchestration: Gas Town in a config file |

**Every level independently useful.** A Level 1 city runs real work, not a demo stub. A Level 3 city manages a team of workers. A Level 8 city is a full Gas Town deployment. The SDK is constant; the config grows.

The progression is a strict superset chain:

```
hello-world = {Agent Protocol, Task Store, Config}
ralph       = hello-world + {Config: agents.loop}
ccat        = ralph + {Event Bus, Templates, Messaging, Dispatch}
gastown     = ccat + {Formulas, Health Patrol, Plugins}
```

No configuration requires a concept outside the 5+4 set. No configuration requires a concept that can't be derived from the five primitives.

## The Bitter Lesson

Gas City's design is informed by [the bitter lesson](http://www.incompleteideas.net/IncsightBlurb/The%20Bitter%20Lesson.html) (Sutton, 2019): general methods that leverage computation scale better than methods that encode human knowledge.

**Three design tests every primitive must pass:**

1. **Atomicity** — Is this concept irreducible? Can it be expressed as a combination of other primitives? If yes, it's derived, not core.
2. **Bitter Lesson** — Will this become MORE useful as models improve, or LESS? Session management scales with agent count. Decision trees don't survive model upgrades.
3. **Zero Framework Cognition (ZFC)** — Does any line of Go contain a judgment call? `if stuck then restart` is framework intelligence. Move the decision to the prompt.

**Explicit exclusions — permanent, not "not yet":**

| Not in Gas City | Why |
|----------------|-----|
| Skill system | The model IS the skill system. A skills primitive is a bet against model improvement. |
| Capability flags | A sentence in the prompt is sufficient. "You can tear down sessions. You cannot merge code." |
| MCP / tool registration | If a tool has a CLI, the agent uses it. Machine interfaces for human tools won't age well. |
| Decision logic in Go | The agent decides from its prompt and observed reality. |
| Hardcoded role names | If a line of Go references a specific role name, it's a bug. |

Everything that survived is plumbing. Everything that was cut was a bet against model improvement. Gas City ages better as models get smarter — more agents need more sessions, more work assignment, more health patrol, more communication. None of that goes away.

The things we excluded? Each one becomes *less* necessary with every model generation. Skills? The model already knows procedures — and tomorrow's model knows more. Capability flags? A sentence in the prompt is sufficient today and even more sufficient tomorrow. MCP tool registration? Models are already using human interfaces directly. The only things that belong in an orchestration SDK are things that are NOT intelligence: session plumbing, work assignment, communication transport, and the declarative definitions that compose them.

## Wasteland: The Federation Layer

Gas City's packs are local — one city, one machine (or cluster). The Wasteland is what happens when cities connect.

Built on Dolt's federation protocol, the Wasteland enables cross-organization agent marketplaces where cities publish available capacity and consume work from each other. Dolt's git-like push/pull/merge semantics make this natural: beads already federate, cities already have declarative packs, and the coordination protocol is just push/pull over Dolt remotes.

The proof of concept is the `wasteland-feeder` pack: a city that watches a Dolt remote for incoming work, dispatches it to local agents, and pushes results back. It's Gas City as a node in a decentralized compute network — each city autonomous, the federation emergent. No central coordinator. No marketplace server. Just Dolt remotes and cities that know how to pull work and push results.

## The Kubernetes Parallel

The mapping between Kubernetes and Gas City is surprisingly tight:

| Kubernetes | Gas City | Notes |
|-----------|----------|-------|
| Pod | Agent session | Smallest schedulable unit |
| Deployment | Agent + pool config | Declares desired replicas |
| ReplicaSet | Pool instances | Maintains N copies |
| Service | Session name | How agents address each other |
| ConfigMap | Prompt template | Injected config that shapes behavior |
| Namespace | Rig | Scoping / isolation boundary |
| Node | Rig path | Physical location where work runs |
| Controller loop | `gc start --controller` | Reconcile desired state to actual state |
| etcd | Beads store | Persistent state |
| kube-apiserver | controller.sock + city.toml | Declared desired state |
| Helm chart | Pack directory | Reusable, versionable, overridable package |
| Kustomize | `[[rigs.overrides]]` | Patch without templates |

Kubernetes solved container orchestration with a declarative model: you describe what you want, a controller loop makes it so. Gas City applies the same pattern to agent orchestration. The config is your desired state. The controller reconciles. The provider abstracts the infrastructure. Packs are your Helm charts.

K8s solved config composition three times: multi-file apply, Kustomize, Helm. Gas City learns from all three. Kustomize-style explicit patches for overrides. Helm-style packaged packs for reuse. And the critical lesson Helm taught by negative example: no Go-template-in-TOML. Ever.

## Configured Roles (Not Concepts)

In Gas Town, the mayor, witness, deacon, refinery, and polecats look like fundamental concepts. In Gas City, they're all just `[[agents]]` entries with different config:

| Gas Town Role | Gas City Config |
|---------------|----------------|
| Mayor | Workspace-scoped agent with a coordinator role template |
| Deacon | Workspace-scoped agent with a patrol role template |
| Witness | Rig-scoped agent with a lifecycle-monitor role template |
| Refinery | Rig-scoped agent with a merge-processor role template, `isolation = "worktree"` |
| Polecat | Ephemeral pool-managed agent with a worker role template, `isolation = "worktree"` |
| Crew | Persistent agent with `isolation = "none"` |
| Boot | Ephemeral agent spawned for health triage |

The SDK doesn't know about any of them. It knows about agents, pools, sessions, templates, and beads. The roles emerge from configuration, not code. That's the whole point.

## Getting Started

```bash
# Initialize a new city
gc init my-city
cd my-city

# Add a rig (a project your agents will work on)
gc rig add my-project /path/to/your/repo

# Start the controller (reconciles desired → actual state)
gc start

# Create a bead (unit of work)
bd create "Fix the login bug"

# Attach to an agent's session to watch it work
gc agent attach mayor
```

See [Tutorial 01: Hello, Gas City](docs/tutorials/01-hello-gas-city.md) for the complete walkthrough.

## Example Packs

| Pack | Agents | What it does |
|----------|--------|-------------|
| **hello-world** | 1 mayor | Single agent, single task — the starting point |
| **ralph** | 1 agent + loop | Continuously polls for beads, clean context per task |
| **ccat** | Coordinator + N workers | Claude Code Agent Teams — parallel work dispatch |
| **gastown** | 8 roles (mayor, deacon, witness, refinery, polecats, crew, boot, dog) | Full Gas Town orchestration — health patrol, formulas, self-healing |
| **wasteland-feeder** | Feeder + workers | Cross-city work federation on Dolt |

Each pack is a set of TOML files and prompt templates. The infrastructure is the same. Only the roles and coordination shape change.

## Example Configs (Progressive)

The `examples/configs/` directory shows the progressive capability model in action:

| Config | Level | What it demonstrates |
|--------|-------|---------------------|
| `01-hello.toml` | 1 | Single agent, single task |
| `02-named-crew.toml` | 1 | Multiple named agents |
| `03-named-crew-loop.toml` | 2 | Agent loop — clean context per bead |
| `04-agent-team.toml` | 3 | Mayor + multiple workers |
| `06-formulas.toml` | 5 | Structured multi-step workflows |
| `08-agent-pools.toml` | 3 | Elastic pool scaling with check command |
| `09-scoped-dirs.toml` | 3 | Per-agent working directories |
| `10-rigs.toml` | 3 | Multi-project isolation with rigs |

## Design Principles

### Zero Framework Cognition (ZFC)

Go handles transport, not reasoning. If a line of Go contains a judgment call, it's a violation. The ZFC test: does any Go code say `if stuck then restart`? That's framework intelligence. Move the decision to the prompt. The agent reads its instructions and observes reality. Go moves bytes and manages processes.

### GUPP — "If you find work on your hook, YOU RUN IT"

No confirmation, no waiting. The hook having work IS the assignment. This principle is rendered into agent prompts via templates, not enforced by Go code. It's the propulsion system that makes agents autonomous rather than passive.

### Nondeterministic Idempotence (NDI)

The system converges to correct outcomes because work (beads), hooks, and molecules are all persistent. Sessions come and go; the work survives. Multiple independent observers check the same state idempotently. Redundancy is the reliability mechanism — not locks, not coordination, not consensus. Three different agents can check whether a convoy is complete. All three checks are idempotent. Running them multiple times is safe. This is NDI in practice.

### SDK Self-Sufficiency

Every infrastructure operation (gate evaluation, health patrol, bead lifecycle, automation dispatch) must function with only the controller running. No SDK operation may depend on a specific user-configured agent role existing. Test: if removing an `[[agents]]` entry breaks an SDK feature, it's a violation. The controller drives infrastructure; user agents execute work.

## City Directory Layout

```
~/my-city/
├── city.toml                   # config — grows with capabilities
├── .gc/                        # runtime state
│   ├── controller.lock         # flock — one controller per city
│   ├── controller.sock         # unix socket for CLI ↔ controller
│   ├── agents/                 # agent registry
│   │   └── mayor.json
│   └── events.jsonl            # event log
├── prompts/                    # prompt templates (Markdown)
│   └── mayor.md
├── packs/                 # reusable agent packages
│   └── gastown/
│       ├── pack.toml
│       └── prompts/
└── rigs/
    └── my-project/
        ├── rig.toml
        └── .beads/
            └── beads.jsonl     # bead storage
```

## License

MIT

---

[![codecov](https://codecov.io/gh/julianknutsen/gascity/graph/badge.svg?token=LF6IU2AIO2)](https://codecov.io/gh/julianknutsen/gascity)
