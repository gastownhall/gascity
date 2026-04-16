# Plan: Replace Network Layer with Huma + OpenAPI 3.1

## Status: In Progress (Phase 2 substantially complete)

### Progress
- **Phase 0 (Setup):** Complete. Huma v2.37.3 added, adapter wired into server.go.
- **Phase 1 (Patterns):** Complete. Health, status endpoints migrated. Generic types
  (ListOutput[T], IndexOutput[T], BlockingParam) established.
- **Phase 2 (Bulk CRUD):** ~75 of ~125 non-SSE endpoints migrated. OpenAPI spec has
  52 paths / 75 operations.
  - Migrated: agents, providers, rigs, patches, config, city, events, orders,
    formulas, convoys, services, extmsg, packs, sling
  - Remaining: sessions (18 endpoints — complex: pagination, immutable field
    detection, async status codes), beads (12 endpoints — handler files need
    recreation), mail (10 endpoints — handler files need recreation)
- **Phase 3 (SSE):** Not started. 4 SSE endpoints remain on old handlers.
- **Phase 4 (Cleanup):** Not started.
- **Phase 5 (Polish):** Not started.

## Context

Gas City has ~169 HTTP REST endpoints and 3-4 SSE streaming endpoints, all using
stdlib `net/http` with manual JSON serialization in every handler. There is no
API specification. The goal: annotated Go types become the single source of truth
for wire format, validation, and OpenAPI spec — no manual JSON, no separate spec
file, no drift.

## Decision Record

**Chose HTTP + SSE + OpenAPI over WebSockets + AsyncAPI** because:
- 169 endpoints are CRUD-shaped; HTTP is the natural fit
- SSE handles the unidirectional streaming use cases
- OpenAPI tooling is vastly more mature than AsyncAPI for Go
- Performance difference is unmeasurable for a localhost dev-tool API

**Chose Huma over Fuego** because:
- OpenAPI 3.1 (Fuego is 3.0 only) — aligns with existing JSON schema generation
- Built-in SSE with typed event mapping (Fuego requires manual http.Flusher)
- Handler signature uses stdlib `context.Context` (Fuego uses custom context)
- 3x community size, more battle-tested

## Architecture

### Before (current)

```
HTTP Request
    |
    v
http.ServeMux route matching
    |
    v
middleware chain (requestID, CORS, recovery, logging, CSRF)
    |
    v
handler_*.go  (manual json.Decode → business logic → manual json.Encode)
    |
    v
envelope.go writeJSON / writeListJSON / writeSSE
```

### After (with Huma)

```
HTTP Request
    |
    v
http.ServeMux route matching (unchanged)
    |
    v
existing middleware chain (unchanged)
    |
    v
Huma adapter (humago)
    |
    v
Huma operation dispatch:
  - Deserialize request into typed Input struct
  - Validate against struct tag constraints
  - Call handler: func(ctx, *Input) (*Output, error)
  - Serialize Output to JSON response
  - Format errors as RFC 9457
    |
    v
/openapi.json served live from registered types (always in sync)
```

### What changes

| Layer | Before | After |
|---|---|---|
| Route registration | `s.mux.HandleFunc("GET /v0/agents", s.handleAgentList)` | `huma.Get(api, "/v0/agents", s.handleAgentList)` |
| Handler signature | `func(w http.ResponseWriter, r *http.Request)` | `func(ctx context.Context, input *AgentListInput) (*AgentListOutput, error)` |
| Request parsing | `decodeBody(r, &req)` + manual query/path parsing | Automatic from Input struct tags |
| Response writing | `writeJSON(w, 200, resp)` | `return &Output{Body: resp}, nil` |
| Error responses | `writeJSON(w, 4xx, Error{...})` | `return nil, huma.Error404NotFound("msg")` |
| SSE streaming | Manual `writeSSE()` + goroutine + ticker | Hybrid: `sse.Register()` for type mapping + custom watcher loop |
| API spec | None | Auto-generated at `/openapi.json` from registered types |
| Validation | Manual checks in each handler | Struct tags (`minLength`, `pattern`, `enum`) |

