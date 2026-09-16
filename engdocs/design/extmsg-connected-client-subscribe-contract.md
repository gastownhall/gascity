# External Messaging — Connected-Client Subscribe Contract

| Field | Value |
|---|---|
| Status | Draft |
| Date | 2026-06-19 |
| Author(s) | gascity/architect |
| Issue | ga-31gfwg, ga-lyikvt |
| Supersedes | — |

This document specifies the wire contract for the `GET /v0/city/{cityName}/extmsg/clients/{client_id}/conversations/{conversation_id}/subscribe` endpoint: pre-stream HTTP error responses, post-stream SSE event schema, replay semantics, and cursor advancement rules. It is the normative reference for both the Go implementation in `internal/api` and consuming clients (e.g., tincan-iris).

For the full feature architecture — token issuance model, subscriber registry design, `gc extmsg reply` command, and trade-off analysis — see bead `ga-31gfwg`.

Related design docs:

- [`external-messaging-fabric.md`](./external-messaging-fabric.md) — `ConversationRef`, `BindingService`, `DeliveryContext`, `AdapterRegistry`, controller-assigned `AccountID` rule.
- [`external-messaging-shared-threads.md`](./external-messaging-shared-threads.md) — `TranscriptService`, `MembershipBackfillPolicy`, `ListBackfill` — the basis for replay.

## Background

The external messaging fabric supports push-to-provider adapters (Discord, Slack) via `HandleOutbound → AdapterRegistry → adapter.Publish`. A generic external client — a local voice assistant, a CLI bot — has no provider API for the controller to push to. It needs to hold a connection open and receive replies as they stream.

This document specifies the receive leg. The send leg (`POST /v0/extmsg/inbound`) is unchanged.

## Endpoint

```
GET /v0/city/{cityName}/extmsg/clients/{client_id}/conversations/{conversation_id}/subscribe
```

**Path parameters:**

| Parameter | Description |
|-----------|-------------|
| `client_id` | The controller-issued `client_id` from `POST /v0/extmsg/clients`. Must match the `client_id` derived from the token in `X-GC-Client-Token`. |
| `conversation_id` | Client-chosen opaque conversation identifier (UUID recommended). Combined with `client_id` to form a `ConversationRef` with `Provider: "llm-client"`. |

**Required headers:**

| Header | Value |
|--------|-------|
| `X-GC-Client-Token` | The token returned by `POST /v0/extmsg/clients` at registration time. |

**Optional headers:**

| Header | Value |
|--------|-------|
| `Last-Event-ID` | Decimal sequence number of the last successfully received `message` event. Triggers transcript replay before live delivery. See §Replay. |

## Pre-stream errors (before SSE commitment)

These errors are returned as standard HTTP responses before the server commits `Content-Type: text/event-stream`. Once the server has written the SSE response headers, all subsequent errors travel as SSE `event: error` frames (see §Post-stream errors).

**Response body shape** (RFC 9457 Problem Details, as produced by Huma):

```json
{
  "title": "Forbidden",
  "status": 403,
  "detail": "session_forbidden: session is not permitted by this client token"
}
```

| HTTP Status | Code | Condition |
|-------------|------|-----------|
| `401 Unauthorized` | `unauthorized` | `X-GC-Client-Token` header missing or token unrecognized. |
| `403 Forbidden` | `account_mismatch` | Token valid, but the `client_id` in the URL path does not match the `client_id` derived from the token. |
| `403 Forbidden` | `session_forbidden` | Token valid, but the per-token `allowed_sessions` list does not include the session the conversation is bound to (only applies when a binding exists and `allowed_sessions` is non-empty). |
| `404 Not Found` | `session_not_found` | The requested session does not exist in the city's active config at subscribe time (only applies when a binding exists). |
| `503 Service Unavailable` | `extmsg_unavailable` | External messaging is not enabled in the city config, or the controller is not ready. |

> **Note (ga-gfuna2):** A missing binding is not a pre-stream error. The server opens the SSE stream and waits; a binding is created on the first `gc extmsg reply`. Clients subscribing before any reply has been sent will receive heartbeats until a binding is established and a reply arrives.

> **Browser EventSource note:** The `event: error` frame MUST carry `id: error` on the wire. Browser `EventSource` clients treat any `id:` value as the new `Last-Event-ID`. Because `error` is non-numeric, a subsequent reconnect will send `Last-Event-ID: error`, which the server treats as absent (triggering no backfill). This prevents an automatic reconnect loop after a terminal error. Programmatic clients (non-browser) should close the connection on any `event: error` frame regardless of the `id:` value.

## SSE response headers (on success)

```
HTTP/1.1 200 OK
Content-Type: text/event-stream
Cache-Control: no-cache
X-Accel-Buffering: no
```

