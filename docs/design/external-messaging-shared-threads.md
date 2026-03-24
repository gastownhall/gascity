# External Messaging Shared Threads

| Field | Value |
|---|---|
| Status | Implemented |
| Date | 2026-03-23 |
| Author(s) | Codex |
| Issue | — |
| Supersedes | [external-messaging-fabric.md](./external-messaging-fabric.md) |

Design update for Gas City's external messaging fabric so sessions can
participate in an external thread like humans in a shared room: when a
session joins, it can read prior thread history; once joined, it sees
all subsequent thread traffic; public reply choice stays separate from
read visibility.

## Problem

The implemented fabric in `internal/extmsg` models inbound routing as
"one target session plus optional passive copies." That shape is too
close to a Discord-specific launcher bridge and not close enough to a
shared-thread model.

Concrete problems:

1. A late-joining session cannot backfill prior external messages.
2. Read visibility is encoded indirectly via passive fanout rather than
   a durable thread transcript.
3. Group routing conflates "who should reply publicly" with "who is
   allowed to see the thread."
4. A connector can normalize a message, but there is no provider-neutral
   store for that message as part of a conversation transcript.
5. Cross-platform conversation flows remain awkward because the system
   lacks a room-level history object shared across adapters.

This shows up immediately in Discord-style agent group sessions. The
desired behavior is:

- add a session to a thread
- give it prior thread context automatically
- let every joined session see new traffic
- choose one speaker for public replies without hiding the thread from
  everyone else

## Goals

1. Make external conversation transcript entries first-class in core.
2. Make conversation membership first-class in core.
3. Default group-session reads to room-wide visibility, not passive
   copies.
4. Preserve targeted/default-speaker routing for public replies.
5. Preserve arbitrary session-to-session messaging as the primary core
   communication model.
6. Keep external adapters thin and provider-neutral.

## Non-Goals

- Shipping a Discord adapter in this change
- Replacing `mail.Provider`
- Removing explicit public publication policy
- Solving every replay/rate-limit concern for every adapter in one step
- Building direct connector-to-connector shortcuts that bypass sessions

## Principles

1. Transcript is shared state.
   External thread history must be durable and queryable in core.
2. Membership controls read visibility.
   Sessions see a thread because they joined it, not because they were
   copied on one routed event.
3. Speaker selection is a policy layer.
   Default handle and last-addressed cursor decide who should reply
   publicly; they do not decide who can read.
4. Transport remains thin.
   Adapters fetch provider history, normalize it, append transcript
   entries, and publish outbound messages.
5. Cross-platform routing still flows through sessions.
   A Discord thread can feed session A, which can message session B,
   whose public delivery may go to Slack. Transcript and delivery state
   stay provider-neutral.

## Proposed Model

Keep the existing binding and delivery services. Add a new transcript
layer and narrow the group service to speaker selection plus participant
membership.

### 1. Conversation transcript

Add a new durable `ConversationTranscriptRecord`:

```go
type TranscriptMessageKind string

const (
    TranscriptMessageInbound  TranscriptMessageKind = "inbound"
    TranscriptMessageOutbound TranscriptMessageKind = "outbound"
)

type ConversationTranscriptRecord struct {
    ID                string
    SchemaVersion     int
    Conversation      ConversationRef
    Sequence          int64
    Kind              TranscriptMessageKind
    Provenance        string
    ProviderMessageID string
    Actor             ExternalActor
    Text              string
    ExplicitTarget    string
    ReplyToMessageID  string
    Attachments       []ExternalAttachment
    SourceSessionID   string
    CreatedAt         time.Time
    Metadata          map[string]string
}
```

Rules:

- Transcript sequence is monotonically increasing per conversation.
- Both inbound external messages and outbound public publications are
  recorded.
- The transcript body text is stored in bead `Description`; structured
  fields remain in bead metadata.
- Transcript append is idempotent for provider-originated traffic when
  the caller supplies a stable `ProviderMessageID`.

### 2. Conversation membership

Add a new durable `ConversationMembershipRecord`:

