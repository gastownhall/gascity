---
title: "Telemetry Roadmap"
---

Gas Town has a mature OTel integration providing dual-signal export
(metrics + structured logs) via OTLP HTTP to VictoriaMetrics/VictoriaLogs.
Gas City adds external observability — analogous to Prometheus + Loki.

The internal event bus (`internal/events/`) stays as-is: it serves
coordination (Kubernetes Events). OTel serves operator dashboards
(Prometheus + Loki). Same operations emit to both; different consumers.

## Gas Town → Gas City instrument mapping

```
GT Instrument                     GC Equivalent                       Phase
─────────────────────────────────────────────────────────────────────────────
COUNTERS (16 in GT)
gt.bd.calls.total                 gc.bd.calls.total                   1
gt.session.starts.total           gc.agent.starts.total               1
gt.session.stops.total            gc.agent.stops.total                1
gt.prompt.sends.total             gc.session.nudges.total             1
gt.pane.reads.total               (defer — low value in gc)           —
gt.prime.total                    gc.prime.total                      2
gt.agent.state_changes.total      (defer — gc uses beads not states)  —
gt.polecat.spawns.total           gc.pool.spawns.total                2
gt.polecat.removes.total          gc.pool.removes.total               2
gt.sling.dispatches.total         gc.sling.dispatches.total           1
gt.mail.operations.total          gc.mail.operations.total            2
gt.nudge.total                    (same as gc.session.nudges.total)   1
gt.done.total                     (defer — done not built yet)        —
gt.daemon.agent_restarts.total    gc.agent.crashes.total              1
gt.formula.instantiations.total   gc.formula.resolves.total           2
gt.convoy.creates.total           (defer — convoys not built yet)     —

HISTOGRAMS (1 in GT)
gt.bd.duration_ms                 gc.bd.duration_ms                   1

DAEMON GAUGES (7 in GT)
gt.daemon.heartbeat.total         gc.reconcile.cycles.total           1
gt.daemon.restart.total           (covered by gc.agent.crashes.total) 1
gt.dolt.connections               gc.dolt.healthy                     3
gt.dolt.max_connections           (defer — low value)                 —
gt.dolt.query_latency_ms          (defer — low value)                 —
gt.dolt.disk_usage_bytes          (defer — low value)                 —
gt.dolt.healthy                   gc.dolt.healthy                     3
```

## New Gas City-specific signals (no GT equivalent)

```
Instrument                        Type       Why                     Phase
─────────────────────────────────────────────────────────────────────────────
gc.agent.quarantines.total        Counter    Crash loop detection    1
gc.agent.idle_kills.total         Counter    Idle timeout restarts   1
gc.config.reloads.total           Counter    Live config reload      1
gc.controller.lifecycle.total     Counter    Controller start/stop   1
gc.worktree.creates.total         Counter    Git worktree ops        2
gc.pool.check.duration_ms         Histogram  Scale check latency     2
gc.hook.executions.total          Counter    Work query (gc hook)    2
gc.drain.transitions.total        Counter    Agent drain lifecycle   2
```

## Phase definitions

- **Phase 1** (done): Core package + 11 counters + 1 histogram.
  The minimum useful set for operator visibility.
- **Phase 2** (done): Pool spawns/removes, pool check latency, mail operations, drain transitions.
  4 new counters + 1 histogram.
- **Phase 3** (later): Dolt health gauges, observable gauges for running
  agent counts. Requires OTel callback registration pattern.

## Architecture

```
┌──────────────────────────────────────────────────────┐
│ gc binary                                            │
│                                                      │
│  cmd/gc/main.go    → telemetry.Init()                │
│  cmd/gc/reconcile  → RecordAgent{Start,Stop,Crash}   │
│  cmd/gc/controller → RecordControllerLifecycle       │
│  internal/beads    → RecordBDCall                    │
│                                                      │
│  internal/telemetry/                                 │
│    telemetry.go    — Init, Provider, Shutdown        │
│    recorder.go     — instruments + Record* functions │
│    subprocess.go   — env propagation to subprocesses │
└───────┬──────────────────────┬───────────────────────┘
        │ OTLP HTTP            │ OTLP HTTP
        ▼                      ▼
  VictoriaMetrics        VictoriaLogs
  :8428                  :9428
```

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `GC_OTEL_METRICS_URL` | (none — opt-in) | VictoriaMetrics OTLP push endpoint |
| `GC_OTEL_LOGS_URL` | (none — opt-in) | VictoriaLogs OTLP insert endpoint |
| `GC_LOG_BD_OUTPUT` | `false` | Include bd stdout/stderr in OTel logs |

When neither `GC_OTEL_METRICS_URL` nor `GC_OTEL_LOGS_URL` is set, all
telemetry is disabled and all `Record*` functions are no-ops.
