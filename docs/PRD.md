# Product Requirements Document: extmsg connected-client transport (SSE streaming)

Source bead: `ga-ty8cfb`
Generated: 2026-06-19 by `gascity/planner`
Type: feature PRD

## Problem Statement

The External Messaging Fabric (`internal/extmsg`) was designed with two
assumptions: (a) inbound messages arrive from an external provider whose
webhook gc handles, and (b) outbound replies are pushed by gc to that same
provider's REST API. The `TransportAdapter` interface encodes this
push-to-provider model explicitly.

A generic external client — a local voice assistant, a CLI bot, or an
LLM-driven agent — has **no provider API for gc to push to**. It needs to
hold a connection open and *receive* the session's replies as they arrive.
The inbound leg already exists (`POST /v0/extmsg/inbound`): a client can
send turns to a session using the existing provider-neutral path. The
outbound leg — the client receiving the session's replies — does not exist.

The fabric's own design doc (`engdocs/design/external-messaging-fabric.md`)
acknowledged this gap as **Open Question #2**: "Which controller-local API
shape is best for out-of-process adapters?" This PRD answers that question
for the generic LLM client use case: SSE over the existing HTTP API,
`127.0.0.1` only.

The first consumer is a local voice assistant (tincan-iris, tracked on its
own rig). The feature must be generic enough that any future external client
uses the same three HTTP calls, with no Gas City changes.

## Goals

1. A generic external LLM client can receive a Gas City session's replies
   by holding a single SSE stream open — no polling, no push webhook needed.
2. Client identity is controller-issued: `AccountID` is derived from a
   controller-assigned token, not from anything the client asserts.
3. A client's replies are routed only to that client's stream; they cannot
   reach another client's binding or the session's existing Discord DM.
4. Streams reconnect transparently with replay: a client that drops and
   reconnects within the retention window receives any messages it missed.
5. The contract is transport-agnostic: the SSE endpoint shape and event
   schema are designed so WS or gRPC can be added later without breaking
   existing clients.
6. Gas City never imports anything client-specific. The client is
   `provider:"llm-client"` with opaque strings.

## Non-Goals

- Replacing or extending the Discord pack or any other existing adapter.
- WebSocket or gRPC transports (design for them; implement in a later PR).
- Multi-city routing (127.0.0.1 / single controller, Phase 1).
- A brokered multi-reader fan-out within a single conversation (one active
  SSE stream per conversation per client is sufficient for v1).
- Implementing the client side of the integration (tincan-iris is a separate
  rig; this PRD only covers the Gas City server side).
- Authentication beyond the local controller token (no mTLS, no OIDC for v1).

## User Stories

1. **As a local voice assistant** that wraps the mayor session, I want to
   send the user's transcribed speech to the session and receive the reply
   in real time over a held-open stream so I can speak the response without
   polling.

2. **As a CLI bot developer**, I want to open a persistent conversation with
   a Gas City session from a Go or Python script, send turns, and receive
   streamed replies — using only standard HTTP — without implementing a
   webhook listener.

3. **As a Gas City operator**, I want to configure which sessions a connected
   client may reach so a rogue or misconfigured client cannot impersonate a
   session or inject turns into an unrelated workflow.

4. **As a test harness author**, I want two isolated test clients to use the
   same dummy conversation UUID and not interfere with each other, because
   client isolation comes from the controller-issued token, not from the
   UUID I pick.

5. **As a session (e.g. mayor)**, I want a generic `gc` command that replies
   to the current external caller — without me knowing whether the caller
   is Discord, a voice assistant, or something else — so the prompt template
   author never has to write provider-specific reply commands.

## Functional Requirements

