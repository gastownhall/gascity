# Argos — rate-limit-recovery watchdog pack

Argos is the city's **watchdog**: an LLM agent that watches every live
session and recovers the ones that have stalled — the canonical case being
a session sitting behind a rate-limit wall while it still holds claimed,
in-progress work. Named after Odysseus's hound.

It is modeled on the in-tree `boot` agent
(`../gastown/agents/boot/`): a city-scoped, always-resident
named session that the controller re-wakes each patrol tick, runs a single
pass of triage with a fresh provider context, then drain-acks and exits.

## Status: read-only detection

Argos now **detects and classifies**, but still takes no action. Each wake
runs one single-pass triage:

1. wake (fresh context, single pass),
2. enumerate `gc session list --json` and pre-filter candidates
   (`provider == claude`, alive, `last_active` as a loose advisory),
3. cross-reference claimed work — join each candidate's `session_name`
   against the `assignee` of an `in_progress` bead,
4. confirm by reading the pane (`gc session peek`),
5. classify each candidate — `healthy` / `idle-no-work` /
   `rate-limit-stalled` / `context-frozen` — and print one verdict line,
6. `gc runtime drain-ack` and exit.

The **fire gate** for a recoverable stall is a conjunction: the pane shows
the rate-limit marker as its current state **and** the session holds a
claimed `in_progress` bead. Only `rate-limit-stalled` trips it in v1.

All of this judgment lives in the **prompt**, not in Go (Zero Framework
Cognition: the model reads the pane and decides; no Go string-matcher, no
per-provider format zoo). **No recovery yet** — that is the next step.

Roadmap after this detection stage:

- **Recovery** — nudge the stalled session (suspended → wake, then nudge),
  with anti-storm tiered backoff.
- **City wiring + tests + docs** — promote from the example city to the
  target deployment, integration test across several patrol ticks.

## Why Argos is its own pack (pack-home decision)

The watchdog could have been added as a sibling of `boot` inside the
`gastown` pack, or folded into the generic `maintenance` pack. It is a
**dedicated, composable pack** instead, for three reasons:

1. **It is not a Gas Town role.** `gastown` defines a specific domain
   roster — mayor, deacon, boot, witness, refinery, polecat. The watchdog
   is generic infrastructure that enumerates *all* sessions and is blind
   to roles; welding it into `gastown` would muddy that domain definition
   (and its roster tests).
2. **It is reusable, role-agnostic infrastructure** — exactly the kind of
   thing a city "includes alongside any domain pack," the same role
   `maintenance` plays. A self-contained pack lets any city opt in with a
   single import.
3. **Cohesion.** The whole Argos capability (scaffold → detection →
   recovery → wiring) lives in one pack with one identity, rather than
   sprawling across an unrelated domain pack.

`maintenance` was rejected as the home because it is the **non-LLM**
housekeeping layer (exec orders + a utility dog pool); Argos is an
always-resident **LLM triage agent** modeled on `boot`, so it belongs with
its own identity rather than buried among shell orders.

## How to compose it

Import the pack from a city's **root** pack definition so its
city-scoped named session expands into the city:

```toml
[imports.argos]
source = "packs/argos"
```

The `examples/gastown` city does exactly this, so Argos comes up as a
city-scoped agent (`mayor`, `deacon`, `boot`, `dog`, `argos`) there.

## Layout

```
packs/argos/
├── pack.toml                       ← [pack] argos + [[named_session]] always
├── README.md                       ← this file
└── agents/argos/
    ├── agent.toml                  ← scope=city, wake_mode=fresh, max_active=1
    └── prompt.template.md          ← single-pass read-only detection triage
```
