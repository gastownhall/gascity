# GC API Server v0

| Field | Value |
|---|---|
| Status | Draft |
| Date | 2026-03-04 |
| Author(s) | Codex, Claude |
| Issue | N/A |
| Supersedes | N/A |

## Summary

The Gas City controller gains an opt-in HTTP API server that exposes
agents, beads, mail, convoys, events, and rigs as typed resources
over REST. It replaces the
dashboard's current pattern of spawning ~20 subprocesses per page load with
in-memory queries served from the controller's own state. Streaming uses two
battle-tested mechanisms: Nomad/Consul-style index-based blocking queries on
any GET endpoint, plus Server-Sent Events for browser consumers. The API
lives in `internal/api/`, activates via `[api] port = 8080` in city.toml
(progressive activation), and runs inside the controller process. URL
structure follows Nomad's flat `/v0/` convention — no Kubernetes API groups,
no city-in-URL (one controller = one city). v0 signals instability; the
prefix becomes `/v1/` when the contract stabilizes.

## Motivation

The dashboard server (`examples/gastown/packs/dashboard/server/`) is a
command broker that shells out for every request:

**Performance:** Each page load spawns ~20 subprocesses (`gc status --json`,
`bd list --status=open --json`, `tmux list-sessions`, etc.). The SSE poller
spawns 3 subprocesses every 2 seconds. With the API server, all
dashboard data sources become in-memory reads — zero subprocess cost.

**Fragility:** The dashboard parses mixed text/JSON CLI output. Any output
format change silently breaks the UI. Typed JSON endpoints eliminate this.

**Security:** The `/api/run` endpoint executes whitelisted commands but still
passes string arguments to shells. Typed endpoints with validated parameters
remove the command-injection surface entirely.

**Architecture:** Go source code doesn't belong in packs. Packs are
config/scripts/templates. The dashboard needs a proper API to consume, not
a Go server that shells out to the same binary that hosts it.

**Bitter Lesson:** A stable HTTP API gets MORE useful as models improve —
agents can consume the API directly, tools can build on it, external
monitoring can integrate. CLI text parsing gets LESS useful.

## Guide-Level Explanation

Enable the API server by adding one line to `city.toml`:

```toml
[api]
port = 8080
```

The controller starts an HTTP listener alongside its existing unix socket.
All resources are available under `/v0/`:

```bash
# List all agents with running/suspended state
curl http://localhost:8080/v0/agents

# Get a specific bead
curl http://localhost:8080/v0/bead/gc-123

# List open beads across all rigs
curl http://localhost:8080/v0/beads?status=open

# Watch for changes (blocks until state changes or timeout)
curl http://localhost:8080/v0/agents?index=42&wait=30s

# Stream events via SSE
curl http://localhost:8080/v0/events/stream

# Send mail
curl -X POST http://localhost:8080/v0/mail \
  -d '{"to":"worker-1","subject":"Review needed","body":"Please check gc-456"}'

# Suspend an agent (reconciler won't restart it)
curl -X POST http://localhost:8080/v0/agent/worker-1/suspend

# Resume an agent (reconciler restarts it)
curl -X POST http://localhost:8080/v0/agent/worker-1/resume

# Route work to an agent
curl -X POST http://localhost:8080/v0/sling \
  -d '{"target":"worker-1","bead":"gc-456"}'
```

The URL pattern follows Nomad: `/v0/agents` (plural) lists the collection,
`/v0/agent/{name}` (singular) gets one instance. No city segment — the
controller serves exactly one city.

No generic "run arbitrary command" endpoint exists. Every operation has a
typed endpoint. Agent lifecycle uses Gas City's suspension model (not
start/stop) — the reconciler manages sessions automatically.

## Reference-Level Explanation

### 1) Deployment: Embedded in Controller

The API server runs inside the controller process. No new binary.

```
controller process
├── unix socket (.gc/controller.sock)   ← existing CLI commands
├── reconciliation loop                  ← existing agent lifecycle
├── event bus                            ← existing event recording
└── HTTP listener (:8080)               ← NEW: API server
    ├── /v0/* resource handlers
    └── /health
```

The controller already owns all the state:
- **Agents**: session provider (`ListRunning`, `IsRunning`, metadata)
- **Beads**: bead stores per rig (via `beads.Store` interface)
- **Events**: event bus (`events.Provider` with `Watch()`)
- **Config**: loaded `config.City` struct
- **Health**: controller liveness, patrol findings

The API handlers query these existing interfaces directly. No subprocess
spawning. No CLI parsing. The same code paths that power `gc status` and
`gc session list` power the API — but without fork/exec overhead.

