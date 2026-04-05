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

## Fast Incident Workflow

From the city root, start detail tracing on the exact normalized template:

```bash
gc trace start --template repo/polecat --for 20m
```

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

## How To Read The Trace

These record types are usually the fastest path to the bug:

- `cycle_result`: per-tick rollup, dropped records, reason and outcome counts
- `template_tick_summary`: why a template did or did not produce work
- `template_config_snapshot`: effective config and provenance for the tick
- `decision`: branch choices inside the reconciler
- `operation`: scale check, start, interrupt, and drain boundary calls
- `mutation`: bead or runtime writes that actually landed

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