`X-Accel-Buffering: no` disables proxy-level response buffering (nginx, Caddy) so events are not held in a proxy buffer before reaching the client.

## SSE event schema

Every SSE event has three wire fields:

| Wire field | Description |
|------------|-------------|
| `id:` | Decimal sequence number for `message` events. The literal string `error` for error events. Omitted for `heartbeat` events. |
| `event:` | One of `message`, `heartbeat`, `error`. |
| `data:` | JSON object. Always includes `"version":"1"`. |

The `data` JSON is the normative payload. All semantic content lives in `data`, not in the SSE wire fields, so the schema remains transport-agnostic: a future WebSocket or gRPC transport carries the same JSON in a single frame.

### message event

```
id: 42
event: message
data: {"version":"1","event":"message","text":"...","session_id":"sess-abc","conversation":{"provider":"llm-client","account_id":"client-xyz","conversation_id":"conv-001"},"sequence":42,"created_at":"2026-06-19T19:32:39Z"}

```

`data` fields:

| Field | Type | Stability | Description |
|-------|------|-----------|-------------|
| `version` | string | stable | Schema version. Always `"1"` for v1. Clients MUST reject unknown versions. |
| `event` | string | stable | Always `"message"`. Clients switch on this field. |
| `text` | string | stable | The session's reply text. |
| `session_id` | string | stable | Bead ID of the session that generated the reply. |
| `conversation.provider` | string | stable | Always `"llm-client"` on this endpoint. |
| `conversation.account_id` | string | stable | The controller-assigned `client_id` for this client. |
| `conversation.conversation_id` | string | stable | The client-chosen conversation identifier. |
| `sequence` | int64 | stable | Monotonically increasing transcript sequence number. Matches the `id:` wire field. Used for `Last-Event-ID` replay. |
| `created_at` | string (RFC3339) | stable | Timestamp when the session reply was recorded in the transcript. |

Fields marked **stable** must not be renamed or removed in a patch or minor release. Clients may switch on them. Additional fields may appear in future versions without a version increment, as long as existing fields are not altered.

### heartbeat event

```
event: heartbeat
data: {"version":"1","event":"heartbeat","ts":"2026-06-19T19:45:00Z"}

```

Emitted every 30 s (configurable via `city.toml` `extmsg.connected_clients.heartbeat_interval`) when no `message` or `error` event has been sent. Clients reset their liveness timer on receipt.

`data` fields:

| Field | Type | Stability | Description |
|-------|------|-----------|-------------|
| `version` | string | stable | Schema version. |
| `event` | string | stable | Always `"heartbeat"`. |
| `ts` | string (RFC3339) | stable | Server wall-clock time at emission. |

**Heartbeat events have no `id:` wire field and do not advance the replay cursor.** See §Cursor advancement rules.

### error event

```
id: error
event: error
data: {"version":"1","event":"error","code":"session_stopped","message":"The target session was stopped.","retryable":true,"retry_after_ms":5000}

```

The `id: error` value is a non-numeric sentinel string. It does not advance the client's `Last-Event-ID` cursor. See §Cursor advancement rules.

After writing the error event, the server closes the HTTP response body. The client's TCP connection is released.

`data` fields:

| Field | Type | Stability | Description |
|-------|------|-----------|-------------|
| `version` | string | stable | Schema version. |
| `event` | string | stable | Always `"error"`. |
| `code` | string | stable | Machine-readable error code. Clients switch on this. |
| `message` | string | informational | Human-readable description. Suitable for logs. Not stable; do not parse it. |
| `retryable` | bool | stable | `true` if the client should reconnect (possibly with backoff). `false` if the error is permanent and the client must re-register or take explicit action. |
| `retry_after_ms` | int | stable | Minimum reconnect delay hint in milliseconds. Present only when `retryable: true`. Clients may apply exponential backoff on top. |

## Error code catalog

> **v1 status:** SSE `error` frame production is schema-reserved in v1. The event type, fields, and error codes below are in the OpenAPI schema and client SDK for forward compatibility, but the server does not yet emit any `error` frames in-stream. When a session stops or the controller shuts down the stream closes silently (HTTP body closed). See the companion wire contract §4.3 note.

### Retryable errors (reconnect is safe)

| Code | Retry hint (ms) | Trigger |
|------|----------------|---------|
| `session_stopped` | 5 000 | The target session stopped cleanly (e.g. `gc stop`). Binding and transcript are retained; client reconnects and re-subscribes. |
| `session_not_found` | 10 000 | The target session was removed from config while the stream was open. Retryable: operator may re-add it. |
| `server_shutdown` | 3 000 | The controller process is shutting down. The server also emits a standard SSE `retry:` hint before this event. Binding and transcript survive on disk. |
| `idle_timeout` | 0 | The server closed an idle stream (reserved; not emitted in v1). |