### What stays the same

- `http.ServeMux` as the router (Huma wraps it via `humago` adapter)
- Middleware chain (CORS, CSRF, logging, recovery, request ID)
- Internal packages (beads, events, config, sling, convoy, etc.)
- Domain types and business logic
- Dashboard static files and HTML rendering
- Service proxy (`/svc/*`)

## Type Design

### Principle: Go types ARE the API contract

Every endpoint has an Input struct and an Output struct. These structs:
1. Define the wire format (via `json:` tags)
2. Define validation rules (via huma struct tags)
3. Define documentation (via `doc:` and `example:` tags)
4. Generate the OpenAPI spec (via huma reflection at startup)

No separate spec file. No code generation step. The spec endpoint
serves what the code actually does.

### Reducing type proliferation with generics

Huma's reflection-based OpenAPI generation works with Go generics. Generic
types get schema names like `ListOutputAgentResponse`. This lets us define
the list envelope once:

```go
// Generic list envelope — one type covers ALL list endpoints
type ListOutput[T any] struct {
    Index int `header:"X-GC-Index" doc:"Latest event sequence number"`
    Body  struct {
        Items      []T    `json:"items"`
        Total      int    `json:"total"`
        NextCursor string `json:"next_cursor,omitempty"`
    }
}

// Usage:
// GET /v0/agents returns *ListOutput[AgentResponse]
// GET /v0/beads  returns *ListOutput[BeadResponse]
// GET /v0/rigs   returns *ListOutput[RigResponse]
```

For inputs, embed common parameter patterns:

```go
type WaitParam struct {
    Wait string `query:"wait" doc:"Block until state changes (Go duration string)"`
}

type PaginationParam struct {
    Cursor string `query:"cursor" doc:"Pagination cursor from previous response"`
    Limit  int    `query:"limit" doc:"Max results per page" minimum:"1" maximum:"1000"`
}

type AgentListInput struct {
    WaitParam
    PaginationParam
    Pool string `query:"pool" doc:"Filter by pool name"`
}
```

This eliminates ~50% of output type definitions and standardizes input patterns.

### Example: Agent endpoints

```go
// --- Input types ---

type AgentGetInput struct {
    Name string `path:"name" doc:"Agent name" example:"deacon-1"`
}

type AgentCreateInput struct {
    Body struct {
        Name     string `json:"name" minLength:"1" doc:"Agent name"`
        Provider string `json:"provider,omitempty" doc:"Provider name"`
        Dir      string `json:"dir,omitempty" doc:"Working directory"`
    }
}

type AgentUpdateInput struct {
    Name string `path:"name" doc:"Agent name"`
    Body struct {
        Provider  string `json:"provider,omitempty"`
        Suspended *bool  `json:"suspended,omitempty"`
    }
}

// --- Output types ---

type AgentResponse struct {
    Name        string       `json:"name" doc:"Agent name"`
    Description string       `json:"description,omitempty" doc:"Agent description"`
    Running     bool         `json:"running" doc:"Whether agent is actively running"`
    Suspended   bool         `json:"suspended" doc:"Whether agent is suspended"`
    Rig         string       `json:"rig,omitempty" doc:"Associated rig"`
    Pool        string       `json:"pool,omitempty" doc:"Pool membership"`
    Provider    string       `json:"provider,omitempty" doc:"Provider name"`
    State       string       `json:"state,omitempty" doc:"Current state"`
    Session     *SessionInfo `json:"session,omitempty" doc:"Active session info"`
}

// GET /v0/agents handler:
func (s *Server) handleAgentList(ctx context.Context, input *AgentListInput) (*ListOutput[AgentResponse], error) {
    // ... business logic ...
    return &ListOutput[AgentResponse]{
        Index: idx,
        Body: struct {
            Items      []AgentResponse `json:"items"`
            Total      int             `json:"total"`
            NextCursor string          `json:"next_cursor,omitempty"`
        }{Items: agents, Total: len(agents)},
    }, nil
}
```

