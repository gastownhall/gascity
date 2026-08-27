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

## Cold-disable and re-enable the keyed reconciler

`session_reconciler` is boot-latched: validate and atomically replace the city
root, then restart the supervisor. There is no `gc config set` command for this
field. Run the following from the city root to cold-disable it. The candidate is
in the same directory as `city.toml`, so the final rename is atomic.

> **Warning:** `gc supervisor stop --wait` is the portable supported path, but
> it is machine-wide and destructive to live runtime state: it stops every
> managed city and its live sessions. Durable work survives and can converge
> again after restart, but tmux identities are not preserved. There is no
> universal CLI command for a preserve-in-place supervisor restart today.

```bash
set -eu
candidate=.city.toml.reconciler-next
trap 'rm -f "$candidate"' EXIT
awk '
  /^[[:space:]]*session_reconciler[[:space:]]*=/ {
    if (++n != 1) exit 42
    sub(/"[^"]*"/, "\"off\"")
  }
  { print }
  END { if (n != 1) exit 42 }
' city.toml > "$candidate"
gc config show --validate --root-file "$candidate"
mv -f "$candidate" city.toml
gc supervisor stop --wait
gc supervisor start
gc trace status
```

The final status must report `configured mode: off` and `effective owner:
legacy`. Keep any incident trace separately; after the old supervisor exits its
`controller_instance_id` must not appear on new shadow records.

The exact-binary cold-disable acceptance journey also covers service-managed
preserve semantics: its test-owned supervisor opts into preserve-on-SIGTERM and
proves tmux identity continuity. That is a separate service configuration, not
a property of the portable copy/paste procedure above.

To re-enable the rollout path, repeat the same sequence with `"auto"` in place
of `"off"`. `gc trace status` must then report `configured mode: auto` and,
when the keyed capability is available, `effective owner: keyed`. Start a fresh,
bounded trace arm before the reproduction:

```bash
gc trace start --template repo/polecat --for 20m --level detail
gc trace show --template repo/polecat --since 20m --json > /tmp/reconciler-after-reenable.json
```

New records must carry a different `controller_instance_id`; do not treat old
records in the append-only trace store as activity by the new controller.

The arm buys DETAIL — the decisions, refusals and yields behind an outcome. It
is not what proves the keyed engine is running. A keyed handler that commits an
effect writes an always-on record carrying `effect_owner=keyed` and
`effect_applied=true`, so an unarmed city already answers "is the opt-in
acting?":

```bash
gc trace show --since 1h --json |
  jq -c '.records[] | select(.fields.effect_owner == "keyed" and .fields.effect_applied == true)'
```

Empty output on a city that has reconciled something means the keyed engine did
not act; it no longer means the trace was switched off.

## Canary queued nudge target selection

`nudge_shadow` is boot-latched. To canary it on an existing city, prepare a
same-directory `city.toml` candidate with this exact daemon tuple and validate
the candidate before stopping anything:

```toml
[daemon]
nudge_dispatcher = "supervisor"
session_reconciler = "off"
nudge_shadow = "required"
```

```bash
cp -f city.toml .city.toml.nudge-shadow-next
# Edit .city.toml.nudge-shadow-next, then:
gc config show --validate --root-file .city.toml.nudge-shadow-next
gc supervisor stop --wait
mv -f .city.toml.nudge-shadow-next city.toml
gc supervisor start
```

The portable stop is destructive as described above; service-managed
preserve-in-place restarts need their own verified supervisor configuration.
Run this canary only while the city is quiescent. Inspect `gc nudge status
<session-id-or-alias> --json` for every live session and require zero pending
and in-flight items everywhere. The canary must be the single queued item in
the whole city; if unrelated work is queued concurrently, abort, let it drain,
and retry from a fresh trace cursor.

After restart, note the `head_seq` from `gc trace status --json`, enqueue one
unique canary, and inspect only later records. Once nudge status is terminal,
poll trace status until `head_seq` stays unchanged for a bounded two-second
flush window (restart that window whenever the head advances), then read the
records:

```bash
gc session nudge <session-id-or-alias> "nudge-shadow-canary-$(date +%s)" --delivery=queue --json
gc trace show --since 5m --json
gc nudge status <session-id-or-alias> --json
```

Select the single later `nudge.due_target_selection.shadow` record whose
`queue_item_count` is `1`. It must report
`scope=queued_exact_due_target_selection`, candidate and legacy counts of `1`,
equal 64-character digests, `comparison_outcome=matched`,
`legacy_effect_owner=true`, and `shadow_effect_applied=false`. The timing fields
must be non-negative. The trace must not contain the raw session ID, session
name, alias, or canary message. Verify the canary appears exactly once in the
target's visible transcript and that nudge status reports zero pending,
in-flight, and dead items.

If `queue_item_count` is not exactly `1`, another item coexisted with the
canary; discard that observation and retry after the entire city queue is
empty. Re-read after the bounded flush window and require the one-item record
count to remain exactly one.

Rollback is the same cold procedure with `nudge_shadow = "off"`. Record a new
trace cursor after the off successor is ready, enqueue a fresh canary, and
confirm legacy delivery still occurs exactly once. After the queue drains,
wait the same bounded two-second quiet/flush window before declaring that no
later `nudge.due_target_selection.shadow` record was created.

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