**Lifecycle:** API server starts after the controller acquires its lock and
loads config. It shares the controller's `context.Context` — when the
controller shuts down, the HTTP server drains and stops.

### 2) URL Structure

Flat paths under `/v0/`, following Nomad/Consul convention.

Why `/v0/` and not `/v1/`:
- Signals instability. Breaking changes expected.
- Reserves `/v1/` for when the contract stabilizes.
- Nomad/Consul started at `/v1/` and can never change it. We learn from that.

Why no `/cities/{city}/` in the path:
- One controller = one city. The city is implicit.
- Nomad doesn't put `/regions/{region}/` in every URL — it uses a query
  parameter when needed.
- Saves 2 URL segments on every request.

Why singular for instances (Nomad pattern):
- `/v0/agents` = list all agents (plural)
- `/v0/agent/worker-1` = get one agent (singular)
- Reads naturally: "get agent worker-1"

### 3) Resources and Endpoints

#### Status & Health

```
GET  /v0/status                      # city overview (name, path, agents, rigs, summary)
GET  /health                         # health probe (no /v0/ prefix — load balancer convention)
```

Health endpoint returns HTTP status codes reflecting state (Vault pattern):
- `200` — running, healthy
- `429` — running, suspended
- `503` — starting up or shutting down

#### Agents

```
GET  /v0/agents                      # list all agents (expanded, including pool members)
GET  /v0/agent/{name}                # get agent details
GET  /v0/agent/{name}/output         # unified conversation output (session log or terminal)
GET  /v0/agent/{name}/output/stream  # SSE stream of new output turns
POST /v0/agent/{name}/suspend        # suspend agent (reconciler won't restart)
POST /v0/agent/{name}/resume         # resume agent (reconciler restarts)
POST /v0/agent/{name}/kill           # force-kill session (reconciler restarts)
POST /v0/agent/{name}/drain          # signal graceful wind-down
POST /v0/agent/{name}/undrain        # cancel drain signal
POST /v0/agent/{name}/nudge          # send message to running session
```

**Agent naming:** The `{name}` parameter is the agent's qualified name
— a slash-delimited path that encodes scope:

```
/v0/agent/mayor                     # city-scoped (Dir="")
/v0/agent/gastown/witness           # rig-scoped (Dir="gastown")
/v0/agent/gastown/polecat-1         # pool member (rig-scoped, hyphen-numbered)
```

Pool members use hyphen-numbered names (`polecat-1`, `polecat-2`), not
slash-separated paths. A rig-scoped pool member's qualified name is
`gastown/polecat-1` — two path segments, same as any rig-scoped agent.
When `pool.max == 1`, the member uses the bare name (no suffix).

Agent base names cannot contain slashes (`validAgentName` enforces
`[a-zA-Z0-9][a-zA-Z0-9_-]*`), so path segments are unambiguous. The
server calls `config.ParseQualifiedName()` to resolve. No separate
scope parameter or encoding needed — the name IS the scope.

**Agents and pools are the same thing.** `GET /v0/agents` returns the
**expanded** list — every running pool member appears as a full agent
entry. Pool definitions are not returned as abstract entries; only
actual runtime agents appear. Each pool member carries a `pool` field
identifying which pool it belongs to. Fixed agents have `pool: null`.

Query parameters for `GET /v0/agents`:

| Param | Description | Example |
|-------|-------------|---------|
| `pool` | Filter to members of a specific pool | `?pool=gastown/polecat` |
| `rig` | Filter to agents in a specific rig | `?rig=gastown` |
| `running` | Filter by running state | `?running=true` |

Gas City uses **suspension**, not start/stop. Agents are session-based:
the reconciler starts sessions for non-suspended agents automatically.
`suspend` prevents restart; `resume` allows it. `kill` forces a session
restart. `drain` signals the agent to wind down gracefully.

`/v0/agent/{name}/output` returns unified conversation output for an
agent. It tries structured session logs (Claude JSONL) first, falling
back to raw terminal capture via `Peek()`. The response format
distinguishes the source via a `format` field (`"conversation"` or
`"text"`). `/v0/agent/{name}/output/stream` provides the same data as
an SSE stream for live UI updates. Session state is an implementation
detail of agents, not a separate resource.

**Agent response includes session state:**

```json
{
  "name": "gastown/polecat-1",
  "running": true,
  "suspended": false,
  "session": {
    "name": "gastown--polecat-1",
    "last_activity": "2026-03-04T14:30:00Z",
    "attached": false,
    "command": "claude"
  },
  "active_bead": "gc-42",
  "rig": "gastown",
  "pool": "gastown/polecat"
}
```