## Error Format Migration

### Current error format (`envelope.go`)

```go
type Error struct {
    Code    string       `json:"code"`
    Message string       `json:"message"`
    Details []FieldError `json:"details,omitempty"`
}

// Usage:
writeError(w, 404, "not_found", "agent not found")
// → {"code":"not_found","message":"agent not found"}
```

### Huma error format (RFC 9457)

```go
huma.Error404NotFound("agent not found")
// → {"status":404,"title":"Not Found","detail":"agent not found"}
```

### Migration decision: adopt RFC 9457

RFC 9457 (Problem Details for HTTP APIs) is a standard. The current custom
`{code, message}` format is equivalent but non-standard. Adopting RFC 9457:

- Better tooling support (clients that understand problem details)
- `status` field is numeric (easier for programmatic error handling)
- `detail` and `title` fields are standard and well-documented
- `errors` array for validation errors replaces custom `details`

**Breaking change:** Error response shape changes. Mitigations:
- The dashboard uses the HTTP status code, not the JSON body, for error handling
- The CLI client (`internal/api/client.go`) checks status codes first
- Update any code that parses `code` or `message` fields from error JSON
- The `writeStoreError` pattern (mapping `beads.ErrNotFound` → 404) becomes
  a simple `if errors.Is(err, beads.ErrNotFound) { return nil, huma.Error404NotFound(...) }`

### Custom error helper for store errors

```go
func storeError(err error) error {
    if errors.Is(err, beads.ErrNotFound) {
        return huma.Error404NotFound(err.Error())
    }
    return huma.Error500InternalServerError(err.Error())
}
```

## Idempotency Caching

### Current pattern (`idempotency.go`)

Create endpoints accept an `Idempotency-Key` header. A two-phase protocol
prevents duplicates:
1. `reserve(key, bodyHash)` — atomically reserve the key
2. Handler executes the create
3. `complete(key, status, body, hash)` — cache the response for replay

Subsequent requests with the same key replay the cached response.
Different body → 422. In-flight → 409.

### Huma approach: Huma middleware

The idempotency cache operates at the HTTP level (reads headers, writes
raw bytes for replay). Implement as a Huma middleware:

```go
func idempotencyMiddleware(cache *idempotencyCache) func(huma.Context, func(huma.Context)) {
    return func(ctx huma.Context, next func(huma.Context)) {
        key := ctx.Header("Idempotency-Key")
        if key == "" {
            next(ctx)
            return
        }

        // Read and hash the body for duplicate detection
        body, _ := io.ReadAll(ctx.Body())
        bodyHash := hashBytes(body)
        ctx.SetBody(io.NopCloser(bytes.NewReader(body))) // re-wrap for Huma

        scopedKey := ctx.Method() + ":" + ctx.URL().Path + ":" + key

        existing, found := cache.reserve(scopedKey, bodyHash)
        if found {
            // Replay cached or return conflict
            handleCachedIdempotency(ctx, existing, bodyHash)
            return
        }

        // Proceed — handler runs, then we capture the response
        next(ctx)
        // Note: capturing response for cache requires a response wrapper
    }
}
```

**Alternative:** Keep idempotency as handler-level logic (called at the top
of each create handler). This is simpler and avoids the complexity of
intercepting Huma's response serialization. The handler calls
`cache.handleIdempotent()` before doing work, same as today but with
the `Idempotency-Key` read from the Huma input struct.

**Recommendation:** Keep as handler-level logic. The idempotency cache is
only used on a few create endpoints. Adding it as middleware would intercept
all requests unnecessarily. Declare the header in the input struct:

```go
type BeadCreateInput struct {
    IdempotencyKey string `header:"Idempotency-Key" doc:"Retry key for safe creates"`
    Body struct {
        Title  string `json:"title" minLength:"1"`
        Type   string `json:"issue_type"`
        // ...
    }
}
```

