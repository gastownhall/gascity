# Implement #646: Supervisor WebSocket Transport

## Summary

Issue #646 should be implemented as a phased transport migration, not as a single PR that deletes the entire HTTP/SSE surface at once.

The goal is to make WebSocket the primary transport for supervisor-aware clients while preserving the current product architecture:

- CLI and other Go clients move from HTTP/SSE to a typed WebSocket client
- the dashboard server becomes the WebSocket client upstream to the supervisor
- the browser remains HTMX + server-rendered HTML and keeps talking to the dashboard server, not directly to the supervisor
- operational HTTP endpoints and non-client mounts stay on HTTP where that still makes sense

The implementation should ship with a machine-readable protocol contract:

- AsyncAPI for the WebSocket protocol
- Modelina for shared DTO/message model generation
- hand-written Go transport/runtime logic on top of that contract

## Implementation Guardrails

The implementation should follow these constraints throughout all phases:

- TDD first: add or update failing protocol/client parity tests before each migrated transport slice, then implement until those tests pass
- Layered architecture: keep transport code at the edge, a typed application/execution layer in the middle, and existing domain logic below it
- Serialization only at the edges: decode WebSocket/HTTP payloads into typed DTOs at the boundary, operate on typed values internally, and encode only on the way out
- DRY and SRP: extract shared request execution, event fan-out, scope resolution, and error mapping instead of duplicating them across HTTP and WebSocket handlers
- KISS and YAGNI: do not add speculative chunking, browser rewrites, new auth models, or transport-specific abstractions that are not needed for current parity
- Async notifications over polling: use subscriptions for ongoing state changes; retain one-shot watch semantics only where needed to match existing `index` + `wait` behavior
- Stateless reconnect semantics: rely on cursors, idempotency keys, and explicit subscription state instead of sticky server affinity or hidden per-client state
- No swallowed errors: invalid envelopes, size-limit violations, keepalive failures, scope mismatches, and reconnect/resume failures must produce structured errors or close codes and be logged centrally
- Observability by default: add connection/request/subscription logs, metrics, and trace points for handshake, dispatch, subscription lifecycle, reconnect, close reasons, and backpressure/drop conditions
- Maintainability over cleverness: prefer small typed helpers and incremental extraction over large framework-style rewrites

## Target Architecture

### WebSocket endpoint placement

Expose the same WebSocket protocol on both API server types:

- per-city server: `GET /v0/ws`
- supervisor mux: `GET /v0/ws`

The protocol is shared, but scope handling differs:

- per-city server: city scope is implicit
- supervisor mux: city scope is carried in the message envelope for city-targeted operations

This preserves current client behavior, because today callers can hit either:

- a standalone/city-local API server
- the supervisor API with city-scoped routing

### Dashboard architecture

Do not convert the dashboard browser to a browser-direct supervisor WebSocket client in this issue.

The dashboard is currently HTMX + server-rendered HTML with server-side data fetching and SSE fan-out. Replacing that with direct browser WebSocket access would turn 646 into a dashboard rewrite. Instead:

- keep the browser-to-dashboard-server boundary intact
- migrate the dashboard server’s upstream transport from HTTP/SSE to WebSocket
- preserve the dashboard’s existing browser-facing HTMX/SSE behavior until a separate dashboard architecture effort exists

### Protocol framing

Use JSON envelopes with explicit request/response/event typing.

Client request envelope:

- `type: "request"`
- `id`
- `action`
- optional `idempotency_key` for create/retry-safe operations that currently rely on `Idempotency-Key`
- `scope` (optional; includes `city` where needed)
- `payload`

Server response envelope:

- `type: "response"`
- `id`
- optional `index` carrying the current event sequence for parity with `X-GC-Index`
- `result`

Server error envelope:

- `type: "error"`
- `id`
- `code`
- `message`
- optional typed details

Server event envelope:

- `type: "event"`
- `subscription_id`
- `event_type`
- optional cursor/resume token
- payload

Server hello envelope:

- `type: "hello"`
- protocol version
- server role (`city` or `supervisor`)
- read-only / mutation capability
- supported actions and subscription kinds

### Connection and concurrency model

The protocol should assume concurrent in-flight requests on a single socket:

- clients may send multiple requests without waiting for prior responses
- the server may process requests concurrently
- responses are correlated by `id` and may arrive out of request order
- subscription events may interleave with responses
- ordering is guaranteed only within a single subscription stream as defined by its cursor semantics

Implementation guidance:

- keep a single serialized writer per connection
- make dispatcher/request handlers safe for concurrent execution
- do not rely on request/response ordering for correctness

### Keepalive and liveness

Replace SSE keepalive comments with native WebSocket liveness:

- server sends periodic ping frames
- clients must respond with pong frames
- idle/dead connections are closed proactively
- reconnecting clients use normal cursor/resume mechanisms where supported

