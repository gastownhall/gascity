---
title: Runbook — Managed-city Dolt endpoints
description: Mental model, forbidden edits, sanctioned escape hatches, and recovery recipe for the city-level Dolt endpoint architecture.
---

# Runbook: Managed-city Dolt endpoints

This runbook is for mayors and operators. It covers the endpoint
architecture introduced in the
[beads-and-Dolt contract redesign](https://github.com/gastownhall/gascity/blob/main/engdocs/design/beads-dolt-contract-redesign.md)
— specifically the case where rigs **inherit** their Dolt endpoint
from a single city-managed server. If you came here because `gc sling`
is dropping work, `supervisor.log` shows `rigStores=0`, or your port
edits keep reverting, you are in the right place.

> **Rules of thumb** — skip to [What NOT to do](#what-not-to-do) if
> you are mid-incident. Come back for the mental model after.

## Mental model

A city runs **one** Dolt SQL server. Every rig is a **logical
database** inside that Dolt. Each rig's `.beads/dolt-server.port` is
a **compatibility mirror** that lets raw `bd` commands reach the
city Dolt — it is not the rig's own server.

```
┌──────────────────────────────────────────────────────────────────┐
│ city/.gc/runtime/packs/dolt/dolt-state.json                      │
│   └─ managed Dolt SQL server @ 127.0.0.1:<P>  (one per city)     │
│        ├─ database "hq"                       (city HQ scope)    │
│        ├─ database "<rigA-prefix>"            (rig A logical DB) │
│        ├─ database "<rigB-prefix>"            (rig B logical DB) │
│        └─ database "<rigC-prefix>"            (rig C logical DB) │
└──────────────────────────────────────────────────────────────────┘

Per-rig state:
  <rig>/.beads/config.yaml   — canonical endpoint marker
  <rig>/.beads/dolt-server.port  — compatibility mirror of port P
```

### Endpoint origins

Two TOML fields, one per scope, describe endpoint ownership:

| Scope | Field | Values |
|-------|-------|--------|
| City | `gc.endpoint_origin` | `managed_city`, `city_canonical` |
| Rig  | `gc.endpoint_origin` | `inherited_city`, `explicit` |

- **`managed_city`** — The city owns the Dolt lifecycle. `gc start`
  launches it; `gc stop` shuts it down. This is the default for
  fresh `bd`-backed cities.
- **`city_canonical`** — The city declares an explicit, externally
  managed Dolt. GC does not launch or manage the server.
- **`inherited_city`** — The rig reuses its parent city's endpoint.
  No rig-local Dolt. This is the default for rigs added to a
  `managed_city` city.
- **`explicit`** — The rig pins its own external endpoint. Use for
  cross-city rigs or rigs pointing at a shared Dolt cluster.

## What NOT to do

These are the edits mayors try first, all of which self-revert at
the next `gc start` and waste an afternoon.

### Do not run `bd dolt set port <N>` inside an inherited rig

If `gc.endpoint_origin: inherited_city`, the rig's canonical port
is whatever the city's managed Dolt is listening on. `bd` will
accept the `set port` command and update `.beads/config.yaml`, but
on the next `gc start` the city's normalization pass
(`syncConfiguredDoltPortFiles`) restores the managed port and
rewrites `.beads/dolt-server.port` back. Your change disappears.

### Do not hand-edit `.beads/dolt-server.port`

Same reason. The file is a **compatibility mirror** for raw `bd`
commands — it reflects the city-managed port, not an authoritative
choice. `gc start` atomically rewrites it to match the managed
port on each startup.

### Do not delete the `gc.endpoint_origin: inherited_city` line

Deleting the line does not switch the rig to `explicit`. It puts
the rig into an unverified state. `gc start`'s verification pass
will re-derive the origin from topology and write the line back.

### Do not run `bd dolt start` inside an inherited rig

Any Dolt server you start this way is orphaned. GC does not track
it, will not shut it down on `gc stop`, and will not publish its
port to dependent sessions. If the rig's `.beads/dolt-server.port`
points at this orphan server, the drift check will flag it on next
`gc doctor` run (see [Drift symptoms](#drift-symptoms) below).

## Sanctioned escape hatches

When you genuinely need to change a rig's endpoint, use `gc rig
set-endpoint` — it is the single owning operation for endpoint
topology, and it refuses footgun configurations up front.

### Make this rig run its own Dolt

```bash
gc rig set-endpoint <rig> --self --port <P> --force
```

`--self` marks the rig as running its own `127.0.0.1:<P>` Dolt.
While the city is in `managed_city` mode the command refuses
unless `--force` is given, and emits a one-line WARN explaining
the rig now falls outside `gc start`'s supervision. The rig's
origin flips to `explicit`; its port file will no longer track
the managed city Dolt.

### Rejoin the city

```bash
gc rig set-endpoint <rig> --inherit
```

Flips the rig back to `inherited_city`. The next `gc start` will
re-publish the managed city port into `.beads/dolt-server.port`
and the rig's sessions will talk to the city Dolt again.

### Point at an external Dolt

```bash
gc rig set-endpoint <rig> --external --host <H> --port <P> --user <U>
```

Pins `explicit` origin to `<H>:<P>`. Use this for cross-city rigs
or rigs pointing at a shared/external Dolt cluster. Add
`--adopt-unverified` if you need to record the endpoint before the
target is reachable.

### Inspect current state

```bash
bd dolt show        # bd's view (note: priority-display is under review)
gc doctor           # run the `dolt-drift` check + other health checks
```

`gc doctor` is authoritative for GC-side state: it reports the
managed city port, each rig's resolved endpoint origin, and any
drift it detects between the two. The `dolt-drift` check only
registers when the workspace is in `bd`-backed `managed_city`
topology — if you do not see it, your workspace is not in that
mode.

## `.beads/dolt-server.port` priority explainer

When raw `bd` commands resolve a port, they consult sources in
this order (first hit wins):

1. `$BEADS_DOLT_SERVER_PORT` environment variable
2. `.beads/dolt-server.port` file (the compatibility mirror)
3. `.beads/config.yaml` `dolt.port` key (or the global beads
   config equivalent)
4. `.beads/metadata.json` historical record

GC runtime publishes the managed port into `$BEADS_DOLT_SERVER_PORT`
for sessions it launches, which is why sessions keep working even
when the on-disk mirror is transiently out of date. For raw
interactive `bd` invocations the mirror file is what matters, which
is why GC aggressively rewrites it during normalization.

The authoritative source for this order is `DefaultConfig` in
[`beads/internal/doltserver/doltserver.go`](https://github.com/gastownhall/beads/blob/main/internal/doltserver/doltserver.go).
`bd dolt show` advertises a slightly different order in its output
text; that is a known cosmetic bug tracked separately in the beads
repo and does not affect actual resolution.

## Drift symptoms

The `dolt-drift` check (`cmd/gc/cmd_doctor_drift.go`) is registered
automatically when the workspace is in `bd`-backed `managed_city`
topology and catches three shapes of drift:

| Shape | Severity | Trigger |
|-------|----------|---------|
| Live rig-local Dolt under `inherited_city` | Error | Rig's `.dolt/sql-server.info` lists a PID that is alive, but its canonical origin is still `inherited_city`. |
| Port mismatch under `inherited_city` | Error | Rig's `.beads/dolt-server.port` disagrees with the managed city port from `.gc/runtime/packs/dolt/dolt-state.json`. |
| Stale `.dolt/sql-server.info` | Warning | Info file exists under an inherited rig but its PID is no longer alive. |

All three carry `FixHint` text pointing at the specific
`gc rig set-endpoint` invocation that resolves them.

At `gc start`, the Dolt-port normalization pass also emits a WARN
line on stderr for each rig whose `.beads/dolt-server.port` it is
rewriting back to the managed port — that is your early warning
that a prior edit or orphan server made the mirror drift.

## Recovery recipe — slung beads not reaching agents

If you see `gc sling` accepting work but agents not processing it,
and the supervisor log reports `rigStores=0` or
`assignedWorkBeads=0`, that is almost always a symptom of the rig's
Dolt view diverging from the city's. Work the following sequence:

1. **Stop the city:**

   ```bash
   gc stop
   ```

2. **Kill any live rig-local Dolt servers**, rig by rig. For each
   rig, check `.dolt/sql-server.info` for a PID and verify it:

   ```bash
   cd <rig>
   if [ -f .dolt/sql-server.info ]; then
     pid=$(awk -F= '/^pid=/{print $2}' .dolt/sql-server.info)
     if kill -0 "$pid" 2>/dev/null; then
       echo "killing rig-local dolt pid $pid in $(pwd)"
       kill "$pid"
     fi
   fi
   ```

3. **Confirm the rig's canonical state is `inherited_city` and
   verified:** each rig's `.beads/config.yaml` should contain:

   ```yaml
   gc:
     endpoint_origin: inherited_city
     endpoint_status: verified
   ```

   If any rig says something else and you want it to rejoin, run
   `gc rig set-endpoint <rig> --inherit`.

4. **Delete the compatibility mirror in each rig**, so it is
   regenerated fresh on next start:

   ```bash
   rm -f <rig>/.beads/dolt-server.port
   ```

5. **Start the city:**

   ```bash
   gc start
   ```

6. **Verify only one Dolt is running on the managed port**:

   ```bash
   bd dolt show   # should name the managed city port
   lsof -iTCP:<managed-port> -sTCP:LISTEN
   ```

7. **Test a sling end-to-end** and watch the supervisor log for
   `assignedWorkBeads > 0`:

   ```bash
   gc sling <template> <test-bead-id>
   tail -f ~/.gc/supervisor.log
   ```

If the symptom persists after step 7, the cause is likely not
endpoint drift. Open an investigator bead referencing this
runbook and attaching the full `gc doctor --verbose` output plus
the `supervisor.log` slice covering the last sling attempt.

## References

- Design: [Beads-and-Dolt contract redesign](https://github.com/gastownhall/gascity/blob/main/engdocs/design/beads-dolt-contract-redesign.md)
  — the authoritative architecture document.
- Code: [`cmd/gc/cmd_doctor_drift.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/cmd_doctor_drift.go)
  — drift detector.
- Code: [`cmd/gc/cmd_rig_endpoint.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/cmd_rig_endpoint.go)
  — `gc rig set-endpoint` implementation.
- Code: [`cmd/gc/beads_provider_lifecycle.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/beads_provider_lifecycle.go)
  — `syncConfiguredDoltPortFiles` normalization.
- Upstream: [`beads/internal/doltserver/doltserver.go`](https://github.com/gastownhall/beads/blob/main/internal/doltserver/doltserver.go)
  `DefaultConfig` — source of truth for port-resolution priority.
