---
title: Supervisor REST API
description: The typed HTTP + SSE control plane exposed by the `gc` supervisor.
---

The `gc` supervisor exposes a single, typed HTTP control plane
described by an OpenAPI 3.1 document. Everything the CLI does, any
third-party client can do too — there is no hidden surface.

## Download the spec

- **<a href="/schema/openapi.txt" download="openapi.json">Download openapi.json</a>** —
  the authoritative contract. Drop it into Stoplight, Postman,
  Swagger UI, or any OpenAPI-aware tool to browse operations
  interactively.
- **<a href="/schema/events.txt" download="events.json">Download events.json</a>** —
  the `gc events` JSONL line schema. It references DTO components in
  `openapi.json`, so the API remains the source of truth.

## Endpoint families

The spec is the full reference. A brief summary of the surfaces:

- **Cities.** `GET /v0/cities`, `POST /v0/city`,
  `GET /v0/city/{cityName}`, `GET /v0/city/{cityName}/status`,
  `GET /v0/city/{cityName}/readiness`,
  `POST /v0/city/{cityName}/stop`.
- **Health & readiness.** `GET /health`, `GET /v0/readiness`,
  `GET /v0/provider-readiness`.
- **Agents.** `GET/POST/DELETE` under `/v0/city/{cityName}/agents`
  plus SSE `/v0/city/{cityName}/agents/{agent}/output/stream`.
- **Beads (work units).** CRUD under `/v0/city/{cityName}/beads`,
  query + hook operations, dependencies, labels.
- **Sessions.** CRUD under `/v0/city/{cityName}/sessions`, submit,
  prompt, resume, interaction response, transcript, SSE stream.
- **Mail, convoys, orders, formulas, molecules, participants,
  transcripts, adapters.** External messaging and orchestration
  surfaces; see the spec for per-operation shapes.
- **Event bus.** `GET /v0/events` + `GET /v0/events/stream` at
  supervisor scope, and `GET /v0/city/{cityName}/events` +
  `GET /v0/city/{cityName}/events/stream` at city scope.
- **Config & packs.** Per-city config and pack metadata under
  `/v0/city/{cityName}/config` and `/v0/city/{cityName}/packs`.

## Request and response headers

Every operation's header contract appears in the OpenAPI spec — if a
request header is required or a response header is promised, the
spec describes it. The two cross-cutting headers every API client
should know about:

- **`X-GC-Request`** (request header, required on all mutations).
  Anti-CSRF token required on every POST, PUT, PATCH, and DELETE.
  Any non-empty value is accepted; the header's presence is what
  the server checks. Requests without it are rejected with
  `403 csrf: X-GC-Request header required on mutation endpoints`.
  Leveraging the same-origin policy, a cross-origin attacker
  cannot set this header on a forged request. The generated Go
  and TypeScript clients set this header automatically; only raw
  HTTP clients need to remember it.
- **`X-GC-Request-Id`** (response header, every response).
  Opaque per-response identifier the server assigns for log
  correlation. Every response — success or error — carries this
  header; the spec declares it via a `$ref` to
  `components.headers.X-GC-Request-Id`. Include its value in bug
  reports so the server's logs can be traced.

SSE stream operations emit additional runtime-status headers before
the first event frame:

- **`stream-agent-output` / `stream-agent-output-qualified`**:
  `GC-Agent-Status` — set to `stopped` when the agent is not
  running and the stream is replaying transcript from the session
  log instead of live output.
- **`stream-session`**: `GC-Session-State` (e.g. `active`,
  `closed`) and `GC-Session-Status` (`stopped` when the session's
  underlying process is not running).

Each header's schema is documented in the operation's
`responses.200.headers` in the spec.

## Errors

Every error response is an RFC 9457 Problem Details body
(`application/problem+json`). Error types are documented in the spec
under `components.schemas.ErrorModel`. The `detail` field carries a
short `code: ` prefix (e.g. `pending_interaction: ...`,
`conflict: ...`, `not_found: ...`, `read_only: ...`) so clients can
pattern-match on the semantic code without needing a typed error
enum. Body-field validation errors (e.g. a required string posted
empty) come back as `422 Unprocessable Entity` or `400 Bad Request`
depending on the operation; the `errors` array of the Problem Details
body pinpoints which fields failed.