### Subscription model

Use explicit subscribe/unsubscribe requests over the socket.

Initial subscription families should match existing streaming surfaces and blocking-watch semantics:

- global events feed
- city-scoped events feed
- session stream
- one-shot blocking query equivalents for existing `index` + `wait` patterns

There is no distinct WebSocket "agent output stream" subscription kind in v1. The canonical streaming surface is session-scoped. If legacy HTTP compatibility keeps an agent-output stream alias during coexistence, it should map internally onto the session stream model rather than introducing a second protocol concept.

Blocking HTTP reads such as `?index=...&wait=...` should map to one of:

- a request option like `watch: {index, wait}` for one-shot “wait until changed” semantics
- or a short-lived subscription with a clear completion condition

Do not lose the current cursor/reconnect behavior:

- SSE `Last-Event-ID` / `after_seq`
- supervisor global composite cursor behavior

Session stream subscriptions need explicit parameters and completion rules:

- support the current session stream format modes rather than assuming one generic shape
- closed sessions emit a bounded snapshot/terminal sequence and then complete instead of remaining live forever
- live sessions remain open and continue streaming updates with normal cursor semantics

## Migration Scope

### Client-facing API domains that must be accounted for

The plan must treat the supervisor client surface as the current full set of client-used domains, not a narrow subset. At minimum, the migration inventory needs to cover:

- supervisor/global: cities, readiness, provider readiness, health, global events
- city status/config: status, config, config explain/validate
- agents: list/get, actions, output surfaces
- rigs: list/get, CRUD/actions where client-facing
- sessions: list/get, transcript, pending, stream, messages/submit/respond/wake/kill/close/rename/agents
- beads: list/get/graph/ready/update/assign/close/reopen/delete/create
- mail
- convoys
- orders / formulas / workflow aliases
- providers and provider CRUD
- patches
- services (status/restart only; keep `/svc/` proxy on HTTP)
- sling
- packs
- extmsg
- events list/emit/stream

The implementation can migrate these in phases, but the plan must inventory them up front so “full cutover” has a concrete meaning.

### Out of scope for #646

- dashboard browser rewrite into a client-rendered SPA
- new remote mutation auth model for non-localhost supervisor access
- replacing workspace service HTTP proxy mounts (`/svc/`) with WebSocket

## Implementation Phases

### Phase 1: Protocol foundation and shared execution layer

- Write failing protocol tests first for handshake, correlation, close/error behavior, and the first migrated request/response actions
- Add AsyncAPI spec for the WebSocket protocol and use Modelina for shared envelope/payload DTOs
- Introduce the transport-neutral execution layer incrementally, not as a full up-front rewrite of all HTTP handlers
  - start with shared query/command functions for the first migrated domains
  - continue extracting typed inputs/outputs and shared error mapping as each domain moves
  - avoid duplicating business logic between HTTP and WebSocket, but do not block phase 1 on extracting the entire API surface at once
- Add `GET /v0/ws` to both `internal/api.Server` and `internal/api.SupervisorMux`
- Implement handshake, request dispatch, error envelopes, and subscription lifecycle
- Preserve current read-only semantics in the WebSocket layer
- Keep HTTP/SSE endpoints live during this phase

### Phase 2: Streaming parity

- Write failing parity tests first for each migrated SSE/blocking surface
- Migrate existing SSE/event surfaces to WebSocket subscriptions:
  - per-city events
  - supervisor global events
  - session stream
- Migrate blocking query semantics that depend on `X-GC-Index`, `index`, and `wait`
- Add reconnect/cursor parity tests for event and session flows
- Keep old SSE endpoints live until all internal clients have switched

### Phase 3: Go client migration

- Write failing client parity tests first for the migrated `internal/api.Client` methods and CLI fallback paths
- Replace `internal/api.Client` HTTP transport with a persistent WebSocket client while preserving the existing high-level method surface where practical
- Preserve current routing behavior:
  - standalone city-local client path
  - supervisor client path
  - implicit single-running-city behavior where it exists today
  - explicit `city_required` errors where it exists today
- Define supervisor-vs-city scoping behavior explicitly:
  - on supervisor sockets, `scope.city` is required whenever the current HTTP surface would require an explicit city
  - on per-city sockets, omitted `scope.city` means the implicit city
  - on per-city sockets, an explicit matching `scope.city` is accepted
  - on per-city sockets, a different `scope.city` is a validation error
- Preserve fallback behavior where CLI mutations currently fall back to direct file mutation on connection/read-only failures

### Phase 4: Dashboard server migration

- Write failing dashboard transport tests first for the upstream read and event paths
- Replace the dashboard server’s upstream HTTP/SSE usage with the new WebSocket client
- Keep browser-facing dashboard behavior stable:
  - server-rendered HTML
  - HTMX refreshes
  - dashboard-local CSRF model
  - dashboard SSE/browser update loop, unless a smaller internal refactor can remove it without browser architecture churn