The `session` block is populated from the session provider (tmux
internally). Tmux session names replace `/` with `--` (e.g.,
`gastown--polecat-1`). If the agent has no running session, `session`
is null. The `command` field is the foreground process in pane 0 (from
`tmux display-message -t {session}:0.0 -p "#{pane_current_command}"`).
The dashboard uses `session.last_activity` for stale/stuck detection,
`session.command` for agent-running heuristics (e.g., is "claude"
running?), and `GET /v0/agent/{name}/output` for agent output — no
direct tmux access needed.

Data source: `session.Provider.ListRunning()` + `config.City.Agents`
(with pool expansion via `poolAgents()`). The controller's reconciliation
loop already expands pool definitions into individual agents — the API
returns the same expanded view.

#### Rigs

```
GET  /v0/rigs                        # list rigs (name, path, suspended, beads prefix)
GET  /v0/rig/{name}                  # get rig details
POST /v0/rig/{name}/suspend          # suspend rig (all its agents suspended)
POST /v0/rig/{name}/resume           # resume rig
POST /v0/rig/{name}/restart          # kill all rig agent sessions (reconciler restarts)
```

Same suspension model as agents. Rig suspension cascades to all agents
in the rig.

Data source: `config.City.Rigs` + filesystem state.

#### Beads

```
GET  /v0/beads                       # query beads (cross-rig)
GET  /v0/beads/ready                 # ready work items (open, unassigned)
GET  /v0/bead/{id}                   # get bead by ID
GET  /v0/bead/{id}/deps              # dependency graph (blocks, blocked-by, tracks)
POST /v0/beads                       # create bead
POST /v0/bead/{id}/close             # close bead
POST /v0/bead/{id}/update            # update bead (status, priority, assignee, labels)
```

Higher-level operations (mail, convoys, sling) also create and mutate
beads through their own endpoints. The bead CRUD endpoints are for
direct issue/task management.

`GET /v0/beads/ready` returns beads that are available for work — the
same query as `bd ready`. These are open beads not currently assigned
or in progress, grouped by source rig.

Data source: `beads.Store` per rig. The controller iterates configured rigs
and queries each store. Cross-rig routing uses the existing `routes.jsonl`
prefix→path mapping.

Query parameters for `GET /v0/beads`:

| Param | Description | Example |
|-------|-------------|---------|
| `status` | Filter by status | `?status=open`, `?status=closed` |
| `type` | Filter by bead type | `?type=convoy` |
| `label` | Filter by label (repeatable) | `?label=gc:message` |
| `assignee` | Filter by assignee | `?assignee=worker-1` |
| `rig` | Filter to specific rig | `?rig=tower-of-hanoi` |
| `limit` | Max results (default 50) | `?limit=100` |
| `continue` | Pagination cursor | `?continue=<token>` |

#### Mail

```
GET  /v0/mail                        # list messages (default: unread)
GET  /v0/mail/{id}                   # get message without marking read
POST /v0/mail                        # send message (to, subject, body)
POST /v0/mail/{id}/read              # mark as read (adds "read" label, bead stays open)
POST /v0/mail/{id}/mark-unread       # mark as unread (removes "read" label)
POST /v0/mail/{id}/archive           # archive (closes bead permanently)
POST /v0/mail/{id}/reply             # reply (inherits thread, sends to original sender)
GET  /v0/mail/thread/{thread-id}     # list all messages in a thread
GET  /v0/mail/count?agent={name}     # total/unread count
```

Mail is beads with `type=message` and the `gc:message` label. Reading
a message adds the "read" label but keeps the bead open — messages
remain accessible via get, thread queries, and count. Archive closes
the bead permanently. Messages support subject/body separation
(`Title` = subject, `Description` = body), threading via `thread:<id>`
labels, and reply chains via `reply-to:<id>` labels.

Query parameters for `GET /v0/mail`:

| Param | Description | Example |
|-------|-------------|---------|
| `agent` | Filter by recipient | `?agent=worker-1` |
| `status` | `unread` (default), `read`, `all` | `?status=all` |

Data source: `mail.Provider` (wraps `beads.Store` with mail semantics).

#### Convoys

```
GET  /v0/convoys                     # list convoys
GET  /v0/convoy/{id}                 # get convoy with child issues + progress
POST /v0/convoys                     # create convoy (name + optional issue IDs)
POST /v0/convoy/{id}/add             # add issues to convoy
POST /v0/convoy/{id}/close           # close convoy
```

