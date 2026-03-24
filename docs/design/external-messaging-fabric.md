# External Messaging Fabric

| Field | Value |
|---|---|
| Status | Implemented |
| Date | 2026-03-23 |
| Author(s) | Codex |
| Issue | — |
| Supersedes | — |

Design for a provider-neutral external messaging layer in Gas City.
This introduces explicit external conversation bindings, delivery-route
state, and provider-neutral group-session policy so Gas City can support
OpenClaw-style channel adapters without giving up Gas City's stronger
"any session can talk to any other session" model.

The transcript-backed shared-thread read model introduced later in
[`external-messaging-shared-threads.md`](./external-messaging-shared-threads.md)
supersedes the ambient-read and passive-fanout portions of this design.

## Problem

Today Gas City's messaging model is:

- `mail.Provider` for durable mailbox-style exchange
- `runtime.Provider.Nudge()` for live session wakeups
- `session.Manager.Send()` for direct session input delivery

That is enough for internal agent-to-agent and human-to-agent work, but
it does not model an external conversation as a first-class object.

Concrete gaps:

1. There is no provider-neutral `ConversationRef`. External adapters
   would need to invent their own routing state.
2. There is no durable conversation-to-session binding service. A
   connector cannot say "this Discord thread now belongs to session X"
   using a shared core API.
3. There is no provider-neutral group-session abstraction. The Discord
   launcher behavior in `../packs` is useful, but it is encoded as
   Discord-specific bridge state rather than reusable core policy.
4. There is no durable, scoped reply-route model for "send the
   completion back to the external conversation that originated this
   work."
5. The existing `mail.Provider` extension point is too narrow. It models
   mailbox operations, not external conversations, thread creation,
   explicit publication, or session-bound return routing.

OpenClaw already separates these concerns more cleanly:

- external conversation identity
- conversation-to-session bindings
- outbound route resolution
- provider-specific transport adapters

Gas City should adopt that shape without flattening everything into a
channel-local session model. Session-to-session messaging stays core.
External messaging becomes a fabric layered on top of it.

## Goals

1. Make external conversation identity first-class in core.
2. Allow any external conversation to bind to any Gas City session.
3. Preserve Gas City's existing arbitrary session-to-session messaging.
4. Support provider-neutral launcher/group-session behavior:
   targeted agents, last-addressed cursor, passive observers, explicit
   public publication, and bounded peer fanout.
5. Keep transport adapters thin so Discord, Slack, Telegram, email, and
   future adapters share the same internals.
6. Preserve current `gc mail`, `gc session`, and session lifecycle
   behavior during migration.

## Non-Goals

- Shipping every OpenClaw adapter in this change
- Replacing `mail.Provider` in one step
- Changing session runtime semantics
- Building connector-to-connector shortcuts that bypass session routing
- Requiring external platforms to become the source of truth for session
  identity or permissions

## Principles

1. Session routing is core-owned.
   External platforms map into sessions; they do not own the session
   graph.
2. Transport is thin.
   Connectors verify inbound events, normalize them, and publish
   outbound messages. They do not own launcher, fanout, or return-route
   policy.
3. Public publication is explicit.
   Internal agent traffic and human-visible external publication are
   distinct operations.
4. Bindings are durable.
   A reply path must survive process restarts.
5. Group sessions are policy, not transport.
   A Discord thread is a surface. A group/cohort is a reusable routing
   object.
6. The controller is the single writer.
   Phase 1 assumes all `extmsg` mutations go through one controller
   process. Direct store writes by adapters are forbidden.

## Trust Model

The fabric introduces new authorization and identity rules.

### Caller rule

Every mutating operation is invoked by an explicit caller:

```go
type CallerKind string

const (
    CallerController CallerKind = "controller"
    CallerAdapter    CallerKind = "adapter"
)

type Caller struct {
    Kind      CallerKind
    ID        string
    Provider  string
    AccountID string
}
```