## Response Caching

### Current pattern (`response_cache.go`)

Short-lived (2-second TTL) cache for expensive responses (agent lists,
order feeds, formula feeds). Keyed by handler name + query string, tied
to the event sequence index. If the index matches and TTL hasn't expired,
raw cached JSON bytes are written directly.

### Huma approach: handler-level caching with `huma.StreamResponse`

The response cache stores raw `[]byte` JSON. Huma normally serializes
typed structs. For cache hits, use `huma.StreamResponse` to write
cached bytes directly, bypassing Huma's serialization:

```go
func (s *Server) handleAgentList(ctx context.Context, input *AgentListInput) (*AgentListCacheableOutput, error) {
    idx := s.latestIndex()
    cacheKey := responseCacheKey("agents", input)

    // Check cache
    if body, ok := s.cachedResponse(cacheKey, idx); ok {
        return &AgentListCacheableOutput{
            Index:  idx,
            Cached: body,
        }, nil
    }

    // Build response
    agents := s.buildAgentList()
    resp := ListBody[AgentResponse]{Items: agents, Total: len(agents)}

    // Store in cache
    s.storeResponse(cacheKey, idx, resp)

    return &AgentListCacheableOutput{
        Index: idx,
        Body:  resp,
    }, nil
}
```

**Alternative (simpler):** Accept the small overhead of re-serializing
on cache hit. The cache stores the typed struct instead of raw bytes.
At 2-second TTL and localhost latency, the JSON marshal cost is negligible.
This lets all endpoints use the standard Huma output pattern.

**Recommendation:** Switch the response cache to store typed structs
instead of raw bytes. The serialization cost is negligible for the
response sizes involved. This avoids the complexity of `StreamResponse`
for cache hits and keeps all handlers using the same output pattern.

```go
type responseCache[T any] struct {
    mu      sync.Mutex
    entries map[string]responseCacheEntry[T]
    ttl     time.Duration
}

type responseCacheEntry[T any] struct {
    index   uint64
    expires time.Time
    value   T
}
```

## SSE Streaming Design (researched)

### What Huma's SSE supports

| Capability | Supported | Notes |
|---|---|---|
| Multiple event types | Yes | Via `eventTypeMap` — maps Go struct types to SSE event names |
| `Last-Event-ID` reading | Manual | Must declare `LastEventID string \`header:"Last-Event-ID"\`` in input struct |
| Event ID on outgoing events | Yes | Via `sse.Message{ID: seqNum, Data: payload}` |
| Keepalive comments | No | Must implement manually with a ticker in the stream function |
| Context cancellation | Yes | Client disconnect cancels the handler's context |
| Blocking stream function | Yes | Can block indefinitely on channels/watchers |
| OpenAPI documentation | Yes | Event types appear in the spec |

### Approach: Huma SSE with custom watcher loop

Huma's `sse.Register()` provides the typed event mapping and OpenAPI
documentation. Our stream function implements the watcher, reconnection,
and keepalive logic — the same patterns as today, but with typed event
structs instead of manual JSON:

```go
type EventStreamInput struct {
    AfterSeq    uint64 `query:"after_seq" doc:"Resume from this sequence number"`
    LastEventID string `header:"Last-Event-ID" doc:"SSE reconnection sequence"`
    // ... filter params
}

sse.Register(api, huma.Operation{
    OperationID: "stream-events",
    Method:      http.MethodGet,
    Path:        "/v0/events/stream",
    Summary:     "Stream city events in real time",
}, map[string]any{
    "event":     EventStreamEnvelope{},
    "heartbeat": HeartbeatEvent{},
}, func(ctx context.Context, input *EventStreamInput, send sse.Sender) {
    // Determine start position from Last-Event-ID or after_seq query param
    startSeq := input.AfterSeq
    if input.LastEventID != "" {
        startSeq, _ = strconv.ParseUint(input.LastEventID, 10, 64)
    }

    watcher := s.state.EventProvider().Watch(startSeq)
    defer watcher.Close()

    ticker := time.NewTicker(15 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return  // client disconnected
        case event := <-watcher.Next():
            send(sse.Message{
                ID:   int(event.Seq),
                Data: toEnvelope(event),  // typed Go struct
            })
        case <-ticker.C:
            send(sse.Message{Data: HeartbeatEvent{}})
        }
    }
})
```

