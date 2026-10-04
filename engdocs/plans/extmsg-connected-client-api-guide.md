# Plan: extmsg connected-client API guide and error catalog

> PM owner: `gascity/pm`
> Source: `ga-lyikvt`
> Origin: designer handoff from `gascity/designer`, 2026-06-19

## Goal

Turn the completed designer handoff for extmsg connected-client documentation
into implementation-ready work. The public docs should explain how an external
LLM client registers, sends an inbound turn, subscribes for replies, reconnects,
and handles the defined error catalog.

## Context

Designer resolved two open questions:

- Documentation placement is the existing docs site, not a new top-level
  integrations section: add a connected-client endpoint-family bullet to
  `docs/reference/api.md` and add a new `docs/guides/connected-clients.md`
  how-to guide in the Guides group.
- Error UX is split by stream state: setup failures return normal HTTP errors
  before SSE commitment; post-commit failures emit `event: error` frames, use
  `id: error` as a non-numeric sentinel, declare `retryable`, optionally include
  `retry_after_ms`, and then close the stream.

The current `docs/docs.json` Guides group does not contain the
`guides/using-json-from-gc` page named in the design handoff. The builder bead
therefore accepts placement after `guides/shareable-packs` unless a newer nav
shape exists at implementation time.

## Work Packages

| Bead | Title | Routing | Dependencies |
| --- | --- | --- | --- |
| `ga-lyikvt.1` | Document the connected-client SSE error contract | `needs-architecture` -> `gascity/architect` | none |
| `ga-lyikvt.2` | Add docsync coverage for connected-client docs | `needs-tests` -> `gascity/validator` | none |
| `ga-lyikvt.3` | Add connected-client API guide and reference docs | `ready-to-build` -> `gascity/builder` | `ga-lyikvt.1`, `ga-lyikvt.2` |

## Acceptance Summary

`ga-lyikvt.1` is complete when an `engdocs/design` contract document captures
the connected-client subscribe behavior: pre-stream HTTP errors, post-stream
SSE error event schema, retryable and non-retryable catalog, heartbeat events,
and `Last-Event-ID` replay semantics. The document must tie back to existing
external messaging design context and avoid role-specific assumptions.

`ga-lyikvt.2` is complete when automated docs validation fails if the connected
client guide is missing from navigation, if the API reference omits the three
connected-client endpoint surfaces, or if the guide omits the register,
subscribe, send, reconnect, error catalog, heartbeat, and configuration
sections. The intended local verification is `go test ./test/docsync`.

`ga-lyikvt.3` is complete when public docs include:

- An endpoint-family bullet in `docs/reference/api.md` for
  `POST /v0/extmsg/clients`, `POST /v0/extmsg/inbound` with
  `provider:"llm-client"`, and
  `GET /v0/extmsg/{provider}/{account_id}/{conversation_id}/subscribe`.
- A new `docs/guides/connected-clients.md` how-to covering overview,
  prerequisites, register, subscribe, send, reconnect, errors, configuration,
  and a minimal Go example.
- The designer-defined HTTP error table and SSE error catalog, including
  retryable flags, retry hints, `id: error`, and heartbeat behavior.
- A `docs/docs.json` navigation entry in the existing Guides group.

## Dependency Graph

`ga-lyikvt.1` -> `ga-lyikvt.3`

`ga-lyikvt.2` -> `ga-lyikvt.3`

## Out Of Scope

- UI or dashboard changes.
- Hand-editing generated OpenAPI artifacts.
- Implementing connected-client endpoints or SSE runtime behavior.
- Creating a new top-level docs navigation group.
- Routing the completed designer handoff back to design.

## Risks

The main risk is contract drift between public docs, generated API schema, and
the SSE implementation. The builder bead explicitly avoids hand-editing
generated schema files; if the public docs reveal OpenAPI drift, the builder
should file a follow-up instead of silently changing generated artifacts.
