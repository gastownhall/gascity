---
title: Tutorial 07 - Orders
sidebarTitle: 07 - Orders
description: Schedule formulas and scripts to run automatically using gate conditions — cooldowns, cron schedules, shell checks, and events.
---

Formulas describe _what_ work looks like. Orders describe _when_ it should
happen. An order pairs a gate condition with an action — either a formula or a
shell script — and the controller checks those gates automatically. When a gate
opens, the order fires. No human dispatch needed.

When you run `gc start`, you launch a _controller_ — a background process that
wakes up every 30 seconds (a _tick_), checks the state of the city, and takes
action. One of the things it does on each tick is evaluate the gates that
unblock an order from running. That periodic check is what makes orders work.

We'll pick up where [Tutorial 06](/tutorials/06-beads) left off. You should
have `my-city` running with agents and formulas configured.

If you've been dispatching formulas by hand with `gc sling`, orders are the next
step: they turn that manual dispatch into something the city does on its own, on
a schedule or in response to events.

## A simple order

Orders live in an `orders/` directory at the top level of your city, alongside
`formulas/` and `prompts/`. Each order gets its own subdirectory containing an
`order.toml` file. The subdirectory name becomes the order name.

```
orders/
  review-check/order.toml
  dep-update/order.toml
formulas/
  pancakes.formula.toml
  review.formula.toml
```

Here's a minimal order that dispatches the `review` formula from Tutorial 04
every five minutes:

```toml
# orders/review-check/order.toml
[order]
description = "Check for PRs that need review"
formula = "review"
gate = "cooldown"
interval = "5m"
pool = "worker"
```

The `pool` field tells the controller where to send the work. A _pool_ is a
named group of one or more agents that share a work queue — the agents chapter
introduced them briefly. When an order fires, the controller creates a wisp from
the formula and routes it to the named pool. Any agent in that pool can pick it
up.

The controller evaluates gate conditions on every tick. When five minutes have
passed since the last run, it instantiates the `review` formula as a wisp and
routes it to the `worker` pool. The order name comes from the subdirectory name
— `review-check` — not from anything in the TOML.

Orders are discovered when the city starts and whenever the controller reloads
config. You don't need to restart anything if the city is already watching the
orders directory.

## Inspecting orders

Once you've defined some orders, you'll want to see what the controller sees —
which orders exist, what their gates look like, and whether any are due. Three
commands give you that view.

`gc order list` shows every enabled order in your city — whether or not it has
ever fired:

```shell
~/my-city
$ gc order list
NAME            TYPE     GATE      INTERVAL/SCHED  TARGET
review-check    formula  cooldown  5m              worker
dep-update      formula  cooldown  1h              worker
release-notes   formula  cooldown  24h             worker
```

The `TARGET` column is the pool the order will route to (the field is still
`pool` in the TOML).

To see the full definition:

```shell
~/my-city
$ gc order show review-check
Order:  review-check
Description: Check for PRs that need review
Formula:     review
Gate:        cooldown
Interval:    5m
Target:      worker
Source:      /Users/you/my-city/orders/review-check/order.toml
```

To check which orders are due right now:

```shell
~/my-city
$ gc order check
NAME            GATE      DUE  REASON
review-check    cooldown  yes  never run
dep-update      cooldown  no   cooldown: 14m remaining
release-notes   cooldown  no   cooldown: 18h remaining
```

## Running an order manually

Any order can be triggered by hand, bypassing its gate:

```shell
~/my-city
$ gc order run review-check
Order "review-check" executed: wisp mc-2xz → gc.routed_to=worker
```

For exec orders, the output is simpler — `Order "<name>" executed (exec)`.

This is useful for testing a new order or for kicking off work that's almost due
anyway.

## Gate types

The gate is what makes an order tick. It controls _when_ the order fires. There
are five gate types.

### Cooldown

The most common gate. The name comes from the idea of a cooldown timer — after
the order fires, it has to cool down for a set interval before it can fire
again:

```toml
[order]
description = "Check for stale feature branches"
formula = "stale-branches"
gate = "cooldown"
interval = "5m"
pool = "worker"
```

If the order has never run, it fires immediately on the first tick. After that,
it waits until `interval` has elapsed since the last run. The interval is a Go
duration string — `30s`, `5m`, `1h`, `24h`.