```go
type MembershipBackfillPolicy string

const (
    MembershipBackfillAll       MembershipBackfillPolicy = "all"
    MembershipBackfillSinceJoin MembershipBackfillPolicy = "since_join"
)

type ConversationMembershipRecord struct {
    ID               string
    SchemaVersion    int
    Conversation     ConversationRef
    SessionID        string
    JoinedAt         time.Time
    JoinedSequence   int64
    LastReadSequence int64
    BackfillPolicy   MembershipBackfillPolicy
    Metadata         map[string]string
}
```

Rules:

- Membership is per `(conversation, session)`.
- `JoinedSequence` captures the transcript head at join time.
- `BackfillAll` means the session can replay the full stored thread.
- `BackfillSinceJoin` means the session starts at the join boundary.
- `LastReadSequence` tracks transcript replay progress.
- Memberships have an explicit lifecycle: active memberships can replay
  and receive new traffic; removed memberships cannot.

### 3. Transcript state and hydration gate

Add one state record per conversation:

```go
type HydrationStatus string

const (
    HydrationLiveOnly HydrationStatus = "live_only"
    HydrationPending  HydrationStatus = "pending"
    HydrationComplete HydrationStatus = "complete"
    HydrationFailed   HydrationStatus = "failed"
)

type ConversationTranscriptStateRecord struct {
    ID                        string
    SchemaVersion             int
    Conversation              ConversationRef
    NextSequence              int64
    EarliestAvailableSequence int64
    HydrationStatus           HydrationStatus
    OldestHydratedMessageID   string
    MaxRetainedEntries        int
    Metadata                  map[string]string
}
```

Rules:

- `NextSequence` is allocated only by core under the conversation lock.
- `EarliestAvailableSequence` marks the retained floor so future
  retention does not silently break replay semantics.
- Historical hydration that must participate in durable transcript
  order is allowed only while `HydrationStatus` is `pending`.
- Live append and historical hydration do not run concurrently for the
  same conversation.
- `HydrationFailed` means replay may proceed only as partial history and
  must say so explicitly.

### 4. Group routing becomes speaker routing

`ConversationGroupRecord` and participant handles stay useful, but the
group service stops deciding readership. It now decides only who should
speak publicly.

- `DefaultHandle` and `LastAddressedHandle` choose the preferred public
  responder
- `FanoutPolicy` applies only to public-publication behavior, not read
  visibility
- `AmbientReadEnabled` is removed because transcript memberships become
  the durable read model

Update `GroupRouteDecision`:

```go
type GroupRouteDecision struct {
    Match           GroupRouteMatch
    TargetSessionID string
    UpdateCursor    bool
}
```

`TargetSessionID` answers "who should reply publicly?".
Readership comes from active transcript memberships, not the route
decision.

## Services

### Transcript service

Add a new `TranscriptService`:

```go
type AppendTranscriptInput struct {
    Caller            Caller
    Conversation      ConversationRef
    Kind              TranscriptMessageKind
    ProviderMessageID string
    Actor             ExternalActor
    Text              string
    ExplicitTarget    string
    ReplyToMessageID  string
    Attachments       []ExternalAttachment
    SourceSessionID   string
    CreatedAt         time.Time
    Metadata          map[string]string
}

type EnsureMembershipInput struct {
    Caller         Caller
    Conversation   ConversationRef
    SessionID      string
    BackfillPolicy MembershipBackfillPolicy
    Metadata       map[string]string
    Now            time.Time
}

type UpdateMembershipInput struct {
    Caller         Caller
    Conversation   ConversationRef
    SessionID      string
    BackfillPolicy MembershipBackfillPolicy
    Metadata       map[string]string
}

type RemoveMembershipInput struct {
    Caller       Caller
    Conversation ConversationRef
    SessionID    string
    Now          time.Time
}

type ListTranscriptInput struct {
    Caller        Caller
    Conversation  ConversationRef
    AfterSequence int64
    Limit         int
}

type ListBackfillInput struct {
    Caller       Caller
    Conversation ConversationRef
    SessionID    string
    Limit        int
}

type AckMembershipInput struct {
    Caller       Caller
    Conversation ConversationRef
    SessionID    string
    Sequence     int64
}

type TranscriptService interface {
    Append(ctx context.Context, input AppendTranscriptInput) (ConversationTranscriptRecord, error)
    List(ctx context.Context, input ListTranscriptInput) ([]ConversationTranscriptRecord, error)
    EnsureMembership(ctx context.Context, input EnsureMembershipInput) (ConversationMembershipRecord, error)
    UpdateMembership(ctx context.Context, input UpdateMembershipInput) (ConversationMembershipRecord, error)
    RemoveMembership(ctx context.Context, input RemoveMembershipInput) error
    ListMemberships(ctx context.Context, caller Caller, ref ConversationRef) ([]ConversationMembershipRecord, error)
    ListConversationsBySession(ctx context.Context, caller Caller, sessionID string) ([]ConversationMembershipRecord, error)
    ListBackfill(ctx context.Context, input ListBackfillInput) ([]ConversationTranscriptRecord, error)
    Ack(ctx context.Context, input AckMembershipInput) error
    BeginHydration(ctx context.Context, caller Caller, ref ConversationRef, metadata map[string]string) (ConversationTranscriptStateRecord, error)
    CompleteHydration(ctx context.Context, caller Caller, ref ConversationRef) (ConversationTranscriptStateRecord, error)
    MarkHydrationFailed(ctx context.Context, caller Caller, ref ConversationRef, metadata map[string]string) (ConversationTranscriptStateRecord, error)
    State(ctx context.Context, caller Caller, ref ConversationRef) (*ConversationTranscriptStateRecord, error)
}
```

Key behaviors:

- Authority model:
  - `Append`, `BeginHydration`, `CompleteHydration`, and
    `MarkHydrationFailed` may be called by controller or scoped adapter.
  - membership and replay methods (`EnsureMembership`, `UpdateMembership`,
    `RemoveMembership`, `ListMemberships`, `ListConversationsBySession`,
    `ListBackfill`, `Ack`) are controller-owned operations because they
    mutate or expose session-scoped replay state.
- `Append` allocates the next transcript sequence under the conversation
  lock and enforces `(conversation, provider_message_id)` uniqueness
  before sequence allocation.
- `EnsureMembership` is idempotent per `(conversation, session)` and
  returns the existing record unchanged on re-call.
- `UpdateMembership` changes replay policy or metadata for an existing
  membership.
- `RemoveMembership` closes read access and stops future replay.
- `List` and `ListBackfill` clamp `Limit` to server-side defaults and a
  hard maximum.
- `ListBackfill` uses membership state:
  - `LastReadSequence` if present
  - otherwise `0` for `BackfillAll`
  - otherwise `JoinedSequence` for `BackfillSinceJoin`
- `Ack` advances `LastReadSequence` monotonically; stale or duplicate
  acks are no-ops.
- `Ack` applies only to the target session's own active membership and
  is validated by the controller before persistence.
- `BeginHydration` starts a controller-visible history import window.
  While hydration is pending, live append for that conversation is
  rejected.
- All adapter-visible operations still enforce the base fabric scope
  rule: adapters may operate only on their own `(Provider, AccountID)`.
- The transcript service shares the same store-identity lock pool as the
  existing binding and delivery services. Conversation sequencing,
  provider-message dedupe, hydration transitions, and membership writes
  all linearize through that shared lock namespace.

### Group service integration

`UpsertParticipant` should also ensure transcript membership with the
default `BackfillAll` policy unless the caller later overrides it. That
gives "session joins thread and sees history" behavior to the default
group-session path without forcing adapter-specific logic into the
connector.

Direct conversation bindings should also ensure a membership for the
bound session so one-to-one and non-group conversations get transcript
history without a separate join call. The default direct-binding policy
is `BackfillSinceJoin` unless the caller asks for more.

Group participation remains the authority for speaker handles. Transcript
membership remains the authority for readership. Group add/remove and
membership add/remove must converge idempotently in the controller:

- adding a participant ensures membership
- removing a participant removes membership
- if a crash splits those writes, the next controller reconciliation pass
  or repeated mutating call repairs the drift