Phase 1 authorization policy:

- `controller` may perform any mutation
- `adapter` may mutate only records for its own `(Provider, AccountID)`
- sessions, humans, and generic public clients may not call mutating
  fabric operations directly

Adapter identity is controller-assigned, not adapter-asserted.

At registration time the controller binds:

- provider
- account ID
- adapter instance

The controller constructs `Caller` for adapter-originated operations.
The adapter never supplies its own authoritative `(Provider, AccountID)`
tuple at call time.

### Inbound identity rule

The fabric never trusts a raw `ConversationRef` returned by an adapter.
Adapters must verify provider authenticity first.

## Architecture

### Layer 1: Session Graph (existing)

Gas City's session graph remains the source of truth for agent work:

- `session.Manager`
- existing session APIs
- mail and nudge

This is where "session A talks to session B" already exists and where it
must continue to live.

### Layer 2: External Messaging Fabric (new)

Add a new internal package:

`internal/extmsg`

This package owns four core concepts:

1. `ConversationRef`
2. `SessionBindingRecord`
3. `DeliveryContextRecord`
4. `ConversationGroupRecord`

The fabric does not send transport traffic itself. It resolves who owns
an external conversation, where explicit public replies should go, and
how group-session policy applies.

### Layer 3: Transport Adapters (new)

Adapters implement provider-specific mechanics:

- verify and normalize inbound payloads
- publish formatted outbound text/media
- create child conversations when supported
- report provider capabilities

Adapters never decide the session graph. They ask the fabric.

### Dependency direction

The import direction is one-way:

`adapter -> extmsg -> session ingress interface`

`session` does not import `extmsg`.

Outbound publication requests are expressed through a narrow interface
owned above `session`, not by introducing a direct `session -> extmsg`
import.

## Core Types

### ConversationRef

```go
type ConversationKind string

const (
    ConversationDM     ConversationKind = "dm"
    ConversationRoom   ConversationKind = "room"
    ConversationThread ConversationKind = "thread"
)

type ConversationRef struct {
    ScopeID              string
    Provider             string
    AccountID            string
    ConversationID       string
    ParentConversationID string
    Kind                 ConversationKind
}
```

This is the provider-neutral identity for an external surface.

Notes:

- `ScopeID` is the city-level namespace. Even though Phase 1 stores are
  city-scoped, the type carries scope explicitly so cross-city behavior
  never depends on deployment topology.
- `ConversationRef` is not a session identifier and never becomes one.

### ExternalInboundMessage

```go
type InboundPayload struct {
    Body        []byte
    ContentType string
    Headers     map[string][]string
    ReceivedAt  time.Time
}

type ExternalActor struct {
    ID          string
    DisplayName string
    IsBot       bool
}

type ExternalInboundMessage struct {
    ProviderMessageID string
    Conversation      ConversationRef
    Actor             ExternalActor
    Text              string
    ExplicitTarget    string
    ReplyToMessageID  string
    Attachments       []ExternalAttachment
    DedupKey          string
    ReceivedAt        time.Time
}

type ExternalAttachment struct {
    ProviderID string
    URL        string
    MIMEType   string
}
```

This is the provider-neutral inbound event contract after verification
and normalization.

### SessionBindingRecord

```go
type BindingStatus string

const (
    BindingActive BindingStatus = "active"
    BindingEnded  BindingStatus = "ended"
)

type SessionBindingRecord struct {
    ID                string
    SchemaVersion     int
    Conversation      ConversationRef
    SessionID         string
    Status            BindingStatus
    BoundAt           time.Time
    ExpiresAt         *time.Time
    BindingGeneration int64
    Metadata          map[string]string
}
```

This is the core equivalent of OpenClaw's conversation binding. It says
"this external conversation currently belongs to this session."

Important:

- `SessionID` is the only canonical target identity
- aliases are display-only and are not persisted in authority records
- there is exactly one active binding per `ConversationRef`