Convoys are parent beads with child issue beads linked via dependency
graph. Progress is derived from child bead statuses.

Data source: `beads.Store` query with `type=convoy`. Child issues via
bead dependency graph.

#### Events

```
GET  /v0/events                      # list recent events
GET  /v0/events/stream               # SSE event stream
```

Events are the append-only log of all system activity. Types include:
`agent.started`, `agent.stopped`, `agent.crashed`, `bead.created`,
`bead.closed`, `mail.sent`, `mail.read`, `convoy.created`, etc.

Data source: `events.Provider.List()` and `events.Provider.Watch()`.

#### Dispatch

```
POST /v0/sling                       # route work to agent
```

Sling is Gas City's work dispatch: find/spawn agent → select formula →
create molecule → hook to agent → nudge. It takes a target agent
and a bead or formula name.

There is no unsling — sling is one-way routing. To reassign, the agent
closes the work and it gets re-routed.

### 4) Blocking Queries

Every GET endpoint supports Nomad/Consul-style blocking queries. This gives
watch semantics to any read endpoint for free.

**Mechanism:** The event bus maintains a monotonically increasing sequence
number (`events.Provider.LatestSeq()`). Every response includes an
`X-GC-Index` header with the current sequence.

```
GET /v0/agents
→ 200 OK
→ X-GC-Index: 42
→ [{"name":"worker-1","running":true}, ...]

GET /v0/agents?index=42&wait=30s
→ (blocks until event bus sequence > 42, or 30s elapses)
→ 200 OK
→ X-GC-Index: 57
→ [{"name":"worker-1","running":true}, {"name":"worker-2","running":true}]
```

Parameters:
- `index` — block until state index exceeds this value
- `wait` — max block time (default `30s`, max `5m`; jitter of `wait/16`)

Response headers:
- `X-GC-Index` — current event sequence number

If the returned index is **less** than the requested index (event log
rotation), reset to 0. Following Consul guidance.

**Implementation:** The blocking query handler:
1. Reads `index` param from request
2. Calls `events.Provider.LatestSeq()`
3. If current > requested: serve immediately
4. Else: call `events.Provider.Watch(ctx, index)` and wait
5. On wake or timeout: serve current state

This reuses the existing `Watch()` implementation (250ms file poll) without
modification.

### 5) Server-Sent Events

`GET /v0/events/stream` provides real-time event streaming for browser
consumers (the dashboard).

```
GET /v0/events/stream?since=1h
→ 200 OK
→ Content-Type: text/event-stream

event: agent.started
id: 57
data: {"type":"agent.started","actor":"controller","subject":"worker-1","message":"started session"}

event: bead.created
id: 58
data: {"type":"bead.created","actor":"worker-1","subject":"gc-789","message":"Fix login timeout"}

: keepalive
```

- Events carry the existing `events.Event` types (`agent.started`,
  `bead.created`, `mail.sent`, etc.)
- The SSE `id` field is the event sequence number, enabling automatic
  reconnection via `Last-Event-ID`
- Keepalive comments sent every 15 seconds
- `since` parameter filters initial replay (default: no replay)

The dashboard subscribes to this stream and selectively refreshes panels
based on event type — instead of polling 3 commands every 2 seconds.

### 6) Response Envelope

Lightweight envelope. No `apiVersion`/`kind` ceremony — the URL tells you
what you're getting.

**Single resource:**

```json
{
  "name": "gc-123",
  "created": "2026-03-04T09:12:11Z",
  "title": "Fix login bug",
  "status": "open",
  "priority": 2,
  "labels": ["gc:feature"],
  "assignee": ""
}
```

Resources are returned directly — no wrapping object. The response headers
carry the index (`X-GC-Index`).

**List:**

```json
{
  "items": [...],
  "total": 47,
  "continue": "eyJ..."
}
```

**Why no `apiVersion`/`kind`/`spec`/`status`:**

Beads are not declarative Kubernetes resources. They don't have a "desired
state" (spec) separate from "actual state" (status). A bead's fields ARE
its state. The Kubernetes object model adds ceremony without benefit here.

If a future version needs resource-type metadata, we add a `kind` field
then. Premature now.

### 7) Error Model

Structured errors with machine-readable codes. One error per response
(not an array — keeps it simple).

```json
{
  "code": "not_found",
  "message": "bead \"gc-999\" not found"
}
```

For validation errors with field-level detail:

