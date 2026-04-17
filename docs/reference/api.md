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
- **Event bus.** `GET /v0/events` (append-only poll) and
  `GET /v0/events/stream` (SSE).
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

## Versioning

The API is versioned by URL prefix (`/v0`). Breaking changes ship as
a new prefix; the current spec is the authoritative contract for
`v0`.