### Cron

Fires on an absolute schedule, like Unix cron job:

```toml
[order]
description = "Generate release notes from yesterday's merges"
formula = "release-notes"
gate = "cron"
schedule = "0 3 * * *"
pool = "worker"
```

The schedule is a 5-field cron expression: minute, hour, day-of-month, month,
day-of-week. This example fires at 3:00 AM every day. Fields support `*` (any),
exact integers, and comma-separated values (`1,15` for the 1st and 15th).

The difference from cooldown: a cooldown fires _relative_ to the last run
("every 5 minutes"), while cron fires at _absolute_ times ("at 3 AM daily").
Cooldown drifts — if the last run was at 3:02, the next is at 3:07. Cron hits
the same wall-clock times every day.

Cron gates fire at most once per minute — if the order already ran during the
current minute, it waits for the next match.

### Condition

Fires when a shell command exits 0:

```toml
[order]
description = "Deploy when the flag file appears"
formula = "deploy"
gate = "condition"
check = "test -f /tmp/deploy-flag"
pool = "worker"
```

The controller runs `sh -c "<check>"` with a 10-second timeout on each tick. If
the command exits 0, the order fires. Any other exit code, and it doesn't. This
is the gate for dynamic, external triggers — check a file, ping an endpoint,
query a database.

One caveat: the check runs synchronously during gate evaluation. A slow check
delays evaluation of subsequent orders on that tick. Keep checks fast.

### Event

Fires in response to system events:

```toml
[order]
description = "Check if all PR reviews are done and merge is ready"
formula = "merge-ready"
gate = "event"
on = "bead.closed"
pool = "worker"
```

This fires whenever a `bead.closed` event appears on the event bus. Event gates
use cursor-based tracking — each firing advances a sequence marker so the same
event isn't processed twice.

### Manual

Never auto-fires. Only triggered by `gc order run`:

```toml
[order]
description = "Full test suite — expensive, run only when needed"
formula = "full-test-suite"
gate = "manual"
pool = "worker"
```

Manual orders don't appear in `gc order check` (there's nothing to check —
they're never due automatically). They do appear in `gc order list`.

## Formula orders vs. exec orders

So far every example has used a formula as the action. But orders can also run
shell scripts directly:

```toml
[order]
description = "Delete branches already merged to main"
gate = "cooldown"
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
  containing the `order.toml`. Pack-sourced orders also get `PACK_DIR`.

Default timeouts differ: 30 seconds for formula orders, 300 seconds for exec
orders.

## Timeouts

Each order can set a timeout:

```toml
[order]
description = "Run the linter on changed files"
formula = "lint-check"
gate = "cooldown"
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

## Disabling and skipping orders

An order can be disabled in its own definition:

```toml
[order]
description = "Temporarily disabled"
formula = "nightly-bench"
gate = "cooldown"
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

Overrides can change `enabled`, `gate`, `interval`, `schedule`, `check`, `on`,
`pool`, and `timeout`. The override matches by order name — if no order with
that name exists, it's an error (fail-fast, not silent).

## Order history

Every time an order fires, Gas City creates a tracking bead labeled with the
order name. You can query the history:

```shell
~/my-city
$ gc order history
ORDER           BEAD     EXECUTED
review-check    mc-3hb   2026-04-08T07:36:36Z
dep-update      mc-784   2026-04-08T06:48:12Z
review-check    mc-zbd   2026-04-08T07:31:22Z
release-notes   mc-zb8   2026-04-07T13:00:01Z

~/my-city
$ gc order history review-check
ORDER           BEAD     EXECUTED
review-check    mc-3hb   2026-04-08T07:36:36Z
review-check    mc-zbd   2026-04-08T07:31:22Z
review-check    mc-9p8   2026-04-08T07:26:18Z
```

The tracking bead is created synchronously _before_ the dispatch goroutine
launches. This is what prevents the cooldown gate from re-firing on the very
next tick — the gate checks for recent tracking beads when deciding if the order
is due.

## Duplicate prevention

Before dispatching, the controller checks whether the order already has open
(non-closed) work. If it does, the order is skipped even if the gate says it's
due. This prevents pileup — if an agent is still working through the last review
check, the controller won't dispatch another one.

## Rig-scoped orders

Orders don't just live at the city level. When a pack is applied to a rig, that
pack's orders come along and run scoped to that rig.

Say you have a pack called `dev-ops` that includes a `test-suite` order:

```
packs/dev-ops/
  orders/
    test-suite/
      order.toml        # gate = "cooldown", interval = "5m", pool = "worker"
  formulas/
    test-suite.formula.toml