```json
{
  "code": "invalid",
  "message": "invalid mail request",
  "details": [
    {"field": "to", "message": "required"},
    {"field": "subject", "message": "exceeds 500 bytes"}
  ]
}
```

Error codes and HTTP status mapping:

| Code | HTTP | When |
|------|------|------|
| `invalid` | 400 | Bad parameters, validation failure |
| `not_found` | 404 | Resource doesn't exist |
| `conflict` | 409 | Concurrent modification, already assigned |
| `gone` | 410 | Watch index expired (event log rotated) |
| `timeout` | 504 | Operation timed out |
| `internal` | 500 | Server error |

### 8) Authentication (v0)

**Localhost only.** The API listens on `127.0.0.1` by default (not `0.0.0.0`).
Same trust model as the existing unix socket.

Config:

```toml
[api]
port = 8080
bind = "127.0.0.1"   # default; set to "0.0.0.0" for network access
```

Future versions add `X-GC-Token` header auth (HashiCorp pattern) gated
behind `bind = "0.0.0.0"`. Non-local listeners without auth are rejected
at startup.

### 9) Internal Architecture

Package: `internal/api/`

```
internal/api/
  server.go              # Server struct, ListenAndServe, mux setup, middleware
  envelope.go            # JSON helpers, error response, X-GC-Index header
  blocking.go            # Blocking query wait logic (index + wait params)
  sse.go                 # SSE writer (event formatting, keepalive, flush)
  handler_status.go      # GET /v0/status, GET /health
  handler_agents.go      # /v0/agents, /v0/agent/{name}
  handler_rigs.go        # /v0/rigs, /v0/rig/{name}
  handler_beads.go       # /v0/beads, /v0/bead/{id}
  handler_mail.go        # /v0/mail
  handler_convoys.go     # /v0/convoys, /v0/convoy/{id}
  handler_events.go      # /v0/events, /v0/events/stream
  handler_dispatch.go    # /v0/sling
```

**Server struct dependencies:**

```go
type Server struct {
    cfg       *config.City
    beads     beads.Store       // bead queries across rigs
    mail      mail.Provider     // mail with threading, read/unread, reply
    events    events.Provider   // event list, watch, latest seq
    sessions  session.Provider  // agent session state
    http      *http.Server
}
```

The `Server` takes the same interfaces the controller already creates.
No new abstractions needed.

**Middleware stack:**
1. Request logging (method, path, duration, status)
2. Panic recovery
3. CORS (for dashboard cross-origin when running standalone)
4. X-GC-Index header injection

No OTP-style supervision tree. Go's `http.Server` with `context.Context`
provides the lifecycle management. If the HTTP server panics, the recovery
middleware catches it per-request. If the listener fails, the controller
logs it — the controller itself continues to function (API is optional).

### 10) What the Controller Changes

**Config addition:**

```go
// In config.City
type APIConfig struct {
    Port int    `toml:"port"`    // 0 = disabled (default)
    Bind string `toml:"bind"`    // "127.0.0.1" default
}
```

Progressive activation: if `[api]` section is absent or `port = 0`, no
HTTP listener starts.

**Controller startup addition** (in `runController`):

```go
if cfg.API.Port > 0 {
    apiSrv := api.New(cfg, beadStores, eventProvider, sessionProvider)
    go func() {
        addr := fmt.Sprintf("%s:%d", cfg.API.Bind, cfg.API.Port)
        log.Printf("api: listening on http://%s", addr)
        if err := apiSrv.ListenAndServe(addr); err != nil && err != http.ErrServerClosed {
            log.Printf("api: %v", err)
        }
    }()
    defer apiSrv.Shutdown(ctx)
}
```

### 11) Data Source Mapping

What moves from subprocess to in-memory:

