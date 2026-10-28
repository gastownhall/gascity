---
title: Reconciler Debugging
description: How to use gc trace when the session reconciler behaves unexpectedly.
---

## When To Use This

Use this workflow when the session reconciler does something unexpected:

- a template does not start when you expect it to
- a session drains, restarts, or quarantines unexpectedly
- a config change appears to be ignored
- acceptance or integration tests fail in controller or lifecycle paths

The trace stream is persisted locally under `.gc/runtime/session-reconciler-trace/`.

If you see `gc convoy control --serve` warning about a legacy control-dispatcher
trace path at `${GC_CITY}/control-dispatcher-trace.log`, treat it as a rollout
action item, not just a symptom: any long-lived control-dispatcher session that
still carries that baked-in `GC_WORKFLOW_TRACE` must be restarted or recycled
after the upgrade so it picks up the watcher-safe default under
`.gc/runtime/control-dispatcher-trace.log`.

## Fast Incident Workflow

From the city root, start detail tracing on the exact normalized template:

```bash
gc trace start --template repo/polecat --for 20m
```

Arm the template even when you only suspect a pool is sitting correctly idle:
the `pool_desired.compute` / `no_demand` / `skipped` decision — the positive
evidence that the reconciler reached that template and found nothing to do — is
detail-tier, so it reaches the persisted trace only once the template is armed.

If you want live visibility while reproducing:

```bash
gc trace tail --template repo/polecat --since 5m
```

After the bug happens, collect the high-signal summary first:

```bash
gc trace status
gc trace reasons --template repo/polecat --since 20m
gc trace show --template repo/polecat --since 20m --type cycle_result --json
```

From the suspicious `cycle_result`, grab the `tick_id`, then dump the full cycle and the full time window:

```bash
gc trace cycle --tick <tick_id> > /tmp/polecat-cycle.json
gc trace show --template repo/polecat --since 20m --json > /tmp/polecat-trace.json
```

When you are done:

```bash
gc trace stop --template repo/polecat
```

## What To Send An Agent

Point the next agent at these artifacts:

- city path
- exact normalized template, for example `repo/polecat`
- what you expected and what actually happened
- approximate UTC time window
- `gc trace reasons --template <template> --since <window>`
- `/tmp/<template>-trace.json` from `gc trace show ... --json`
- suspicious `tick_id`
- `/tmp/<template>-cycle.json` from `gc trace cycle --tick ...`
- controller stdout or stderr for the same window
- `.gc/events.jsonl` for the same window
- anything under `.gc/runtime/session-reconciler-trace/quarantine/` if it exists

If a real session existed and the bug crossed into runtime behavior, also include the relevant session or provider logs.

Drain-ack stop completion is event-first: newer controllers record
`SessionStopped` with message `drain acknowledged by agent` instead of relying
on the old `Stopped drain-acked session` stdout line. Use `.gc/events.jsonl`
and the trace `operation` records as the durable signal, with stdout/stderr as
supporting diagnostics only.

## Rig-Scoped Convergence Rollback

Before rolling back a release that has created rig-scoped convergence loops,
stop active loops in each affected rig:

```bash
gc --rig <rig-name> converge list
gc --rig <rig-name> converge stop <bead-id>
```

Older controllers only watch the city/HQ convergence store. If rollback happens
with active rig-scoped convergence beads still present, those loops become
crash-orphans until a controller with rig-scoped convergence support runs again.

## How To Read The Trace

These record types are usually the fastest path to the bug:

- `cycle_result`: per-tick rollup, dropped records, reason and outcome counts
- `template_tick_summary`: why a template did or did not produce work
- `template_config_snapshot`: effective config and provenance for the tick
- `decision`: branch choices inside the reconciler
- `operation`: scale check, start, interrupt, and drain boundary calls
- `mutation`: bead or runtime writes that actually landed

## Sessions Skipped In An Unknown State

The reconciler skips, on purpose, any open session row whose raw `state` is
one this gc version's controller does not manage, and never acts on it. The
skip is a durable, throttled signal, not an error — but a session parked
there holds its pool slot (or its named row) until someone releases it.

**Symptoms.**

- The trace decision `reconciler.session.skip_unknown_state` (reason
  `unknown_state_skipped`, payload `state`) on the same session, every tick
  that observes it.
- The `session.unknown_state` event on first sight or on a state change, and
  once more with `escalated=true` after the row has sat unrecognized for 30
  minutes.
- The session bead's `unknown_state_first_seen`, `unknown_state_value` and
  `unknown_state_escalated_at` markers.
- Under the v2 controller, the `reconciler.alert` of kind `unknown-state`, and
  the boot refusal about enterprise-era session rows.

**Listing them.**

```bash
gc doctor --check v2-session-migration --verbose   # runs in either mode; lists the IDs
```

or read the `session.unknown_state` events.

**Causes.**

- A newer or older gc writer in a mixed-version fleet: the skip is what makes
  forward-compatible rollback safe.
- This version's own archived rows (retired named duplicates, removed named
  sessions), parked open on purpose — their signal is expected.
- A close whose `state=<close code>` landed but whose status write failed on
  a store without the atomic close.
- Pre-#1912 builds' `failed-create` rows.

**Recovery.**

```bash
gc session close <id>
```

A pool seat's slot is freed and the pool spawns a replacement; a configured
named session gets a fresh canonical row. Or run the gc version that wrote
the state. `gc session wake` and `gc session reset` (and the API wake and
reset) refuse such rows by design — the recorded wake aside, nothing takes
effect until the row is released — and `gc session kill` on one answers
"session is not active" and names the close.

## Acceptance And Integration Failures

For acceptance or integration failures, keep baseline tracing as-is and collect trace artifacts on failure. Prefer template-scoped detail tracing only for tests that intentionally exercise reconciler or lifecycle behavior.

On failure, collect at least:

```bash
gc trace status
gc trace reasons --since 15m
gc trace show --since 15m --type cycle_result --json
gc trace show --since 15m --json
```

For tests that know the target template ahead of time, arm tracing in setup:

```bash
gc trace start --template repo/polecat --for 15m
```

Then dump the template-scoped window on failure:

```bash
gc trace show --template repo/polecat --since 15m --json
```
