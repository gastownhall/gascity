---
title: gc events Formats
description: Exact output formats emitted by `gc events`.
---

`gc events` is a CLI reflection of the supervisor event APIs. The API is the
source of truth, but this page documents the CLI output contract explicitly so
users can consume `gc events` without reverse-engineering it from the OpenAPI
document.

## Source of Truth

These CLI formats are projections of the supervisor API and SSE contract:

- City list API: `GET /v0/city/{cityName}/events`
- City SSE API: `GET /v0/city/{cityName}/events/stream`
- Supervisor list API: `GET /v0/events`
- Supervisor SSE API: `GET /v0/events/stream`

The underlying DTOs come from the published OpenAPI document:

- `Event`
- `TaggedEvent`
- `EventStreamEnvelope`
- `TaggedEventStreamEnvelope`
- `HeartbeatEvent`

Download the canonical schema from [Schemas](/schema), or read the broader
event-bus notes in the [Supervisor REST API](/reference/api).

## Output Modes

`gc events` has two output families:

- List mode: `gc events`
- Stream mode: `gc events --watch` and `gc events --follow`

There is one exception:

- Cursor mode: `gc events --seq`

### List Mode

`gc events` writes **JSON Lines** to stdout.

- Each line is exactly one JSON object.
- There is no outer array or wrapper object.
- If nothing matches, stdout is empty.

#### City Scope

When a city is in scope, each output line is one `Event` object.

Example:

```json
{"actor":"human","message":"hello","seq":21,"subject":"mayor","ts":"2026-04-17T15:20:52.136314-07:00","type":"mail.sent"}
```

#### Supervisor Scope

When no city is in scope and the supervisor API is being used, each output line
is one `TaggedEvent` object.

Example:

```json
{"actor":"human","city":"mc-city","message":"hello","seq":21,"subject":"mayor","ts":"2026-04-17T15:20:52.136314-07:00","type":"mail.sent"}
```

The supervisor form adds `city` because the merged event bus spans multiple
cities.

### Stream Mode

`gc events --watch` and `gc events --follow` also write **JSON Lines** to
stdout, but the line schema is different from list mode.

- Each line is exactly one SSE event envelope serialized as JSON.
- The CLI only emits matching event envelopes.
- Heartbeat SSE frames are consumed internally and are **not** written to
  stdout.
- If `--watch` times out without a match, stdout is empty and the command exits
  successfully.

#### City Scope

Each line is one `EventStreamEnvelope` object, matching the API's
`event: event` SSE payload.

Example:

```json
{"actor":"human","message":"hello","seq":21,"subject":"mayor","ts":"2026-04-17T15:20:52.136314-07:00","type":"mail.sent"}
```

#### Supervisor Scope

Each line is one `TaggedEventStreamEnvelope` object, matching the API's
`event: tagged_event` SSE payload.

Example:

```json
{"actor":"human","city":"mc-city","message":"hello","seq":21,"subject":"mayor","ts":"2026-04-17T15:20:52.136314-07:00","type":"mail.sent"}
```

### Cursor Mode

`gc events --seq` does **not** emit JSONL. It prints a single plain-text cursor
to stdout.

#### City Scope

The value is the current `X-GC-Index` head for that city's event log.

Example:

```text
21
```

#### Supervisor Scope

The value is the composite supervisor cursor used by `--after-cursor`.

Example:

```text
alpha:4,beta:9,mc-city:21
```

## Filtering and Shape

The following flags only filter which objects are emitted. They do not change
the JSON shape:

- `--type`
- `--since`
- `--payload-match`
- `--after`
- `--after-cursor`

The same rule applies to both list mode and stream mode.

## Transport vs Semantic Type

For stream mode, keep these separate:

- The SSE `event:` value is the transport envelope name:
  `event`, `tagged_event`, or `heartbeat`.
- The JSON object's `type` field is the semantic event type:
  `bead.created`, `mail.sent`, `session.woke`, and so on.

`gc events` outputs the JSON payloads and envelopes, not the raw SSE frame text.

## Errors

Successful event queries write only data to stdout.

Operational failures are written to stderr as human-readable text and return a
non-zero exit status. Examples include:

- API discovery failure
- invalid flag combinations such as `--after` with `--after-cursor`
- stream setup failures returned by the API as Problem Details
- malformed or undecodable stream payloads

## Stability Contract

The CLI does not define an independent event schema. Its stability contract is:

- the published supervisor OpenAPI schemas for `Event`, `TaggedEvent`,
  `EventStreamEnvelope`, and `TaggedEventStreamEnvelope`
- the explicit CLI framing rules on this page:
  JSONL for list and stream modes, plain text for `--seq`, empty stdout for
  no-match list queries, and heartbeat suppression in stream mode