### Session streaming (3 modes)

The session stream endpoint has three modes. Each returns different event types:

```go
type SessionStreamInput struct {
    ID          string `path:"id" doc:"Session ID"`
    Format      string `query:"format" doc:"Output format: conversation or raw"`
    LastEventID string `header:"Last-Event-ID"`
}

sse.Register(api, huma.Operation{
    OperationID: "stream-session",
    Method:      http.MethodGet,
    Path:        "/v0/session/{id}/stream",
}, map[string]any{
    "transcript": SessionTranscriptEvent{},
    "peek":       SessionPeekEvent{},
    "snapshot":   SessionSnapshotEvent{},
}, func(ctx context.Context, input *SessionStreamInput, send sse.Sender) {
    session := s.getSession(input.ID)
    switch {
    case session.IsClosed():
        // Replay from JSONL log
        s.streamSessionTranscriptLog(ctx, session, send)
    case session.IsRunning():
        // Live tmux pane polling
        s.streamSessionPeek(ctx, session, send)
    default:
        // Snapshot of current state
        send(sse.Message{Data: buildSnapshot(session)})
    }
})
```

### Fallback: `huma.StreamResponse` for complex SSE cases

If `sse.Register()` proves insufficient for a specific streaming endpoint
(e.g., the session stream's three modes with different content types, or
the orders/feed response caching), Huma provides `huma.StreamResponse`
as a lower-level escape hatch:

```go
type StreamResponse struct {
    Body func(ctx huma.Context) error
}
```

This gives direct access to the `io.Writer` and `http.Flusher`, so you
can write SSE frames manually — same as today's `writeSSE()` — while
still getting Huma's input parsing and OpenAPI documentation for the
request side. The trade-off: event types won't be documented in the
OpenAPI spec automatically.

**When to use `StreamResponse` vs `sse.Register()`:**
- `sse.Register()`: standard streaming endpoints where all events share
  a common structure (events/stream, formulas/feed, orders/feed)
- `StreamResponse`: endpoints with mode-switching (session/stream with
  its three modes) or complex response caching that doesn't fit the
  `Sender` callback model

## Supervisor / Multi-City Architecture (researched)

### Each city gets its own Huma API instance

Huma API instances are fully independent — separate schema registries,
separate OpenAPI specs, no shared singleton state. This maps directly
to the existing SupervisorMux pattern:

```go
// Each city creates its own huma.API wrapping its own mux
func NewCityServer(state State) *Server {
    mux := http.NewServeMux()
    api := humago.New(mux, huma.DefaultConfig("Gas City", "0.1.0"))

    s := &Server{mux: mux, api: api, state: state}
    s.registerRoutes()  // registers all 169 endpoints on this city's API
    return s
}
```

### Supervisor stays outside Huma

The supervisor has just a few endpoints (`/v0/cities`, `/health`,
`/v0/city/{name}/...` routing). It stays as raw `http.ServeMux` handlers.
The city-level OpenAPI spec is served at `/v0/city/{name}/openapi.json`.

Rationale: the supervisor is a routing layer, not an API surface. Its
3-4 endpoints don't justify a separate Huma instance. Documenting them
in a README is sufficient.

### Dynamic city instances

Cities start/stop at runtime. Creating a new `huma.API` instance per city
is fine — the reflection cost is negligible (one-time at city startup).
Cache the API instance per city; recreate only when the city's State
pointer changes (indicating a config reload or restart).

### Read-only mode