| ID | Requirement | Priority | Acceptance Criteria |
|----|-------------|----------|---------------------|
| FR-1 | **Token issuance.** A client calls `POST /v0/extmsg/clients` with a credential (operator-issued shared secret or no-auth for localhost-only mode) and receives a `client_id` + `token`. The `client_id` is the stable `AccountID` for that client's `ConversationRef`s; the token is presented on every subsequent call via a header (`Authorization: Bearer <token>` or `X-GC-Client-Token: <token>`). Issuance is idempotent for the same credential: the same credential always returns the same `client_id`. | Must | Unit tests cover: new issuance, repeat issuance returns same id, bad credential returns 401. |
| FR-2 | **Bind on first turn.** `POST /v0/extmsg/inbound` with `provider:"llm-client"`, the client's `ConversationRef`, text, and `Actor` implicitly binds the conversation to the target session if no binding exists. The `AccountID` field of the presented `ConversationRef` MUST match the `client_id` from the token; the API rejects mismatches. | Must | Integration test: inbound with mismatched AccountID returns 403; inbound with correct AccountID creates binding visible in `GET /v0/extmsg/bindings`. |
| FR-3 | **SSE subscribe endpoint.** `GET /v0/extmsg/{provider}/{account_id}/{conversation_id}/subscribe` opens an SSE stream. The client authenticates via token (same header as FR-1). The controller delivers the target session's replies to this stream as SSE events. The endpoint returns HTTP 200 with `Content-Type: text/event-stream`. On (re)connect the client MAY supply `Last-Event-ID`; the controller replays any transcript entries after that sequence before switching to live delivery. | Must | Acceptance: (a) client sends turn, target session replies, reply appears on stream within 5 s p95; (b) client disconnects, session replies, client reconnects with Last-Event-ID, missed reply is replayed. |
| FR-4 | **In-process subscriber registry.** An in-memory registry (`ConversationRef → channel`) receives from `HandleOutbound` when the conversation's provider is `llm-client`. The registry is created once per controller lifetime (ephemeral, like `AdapterRegistry`). A `llm-client` `TransportAdapter` implementation writes to the registry instead of POSTing to a callback URL. Multiple goroutines subscribing to the same ref are serialized by the registry. | Must | Unit test: Publish to registry → event appears on subscriber channel within 100 ms; Publish with no subscriber → returns receipt with `Delivered:false`. |
| FR-5 | **Generic session reply command.** A session can reply to the originating external caller (whatever provider it is) via a generic `gc extmsg reply --session <name> --conversation <ref> "<text>"` command or by posting to `POST /v0/extmsg/outbound` with an explicit `ConversationRef`. The target session's prompt template receives the `ConversationRef` in its context so it can construct the reply call without knowing the provider. | Must | Acceptance: session replies via generic command; the reply appears on the llm-client SSE stream. |
| FR-6 | **SSE event schema.** Each SSE event carries: `id` (transcript sequence number as string), `event` (one of `message`, `heartbeat`, `error`), `data` (JSON object). For `message` events the data includes at minimum: `text`, `session_id`, `conversation`, `sequence`, `created_at`. Schema is documented and versioned via a `version` field in the data object. | Must | Schema documented in `engdocs/design/` before implementation begins; contract test validates round-trip. |
| FR-7 | **Allowed-session config.** A `city.toml` section (or controller command) lets an operator restrict which sessions a connected client token may bind to. Default: any session on 127.0.0.1 is reachable. A client presenting a token that is not allowed to reach the requested session receives 403 on bind and on subscribe. | Should | Config surface defined by architect; acceptance test: token with explicit allowlist cannot bind to unlisted session. |
| FR-8 | **Heartbeat.** The SSE stream emits a `heartbeat` event every 30 s (configurable) when no message has been sent, so client-side connection liveness checks work without a protocol-level ping. | Should | Integration test: idle stream emits heartbeat event within 35 s. |
| FR-9 | **Stream cleanup on client disconnect.** When the SSE client disconnects, the subscriber registry entry is removed and the associated goroutine exits cleanly. The binding and transcript membership are retained (the client can reconnect). | Must | Unit test: disconnect → registry entry removed; no goroutine leak (goleak or manual count). |
| FR-10 | **Transcript membership.** When a binding is created for `llm-client`, the `TranscriptService.EnsureMembership` is called with `BackfillPolicy: MembershipBackfillSinceJoin` (default). On reconnect with `Last-Event-ID`, the controller lists backfill from that sequence before switching to live delivery. | Must | Matches existing `TranscriptService` API; no new storage needed. |

## Non-Functional Requirements

| ID | Requirement | Metric |
|----|-------------|--------|
| NFR-1 | **Latency (inbound→stream).** Time from session reply to SSE event at the client. | < 500 ms p95 on localhost. |
| NFR-2 | **Concurrent clients.** Number of simultaneous SSE subscribers the controller can serve without memory pressure. | ≥ 50 concurrent streams on a 4-core dev machine (extrapolate to prod). |
| NFR-3 | **Reconnect replay window.** Duration of replay window after disconnect, using existing transcript retention. | ≥ 7 days (matches existing closed-delivery-bead retention). |
| NFR-4 | **No goroutine leak.** Each disconnected client SSE goroutine exits within 5 s of the HTTP connection closing. | Verified by goroutine count in integration test. |
| NFR-5 | **127.0.0.1 only.** The subscribe and client-registration endpoints MUST NOT be reachable from off-loopback interfaces (matches existing extmsg surface). | Enforced by the existing `cityGet`/`cityPost` registration path; verified by test. |
| NFR-6 | **Transport-agnostic contract.** The event schema and path structure must be designed so the SSE endpoint can be replaced by WS or gRPC later with a wire-compatible field set. | Verified by architect review of the schema doc before implementation. |

## Technical Constraints

Derived from `CLAUDE.md` and the extmsg fabric design:

- **No upward dependencies.** `internal/extmsg` must not import `internal/api`
  or `cmd/gc`. The subscriber registry and `llm-client` adapter live in
  `internal/extmsg`; the SSE HTTP handler lives in `internal/api`.
- **Single controller writer.** Phase 1: all `extmsg` mutations go through
  one controller process. The subscriber registry is in-process; no
  cross-process fanout is needed.
- **Adapter identity is controller-assigned.** The `AccountID` in the
  `ConversationRef` is the controller-issued `client_id`, never
  client-asserted. The API must validate this on every mutating call.