### DeliveryContextRecord

```go
type DeliveryContextRecord struct {
    ID                string
    SchemaVersion     int
    SessionID         string
    Conversation      ConversationRef
    BindingGeneration int64
    LastPublishedAt   time.Time
    LastMessageID     string
    SourceSessionID   string
    Metadata          map[string]string
}
```

This is the durable public return-route state for one
`(SessionID, ConversationRef)` pair.

It is not "latest route for the session." That shape creates shadow
routing. It is scoped to a specific origin conversation and invalidated
when the binding generation changes or the conversation is unbound.

### ExternalOriginEnvelope

```go
type ExternalOriginEnvelope struct {
    Conversation      ConversationRef
    BindingID         string
    BindingGeneration int64
    Passive           bool
}
```

Phase 1 defines this type even though full cross-session propagation is
Phase 2 work. That keeps the session-to-session contract forward
compatible.

### AdapterCapabilities

```go
type AdapterCapabilities struct {
    SupportsChildConversations bool
    SupportsAttachments        bool
    MaxMessageLength           int
}
```

### PublishRequest / PublishReceipt

```go
type PublishRequest struct {
    Conversation   ConversationRef
    Text           string
    ReplyToMessageID string
    IdempotencyKey string
    Metadata       map[string]string
}

type PublishFailureKind string

const (
    PublishFailureUnsupported PublishFailureKind = "unsupported"
    PublishFailureTransient   PublishFailureKind = "transient"
    PublishFailureRateLimited PublishFailureKind = "rate_limited"
    PublishFailurePermanent   PublishFailureKind = "permanent"
    PublishFailureAuth        PublishFailureKind = "auth"
    PublishFailureNotFound    PublishFailureKind = "not_found"
)

type PublishReceipt struct {
    MessageID       string
    Conversation    ConversationRef
    Delivered       bool
    FailureKind     PublishFailureKind
    RetryAfter      time.Duration
    Metadata        map[string]string
}
```

```go
var ErrAdapterUnsupported = errors.New("adapter unsupported")
```

### ConversationGroupRecord

```go
type GroupMode string

const (
    GroupModeLauncher GroupMode = "launcher"
)

type FanoutPolicy struct {
    Enabled                     bool
    AllowUntargetedPublication  bool
    MaxPeerTriggeredPublishes   int
    MaxTotalPeerDeliveries      int
}

type ConversationGroupRecord struct {
    ID                    string
    SchemaVersion         int
    RootConversation      ConversationRef
    Mode                  GroupMode
    DefaultHandle         string
    LastAddressedHandle   string
    AmbientReadEnabled    bool
    FanoutPolicy          FanoutPolicy
    Metadata              map[string]string
}

type ConversationGroupParticipant struct {
    ID            string
    GroupID       string
    Handle        string
    SessionID     string
    Public        bool
    Metadata      map[string]string
}
```

### GroupRouteDecision

```go
type GroupRouteMatch string

const (
    GroupRouteExplicitTarget GroupRouteMatch = "explicit_target"
    GroupRouteLastAddressed  GroupRouteMatch = "last_addressed"
    GroupRouteDefault        GroupRouteMatch = "default"
    GroupRouteNoMatch        GroupRouteMatch = "no_match"
)

type GroupRouteDecision struct {
    Match          GroupRouteMatch
    TargetSessionID string
    PassiveSessionIDs []string
    UpdateCursor   bool
}
```

## Services

### BindingService

```go
type BindingService interface {
    Bind(ctx context.Context, caller Caller, input BindInput) (SessionBindingRecord, error)
    ResolveByConversation(ctx context.Context, ref ConversationRef) (*SessionBindingRecord, error)
    ListBySession(ctx context.Context, sessionID string) ([]SessionBindingRecord, error)
    Touch(ctx context.Context, caller Caller, bindingID string, now time.Time) error
    Unbind(ctx context.Context, caller Caller, input UnbindInput) ([]SessionBindingRecord, error)
}
```

