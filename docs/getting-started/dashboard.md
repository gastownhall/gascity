---
title: Web dashboard
description: The supervisor hosts a built-in web dashboard for all your cities.
---

The Gas City supervisor hosts a built-in web dashboard. It is a single-page app
compiled into the `gc` binary and served by the supervisor on its own listener,
so there is nothing extra to install or run.

## Open it

Start the supervisor, then open the URL it prints:

```bash
gc supervisor start
# Supervisor API listening on http://127.0.0.1:8372
# Dashboard:  http://127.0.0.1:8372/
```

If the supervisor is already running, `gc dashboard` prints where it is served:

```bash
gc dashboard
# The dashboard is served by the gc supervisor at http://127.0.0.1:8372
```

`gc dashboard` is informational only: it does not start a server. The supervisor
is the host, and one supervisor serves every registered city. Pick the city you
want from the switcher in the dashboard header.

## What it shows

The dashboard reads the supervisor's typed API directly (same origin), so it
reflects live state: agents and their sessions, beads, mail, formula runs, and a
health view (system, local tools, per-rig store health, and the dolt store
trend).

## Security posture

The dashboard is served on the supervisor's bind address, which defaults to
loopback (`127.0.0.1`). It is intended for local, single-operator use:

- It is same-origin with the API; browser mutations carry the supervisor's
  `X-GC-Request` CSRF header.
- When the supervisor binds a non-localhost address without
  `allow_mutations`, it runs read-only and the dashboard disables its mutating
  controls.

## Turn it off

Set `GC_SUPERVISOR_DASHBOARD=0` before starting the supervisor to run a
typed-API-only supervisor with no embedded dashboard.