### Non-retryable errors (client must take action before reconnecting)

| Code | Required action |
|------|----------------|
| `token_revoked` | Re-register via `POST /v0/extmsg/clients` with a new credential. Existing bindings and transcript for the old `client_id` are retained on disk; re-subscribe after re-registration. |
| `binding_removed` | An operator or API call removed the conversation binding. Send a new inbound turn to re-create the binding before subscribing again. |
| `account_mismatch` | The `account_id` URL path param does not match the `client_id` derived from the presented token. This is a client programming error; fix the request before retrying. |

## Cursor advancement rules

These rules determine what value the client sends as `Last-Event-ID` on reconnect, and what the server replays.

| Event type | `id:` field | Advances cursor | Notes |
|------------|-------------|-----------------|-------|
| `message` | decimal sequence number | **yes** | Client updates its saved cursor to this value on receipt. |
| `heartbeat` | absent | **no** | Client does not update its cursor. |
| `error` | literal string `"error"` | **no** | Client does not update its cursor. On reconnect the client sends the cursor from the last successfully received `message` event. |

**Implementation note:** Browser `EventSource` automatically sends `Last-Event-ID` equal to the last `id:` received. Because `id: error` is a non-numeric string, implementations that store it verbatim and send it on reconnect will trigger a 400 response from the server (non-numeric `Last-Event-ID`). The server MUST treat a non-numeric or absent `Last-Event-ID` as "no cursor" and deliver from the beginning of the membership window. Go/Python SSE clients that manage `Last-Event-ID` manually MUST update the cursor only on `message` events.

## Replay

When the client reconnects and supplies `Last-Event-ID: <sequence>`:

1. The server registers the new adapter and subscriber channel **first**, before reading backfill. This prevents a race where a live reply arrives during replay and is dropped.
2. The server calls `TranscriptService.List(ConvRef, afterSequence: Last-Event-ID)` (cursor-based) to retrieve transcript entries with `sequence > Last-Event-ID`. `List` is deliberate over `ListBackfill` (membership/ack-based): `Last-Event-ID` replay must resume from the client's own cursor, not the membership window.
3. The server emits each backfill entry as a `message` event in ascending sequence order.
4. After exhausting backfill, the server switches to the live channel (events buffered in step 1 are drained immediately).

Replay window: ≥ 7 days (matches existing transcript retention).

If `Last-Event-ID` is absent or non-numeric: the server treats it as "no cursor" and begins live delivery from the current position (no backfill). The client receives only messages delivered after the stream opens.

## Versioning

`version` is present in every `data` object. Current value: `"1"`.

Rules for incrementing `version`:

- Removing or renaming a **stable** field requires a version increment.
- Adding new fields to the `data` object does **not** require a version increment (clients MUST ignore unknown fields).
- Changing the type of an existing field requires a version increment.
- A version increment MUST be accompanied by a docs update in this file and a migration note in the `CHANGELOG`.

Client behavior on receiving an unknown version: log the unknown version and reconnect to trigger the server to resend the event. Do not silently drop the event (the text it carries may be important).

## Connection lifecycle

```
Client                                        Server
  |                                              |
  |-- GET .../subscribe (Last-Event-ID: N) ----->|
  |                                              | validate token
  |                                              | check allowed_sessions
  |                                              | register adapter + channel
  |                                              | list backfill (seq > N)
  |<--- 200 text/event-stream ------------------|
  |<--- id:N+1  event:message data:{...} --------|  (replay)
  |<--- id:N+2  event:message data:{...} --------|  (replay)
  |<--- event:heartbeat data:{...} -------------|  (live, every 30s when idle)
  |<--- id:N+3  event:message data:{...} --------|  (live)
  |                                              |
  | (disconnect)                                |
  |                                              | deregister adapter + channel
  |                                              | binding + transcript retained
```

## Invariants (enforced by the implementation)

1. `AccountID` in any `ConversationRef` constructed server-side is always the controller-issued `client_id` derived from the validated token — never a client-asserted value.
2. Reply routing is scoped to `(session, ConversationRef)` via `DeliveryContextRecord`. No session-level catch-all routing.
3. The subscriber channel has a fixed buffer (default 64). When full, the **oldest** event is dropped (non-blocking write). Drops are emitted as events on the internal event bus; the next SSE `message` event carries a `dropped_count` field when drops occurred.
4. The SSE handler goroutine exits within 5 s of the HTTP connection closing. It must select on `ctx.Done()`.
5. No role name appears in Go source code. The allowed-sessions configuration is a set of session names supplied by the operator, resolved at connect time from the token bead's `allowed_sessions` field.