## Streaming

SSE endpoints set `Content-Type: text/event-stream` and emit typed
`event:` frames. The spec describes each event's payload schema under
the per-operation `responses.200.content.text/event-stream` entry.
Clients should follow the standard SSE reconnection protocol
(`Last-Event-ID` header) where the server supports it; the event bus
stream (`/v0/events/stream`) replays from the last received index.
When no cursor is supplied, event streams start at the current event
head and deliver future events only. Async `202 Accepted` responses
include an `event_cursor` captured before the operation starts; pass
that value as `after_cursor` or `after_seq` to wait for the operation's
request-result event without replaying unrelated historical backlog.

Fatal setup errors are returned as normal Problem Details responses
*before* the stream's 200 headers commit, never as a 200 stream that
closes immediately. For example, `GET /v0/events/stream` returns
`503 application/problem+json` with `detail: "no_providers: ..."`
when no running city has an event provider registered.

## Creating a city (asynchronous)

`POST /v0/city` is an **asynchronous** operation. The response is
`202 Accepted` returned as soon as the city has been scaffolded on
disk and registered with the supervisor. The slow finalize work
(pack materialization, bead store startup, formula resolution,
agent validation) runs on the supervisor reconciler's next tick.
Clients observe completion via the supervisor event stream — there
is nothing to poll.

### Response

```json
{
  "request_id": "req-...",
  "event_cursor": "__supervisor__:42,my-city:17"
}
```

Use `request_id` to correlate the completion event. Use `event_cursor`
as the `after_cursor` value on the supervisor event stream.

### Completion events

On the same `/v0/events/stream` the client will see:

- `city.created` (`CityLifecyclePayload`) — emitted by the scaffold
  step before `POST` returns. `subject` and payload `name` equal
  the resolved city name.
- `request.result.city.create` (`CityCreateSucceededPayload`) — the
  reconciler finished `prepareCityForSupervisor` successfully.
- `request.failed` (`RequestFailedPayload`) — the reconciler failed
  the async operation. Match `payload.request_id` to the 202 response.

Exactly one terminal event (`request.result.city.create` or
`request.failed`) lands per successful `POST`. Clients wait for the
returned `request_id`; no polling of `GET /v0/cities` or
`GET /v0/city/{cityName}/readiness` is required.

### Subscribe before or after POST

Either order works. The recommended flow is:

1. `POST /v0/city` and wait for `202 {request_id, event_cursor}`.
2. `GET /v0/events/stream?after_cursor=<event_cursor>`.
3. Read frames until `payload.request_id == response.request_id` and
   `type ∈ {"request.result.city.create", "request.failed"}`.

**Empty supervisor is fine.** The event stream works even when
no cities existed before the `POST`. `POST` writes the city to
the supervisor registry (`cities.toml`) and creates
`.gc/events.jsonl` synchronously before returning 202, so the
event multiplexer finds the new city on the very next
`buildMultiplexer` call. Subscribers do **not** need to retry on
`503 no_providers`; if that error surfaces after a successful
202, it's a bug.

### Errors

- `409 conflict: city already initialized at <path>` — the target
  directory already has a scaffolded city.
- `422` — invalid provider, invalid bootstrap profile, or other
  body-validation failure.
- `503` — a hard dependency is missing on the host, or a provider
  the city needs is not ready.
- `500` — unexpected scaffold failure; consult the server logs
  via the `X-GC-Request-Id` correlation header.

## Unregistering a city (asynchronous)

`POST /v0/city/{cityName}/unregister` removes a city from the
supervisor's registry and signals the supervisor to stop the city's
controller. Like `POST /v0/city`, it is asynchronous: the response
is `202 Accepted` returned as soon as the registry entry is gone
and the supervisor is notified. The supervisor reconciler stops the
controller on its next tick and emits the completion event.

The city directory on disk is **not** touched. This operation only
detaches the city from the supervisor; reattaching it later is a
simple `gc register`.

### Response

```json
{
  "request_id": "req-...",
  "event_cursor": "__supervisor__:43,my-city:21"
}
```

