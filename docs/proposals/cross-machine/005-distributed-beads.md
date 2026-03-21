---
title: "Distributed Beads Store"
type: satellite-issue
epic: 000-epic-cross-machine-city
status: proposed
component: beads
current_state: partially-implemented
priority: highest
author: trillium
date: 2026-03-21
labels: [beads, dolt, networking, data]
---

# Distributed Beads Store

## Parent Epic

[Epic: Cross-Machine City Operation](000-epic-cross-machine-city.md)

## Summary

The beads store is the closest component to working cross-machine. Dolt is already a
network-accessible SQL database, and the BdStore implementation already supports remote
connections. This issue tracks what's working, what's not, and what needs to happen for
reliable cross-machine bead access.

## Current State: Partially Implemented

### What Works

**BdStore** (`internal/beads/bdstore.go`) connects to Dolt via the `bd` CLI, which
talks to a Dolt SQL server over TCP. The connection is configured via environment
variables:

| Variable | Purpose |
|----------|---------|
| `GC_DOLT_HOST` | Dolt server hostname/IP |
| `GC_DOLT_PORT` | Dolt server port |
| `GC_DOLT_USER` | Authentication username |
| `GC_DOLT_PASSWORD` | Authentication password |

**K8s Integration**: Pods already receive Dolt connection info via environment
variables (`GC_K8S_DOLT_HOST`, `GC_K8S_DOLT_PORT`), and the beads metadata is
patched with the correct server address for the pod environment.

**In practice**: If you run a Dolt server on mini2 and point agents on mini3 at it,
the beads store should work over the network today.

### What's Missing

1. **No automatic Dolt server exposure**: The controller starts Dolt on localhost.
   For cross-machine access, it needs to bind to a routable address.

2. **Single Dolt instance**: No replication or failover. If the Dolt server goes
   down, all agents (local and remote) lose bead access.

3. **No connection resilience**: BdStore calls `bd` CLI commands that connect per
   invocation. Network interruptions cause immediate failures with no retry.

4. **No cross-city bead sharing**: Each city has an independent store. Federation
   (reading beads from another city) is not supported.

5. **Latency considerations**: Every bead operation is a CLI invocation over the
   network. For high-frequency operations (hook checks, mail polling), this could
   be slow.

6. **Credential propagation**: Satellite machines need Dolt credentials. No
   mechanism to distribute these securely.

## What Needs to Happen

### Minimum Viable (get cross-machine beads working)

1. **Dolt bind address**: Configure Dolt to listen on `0.0.0.0` or a specific
   interface instead of `localhost`
2. **Firewall/Tailscale**: Ensure Dolt port is accessible across Tailscale network
3. **Environment propagation**: When spawning remote agents (via SSH provider),
   pass `GC_DOLT_HOST` and `GC_DOLT_PORT` pointing to hub

### Reliability Improvements

4. **Connection retry**: Wrap BdStore operations with retry logic for transient
   network failures
5. **Health check**: Include Dolt connectivity in doctor checks for remote machines
6. **Connection pooling**: Reduce per-operation connection overhead

### Future Enhancements

7. **Dolt replication**: Read replicas on satellite machines for lower latency
8. **Write-ahead cache**: Buffer bead writes locally, sync to hub asynchronously
9. **Federation**: Cross-city bead queries for multi-city setups

## city.toml Configuration

```toml
[dolt]
host = "0.0.0.0"                        # Bind to all interfaces (for cross-machine)
port = 3307                             # Fixed port (not ephemeral)
# Or:
host = "mini2.hippo-tilapia.ts.net"     # Tailscale hostname
```

## Risk Assessment

**Low risk**: The network path already works (BdStore → bd CLI → Dolt over TCP).
The main work is configuration and reliability, not new architecture.

**Testing**: Can be validated with two machines on Tailscale today without any
code changes — just configuration.

## Dependencies

- None (this is the most independent cross-machine component)

## Dependents

- [007 — Cross-Machine Mail](007-cross-machine-mail.md) (mail is stored in beads)
- [006 — Cross-Machine Events](006-cross-machine-events.md) (events could use beads as transport)