- **No "latest route for session."** Reply routing must use a scoped
  `(session, ConversationRef)` path, not a session-level catch-all. The
  existing `DeliveryContextRecord` model already enforces this; the
  `llm-client` path must follow the same rule.
- **No status files — query live state.** The subscriber registry is
  in-memory. Do not write SSE connection state to disk.
- **Zero hardcoded role names.** No Go code may reference `"mayor"` or any
  other role name. The allowed-session config is a set of session names
  supplied by the operator, not hardcoded.
- **Tests next to code.** Unit tests in `internal/extmsg`; HTTP handler tests
  in `internal/api`; end-to-end reconnect test under `test/` with build tag
  `integration`.
- **TDD.** Test first, watch fail, make pass.
- **Atomic file writes** for any config mutations (temp → rename).
- **No panics in library code** — return errors.

## Dependencies

- `internal/extmsg` — all services (Binding, DeliveryContext, Group,
  Transcript) are reused as-is. No changes to existing service interfaces.
- `internal/api` — new HTTP handler for `GET /v0/extmsg/{...}/subscribe`
  and `POST /v0/extmsg/clients` wired via `supervisor_city_routes.go`.
- `internal/events` — existing event bus; the outbound subscriber registry
  may subscribe to `events.ExtMsgOutbound` events as an alternative to
  direct adapter calls (architect to decide).
- `city.toml` + `internal/config` — new `[extmsg.connected_clients]`
  config section for token storage and allowed-session allowlists
  (architect to design the exact shape).
- SSE standard (W3C Server-Sent Events) — no new external libraries needed;
  Go's `net/http` standard library handles SSE without a dependency.
- `tincan-iris` rig — the first consumer client (tracked separately; not a
  dependency for this PRD, but the schema design must accommodate it).

## Open Questions

These are unresolved items the architect and designer must address.

### For the architect

1. **SSE endpoint path.** The proposal uses
   `GET /v0/extmsg/{provider}/{account_id}/{conversation_id}/subscribe`.
   Is the path encoded in URL segments or query params? Define the exact
   path and whether a `ConversationRef` wrapper resource exists separately
   from the subscribe endpoint.

2. **Token storage.** Where are issued tokens stored durably? Options: (a)
   beads with a new `extmsg_client_token` type, (b) a `city.toml` section
   the operator manages, (c) a separate in-memory-only token issued fresh
   each session (loses persistence across controller restart). Persistent
   tokens (option a or b) let a voice assistant survive controller restarts
   without re-registration.

3. **Subscriber registry vs. event bus wiring.** Two approaches for routing
   session replies to SSE streams: (a) `HandleOutbound` calls the
   `llm-client` adapter directly (adapter delivers to the registry); (b)
   the SSE handler subscribes to the event bus and filters by conversation.
   Approach (a) is simpler and consistent with the existing adapter model.
   Approach (b) requires no adapter at all but needs a pub-sub mechanism on
   the event bus. Architect to choose; if (a), define the `llm-client`
   adapter interface clearly.

4. **Generic session reply.** Should the target session reply via
   (a) `gc extmsg reply --conversation <ref>` (new CLI command), (b)
   reuse `POST /v0/extmsg/outbound` from the session's prompt, or (c) a
   named variable in the prompt template that auto-routes? Whichever is
   chosen, the session must NOT need to know the provider is `llm-client`.
   Architect to specify how the `ConversationRef` is surfaced to the session.

5. **Backpressure.** If the SSE subscriber is slow (congested network, slow
   client), how many undelivered messages does the registry buffer before
   dropping or blocking? Architect to define the channel buffer size and
   drop policy.

6. **Multiple conversations per client.** The feature request confirms a
   single client token can own multiple `ConversationID`s (namespaced by
   `AccountID`). Architect to confirm no per-client conversation cap is
   needed in v1.

### For the designer (no UI/UX in this feature; designer reviews docs)

7. **API reference page.** The three new HTTP calls (`POST /v0/extmsg/clients`,
   `POST /v0/extmsg/inbound` with `llm-client` provider, and
   `GET .../subscribe`) need an API reference section and a how-to guide.
   Where does this live in the docs site (mintlify): `reference/`, `guides/`,
   or a new `integrations/` section?

8. **Error UX.** When a client's stream is closed server-side (token expired,
   session stopped), what HTTP status and SSE `error` event payload should
   the client receive? Define the error catalog for this endpoint.

## References

- Source bead: `ga-ty8cfb`
- External Messaging Fabric design:
  `engdocs/design/external-messaging-fabric.md`
  (Trust model §, Open Question #2, controller-assigned adapter identity)
- External Messaging Shared Threads design:
  `engdocs/design/external-messaging-shared-threads.md`
  (Transcript service, backfill, MembershipBackfillPolicy — reused for replay)
- Existing extmsg implementation:
  `internal/extmsg/` (types.go, outbound.go, adapter_registry.go, transcript_service.go)
- Existing API surface:
  `internal/api/huma_handlers_extmsg.go`,
  `internal/api/supervisor_city_routes.go` (lines 346–371)
- First consumer: tincan-iris voice assistant (separate rig, tracked there)