Responsibilities:

- explicit bind/unbind
- durable lookup by conversation
- reverse lookup by session
- TTL/idle touch semantics
- one-active-binding-per-conversation invariant

### DeliveryContextService

```go
type DeliveryContextService interface {
    Record(ctx context.Context, caller Caller, input DeliveryContextRecord) error
    Resolve(ctx context.Context, sessionID string, ref ConversationRef) (*DeliveryContextRecord, error)
    ClearForConversation(ctx context.Context, sessionID string, ref ConversationRef) error
}
```

Responsibilities:

- remember where public output last went for a specific origin
- support bound completion routing
- invalidate state on unbind/rebind

### GroupService

```go
type GroupService interface {
    EnsureGroup(ctx context.Context, caller Caller, input EnsureGroupInput) (ConversationGroupRecord, error)
    UpsertParticipant(ctx context.Context, caller Caller, input UpsertParticipantInput) (ConversationGroupParticipant, error)
    ResolveInbound(ctx context.Context, event ExternalInboundMessage) (*GroupRouteDecision, error)
    UpdateCursor(ctx context.Context, caller Caller, input UpdateCursorInput) error
}
```

Responsibilities:

- launcher/group-session state
- explicit targeting by handle
- best-effort last-addressed cursor
- passive observers

Important:

- `ResolveInbound` is a pure query
- it does not create bindings as a side effect
- binding creation stays in `BindingService`
- Phase 1 ships launcher-mode routing only; additional group modes stay
  deferred until they have distinct semantics
- `EnsureGroup(...)` preserves an existing `last_addressed_handle` when
  the upsert input leaves `LastAddressedHandle` empty
- cursor persistence for targeted inbound routing is handled explicitly via
  `UpdateCursor(...)`
- peer-fanout accounting is a Phase 2 adapter/controller concern; Phase 1
  stores the policy fields but does not expose a publication-accounting API

### TransportAdapter

```go
type TransportAdapter interface {
    Name() string
    Capabilities() AdapterCapabilities
    VerifyAndNormalizeInbound(ctx context.Context, payload InboundPayload) (*ExternalInboundMessage, error)
    Publish(ctx context.Context, req PublishRequest) (*PublishReceipt, error)
    EnsureChildConversation(ctx context.Context, ref ConversationRef, label string) (*ConversationRef, error)
}
```

Notes:

- `EnsureChildConversation` returns `ErrAdapterUnsupported` when
  `SupportsChildConversations` is false
- adapter lifecycle such as gateways, pollers, or webhooks stays
  adapter-internal; the message-plane contract above is intentionally
  smaller
- adapters must enforce provider-specific payload size limits before
  calling into the fabric
- `ExternalAttachment.URL` is a provider-hosted URL or controller-managed
  fetch URL; it is never a local filesystem path

## Storage Model

Use the city bead store as the durable state backend for the fabric.

New bead types:

- `external_binding`
- `external_delivery`
- `external_group`
- `external_group_participant`

New labels:

- `gc:extmsg-binding`
- `gc:extmsg-delivery`
- `gc:extmsg-group`
- `gc:extmsg-group-participant`

### Exact lookup labels

Phase 1 avoids broad scans by writing composite lookup labels:

- binding conversation key:
  `extmsg:binding:conv:v1:<sha256([scope,provider,account,conversation,parent,kind])>`
- binding session key:
  `extmsg:binding:session:v1:<sessionID>`
- delivery route key:
  `extmsg:delivery:route:v1:<sha256([scope,provider,account,conversation,parent,kind,sessionID])>`
- delivery session key:
  `extmsg:delivery:session:v1:<sessionID>`
- group root key:
  `extmsg:group:root:v1:<sha256([scope,provider,account,conversation,parent,kind])>`
- participant group key:
  `extmsg:group:participant:v1:<groupID>`
- participant session key:
  `extmsg:group:participant:session:v1:<sessionID>`

