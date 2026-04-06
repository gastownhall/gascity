---
title: "Machine-Wide Supervisor"
---

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-03-06 |
| Author(s) | Claude |
| Issue | N/A |
| Supersedes | N/A |

## Summary

The Gas City controller evolves from a per-city process into a
machine-wide supervisor that manages multiple cities from a single
daemon. Today each city runs its own controller (lock, socket,
reconciliation loop, API server). This proposal introduces a two-level
Erlang/OTP-style supervision tree: a **machine supervisor** (top-level)
manages N **city supervisors** (children), each retaining its current
isolation properties (own tmux socket, own bead stores, own event log).
The API gains a `/v0/cities` resource and a city namespace prefix. The
design explicitly prepares for future multi-tenant hosting where
different customers share a machine, by introducing tenant-level
isolation boundaries above the city level.

Impact: the `api.State` interface stays per-city (handlers barely
change). The new layer is above it -- a registry that routes requests to
the correct city's State. Existing single-city deployments work
unchanged via a compatibility shim.

## Motivation

### Pain today

1. **Operational overhead of N controllers.** A developer running 3
   cities (one per project) has 3 controller processes, 3 API ports, 3
   sets of tmux sessions to monitor. There is no single pane of glass.
   `gc status` only shows the current city. Checking all cities requires
   `cd`-ing into each directory.

2. **No cross-city visibility.** The dashboard connects to one API URL.
   Monitoring multiple cities requires multiple browser tabs pointed at
   different ports, or re-launching with different `--api` flags.

3. **Resource waste.** Each controller runs its own fsnotify watcher,
   reconciliation goroutine, and HTTP listener. On machines with 5+
   cities this is 5+ processes doing the same structural work
   independently.

4. **Future multi-tenancy.** Hosting Gas City as a service (multiple
   customers on the same box) requires tenant isolation, resource limits,
   and a single management plane. The current architecture has no concept
   of "who owns this city."

### Erlang/OTP parallel

Gas City already explicitly follows the Erlang/OTP supervision model
(documented in `engdocs/architecture/health-patrol.md`):

| Erlang/OTP | Gas City today | Gas City proposed |
|---|---|---|
| Application | N/A | Tenant |
| Top-level supervisor | N/A | Machine supervisor |
| Supervisor | Controller per city | City supervisor (child of machine) |
| Worker | Agent | Agent (unchanged) |
| Child spec | `[[agent]]` in city.toml | `[[agent]]` (unchanged) |
| Application controller | N/A | `~/.gc/supervisor.sock` |

Today each city is an independent Erlang "node." This proposal connects
them under one application controller -- the same pattern as Erlang's
`application` module managing multiple `supervisor` trees.

### Design principles alignment

- **NDI (Nondeterministic Idempotence):** A machine-wide supervisor
  converges all cities to their desired state on every tick, regardless
  of which cities were added, removed, or crashed since the last tick.
  The registry file is the desired state; running city supervisors are
  the actual state.

- **SDK self-sufficiency:** The machine supervisor is pure
  infrastructure. It does not require any user-configured agent role to
  function.

- **Bitter Lesson:** A unified API surface gets MORE useful as models
  improve -- agents can query cross-city state, external tools can
  monitor all cities, dashboards can show a fleet view.

## Guide-Level Explanation

### Registering cities

```bash
# From inside a city directory
cd ~/bright-lights
gc register          # adds this city to machine supervisor

cd ~/dark-alleys
gc register          # adds another city

gc cities            # list registered cities
# NAME            PATH                     STATUS
# bright-lights   /home/user/bright-lights running
# dark-alleys     /home/user/dark-alleys   running
```

### Starting the machine supervisor

```bash
# Start the machine supervisor (manages all registered cities)
gc supervisor start

# Register the current city and ensure it is running
gc register
```

The supervisor is a single long-running daemon. It replaces the exposed
per-city controller mode. `gc start` from inside a city directory
auto-registers the city, ensures the machine supervisor is running, and
triggers an immediate reconcile.

### Stopping

```bash
gc supervisor stop    # stops ALL cities, then the supervisor
gc unregister         # removes current city from supervisor
gc stop               # unregisters and stops the current city
```

### API access

The machine supervisor runs a single API server (one port):

