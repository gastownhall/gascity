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
picture: upscaling, downscaling, drain mode, and the scaling signal
mechanism that keeps judgment out of Go.

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

Following the order trigger `condition` pattern (§16.2), the scaling
signal is a **user-supplied shell command** (field name: `check`):

- Go runs the command via `sh -c`
- Reads stdout as an integer (the desired count)
- Clamps to `[min, max]`
- Starts or drains agents to match

This keeps judgment out of Go: all policy is in the shell command
(user-supplied config), Go is the state machine that executes it. No
judgment calls in Go code.

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

### Keeping judgment out of Go

| Concern | Where | Role |
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
- **Shell command execution** for user-supplied policy (order trigger
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

New agents immediately enter the work loop: if you find work on your
hook, you run it — check hook, claim work, execute, repeat. The prompt
template tells them what to do. No framework intelligence needed.

## Failed starts: backoff, then park

A routed bead whose sessions cannot start is not capacity demand forever.
Every start the pool plans for a bead that then fails in `provider_error`
(a failing `pre_start` included) is charged to the **work bead**, not the
session bead: the session bead is rolled back and closed `failed-create`
and the next tick would create a fresh one with zeroed counters, which is
how one bead once spawned ~160 sessions in 23 minutes (papercut
pc_b969af2a45eb). The record lives in the bead's metadata, so it survives
a supervisor restart and a handoff:

| key | meaning |
|---|---|
| `gc.start_failures` | consecutive failed starts since the last success |
| `gc.start_failed_at` / `gc.start_failure` | when, and the last stderr line |
| `gc.start_backoff_until` | no start is planned before this time: 10s after the first failure, doubling per failure, capped at 5m |
| `gc.parked_at` / `gc.park_reason` / `gc.park_failures` | the park: written when the count reaches the agent's `max_start_failures` (default 5, `0` = never park) |
| `gc.park_id` | the park's random identity: names its mail (`[park <id>]` in the subject), fences the delivered stamp, lets a retry recognize a mail that landed before a restart could stamp it |
| `gc.park_mailed_at` | the one park mail landed (an unlanded mail is retried by a background sweep the tick hands off and never waits on; a landed one is found by its tag — open or archived, the receipt is the message bead itself — and never sent twice; only a landed mail the read-mail retention purge has already deleted can be sent once more) |

Both demand tiers honour the record: an in_progress bead assigned to the
pool identity (the wake-known-identity tier) and an open unassigned routed
bead (the scale_check tier) are skipped while backed off or parked, so the
pool plans no start for them. A custom `scale_check` stays authoritative:
if its query counts routed rows, exclude parked ones (`gc.parked_at` set)
unless you want a seat spawned for them; the seats it spawns are tied to the
routed rows the default probe can see, so their failed starts are charged. A **parked** bead keeps its status, assignee
and `gc.routed_to` — no other pool claims it — and exactly one mail goes to
`mayor` naming the bead, the agent, the count, the last stderr line and the
unpark verbs. `gc bd show` prints the park keys under METADATA; the
reconciler says `PARKED work bead <id>` on stderr and records a
`reconciler.pool.start_deferred` trace decision for every skipped row.

The controller's cached demand snapshot expires at the earliest backoff
deadline it held a row back to: a deadline passes with no bead or session
write to fingerprint, so a 10s backoff never lasts the cache's backstop age.
A retained on-demand named holder woken for work — a bead assigned to it, or
a routed row of its backing template (`NamedSessionRoutedDemand`) — carries
that bead as its trigger (only `gc.trigger_bead_id` / store ref; its pack,
workspace and work dir are its own), so a failed start charges that bead the
way a pool seat's would; a holder created (or reopened) fresh for direct
demand carries the trigger from its first start. A holder created fresh for
routed-only demand has no bead to bind to on its first tick; the next tick
binds it. A bead still carrying an agent's legacy bound
identity (`rig/old.worker` after a bound→unbound migration) is routed to that
agent, not "elsewhere".

A confirmed start clears the whole record, and the clear comes FIRST: on
the one commit path the record of the bead the start ran for is cleared
before the batch that stamps `creation_complete`, and a clear that fails (a
work-store read or write error; a bead that is gone or since re-routed
counts as settled) fails the commit like a failed metadata batch — the
runtime keeps running, the session stays pending-create, and the next tick's
pending-create recovery clears again before it confirms — for a fresh
create under its pending-create claim and for a KEPT session whose resume
left it start-pending/creating alike (the pre-heal state is the gate). The
bead a start ran for is its session's trigger — the one operand its trigger
env, its failure charge and its pre-confirmation clear all read — pinned
while the start is in flight, for named holders and pool seats alike: a
trigger follows its wake request only between starts (a named holder's is
cleared when there is none — its bead parked, backed off, closed or assigned
elsewhere; a reopened holder is reopened with the same clear — so a
`mode = "always"` holder restarting for no work is charged to nothing and
lifts no park), and a bind that did not land leaves the previous trigger,
which the start then runs for and is charged to. The gate is re-proven on
the row at START time (`startDeferred`, before the provider is called),
read live and deferred too when the row cannot be read: a bead parked or
backed off since the plan — another seat's failure parked it, a queued seat
outlived it, a holder's clear-bind did not land — is not started for
(outcome `work_deferred`, nothing charged; a kept session queued for that
start goes back to asleep so the next build can bind it to other work,
while a fresh seat keeps its claim and expires as never started), so a
success can never lift a park it was not planned past. An uncommitted
start stays durable as its start-pending/creating state: the heal never
moves a live start-pending/creating session on — only the commit that
clears the record first confirms it (the start path, or the next tick's
recovery) — so a kept session's resume whose clear failed is recovered
however many ticks, restarts or in-flight deferrals come between. Every record read is live (a
caching store's backing), fenced or not; the `--reassign` unpark is fenced
through the same policy-aware seam (`beads.ResolveConditionalWriter`,
following the CLI's policy wrapper; `require` refuses rather than write
unfenced), re-reads a moved row live, and clears the whole record family
whenever any of it was read, so a park written between its read and its
write is lifted; it reads the row live too, so a cache that still serves a
clean row cannot answer "nothing to reopen" over a parked backing. The one
park mail is owed whether or not the bead is still demand: besides the
demand-side retries, the controller sweeps every store it knows for parked
beads whose mail has not landed (both tiers, at most once per retry
interval; a park is mailed from the store it was found in, so a relocated
class store's active row is never mistaken for a retained copy), so a
bead whose agent was suspended, or that gained a dependency, still gets
its mail (a closed bead is not swept: its park, and its mail, are moot);
the listings and the sends run off the tick; the re-read before any send
is live, and a delivery never stamps a park lifted meanwhile. The gate the
demand build applies reads the wall clock, not the beacon time the
supervisor captured at start, so a backoff expires. The targeted
dispatcher reconcile carries the same record policy as the main tick. The start-time gate runs
before the named-session circuit breaker records an attempt: a deferred
start makes none and spends none. A failure that lands on an already parked bead (a start in
flight when the park was written) is recorded as the last failure and
does not count toward the next attempt. A park written before
`gc.park_id` existed is identified by the bead id and `gc.parked_at`
together. Under `beads.conditional_writes = "require"` a fenced write the
store refuses at write time is not retried unfenced. A trigger whose named
store fails to answer is an error, never absence — a migration's retained
copy is not charged in the active copy's place. The unpark is a designed
surface, never a hand edit (for a graph bead migrated into its class
binding, `gc sling --reassign` acts on the primary store's copy like every
sling route does; lift the park on the class store's copy in place):

```
gc sling --reassign <agent> <bead>     # re-dispatch: clears the record and the park before routing
gc bd update <bead> --unset-metadata gc.park_reason --unset-metadata gc.parked_at --unset-metadata gc.park_failures
                                        # hold in place: lifts the park; the count restarts from zero
```

Implementation: `cmd/gc/pool_start_backoff.go` (record, gate, mail),
`commitStartFailure` / `commitStartResultTraced` in
`cmd/gc/session_lifecycle_parallel.go` (where the start result is known),
`excludeStartDeferredWork` and `defaultScaleCheckCountsAndDemand` in
`cmd/gc/build_desired_state.go` (the gate), `reopenForReassign` in
`internal/sling` (the unpark). A transient failure — one `pre_start` lost a
lock, the next attempt succeeded — costs one 10s backoff and nothing else.
A convoy re-dispatched as a batch skips children already routed to the
target (the batch's pre-existing idempotence check) and lifts no park on
them: unpark a parked child by its own id.

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

This keeps judgment out of Go: the agent decides how to wrap up
(cognition). Go just flips the flag (transport).

### The hard deadline

If an agent ignores the drain signal or a formula runs too long:

1. `drain_timeout` expires (default 15m)
2. Go force-kills the session
3. Any hooked bead is re-queued (unhook without close)
4. Another agent picks it up (the system converges because work
   persists — work survives sessions)

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