Human-readable fields stay in metadata. Labels are for exact lookup only.
The hash input is a structured tuple encoding, not a delimiter-joined string.

### Bead field mapping

#### Binding bead

- `Type`: `external_binding`
- `Status`: `open` while active, `closed` when ended
- `Title`: `<provider>/<account>/<conversation>`
- `Labels`: exact lookup labels above
- `Metadata`:
  `schema_version`, `scope_id`, `provider`, `account_id`,
  `conversation_id`, `parent_conversation_id`, `conversation_kind`,
  `session_id`, `binding_generation`, `bound_at`, `expires_at`,
  `last_touched_at`, `created_by_kind`, `created_by_id`

#### Delivery bead

- `Type`: `external_delivery`
- `Status`: `open` while valid, `closed` when invalidated
- `Title`: `<sessionID> -> <provider>/<account>/<conversation>`
- `Labels`: exact delivery-route label and session label
- `Metadata`:
  `schema_version`, `session_id`, `scope_id`, `provider`, `account_id`,
  `conversation_id`, `parent_conversation_id`, `conversation_kind`,
  `binding_generation`, `last_published_at`, `last_message_id`,
  `source_session_id`

There is one active delivery bead per `(SessionID, ConversationRef)` pair.
This is intentionally not append-only.

#### Group bead

- `Type`: `external_group`
- `Status`: `open`
- `Title`: `<provider>/<account>/<conversation>`
- `Labels`: group root key
- `Metadata`:
  `schema_version`, `scope_id`, `provider`, `account_id`,
  `conversation_id`, `parent_conversation_id`, `conversation_kind`,
  `mode`, `default_handle`, `last_addressed_handle`,
  `ambient_read_enabled`, fanout policy fields

#### Group participant bead

- `Type`: `external_group_participant`
- `Status`: `open`
- `Title`: `<groupID>/<handle>`
- `Labels`: participant group key, session label
- `Metadata`:
  `schema_version`, `group_id`, `handle`, `session_id`, `public`

## Concurrency Model

Phase 1 correctness depends on one controller process being the sole
writer to `extmsg` beads.

Within that controller process, services that share a bead store also
share a process-global lock pool keyed by store identity so duplicate
`Services` or `GroupService` construction does not bypass uniqueness
locking.

### Binding uniqueness

- `Bind()` acquires a process-local lock keyed by normalized
  `ConversationRef`
- it performs an exact lookup by conversation label
- it re-checks the lookup after the lock is acquired
- if an active binding already exists for another session, it returns
  `ErrBindingConflict`
- it does not silently unbind and rebind
- if corrupted state exposes multiple active matches, resolution fails
  with an invariant error instead of picking a backend-order-dependent
  winner
- the bead create/write is the linearization point

This is sufficient because the controller is the only writer.

### Cursor updates

`LastAddressedHandle` is group-scoped and uses last-writer-wins.

That is acceptable because:

- targeted messages still route explicitly
- stale cursor reads never fan out
- if the handle is missing or invalid, routing falls back to
  `DefaultHandle` or `no_match`

Phase 1 intentionally does not implement per-external-user cursors.

### Touch debouncing

`Touch()` is debounced. Frequent adapter keepalives do not rewrite the
 same bead on every inbound event.

### Retention and expiry

Phase 1 retention defaults:

- closed binding beads: purge after 30 days
- closed delivery beads: purge after 7 days
- closed group participant beads: purge after 30 days
- closed group beads: purge after 90 days

Expiry enforcement:

- `ResolveByConversation` treats an expired binding as a miss
- the controller sweep closes expired bindings every 15 minutes
- `Unbind()` closes the binding before returning and then attempts
  synchronous delivery cleanup; a cleanup failure leaves stale delivery
  state to be reaped lazily on the next `Resolve()`

## Routing Model

### Inbound Flow