| Dashboard data | Current (subprocess) | API (in-memory) |
|---|---|---|
| City status | `gc status --json` | `config.City` + `session.Provider` |
| Agent list | `gc session list --json` | `session.Provider.ListRunning()` + config (pool expansion via `poolAgents()`) |
| Convoy list | `bd list --type=convoy --json` | `beads.Store.List(type=convoy)` |
| Convoy tracking | `bd dep list <id> -t tracks --json` | `beads.Store.Deps(id, "tracks")` |
| Issues | `bd list --status=open --json` | `beads.Store.List(status=open)` |
| Issue details | `bd show <ids> --json` | `beads.Store.Get(ids)` |
| Mail | `bd list --label=gc:message --json` | `mail.Provider` (wraps beads with threading/read semantics) |
| Escalations | `bd list --label=gc:escalation --json` | `GET /v0/beads?label=gc:escalation` |
| Queues | `bd list --label=gc:queue --json` | `GET /v0/beads?label=gc:queue` |
| Active work | `bd list --status=hooked --json` | Folded into `/v0/agents` (`active_bead` per agent) |
| Ready work | `bd ready --json` | `GET /v0/beads/ready` |
| Issue create | `bd create ...` | `POST /v0/beads` |
| Issue close | `bd close <id>` | `POST /v0/bead/{id}/close` |
| Issue update | `bd update <id> ...` | `POST /v0/bead/{id}/update` |
| Events | `gc events --json --since 1h` | `events.Provider.List(since=1h)` |
| Health | `gc status --json` + `gc session list --json` | Controller state + session provider |
| Sessions | `tmux list-sessions` | Folded into `/v0/agents` (session block per agent) |
| Crew state | `tmux display-message -t ... #{pane_current_command}` | Folded into `/v0/agents` (session.command per agent) |
| Merge queue / PRs | `gh pr list --json`, `gh pr view --json` | Stays in dashboard (GitHub-specific, not Gas City state) |

**Result:** All Gas City data sources become in-memory reads. GitHub
integration (PR list, PR details) stays in the dashboard — it's
gastown-specific, not SDK state. Session data and crew state detection
are accessed through agents — tmux is an implementation detail.

### 12) Dashboard Migration

The dashboard (`examples/gastown/packs/dashboard/`) migrates in phases:

**Phase 1: Read endpoints.** Dashboard's `fetcher.go` switches from
`runGcCmd`/`runBdCmd` to `http.Get("http://localhost:PORT/v0/...")`.
The 14 goroutine fan-out stays but each goroutine does an HTTP call
instead of spawning a subprocess.

**Phase 2: SSE.** Dashboard's SSE poller switches from hashing 3
subprocess outputs to subscribing to `/v0/events/stream`. Panel
refreshes become event-driven instead of poll-driven.

**Phase 3: Mutations.** Dashboard's `/api/run` callers switch to typed
POST endpoints (`/v0/mail`, `/v0/sling`, etc.). The command
whitelist and `ValidateCommand` machinery are removed.

**Phase 4: Embed UI.** Static assets (HTML/CSS/JS) move into
`internal/api/ui/` and are served via `go:embed`. The controller
serves both the API and the dashboard UI. The pack's command becomes
`gc dashboard open` (opens browser).

Current → v0 endpoint mapping:

| Current endpoint | v0 endpoint |
|---|---|
| `GET /` (14 subprocess fan-out) | `GET /v0/status` + individual resource GETs |
| `GET /api/crew` | `GET /v0/agents` (expanded pool members, session state, active_bead, command) |
| `GET /api/mail/inbox` | `GET /v0/mail?agent=X` |
| `GET /api/mail/read?id=X` | `POST /v0/mail/{id}/read` |
| `GET /api/mail/thread?id=X` | `GET /v0/mail/thread/{thread-id}` |
| `POST /api/mail/send` | `POST /v0/mail` |
| `POST /api/mail/reply` | `POST /v0/mail/{id}/reply` |
| `FetchQueues()` (gc:queue label) | `GET /v0/beads?label=gc:queue` |
| `GET /api/issues/show?id=X` | `GET /v0/bead/{id}` |
| `POST /api/issues/create` | `POST /v0/beads` |
| `POST /api/issues/close` | `POST /v0/bead/{id}/close` |
| `POST /api/issues/update` | `POST /v0/bead/{id}/update` |
| `GET /api/ready` | `GET /v0/beads/ready` |
| `GET /api/session/preview` | `GET /v0/agent/{name}/output` |
| `FetchSessions()` | `GET /v0/agents` (session block per agent, pool members expanded) |
| `GET /api/pr/show` | Stays in dashboard (shells out to `gh pr view` — GitHub-specific) |
| `FetchMergeQueue()` | Stays in dashboard (shells out to `gh pr list` — GitHub-specific) |
| `GET /api/events` (SSE hash poll) | `GET /v0/events/stream` (SSE) |
| `GET /api/commands` | Removed (typed endpoints replace command palette) |
| `GET /api/options` | Removed (each typed endpoint returns its own options) |
| `POST /api/run` | Removed (typed mutation endpoints) |

Note: The current dashboard was ported from upstream gastown. Gas City
now supports threading, subject/body separation, and reply chains in
its mail system, so thread views in the dashboard have data to display.

## Primitive Test

Not applicable — this proposal does not add a Layer 0-1 primitive. It adds
an HTTP transport layer over existing primitives (beads, events, session,
config). The API server is a derived access mechanism, composing:

