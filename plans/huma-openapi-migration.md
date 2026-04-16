# Plan: Replace Network Layer with Huma + OpenAPI 3.1

## Status: Phase 1 Complete, Phase 2 In Progress

Phase 1 migrated 128 operations to Huma handlers with an auto-generated
OpenAPI 3.1 spec. Phase 2 makes the spec the engine, not a trophy.

### Core principle

**The OpenAPI spec drives all networking.** Annotated Go types define the
contract. Huma generates the spec from those types. Clients generate from
that spec. Zero hand-generated networking code — only Go endpoint
implementations and Go type definitions. Everything else is framework.

### Phase 1 summary (complete)
- 95 paths, 128 operations in the auto-generated spec
- All CRUD and SSE endpoints registered through Huma
- 5,600 lines of dead old handler code removed
- 1 old mux.HandleFunc remaining: `/svc/` proxy

### Phase 2: Spec-Driven API (in progress)

Phase 1 left gaps that undermine the core principle:

**2a. SSE event schemas missing from spec.** All 3 SSE endpoints use
`StreamResponse` which produces empty-body responses in the spec. Event
types (`eventStreamEnvelope`, session transcript events, agent output turns)
are invisible to clients reading the spec. **Fix:** Refactor to
`sse.Register()` with typed event maps so event schemas appear in the spec.
Remove `writeSSE()`/`writeSSEComment()` helpers — use Sender callback.

**2b. Validation bypassed with `omitempty`.** All 35 body input types use
`json:"field,omitempty"` to prevent Huma's 422. The spec marks all fields
as optional even when required. Handlers validate manually. **Fix:** Remove
`omitempty` from required fields, add proper validation tags (`minLength`,
`required`). Override `huma.NewError` for consistent error format. Accept
422 as the correct status for validation errors.

**2c. Three error formats.** RFC 9457, legacy `{code,message}`, and
`apiError`. `mutationError()` uses `strings.Contains` to guess HTTP status.
**Fix:** Define typed domain errors in each package. Single `domainError()`
encoder. Eliminate string matching. Consistent error format everywhere
including middleware.

**2d. Response cache uses hand-built string keys.** Add a query param,
forget to update the key, serve stale data. **Fix:** Generic cached handler
decorator that derives cache keys from input struct fields.

**2e. huma_types.go is a 1300-line monolith.** **Fix:** Split by domain
(agents, beads, sessions, etc.). Keep only shared generics in the base file.

**2f. Dual handler file pattern.** `handler_agents.go` (helpers) and
`huma_handlers_agents.go` (handlers) are confusing. **Fix:** Merge into
single domain files.

**2g. No typed client generation.** 128 operations in the spec, but no
generated clients. Dashboard uses hand-written fetch. CLI client hand-parses
responses. **Fix:** Generate typed Go client from `/openapi.json` using
`oapi-codegen` or `ogen`. Replace hand-written `client.go`. Consider
TypeScript client for dashboard. The spec becomes the single source of
truth for both server AND client.

**2h. Session state management is ad-hoc.** `huma_handlers_sessions.go` is
1200 lines with 16 handlers mixing state management, provider quirks,
naming, transcript logic, and legacy compat. State transitions are string
comparisons scattered across handlers. **Fix:** Extract an explicit session
state machine with typed states, a transition table, a single reducer for
legality, and a traceable event timeline.

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

### Migration decision: hybrid RFC 9457 + legacy

RFC 9457 (Problem Details for HTTP APIs) was adopted for Huma handlers, but
the legacy `{code, message}` format is preserved where needed:

- **Huma handlers** → RFC 9457 via `huma.Error*()` functions
- **Middleware** (read-only, CSRF, panic recovery) → legacy `{code, message}`
  via `writeError()` — these run before Huma and must match what `client.go`
  expects for the read-only fallback detection
- **Idempotency paths** (session/bead/mail create) → legacy `{code, message}`
  via `apiError` type for backward compatibility with existing test assertions

**client.go updated** to parse both formats: `json:"code"` + `json:"message"`
for legacy, `json:"status"` + `json:"detail"` for RFC 9457.

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

**What was implemented:** Kept the raw byte cache (`response_cache.go`).
On cache hit, Huma handlers call `json.Unmarshal` to decode cached bytes
back into a typed struct for return. The re-serialization cost is negligible
at 2-second TTL on localhost. This avoided rewriting the cache infrastructure
while keeping all handlers using the standard Huma output pattern.

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

### Approach: `huma.StreamResponse` for all SSE endpoints

All SSE endpoints use `huma.StreamResponse` — the handler returns a
`StreamResponse` whose `Body` callback sets SSE headers and delegates
to the existing streaming infrastructure. The `sse.Register()` API was
evaluated but not adopted: `StreamResponse` is simpler, gives direct
`http.ResponseWriter` access, and lets us reuse the existing `writeSSE()`
helpers without refactoring.

```go
func (s *Server) humaHandleEventStream(_ context.Context, input *EventStreamInput) (*huma.StreamResponse, error) {
    ep := s.state.EventProvider()
    if ep == nil {
        return nil, huma.Error503ServiceUnavailable("events not enabled")
    }

    afterSeq := input.resolveAfterSeq()
    watcher, err := ep.Watch(ctx, afterSeq)
    if err != nil {
        return nil, huma.Error503ServiceUnavailable("failed to start watcher")
    }

    return &huma.StreamResponse{
        Body: func(ctx huma.Context) {
            rw := ctx.BodyWriter().(http.ResponseWriter)
            ctx.SetHeader("Content-Type", "text/event-stream")
            streamProjectedEventsWithWatcher(ctx.Context(), rw, watcher, s.state)
        },
    }, nil
}
```

**Key pattern:** validation and watcher creation happen before the
`StreamResponse` is returned, so errors produce proper HTTP status codes.
The `Body` callback can't return errors — it just streams until the client
disconnects.

**SSE endpoints (3):**
- `GET /v0/events/stream` — event watcher with workflow projections
- `GET /v0/session/{id}/stream` — session log replay or live peek
- `GET /v0/agent/{name}/output/stream` — agent output polling

**Note:** `/v0/orders/feed` and `/v0/formulas/feed` are plain JSON endpoints
with response caching, not SSE streams. They were migrated as standard
Huma handlers.

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

### Phase 3: SSE + feed endpoints
- `GET /v0/events/stream` — `huma.StreamResponse` wrapping event watcher
- `GET /v0/session/{id}/stream` — `huma.StreamResponse` with 3 streaming modes
- `GET /v0/agent/{name}/output/stream` — `huma.StreamResponse` with log/peek polling
- `GET /v0/orders/feed` — standard Huma JSON handler (not SSE)
- `GET /v0/formulas/feed` — standard Huma JSON handler (not SSE)
- `sse.go` kept — `writeSSE()`/`writeSSEComment()` used by StreamResponse callbacks

### Phase 4: Cleanup
- Removed ~5,600 lines of dead old handler functions
- Removed unused envelope helpers (writePagedJSON, writeIndexJSON, etc.)
- Kept live envelope.go (writeJSON, writeError, writeListJSON) — used by
  middleware, supervisor, service proxy, city create, provider readiness
- Kept sse.go — used by StreamResponse callbacks
- Updated `client.go` to parse both legacy and RFC 9457 error formats

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
