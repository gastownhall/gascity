---
title: "Agent Pools & Autoscaling"
---

| Field | Value |
|---|---|
| Status | Implemented |
| Date | 2026-03-01 |
| Author(s) | Claude |
| Issue | — |
| Supersedes | — |

Design document for elastic agent pools in Gas City. Covers the full
picture: upscaling, downscaling, drain mode, and the ZFC-compliant
scaling signal mechanism.

## Problem

Today, `[[agent]]` defines a fixed set of always-on agents. Each is
either running or suspended. There is no concept of "start N copies of
a template based on demand" or "start only when work exists."

Concrete examples that today's model can't express:

1. **Ephemeral worker pool** — start at 0, cap at 10, scale based on
   the number of ready beads plus agents already working.
2. **Merge agent** — max of 1, start at 0, start only when beads with
   a `merge` label exist.

Both are: `desired_count = f(bead_store_state)`, bounded by min/max.

## Kubernetes parallel

| Kubernetes | Gas City | Notes |
|---|---|---|
| Pod | Agent session | Unit of compute |
| Deployment | Pool config in TOML | Template + desired count |
| HPA / KEDA | `check` shell command | Computes desired from observables |
| Metrics Server | Bead store (`bd` queries) | Observable state |
| Controller loop | Reconciler | Already exists (`doReconcileAgents`) |
| Scheduler | `runtime.Provider.Start` | No node selection needed |
| Graceful termination | Drain mode + prompt | Agent-aware, not just SIGTERM |
| preStop hook | Drain signal in prompt | Agent decides how to wrap up |
| terminationGracePeriodSeconds | `drain_timeout` | Hard deadline for draining |
| Pod disruption budget | `min` field | Never go below this count |

**What Gas City doesn't need from Kubernetes:** node scheduling, resource
requests/limits, affinity/anti-affinity, multi-node networking, rolling
update strategy, service mesh. Scale is 0-10, not 0-10000.

**What Gas City does better:** Kubernetes doesn't understand "current
task" — it just sends SIGTERM and hopes. Gas City has structured work
(beads, molecules, steps), so it can say "finish your current bead"
instead of "you have 30 seconds to die."

## Config shape

Pool config is now merged into `[[agent]]` via an optional `[agent.pool]`
sub-table. Every agent is implicitly a pool of size 1. Explicit `[agent.pool]`
overrides the defaults.

### Fixed agent (implicit pool: min=1, max=1)

```toml
[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
```

### Ephemeral singleton (starts only when work exists)

```toml
[[agent]]
name = "refinery"
prompt_template = "prompts/refinery.md"

[agent.pool]
min = 0
max = 1
check = "bd list --label=needs-merge --json | jq length"
```

### Elastic pool (max>1 -> gets -1, -2, ... suffixes)

```toml
[[agent]]
name = "worker"
provider = "claude"
prompt_template = "prompts/worker.md"

[agent.pool]
min = 0
max = 10
check = "bd ready --json | jq length"
```

### Key rules

- No `[agent.pool]` → implicit min=1, max=1, check="echo 1" (always-on)
- `[agent.pool]` present → defaults: max=1, check="echo 1", min=0
- If max == 1: bare name (no `-1` suffix)
- If max > 1: instances are `{name}-1`, `{name}-2`, etc.

### Relationship to old `[[pools]]`

The separate `[[pools]]` top-level section has been removed. All pool
configuration now lives on `[[agent]]` entries via `[agent.pool]`.
An `[[agent]]` entry without `[agent.pool]` is a fixed, always-on
agent. The pool manager evaluates `check` and produces a desired agent
list; the reconciler makes reality match.

## Scaling signal: `check`

Following the order gate `condition` pattern (§16.2), the scaling
signal is a **user-supplied shell command** (field name: `check`):

- Go runs the command via `sh -c`
- Reads stdout as an integer (the desired count)
- Clamps to `[min, max]`
- Starts or drains agents to match

This is ZFC-compliant: all policy is in the shell command (user-supplied
config), Go is the state machine that executes it. No judgment calls in
Go code.

### Examples

```toml
# Scale on queue depth
check = "bd ready --json | jq length"

# Scale on labeled beads
check = "bd list --label merge --status ready --json | jq length"

# Custom logic: your script, your policy
check = "/path/to/my-scaler.sh"

# Combined: ready + working = total demand
check = "echo $(( $(bd ready --json | jq length) + $(bd list --status hooked --json | jq length) ))"
```

### ZFC analysis

| Concern | Where | ZFC role |
|---------|-------|----------|
| min/max bounds | TOML config | User-supplied policy |
| Scaling signal | Shell command in config | User-supplied policy |
| "Finish current work" | Prompt template | Agent cognition |
| drain_timeout | TOML config | User-supplied policy |
| Start/stop sessions | Go state machine | Deterministic transport |
| Clamp desired to [min,max] | Go code | Arithmetic (transport) |
| Re-queue hooked beads | Go code | Deterministic safety |

The spec explicitly allows:
- **Deterministic infrastructure operations** in Go (concepts.md:
  "Some infrastructure operations must be deterministic to be safe")
