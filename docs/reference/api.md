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

Fatal setup errors are returned as normal Problem Details responses
*before* the stream's 200 headers commit, never as a 200 stream that
closes immediately. For example, `GET /v0/events/stream` returns
`503 application/problem+json` with `detail: "no_providers: ..."`
when no running city has an event provider registered.

## Event Contract

The event APIs, the SSE streams, and `gc events` are the same contract
at three different presentation layers. The API is the source of
truth.

For the explicit CLI output contract, including JSONL framing, empty-output
behavior, heartbeat suppression, and the `--seq` scalar format, see
[gc events Formats](/reference/events).

### City Scope

- `GET /v0/city/{cityName}/events`
  returns `ListBody<Event>` and includes `X-GC-Index`.
- `GET /v0/city/{cityName}/events/stream`
  emits:
  - `event: event` with `EventStreamEnvelope`
  - `event: heartbeat` with `HeartbeatEvent`
- Resume:
  - `Last-Event-ID` or `after_seq`
- `gc events` in city scope outputs one `Event` JSON object per line.
- `gc events --watch` and `gc events --follow` in city scope output one
  `EventStreamEnvelope` JSON object per line.
- `gc events --seq` in city scope prints the API's `X-GC-Index` value.

### Supervisor Scope

- `GET /v0/events`
  returns `{ items: TaggedEvent[], total }`.
- `GET /v0/events/stream`
  emits:
  - `event: tagged_event` with `TaggedEventStreamEnvelope`
  - `event: heartbeat` with `HeartbeatEvent`
- Resume:
  - `Last-Event-ID` or `after_cursor`
- `gc events` in supervisor scope outputs one `TaggedEvent` JSON object
  per line.
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