- **Task Store (Beads)** — bead CRUD, query, deps
- **Event Bus** — event list, watch, streaming
- **Agent Protocol** — session start/stop/query
- **Config** — rig list, agent definitions, city metadata

No new persistence, no new state machines, no new abstractions.

## Drawbacks

**Compatibility surface.** Once external tools depend on `/v0/` endpoints,
changing them requires migration. Mitigated by the `v0` prefix (explicit
instability signal) and by deferring `/v1/` until the contract is proven
via the dashboard.

**Bead store coupling.** The API server imports `internal/beads` directly,
tying it to the current bead store interface. If the bead store interface
changes, the API handlers must update. This is acceptable — the API and
beads are in the same repo and the same binary.

**Controller complexity.** The controller gains an HTTP server. This
increases its surface area. Mitigated by: the API is optional
(progressive activation), the handlers are stateless query translators
(no business logic), and the HTTP server runs in a separate goroutine
with independent error handling.

**No offline access.** The API only works when the controller is running.
The CLI (`gc status`, `gc session list`) can work without a controller by
reading files directly. The API cannot. This is acceptable — the dashboard
already requires a running city.

## Alternatives

### A: Keep subprocess-based dashboard

Don't build an API server. The dashboard continues to spawn `gc` and `bd`
subprocesses.

Advantages: No new code. No compatibility surface.

Rejected because: 20+ subprocess spawns per page load is untenable for a
real-time dashboard. SSE polling spawns 3 processes every 2 seconds. This
gets worse as we add more panels. And Go source in packs remains wrong.

### B: Kubernetes-style API groups (`/apis/gc.dev/v1alpha1/cities/{city}/...`)

Full Kubernetes API conventions: `apiVersion`, `kind`, `metadata` with
`resourceVersion`, `spec`/`status` split, API group discovery.

Advantages: Familiar to Kubernetes users. Rich metadata model.

Rejected because: Gas City is not Kubernetes. It has ~15 resource types,
one city per controller, and beads are not declarative (no spec/status
distinction). The URL depth (`/apis/gc.dev/v1alpha1/cities/bright-lights/convoys/gc-1kp`)
is 7 segments — `/v0/convoy/gc-1kp` is 3. The ceremony adds cognitive cost
without proportional benefit. If Gas City ever needs API groups, we add
them at `/v1/` time.

### C: Separate `gc-apiserver` binary (ArgoCD pattern)

API server as a standalone binary that connects to the controller via
the unix socket or shared state.

Advantages: Process isolation. Independent scaling.

Rejected because: The controller already owns all the state. A separate
binary would need to either re-read files (duplicating work) or add an
IPC protocol to the controller (adding complexity). The Nomad/Consul model
(API embedded in the same process) is simpler and proven at scale. If
process isolation is ever needed, we can extract later.

### D: gRPC with REST gateway (ArgoCD/Temporal pattern)

Define services in protobuf, use grpc-gateway for REST.

Advantages: Type-safe RPC. Streaming via gRPC. Code generation.

Rejected because: Gas City's consumers are browsers (dashboard) and shell
scripts (`curl`). gRPC adds proto compilation, code generation, and tooling
complexity. The dashboard can't consume gRPC directly — it would need the
REST gateway anyway. Plain HTTP REST with SSE covers all v0 use cases.
Reconsider if programmatic SDK clients emerge at scale.

## Unresolved Questions

### Before accepting this design

1. **Bead store interface sufficiency.** Does `beads.Store` expose all the
   query capabilities the API needs (label filter, status filter, type
   filter, dependency traversal, pagination)? If not, what extensions are
   needed?

2. **Cross-rig bead aggregation.** The controller initializes per-rig bead
   stores. Should the API server aggregate across rigs by iterating stores,
   or should there be a unified query interface? What happens when one rig's
   store is unavailable?

### During implementation

3. **Pagination tokens.** What's the cursor format? Opaque base64 of
   `rig:offset`?

4. **Event sequence stability.** The file-based event recorder scans the
   entire log to find `LatestSeq()`. For blocking queries that call this
   frequently, we may need to cache the latest sequence in memory.

5. **CORS configuration.** Should the dashboard pack be able to specify
   allowed origins, or is `*` acceptable for localhost?

6. **Dashboard embedding timeline.** At what point do we move static assets
   into the controller binary? After Phase 2 (SSE migration) or Phase 4?

### Deferred to v1

7. **Convoy refresh.** `gc convoy refresh` is in the dashboard's allowed
   commands. Unclear what it does — may be a bead update or a
   recalculation of convoy progress. Track for v1.