```bash
# List all cities
curl http://localhost:8080/v0/cities

# City-scoped requests (new URL pattern)
curl http://localhost:8080/v0/city/bright-lights/agents
curl http://localhost:8080/v0/city/bright-lights/agent/worker/output
curl http://localhost:8080/v0/city/dark-alleys/beads?status=open

# Cross-city queries
curl http://localhost:8080/v0/agents              # all agents, all cities
curl http://localhost:8080/v0/events/stream        # global event stream
```

### Backward compatibility

When only one city is registered, the existing `/v0/agents` (no city
prefix) routes to that city. This means existing dashboards and scripts
work unchanged. The city prefix becomes required only when 2+ cities are
registered.

### Dashboard

The dashboard connects to the single supervisor API. In v0 it queries
the first registered city automatically -- no city selector yet:

```bash
gc dashboard --api http://localhost:8080
# Queries GET /v0/cities, picks the first one,
# prefixes all API calls with /v0/city/{name}/...
# City switcher is follow-on work.
```

### Config

The supervisor reads a global config at `~/.gc/supervisor.toml`:

```toml
[supervisor]
# API port for the machine-wide supervisor.
# City-level [api] sections are ignored when running under the supervisor.
port = 8080
bind = "127.0.0.1"

# Patrol interval for the supervisor's own reconciliation
# (checking city health, not agent health -- that's per-city).
patrol_interval = "10s"

# Future: tenant isolation
# [tenants.acme]
# cities = ["bright-lights", "dark-alleys"]
# resource_limit = { max_agents = 50, max_cities = 5 }
```

Cities are registered in `~/.gc/cities.toml`:

```toml
[[cities]]
path = "/home/user/bright-lights"

[[cities]]
path = "/home/user/dark-alleys"
```

## Reference-Level Explanation

### 1) Two-Level Supervision Tree

```
machine supervisor process (PID 1234)
~/.gc/supervisor.lock (flock)
~/.gc/supervisor.sock (unix socket)
|
+-- HTTP listener :8080
|   +-- /v0/cities
|   +-- /v0/city/{name}/*  --> dispatches to cityState
|   +-- /v0/* (compat)     --> dispatches to sole city (if only one)
|
+-- supervisor reconciliation loop (10s tick)
|   +-- for each registered city:
|       +-- ensure cityState exists
|       +-- city reconciliation loop (per-city patrol_interval tick)
|           +-- agent start/stop/drift detection (unchanged)
|           +-- crash loop quarantine (unchanged)
|           +-- idle timeout (unchanged)
|           +-- order dispatch (unchanged)
|
+-- per-city isolation:
    +-- bright-lights/
    |   +-- cityState { cfg, sp, stores, events }
    |   +-- tmux -L bright-lights (own tmux server)
    |   +-- .gc/events.jsonl (own event log)
    |
    +-- dark-alleys/
        +-- cityState { cfg, sp, stores, events }
        +-- tmux -L dark-alleys (own tmux server)
        +-- .gc/events.jsonl (own event log)
```

### 2) New Types

```go
// Package supervisor manages multiple cities from a single process.
package supervisor

// Registry tracks registered cities. Backed by ~/.gc/cities.toml.
type Registry struct {
    mu     sync.RWMutex
    cities map[string]CityEntry // keyed by city name
    path   string               // path to cities.toml
}

// CityEntry is one registered city.
type CityEntry struct {
    Name string // derived from workspace.name or dir basename
    Path string // absolute path to city root
}

// Supervisor is the machine-wide controller.
type Supervisor struct {
    registry *Registry
    cities   map[string]*CityRuntime // keyed by city name
    mu       sync.RWMutex
    config   SupervisorConfig
}

// CityRuntime holds the running state for one city.
// This is essentially today's controllerState + reconciliation loop,
// extracted from cmd/gc/controller.go into a reusable unit.
type CityRuntime struct {
    state    *controllerState  // existing type, unchanged
    cancel   context.CancelFunc
    loop     *controllerLoop   // existing reconciliation loop
    watcher  *fsnotify.Watcher // per-city config watcher
}
```

### 3) API Routing

The API server gains a city resolver middleware:

