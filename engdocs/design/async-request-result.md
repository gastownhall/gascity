# Async Request/Result Pattern

Every long-running API mutation follows the same async pattern.
The handler returns 202 immediately; typed completion events on
`/v0/events/stream` signal success or failure. Each operation has
its own success event type with a fully typed payload matching the
old synchronous response. A single shared `request.failed` event
covers all failure cases.

## Why async

Clients can make async calls on their end regardless. The real value
of the server-side async pattern is the **event stream as a progress
channel**. Today it delivers terminal success/failure events. In the
future, `request.progress.*` events will report intermediate steps
as long operations proceed (city init phases, session startup stages,
etc.), giving users real-time visibility into what's happening. The
per-operation event type namespace is designed to support this
progression.

## The request_id

1. A `request_id` is **opaque**. Clients must not parse it. It is not a
   session ID or a resource identifier; it lets a client match a 202
   response to the request's terminal event on the SSE stream.
2. When the server recorded the request as an operation (the
   `ops.content` rollout gate is `auto` or `require` and the operation
   journal could record it; `engdocs/design/des-o0-api-shadow-ops.md`),
   the `request_id` is the op id and equals `op.id` in the 202 body and
   in the terminal event. Otherwise it is a fresh correlation id from
   `newRequestID()` (crypto/rand hex with a `req-` prefix).
3. Retries that reuse the same `Idempotency-Key` against the same session
   get the same `request_id`, whichever endpoint (`/submit`, `/messages`)
   or target spelling (alias, id) they use, when the server records both
   the request and the retry (rule 2). Otherwise a retry gets the same
   `request_id` only when the API's per-city `Idempotency-Key` cache
   answers it, which it does on the same endpoint and target spelling, for
   at most 30 minutes, and not across an API or city restart. Any other
   retry is a new request with its own `request_id`, so the message can be
   delivered twice.
4. A request that finishes has **at least one** terminal event matched by
   `payload.request_id`: the executor emits it, and a replay of a finished
   request (`replayed: true` in the 202) re-emits it after the replay's
   `event_cursor`. All terminal events for one `request_id` carry the same
   outcome class, except for an op whose close kept failing (rule 5). Its
   executor's event went out with the op still open, and the op can then
   close with another outcome: failed `deadline_in_flight` when the
   sweeper, or a same-key retry, closes it after its `finish_by`, or the
   outcome of a same-key retry that adopted it before its `finish_by`, in
   another API process, and ran it again. A later replay reports that
   outcome, for example `request.failed` with `timeout` for a message that
   was delivered, so a replayed failure does not prove the message did not
   land. A request whose executor crashed after the 202 has none until it
   is retried. In `auto` the executor re-resolves its target (DES-O0API
   §6.1), so when an alias moved to another session after the 202, the
   executor's event names the session that got the message, while the op
   and a replay's event name the one the alias named at acceptance;
   `require` delivers to the op's session.
5. Results are projections of the op: the executor closes the op, then
   emits the terminal event. When the close keeps failing, the event goes
   out at once, in every `ops.content` mode, and carries the op still
   open: the close retries in the background, holding the in-flight entry
   (rule 6), until it lands, the op's `finish_by` passes (the sweeper, or
   a same-key retry, closes it then) or the process shuts down.