Keep the existing `withReadOnly()` middleware on the mux level. It wraps
the entire handler chain including Huma. No changes needed — mutations
get rejected before Huma even sees them.

The OpenAPI spec will still list all endpoints. Clients get 403 on
mutations, which is the correct semantic for "this server is read-only."

## Blocking reads (`?wait=...` pattern) (researched)

Huma handlers can block indefinitely. No built-in request timeout
conflicts with long-polling. The handler just blocks on a channel:

```go
type AgentListInput struct {
    WaitParam  // embeds Wait string `query:"wait"`
}

func (s *Server) handleAgentList(ctx context.Context, input *AgentListInput) (*ListOutput[AgentResponse], error) {
    if input.Wait != "" {
        dur, _ := time.ParseDuration(input.Wait)
        waitCtx, cancel := context.WithTimeout(ctx, dur)
        defer cancel()
        s.waitForChange(waitCtx)  // blocks until event or timeout
    }

    agents := s.buildAgentList()
    return &ListOutput[AgentResponse]{...}, nil
}
```

Context cancellation propagates correctly — if the client disconnects
during a wait, the handler's context is cancelled.

## Migration Automation (researched)

### Strategy: hybrid AST scanner + template generator

Full AST-driven code transformation is not worth the effort (diminishing
returns on the last 15% of handlers). Instead:

**Step 1: AST scanner (4-6 hours to build)**

Scans all 31 handler files and produces `endpoints.json`:
```json
[
  {
    "func_name": "handleAgentList",
    "route": "GET /v0/agents",
    "method": "GET",
    "has_body_decode": false,
    "query_params": ["pool", "suspended", "wait"],
    "path_params": [],
    "response_type": "agentResponse",
    "response_writer": "writeListJSON",
    "has_sse": false,
    "has_custom_headers": true,
    "line_range": [45, 92]
  },
  ...
]
```

**Step 2: Stub generator (2-3 hours)**

Reads `endpoints.json`, emits for each endpoint:
- Input struct with query/path/header/body fields
- Output struct (or uses `ListOutput[T]` for list endpoints)
- Huma registration call
- Handler signature with TODO placeholder for business logic

**Step 3: Manual migration (bulk of the work)**

Developer copies business logic from old handler into new handler stub.
The scanner flags ~15-20 endpoints that need special attention (SSE,
custom headers, conditional responses). The other ~150 are mechanical.

**Why not full automation:** The business logic between "parse input" and
"write output" has too many variations (error branches, conditional
responses, multi-step queries) for reliable AST extraction. The scanner
identifies what needs to change; humans move the logic.

## Migration strategy

### Phase 0: Setup (1 PR)
- Add `github.com/danielgtaylor/huma/v2` dependency
- Create `humago.New()` adapter wrapping existing mux in `server.go`
- Serve `/openapi.json` and `/docs` endpoints
- No handler changes — just the wiring
- Build the AST scanner tool in `cmd/genmigrate/` (or a script)

### Phase 1: Establish patterns (1-2 PRs)
- Define shared generic types: `ListOutput[T]`, `SingleOutput[T]`
- Define shared input mixins: `WaitParam`, `PaginationParam`
- Migrate 5-10 simplest endpoints to establish patterns:
  - `GET /health` (no input, simple output)
  - `GET /v0/status` (no input, structured output)
  - Agent CRUD (path params, list response, body decode)
- Delete corresponding old handler code as each migrates
- Verify OpenAPI spec includes migrated endpoints
- Verify dashboard still works

### Phase 2: Bulk CRUD migration (2-3 PRs)
- Run AST scanner to generate `endpoints.json`
- Run stub generator to produce handler skeletons
- Migrate in batches by domain:
  - Beads (CRUD + graph + dependencies)
  - Sessions (create, list, get, patch, transcript)
  - Mail (CRUD + threads)
  - Convoys (CRUD + progress)
  - Orders and Formulas (list, detail, enable/disable)
  - Rigs, Providers, Patches, Config endpoints
  - Workspace services, ExtMsg, Packs, Sling

