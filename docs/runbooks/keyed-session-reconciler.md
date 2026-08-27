---
title: Turn On Keyed Session Reconciliation
description: What `[daemon] session_reconciler = "auto"` changes about how a city keeps its sessions correct, how to switch onto it, and how to switch back — one line of config, latched at boot, with nothing on disk to convert.
---

Every city keeps its running sessions matching what its config asks for: start
what is missing, stop what has finished, wake what has work waiting. By default
that runs as **one pass over every session in the city** on each patrol tick.

`session_reconciler` selects a second engine that does the same job **by key**:
each session that needs attention is handled against its own identity, rather
than re-derived from a pass over the whole fleet. Both engines ship in every
build, and choosing between them is one line of `city.toml`.

This is a change of engine, not of behaviour. The same triggers start, stop and
wake the same sessions under the same config; what changes is which code
decides — and when it decides. Under `auto`, a session that needs attention is
acted on when the triggering work lands, rather than on the next patrol tick.

## What each value means

| Value | What the city does |
|---|---|
| `off` (the default) | One fleet-wide pass per patrol tick decides for every session. |
| `auto` | Keyed handling per session, with the fleet-wide pass covering anything the keyed engine declines. This is the value to switch to. |
| `require` | Keyed handling, and no falling back to the fleet-wide pass for the session starts the keyed engine owns. Read the next section before you set it: `require` is narrower than "the city refuses to start", and on one store class it is worse than `auto`. |

Under `auto`, a session the keyed engine **declines** is decided the way `off`
decides it, on the same tick. A session the keyed engine **takes and cannot
finish** is the case to know about: while it holds that claim the fleet-wide
pass skips the row, so `off`'s pass is not standing behind it. Such a session is
not abandoned — it is retried on a bounded cadence, and a claim that keeps
refusing crosses into a named escalated state (`drain_ack_escalated`) and is
re-examined on the drain's own ack-or-timeout deadline until it resolves or that
deadline hands the row back to detection. So `auto` is the safe end of the
switch, but the honest worst case is a bounded, named delay on the sessions the
keyed engine is holding — not "identical to today."

## What `require` actually refuses

`require` refuses **startup** for one thing only: the exact-start requirements.
Everything else it strengthens is refused at the point of use, not at boot.

The case that matters is a session store that cannot fence its own writes. Every
city checks for this at boot in every mode, and a city that fails the check
prints one startup `ERROR` line naming the store kind and what it lacks. Under
`require` the city **still starts** — and then refuses each pool drain
acknowledgement that store has to serve, so drains wedge until their own
ack-or-timeout deadline fires. Under `auto` the same city is covered, because
the refused acknowledgement hands back to the fleet-wide pass; under `require`
there is no hand-back. "It started, so I am safe" is exactly the wrong reading.

The store class this reaches today is the **bd-hook-backed** one: a scope
carrying executable `.beads/hooks/on_create`, `on_update` or `on_close` handlers
falls back to the `bd` subprocess store, and the pinned `bd` does not advertise
the conditional-write flag those fenced writes need. Because the check runs in
every mode, you can find out before committing: restart once on `auto`, look for
that `ERROR` line in the orchestrator log, and only consider `require` if it is
absent.

## Switch a city

```toml
[daemon]
session_reconciler = "auto"
```

The value is read once, when the city starts. Restart to take it:

```
gc stop
gc start
```

Nothing else changes. No state is converted, no format is rewritten, and no
session is stamped as belonging to one engine.

## Sessions that were already in flight

You do not have to quiet the city first, and there is no drain-and-wait step
before the restart.

A session that was mid-drain when you restarted is re-evaluated against the
state it is actually in when the city comes back, so a drain already under way
finishes normally. The engine acts on where each session is now, not on a
decision taken before the restart, so nothing is left half-applied by the
switch itself.

## Rolling back

Rollback is config-only and always available:

```toml
[daemon]
session_reconciler = "off"
```

then `gc stop` and `gc start` again. Because neither engine writes anything the
other cannot read, a city can move back and forth as often as you like, and a
rollback costs exactly one restart. Reverting to your previous build works too:
a build without the setting behaves the same as `off`.

## What to watch after the switch

- **Session lifecycle.** Sessions start, stop and wake when you expect them to,
  and health patrol stays steady. `gc status` is the fastest read.
- **Provider flaps.** If the session provider is briefly unreachable, the keyed
  engine declines that cycle rather than guessing at what it cannot see. Under
  `auto` the fleet-wide pass covers those sessions until the provider answers
  again.
- **Drains that stop finishing.** Two strings in the orchestrator log name the
  bounded-delay case above. `drain_ack_escalated` is a drain acknowledgement the
  keyed engine has retried past its escalation bound and moved onto the slow
  cadence; `live runtime holds no recognizable drain acknowledgement provenance`
  is the keyed engine parking a hand-off it cannot attribute rather than
  guessing at it. Either one against a session that is not draining within its
  usual window is the signal to roll back to `off`.

The setting and its enum live in the
[configuration reference](/reference/config) under `[daemon]`.
