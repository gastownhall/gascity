---
title: "API Operations Design"
---

| Field | Value |
|---|---|
| Status | Implemented |
| Date | 2026-03-06 |
| Author(s) | Claude, Codex |
| Issue | — |
| Supersedes | Earlier drafts in this file and `gc-api-state-mutations-v0.md` |

---

## Table of Contents

1. [Summary](#1-summary)
2. [Motivation](#2-motivation)
3. [Industry Analysis](#3-industry-analysis)
4. [Design Principles](#4-design-principles)
5. [The Semantic Mismatch (Critical Bug)](#5-the-semantic-mismatch)
6. [Resource Model](#6-resource-model)
7. [URL Structure](#7-url-structure)
8. [Complete Endpoint Catalog](#8-complete-endpoint-catalog)
9. [StateMutator Interface Evolution](#9-statemutator-interface-evolution)
10. [Implementation Architecture](#10-implementation-architecture)
11. [Concurrency, Idempotency, and Operations](#11-concurrency-idempotency-and-operations)
12. [Security](#12-security)
13. [Error Handling](#13-error-handling)
14. [Legacy Endpoint Policy](#14-legacy-endpoint-policy)
15. [Delivery Phases](#15-delivery-phases)
16. [Testing Strategy](#16-testing-strategy)
17. [Open Questions](#17-open-questions)
18. [Alternatives Considered](#18-alternatives-considered)
19. [Appendix: Quick Reference](#appendix-quick-reference)

---

## 1. Summary

Gas City needs a coherent write API that:

- Separates **desired state** (what the controller should do) from
  **runtime actions** (what to do to a live session right now)
- Fixes the existing semantic mismatch where CLI and API use the same
  verbs for different state planes
- Covers every CLI mutation with an API equivalent (26 operations across
  8 categories have no API today)
- Handles pack-derived resources correctly (you can't PATCH a derived
  agent — you create a patch resource)
- Adds optimistic concurrency, idempotency, dry-run, and operation
  tracking where they reduce ambiguity
- Follows battle-tested patterns from Kubernetes, AWS, Nomad, and Fly.io
  without importing their ceremony

The API remains embedded in the controller, stays under `/v0/`, and ships
incrementally across 4 phases. Existing endpoints continue to work — the
migration is additive with explicit deprecation.

---

## 2. Motivation

### The Two-Writer Problem

Gas City currently has two write models that disagree on semantics:

**CLI writes desired state to `city.toml`:**
- `gc agent suspend worker` → sets `suspended=true` in city.toml
  (`cmd/gc/cmd_agent.go:488`)
- `gc rig add ./payments` → writes `[[rigs]]` entry + bootstraps filesystem
- `gc suspend` → sets `workspace.suspended=true`
  (`cmd/gc/cmd_suspend.go:104`)

**API writes runtime state to session metadata:**
- `POST /v0/agent/worker/suspend` → calls `sp.SetMeta(sessionName,
  "suspended", "true")` (`cmd/gc/api_state.go:220`)
- `POST /v0/rig/payments/suspend` → sets metadata on all rig sessions
  (`cmd/gc/api_state.go:269`)

**This is a bug.** The CLI suspend survives controller restarts. The API
suspend does not. A user who suspends an agent via the dashboard will find
it running again after a restart. The same verb produces different durability
guarantees depending on which surface invoked it.

### The Coverage Gap

Beyond the semantic mismatch, 26 CLI mutations have no API equivalent:

| Category | Missing Operations |
|---|---|
| City lifecycle | start, stop, restart, suspend, resume |
| Agent CRUD | add, destroy, start, stop, scale |
| Rig CRUD | add, remove, restart |
| Config | apply, validate, provider CRUD |
| Packs | fetch |
| Orders | run, enable/disable |
| Events | emit |
| Misc | handoff, reconcile |

Dashboards, CI/CD pipelines, Terraform providers, and Kubernetes operators
all need programmatic access to these operations.

### The Provenance Problem

When an agent comes from a pack, the CLI already knows you can't edit it
directly:

```
$ gc agent suspend pack-derived-agent
Error: agent "pack-derived-agent" is defined by pack "gastown";
       use [[patches.agent]] to override
```

The API has no equivalent awareness. A naive "just add POST endpoints"
approach would silently create inconsistencies between the pack definition
and the API-applied state.

---

## 3. Industry Analysis

### Patterns We Adopt

| Pattern | Source | GC Implementation |
|---|---|---|
| Desired state vs observed state | K8s spec/status | Config is spec; runtime view is status |
| Resource-oriented URLs | K8s, Nomad | `/v0/{resource}` flat namespace |
| Standard verbs | K8s, REST | GET, POST, PUT, PATCH, DELETE |
| Action subresources | K8s, Fly.io | `POST /v0/agent/{name}/kill` |
| Blocking queries | Nomad, Consul | `?index=N&wait=30s` (already implemented) |
| SSE streaming | Nomad | `/v0/events/stream` (already implemented) |
| Dry-run | K8s, AWS EC2 | `?dry_run=true` on desired-state mutations |
| Idempotency tokens | AWS | `Idempotency-Key` header on creates/deletes |
| Optimistic concurrency | K8s resourceVersion | `If-Match` / `ETag` on desired-state writes |
| Structured errors | K8s, AWS | `{code, message, details[]}` (already implemented) |
| Operation tracking | AWS CloudFormation | Async operations return trackable operation IDs |
| Finalizer-like deletion | K8s | Drain-before-destroy for agents |
| Generation tracking | K8s | `generation` bumps on spec change; `observed_generation` on reconcile |
| Provenance/origin | K8s field ownership | `origin: inline|patch|derived` on resources |

### Patterns We Reject

| Pattern | Source | Why Not |
|---|---|---|
| API groups / discovery docs | K8s | Too much ceremony for single-binary SDK |
| Admission webhooks | K8s | No extension model needed |
| CRDs / dynamic schema | K8s | Static types sufficient |
| Full MVCC | etcd | Event log provides similar semantics more simply |
| Request-only CRUD | AWS Cloud Control | Direct resource verbs are simpler |
| Lease-based mutation | Fly.io | Single controller, no contention |
| Separate API server binary | — | Embedded server has direct state access |
| gRPC transport | — | HTTP JSON sufficient; OpenAPI later |

### Key Insight: Nomad Is Our Closest Analog

Nomad is a single-binary orchestrator with an embedded HTTP API that manages
desired state (jobs) separately from runtime state (allocations). Gas City's
architecture — controller + agents, config + sessions — maps naturally to
this model. We adopt Nomad's flat URL structure, blocking queries, and
plan/dry-run semantics.

---

## 4. Design Principles

Seven principles govern the write API:

1. **Desired state and runtime actions are different operations.** Suspend
   is desired state (survives restarts). Kill is a runtime action (immediate,
   ephemeral). The API makes this explicit.

2. **The API is the supported writer when the controller is running.** CLI
   should delegate to the API. Direct file edits are treated as out-of-band
   changes the controller re-ingests via fsnotify.

3. **Derived resources are overridden by patches, not edited directly.** If
   an agent came from a pack, `PATCH /v0/agent/{name}` on `origin=derived`
   returns 409 and tells you to create a patch resource instead.

4. **All mutations are typed and auditable.** Every write emits an event.
   Structural changes (config mutations) also create operation records.

5. **Optimistic concurrency on desired-state writes.** Prevents lost updates
   from concurrent CLI + dashboard modifications.

6. **Idempotent creates and deletes.** Safe to retry after network failures.

7. **The controller is the reconciler; API handlers never shell out.** The
   API writes config and triggers reconciliation. It does not exec `gc`
   subcommands.

---

## 5. The Semantic Mismatch

This is the most important thing to fix. The table below shows every
existing mutation endpoint and its current vs correct behavior:

| Endpoint | Current Behavior | Correct Behavior | Fix |
|---|---|---|---|
| `POST /v0/agent/{name}/suspend` | Sets session metadata | Write `suspended=true` to city.toml | Redefine as desired-state write |
| `POST /v0/agent/{name}/resume` | Removes session metadata | Write `suspended=false` to city.toml | Redefine as desired-state write |
| `POST /v0/rig/{name}/suspend` | Sets session metadata on all agents | Write `suspended=true` on rig in city.toml | Redefine as desired-state write |
| `POST /v0/rig/{name}/resume` | Removes session metadata on all agents | Write `suspended=false` on rig in city.toml | Redefine as desired-state write |
| `POST /v0/agent/{name}/kill` | Calls `sp.Stop()` | Correct (runtime action) | Keep as-is |
| `POST /v0/agent/{name}/drain` | Sets drain metadata | Correct (runtime action) | Keep as-is |
| `POST /v0/agent/{name}/undrain` | Removes drain metadata | Correct (runtime action) | Keep as-is |
| `POST /v0/agent/{name}/nudge` | Sends to session | Correct (runtime action) | Keep as-is |

Because the API is still v0, **now is the right time to fix this.** The
suspend/resume endpoints change from runtime metadata writes to desired-state
config writes. This makes them durable and consistent with the CLI.

---

## 6. Resource Model

### 6.1 Resource Envelope

Desired-state resources use a lightweight envelope inspired by Kubernetes
but without the full ceremony:

```json
{
  "metadata": {
    "name": "payments/reviewer",
    "uid": "rig_01JNPZK6Q4...",
    "resource_version": "rv_184",
    "generation": 3,
    "observed_generation": 3,
    "origin": "inline",
    "created_at": "2026-03-06T10:00:00Z",
    "updated_at": "2026-03-06T12:00:00Z"
  },
  "spec": {
    "provider": "claude",
    "prompt_template": "reviewer.md",
    "suspended": false,
    "pool": { "min": 1, "max": 4 }
  },
  "status": {
    "ready": true,
    "running_count": 3,
    "conditions": [
      {"type": "Ready", "status": "True", "reason": "AllInstancesRunning"}
    ]
  }
}
```

**Key fields:**

| Field | Purpose |
|---|---|
| `resource_version` | Optimistic concurrency token. Changes on every mutation. Used with `If-Match`/`ETag`. |
| `generation` | Bumps only when `spec` changes. Unchanged by metadata-only updates. |
| `observed_generation` | Set by the reconciler when it processes a generation. `observed_generation < generation` means convergence pending. |
| `origin` | `inline` (in city.toml), `patch` (via `[[patches]]`), or `derived` (from pack expansion). Controls mutability. |
| `conditions` | Structured status signals. Types: `Ready`, `Healthy`, `Degraded`, `BootstrapComplete`. |

### 6.2 Provenance and Mutability Rules

| Origin | Mutable via resource endpoint? | How to modify |
|---|---|---|
| `inline` | Yes — PATCH/PUT/DELETE work | Direct config edit |
| `patch` | Yes — PATCH/PUT/DELETE on the patch | Modifies `[[patches]]` entry |
| `derived` | No — returns 409 | Create a patch resource via `POST /v0/patches/agents` |

This matches the CLI's existing behavior where `gc agent suspend` on a
pack-derived agent tells you to use `[[patches]]`.

### 6.3 Resource Kinds

**Desired-state resources** (persisted in city.toml):
- `City` — workspace-level settings
- `Agent` — agent definitions (includes agents with pool config)
- `Rig` — external project registrations
- `Provider` — provider presets
- `AgentPatch` — override for a derived agent
- `RigPatch` — override for a derived rig
- `ProviderPatch` — override for a derived provider

**Runtime views** (computed, not persisted):
- Agent list/detail with session state, active bead, etc. (existing `/v0/agents`)
- Rig list/detail with running counts (existing `/v0/rigs`)

**Operational resources**:
- `Operation` — tracks async mutation progress

### 6.4 Agent vs AgentPool: One Resource

An agent with a `pool` block in its spec is a pool. An agent without one is
a singleton. This matches the config model (`config.Agent` with optional
`*PoolConfig`). There is no separate `AgentPool` resource kind.

Rationale: Agents and pools share 95% of their fields. A separate kind would
force structural changes (delete singleton + create pool) for what is
logically a config change. One resource with an optional pool block is
simpler for both API consumers and the implementation.

---

## 7. URL Structure

### Flat Namespace with Semantic Clarity

URLs use a flat `/v0/` namespace. We do NOT split into `/v0/state/` and
`/v0/runtime/` prefixes, despite the conceptual distinction between desired
state and runtime actions. Reasons:

1. **Simplicity.** Users don't want to think about which URL prefix to use
   for suspend vs kill. The verb on the action subresource makes it clear.
2. **Backward compatibility.** Existing endpoints stay at their current
   paths. No mass migration.
3. **Nomad precedent.** Nomad uses flat `/v1/job/{id}` for both spec
   updates and evaluations without a state/runtime split.

Instead, the **HTTP method + path** communicates the intent:

| Pattern | Semantics |
|---|---|
| `GET /v0/{resource}` | Read current state |
| `POST /v0/{resources}` | Create new resource (desired state) |
| `PUT /v0/{resource}/{id}` | Replace resource spec (desired state) |
| `PATCH /v0/{resource}/{id}` | Partial update spec (desired state) |
| `DELETE /v0/{resource}/{id}` | Remove resource (desired state) |
| `POST /v0/{resource}/{id}/{action}` | Imperative runtime action |

Documentation and error messages always clarify which state plane an
operation affects. The `operation` response field shows whether the
mutation is synchronous (config commit) or async (reconciliation pending).

### URL Conventions

```
/v0/{plural}                        # collection (list, create)
/v0/{singular}/{id}                 # instance (get, update, delete)
/v0/{singular}/{id}/{action}        # imperative action
/v0/patches/{resource-plural}       # patch resources for derived objects
/v0/config                          # config inspection/apply
/v0/operations                      # operation tracking
```

Existing Nomad-style convention preserved: plural for collections
(`/v0/agents`), singular for instances (`/v0/agent/{name}`).

---

## 8. Complete Endpoint Catalog

### 8.1 Health & Status

```
GET  /health                                       (existing)
GET  /v0/status                                    (existing, enhanced)
```

Enhanced status response adds `controller_uptime`, `suspended`,
`config_generation`, and `observed_generation`.

### 8.2 City Lifecycle

```
GET    /v0/city                                    (new)
PATCH  /v0/city                                    (new)
POST   /v0/city/start                              (new)
POST   /v0/city/stop                               (new)
POST   /v0/city/restart                            (new)
POST   /v0/city/reconcile                          (new)
```

**`GET /v0/city`** — Returns city desired state as a resource with envelope.
Includes `spec.suspended`, `spec.provider`, `spec.session_template`, etc.

**`PATCH /v0/city`** — Partial update of city desired state. This is how
suspend/resume works at the city level:

```bash
# Suspend city (desired state — survives restarts)
curl -X PATCH http://127.0.0.1:8080/v0/city \
  -H 'X-GC-Request: true' \
  -H 'Content-Type: application/merge-patch+json' \
  -H 'If-Match: "rv_42"' \
  -d '{"spec": {"suspended": true}}'
```

**`POST /v0/city/start`** — Triggers reconciliation pass. Starts agents
per current config. Supports `{"dry_run": true}`.

**`POST /v0/city/stop`** — Graceful shutdown of all agents.
Accepts `{"timeout": "10s"}`.

**`POST /v0/city/restart`** — Stop then start. Atomic.

**`POST /v0/city/reconcile`** — Force immediate reconciliation without
restart. Like Nomad's `POST /v1/job/{id}/evaluate`.

### 8.3 Agents

```
GET    /v0/agents                                  (existing, enhanced)
GET    /v0/agent/{name}                            (existing, enhanced)
GET    /v0/agent/{name}/peek                       (existing)
POST   /v0/agents                                  (new)
PUT    /v0/agent/{name}                            (new)
PATCH  /v0/agent/{name}                            (new)
DELETE /v0/agent/{name}                            (new)
POST   /v0/agent/{name}/suspend                    (existing, REDEFINED)
POST   /v0/agent/{name}/resume                     (existing, REDEFINED)
POST   /v0/agent/{name}/kill                       (existing)
POST   /v0/agent/{name}/drain                      (existing)
POST   /v0/agent/{name}/undrain                    (existing)
POST   /v0/agent/{name}/nudge                      (existing)
POST   /v0/agent/{name}/start                      (new)
POST   /v0/agent/{name}/stop                       (new)
POST   /v0/agent/{name}/restart                    (new)
POST   /v0/agent/{name}/scale                      (new)
```

**`POST /v0/agents`** — Create Agent (desired state)

Adds agent to city.toml. Returns resource with envelope. If `pool` block
is present, creates a pool agent. Requires `Idempotency-Key`.

```json
{
  "spec": {
    "name": "reviewer",
    "rig": "payments",
    "provider": "claude",
    "prompt_template": "reviewer.md",
    "pool": {"min": 1, "max": 4, "check": "echo 2"},
    "env": {"REVIEW_MODE": "strict"},
    "work_query": "gc hook reviewer",
    "sling_query": "bd assign {{.BeadID}} reviewer"
  }
}
```

Response `201`:
```json
{
  "resource": {
    "metadata": {
      "name": "payments/reviewer",
      "uid": "ag_01JN...",
      "resource_version": "rv_185",
      "generation": 1,
      "observed_generation": 0,
      "origin": "inline"
    },
    "spec": { ... },
    "status": {
      "ready": false,
      "conditions": [
        {"type": "Ready", "status": "False", "reason": "ReconcilePending"}
      ]
    }
  },
  "operation": {
    "id": "op_01JN...",
    "action": "CreateAgent",
    "phase": "Succeeded"
  }
}
```

**`PUT /v0/agent/{name}`** — Replace Agent Spec (desired state)

Full spec replacement. Requires `If-Match`. Returns 409 if `origin=derived`.

**`PATCH /v0/agent/{name}`** — Partial Agent Update (desired state)

Merge-patch semantics matching `AgentPatch`. Requires `If-Match`. Returns
409 if `origin=derived` with instructions to use patch resource.

```json
// Suspend agent (desired state — durable)
{"spec": {"suspended": true}}

// Change pool scaling
{"spec": {"pool": {"max": 8}}}

// Update env (additive merge)
{"spec": {"env": {"NEW_KEY": "value"}, "env_remove": ["OLD_KEY"]}}
```

**`DELETE /v0/agent/{name}`** — Destroy Agent (desired state)

Removes from city.toml. Requires `Idempotency-Key`. Default behavior:
drain running sessions first, then remove config.

Query params:
- `?force=true` — skip drain, immediate kill + remove
- `?drain_timeout=30s` — override default

Returns 409 if agent has in-progress work and `force` not set.

**`POST /v0/agent/{name}/suspend`** — **(REDEFINED)**

Now writes `suspended=true` to city.toml (desired state), matching CLI
behavior. Previously set session metadata (runtime only). This is a
semantic fix, not a new endpoint.

**`POST /v0/agent/{name}/resume`** — **(REDEFINED)**

Now writes `suspended=false` to city.toml, matching CLI behavior.

**`POST /v0/agent/{name}/start`** — Start Session (runtime action)

Starts agent session(s). For pools, accepts `{"count": 2}`.

**`POST /v0/agent/{name}/stop`** — Stop Session (runtime action)

Stops running session(s). For pools, accepts `{"count": 1, "timeout": "10s"}`.

**`POST /v0/agent/{name}/restart`** — Restart Session (runtime action)

Stops then starts. The reconciler handles the restart naturally.

**`POST /v0/agent/{name}/scale`** — Scale Pool (runtime action)

Adjusts pool instance count. Only valid for pool agents.

```json
{"desired": 6}
```

**Enhanced `GET /v0/agent/{name}` response:**
```json
{
  "name": "payments/reviewer-1",
  "running": true,
  "suspended": false,
  "draining": false,
  "quarantined": false,
  "drift_detected": false,
  "origin": "inline",
  "provider": "claude",
  "pool": "payments/reviewer",
  "rig": "payments",
  "config_hash": "abc123",
  "restart_count": 2,
  "idle_timeout": "30m",
  "session": {
    "name": "city--payments--reviewer-1",
    "last_activity": "2026-03-06T10:30:00Z",
    "attached": false,
    "uptime": "2h15m"
  },
  "active_bead": "pay-42"
}
```

### 8.4 Rigs

```
GET    /v0/rigs                                    (existing, enhanced)
GET    /v0/rig/{name}                              (existing, enhanced)
POST   /v0/rigs                                    (new)
PATCH  /v0/rig/{name}                              (new)
DELETE /v0/rig/{name}                              (new)
POST   /v0/rig/{name}/suspend                      (existing, REDEFINED)
POST   /v0/rig/{name}/resume                       (existing, REDEFINED)
POST   /v0/rig/{name}/restart                      (new)
```

**`POST /v0/rigs`** — Create Rig (desired state)

Registers project directory, initializes bead store, writes city.toml.
Bootstrap work (bead init, hook install, route generation) may be async.

```json
{
  "spec": {
    "path": "/repos/payments",
    "name": "payments",
    "prefix": "pay",
    "includes": ["gastown"],
    "suspended": false,
    "bootstrap": {
      "init_beads": true,
      "install_hooks": true,
      "generate_routes": true
    }
  }
}
```

When bootstrap is needed, response is `202 Accepted` with operation:
```json
{
  "resource": { ... },
  "operation": {
    "id": "op_01JN...",
    "action": "CreateRig",
    "phase": "Running",
    "steps": [
      {"name": "config_written", "status": "complete"},
      {"name": "beads_initialized", "status": "running"},
      {"name": "hooks_installed", "status": "pending"}
    ]
  }
}
```

**`PATCH /v0/rig/{name}`** — Update Rig (desired state)

**`DELETE /v0/rig/{name}`** — Remove Rig (desired state)

Stops rig agents, removes config entry. Does NOT delete the project
directory or bead data. Accepts `?force=true` and `?keep_beads=true`.

**`POST /v0/rig/{name}/suspend`** and **`resume`** — **(REDEFINED)**

Now write to city.toml (desired state), matching CLI behavior.

**`POST /v0/rig/{name}/restart`** — Restart Rig (runtime action)

Kills all agents in the rig. Reconciler restarts them.

### 8.5 Providers

```
GET    /v0/providers                               (new)
GET    /v0/provider/{name}                         (new)
POST   /v0/providers                               (new)
PUT    /v0/provider/{name}                         (new)
PATCH  /v0/provider/{name}                         (new)
DELETE /v0/provider/{name}                         (new)
```

**`GET /v0/providers`** — Lists all providers (built-in + user-defined) with
`origin` and `in_use_by` fields.

**`POST /v0/providers`** — Create custom provider.

**`DELETE /v0/provider/{name}`** — Returns 409 if agents reference it.

**`PATCH /v0/provider/{name}`** on `origin=builtin` creates a
`[[patches.providers]]` entry (you can't edit built-in definitions, only
override them).

### 8.6 Patch Resources

```
GET    /v0/patches/agents                          (new)
POST   /v0/patches/agents                          (new)
GET    /v0/patches/agent/{name}                    (new)
PATCH  /v0/patches/agent/{name}                    (new)
DELETE /v0/patches/agent/{name}                    (new)

GET    /v0/patches/rigs                            (new)
POST   /v0/patches/rigs                            (new)
GET    /v0/patches/rig/{name}                      (new)
PATCH  /v0/patches/rig/{name}                      (new)
DELETE /v0/patches/rig/{name}                      (new)

GET    /v0/patches/providers                       (new)
POST   /v0/patches/providers                       (new)
GET    /v0/patches/provider/{name}                 (new)
PATCH  /v0/patches/provider/{name}                 (new)
DELETE /v0/patches/provider/{name}                 (new)
```

Patch resources project into `[[patches.agent]]`, `[[patches.rigs]]`, and
`[[patches.providers]]` sections of city.toml.

**`POST /v0/patches/agents`** — Create agent patch:

```json
{
  "spec": {
    "target": "payments/reviewer",
    "provider": "codex",
    "suspended": true,
    "pool": {"max": 8}
  }
}
```

This is the correct way to modify a pack-derived agent via the API. When
a user tries `PATCH /v0/agent/payments/reviewer` on an `origin=derived`
agent, the error response includes:

```json
{
  "code": "conflict",
  "message": "agent \"payments/reviewer\" is pack-derived (origin=derived); create a patch resource instead",
  "details": [
    {"field": "origin", "message": "use POST /v0/patches/agents to override derived resources"}
  ]
}
```

### 8.7 Config Operations

```
GET    /v0/config                                  (new)
POST   /v0/config/apply                            (new)
POST   /v0/config/validate                         (new)
GET    /v0/config/explain                          (new)
```

**`GET /v0/config`** — Returns fully-resolved config as JSON.

**`POST /v0/config/apply`** — Declarative bulk config mutation. Accepts a
partial config document and merges it into city.toml. Supports
`{"dry_run": true}` for preview.

```json
{
  "workspace": {"provider": "gemini"},
  "agents": [{"name": "reviewer", "provider": "claude"}],
  "patches": {
    "agents": [{"name": "polecat", "dir": "myapp", "pool": {"max": 8}}]
  },
  "dry_run": false
}
```

Response includes a diff of what changed and what the reconciler will do:
```json
{
  "status": "applied",
  "changes": [
    {"path": "workspace.provider", "old": "claude", "new": "gemini"},
    {"path": "agents[reviewer]", "action": "created"}
  ],
  "reconciliation": {
    "agents_to_restart": ["myapp/polecat-1"],
    "agents_to_start": ["reviewer"]
  }
}
```

**`POST /v0/config/validate`** — Validates without applying. Returns
validation errors and warnings.

**`GET /v0/config/explain`** — Returns config provenance (where each
value came from). Accepts `?agent=` and `?rig=` filters.

### 8.8 Orders

```
GET    /v0/orders                             (new)
GET    /v0/order/{name}                       (new)
POST   /v0/order/{name}/run                   (new)
POST   /v0/order/{name}/enable                (new)
POST   /v0/order/{name}/disable               (new)
GET    /v0/order/{name}/history               (new)
GET    /v0/orders/check                       (new)
```

**`POST /v0/order/{name}/run`** — Manual trigger, bypasses trigger checks.

**`POST /v0/order/{name}/enable`** / **`disable`** — Persists as
`OrderOverride` in city.toml.

### 8.9 Packs

```
GET    /v0/packs                                   (new)
POST   /v0/packs/fetch                             (new)
```

### 8.10 Operations

```
GET    /v0/operations                              (new)
GET    /v0/operation/{id}                          (new)
POST   /v0/operation/{id}/cancel                   (new)
POST   /v0/operation/{id}/retry                    (new)
```

Operations track the lifecycle of async mutations (rig bootstrap, agent
drain-then-destroy, pool scale-down).

```json
{
  "id": "op_01JN...",
  "action": "CreateRig",
  "target": {"kind": "Rig", "name": "payments"},
  "phase": "Running",
  "idempotency_key": "4db0a739-...",
  "created_at": "2026-03-06T10:00:00Z",
  "started_at": "2026-03-06T10:00:01Z",
  "steps": [
    {"name": "config_written", "status": "complete", "finished_at": "..."},
    {"name": "beads_initialized", "status": "running"},
    {"name": "hooks_installed", "status": "pending"}
  ],
  "last_error": null,
  "retryable": false
}
```

Phase state machine:
```
Accepted → Running → Succeeded
Accepted → Running → Failed
Accepted → Canceled
Running  → Canceled
```

Fast mutations (config-only writes) complete synchronously and return
`phase: "Succeeded"` inline. Slow mutations (bootstrap, drain-then-delete)
return `202 Accepted` with `phase: "Running"`.

### 8.11 Events

```
GET    /v0/events                                  (existing)
GET    /v0/events/stream                           (existing)
POST   /v0/events                                  (new)
```

**`POST /v0/events`** — Emit custom event:
```json
{
  "type": "deploy.completed",
  "actor": "ci-pipeline",
  "subject": "myapp",
  "message": "Deployed v2.3.1",
  "payload": {"version": "2.3.1"}
}
```

### 8.12 Beads, Mail, Convoys, Sling

Existing endpoints are kept with minimal additions:

```
PATCH  /v0/bead/{id}                               (new, preferred over POST .../update)
POST   /v0/bead/{id}/reopen                        (new)
POST   /v0/bead/{id}/assign                        (new, convenience)
DELETE /v0/bead/{id}                               (new)
DELETE /v0/mail/{id}                               (new)
POST   /v0/convoy/{id}/remove                      (new)
GET    /v0/convoy/{id}/check                       (new)
DELETE /v0/convoy/{id}                             (new)
```

Existing bead/mail/convoy/sling endpoints gain audit event emission and
optional `Idempotency-Key` support but no behavioral changes.

### 8.13 Endpoint Summary

| Category | Existing | Redefined | New | Total |
|---|---|---|---|---|
| Health/Status | 2 | 0 | 1 | 3 |
| City | 0 | 0 | 6 | 6 |
| Agents | 8 | 2 | 8 | 18 |
| Rigs | 4 | 2 | 3 | 9 |
| Providers | 0 | 0 | 6 | 6 |
| Patches | 0 | 0 | 15 | 15 |
| Config | 0 | 0 | 4 | 4 |
| Orders | 0 | 0 | 7 | 7 |
| Packs | 0 | 0 | 2 | 2 |
| Operations | 0 | 0 | 4 | 4 |
| Events | 2 | 0 | 1 | 3 |
| Beads | 7 | 0 | 4 | 11 |
| Mail | 9 | 0 | 1 | 10 |
| Convoys | 4 | 0 | 3 | 7 |
| Sling | 1 | 0 | 0 | 1 |
| **Total** | **37** | **4** | **65** | **106** |

---

## 9. StateMutator Interface Evolution

### 9.1 Current Interface

```go
type StateMutator interface {
    State
    SuspendAgent(name string) error
    ResumeAgent(name string) error
    KillAgent(name string) error
    DrainAgent(name string) error
    UndrainAgent(name string) error
    NudgeAgent(name, message string) error
    SuspendRig(name string) error
    ResumeRig(name string) error
}
```

### 9.2 Proposed Decomposition

```go
// DesiredStateMutator handles config-backed mutations.
// All methods write to city.toml and trigger reconciliation.
type DesiredStateMutator interface {
    // City
    PatchCity(rv string, patch CityPatch) (*MutationResult, error)

    // Agents
    CreateAgent(spec config.Agent, idemKey string) (*MutationResult, error)
    PatchAgent(name, rv string, patch AgentMergePatch) (*MutationResult, error)
    ReplaceAgent(name, rv string, spec config.Agent) (*MutationResult, error)
    DeleteAgent(name, rv string, opts DeleteOpts) (*MutationResult, error)

    // Rigs
    CreateRig(spec config.Rig, idemKey string) (*MutationResult, error)
    PatchRig(name, rv string, patch RigMergePatch) (*MutationResult, error)
    DeleteRig(name, rv string, opts DeleteOpts) (*MutationResult, error)

    // Providers
    CreateProvider(name string, spec config.ProviderSpec, idemKey string) (*MutationResult, error)
    PatchProvider(name, rv string, patch ProviderMergePatch) (*MutationResult, error)
    DeleteProvider(name, rv string) (*MutationResult, error)

    // Patch resources (for derived objects)
    CreateAgentPatch(spec config.AgentPatch, idemKey string) (*MutationResult, error)
    CreateRigPatch(spec config.RigPatch, idemKey string) (*MutationResult, error)
    CreateProviderPatch(spec config.ProviderPatch, idemKey string) (*MutationResult, error)

    // Bulk config apply
    ApplyConfig(partial config.City, dryRun bool) (*ApplyResult, error)
    ValidateConfig(partial config.City) (*ValidationResult, error)

    // Orders
    EnableAutomation(name, rig string) error
    DisableAutomation(name, rig string) error
}

// RuntimeMutator handles live session operations.
// These never write to city.toml.
type RuntimeMutator interface {
    KillAgent(name string) error
    DrainAgent(name string) error
    UndrainAgent(name string) error
    NudgeAgent(name, message string) error
    StartAgent(name string, count int) ([]string, error)
    StopAgent(name string, count int) ([]string, error)
    RestartAgent(name string) error
    ScaleAgent(name string, desired int) error
    RestartRig(name string) ([]string, error)
    RunAutomation(name, rig string) (*RunResult, error)
    Reconcile() (*ReconcileResult, error)
}

// MutationResult returned by desired-state mutations.
type MutationResult struct {
    Resource       any             // The created/updated resource with envelope
    Operation      *Operation      // Operation record (nil for sync-only)
    ResourceVersion string         // New resource version after mutation
}
```

### 9.3 Capability Discovery

The API server discovers capabilities via type assertion, enabling
incremental implementation:

```go
func (s *Server) handleAgentCreate(w http.ResponseWriter, r *http.Request) {
    ds, ok := s.state.(DesiredStateMutator)
    if !ok {
        writeError(w, 501, "not_implemented",
            "agent creation not supported by this controller")
        return
    }
    // ...
}
```

This follows the existing pattern in `handleAgentAction` which already
type-asserts to `StateMutator`. The server gracefully degrades when running
against a controller that hasn't implemented all interfaces yet.

---

## 10. Implementation Architecture

### 10.1 Config Mutation Flow

```
API Request
    │
    ├─ Validate request body (schema + business rules)
    │
    ├─ Check provenance: origin=derived? → 409
    │
    ├─ Check optimistic concurrency: If-Match vs current resourceVersion
    │      → 412 Precondition Failed on mismatch
    │
    ├─ Check idempotency: Idempotency-Key seen before?
    │      → Return cached result (same hash) or 422 (different hash)
    │
    ├─ Acquire configMu (serialization lock)
    │
    ├─ Read current city.toml from disk
    │
    ├─ Apply mutation to in-memory config
    │
    ├─ Validate resulting config (full LoadWithIncludes pass)
    │
    ├─ Atomic write: temp file → os.Rename → city.toml
    │
    ├─ Release configMu
    │
    ├─ Emit audit event
    │
    └─ Controller detects fsnotify change → hot-reload → reconcile
```

**Key invariant:** The API never modifies the in-memory config directly. It
writes to city.toml and lets the existing hot-reload mechanism propagate the
change. This ensures:

1. **Durability** — changes survive controller restart
2. **Consistency** — same validation pipeline regardless of source
3. **Observability** — `git diff city.toml` shows all API-applied changes
4. **Safety** — out-of-band edits are detected and re-ingested

### 10.2 Concurrency Model

```go
type controllerState struct {
    mu       sync.RWMutex   // existing: protects in-memory reads
    configMu sync.Mutex     // new: serializes config file writes
    idemCache *idempotencyCache // new: in-memory, TTL-based
    // ...
}
```

- **Reads** take `mu.RLock()` (concurrent, non-blocking)
- **Config writes** take `configMu.Lock()` (serialized, prevents lost updates)
- **Runtime actions** (kill, drain, nudge) take neither lock — they go
  directly to the session provider

### 10.3 No Metadata Store — Derive Everything

Gas City's design principle: **no status files — query live state.** State
files go stale on crash and create false positives. Every piece of metadata
the API needs is derivable from existing sources of truth:

| Need | Derivation |
|---|---|
| Optimistic concurrency (ETag) | SHA256 hash of the resource's serialized TOML section |
| Provenance/origin | Raw config vs expanded config comparison (CLI already does this) |
| Convergence tracking | Event log records `controller.config_reloaded` events |
| Idempotency cache | In-memory map with TTL (single-process, single-user) |
| Operation tracking | Event log with correlation IDs (Phase 3) |

**ETag computation** is a pure function — same config = same ETag, no stored
counter needed:

```go
func agentETag(cfg *config.City, name string) string {
    for _, a := range cfg.Agents {
        if a.QualifiedName() == name {
            h := sha256.New()
            toml.NewEncoder(h).Encode(a)
            return fmt.Sprintf(`"gc-%x"`, h.Sum(nil)[:8])
        }
    }
    return ""
}
```

**Provenance detection** reuses the CLI's proven two-phase pattern:
1. Load raw config (no pack expansion) → look for agent
2. Found? → `origin=inline`
3. Not found? Load expanded config → found there? → `origin=derived`

No new files. No state to go stale. Everything derived from city.toml, the
expanded config, and the event log.

### 10.4 Suspend/Resume Fix

The suspend/resume semantic fix is implemented by changing the
`controllerState` methods:

**Before (runtime only):**
```go
func (cs *controllerState) SuspendAgent(name string) error {
    sp, sessionName := cs.spAndSession(name)
    return sp.SetMeta(sessionName, "suspended", "true") // lost on restart!
}
```

**After (desired state):**
```go
func (cs *controllerState) SuspendAgent(name string) error {
    return cs.editor.EditExpanded(func(raw, expanded *config.City) error {
        origin := configedit.AgentOrigin(raw, expanded, name)
        if origin == configedit.OriginDerived {
            // Auto-create patch for suspend (too common to require explicit patch)
            return configedit.AddOrUpdateAgentPatch(raw, name, func(p *config.AgentPatch) {
                p.Suspended = boolPtr(true)
            })
        }
        return configedit.SetAgentSuspended(raw, name, true)
    })
}
```

The `configedit.Editor` handles the serialization lock, raw config load,
validation, and atomic write. The caller just provides the mutation function.

### 10.5 CSRF and Read-Only Middleware Extension

Extend to all mutation methods (currently POST-only):

```go
func withCSRFCheck(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        switch r.Method {
        case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
            if r.Header.Get("X-GC-Request") == "" {
                writeError(w, 403, "csrf", "X-GC-Request header required")
                return
            }
        }
        next.ServeHTTP(w, r)
    })
}
```

Same extension for `withReadOnly`.

---

## 11. Concurrency, Idempotency, and Operations

### 11.1 Optimistic Concurrency

**Required** on desired-state writes (PATCH, PUT, DELETE on resources).
**Not required** on runtime actions (kill, drain, nudge — these are
inherently imperative).

Read responses include:
- `ETag: "rv_184"` header
- `metadata.resource_version: "rv_184"` in body

Write requests must include:
- `If-Match: "rv_184"` header

Stale version → `412 Precondition Failed`.

### 11.2 Idempotency

**Required** on non-idempotent creates and deletes:
- `POST /v0/agents` — `Idempotency-Key` required
- `POST /v0/rigs` — `Idempotency-Key` required
- `DELETE /v0/agent/{name}` — `Idempotency-Key` required

**Optional but supported** on PATCH, runtime actions.

Rules:
- Same key + same request body hash → return original result
- Same key + different body hash → `422 Unprocessable Entity`
- Expired/evicted key → treated as new request

TTL: 10 minutes (covers retry window for network failures).

### 11.3 Dry-Run

Supported on desired-state mutation endpoints via `?dry_run=true`.

Behavior:
- Full validation runs
- Provenance checks run
- Optimistic concurrency checks run
- Response shows the would-be resource and reconciliation preview
- **No** city.toml write
- **No** operation record
- **No** audit event

Not supported on runtime actions (kill, drain, nudge are inherently
side-effectful).

---

## 12. Security

### Threat Model

Same as today: single-user, local-machine operation.

| Threat | Mitigation |
|---|---|
| Cross-origin browser attacks | CORS (localhost-only) + CSRF header |
| Non-localhost exposure | Automatic read-only mode |
| Stale concurrent writes | Optimistic concurrency (If-Match) |
| Config injection | Full validation on all config mutations |
| Path traversal | Rig paths validated |
| Oversized requests | 1 MiB body limit |
| Duplicate side effects | Idempotency keys |

### Destructive Operation Safety

| Operation | Protection |
|---|---|
| DELETE agent | Drain-first; `?force=true` to skip |
| DELETE rig | 409 if agents running; `?force=true` to skip |
| City stop | No extra protection (matches Ctrl-C) |
| Config apply | Dry-run available; validation always runs |
| DELETE bead | 409 if open children exist |

### Future: Token Auth

When implemented, tokens will have scoped capabilities:

| Scope | Access |
|---|---|
| `gc.read` | All GET endpoints |
| `gc.write` | Desired-state mutations |
| `gc.runtime` | Runtime actions (kill, drain, nudge) |
| `gc.admin` | City lifecycle, config apply |
| `gc.operations` | Read/cancel operations |

The interface decomposition (Section 9.2) is designed to support
per-capability authorization.

---

## 13. Error Handling

### Error Codes

| HTTP | Code | When |
|---|---|---|
| 400 | `invalid` | Malformed body, invalid field values |
| 404 | `not_found` | Resource doesn't exist |
| 409 | `conflict` | Duplicate create, derived resource direct edit, busy delete |
| 412 | `precondition_failed` | Stale `If-Match` value |
| 422 | `idempotency_mismatch` | Same key, different request body |
| 403 | `read_only` | Non-localhost mutation |
| 403 | `csrf` | Missing `X-GC-Request` header |
| 501 | `not_implemented` | Capability not available on this controller |
| 500 | `internal` | Unexpected server error |

### Recovery Model

- Desired-state commits are atomic (succeed or fail completely)
- Follow-on reconciliation may fail independently
- Failure is represented in operation status and resource conditions
- Retryable failures can be requeued via `POST /v0/operation/{id}/retry`
- Spec persisted but not yet healthy is the correct failure mode (K8s model)

---

## 14. Legacy Endpoint Policy

| Existing Endpoint | Policy |
|---|---|
| `POST /v0/agent/{name}/suspend` | Redefined: now writes city.toml |
| `POST /v0/agent/{name}/resume` | Redefined: now writes city.toml |
| `POST /v0/rig/{name}/suspend` | Redefined: now writes city.toml |
| `POST /v0/rig/{name}/resume` | Redefined: now writes city.toml |
| `POST /v0/agent/{name}/kill` | Kept, same path |
| `POST /v0/agent/{name}/drain` | Kept, same path |
| `POST /v0/agent/{name}/undrain` | Kept, same path |
| `POST /v0/agent/{name}/nudge` | Kept, same path |
| `POST /v0/bead/{id}/update` | Kept, deprecated in favor of `PATCH /v0/bead/{id}` |
| All bead/mail/convoy/sling | Kept, gain audit events + optional idempotency |

Because the API is v0, fixing the suspend/resume semantic mismatch is
acceptable as a behavioral change rather than a breaking change.

---

## 15. Delivery Phases

### Phase 1: Fix Semantics + Agent/Rig CRUD ✓

**The critical fix.** Suspend/resume becomes desired-state. Add structural
CRUD for agents and rigs.

**Endpoints delivered:**
```
PATCH  /v0/city                        # city suspend/resume (desired state)
POST   /v0/agents                      # create agent
PATCH  /v0/agent/{name}                # partial update agent
DELETE /v0/agent/{name}                # destroy agent
POST   /v0/rigs                        # create rig
PATCH  /v0/rig/{name}                  # update rig
DELETE /v0/rig/{name}                  # remove rig
+ Suspend/resume rewritten as desired-state (city.toml, not session metadata)
+ CSRF/read-only middleware extended to PATCH/DELETE
+ configedit.Editor serializes config mutations with mutex
+ Provenance detection for pack-derived agents (409 on direct mutation)
+ *bool for Suspended in PATCH structs to avoid zero-value trap
```

**Implementation files:**
- `internal/fsys/atomic.go` — atomic file write helper (temp + rename)
- `internal/fsys/fsys.go` — added `Remove` to FS interface
- `internal/configedit/configedit.go` — serialized config editor with provenance detection
- `internal/configedit/configedit_test.go` — 33 tests
- `internal/api/state.go` — `AgentUpdate`/`RigUpdate` types, extended `StateMutator`
- `internal/api/handler_agent_crud.go` — agent create/update/delete handlers
- `internal/api/handler_rig_crud.go` — rig create/update/delete handlers
- `internal/api/handler_city.go` — city suspend/resume handler
- `internal/api/middleware.go` — `isMutationMethod()` for CSRF/read-only
- `cmd/gc/api_state.go` — suspend/resume rewritten to use `configedit.Editor`

**Deferred from original design (moved to Phase 2+):**
- PUT (full replace) — PATCH-only is simpler and avoids the PUT=PATCH trap
- ETags / optimistic concurrency
- start/stop/restart/scale actions (remain as existing POST actions)
- Idempotency keys, dry-run mode

### Phase 2: Providers + Config + Patch Resources ✅

**Status:** Delivered. 20 endpoints across 3 commits.

**Endpoints delivered:**
```
Provider CRUD (5):
  GET /v0/providers — list all (builtins + city overrides)
  GET /v0/provider/{name} — single provider
  POST /v0/providers — create city-level provider
  PATCH /v0/provider/{name} — update city-level provider
  DELETE /v0/provider/{name} — delete city-level provider

Config (3):
  GET /v0/config — expanded config snapshot
  GET /v0/config/explain — provenance annotations
  GET /v0/config/validate — dry-run validation

Patch resources (12):
  GET/PUT/DELETE agent patches (/v0/patches/agents, /v0/patches/agent/{name})
  GET/PUT/DELETE rig patches (/v0/patches/rigs, /v0/patches/rig/{name})
  GET/PUT/DELETE provider patches (/v0/patches/providers, /v0/patches/provider/{name})
```

**Implementation:**
- `configedit.Editor` methods: CreateProvider, UpdateProvider, DeleteProvider,
  SetAgentPatch, DeleteAgentPatch, SetRigPatch, DeleteRigPatch,
  SetProviderPatch, DeleteProviderPatch
- `api.ProviderUpdate` type with `*string`/`*int` fields
- `api.StateMutator` extended with provider + patch CRUD
- `cmd/gc/api_state.go` bridge to configedit
- Handler tests for all 20 endpoints
- ConfigEdit unit tests for all 9 new Editor methods

**Files added/changed:**
- `internal/api/handler_providers.go` — provider list/get
- `internal/api/handler_provider_crud.go` — provider create/update/delete
- `internal/api/handler_provider_crud_test.go` — provider tests
- `internal/api/handler_config.go` — config GET/explain/validate
- `internal/api/handler_config_test.go` — config tests
- `internal/api/handler_patches.go` — patch resource handlers
- `internal/api/handler_patches_test.go` — patch tests
- `internal/api/state.go` — ProviderUpdate type, extended StateMutator
- `internal/api/fake_state_test.go` — extended fake
- `internal/configedit/configedit.go` — 9 new Editor methods
- `internal/configedit/configedit_test.go` — 15 new tests
- `cmd/gc/api_state.go` — bridge methods

**Deferred from original design (moved to Phase 3+):**
- Config apply (POST /v0/config) — complex diff/merge engine
- PUT (full replace) for providers
- Optimistic concurrency (ETags)

### Phase 3: City Lifecycle + Orders + Operations ✅

**Status:** Delivered. Orders, events, enhanced status, rig restart all implemented.

**Endpoints implemented:**
- `GET /v0/city` — city info
- Order CRUD: list/show/enable/disable
- `POST /v0/events` — event emission
- Enhanced status with uptime, version, agent counts
- `POST /v0/rig/{name}/restart` — kills all agents in rig (reconciler restarts)
- `POST /v0/agent/{name}/restart` — kills agent session (reconciler restarts)

**Deferred:** City start/stop/reconcile (controller lifecycle), operation tracking.

### Phase 4: Polish + Bead/Mail Extensions + Packs ✅

**Status:** Delivered. All bead/mail extensions, cursor pagination, and idempotency implemented.

**Endpoints implemented:**
```
Packs list/fetch (2)
PATCH /v0/bead/{id} (1)
POST /v0/bead/{id}/reopen (1)
POST /v0/bead/{id}/assign (1)
DELETE /v0/bead/{id}, /v0/mail/{id}, /v0/convoy/{id} (3)
POST /v0/convoy/{id}/remove, GET .../check (2)
POST /v0/events (1)
```

**Cross-cutting features:**
- Cursor pagination on list endpoints (beads, mail, convoys, events)
  via `?cursor=<opaque>&limit=N` with `next_cursor` in response
- `Idempotency-Key` header on `POST /v0/beads` and `POST /v0/mail`
  (in-memory cache with 30-minute TTL; 422 on key reuse with different body)
- `X-GC-Request-Id` on all responses (via middleware)

### Phase 5: CLI as API Client ✅

**Status:** Delivered. No new endpoints — CLI routes writes through API
when controller is running.

**Implementation:**
- `internal/api/client.go` — HTTP client wrapping mutation endpoints
  (SuspendCity, ResumeCity, SuspendAgent, ResumeAgent, SuspendRig, ResumeRig)
- `cmd/gc/apiroute.go` — `apiClient(cityPath)` detects running controller
  with API, returns client or nil for fallback to direct mutation
- CLI commands wired: `gc suspend`, `gc resume`, `gc agent suspend/resume`,
  `gc rig suspend/resume`

**Pattern:**
```go
if c := apiClient(cityPath); c != nil {
    return c.SuspendAgent(name)
}
// No controller — direct file mutation (existing behavior)
return doAgentSuspend(fs, cityPath, name, stdout, stderr)
```

**Tests:**
- `internal/api/client_test.go` — 8 tests covering all client methods,
  error responses, and CSRF header propagation

---

## 16. Testing Strategy

### Unit Tests

Every handler gets a `*_test.go` using `httptest.NewServer` with mock
`State`/`DesiredStateMutator`/`RuntimeMutator`. Coverage:

- Happy path (create, read, update, delete)
- Validation errors (missing fields, invalid values)
- Provenance rejection (409 on derived resource PATCH)
- Optimistic concurrency (412 on stale If-Match)
- Idempotency (replay returns cached result; mismatch returns 422)
- Dry-run (validation without write)
- CSRF rejection
- Read-only mode rejection

### Integration Tests

Build-tagged `//go:build integration` tests:

- Start real controller with API enabled
- Create agent via API → verify city.toml updated → agent starts
- Suspend via API → verify city.toml has `suspended=true` → survives restart
- Concurrent PATCH with stale version → verify 412
- Rig create with bootstrap → verify operation progresses to Succeeded

### Backward Compatibility Tests

- All existing request/response shapes unchanged
- `POST /v0/bead/{id}/update` still works
- `POST /v0/agent/{name}/suspend` still works (now with correct semantics)

---

## 17. Open Questions

### Before Accepting

1. ~~**Metadata store format**~~ — **Resolved: no metadata store.** All
   metadata is derived from city.toml, the expanded config, and the event
   log. No state files.

2. **Optimistic concurrency on legacy suspend/resume:** Should the redefined
   `POST /v0/agent/{name}/suspend` require `If-Match`? The old endpoint
   didn't. Adding it is technically a breaking change. **Recommendation:**
   Optional in Phase 1. Clients that don't send `If-Match` get
   last-writer-wins (same as today). Clients that do send it get safety.

3. **Agent vs AgentPool as separate resources:** The Codex doc suggests a
   separate `AgentPool` kind. This doc proposes one `Agent` kind with
   optional pool block. **Recommendation:** One resource. The config model
   already works this way. Singleton→pool conversion is a spec change, not
   a type change.

4. **Patch resource naming:** Should patch resources for agent
   `payments/reviewer` be named `payments-reviewer-override` (opaque) or
   just target `payments/reviewer` (one patch per target)?
   **Recommendation:** One patch per target, named by target. Multiple
   patches for the same target would be confusing.

5. **Config apply scope:** Should `POST /v0/config/apply` accept JSON only,
   or also TOML? **Recommendation:** JSON only for v0.

### During Implementation

1. Default retention period for operations (recommend: 7 days)
2. Whether `POST /v0/bead/{id}/update` should emit a `Deprecation` header
3. Which phase adds `gc order run` API parity (recommend: Phase 3)

---

## 18. Alternatives Considered

### A. Keep Current Split Model

Leave CLI as desired-state writer, API as runtime writer. Add missing
endpoints ad hoc.

**Rejected:** Suspend/resume semantics stay broken. Every new endpoint
rediscovers the same rules. Pack-derived writes stay ambiguous.

### B. `/v0/state/*` and `/v0/runtime/*` URL Prefix Split

Separate URL namespaces for desired-state and runtime operations (from
`gc-api-state-mutations-v0.md`).

**Rejected for v0:** Adds cognitive overhead (users must pick the right
prefix). Backward-incompatible with existing endpoints. The HTTP method
+ action subresource already communicates intent. The conceptual distinction
is preserved in documentation and error messages, not URL structure.

**May revisit for v1** if the flat namespace proves confusing in practice.

### C. Full Kubernetes-Shaped API

Full `metadata`, API groups, discovery documents, admission webhooks, scale
subresources.

**Rejected:** Too much ceremony for a single-binary SDK serving one city.
We adopt K8s *patterns* (spec/status, generation, conditions, optimistic
concurrency) without K8s *structure*.

### D. Thin CLI Wrapper

Shell out to `gc rig add`, `gc agent add`, etc. from API handlers.

**Rejected:** Couples API to CLI output format. Prevents typed idempotency,
concurrency control, and structured operations. Repeats the dashboard
subprocess problem.

### E. Separate API Server Binary

Extract API into its own process immediately.

**Rejected for v0:** Expands project boundary before the mutation model is
stable. The immediate problem is semantic inconsistency, not process
topology. The embedded server has direct access to state and reconciliation.

---

## Appendix: Quick Reference

```
Health & Status
  GET  /health
  GET  /v0/status

City
  GET    /v0/city
  PATCH  /v0/city
  POST   /v0/city/start
  POST   /v0/city/stop
  POST   /v0/city/restart
  POST   /v0/city/reconcile

Agents
  GET    /v0/agents
  GET    /v0/agent/{name}
  GET    /v0/agent/{name}/peek
  POST   /v0/agents
  PUT    /v0/agent/{name}
  PATCH  /v0/agent/{name}
  DELETE /v0/agent/{name}
  POST   /v0/agent/{name}/suspend       (redefined: desired state)
  POST   /v0/agent/{name}/resume        (redefined: desired state)
  POST   /v0/agent/{name}/kill
  POST   /v0/agent/{name}/drain
  POST   /v0/agent/{name}/undrain
  POST   /v0/agent/{name}/nudge
  POST   /v0/agent/{name}/start
  POST   /v0/agent/{name}/stop
  POST   /v0/agent/{name}/restart
  POST   /v0/agent/{name}/scale

Rigs
  GET    /v0/rigs
  GET    /v0/rig/{name}
  POST   /v0/rigs
  PATCH  /v0/rig/{name}
  DELETE /v0/rig/{name}
  POST   /v0/rig/{name}/suspend         (redefined: desired state)
  POST   /v0/rig/{name}/resume          (redefined: desired state)
  POST   /v0/rig/{name}/restart

Providers
  GET    /v0/providers
  GET    /v0/provider/{name}
  POST   /v0/providers
  PUT    /v0/provider/{name}
  PATCH  /v0/provider/{name}
  DELETE /v0/provider/{name}

Patch Resources
  GET    /v0/patches/agents
  POST   /v0/patches/agents
  GET    /v0/patches/agent/{name}
  PATCH  /v0/patches/agent/{name}
  DELETE /v0/patches/agent/{name}
  GET    /v0/patches/rigs
  POST   /v0/patches/rigs
  GET    /v0/patches/rig/{name}
  PATCH  /v0/patches/rig/{name}
  DELETE /v0/patches/rig/{name}
  GET    /v0/patches/providers
  POST   /v0/patches/providers
  GET    /v0/patches/provider/{name}
  PATCH  /v0/patches/provider/{name}
  DELETE /v0/patches/provider/{name}

Config
  GET    /v0/config
  POST   /v0/config/apply
  POST   /v0/config/validate
  GET    /v0/config/explain

Orders
  GET    /v0/orders
  GET    /v0/order/{name}
  POST   /v0/order/{name}/run
  POST   /v0/order/{name}/enable
  POST   /v0/order/{name}/disable
  GET    /v0/order/{name}/history
  GET    /v0/orders/check

Packs
  GET    /v0/packs
  POST   /v0/packs/fetch

Operations
  GET    /v0/operations
  GET    /v0/operation/{id}
  POST   /v0/operation/{id}/cancel
  POST   /v0/operation/{id}/retry

Events
  GET    /v0/events
  GET    /v0/events/stream
  POST   /v0/events

Beads
  GET    /v0/beads
  GET    /v0/beads/ready
  GET    /v0/bead/{id}
  GET    /v0/bead/{id}/deps
  POST   /v0/beads
  PATCH  /v0/bead/{id}
  POST   /v0/bead/{id}/close
  POST   /v0/bead/{id}/update           (deprecated)
  POST   /v0/bead/{id}/reopen
  POST   /v0/bead/{id}/assign
  DELETE /v0/bead/{id}

Mail
  GET    /v0/mail
  GET    /v0/mail/count
  GET    /v0/mail/thread/{id}
  GET    /v0/mail/{id}
  POST   /v0/mail
  POST   /v0/mail/{id}/read
  POST   /v0/mail/{id}/mark-unread
  POST   /v0/mail/{id}/archive
  POST   /v0/mail/{id}/reply
  DELETE /v0/mail/{id}

Convoys
  GET    /v0/convoys
  GET    /v0/convoy/{id}
  POST   /v0/convoys
  POST   /v0/convoy/{id}/add
  POST   /v0/convoy/{id}/close
  POST   /v0/convoy/{id}/remove
  GET    /v0/convoy/{id}/check
  DELETE /v0/convoy/{id}

Sling
  POST   /v0/sling
```