8. **GitHub integration proxy.** PR list and PR detail stay in the
   dashboard for v0 (it shells out to `gh` directly). A future
   `/v1/integrations/github/...` could consolidate this if other packs
   also need GitHub data.

## Architecture Evolution

The v0 design embeds the API in the controller because the controller owns
all the state and a separate binary has no good way to access it (see
Alternatives C).

But this is the starting position, not the end state. The natural evolution
follows the Kubernetes pattern:

```
v0 (now):   controller embeds API → API calls beads.Store directly
v1 (later): API server owns state → controller is a client of the API
```

**v0:** The controller creates `beads.Store`, `events.Provider`,
`session.Provider` and passes them to `api.New(...)`. The API handlers are
consumers of these interfaces.

**v1:** The API server becomes the state authority. It owns the store
interfaces and exposes them over HTTP. The controller's reconciliation loop
becomes a client — it reads agent state from `GET /v1/agents` and writes
mutations through `POST /v1/agent/{name}/resume`. Same as how
kube-controller-manager is a client of kube-apiserver, not of etcd.

This resolves the two-writer problem cleanly: only the API server writes to
the bead store and event bus. The controller, dashboard, CLI, and any future
tool are all clients of the same API. One writer, many readers.

**What enables this later split:**

The v0 `internal/api/` package takes interfaces, not concrete types. It has
no import of `cmd/gc/`. When the time comes to extract:

1. Create `cmd/gc-apiserver/main.go`
2. Instantiate the store/event/session providers directly
3. Pass them to `api.New(...)`
4. Refactor controller to call HTTP endpoints instead of Go interfaces
5. The API package doesn't change at all

The package boundary does the work now. The deployment model changes later.

## Implementation Plan

### Phase 1: Core read endpoints (medium)

- Add `[api]` to config
- Create `internal/api/` package with server, envelope, and blocking query support
- Implement handlers: status, health, agents, rigs, beads, events (list)
- Controller starts HTTP listener when configured
- Tests: handler unit tests with mock stores

Delivers: API server that serves all dashboard read data from in-memory state.
`curl` can query the running city.

### Phase 2: Streaming (small)

- Implement SSE handler (`/v0/events/stream`)
- Implement blocking query support on all GET handlers
- Wire `events.Provider.Watch()` into both mechanisms

Delivers: Real-time updates without polling. Dashboard SSE can switch from
hash-based subprocess polling to event stream subscription.

### Phase 3: Mutations (medium)

- Implement write handlers: bead create/close/update, mail send/read/archive/reply,
  convoy create/add/close, agent suspend/resume/kill/drain/nudge,
  rig suspend/resume/restart, sling
- Input validation on all mutation endpoints

Delivers: Complete API surface for dashboard migration. `/api/run` can be
removed from the dashboard.

### Phase 4: Dashboard migration + UI embedding (large)

- Rewrite dashboard `fetcher.go` to use API instead of subprocesses
- Rewrite dashboard `api.go` to proxy to gc-apiserver
- Move static assets into `internal/api/ui/` with `go:embed`
- Add `gc dashboard open` command
- Remove Go source from pack

Delivers: Single-binary dashboard served by the controller. Pack contains
only `pack.toml` and doctor check.

### Phase 5: Controller as API client (future, large)

- Controller reconciliation loop calls API instead of stores directly
- API server becomes the single writer to beads/events
- Controller can be restarted independently of API server
- Extract `cmd/gc-apiserver/` as a separate binary

Delivers: Clean separation of state authority (API) from control loops
(controller). The Kubernetes architecture, earned incrementally.

## External Patterns and References

- Nomad HTTP API (URL structure, blocking queries, embedded UI):
  https://developer.hashicorp.com/nomad/api-docs
- Consul HTTP API (blocking queries, consistency, hash-based blocking):
  https://developer.hashicorp.com/consul/api-docs
- Consul blocking queries (index semantics, jitter, reset-on-decrease):
  https://developer.hashicorp.com/consul/api-docs/features/blocking
- Prometheus HTTP API (response envelope, error format, `/api/v1/` prefix):
  https://prometheus.io/docs/prometheus/latest/querying/api/
- Vault health endpoint (state-dependent HTTP status codes):
  https://developer.hashicorp.com/vault/api-docs/system/health
- Kubernetes API concepts (watch, resourceVersion, bookmarks):
  https://kubernetes.io/docs/reference/using-api/api-concepts/
- Kubernetes API conventions (object metadata, naming):
  https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md