```go
// resolveCity extracts the city name from the URL and returns the
// corresponding State. For /v0/city/{name}/..., it uses the path
// segment. For /v0/... (no city prefix), it uses the sole registered
// city (if exactly one) or returns 400 "city required".
func (s *SupervisorServer) resolveCity(r *http.Request) (api.State, string, error)
```

The existing `api.State` interface is **unchanged**. Handlers receive
a per-city State exactly as they do today. The new layer sits above
the handler dispatch, not inside it.

```
Request: GET /v0/city/bright-lights/agents
                      |
                      v
              resolveCity("bright-lights")
                      |
                      v
              cityRuntime.state (implements api.State)
                      |
                      v
              handleAgentList(w, r)  <-- UNCHANGED handler
```

New endpoints that don't exist today:

```
GET  /v0/cities                    # list registered cities
GET  /v0/city/{name}               # city detail (status, agent count, etc.)
POST /v0/city/{name}/register      # register a city (alternative to CLI)
POST /v0/city/{name}/unregister    # unregister a city
GET  /v0/events/stream             # global SSE stream (all cities, tagged)
```

### 4) Supervisor Reconciliation

The supervisor has its own reconciliation loop (separate from per-city
agent reconciliation):

```
On each supervisor tick:
  1. Read ~/.gc/cities.toml (desired state)
  2. Compare against running CityRuntime map (actual state)
  3. For new cities:  load config, create CityRuntime, start city loop
  4. For removed cities: cancel city loop, graceful agent shutdown
  5. For existing cities: no action (city loop handles its own health)
```

City addition/removal is hot -- no supervisor restart needed. The
supervisor watches `~/.gc/cities.toml` with fsnotify, same pattern as
per-city config watching.

### 5) Per-City Isolation (Unchanged)

Each city retains full isolation:

| Resource | Isolation mechanism | Changed? |
|---|---|---|
| Tmux sessions | Per-city socket (`tmux -L <cityName>`) | No |
| Bead stores | Per-rig within city (`.beads/` or bd) | No |
| Event log | Per-city `.gc/events.jsonl` | No |
| Config | Per-city `city.toml` | No |
| Session names | Per-city tmux server = no collision | No |

The only thing that moves from per-city to machine-wide:
- **Lock file:** `~/.gc/supervisor.lock` (replaces per-city `.gc/controller.lock`)
- **Control socket:** `~/.gc/supervisor.sock` (replaces per-city `.gc/controller.sock`)
- **API port:** Single port from `supervisor.toml` (replaces per-city `[api] port`)

### 6) Global Event Bus

A new `events.Multiplexer` aggregates per-city event providers:

```go
// Multiplexer merges events from multiple city providers into one
// stream, tagging each event with its source city.
type Multiplexer struct {
    providers map[string]events.Provider // city name -> provider
}

func (m *Multiplexer) Watch(ctx context.Context, afterSeq uint64) (Watcher, error)
```

Per-city event files remain untouched. The multiplexer reads from them.
Global sequence numbers use a `{city}:{seq}` composite cursor to avoid
cross-city ordering ambiguity.

### 7) Concurrency Model

```
supervisor goroutines:
  1. main: supervisor reconciliation loop
  2. per-city: N city reconciliation loops (one goroutine each)
  3. HTTP server: shared across all cities (goroutine per request)
  4. fsnotify: 1 for cities.toml + N for per-city config dirs

Locking:
  - Supervisor.mu: protects cities map (add/remove city)
  - Each CityRuntime.state.mu: protects per-city state (existing RWMutex)
  - No cross-city locks needed (cities are independent)
```

### 8) Backward Compatibility

| Scenario | Behavior |
|---|---|
| `gc start` in a city dir (no supervisor running) | Auto-registers city, starts supervisor |
| `gc start` in a city dir (supervisor running) | Auto-registers city, supervisor picks it up |
| `gc start --standalone` | Legacy mode: per-city controller, no supervisor |
| `gc stop` in a city dir | Unregisters city from supervisor |
| `gc supervisor stop` | Stops all cities, then supervisor |
| Existing `[api] port` in city.toml | Ignored when running under supervisor (warning logged) |
| Single registered city, no city prefix in URL | Routes to sole city (backward compat) |

### 9) Future: Multi-Tenant Isolation