1. Adapter receives provider payload.
2. Adapter verifies authenticity and normalizes into
   `ExternalInboundMessage`.
3. Fabric resolves:
   - exact binding by `ConversationRef`
   - group routing policy if the conversation is a launcher/group
4. Fabric returns a target session ID and optional passive observers.
5. Adapter-facing bridge delivers the input into a session ingress
   interface implemented above `session.Manager.Send(...)`.

Passive observer deliveries carry `ExternalOriginEnvelope{Passive:true}`
but do not create `DeliveryContextRecord` entries.

### Outbound Flow

1. A session or higher-level workflow requests explicit public
   publication.
2. The request carries either:
   - explicit `ConversationRef`, or
   - a scoped reply context derived from the originating conversation
3. Fabric resolves the destination using the scoped route state for that
   exact `(SessionID, ConversationRef)` pair.
4. Adapter publishes the message.
5. Fabric updates the matching `DeliveryContextRecord`.

The fabric never resolves publication from "latest route for this
session." That shape is forbidden.

### Cross-Platform Messaging

This design allows:

1. Discord thread bound to session A
2. Session A sends to session B through normal session-to-session APIs
3. The session-to-session envelope carries the originating
   `ConversationRef`
4. Session B explicitly publishes to a Slack conversation it owns, or
   replies to the carried origin if policy allows

The path remains:

`external conversation -> session -> session -> external conversation`

No connector-to-connector bridge is needed.

## Group Session Semantics

Provider-neutral group sessions capture the useful parts of the current
Discord bridge:

- launcher surface bound to a room/thread
- explicit participant handles
- optional default participant
- last-addressed cursor
- passive observers for targeted turns
- explicit public publish
- bounded peer fanout

### Dispatch rules

#### Targeted inbound message

- route to the explicitly addressed participant
- if `AmbientReadEnabled`, deliver passive copies to non-target
  participants
- update cursor to the explicit handle
- do not peer-fanout
- participant routing is based on open participant membership, not on
  per-participant direct bindings to the root conversation

#### Untargeted inbound message

- route to `LastAddressedHandle` if valid
- otherwise route to `DefaultHandle` if configured
- otherwise return `no_match`
- passive observers apply only when a target was resolved

#### Peer fanout

Peer fanout applies only to explicit publication events, not to raw human
inbound turns.

`AllowUntargetedPublication` controls whether a publication with no
explicit participant target may notify peer participants internally.

### Loop guard

Every peer-triggered publication carries a `PublicationRootID`.

`FanoutPolicy` bounds:

- max peer-triggered publishes per root
- max total peer deliveries per root

If either bound is exceeded, the publication is suppressed.

## Compatibility Plan

### Existing mail

`mail.Provider` remains intact.

It continues to model internal mailbox semantics. It is not promoted as
the external messaging abstraction.

### Existing session APIs

Existing session APIs remain.

Phase 1 does not require new public HTTP endpoints for `extmsg`.
Adapters embedded in the controller or in controller-owned services call
the internal package directly.

### Existing packs Discord flow

The current Discord pack migrates in stages.

#### Migration states

1. `legacy-only`
   Pack-local JSON remains authoritative.
2. `dual-write-compare`
   Legacy write remains authoritative, but equivalent fabric records are
   written and compared.
3. `extmsg-read-with-legacy-fallback`
   Reads prefer fabric state, but missing state falls back to legacy.
4. `extmsg-only`
   Fabric is authoritative. Legacy JSON is no longer read.

Rollback is always to the previous state, never directly from
`extmsg-only` to `legacy-only`.

Migration state is stored in `city.toml` under a controller-owned
configuration field and reloaded on controller restart.

#### Reconciliation contract

`dual-write-compare` compares the normalized legacy projection and the
normalized fabric projection for the same operation.

Any divergence:

- emits a structured controller event
- marks the conversation ineligible for promotion
- requires operator acknowledgement before promotion resumes

#### Promotion gates