Pass `event_cursor` as `after_cursor` on `/v0/events/stream` and wait
for the terminal event whose payload contains the returned `request_id`.

### Completion events

On `/v0/events/stream` the client will see (in order):

- `city.unregister_requested`
  (`CityLifecyclePayload`) — emitted by the handler
  before the registry write so subscribers see the teardown start.
- `request.result.city.unregister`
  (`CityUnregisterSucceededPayload`) — emitted by the reconciler once
  the city's controller has stopped.
- `request.failed` (`RequestFailedPayload`) — emitted by the
  reconciler if the controller did not stop cleanly. Match
  `payload.request_id`.

Exactly one terminal event lands per successful unregister. Clients
wait for the returned `request_id`.

### Errors

- `404 not_found: city not registered with supervisor: <name>` — no
  entry in the registry for that name.
- `501` — supervisor has no Initializer wired (test-only configs).
- `500` — unexpected registry write failure.

## Event Contract

The event APIs, the SSE streams, and `gc events` are the same contract
at three different presentation layers. The API is the source of
truth.

For the explicit CLI output contract, including JSONL framing, empty-output
behavior, heartbeat suppression, and the `--seq` plain-text cursor format, see
[gc events Formats](/reference/events).

### City Scope

Per-city routes are available only after the supervisor marks the city
`running=true` in `GET /v0/cities`. During startup reconciliation, a city can
appear in the city list with `running=false` and `status=starting_agents`; in
that window typed `/v0/city/{cityName}/...` routes return `404` with
`not_found: city not found or not running: <cityName>`. The raw
`/v0/city/{cityName}/svc/*` workspace-service proxy is outside the Huma-typed
API surface and returns the static readiness detail
`not_found: city not found or not running`. Clients should use the supervisor
city list or lifecycle events as the readiness boundary before issuing per-city
requests.

- `GET /v0/city/{cityName}/events`
  returns `ListBodyWireEvent` and includes `X-GC-Index`.
- `GET /v0/city/{cityName}/events/stream`
  emits:
  - `event: event` with `EventStreamEnvelope`
  - `event: heartbeat` with `HeartbeatEvent`
- Async session mutations in that city (`session.create`,
  `session.message`, `session.submit`) complete on this stream. Match
  terminal `request.result.session.*` or `request.failed` events by
  `payload.request_id`.
- Resume:
  - `Last-Event-ID` or `after_seq`; omit both to start from the
    current city event head.
- `gc events` in city scope outputs one `TypedEventStreamEnvelope` JSON
  object per line.
- `gc events --watch` and `gc events --follow` in city scope output one
  `EventStreamEnvelope` JSON object per line.
- `gc events --seq` in city scope prints the API's `X-GC-Index` value.

### Supervisor Scope

- `GET /v0/events`
  returns `SupervisorEventListOutputBody` with `WireTaggedEvent` items.
- `GET /v0/events/stream`
  emits:
  - `event: tagged_event` with `TaggedEventStreamEnvelope`
  - `event: heartbeat` with `HeartbeatEvent`
- Async supervisor mutations (`city.create`, `city.unregister`) complete
  on this stream. Match terminal `request.result.city.*` or
  `request.failed` events by `payload.request_id`.
- Resume:
  - `Last-Event-ID` or `after_cursor`; omit both to start from the
    current supervisor event head.
- `gc events` in supervisor scope outputs one `TypedTaggedEventStreamEnvelope`
  JSON object per line.
- `gc events --watch` and `gc events --follow` in supervisor scope
  output one `TaggedEventStreamEnvelope` JSON object per line.
- `gc events --seq` in supervisor scope prints the current composite
  supervisor cursor, suitable for `--after-cursor`.

### Transport vs Semantic Type

- The SSE `event:` line is the transport envelope:
  `event`, `tagged_event`, or `heartbeat`.
- The semantic event kind is the JSON payload's `type` field:
  `bead.created`, `mail.sent`, `session.woke`, and so on.
- The CLI does not define a separate event schema. It streams the same
  DTOs and envelopes as JSONL.

## Versioning

The API is versioned by URL prefix (`/v0`). Breaking changes ship as
a new prefix; the current spec is the authoritative contract for
`v0`.
