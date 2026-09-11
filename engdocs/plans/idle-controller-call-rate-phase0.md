# Plan: idle-controller-call-rate — Phase 0 baseline measurement

> **Status:** In progress — 2026-06-16
> **Design:** [`idle-controller-call-rate`](../design/idle-controller-call-rate.md)
> **Issues:** #3543, #2463

Phase 0 of [`idle-controller-call-rate`](../design/idle-controller-call-rate.md).
It produces the numbers that **gate** every optimisation pillar. No optimisation
code is written until this baseline exists, because the headline #2463 figures
(~7 `bd`/sec, ~463 Dolt q/sec) are stale: they predate #3097 (suspended-rig
reconcile skip + pack-hash memoization), #3270 (native store with hooks), and
the order-dispatcher's suspended-skip. We must measure the **residual** on a
current native-store build, not re-quote pre-fix numbers.

## What's already done

- **Instrumentation exists** — `internal/beads/bdtrace.go` (#2485) writes one
  JSONL record per `bd` subprocess to `GC_BD_TRACE_JSON`, scope-classified
  (`order-dispatch`, `tick-body`, `bead-event-watcher`, `hook:*`, `cli-command`,
  `unknown`) and tick-reason attributed (`patrol`/`poke`/…). No new tracer needed.
- **Aggregator** — [`scripts/bd-call-rate/aggregate.py`](../../scripts/bd-call-rate/aggregate.py)
  turns that JSONL into by-subcommand (the #2463 table), by-scope, by-tick-trigger,
  and a scope×subcommand cross-tab. `python3 scripts/bd-call-rate/aggregate.py --self-test`.
- **Baseline binary** — builds clean from `origin/main` (post-#3097/#3270):
  `go build -o /tmp/gc-baseline ./cmd/gc`.

## Procedure

> ⚠️ **Measure the *idle controller*, not paid agents.** The target is the
> controller's steady-state read fan-out, so the city must tick **without
> spawning provider agents** (no Claude/API cost, and agent work would pollute
> the idle signal). Use a disposable city and keep agents suspended.

```bash
GC=/tmp/gc-baseline
CITY=$(mktemp -d /tmp/bd-rate-city.XXXX)
TRACE=$(mktemp /tmp/bd-rate.XXXX.jsonl)

# 1. Disposable city. git-init the scope so the native store is eligible
#    (avoids the #3248 bd_context_agreement gate → real native-store baseline).
"$GC" init --template gastown --default-provider claude "$CITY"
git -C "$CITY" init -q
"$GC" doctor --city "$CITY" 2>&1 | grep -i native_store || echo "native store eligible"

# 2. Keep the controller running but spawn no agents.
"$GC" suspend "$CITY"          # city suspended ⇒ reconciler skips agent spawn…
#    …but we still want order-dispatch/tick reads. If suspend also halts the
#    tick loop, instead leave the city active with every agent suspended
#    (gc agent suspend <name> for each) so the controller ticks with zero agents.

# 3. Idle run with tracing. 10 min mirrors the #2463 methodology; 3 min is a
#    quick first read.
GC_BD_TRACE_JSON="$TRACE" "$GC" start "$CITY"
sleep 600
"$GC" stop "$CITY"

# 4. Aggregate.
python3 scripts/bd-call-rate/aggregate.py "$TRACE"
```

Repeat for three shapes to characterise the rate's drivers:

| Shape | Why |
|---|---|
| **single-rig, idle** | the floor: order-dispatch + tick-body + event-watcher reads |
| **multi-rig (e.g. 8), all active** | does the rate scale with rig count? |
| **multi-rig, mostly suspended** | confirms #3097 + dispatch suspended-skip (#3543's 15-of-16 case ⇒ should ≈ single-rig) |

## Results (2026-06-16, native-store build `/tmp/gc-baseline` of `origin/main`)

Measured on an isolated throwaway city (`GC_HOME=/tmp/bd-rate-gchome`, gastown
template, claude provider, **agents never started — no spawn, no cost**) by
tracing single read-only operations the controller/status-line invoke.

**Per-operation `bd` *subprocess* fan-out, native store vs forced CLI-fallback:**

| operation | bd subprocs (native store) | bd subprocs (CLI-fallback) |
|---|--:|--:|
| `gc order check` | **1** (`bd context` only) | **43** (21 `list` + 21 `query` + 1 `context`) |
| `gc doctor` | 10 | — |
| `gc rig list --json` | 0 | — |
| `gc mail count` | 1 | — |

CLI-fallback was forced by moving the scope's `.git` aside, tripping
`gate=bd_context_agreement` — **the exact warning observed in the #3543
incident.** The 21 `list` + 21 `query` per single order-eval pass mirrors
#2463's read-dominated profile (list+query were 66% there).

> **Note (#3552, merged `04eef3468` 2026-06-16):** the
> `gate=bd_context_agreement` fallback line was dropped from `WARN` to `DEBUG`.
> On a current binary this repro therefore needs `-v` (or
> `GC_LOG_LEVEL=debug`) to observe the gate firing; without it the fallback is
> silent. The subprocess-count A/B itself is unaffected.

### Findings

1. **The #2463/#3543 `bd`-subprocess flood is a CLI-fallback artifact.** One
   order-eval pass: **43 → 1** bd subprocesses (native store), a ~43× cut. Our
   #3543 incident ran in CLI-fallback (binary predated #3505); on the
   native-store baseline (#3270/#3505) the subprocess fan-out is near-zero.
2. **Metric pivot (important).** `GC_BD_TRACE_JSON` counts `bd` **subprocess**
   spawns only. With the native store, reads are served **in-process** and are
   *not* traced — so a near-zero `bd`/sec here does **not** mean zero Dolt load.
   The residual rate the design's Pillars 1/2 target (the ~463 `Com_select`/sec
   @mmlac measured) is now **in-process Dolt query volume**, which must be
   measured via Dolt `Com_select` (`SHOW GLOBAL STATUS LIKE 'Com_select'`
   delta) under an idle, *ticking* native-store city — not via this tracer.
3. **The per-order N+1 (Pillar 2 / #3492) only bites in CLI-fallback.** With the
   native store it is already coalesced to ~1 subprocess; whether an in-process
   N+1 remains in Dolt-query terms is the open question for the Com_select pass.

### Next sub-step (the real gate for Pillars 1/2)

Stand up a *ticking* native-store city with **zero agents** and sample
`Com_select`/sec over an idle window (single-rig, multi-active, mostly-suspended).
That is blocked on a clean "controller-ticks-but-spawns-no-agents" config: `gc
suspend` halts order dispatch (`order_dispatch.go:428`), and the gastown pack
agents (incl. the autonomous `mayor`) can't be suspended via `gc agent suspend`
(pack-defined → needs `[[patches.agent]] suspended=true`, ambiguous for the two
`dog` agents). Resolve that targeting, then measure `Com_select`.

**In-process Dolt query volume (the real residual), single-rig native-store city:**

Measured `Com_select` (`SHOW GLOBAL STATUS`, via `dolt --host …:32033 --no-tls`)
across a loop of `gc order check`:

| metric | value |
|---|--:|
| `Com_select` per `gc order check` | **~56** (incl. per-process native-store *open* overhead) |
| back-to-back loop rate (20 checks / 19 s) | ~59 `Com_select`/sec |
| **estimated idle order-dispatch rate** (56 ÷ 30 s patrol) | **~1.9 `Com_select`/sec** |

So the in-process query *volume per order-eval is real* (~56), but at the idle
patrol cadence (one eval / 30 s) the **steady-state order-dispatch Dolt load is
modest** — orders of magnitude below @mmlac's 463/sec, which was a 16-agent
*active* town, not idle. The 56 is also inflated by per-invocation store-open
(version/schema/`bd context`) that a long-running controller pays once, not per
tick — so the true steady-state per-tick figure is lower still.

**Long-running-controller idle `Com_select`/sec — attempted, contaminated.** A
long-running isolated supervisor (open-once, tick at 30 s) was run on the
native-store baseline. `[[patches.agent]] suspended=true` for the three
auto-start agents (`boot`/`deacon`/`mayor` — verified by `gc start --dry-run`
reporting "0 agents would start") successfully stopped autonomous spawns. Over a
90 s window:

| window | Com_select Δ | rate |
|---|--:|--:|
| 0–45 s (controller ticking) | 422 | ~9.4 /sec |
| 45–90 s | 2125 | ~47 /sec |

**But the run was contaminated:** the controller dispatched **housekeeping pool
work** that spawned an ephemeral `gastown.dog` agent (a real
`claude --dangerously-skip-permissions`) despite the autonomous agents being
suspended. The 45–90 s spike is that dog's startup `work_query` pipelines, not
controller idle. The dog was killed within ~2 min; only a brief, bounded number
of dog invocations occurred.

**Finding:** a gastown city has **no truly agentless idle state** — the
controller's own housekeeping orders route work to the dog pool, spawning agents
on-demand. Suspending the autonomous agents is not sufficient to quiesce it.
A clean controller-only number needs *also* disabling the housekeeping orders
that generate dog work (or measuring on a minimal non-gastown city with core
orders only). Even so, the early/uncontaminated signal (~9 `Com_select`/sec) and
the per-eval figure (~56 ÷ 30 s ≈ 2 /sec from order dispatch) both confirm the
idle native-store rate is **single-digit to low-tens /sec — modest**, orders of
magnitude below the 463/sec active-town figure.

| Shape | window | Com_select/sec | notes |
|---|---|---|---|
| native-store, autonomous-agents-suspended | 90 s | ~9 idle → ~47 w/ housekeeping dog | contaminated by pool-dog spawn |
| clean controller-only (orders+housekeeping disabled) | | | TODO — needs order-level quiesce |

## What the numbers decide

- **Overall idle bd/sec on the native-store baseline** — if already near-zero,
  the design narrows to Pillar 1 (tick backoff) only, or closes.
- **`order-dispatch` vs `tick-body` vs `bead-event-watcher` split** — picks
  whether Pillar 2 (per-pass snapshot coalescing) is worth it, and which path.
- **Suspended-rig shape ≈ single-rig?** — confirms Pillar 3 is already covered
  by #3097/dispatch-skip; any excess isolates the residual (e.g. `gc rig list
  --json` runtime probes, phantom event cursors).
- **`patrol` vs `poke` trigger split** — sizes the Pillar 1 (demand-gated tick)
  win: a high `patrol` share with low `poke` means most reads are the fixed
  cadence sweeping with nothing to do.

## Exit criteria

A filled-in results table + a one-paragraph readout that (a) states the residual
idle rate post-#3097, (b) confirms or revises the Pillar-3 "already shipped"
claim, and (c) gives a go/no-go for Pillars 1 and 2 with the scope split as
evidence.
