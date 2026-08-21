# External Messaging — Connected-Client SSE Wire Contract

| Field | Value |
|---|---|
| Status | Draft |
| Date | 2026-06-19 |
| Author(s) | gascity/designer, gascity/architect |
| Issue | ga-1y4mb7, ga-31gfwg, ga-lyikvt |
| Supersedes | — |

This is the normative NFR-6 wire contract for the connected-client SSE
subscribe endpoint. It specifies the exact on-wire format — HTTP response
headers, SSE event field layout, JSON schemas with Go struct definitions,
error catalogs, cursor advancement rules, and versioning policy — for:

- The Go builder implementing `internal/api` and `internal/extmsg`.
- The tincan-iris client author consuming the stream.
- Any future WebSocket or gRPC transport carrying the same payload schema.

For the endpoint's HTTP error responses, replay semantics, and
implementation invariants see the companion doc
[`extmsg-connected-client-subscribe-contract.md`](./extmsg-connected-client-subscribe-contract.md).

For full feature architecture (token issuance model, subscriber registry,
`gc extmsg reply`, and trade-off analysis) see bead `ga-31gfwg`.

Related design docs:

- [`external-messaging-fabric.md`](./external-messaging-fabric.md) — `ConversationRef`, controller-assigned `AccountID` rule, `AdapterRegistry`.
- [`external-messaging-shared-threads.md`](./external-messaging-shared-threads.md) — `TranscriptService`, `ListBackfill`, replay substrate.

## 1. Endpoint

```
GET /v0/city/{cityName}/extmsg/clients/{client_id}/conversations/{conversation_id}/subscribe
```

**Required header:** `X-GC-Client-Token: <token>`

The `Authorization: Bearer` header is NOT used for this endpoint. The
client presents the token obtained from `POST /v0/extmsg/clients` via the
`X-GC-Client-Token` header. The server derives `client_id` from this token
and asserts it equals the `client_id` URL path segment.

**Optional header:** `Last-Event-ID: <sequence>` — decimal sequence number
of the last successfully received `message` event. Triggers transcript
backfill before live delivery.

## 2. HTTP response headers (on success)

| Header | Value | Notes |
|--------|-------|-------|
| `Content-Type` | `text/event-stream` | Required by W3C SSE spec. |
| `Cache-Control` | `no-cache` | Prevents proxy-level caching. |
| `Connection` | `keep-alive` | Required on HTTP/1.1; omit on HTTP/2. |
| `X-Accel-Buffering` | `no` | Disables nginx/Caddy proxy buffering. |

`Access-Control-Allow-Origin` is omitted in v1 — the endpoint is
loopback-only (`127.0.0.1`) and CORS does not apply.

## 3. SSE wire format