### Phase 3: SSE streaming endpoints (1 PR)
- `GET /v0/events/stream` — Huma SSE with custom watcher loop + keepalive
- `GET /v0/session/{id}/stream` — Huma SSE with 3 streaming modes
- `GET /v0/orders/feed` — Huma SSE with response caching
- `GET /v0/formulas/feed` — Huma SSE with response caching
- `Last-Event-ID` handled via input struct header field
- Keepalive via manual 15-second ticker (same as today)
- Remove `sse.go` helper file

### Phase 4: Cleanup (1 PR)
- Remove `envelope.go` (writeJSON, writeListJSON, writePagedJSON, writeIndexJSON)
- Remove `decodeBody()` / `decodeBodyBytes()`
- Remove old response types replaced by Huma output structs
- Remove AST scanner tool (one-time use)
- Update dashboard API proxy if response shapes changed
- Update CLI client code (`internal/api/client.go`)

### Phase 5: Polish
- Add `doc:` and `example:` tags for API documentation quality
- Serve Swagger UI at `/docs` for interactive API exploration
- Consider generating a TypeScript client from OpenAPI spec for dashboard

## Files to modify

**Core changes:**
- `internal/api/server.go` — add Huma adapter, migrate route registration
- `internal/api/handler_*.go` (31 files) — change handler signatures, remove manual JSON
- `internal/api/envelope.go` — eventually delete
- `internal/api/sse.go` — eventually delete
- `go.mod` — add huma dependency

**New files:**
- `internal/api/types.go` — shared generic output types, input mixins
- `cmd/genmigrate/main.go` — AST scanner (temporary, removed in Phase 4)

**Unchanged:**
- `internal/api/middleware.go` — stays as-is (wraps mux, not Huma)
- `internal/api/state.go` — interface unchanged
- `internal/api/supervisor.go` — stays as raw http.ServeMux
- All internal packages (beads, events, config, sling, convoy, etc.)
- Dashboard HTML/JS (same HTTP endpoints, same response shapes)

## Verification

At each phase:
- `go test ./...` passes
- `go vet ./...` clean
- OpenAPI spec at `/openapi.json` validates
- Dashboard still works (start dev server, test golden paths)
- SSE streaming works (subscribe to events, trigger activity, see updates)
- `curl` smoke tests against key endpoints
- Response shapes haven't changed (backward compatible for existing clients)

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| Huma SSE keepalive: no built-in comment frames | Manual 15s ticker in stream function (same pattern as today) |
| Huma SSE reconnection: no built-in `Last-Event-ID` handling | Declare in input struct as `header:"Last-Event-ID"` — works, just manual |
| Response shape changes break dashboard | Migrate one endpoint, test dashboard, then batch |
| Huma middleware doesn't compose with existing middleware | Existing middleware stays on the mux level — no conflict (verified) |
| 169 endpoints is a lot of migration work | AST scanner automates stub generation; business logic copy is mechanical |
| Generic output types don't work with Huma OpenAPI | Verified: Huma reflection handles generics, generates schema names like `ListOutputAgentResponse` |
| SupervisorMux multi-city routing conflicts with Huma | Verified: each city gets independent `huma.API` instance, no shared state |
| Blocking `?wait=...` handlers conflict with Huma timeouts | Verified: no built-in timeout, context cancellation works correctly |
| Read-only mode breaks with Huma | Verified: existing `withReadOnly()` middleware works unchanged on the mux level |
| Error format change breaks clients | Dashboard uses HTTP status codes (unaffected). CLI client checks status first (unaffected). Any code parsing `code`/`message` fields needs update to RFC 9457 `status`/`detail` fields |
| Idempotency cache bypasses Huma serialization | Keep as handler-level logic with `Idempotency-Key` in input struct — no middleware complexity |
| Response cache stores raw bytes, incompatible with Huma typed output | Switch cache to store typed structs. Serialization cost is negligible at 2s TTL + localhost |
