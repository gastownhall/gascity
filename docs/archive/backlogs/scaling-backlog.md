---
title: "Scaling Backlog"
---

Phased improvements for running Gas City at scale. Phase 1 items are
pure algorithm changes that benefit all providers (tmux, exec, K8s).
Later phases are K8s-specific infrastructure.

## Phase 1: Algorithm Changes (all providers)

These reduce per-tick I/O calls from O(N) per-agent subprocess invocations
to O(1) batch calls + O(N) map lookups.

- [x] **Pre-fetch running set in reconcile loop** — Call `listRunning()`
  once at top of `doReconcileAgents`, skip `IsRunning()`+`ProcessAlive()`
  for sessions not in set, reuse for orphan cleanup (eliminates second
  `listRunning()` call). Saves N provider calls per tick for non-running
  agents.

- [x] **Replace `countRunningPoolInstances` with `ListRunning`** — Current
  code calls `sp.IsRunning()` per pool instance (1..max). Replace with
  single `sp.ListRunning()` call + set intersection. For pool max=100,
  saves 99 provider calls.

- [x] **Parallelize pool `scale_check` commands** — Pool scale checks are
  independent shell commands. Run them concurrently with goroutines in
  `buildAgents`. For 5 pools with 2s checks each, wall-clock drops from
  ~10s to ~2s.

## Phase 2: K8s Infrastructure

- [ ] **`gc-events-dolt`** — Replace ConfigMap-based events with a Dolt
  table. Eliminates CAS counter serialization bottleneck. ConfigMap list
  performance degrades after ~1000 events; Dolt handles millions of rows.

- [x] **Event cleanup CronJob** — Prune event ConfigMaps older than N
  hours. Prevents unbounded accumulation that slows list operations.

- [x] **Scale mcp-mail** — Bump resources to 500m/512Mi request,
  1 CPU / 1Gi limit. SQLite single-writer means multiple replicas
  won't help; scale the single replica instead.

- [x] **Scale Dolt** — Increase to 500m/1Gi request, 2 CPU / 4Gi limit,
  raise max_connections to 500, storage to 20Gi.

- [x] **Increase `patrol_interval`** — Added scaling guidance to
  example-city.toml (30s recommended for 50+ agents).
  Controller resource env vars added to gc-controller-k8s.

## Phase 3: Native Providers

- [x] **Native K8s session provider** — `internal/session/k8s/` package
  using `client-go` for direct API calls over reused HTTP/2 connections.
  Eliminates ~300 kubectl subprocesses per tick at 100 agents. Pod
  manifests compatible with gc-session-k8s for mixed-mode migration.
  Configured via `[session] provider = "k8s"` or `GC_SESSION=k8s`.

- [ ] **MCP mail PostgreSQL backend** — Replace SQLite with PostgreSQL
  for concurrent writes. SQLite serializes at ~15-20 agents under burst.

- [x] **Bake city into agent image** — `gc build-image` assembles a
  Docker context and builds a prebaked image. `prebaked = true` in
  `[session.k8s]` skips init containers and staging. Pod startup
  drops from 30-60s to seconds.

## Scaling Estimates

| Agents | Phase 1 (algorithm) | + Phase 2 (infra) | + Phase 3 (native) |
|--------|---------------------|--------------------|--------------------|
| 10     | works today         | —                  | —                  |
| 25-30  | removes bottleneck  | —                  | —                  |
| 50     | marginal            | removes bottleneck | —                  |
| 100    | insufficient alone  | marginal           | removes bottleneck |