The stream follows [W3C Server-Sent Events](https://html.spec.whatwg.org/multipage/server-sent-events.html).
Each event block is terminated by a blank line (`\n\n`).

```
id: <value>\n
event: <type>\n
data: <JSON>\n
\n
```

Possible `event:` values: `message`, `heartbeat`, `error`.

For retryable error events, a `retry:` field is emitted **before** the
`id:` line to hint the client reconnect delay:

```
retry: 5000
id: error
event: error
data: {...}

```

### 3.1 `id:` field rules

| Event type | `id:` value | Effect on `Last-Event-ID` cursor |
|-----------|------------|----------------------------------|
| `message` | Decimal string of the `sequence` value, e.g. `"42"` | **Advances cursor** |
| `heartbeat` | Omitted | Does not change cursor |
| `error` | Literal string `"error"` | Does not advance cursor |

The `"error"` sentinel ensures that when a client reconnects after a
retryable error, the `Last-Event-ID` header it sends reflects the last
successfully received `message` sequence, not the error event. The server
uses this value as the `ListBackfill` start point.

Browser `EventSource` implementations automatically send `Last-Event-ID`
equal to the last `id:` received. Because `id: error` is a non-numeric
string, servers MUST treat a non-numeric or absent `Last-Event-ID` as "no
cursor" and begin live delivery without backfill. Implementation: parse
`Last-Event-ID` as `int64`; if parsing fails, treat as absent.

## 4. Event schemas

All event `data:` values are JSON objects. Every object includes `version`
and `event` as its first two fields (by convention; JSON field order is not
normative).

**Stability levels:**

- **stable** — Must not be renamed, removed, or have their type changed
  without a version increment. Clients may switch on these fields.
- **informational** — May change between minor releases without a version
  increment. Do not parse or display; use for logging only.

### 4.1 `message` event

Emitted when the target session produces a reply for this conversation.

```
id: 42
event: message
data: {"version":"1","event":"message","text":"Hello from the session.","session_id":"s-abc123","conversation":{"provider":"llm-client","account_id":"bd-token-bead-id","conversation_id":"conv-uuid-here"},"sequence":42,"created_at":"2026-06-19T19:32:39Z"}

```

**Fields:**

| Field | Type | Stability | Description |
|-------|------|-----------|-------------|
| `version` | string | stable | Schema version. Always `"1"` for v1. Clients MUST reject unknown versions (§6). |
| `event` | string | stable | Always `"message"`. |
| `text` | string | stable | The session's reply text. UTF-8. May be empty if the session produces only metadata. |
| `session_id` | string | stable | Bead ID of the session that produced this reply. |
| `conversation.provider` | string | stable | Always `"llm-client"` for connected-client streams. |
| `conversation.account_id` | string | stable | The controller-assigned `client_id`. Matches the URL path segment. |
| `conversation.conversation_id` | string | stable | Client-chosen conversation identifier. Matches the URL path segment. |
| `sequence` | int64 | stable | Monotonically increasing transcript sequence number. Value of the `id:` wire field. Use as `Last-Event-ID` on reconnect. |
| `created_at` | string (RFC 3339) | stable | UTC timestamp when the transcript entry was recorded. |

**Go struct:**
```go
// SSEMessageEvent is the data payload for event: message.
type SSEMessageEvent struct {
    Version      string          `json:"version"`
    Event        string          `json:"event"`
    Text         string          `json:"text"`
    SessionID    string          `json:"session_id"`
    Conversation ConversationRef `json:"conversation"`
    Sequence     int64           `json:"sequence"`
    CreatedAt    time.Time       `json:"created_at"`
}
```

### 4.2 `heartbeat` event

Emitted every 30 s (configurable via `city.toml`
`extmsg.connected_clients.heartbeat_interval`) when no `message` or `error`
event has been sent. Clients should reset a liveness timer on receipt. Has
no `id:` field and does not advance the replay cursor.

```
event: heartbeat
data: {"version":"1","event":"heartbeat","ts":"2026-06-19T19:45:00Z"}

```

**Fields:**

| Field | Type | Stability | Description |
|-------|------|-----------|-------------|
| `version` | string | stable | Schema version. Always `"1"` for v1. |
| `event` | string | stable | Always `"heartbeat"`. |
| `ts` | string (RFC 3339) | informational | Server wall-clock time at emission. Not monotonic relative to `message.created_at`. |

**Go struct:**
```go
// SSEHeartbeatEvent is the data payload for event: heartbeat.
type SSEHeartbeatEvent struct {
    Version string    `json:"version"`
    Event   string    `json:"event"`
    TS      time.Time `json:"ts"`
}
```

### 4.3 `error` event

Emitted when the server must close the stream. The server closes the HTTP
response body immediately after writing this event block. For retryable
errors the `retry:` SSE field is written before the event block.

**Wire example — retryable:**
```
retry: 5000
id: error
event: error
data: {"version":"1","event":"error","code":"session_stopped","message":"Session has stopped.","retryable":true,"retry_after_ms":5000}

```

**Wire example — non-retryable:**
```
id: error
event: error
data: {"version":"1","event":"error","code":"token_revoked","message":"The client token has been revoked. Re-register to obtain a new token.","retryable":false}

```

**Fields:**

| Field | Type | Stability | Description |
|-------|------|-----------|-------------|
| `version` | string | stable | Schema version. Always `"1"` for v1. |
| `event` | string | stable | Always `"error"`. |
| `code` | string | stable | Machine-readable error code. Clients switch on this. See §5. |
| `message` | string | informational | Human-readable description for logs. Not stable; do not parse. |
| `retryable` | bool | stable | `true` → reconnect is safe. `false` → client must take explicit action first. |
| `retry_after_ms` | int64 | stable | Minimum reconnect delay (ms). Present only when `retryable: true`. |

**Go struct:**
```go
// SSEErrorEvent is the data payload for event: error.
type SSEErrorEvent struct {
    Version      string `json:"version"`
    Event        string `json:"event"`
    Code         string `json:"code"`
    Message      string `json:"message"`
    Retryable    bool   `json:"retryable"`
    RetryAfterMs *int64 `json:"retry_after_ms,omitempty"`
}
```

## 5. Error catalogs

### 5.1 Pre-stream HTTP errors

Returned before the server writes `Content-Type: text/event-stream`.

**Response body:**
```json
{"code": "session_forbidden", "message": "..."}
```

| HTTP | `code` | Condition |
|------|--------|-----------|
| 401 | `unauthorized` | `X-GC-Client-Token` header missing, malformed, or token not found in token store. |
| 403 | `session_forbidden` | Token valid; `client_id` matches token; but the token's `allowed_sessions` list does not include the target session. |
| 403 | `account_mismatch` | Token valid; but `client_id` URL path segment ≠ `client_id` derived from token. Programming error on client side. |
| 404 | `session_not_found` | Target session name not in city's active config at subscribe time. |
| 503 | `extmsg_unavailable` | External messaging not enabled in city config, or controller not ready. |

> **Missing binding is not a pre-stream error.** When no binding exists for the `ConversationRef`, the server opens the SSE stream and begins sending heartbeats. A binding is established on the first `gc extmsg reply`; live `message` events follow. Clients that subscribe before any reply are expected to wait on the open heartbeat stream.

### 5.2 Post-stream SSE error events

> **v1 status:** SSE `error` frame production is schema-reserved in v1. The `SSEErrorEvent` type, fields, and codes below are present in the OpenAPI schema and generated client SDK for forward compatibility. The server does not yet emit any `error` frames in-stream: when a session stops or the controller shuts down the HTTP response body is closed silently. A `server_shutdown` emitter requires a server-shutdown context (see `internal/api/server.go`) and is deferred.

| `code` | `retryable` | `retry_after_ms` | Trigger |
|--------|-------------|-----------------|---------|
| `session_stopped` | true | 5 000 | Target session stopped cleanly. Binding and transcript retained. |
| `session_not_found` | true | 10 000 | Target session removed from config while stream was open. Binding retained. |
| `server_shutdown` | true | 3 000 | Controller process shutting down. Binding and transcript durable on disk. |
| `idle_timeout` | true | 0 | Reserved; not emitted in v1. |
| `token_revoked` | false | — | Operator revoked the token. Client must call `POST /v0/extmsg/clients` with new credential. Existing bindings for the old `client_id` are retained. |
| `binding_removed` | false | — | Conversation binding removed by operator or API call. Client must send new inbound turn before subscribing again. |
| `account_mismatch` | false | — | `account_id` in URL ≠ `client_id` from token (may race token re-issuance). Programming error; fix before retrying. |

## 6. Versioning contract

### 6.1 Client MUST

- Read `version` on every received event.
- Reject events with an unknown `version` value — close the connection and
  do not reconnect automatically until the client library is updated.
- Ignore unknown JSON fields within a known version (standard JSON extensibility).

### 6.2 Server MUST (before incrementing `version`)

1. Publish a migration notice in the user docs.
2. Emit a `meta_version_change` event on existing streams at least one
   major release before dropping the old version.
3. Support both versions in parallel for a documented deprecation window.

### 6.3 Backwards-compatible (no version bump required)

Adding new optional JSON fields to an existing event type. Clients ignore unknown fields.

### 6.4 Requires version bump

- Removing or renaming any **stable** field.
- Changing the type or semantics of any **stable** field.
- Changing the `id:` sentinel behavior for any event type.
- Adding a new required field to an existing event type.

## 7. Transport-agnostic rule (NFR-6)

The field set in §4 is designed so a future WebSocket or gRPC transport
carries the same JSON without semantic changes:

- All semantic content is in the `data:` JSON object, not in SSE wire
  fields (`id:`, `event:`). A WebSocket frame carries the same JSON verbatim.
- The `event` field inside JSON discriminates event types without the SSE
  `event:` line.
- `sequence` inside JSON replaces `id:` for cursor tracking; a WS client
  uses `sequence` directly for `Last-Event-ID` equivalents.
- `retry_after_ms` inside JSON replaces the SSE `retry:` field; a gRPC
  stream uses this for backoff hints.

Adding a WebSocket transport requires only a framing shim; no changes to
`SSEMessageEvent`, `SSEHeartbeatEvent`, or `SSEErrorEvent`.

## 8. Contract test requirements (NFR-6 gate)

The builder MUST implement `TestSSEEventRoundTrip` before the implementation
PR is opened. The test validates:

```go
// TestSSEEventRoundTrip validates each SSE event type:
// 1. Marshals to JSON with correct field names and types.
// 2. Unmarshals back without data loss.
// 3. Contains all required stable fields.
// 4. version field equals "1".
// 5. event field matches the expected type name.
// 6. Stable field names match those in this contract doc.
func TestSSEEventRoundTrip(t *testing.T) { ... }
```

Once the new endpoints are Huma-registered, `TestOpenAPISpecInSync` must
also pass (existing CI invariant).

## 9. Deferred / follow-up items

These are explicitly deferred from v1 scope. File follow-up beads as needed:

| Item | Reason deferred |
|------|----------------|
| `idle_timeout` SSE error | No idle-timeout policy in v1; code reserved for future use. |
| `Access-Control-Allow-Origin` | Loopback-only in v1; add when/if non-loopback is needed. |
| `meta_version_change` event type | No schema change planned in v1; reserved for versioning machinery. |
| WebSocket transport | Design deferred; same JSON payload, new framing layer only. |
| gRPC transport | Design deferred; same payload contract applies. |
| Per-client conversation cap | Explicitly no cap in v1; revisit under load. |
| `dropped_count` in `message` events | Buffer overflow signal; add if observability need is confirmed. |

## 10. Quick reference

| Scenario | Wire behavior |
|----------|---------------|
| Missing/invalid token | 401 HTTP (no stream) |
| Token not allowed for session | 403 HTTP (no stream) |
| client_id path ≠ token client_id | 403 HTTP (no stream) |
| Session not in config | 404 HTTP (no stream) |
| No binding for conversation | 200 + open stream, heartbeats until binding established |
| extmsg not enabled | 503 HTTP (no stream) |
| Stream open, session replies | `event: message`, `id: <sequence>` |
| Stream idle ≥ 30 s | `event: heartbeat`, no `id:` |
| Session stopped | `retry: 5000` + `event: error code: session_stopped` |
| Controller shutdown | `retry: 3000` + `event: error code: server_shutdown` |
| Token revoked | `event: error code: token_revoked` (no retry) |
| Binding removed | `event: error code: binding_removed` (no retry) |
| Reconnect with cursor | `Last-Event-ID: <N>` → backfill `sequence > N`, then live |
| Reconnect after error | `Last-Event-ID: <N>` (last message seq, not `"error"`) |
