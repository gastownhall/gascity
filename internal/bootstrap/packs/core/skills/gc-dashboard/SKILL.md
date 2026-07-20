---
name: gc-dashboard
description: API server and web dashboard — config, start, monitor
---

# Dashboard

The dashboard is a web UI compiled into the `gc` binary for monitoring
convoys, agents, mail, rigs, sessions, and events in real time.

## Prerequisites

The dashboard SPA is embedded in the `gc` binary and served same-origin by a
GC API server — it is no longer a separate static server. It needs a GC API
server to talk to, but it no longer has to be launched from inside a city
directory.

### Standalone city mode

If you are using `gc start` without the machine-wide supervisor, the dashboard
talks to that city's own API server. Ensure the city API is enabled in
`city.toml`:

```toml
[api]
port = 9443
```

Then start the city normally with `gc start`. The API server starts with the
controller on that port. This `[api]` port is the CITY's API server, not a
dashboard port — the dashboard itself has no port flag (see below); it only
needs to know which API server to talk to.

### Supervisor mode

If you are using the machine-wide supervisor, the dashboard talks to the
supervisor API instead. The default supervisor API address is:

```text
http://127.0.0.1:8372
```

In this mode, per-city `[api]` ports are ignored. The dashboard detects
supervisor mode automatically via `/v0/cities`, enables a city selector, and
routes requests through `/v0/city/{name}/...`.

## Starting the dashboard

```
gc dashboard                               # Resolve + open the dashboard in your browser
gc dashboard --no-open                    # Print the dashboard URL instead of opening a browser
gc dashboard serve                        # Print where the web dashboard is served
gc dashboard --city /path/to/city         # Optional city context for standalone discovery
gc dashboard --api http://127.0.0.1:8372 # Optional API server override
```

There is no `--port` flag: the dashboard SPA is embedded in the `gc` binary and
served same-origin by the API server (supervisor or standalone), not by a
separate server with its own port.

`gc dashboard` auto-discovers the right API server in this order:

- Supervisor-managed city: uses the machine supervisor API and defaults the UI
  to the supervisor view. Pick a city in the UI.
- Standalone city context: uses that city's configured `[api]` listener.
- No city context: if the machine supervisor is running, uses the supervisor
  API and shows supervisor-level state.

The `--api` flag remains available as an override for non-standard setups.

## Features

The dashboard provides:

- **Convoys** — progress tracking, tracked issues, create new convoys
- **Crew** — named worker status with activity detection
- **Polecats** — ephemeral worker activity and work status
- **Activity timeline** — categorized event feed with filters
- **Mail** — inbox with threading, compose, and all-traffic view
- **Merge queue** — open PRs with CI and mergeable status
- **Escalations** — priority-colored escalation list
- **Ready work** — items available for assignment
- **Health** — system heartbeat and agent counts
- **Issues** — backlog with priority, age, labels, assignment
- **Command palette** (Cmd+K) — execute gc commands from the browser

Real-time updates via SSE (Server-Sent Events) from the API server.