The two-level tree naturally extends to three levels for multi-tenancy:

```
machine supervisor
+-- tenant "acme"
|   +-- city "acme-prod"
|   +-- city "acme-staging"
+-- tenant "bigcorp"
    +-- city "bigcorp-prod"
```

Tenant boundaries provide:
- **Resource limits:** max agents, max cities, CPU/memory cgroups
- **API authentication:** tenant API keys, JWT with tenant claim
- **Network isolation:** per-tenant bind addresses or Unix sockets
- **Data isolation:** per-tenant `~/.gc/tenants/{name}/cities.toml`

The city-level isolation (tmux socket, bead stores, event log) already
provides process and data separation. Tenant-level adds authorization
and resource governance on top.

URL pattern extends naturally:

```
/v0/tenant/{tenant}/city/{city}/agents    # tenant-scoped
/v0/city/{city}/agents                    # single-tenant (default)
```

This is not implemented in v0 but the design explicitly avoids
decisions that would block it:
- City names are unique within a registry (extend to unique within tenant)
- No global mutable state shared between cities
- API routing is a prefix match that can gain another segment

## Primitive Test

Not applicable -- this proposal does not add a primitive or derived
mechanism. It restructures the deployment topology of the existing
controller (infrastructure concern). All five primitives and four
derived mechanisms remain unchanged. The supervisor is a process
management layer, not a Gas City concept.

## Drawbacks

1. **Complexity cost.** One process managing N cities is harder to
   reason about than N independent processes. A bug in the supervisor
   takes down all cities, not just one. Erlang solves this with
   per-supervisor crash isolation; we'd need similar care (a panicking
   city loop must not crash the supervisor).

2. **Blast radius.** Today a misbehaving city.toml only affects one
   controller. With a shared supervisor, a city that causes excessive
   CPU or memory pressure affects all cities on the machine. Resource
   limits (cgroups, goroutine budgets) add complexity.

3. **Migration burden.** Users running per-city controllers must
   migrate. The `--standalone` escape hatch helps, but two modes means
   two code paths to maintain.

4. **Per-city `[api] port` becomes obsolete.** Users who built tooling
   around city-specific API ports must migrate to the unified port with
   city prefixes. The single-city backward-compat shim buys time but
   doesn't eliminate the migration.

5. **Lock file location.** `~/.gc/` is user-scoped. Running the
   supervisor as a system service (multiple users) requires a different
   location (`/var/run/gc/`). Two code paths for lock location.

## Alternatives

### A. Do Nothing (Status Quo)

Each city runs its own controller. Users manage multiple cities by
running multiple `gc start` commands and tracking multiple API ports.

**Advantages:** Simple. No new concepts. Each city is fully
independent -- a crash affects only that city.

**Why rejected:** The pain points (no cross-city visibility, N
controller processes, no path to multi-tenancy) are real and grow
with the number of cities. The "do nothing" option works for single-
city users but blocks the multi-city and hosted use cases.

### B. Proxy-Only Aggregator (No Shared Process)

Keep per-city controllers. Add a lightweight proxy that aggregates
their APIs into one endpoint. The proxy reads `cities.toml`, discovers
API ports, and reverse-proxies requests to the right city.

```
proxy :8080
  /v0/city/bright-lights/* --> http://localhost:8081/*
  /v0/city/dark-alleys/*   --> http://localhost:8082/*
```

**Advantages:** No change to the controller. Each city remains
independent. Proxy is stateless and easy to restart.

**Why rejected:** Doubles the port allocation problem (N city ports +
1 proxy port). The proxy has no access to in-memory state, so it
can't provide a unified event stream without N SSE connections. It
also can't provide cross-city queries (e.g., "all agents across all
cities") without N fan-out requests. The proxy adds latency and
failure modes without reducing the process count.

### C. Kubernetes-Style: Cities as CRDs

Model cities as custom resources in a central store (etcd, sqlite).
A single controller watches the store and reconciles cities.

**Advantages:** Well-understood pattern. Declarative. Easy to add
RBAC and multi-tenancy.

**Why rejected:** Massive over-engineering for a local developer
tool. Introduces a dependency on a central store. Violates the
city-as-directory principle. Gas City's strength is filesystem
simplicity; replacing it with a database contradicts the design
philosophy.