- `legacy-only -> dual-write-compare`
  operator opt-in only
- `dual-write-compare -> extmsg-read-with-legacy-fallback`
  zero divergence events for 48 hours on the promoted canary scope
- `extmsg-read-with-legacy-fallback -> extmsg-only`
  zero fallback reads and zero divergence events for 7 days on the
  promoted scope

Promotion may be city-wide or canary-scoped by conversation/channel, but
the controller must record the chosen granularity explicitly.

#### Fallback semantics

`extmsg-read-with-legacy-fallback` falls back only when the fabric lookup
returns "not found".

It does not silently fall back for:

- closed records
- authorization failures
- verification failures
- bead store read errors

Those cases surface as errors because silent fallback would hide data
loss or policy bugs.

#### Legacy field mapping

| Legacy source | Fabric destination |
|---|---|
| `chat.bindings[*].kind + conversation_id` | `SessionBindingRecord.Conversation` |
| `chat.bindings[*].session_names[]` | one or more `ConversationGroupParticipant` or direct binding target session IDs |
| `chat.bindings[*].policy` | `ConversationGroupRecord.FanoutPolicy` / ambient-read fields |
| `chat.launchers[*].conversation_id` | `ConversationGroupRecord.RootConversation` |
| `chat.launchers[*].response_mode` | `ConversationGroupRecord.Mode` |
| `chat.launchers[*].default_qualified_handle` | `ConversationGroupRecord.DefaultHandle` |
| `chat.launchers[*].policy` | `ConversationGroupRecord` fanout fields |

## Initial Implementation Scope

This design is intentionally staged.

### Phase 1

Implement in Gas City:

- `internal/extmsg` core types
- bead-backed binding store
- bead-backed delivery-context store
- exact-lookup label helpers
- process-local binding locks
- group and participant record stores
- pure group route resolver
- unit tests for authorization gates, uniqueness, invalidation, and
  routing

Phase 1 does not add general public HTTP APIs for `extmsg`.

### Phase 2

Add:

- controller-local adapter integration interfaces
- scoped reply-context propagation for session-to-session flows
- event recording / observability helpers
- controller-local API surface if an out-of-process connector still
  needs it
- adapter readiness / health reporting

### Phase 3

Migrate Discord launcher semantics from `../packs` onto the new fabric
using the migration states above.

### Phase 4

Add transport adapters beyond Discord.

## Risks

### Risk: overloading mail

Do not stretch `mail.Provider` to absorb these concepts. Mail is a
useful legacy/internal abstraction, but it is not a conversation
binding/runtime model.

### Risk: provider-owned routing

If adapters decide session ownership locally, the system loses
cross-platform routing and coherent provenance.

### Risk: text-envelope state

Discord-style metadata embedded in message bodies is not durable enough.
State must be structured and stored in core.

### Risk: coupling group policy to one platform

Launcher and peer-fanout behavior must be defined generically so that a
Slack thread or Telegram topic can use the same logic.

### Risk: unbounded state

Bindings, route state, and group data must have exact lookup keys,
retention rules, and invalidation behavior from day one.

## Open Questions

1. Should Phase 2 introduce a route token in addition to the scoped
   `DeliveryContextRecord`, or is the `(SessionID, ConversationRef)`
   route key sufficient?
2. Which controller-local API shape is best for out-of-process adapters:
   HTTP with service identity, or a narrower supervisor-owned IPC path?
3. When we add non-Discord adapters, do we need a richer capability model
   for edits, reactions, or media threading?

## Recommendation

Adopt the fabric in core first, then migrate transport bridges onto it.

The key architectural decision is:

- OpenClaw's binding and routing model becomes the foundation
- Gas City's session graph remains the source of truth
- Discord launcher behavior becomes a provider-neutral group-session
  policy layer
- the controller remains the only writer and authorization boundary

That gives Gas City the thing OpenClaw does not have by default:
arbitrary session-to-session messaging across any connected surface.