6. When the server records both requests (rule 2), a same-key retry on
   the **same** endpoint while the first request is still running gets
   the first request's 202 (same `request_id`, same `event_cursor`). A
   same-key retry on the **other** endpoint while it runs gets 409
   `in_flight`: the running request emits its terminal event for its own
   operation only, so the other endpoint's waiter would never match it.
   Once the request has finished, a retry the `Idempotency-Key` cache
   answers (rule 3) gets the 202 the cache holds for that endpoint and
   spelling again, whose terminal event is after its cursor: the first
   request's, or a replay's (`replayed: true`, with the replay's own
   `event_cursor`) when the first retry on that endpoint and spelling
   came after the request finished; any other retry gets the stored
   outcome and a terminal event for its own operation. When either
   request is unrecorded, a retry the cache answers gets the first
   request's 202 (or rule 9's 409 `in_flight`), whether that request is
   still running or has finished, and any other retry is a new request
   (rule 3).
   A same-key retry answered from the first request, by the cache, the
   in-flight registry or the stored op, applies none of its own options.
   The stored op's digest covers the session, the message and the
   intent, not `resume` (DES-O0API §3.1), so a retry that adds
   `resume: true` to a message that queued for a session that was not
   running gets that outcome, and the session is not started. A replay's
   re-emitted terminal event says so in its envelope `message` (and, for
   a failure, its `error_message`). To start the session, wake it
   (`POST /v0/city/{cityName}/session/{id}/wake`); to deliver the message
   again, use a new key. The cache refuses a retry whose body differs
   from the one it holds for that endpoint and spelling with 422
   `idempotency_mismatch`, as it compares whole request bodies.
7. A replay's `error_code` is derived from the stored outcome, which
   keeps the failure's class but not the step that failed, so it can
   differ from the original's: a session closed or gone during the
   delivery (originally `submit_failed` or `message_failed`) replays as
   `resolve_failed`, and a deadline or cancellation inside `/submit`
   (originally `submit_failed`, or `timeout` when a recorded `/submit`'s
   own delivery timeout caused it; see the client contract) replays as
   `timeout`. The original `error_message` is not kept either: a replay's
   says which op it replays and its outcome, and that the retry's own
   options took no effect (rule 6).
8. A retry the `Idempotency-Key` cache misses still passes the front-door
   checks first: an unknown target is 404 and a session waiting on a
   pending interaction (for example an approval the first message raised)
   is 409, before the in-flight registry or the stored op is consulted.
   Once the target is deliverable again, such a retry is answered as rule
   6 says. A retry the cache answers skips those checks: it gets the first
   request's 202 (or rule 9's 409 `in_flight`) whatever the target's state
   now, because the cache sits in front of the handler in every
   `ops.content` mode (DES-O0API D7). It answers as it did before the
   server recorded operations but for that 409, which it gives in every
   mode, `off` included, while the first request's terminal event is
   kept (rule 9).