`ResolveInbound` should:

1. find the group for the root conversation
2. pick `TargetSessionID` using:
   - explicit target
   - last addressed handle
   - default handle
3. never decide read visibility; the controller gets active readers from
   `TranscriptService.ListMemberships`

## Adapter flow

For an adapter such as Discord:

1. Verify inbound provider event.
2. Normalize to `ExternalInboundMessage`.
3. Ensure the conversation hydration state is acceptable for the desired
   operation:
   - full-history conversation: `BeginHydration` -> import oldest to
     newest -> `CompleteHydration`
   - live-only conversation: state remains `live_only`
4. Append transcript entry.
5. Resolve group route.
6. List active conversation memberships.
7. Deliver the normalized message to every active reader session.
8. Use `TargetSessionID` only for default public response policy.
9. When a session is added to the thread:
   - ensure transcript membership
   - if the conversation is not yet hydrated, finish hydration before
     replaying transcript history
   - snapshot the current transcript head
   - list backfill from `TranscriptService`
   - deliver backfill to the joining session
   - deliver any records appended after the snapshot before declaring
     live replay complete
   - acknowledge the highest delivered sequence

The important separation is:

- provider history fetch is adapter work
- durable shared history, replay semantics, and hydration state are
  core work

## Storage Layout

Add three new bead types:

1. `external_transcript`
2. `external_membership`
3. `external_transcript_state`

New labels:

- transcript by conversation
- transcript by conversation + sequence bucket
- transcript by conversation + provider message ID
- membership by conversation
- membership by conversation + session
- membership by session
- transcript state by conversation

Transcript entries use:

- `Title`: `<provider>/<account>/<conversation>#<sequence>`
- `Description`: normalized text body
- `Metadata`: sequence, actor JSON, attachments JSON, provider message
  ID, reply-to ID, explicit target, source session ID, timestamps,
  provenance (`live` or `hydrated`)

Membership entries use:

- `Title`: `<session> -> <provider>/<account>/<conversation>`
- `Metadata`: join timestamp, join sequence, last read sequence,
  backfill policy, closed-at timestamp when removed

Transcript state entries use:

- `Title`: `<provider>/<account>/<conversation>/state`
- `Metadata`: next sequence, earliest available sequence, hydration
  status, retention configuration

## Migration

Because `internal/extmsg` is new and not yet wired to a shipping
external adapter in this repo, we can make a clean semantic break now.

### Step 1

Add transcript and membership services behind `extmsg.NewServices`.

### Step 2

Add transcript state, exact lookup labels for provider message ID and
membership keys, and explicit hydration lifecycle.

### Step 3

Change group routing to speaker-only decisions and remove
`AmbientReadEnabled`.
There is no supported mixed-semantics rollout: existing experimental
group records are rewritten through `EnsureGroup`/`UpsertParticipant`
before use.

### Step 4

Make participant upsert ensure transcript membership with
`BackfillAll`.
Direct binding paths ensure `BackfillSinceJoin` membership.

### Step 5

Update tests to cover:

- transcript append ordering
- dedupe by `(conversation, provider_message_id)`
- hydration gating
- replay handoff across backfill and live traffic
- membership backfill-all vs since-join
- monotonic ack behavior
- late join replay
- membership removal
- group route speaker selection stays independent from readership

### Step 6

Later adapter work can use these primitives to backfill Discord thread
history and share the same model with Slack, Telegram, or other
providers.

## Risks

1. Transcript storage growth
   Phase 1 defines retention primitives but does not implement sweeping
   or archival.
2. Provider history catch-up gaps
   `HydrationFailed` must surface partial-history state to callers.
3. Duplicate provider events
   Exact provider-message lookup prevents duplicate appends, but bad
   providers may still omit stable IDs.

## Open Questions

1. Should public outbound publications always be appended to transcript,
   or only those visible in the same external conversation?
2. Do we want a future membership policy for bounded backfill
   (`last_n`) in addition to `all` and `since_join`?
3. Should message edits and deletions be represented as new transcript
   events or as mutation of existing transcript records?