- The dashboard server, not the browser, is the supervisor WebSocket client in this issue
- The dashboard `/api/run` subprocess execution path is not part of this migration; the migration target is the dashboard server’s upstream API fetches and SSE proxy path

### Phase 5: Remove legacy client transport

- After CLI and dashboard server are both running on WebSocket with parity, remove the old HTTP/SSE client paths
- Keep only the HTTP endpoints that still make sense as operational/public surface:
  - `/health`
  - `/v0/readiness`
  - `/v0/provider-readiness`
  - `POST /v0/city`
  - `/svc/` service proxy mounts
  - pprof/debug endpoints

## Security and Access Model

- Preserve current localhost/private-bind mutation semantics
- On WebSocket upgrade, validate `Origin` against the same localhost/private-host policy that protects the current browser-facing API surface
- On WebSocket connect, advertise read-only capability in `hello`
- Reject mutating actions when the server is in read-only mode
- The current HTTP `X-GC-Request` CSRF mechanism does not apply to WebSocket frames; mutation authorization is established at handshake time and enforced for the lifetime of the connection
- For supervisor-hosted browser traffic, do not invent a new browser-direct mutation model in this issue
- Keep the existing dashboard CSRF/browser protection model on the dashboard server boundary

## Operational Semantics

### City lifecycle under supervisor subscriptions

When a supervisor-scoped subscription targets a specific city and that city stops, restarts, or disappears:

- emit a terminal subscription event/error indicating the target city became unavailable
- end that subscription cleanly
- require the client to resubscribe after the city is available again

This matches current resolver-style behavior more closely than trying to make subscriptions survive arbitrary city process churn.

### WebSocket close codes

Use explicit close codes so clients can distinguish expected shutdown from policy errors:

- `1000` for normal close
- `1001` for server shutdown/restart or supervisor lifecycle transitions
- `1008` for policy violations such as invalid origin or forbidden mutation attempts
- `1011` for internal server errors where the connection cannot continue safely

### Message size limits

Set explicit message size limits rather than leaving large payload behavior implicit:

- enforce a bounded maximum inbound message size
- define and test bounded outbound behavior so oversized responses fail explicitly rather than hanging or truncating silently
- keep large transcript/content reads on typed request/response operations rather than inventing ad hoc chunking in phase 1
- if a response class proves too large for a single message in practice, add explicit chunked protocol support as a later protocol revision rather than silently truncating

### Observability and diagnostics

The WebSocket transport should emit enough telemetry to debug production failures without packet-level forensics:

- connection lifecycle logs with remote address, server role, origin decision, and close code
- request logs/traces keyed by request `id`, action, scope, latency, and outcome
- subscription lifecycle logs/traces keyed by subscription id, kind, scope, cursor, and termination reason
- metrics for active connections, active subscriptions, request latency, error counts, ping/pong failures, reconnect attempts, and oversize message rejections
- explicit logging for fallback-to-direct-mutation paths so transport failures are visible rather than silently masked

## Test Plan

### Protocol tests

- handshake on city server and supervisor mux
- request/response correlation
- structured error mapping
- malformed envelope rejection
- read-only mutation rejection
- city scope required vs auto-resolved behavior
- request/response concurrency and out-of-order response correlation
- ping/pong keepalive behavior and dead-connection detection
- idempotency-key replay protection for create operations
- close-code behavior for normal shutdown, policy violation, and internal error cases

### Subscription tests

- per-city event subscription
- global event subscription with composite cursor parity
- session stream parity
- unsubscribe behavior
- reconnect/resume behavior
- city-unavailable termination behavior on supervisor-scoped city subscriptions

### Client migration tests

- `internal/api.Client` parity tests for migrated methods
- CLI command tests covering API-routing paths and direct-mutation fallback
- dashboard upstream transport tests proving the dashboard server can render current views from WebSocket-sourced data

### Migration safety tests

- HTTP and WebSocket paths produce equivalent results during coexistence phases
- blocking query behavior preserves semantics
- standalone city-local server and supervisor mux both serve the same protocol correctly
- service proxy routes remain HTTP and unaffected
- transport failure paths are observable and preserve current CLI direct-mutation fallback behavior without silently masking errors

## Assumptions and Defaults

- The correct implementation path is phased, not one giant cutover PR
- The dashboard browser is not the direct supervisor WebSocket client for #646
- The dashboard server is a supervisor client and should migrate upstream transport
- WebSocket becomes the primary client transport, but HTTP remains where it is still the right operational interface
- AsyncAPI + Modelina is the approved OSS contract/codegen direction
- Fern is out of scope
- `.env` is already handled elsewhere and is not part of this plan