- **Config-driven thresholds** read by Go (health patrol pattern)
- **Shell command execution** for user-supplied policy (order gate
  `condition` pattern, §16.2)
- **Lifecycle handlers** as shell commands (§19)

## Agent identity

Pool agents are named `{pool}-{n}`: `worker-1`, `worker-2`, `merger-1`.
Session names follow the existing pattern: `gc-{city}-{pool}-{n}`.

Gastown's themed name pools ("Toast", "Furiosa") are cosmetic and can
be added later via the `theme` field in PoolConfig. For now, numeric
suffixes are simple, predictable, and debuggable.

## Upscaling

The simple case. The reconciler evaluates `check`, computes
desired count, starts new agents if `desired > current`.

```
1. Run check command → get raw desired count
2. Clamp to [min, max]
3. Count currently running pool agents
4. If desired > current: start (desired - current) new agents
5. Each new agent gets: name, session, prompt, env, hook
```

New agents immediately enter the GUPP loop: check hook, claim work,
execute, repeat. The prompt template tells them what to do. No framework
intelligence needed.

## Downscaling (full design — implement later)

Three agent lifecycle states:

```
active  →  draining  →  stopped
```

**Active:** normal operation. Claim beads, run formulas.
**Draining:** finish current bead, do NOT claim new work. Stop when idle.
**Stopped:** session terminated.

### The drain signal

When the scaler determines `desired < current`, it picks agents to drain
(least recently active first) and writes a drain signal:

- A file: `.gc/agents/{name}.drain`
- Or an env var set via tmux: `GC_DRAIN=1`

The agent's prompt template includes:

```
{{if .Draining}}
You are being drained. Finish your current task, push your work,
run `gc done`, and exit. Do NOT claim new beads.
{{end}}
```

This is ZFC: the agent decides how to wrap up (cognition). Go just
flips the flag (transport).

### The hard deadline

If an agent ignores the drain signal or a formula runs too long:

1. `drain_timeout` expires (default 15m)
2. Go force-kills the session
3. Any hooked bead is re-queued (unhook without close)
4. Another agent picks it up (NDI — work survives sessions)

### Scale-down sequence

```
1. Scaler evaluates: desired=3, current=5, need to drain 2
2. Pick 2 agents to drain (longest idle or least recently active)
3. Write drain signal for each
4. Nudge each agent: "You are being drained"
5. Start drain_timeout timer
6. Agent finishes current bead → runs gc done → session exits
7. Controller detects exit → cleans up
8. If drain_timeout expires → force-kill → re-queue hooked bead
```

### Polecats as a simplification

For polecats (ephemeral task workers), downscaling is even simpler:
polecat prompts already exit after finishing their hooked work. When the
scaler determines fewer agents are needed, it simply doesn't start new
ones. Existing polecats finish their current bead and exit naturally.

This means **upscale-only is sufficient for the first tutorial** — the
prompt handles the "stop when done" behavior.

## Reconciler changes

The existing `doReconcileAgents` handles `[]agent.Agent`. For pools, the
flow becomes:

```
1. For each [[agent]] entry: reconcile as today (fixed agents)
2. For each [[pools]] entry:
   a. Run check → get desired count
   b. Clamp to [min, max]
   c. Count currently running agents with this pool prefix
   d. If desired > current: create new agent.Agent entries, start them
   e. If desired < current: drain excess agents (future)
3. Orphan cleanup: stop sessions not in any desired set
```

The pool manager produces `[]agent.Agent` entries dynamically. The
reconciler doesn't need to know whether an agent came from `[[agent]]`
or `[[pools]]` — it just starts/stops sessions.

## Controller loop

Today `doReconcileAgents` runs once at `gc start`. For autoscaling, the
controller needs a persistent loop:

```
loop:
  for each pool:
    desired = clamp(evaluate_check(pool), pool.min, pool.max)
    current = count_running(pool)
    if desired > current: start (desired - current) agents
    if desired < current: drain excess (future)
  sleep scale_interval
```

This loop lives in the controller (the process holding
`controller.lock`). For the tutorial, a simpler approach works: run the
scaling check once at `gc start` and let polecats self-terminate.

## Implementation order

### Phase 1: Tutorial 08 — upscale only

Minimum viable pools:
- Parse `[[pools]]` from city.toml
- Evaluate `check` shell command
- Start agents up to `min(desired, max)`
- Polecat prompts exit naturally after finishing work
- No drain mode, no controller loop, no idle_timeout

### Phase 2: Controller loop

- Persistent scaling loop in the controller
- Periodic re-evaluation of check
- Dynamic start of new agents as work arrives

### Phase 3: Downscaling

- Drain mode (signal + prompt template)
- drain_timeout with force-kill
- Re-queue hooked beads on force-kill
- idle_timeout for natural scale-down

### Phase 4: Polish

- Themed name pools (Gastown polecat names)
- Scale-down agent selection (least-recently-active)
- Scale-up/down cooldowns (prevent flapping)
- Event recording for scale events
