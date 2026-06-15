---
title: Tutorial 07 - Orders
sidebarTitle: 07 - Orders
description: Schedule formulas and scripts to run automatically using trigger conditions — cooldowns, cron schedules, shell checks, and events.
---

Formulas describe _what_ work looks like. Orders describe _when_ it should
happen. An order pairs a trigger condition with an action — either a formula or a
shell script — and the controller checks those triggers automatically. When a trigger
opens, the order fires. No human dispatch needed.

When you run `gc start`, you launch a _controller_ — a background process that
wakes up every 30 seconds (a _tick_), checks the state of the city, and takes
action. One of the things it does on each tick is evaluate the triggers that
unblock an order from running. That periodic check is what makes orders work.

We'll pick up where [Tutorial 06](/tutorials/06-beads) left off. You should
have `my-city` running with agents and formulas configured.

If you've been dispatching formulas by hand with `gc sling`, orders are the next
step: they turn that manual dispatch into something the city does on its own, on
a schedule or in response to events.

## A simple order

Orders live in an `orders/` directory at the top level of your city, alongside
`formulas/` and `agents/`. Each order is a flat `*.toml` file in that
directory.

```
orders/
  pancakes-check.toml
  dep-update.toml
formulas/
  pancakes.toml
```

Here's a minimal order that dispatches the `pancakes` formula from
[Tutorial 05](/tutorials/05-formulas) every five minutes:

```toml
# orders/pancakes-check.toml
[order]
description = "Cook pancakes on a timer"
formula = "pancakes"
trigger = "cooldown"
interval = "5m"
pool = "worker"
```

The `pool` field tells the controller where to send the work. A _pool_ is a
named group of one or more agents that share a work queue — you glimpsed one
in Tutorial 01, where `gc status` showed the bundled `dolt.dog` pool. A single
agent's name works as a pool target too; the examples here route to the
`worker` agent from Tutorial 05. When an order fires, the controller creates a
wisp from the formula and routes it to the named pool. Any agent in that pool
can pick it up.

The controller evaluates trigger conditions on every tick. When five minutes have
passed since the last run, it instantiates the `pancakes` formula as a wisp and
routes it to the `worker` pool. The order name comes from the file basename
(`pancakes-check.toml` → `pancakes-check`), not from anything in the TOML.

(Tutorial 05 taught you that v2 formulas start **workflows** — the order
machinery still calls any dispatched formula instance a wisp, and that's the
word `gc order run` prints even for v2 formulas. Same beads either way.)