### D. systemd/launchd Integration

Instead of a custom supervisor, register each city as a systemd user
service. Use systemd's existing process management, logging, and
restart capabilities.

**Advantages:** Zero custom supervision code. Systemd handles
process lifecycle, logging, and cgroup isolation. Cross-platform via
launchd on macOS.

**Why rejected:** Doesn't solve the API aggregation problem. Each
city still needs its own port. No unified event stream. No cross-city
queries. Also platform-specific and harder to test.

## Resolved Questions

Resolved during design review (2026-03-07):

1. **Lock file location.** Use `$XDG_RUNTIME_DIR/gc/` with fallback to
   `~/.gc/`. System service deployment is a future concern -- a
   `GC_HOME` env var override covers that case when it arises.

2. **City name uniqueness.** Path is the primary key in the registry
   (unique by definition). City name is the display label used in API
   URLs. Registration rejects duplicate names -- the user must set a
   unique `workspace.name` in city.toml. Explicit, no magic.

3. **Per-city API port migration.** Warn and ignore. Log
   `"city 'X' has [api] port=N which is ignored under supervisor mode"`
   at startup. Not an error -- just unused config the user can clean up
   at their pace.

4. **`gc start` behavior change.** Option B: `gc start` auto-registers
   the city and starts the supervisor if not running. The supervisor
   should just exist, not be thought about. `gc start --standalone` is
   the escape hatch for legacy per-city mode.

5. **Goroutine isolation.** Wrap each city loop goroutine in
   `defer recover()`. On panic: log error, emit `city.crashed` event,
   mark city unhealthy, retry after backoff. Same pattern as Erlang
   supervisor restart.

6. **Config reload atomicity.** Use the same 200ms debounce strategy as
   per-city config watching. Already a solved problem.

7. **Dashboard protocol.** Minimum viable: dashboard queries the first
   registered city and updates all API URLs to include the city prefix.
   No city selector in v0 -- just make it work with the new URL scheme.
   City switcher is follow-on work.

8. **Global event sequence numbering.** Wall-clock ordering is
   sufficient for the global stream. The `{city}:{seq}` composite cursor
   is for resumption, not total ordering. Cities are independent --
   total cross-city ordering is a non-goal.

## Implementation Plan

### Phase 0: Extract CityRuntime (small)

Refactor `cmd/gc/controller.go` to separate city-specific state and
reconciliation into a `CityRuntime` struct that can be instantiated
multiple times. No behavior change -- the existing per-city controller
constructs one `CityRuntime` and runs it. This is the prerequisite
for Phase 1.

**Delivers:** Clean separation of city lifecycle from process lifecycle.
Existing tests pass unchanged.

### Phase 1: Registry and Supervisor Daemon (medium)

- Add `~/.gc/` global directory, `cities.toml` registry file.
- `gc register` / `gc unregister` CLI commands.
- `gc supervisor start` / `gc supervisor stop`.
- `gc cities` list command.
- Supervisor process: reads registry, starts one `CityRuntime` per city.
- Single API port from `supervisor.toml`.
- Backward compat: `gc start` still works (registers + starts supervisor).
- `gc start --standalone` for legacy per-city mode.

**Delivers:** Machine-wide supervision. Single process for all cities.

### Phase 2: API City Namespace (medium)

- Add `/v0/cities` endpoint.
- Add `/v0/city/{name}/...` URL prefix routing.
- Single-city backward compat: `/v0/agents` routes to sole city.
- Dashboard minimum viable: query first registered city, update all API
  URLs to include city prefix. No city selector yet.

**Delivers:** Unified API for multi-city access. Dashboard works
unchanged for single-city users.

### Phase 3: Global Event Stream (small)

- `events.Multiplexer` wrapping per-city providers.
- `GET /v0/events/stream` global SSE endpoint.
- Events tagged with city name.

**Delivers:** Cross-city observability from a single stream.

### Phase 4: Tenant Isolation (future, large)

- Tenant registry and configuration.
- Per-tenant API authentication.
- Resource limits.
- URL prefix: `/v0/tenant/{tenant}/city/{city}/...`

**Delivers:** Multi-customer hosting on shared infrastructure.