```

And your city applies that pack to two rigs:

```toml
# city.toml
[[rigs]]
name = "my-api"
path = "../my-api"
includes = ["packs/dev-ops"]

[[rigs]]
name = "my-frontend"
path = "../my-frontend"
includes = ["packs/dev-ops"]
```

Now the city has the same order running independently for each rig:

```shell
~/my-city
$ gc order list
NAME        TYPE     GATE      INTERVAL/SCHED  TARGET
test-suite  formula  cooldown  5m              worker
test-suite  formula  cooldown  5m              my-api/worker
test-suite  formula  cooldown  5m              my-frontend/worker
```

Three identical names, three different targets — the rig that owns each one is
encoded in the qualified target name (`my-api/worker` vs `my-frontend/worker`). To
act on a specific one, pass `--rig`:

```shell
$ gc order show test-suite --rig my-api
$ gc order run test-suite --rig my-api
```

These are three independent orders. The city-level `test-suite` has its own
cooldown timer, its own tracking beads, its own history. The `my-api` version
tracks separately — if the city-level order fired two minutes ago, that doesn't
affect whether the `my-api` order is due. Internally, Gas City distinguishes
them by _scoped name_: `test-suite` vs `test-suite:rig:my-api` vs
`test-suite:rig:my-frontend`.

Pool targets are auto-qualified: `pool = "worker"` in the order definition
becomes `gc.routed_to=my-api/worker` on the dispatched wisp, routing work to
the rig's own agents rather than the city-level pool.

## Order layering

With orders coming from packs, rigs, and your city's own `orders/` directory,
the same order name can exist in multiple places. When that happens, the
highest-priority layer wins. The layers, from lowest to highest priority:

1. **City packs** — orders that ship with a pack you've included (e.g., the
   `dev-ops` pack's `test-suite`)
2. **City local** — orders in your city's own `orders/` directory
3. **Rig packs** — orders from packs applied to a specific rig
4. **Rig local** — orders in a rig's own formula directories

A higher layer completely replaces a lower layer's definition for the same order
name. So if the `dev-ops` pack defines `test-suite` with a 5-minute cooldown and
you create your own `orders/test-suite/order.toml` with a 1-minute cooldown,
yours wins — the pack version is ignored entirely.

## Putting it together

Here's a city with two orders: a frequent lint check (exec, no agent needed) and
weekly release notes (formula, dispatched to an agent).

```toml
# city.toml
[workspace]
name = "my-city"
provider = "claude"

[formulas]
dir = "formulas"

[[agent]]
name = "worker"
prompt_template = "prompts/worker.md"
```

```toml
# orders/lint-check/order.toml
[order]
description = "Run the linter on changed files"
gate = "cooldown"
interval = "30s"
exec = "scripts/lint-changed.sh"
timeout = "60s"
```

```toml
# orders/release-notes/order.toml
[order]
description = "Generate release notes from the week's merges"
formula = "release-notes"
gate = "cron"
schedule = "0 9 * * 1"
pool = "worker"
```

```toml
# formulas/release-notes.formula.toml
formula = "release-notes"

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
NAME           TYPE     GATE      INTERVAL/SCHED  TARGET
lint-check     exec     cooldown  30s             -
release-notes  formula  cron      0 9 * * 1       worker

~/my-city
$ gc order check
NAME           GATE      DUE  REASON
lint-check     cooldown  yes  never run
release-notes  cron      no   next fire in 3d 14h
```

The lint check fires immediately (never run + cooldown gate = due), then every
30 seconds after that. The release notes fire Monday at 9 AM, dispatching a
three-step formula wisp to the `worker` pool. Neither requires anyone to type
`gc sling`.

Orders are formulas and scripts on autopilot, gated by time, schedule,
conditions, or events, evaluated by the controller on every tick.