When the dispatcher stamps the wisp for a pool target it writes
`gc.routed_to=<pool>` so the worker's `bd ready` query and the supervisor's
default `scale_check` both see it through the shared routed queue. Formula
orders that should wake a pool must compile to Ready-visible work. v2
formulas — anything declaring `[requires] formula_compiler = ">=2.0.0"` (see
[Choosing a Compiler Contract](/guides/understanding-formulas#choosing-a-compiler-contract))
— qualify: the dispatcher routes their step beads to the pool, and steps with
no open blockers are Ready immediately. (Root-only v1 wisps qualify too,
but that shape is a holdover — use v2 for new formulas.) What doesn't
qualify is a v1 formula whose root is a molecule container — `bd ready`
filters those out, and the dispatcher logs a warning telling you to convert
the formula before routing it to a pool.

After upgrading from older dispatcher versions, close any open v1
molecule-shaped pool-order wisps with `gc order sweep-tracking --include-wisps
<order>` or let a min-floor worker drain them before expecting that order to
fire again from a scale-from-zero pool.

Orders are discovered when the city starts, and the controller rescans the
order set as it ticks — at most once per minute. You don't need to restart
anything: drop a new order file into `orders/` and it shows up within a
minute.

## Inspecting orders

Once you've defined some orders, you'll want to see what the controller sees —
which orders exist, what their triggers look like, and whether any are due. Three
commands give you that view.

`gc order list` shows every enabled order in your city — whether or not it has
ever fired:

```shell
~/my-city
$ gc order list
NAME                 TYPE     TRIGGER      INTERVAL/SCHED  TARGET
pancakes-check       formula  cooldown     5m              worker
dep-update           formula  cooldown     1h              worker
release-notes        formula  cooldown     24h             worker
```

Your output will also include built-in housekeeping orders that ship
with the bundled core pack (`beads-health`, `gate-sweep`, `jsonl-export`,
`reaper`, `orphan-sweep`, `prune-branches`, `spawn-storm-detect`,
`wisp-compact`, etc.). They're the city's housekeeping orders — you can leave
them alone.

The `TARGET` column is the pool the order will route to (the field is still
`pool` in the TOML).

To see the full definition:

```shell
~/my-city
$ gc order show pancakes-check
Order:  pancakes-check
Description: Cook pancakes on a timer
Formula:     pancakes
Trigger:     cooldown
Interval:    5m
Target:      worker
Source:      /Users/you/my-city/orders/pancakes-check.toml
```

To check which orders are due right now:

```shell
~/my-city
$ gc order check
NAME                 TRIGGER      DUE   REASON
pancakes-check       cooldown     yes   never run
dep-update           cooldown     no    cooldown: 59m52s remaining
release-notes        cooldown     no    cooldown: 23h59m53s remaining
```

## Running an order manually

Any order can be triggered by hand, bypassing its trigger:

```shell
~/my-city
$ gc order run pancakes-check
Order "pancakes-check" executed: wisp mc-2xz → gc.routed_to=worker
```

For exec orders, the output is simpler — `Order "<name>" executed (exec)`.

This is useful for testing a new order or for kicking off work that's almost due
anyway.

## Trigger types

The trigger is what makes an order tick. It controls _when_ the order fires. There
are five trigger types.

### Cooldown

The most common trigger. The name comes from the idea of a cooldown timer — after
the order fires, it has to cool down for a set interval before it can fire
again:

```toml
[order]
description = "Check for stale feature branches"
formula = "stale-branches"
trigger = "cooldown"
interval = "5m"
pool = "worker"
```

If the order has never run, it fires immediately on the first tick. After that,
it waits until `interval` has elapsed since the last run. The interval is a Go
duration string — `30s`, `5m`, `1h`, `24h`.

### Cron

Fires on an absolute schedule, like a Unix cron job:

```toml
[order]
description = "Generate release notes from yesterday's merges"
formula = "release-notes"
trigger = "cron"
schedule = "0 3 * * *"
pool = "worker"
```

The schedule is a 5-field cron expression: minute, hour, day-of-month, month,
day-of-week. This example fires at 3:00 AM every day. Fields support `*` (any),
exact integers, comma-separated values (`1,15` for the 1st and 15th), and
`*/N` steps (`*/4` matches every value divisible by 4).

The difference from cooldown: a cooldown fires _relative_ to the last run
("every 5 minutes"), while cron fires at _absolute_ times ("at 3 AM daily").
Cooldown drifts — if the last run was at 3:02, the next is at 3:07. Cron hits
the same wall-clock times every day.

Cron triggers fire at most once per minute — if the order already ran during the
current minute, it waits for the next match. They also catch up: if no
controller tick landed during a scheduled minute (the controller was busy or
down), the next tick fires the missed occurrence once — `gc order check` shows
the reason `cron: caught up missed occurrence`. Orders that have never run
don't backfill; they fire only on an exact match.

### Condition

Fires when a shell command exits 0:

```toml
[order]
description = "Deploy when the flag file appears"
formula = "deploy"
trigger = "condition"
check = "test -f /tmp/deploy-flag"
pool = "worker"
```

The controller runs `sh -c "<check>"` with a 10-second timeout on each tick. If
the command exits 0, the order fires. Any other exit code, and it doesn't. This
is the trigger for dynamic, external triggers — check a file, ping an endpoint,
query a database.

One caveat: the check runs synchronously during trigger evaluation. A slow check
delays evaluation of subsequent orders on that tick. Keep checks fast.

### Event

Fires in response to system events:

```toml
[order]
description = "Check if all PR reviews are done and merge is ready"
formula = "merge-ready"
trigger = "event"
on = "bead.closed"
pool = "worker"
```

This fires whenever a `bead.closed` event appears on the event bus. Event triggers
use cursor-based tracking — each firing advances a sequence marker so the same
event isn't processed twice.

### Manual

Never auto-fires. Only triggered by `gc order run`:

```toml
[order]
description = "Full test suite — expensive, run only when needed"
formula = "full-test-suite"
trigger = "manual"
pool = "worker"
```

Manual orders show up in `gc order check` like every other enabled order —
always with DUE `no` and the reason `manual trigger — use gc order run`. They
appear in `gc order list` too.

## Formula orders vs. exec orders

So far every example has used a formula as the action. But orders can also run
shell scripts directly:

```toml
[order]
description = "Delete branches already merged to main"
trigger = "cooldown"
interval = "5m"
exec = "scripts/prune-merged.sh"
```

An exec order runs the script on the controller — no agent, no LLM, no wisp.
This is the right choice for purely mechanical operations: pruning branches,
running linters, checking disk usage, anything where involving an agent would be
wasteful.

The rules:

- Every order has either `formula` or `exec`, never both.
- Exec orders can't have a `pool` — there's no agent pipeline to route to.
- The script receives `ORDER_DIR` in its environment, set to the directory
  containing the order file. Pack-sourced orders also get `PACK_DIR`.
- An `[order.env]` table exports extra environment variables into the script —
  handy for tuning thresholds without editing the script. `env` is only valid
  on exec orders.

Default timeouts differ: 30 seconds for formula orders, 300 seconds for exec
orders.

## Timeouts

Each order can set a timeout:

```toml
[order]
description = "Run the linter on changed files"
formula = "lint-check"
trigger = "cooldown"
interval = "30s"
pool = "worker"
timeout = "60s"
```

For formula orders, the timeout covers the initial dispatch — compiling the
formula, creating the wisp, and routing it to the pool. Once the wisp is created
and handed off, the agent works on it at its own pace; the timeout doesn't kill
an agent mid-work. For exec orders, the timeout covers the full script execution
— if the script is still running when time is up, the process is killed. You can
also set a global cap in `city.toml`:

```toml
[orders]
max_timeout = "120s"
```

The effective timeout is the lesser of the per-order timeout and the global cap.

## Order scope

When a pack is imported into more than one rig, its orders are instantiated
**once per rig** by default — the same way per-rig agents are. That's usually
what you want for an order that acts on a single rig's work. But some orders are
city-wide: a sweep or health probe that already iterates over every rig
internally would then run redundantly, once per rig.

Mark such an order city-scoped so it registers exactly once, no matter how many
rigs import the pack:

```toml
[order]
description = "Sweep merged convoys across the whole city"
trigger = "cooldown"
interval = "5m"
exec = "scripts/convoy-sweep.sh"
scope = "city"
```

`scope` accepts `"city"` or `"rig"`. The default (when omitted) is `"rig"`, so
existing orders are unaffected. A `scope = "city"` order appears once in `gc
order list` with no rig qualifier; rig-scoped orders appear once per importing
rig. This mirrors the `scope` field on `[[named_session]]`.

Use `scope = "city"` for orders that live in a **shared pack** imported by
several rigs — that's where the per-rig duplication it collapses comes from. A
city-scoped order keeps the formula layer of the pack it was scanned from, so
its formula must resolve from that pack rather than from any one rig's local
`orders/` directory.

## Disabling and skipping orders

An order can be disabled in its own definition:

```toml
[order]
description = "Temporarily disabled"
formula = "nightly-bench"
trigger = "cooldown"
interval = "1m"
pool = "worker"
enabled = false
```

Disabled orders are excluded from scanning entirely — they don't appear in `gc
order list` or get evaluated.

You can also skip orders by name in `city.toml` without editing the order file:

```toml
[orders]
skip = ["nightly-bench", "experimental-check"]
```

This is useful when a pack provides orders you don't want running in your city.

## Overrides

Sometimes a pack's order is almost right but you need to tweak the interval or
change the pool. Rather than copying and modifying the order file, use overrides
in `city.toml`:

```toml
[[orders.overrides]]
name = "test-suite"
interval = "1m"

[[orders.overrides]]
name = "release-notes"
pool = "mayor"
schedule = "0 6 * * *"
```

Overrides can change `enabled`, `trigger`, `interval`, `schedule`, `check`, `on`,
`pool`, `timeout`, `idempotent`, and `env`. The override matches by order name. An override that
targets a nonexistent order produces an error rather than silently no-opping
— `gc order` CLI commands fail; `gc start` logs the error and continues
running with the unmatched override skipped.

### Rig scoping

Many orders expand at scan time into one instance per rig (anything in a
rig's `orders/` directory or a pack imported into a rig). When the same
order appears city-wide AND per-rig, an override must say which:

```toml
# Targets ONLY the city-level instance. Per-rig copies are unaffected.
[[orders.overrides]]
name = "patrol"
enabled = false

# Targets ONLY the demo-repo rig's copy.
[[orders.overrides]]
name = "patrol"
rig = "demo-repo"
enabled = false

# Wildcard: targets every instance — city-level + all rig copies.
[[orders.overrides]]
name = "patrol"
rig = "*"
enabled = false
```

A rigless override against a name that exists ONLY as per-rig copies is an
error; the message names the rigs so you know what to type. The literal
`"*"` is reserved as the wildcard token and may not be used as a real rig
name.

## Order history

Every time an order fires, Gas City creates a tracking bead labeled with the
order name. You can query the history:

```shell
~/my-city
$ gc order history
ORDER           BEAD     EXECUTED
pancakes-check  mc-3hb   2026-04-08T07:36:36Z
dep-update      mc-784   2026-04-08T06:48:12Z
pancakes-check  mc-zbd   2026-04-08T07:31:22Z
release-notes   mc-zb8   2026-04-07T13:00:01Z

~/my-city
$ gc order history pancakes-check
ORDER           BEAD     EXECUTED
pancakes-check  mc-3hb   2026-04-08T07:36:36Z
pancakes-check  mc-zbd   2026-04-08T07:31:22Z
pancakes-check  mc-9p8   2026-04-08T07:26:18Z
```

The tracking bead is created synchronously _before_ the dispatch goroutine
launches. This is what prevents the cooldown trigger from re-firing on the very
next tick — the trigger checks for recent tracking beads when deciding if the order
is due.

## Duplicate prevention

Before dispatching, the controller checks whether the order already has open
(non-closed) work. If it does, the order is skipped even if the trigger says it's
due. This prevents pileup — if an agent is still working through the last
pancakes run, the controller won't dispatch another one.

The open-work check runs against the store with a bounded timeout. If the
store is so contended that the check itself times out, the order is skipped —
it fails closed. Orders whose dispatch is safe to repeat (sweeps and feeders
where a duplicate run is a no-op) can set `idempotent = true` in the order
file to fail open instead: on a gate timeout they dispatch anyway rather than
be starved.

## Rig-scoped orders

Orders don't just live at the city level. When a pack is applied to a rig, that
pack's orders come along and run scoped to that rig.

Say you have a pack called `dev-ops` that includes a `test-suite` order:

```
packs/dev-ops/
  orders/
    test-suite.toml         # trigger = "cooldown", interval = "5m", pool = "worker"
  formulas/
    test-suite.toml
```

And your city applies that pack to two rigs:

```toml
# city.toml
[[rigs]]
name = "my-api"

[rigs.imports.dev_ops]
source = "./packs/dev-ops"

[[rigs]]
name = "my-frontend"

[rigs.imports.dev_ops]
source = "./packs/dev-ops"
```

```toml
# .gc/site.toml
[[rig]]
name = "my-api"
path = "../my-api"

[[rig]]
name = "my-frontend"
path = "../my-frontend"
```

Now the city has the same order running independently for each rig:

```shell
~/my-city
$ gc order list
NAME                 TYPE     TRIGGER      INTERVAL/SCHED  RIG             TARGET
test-suite           formula  cooldown     5m              my-api          worker
test-suite           formula  cooldown     5m              my-frontend     worker
```

Two rows with the same name — one per importing rig. As soon as any rig-scoped
order exists, `gc order list` adds a RIG column showing which rig owns each
instance (city-level orders show `-` there). TARGET stays the raw pool name
from the order file; the rig qualification happens at dispatch time, below. To
act on a specific instance, pass `--rig`:

```shell
$ gc order show test-suite --rig my-api
$ gc order run test-suite --rig my-api
```

These are two independent orders. The `my-api` copy has its own cooldown
timer, its own tracking beads, its own history — if the `my-frontend` copy
fired two minutes ago, that doesn't affect whether the `my-api` copy is due.
Internally, Gas City distinguishes them by _scoped name_:
`test-suite:rig:my-api` vs `test-suite:rig:my-frontend`.

Pool targets are auto-qualified at dispatch: `pool = "worker"` in the order
definition becomes `gc.routed_to=my-api/worker` on the dispatched wisp,
routing work to the rig's own agents rather than the city-level pool.

## Order layering

With orders coming from packs and local `orders/` directories, the same order
name can exist in more than one place. Within a single scope — the city, or
one rig — the highest-priority source wins:

1. **Packs** — orders that ship with a pack that scope has included or
   imported (e.g., the `dev-ops` pack's `test-suite`)
2. **Local** — orders in that scope's own `orders/` directory

A higher layer completely replaces a lower layer's definition for the same
order name within its scope. So if the `dev-ops` pack defines `test-suite`
with a 5-minute cooldown and you create your own city-level
`orders/test-suite.toml` with a 1-minute cooldown, yours wins — the pack
version is ignored entirely.

Replacement never crosses scopes, though. A rig's `test-suite` doesn't replace
a city order of the same name — as the previous section showed, those coexist
as independent scoped instances (`test-suite` vs `test-suite:rig:my-api`).

## Putting it together

Here's a city with two orders: a frequent lint check (exec, no agent needed) and
weekly release notes (formula, dispatched to an agent).

Assume you've already created a `worker` agent as in
[Tutorial 05](/tutorials/05-formulas). The remaining pieces are just the order
files and the formula they dispatch.

```toml
# orders/lint-check.toml
[order]
description = "Run the linter on changed files"
trigger = "cooldown"
interval = "30s"
exec = "scripts/lint-changed.sh"
timeout = "60s"
```

```toml
# orders/release-notes.toml
[order]
description = "Generate release notes from the week's merges"
formula = "release-notes"
trigger = "cron"
schedule = "0 9 * * 1"
pool = "worker"
```

```toml
# formulas/release-notes.toml
formula = "release-notes"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "gather"
title = "Gather merged PRs from the last week"

[[steps]]
id = "summarize"
title = "Write release notes"
needs = ["gather"]

[[steps]]
id = "post"
title = "Post release notes to the team channel"
needs = ["summarize"]
```

```shell
~/my-city
$ gc start
City 'my-city' started

~/my-city
$ gc order list
NAME                 TYPE     TRIGGER      INTERVAL/SCHED  TARGET
lint-check           exec     cooldown     30s             -
release-notes        formula  cron         0 9 * * 1       worker

~/my-city
$ gc order check
NAME                 TRIGGER      DUE   REASON
lint-check           cooldown     yes   never run
release-notes        cron         no    cron: schedule not matched
```

The lint check fires immediately (never run + cooldown trigger = due), then every
30 seconds after that. The release notes fire Monday at 9 AM, dispatching the
three-step `release-notes` workflow to the `worker` pool. Neither requires
anyone to type `gc sling`.

Orders are formulas and scripts on autopilot, gated by time, schedule,
conditions, or events, evaluated by the controller on every tick.

## What's next

This is the last tutorial in the series. From here, the reference pages and
guides go deeper:

- **[Formula spec (v2)](/reference/specs/formula-spec-v2)** — the full formula file format,
  including the v2 runtime constructs your orders dispatch
- **[Understanding packs](/guides/understanding-packs)** — how formulas,
  orders, agents, and prompts travel together as importable packs