9. The in-flight registry that answers rule 6 is one per city per API
   process, not one per per-city `Server`: the supervisor builds a new
   `Server` when a city's state changes while the old one may still be
   delivering, and a retry must find that delivery instead of re-running
   it. Such a delivery reports and closes through the city's live
   `Server`, the one the supervisor serves the city with when the
   delivery finishes (its own `Server` when the supervisor dropped the
   city): its terminal event goes to that `Server`'s event log, where a
   client of the new `Server` reads it, and a failing op close retries
   through that `Server`'s store. Until the op closes, its in-flight entry
   stays held, at most until `finish_by`: the message may already have
   landed, so a same-key retry in that process gets the first request's
   202 (or 409 `in_flight` on the other endpoint) rather than adopting
   the op and delivering it again. After `finish_by` the sweeper closes
   the op failed `deadline_in_flight`, and a same-key retry that finds it
   still open closes it so and replays it (rule 4): no retry adopts an op
   past its `finish_by`. A recorded delivery's own timeout ends inside its
   op's `finish_by` (client contract), so the op closes from the
   delivery's result, or from that timeout, first. The margin is fixed,
   five minutes for `/submit` and eleven for `/messages`, and the store
   I/O around the delivery spends it first: on a store that slow, the
   sweeper can close the op `deadline_in_flight` before a late success
   lands, and that success's event then carries the op so closed. The
   supervisor builds a restarted city's new `Server` only when a request
   for the city arrives, so a delivery can finish after the restart closed
   the old event log and before any new `Server` is live. In every
   `ops.content` mode, `off` included, a `/submit` or `/messages` terminal
   event goes to the city's live `Server`'s event log (its own `Server`'s when the supervisor dropped the city), whether
   or not its request was recorded, and one the log refuses is kept, in
   order, and recorded the next time a log takes it: when the
   supervisor builds the city's next `Server`, before the city's next
   `/submit` or `/messages` terminal event, and before a same-key retry
   of its request is answered, whether the `Idempotency-Key` cache, the
   in-flight registry or the stored op answers it. While it cannot be
   recorded, that retry gets 409 `in_flight` instead of a 202, and so
   does a replay whose re-emitted event the log refuses, so no retry is
   answered 202 while its request's terminal event is kept. Nothing else
   records it: a client that only watches the event stream, or whose 202
   came before the log refused the event, does not see it until one of
   those happens. A city keeps at most 1024 such events; past that it
   drops the oldest and logs it, and a dropped event is never recorded,
   so a client holding that request's 202 may never see its terminal
   event. Kept events live only in the API process: any it cannot record
   by shutdown are logged and lost. A replay that gets the log to take a
   kept event of its request with the same `type` for its own endpoint
   re-emits no other: that one is after its `event_cursor`. A city's
   terminal events are recorded one at a time, in every `ops.content`
   mode, `off` included: while its event log is slow to take one (an exec
   provider runs its script once per event), the city's other terminal
   events, and the answers to same-key retries, wait for it.
   An `ops.recording_degraded` event the log refuses is not kept; its
   class stays unlatched, so the class's next failure raises it again.

## The pattern

```
Client                        Handler                    Internal OM
  |                             |                            |
  |--- POST mutation ---------->|                            |
  |                             | validate (sync, fast)      |
  |                             | generate request_id        |
  |                             | capture event_cursor       |
  |<-- 202 { request_id,        |                            |
  |          event_cursor } ----|                            |
  |                             | go func() {               |
  |                             |   result, err := om(...)  ->| (unchanged sync OM)
  |                             |   emit typed event         |
  |                             | }()                        |
  |                             |                            |
  |--- GET /v0/events/stream -->|                            |
  |<-- request.result.session.create { request_id, session } |
```

## How it works

Take any synchronous POST handler. The old implementation looked
like this:

```go
func handler(ctx, input) (*Output, error) {
    // validate
    // call OM synchronously (may block 30-90s)
    result, err := om.DoWork(ctx, ...)
    if err != nil { return nil, mapError(err) }
    // build response with resource data
    return &Output{Body: result}, nil
}
```

The async version wraps the OM call in a goroutine:

```go
func handler(ctx, input) (*Output, error) {
    // validate (same as before — sync, fast)
    reqID := newRequestID()
    // Capture the cursor before starting the goroutine. A cursor read
    // afterwards may already be past a fast terminal event, and a
    // client resuming from it would miss the result.
    eventCursor := currentCityEventCursor()
    go func() {
        defer recoverAndEmitFailure(reqID, "session.create")
        // EXACT SAME OM call, unchanged
        result, err := om.DoWork(context.Background(), ...)
        if err != nil {
            emitRequestFailed(reqID, "session.create", "create_failed", err)
        } else {
            emitSessionCreateSucceeded(reqID, sessionToResponse(result))
        }
    }()
    return &Output{Body: {
        Status:      "accepted",
        RequestID:   reqID,
        EventCursor: eventCursor,
    }}, nil
}
```

The OM call is identical. The only change is WHERE it runs (in a
goroutine instead of inline) and WHAT the handler returns (the
request_id and event cursor instead of domain data). The completion
event carries the full typed response the old sync handler returned.

## The 202 response

The 202 response body carries the correlation `request_id` and an
`event_cursor` captured before the request was accepted. Pass it as
`after_seq` on the city stream, or `after_cursor` on the supervisor
stream, to receive the result without replaying unrelated backlog.
City-scoped session and rig-create 202s also return
`status: "accepted"`; the supervisor's city create and unregister
202s do not:

```json
{ "status": "accepted", "request_id": "req-a1b2c3d4e5f6a1b2c3d4e5f6", "event_cursor": "1042" }
```

A cursor of `"0"` means different things on the two streams; see the
`event_cursor` descriptions on the `AsyncAcceptedBody` (city) and
`AsyncAcceptedResponse` (supervisor) schemas in the OpenAPI spec.

A session `/submit` or `/messages` 202 the server recorded as an
operation also carries `idempotency_key` (the client's key, or one the
server minted), `op` (the operation), and, on a retry of a request that
already finished, `replayed: true` with the stored outcome in
`op.outcome`. A client that sees `replayed: true` and `op.status:
"closed"` has its answer and need not wait on the stream. Nothing ran
for the retry, so neither did its own options: a `resume: true` on it
did not start the session (request_id rule 6).

No resource IDs. No session data. No domain fields. The resource
does not exist yet. Returning an ID for it invites the client to
use it before it's ready, causing errors like "session not found"
or "state creating does not accept command suspend."

The client gets the full typed result from the success event
AFTER the operation completes.

## Event types

5 typed success event types + 1 shared failure type. The event
type encodes both the operation and the outcome — no string
discriminator fields needed on success payloads.

Clients discriminate result payloads by the event envelope's `type`
field, not by payload shape. This matters because some success
payloads intentionally have the same fields. For example,
`request.result.city.create` and `request.result.city.unregister`
both carry `{request_id, name, path}`; the envelope type is what
makes them distinct.

| Event type | Payload |
|------------|---------|
| `request.result.city.create` | `CityCreateSucceededPayload` |
| `request.result.city.unregister` | `CityUnregisterSucceededPayload` |
| `request.result.session.create` | `SessionCreateSucceededPayload` |
| `request.result.session.message` | `SessionMessageSucceededPayload` |
| `request.result.session.submit` | `SessionSubmitSucceededPayload` |
| `request.failed` | `RequestFailedPayload` |

### Success payloads

Each success payload carries `request_id` for correlation plus the
full typed data the old synchronous handler returned. No optional
fields — every field is always present.

**City create:**
```json
{
  "type": "request.result.city.create",
  "payload": {
    "request_id": "req-...",
    "name": "my-city",
    "path": "/home/user/cities/my-city"
  }
}
```

**City unregister:**
```json
{
  "type": "request.result.city.unregister",
  "payload": {
    "request_id": "req-...",
    "name": "my-city",
    "path": "/home/user/cities/my-city"
  }
}
```

**Session create:**
```json
{
  "type": "request.result.session.create",
  "payload": {
    "request_id": "req-...",
    "session": {
      "id": "gc-42",
      "kind": "agent",
      "template": "worker",
      "state": "active",
      "title": "My Session",
      "provider": "claude",
      "..."
    }
  }
}
```

**Session message:**
```json
{
  "type": "request.result.session.message",
  "payload": {
    "request_id": "req-...",
    "session_id": "gc-42"
  }
}
```

**Session submit:**
```json
{
  "type": "request.result.session.submit",
  "payload": {
    "request_id": "req-...",
    "session_id": "gc-42",
    "queued": false,
    "intent": "default"
  }
}
```

### Failure payload

One shared type for all operation failures. The `operation` field
is an enum identifying which operation failed.

```json
{
  "type": "request.failed",
  "payload": {
    "request_id": "req-...",
    "operation": "session.create",
    "error_code": "create_failed",
    "error_message": "provider startup failed"
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `request_id` | string | Correlation number from the 202 response |
| `operation` | enum | `city.create`, `city.unregister`, `session.create`, `session.message`, `session.submit` |
| `error_code` | string | Machine-readable error code |
| `error_message` | string | Human-readable description |
| `session_id` | string | Optional, `session.create` only: the session the failed create had already written, when the server could identify it. Absence does not prove that no session was written |
| `op` | object | Optional: the operation a recorded session message was recorded as (`id`, `conversation`, `kind`, `status`, `attempt`, `outcome`) |

## Operations using this pattern

| Endpoint | Success event type |
|----------|--------------------|
| `POST /v0/city` | `request.result.city.create` |
| `POST /v0/city/{name}/unregister` | `request.result.city.unregister` |
| `POST /v0/city/{city}/sessions` (all kinds) | `request.result.session.create` |
| `POST /v0/city/{city}/session/{id}/messages` | `request.result.session.message` |
| `POST /v0/city/{city}/session/{id}/submit` | `request.result.session.submit` |

## Future: progress events

The event type namespace is designed to grow. Future
`request.progress.*` events will report intermediate steps during
long operations:

```
request.progress.city.create      — city init phase completion
request.progress.session.create   — session startup stages
```

These will carry typed payloads describing the current phase,
giving clients real-time visibility into operation progress.

## Client contract

1. Subscribe to the event stream that carries the operation's terminal
   event:
   - city create/unregister: `/v0/events/stream`
   - session create/message/submit: `/v0/city/{city}/events/stream`
2. Send the mutation POST.
3. Parse the 202 response; extract `request_id` and `event_cursor`.
   A client that subscribes after the POST resumes from that cursor
   (`after_seq` on the city stream, `after_cursor` on the supervisor
   stream), so it still receives a result emitted before it subscribed.
4. Wait for an event where the envelope `type` is the expected
   success type or `request.failed`, and `payload.request_id`
   matches:
   - `request.result.*` for success (typed per operation)
   - `request.failed` for failure (shared, `operation` enum)
5. On success, the payload contains the full typed result.

`session.message` uses the same four-minute timeout on both sides of
the API adapter: the server emits `request.failed` with
`error_code=timeout` at the same boundary the CLI client waits for on
the SSE stream. If the provider path ignores cancellation and returns
after that timeout, the API logs a late `session.message` result with
the request ID instead of emitting a second terminal event.

A recorded `session.submit` (rule 2) is bounded at ten minutes, inside
its op's `finish_by`: past that, the server closes the op failed
`deadline_in_flight`, emits `request.failed` with `error_code=timeout`,
and logs a late result instead of emitting a second terminal event. An
unrecorded `session.submit` is not bounded: its terminal event comes when
the submit returns. The CLI waits four minutes for either, as for
`session.message`.
6. On failure, `error_code` + `error_message` describe the problem.
7. Do NOT use the resource before the success event arrives.

## Implementation rules

1. **For ordinary async handlers, the goroutine runs the EXACT SAME OM code the old sync
   handler ran.** No changes to the OM. No new queuing mechanisms.
   Take the old synchronous handler body, put it in
   `go func() { ... }()`, emit the typed event.

2. **The request_id is opaque.** Generated by the handler (or, for a
   recorded session message, the op id), returned in the 202, included
   in the event payload. The OM has no knowledge of it, and it never
   reaches delivered text.

3. **Use `context.Background()` in the goroutine.** The HTTP
   request context is cancelled when the 202 is sent.

4. **Every async-request route's 202 contains no resource IDs or
   domain data.** Only the `request_id`, the event cursor, and (for
   city-scoped session and rig-create requests) `status`. The
   synchronous `respond` 202, which returns the session `id`, is
   outside this pattern.

5. **Every goroutine has panic recovery.** Panics emit
   `request.failed` with `error_code: "internal_error"`. The
   goroutine must never silently complete without emitting.

6. **Success events carry the full typed response.** The same data
   the old synchronous handler returned, built from the same OM
   result, using the same response-building functions (e.g.,
   `sessionToResponse`).

7. **City terminal request events are supervisor-visible.**
   City create/unregister completion is reported as
   `request.result.city.*` or `request.failed` on the supervisor
   event stream because the city may not exist yet during create
   and may be going away during unregister.

   City create/unregister are the exception to the ordinary
   goroutine-wrapper implementation rule. The handler accepts the
   request, records durable `request_id` correlation for the city
   path, and the supervisor reconciler emits the terminal request
   event when infrastructure startup or teardown actually completes.
   That exception does not weaken the client contract: every
   successful 202 still has exactly one terminal request event matched
   by `payload.request_id`.

   Non-terminal lifecycle events such as `city.created` and
   `city.unregister_requested` are diagnostic progress markers.
   They do not replace the terminal request result events and
   clients should not treat them as completion.

8. **Session events go to the city event provider.**
   The city exists when session operations run.
