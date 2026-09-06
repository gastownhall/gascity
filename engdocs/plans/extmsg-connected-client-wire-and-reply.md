# Plan: extmsg connected-client wire contract and reply command

> PM owner: `gascity/pm`
> Sources: `ga-1y4mb7`, `ga-a3z0hg`
> Origin: designer handoff from `gascity/designer`, 2026-06-19

## Goal

Turn the completed connected-client SSE wire contract and generic
`gc extmsg reply` command specs into implementation-ready work. The slice
must let an external client subscribe to a durable reply stream, send an
inbound turn, and receive a session reply through the provider-neutral extmsg
reply path.

## Context

Both source beads came from the designer after architecture bead `ga-31gfwg`.
Because they are labeled `source:actual-designer`, no work routes back to
design.

The SSE wire contract (`ga-1y4mb7`) has an explicit NFR-6 gate: architect
review is required before builder implementation. The target contract path is
`engdocs/design/extmsg-connected-client-wire-contract.md`. Architect should
reconcile it with the already completed
`engdocs/design/extmsg-connected-client-subscribe-contract.md`.

The reply command spec (`ga-a3z0hg`) defines the `gc.extmsg.origin` metadata
surface, `<external-origin>` reminder block, and provider-neutral
`gc extmsg reply` command. The command must route through the existing
`POST /v0/extmsg/outbound` / `HandleOutbound` path.

## Work Packages

| Bead | Title | Routing | Dependencies |
| --- | --- | --- | --- |
| `ga-1y4mb7.1` | Review and publish the connected-client SSE wire contract | `needs-architecture` -> `gascity/architect` | none |
| `ga-1y4mb7.2` | Add tests for the connected-client SSE wire contract | `needs-tests` -> `gascity/validator` | `ga-1y4mb7.1` |
| `ga-1y4mb7.3` | Implement the connected-client SSE subscribe wire contract | `ready-to-build` -> `gascity/builder` | `ga-1y4mb7.1`, `ga-1y4mb7.2`, `ga-a3z0hg.4` |
| `ga-a3z0hg.1` | Add tests for external-origin context and gc extmsg reply | `needs-tests` -> `gascity/validator` | none |
| `ga-a3z0hg.2` | Inject external-origin metadata into session context | `ready-to-build` -> `gascity/builder` | `ga-a3z0hg.1` |
| `ga-a3z0hg.3` | Implement the provider-neutral gc extmsg reply command | `ready-to-build` -> `gascity/builder` | `ga-a3z0hg.1`, `ga-a3z0hg.2`, `ga-a3z0hg.4` |
| `ga-a3z0hg.4` | Add end-to-end coverage for connected-client subscribe and reply | `needs-tests` -> `gascity/validator` | `ga-1y4mb7.1`, `ga-a3z0hg.1` |

## Acceptance Summary

`ga-1y4mb7.1` is complete when an architect-reviewed
`engdocs/design/extmsg-connected-client-wire-contract.md` is published,
conflicts with the subscribe-contract doc are resolved, and the builder can
treat the wire contract as authoritative for NFR-6.

`ga-1y4mb7.2` is complete when tests cover the reviewed SSE event payloads,
id rules, retryable error framing, and `Last-Event-ID` parsing, including the
non-numeric `error` sentinel.

`ga-1y4mb7.3` is complete when the subscribe endpoint emits the reviewed
headers and `message`, `heartbeat`, and `error` frames, registers subscribers
before replay, backfills transcript entries after numeric cursors, treats
non-numeric cursors as absent, and exits request goroutines cleanly.

`ga-a3z0hg.1` is complete when focused tests cover `gc extmsg reply`,
`gc.extmsg.origin`, the reminder block, stdin input, dry-run, and documented
exit codes.

`ga-a3z0hg.2` is complete when inbound extmsg dispatch writes
`gc.extmsg.origin` through a `beadmeta` constant, the reminder block appears
only when the metadata is present, and stale refs are cleared or replaced when
session context changes.

`ga-a3z0hg.3` is complete when `gc extmsg reply` resolves context or `--ref`,
uses stdin when no text argument is supplied, sends through the existing
outbound API path, and implements the documented stdout/stderr and exit codes.

`ga-a3z0hg.4` is complete when integration or testscript coverage exercises
register, subscribe, inbound, session reply, reconnect replay, and no-subscriber
behavior across the connected-client path.

## Dependency Graph

`ga-1y4mb7.1` -> `ga-1y4mb7.2` -> `ga-1y4mb7.3`

`ga-a3z0hg.1` -> `ga-a3z0hg.2` -> `ga-a3z0hg.3`

`ga-1y4mb7.1` + `ga-a3z0hg.1` -> `ga-a3z0hg.4`

`ga-a3z0hg.4` -> `ga-1y4mb7.3`

`ga-a3z0hg.4` -> `ga-a3z0hg.3`

## Out Of Scope

- UI or dashboard changes.
- New top-level docs navigation changes.
- Hand-editing generated OpenAPI artifacts outside normal Huma generation.
- Provider-specific reply commands or role-specific Go behavior.
- Replacing the existing `HandleOutbound` delivery path.

## Risks

The main risk is contract drift across two design documents, generated OpenAPI,
and implementation. The architect review bead exists to make one wire contract
authoritative before builder work starts. The second risk is over-coupling the
reply command to the connected-client provider; the builder acceptance requires
provider values to come from `ConversationRef` data rather than hardcoded
delivery branches.
