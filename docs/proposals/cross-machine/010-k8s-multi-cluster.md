---
title: "Kubernetes Multi-Cluster Support"
type: satellite-issue
epic: 000-epic-cross-machine-city
status: proposed
component: k8s
current_state: partially-implemented
priority: lowest
author: trillium
date: 2026-03-21
labels: [k8s, kubernetes, multi-cluster]
---

# Kubernetes Multi-Cluster Support

## Parent Epic

[Epic: Cross-Machine City Operation](000-epic-cross-machine-city.md)

## Summary

The K8s provider is the most mature remote execution capability in Gas City. It already
runs agents as pods in a Kubernetes cluster. This issue covers extending it to support
multiple clusters simultaneously and improving its integration with the cross-machine
architecture.

## Current State: Partially Implemented (single cluster)

### What Exists

**K8s Provider** (`internal/runtime/k8s/`):

- Full `runtime.Provider` implementation using `client-go`
- Pod creation with init containers for file staging
- Dolt server discovery and credential injection
- Tmux sessions inside containers
- Activity tracking, process liveness checks
- Configurable resource limits (CPU, memory)
- Support for prebaked images

**Configuration**:

```toml
[session]
provider = "k8s"

[session.k8s]
namespace = "gc"
image = "ghcr.io/gastownhall/gc-agent:latest"
context = "production"
cpu_request = "500m"
mem_request = "1Gi"
```

### What Works

- Single cluster operation is fully functional
- Pods run agents with full bead access via remote Dolt
- Context switching selects different clusters
- Pod lifecycle management (create, delete, check health)

### What's Missing

1. **Single cluster at a time**: One `context` per city. Cannot spread agents across
   multiple K8s clusters simultaneously.

2. **No cluster-level dispatch**: Cannot say "put polecats in cluster A, dogs in
   cluster B."

3. **No hybrid K8s + bare metal**: Cannot mix K8s pods with tmux sessions on other
   machines in the same city.

4. **No multi-namespace**: Fixed namespace per city. Multi-tenant workloads would
   need separate namespaces.

5. **No cluster health in doctor**: Doctor checks don't verify K8s cluster
   connectivity or resource availability.

## Proposed Design

### Multi-Cluster via Hybrid Provider

Rather than making the K8s provider itself multi-cluster, use the hybrid provider
to combine multiple K8s providers:

```toml
[session]
provider = "hybrid"

[session.hybrid]
local_provider = "tmux"

[[session.hybrid.remote]]
machine = "k8s-staging"
provider = "k8s"
[session.hybrid.remote.k8s]
context = "staging"
namespace = "gc"

[[session.hybrid.remote]]
machine = "k8s-prod"
provider = "k8s"
[session.hybrid.remote.k8s]
context = "production"
namespace = "gc"
```

### Mixed K8s + Bare Metal

The hybrid provider can combine K8s pods with tmux sessions:

```toml
[session]
provider = "hybrid"

[session.hybrid]
local_provider = "tmux"                # mini2: local tmux agents

[[session.hybrid.remote]]
machine = "mini3"
provider = "tmux"                       # mini3: remote tmux over SSH
transport = "ssh"

[[session.hybrid.remote]]
machine = "k8s-cluster"
provider = "k8s"                        # K8s: pod-based agents
[session.hybrid.remote.k8s]
context = "production"
```

This gives a single city spanning local machines + K8s clusters.

### Agent-to-Cluster Affinity

```toml
[[agent]]
name = "polecat"
[agent.pool]
min = 0
max = 10
machines = ["mini2", "mini3", "k8s-cluster"]
dispatch_policy = "least-loaded"
```

## Relationship to Other Cross-Machine Work

The K8s provider is already a working example of remote agent execution. The
cross-machine work (issues 001–009) generalizes this pattern to arbitrary machines.
Key learnings from K8s:

- **Pod = remote session**: The abstraction works
- **Dolt over network**: Already proven in K8s pods
- **Init containers**: File staging pattern for remote environments
- **Environment injection**: Credential propagation via env vars

These patterns should inform the SSH provider design (003).

## Dependencies

- [002 — Hybrid Provider Config](002-hybrid-provider-config.md) (multi-backend routing)

## Dependents

- None
