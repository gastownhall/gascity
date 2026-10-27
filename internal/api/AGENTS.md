# internal/api — change guide

The HTTP + SSE API is a projection over the domain packages
(`internal/{beads, mail, convoy, formula, events, session, worker, sling, ...}`);
it never re-implements domain logic.

## Read first

Read **`engdocs/architecture/api-control-plane.md`** and
**`engdocs/contributors/huma-usage.md`** before touching:

- `internal/api/` (HTTP + SSE API layer)
- `cmd/gc/` (CLI) — especially anything that constructs events,
  calls `apiroute.go:apiClient()`, or uses
  `internal/api/genclient`
- `internal/events/` (event bus, registry)
- `internal/extmsg/` (external-messaging emitters)
- Anything that affects `internal/api/openapi.json`,
  `docs/reference/schema/openapi.json`, or the generated TS types under
  `internal/api/dashboardspa/web/shared/src/generated/`

## Load-bearing invariants enforced by CI

Violating any fails the build; full rationale is in the architecture docs.

- **Object model at the center.** `internal/{beads, mail, convoy,
  formula, events, session, worker, sling, ...}` is the canonical
  domain. The CLI (`cmd/gc/`) and the HTTP+SSE API
  (`internal/api/`) are projections over it. Neither re-implements
  domain logic. `internal/agent/` is a small helper package
  (session-name utilities, startup hints) — not a primitive.
- **Typed wire.** No hand-written JSON on any HTTP or SSE wire
  path; no `map[string]any` or `json.RawMessage` on wire types
  (documented exceptions live in the API control-plane doc). All
  endpoints are Huma-registered; the OpenAPI spec is generated,
  never hand-written (`TestOpenAPISpecInSync`).
- **Typed events.** Every constant in `events.KnownEventTypes`
  must have a registered payload via
  `events.RegisterPayload(constant, sample)`. Use
  `events.NoPayload` for events whose envelope fields alone
  capture the semantics. Enforced by
  `TestEveryKnownEventTypeHasRegisteredPayload`.
- **Nudge queue through State.** The API writes the city's nudge
  queue only through `State.NudgeQueue`; only `seededState` builds a
  `nudgequeue.StateFileQueue`. Enforced by
  `TestAPIReachesTheNudgeQueueOnlyThroughState`.

## Configuration publication

API config edits, validation, snapshot publication and city-config rollback
share the controller's publication transaction with runtime reloads. A rejected
edit must remain absent from both disk and the API's cached raw config. Preserve
that boundary when changing `mutateAndPoke` or runtime config publication.

The controller's config locks have one order: the config transaction fence
(`configTransactionMu`), then the publication lock (`publishMu`), then the
`configedit.Editor` lock, then `updateMu`, then `mu`. A multi-file config
transaction (pack import, rig provisioning) takes the publication lock before
the Editor lock and publishes through `mutateAndPokeLocked`; taking the Editor
lock first deadlocks against a plain edit, which holds the publication lock
when it takes the Editor lock. The publication lock stays held across a
transaction's pack fetch or rig provisioning, and across an API city or rig
suspend or resume while it waits on the suspension fence; reload publication
and other API edits wait out those bounded stalls.

## Worker boundary exception

`session_manager.go` constructs `session.Manager` values for API handlers —
a sanctioned bypass of the worker boundary. Do not add others; see
`internal/worker/AGENTS.md`.

## Dashboard and API-schema gates

- `make dashboard-check` passes for any change touching `internal/api/`,
  `internal/api/openapi.json`, `docs/reference/schema/openapi.*`,
  `internal/api/dashboardspa/`, or generated dashboard types. It runs the
  dashboard's Bazel targets (`//internal/api/dashboardspa/...`, which CI's
  `bazel test //...` gates on): typecheck, Vitest, the SPA build, and the
  diff tests that fail when the committed `dist/`, generated API client, or
  `pnpm-lock.yaml` is stale (`make dashboard-build`,
  `make dashboard-generate-client`, `make dashboard-lock` fix them)
- The dashboard starts locally and serves the app for dashboard/API-schema
  changes (a manual step, not a CI gate); use
  `npm run preview -- --host 127.0.0.1 --port <port>` from
  `internal/api/dashboardspa/web/frontend` after `make dashboard-build-npm`

## Fresh active-work lookup

Batch a session's assignee aliases into one live `List` call per bead store,
not one per alias. That bounds the calls, not the backend work: how far a store
narrows the call varies by store (see `findLiveActiveBeadInStores`). Preserve
assignee priority across stores and newest-first order within a store, so a
concrete session's work wins over a template fallback. Stop reading stores once
the first identity (the session id, when known) has a match, and keep a store's
partial rows instead of dropping the store. Neither the live nor the cached
lookup reads a store that only rigs suspended by effective state reach (the
set `statusWorkCounts` skips, `cacheColdRigNames`); a store such a rig shares
with a rig that is not suspended, as in legacy shared-file mode, is read
through that rig. A lookup scoped to a suspended rig reads only that shared
store, or nothing, and never falls back to the other rigs' stores
(`activeBeadSearchScope`).

## Deferred creation baseline

Before scheduling an agent session, persist the command the controller resolves
for that agent's template as the session's command baseline. Build it with
`config.BuildProviderTemplateLaunchCommand`, the composition the controller's
`resolveTemplate` shares, not with the strict `BuildProviderLaunchCommand`:
the controller omits an `option_defaults` pin it cannot render and logs a
warning, so the create must do the same instead of failing. Keep explicit
per-session options, validated at create time, in `template_overrides`;
otherwise startup can mistake its own baseline for config drift or compare one
session's overrides against the template.

## City op state and kept terminal events

The supervisor builds a new `Server` for a city whenever the city's `State`
changes, while the old one's `/submit` and `/messages` deliveries may still
run. What those deliveries share lives per city per process in `cityOps`
(`session_ops.go`; DES-O0API D9, I3), not on a `Server`: the in-flight
registry, the degrade reporter, the kept terminal events and the city's live
`Server`. `SupervisorMux` never removes a city's `cityOps`, and
`getCityServer` publishes a new `Server` before it retires the one it
replaces, so a same-key retry still finds a replaced `Server`'s running
delivery, and that delivery a live `Server` to report through.

In every `ops.content` mode, `off` included, a delivery, and a session create,
records its terminal event to the city's live `Server`'s event log
(`emitSessionRequestResult`), and a recorded delivery closes its op through
that `Server`'s store (`openOpStore`); its own `Server` serves only when the
city has no live `Server` (the supervisor dropped it). An event the log
refuses is kept, in order, and recorded when the supervisor publishes the
city's next `Server`, before the city's next terminal event, and before a
same-key retry of its request is answered. Three points answer such a retry:
the `Idempotency-Key` LRU (`withMessageIdempotency`), the in-flight registry
and the stored op (`replayAnswer`). Each records the kept events first and
answers 409 `in_flight` while the retry's event stays kept, so no 202 goes out
without its terminal event after its cursor. A new answer point must do the
same. The outcome lookup (`lookupRequestOutcome`) records the kept events too
before it answers `outcome_unknown`, and answers `pending` while the request's
event stays kept: a kept outcome is held, not lost. The stored op's replay
then re-emits its event unless the log just took a kept one of its request,
endpoint and type, which is after its cursor too (`cityOps.emitReplay`); one
for the other endpoint, whose waiter would never match it, does not stand in
for it. `engdocs/design/async-request-result.md` rules 8 and 9 are the client
contract.

The city's terminal events serialize on one lock, `cityOps.emitMu`, held
across the event log's `Record` in every `ops.content` mode, `off` included,
so the kept events are recorded in order and before any later one; the three
answer points take it before they check for kept events. A slow event log (an
exec provider runs its script once per event, under a 30-second timeout)
therefore holds back the city's other terminal events, and the answers to
same-key retries, until its `Record` returns
(`TestCityOpsSerializesTerminalEventsAcrossABlockedWrite`). Keep the lock's
critical section to the log write: no store, runtime or other blocking call
under it.

A recorded delivery ends, through its own timeout, inside its op's
`finish_by`, `legacyop.ExecutionBound` after its admission or adoption:
a recorded `/messages` at `sessionMessageAsyncTimeout`, a recorded `/submit`
at `sessionSubmitAsyncTimeout` (`TestMessageDeliveryTimeoutsEndInsideFinishBy`).
It closes its op from its result, or `failed{deadline_in_flight}` from the
timeout, with time left for that close to land. That time is a fixed margin,
`ExecutionBound` less the delivery's timeout (five minutes for `/submit`,
eleven for `/messages`), and the store I/O around the delivery spends it
first: `Admit` stamps `finish_by` before the delivery's timer starts, and a
delivery that runs to its timeout closes its op after the timer fires. Past
`finish_by` the controller's op sweeper, or a same-key retry, closes an op
still open `failed{deadline_in_flight}`, and the retry replays it; nothing
adopts it, since its message may have landed. A new recorded endpoint must
bound its delivery the same way.

Every execution of a recorded message op, an adopted one included, delivers
under the runtime delivery key `gc:<op id>:1`
(`legacyop.Admission.DeliveryKey`, threaded by `opDeliveryOf` into
`worker.MessageRequest`). An unrecorded request and `off` deliver unkeyed. An
adoption asks the runtime first (`MessageRequest.Redelivery`). When the
runtime already holds the delivery, the op closes `succeeded{legacy.delivered}`
with nothing sent: no turn sighting, no interrupt and no send (DES-T3I §14
item 8; `TestSessionContentOpsAdoptedInterruptNowThatLandedSendsNothing`). A
lookup that cannot tell fails the execution with nothing sent
(`TestSessionContentOpsAdoptionLookupFailureSendsNothing`).

## Session command ops

The synchronous session commands record ops through `runSyncOp`
(`session_control_ops.go`; DES-O0API §7): `/stop` and `/respond` under
`ops.control`, `/wake`, `/suspend`, `/kill`, `/close` and `/reset` under
`ops.lifecycle`. The op is admitted after the front-door checks (target
resolution, the always-on named close refusal, a named stop turn's turn
identity, unless the request's key already names a stop of that turn) and
before the command's effect, and closed from the command's result, every
step of it (a `/close?delete=true` closes its op after the delete), before
the answer. A sync command caches no in-flight answer, so a
same-key request while one runs is 409 `in_flight`; a retryable error leaves
a client-keyed op open for a same-key retry to adopt. A retry that reuses a
key on a session-scoped command (stop, kill, respond) reuses the first
request's scope (`priorSessionScope`), so a session that moved to another
generation or turn since replays the first request instead of refusing the
key as another request. A recorded `/respond` answers only the interaction
its op names, even when the request names none, so a respond adopted after
the agent raised another prompt, or a second click that read the same prompt
pending, closes `skipped{stale}` rather than approving a prompt nobody saw.
That holds only as far as request IDs tell prompts apart: where the runtime
derives them from the prompt's content (tmux hashes the tool and its input),
an identical prompt raised again carries the first one's ID and is answered
too. That is permanent for `/respond`: its op stays keyed by request id, with
its scope, payload (`respondOpPayload`) and at_least_once policy frozen, so
rows earlier builds recorded keep their request digest and a same-key retry
replays them. Only lane respond ops (`opjournal.RespondKey`) are keyed by the
interaction occurrence; do not re-key the legacy op to match. A
recorded kill is `gc session kill`'s operator kill (`worker.OperatorKill`,
through `Server.operatorKill`): it takes the runtime lease first (an open
row's as `gc session kill` does, `session.KillRuntimeLease`; a closed row's,
which keeps no lease record, the runtime name's flock alone,
`recordedKillLease`), checks its op's generation under it
(`OperatorKillRequest.Check`), and only then lends the lease to the fenced
kill: a kill whose generation is gone refuses `refused:target_moved` before
the fence, the stop or the asleep sync touches anything, and a row that does
not read is a retryable error, nothing killed. Unlike the API,
`gc session kill` takes no lease on a closed row. A
`/close?delete=true` op carries `{"delete":true}` as its payload
(`closeOpRequest`; a plain close carries none, as `gc session close`'s op
does), so the op's digest covers the delete: reusing a key across `/close`
and `/close?delete=true` is 422 `idempotency_mismatch` at admission, nothing
run, whether the key's op is still open after a retryable failure or closed,
in one process and after a restart. A close that also deleted its bead
closes its op `succeeded{via: legacy.deleted}`, and a replay never deletes. A
same-key close retry finds its op by the session ID only: by an alias or
session_name after a restart, or after a completed delete, the target no
longer resolves and the answer is 404. A `/reset` Idempotency-Key that
starts with `handoff:` or `request-restart:` is invalid
(`legacyop.ErrReservedKey`): `gc handoff` and `gc runtime request-restart`
record the same `restart{fresh}` request with their keys in those
namespaces, so `require` answers 422 `invalid_idempotency_key` and `auto`
runs the reset unrecorded. With the gates off nothing is read or
written beyond the legacy path, except that a `/stop` that names a turn is
stopped turn-scoped through the worker in every mode. A request `auto` could
not record runs exactly as in off (I8).

While `ops.control` records, `/stop` reads the session's turn live
(`resolveStopTurn`) only when its turns come from its runtime's protocol
(t3bridge, ACP), and interrupts a running turn turn-scoped; every other
runtime keeps the session-scoped legacy stop. On a protocol turn source it
never falls back to the unfenced session-scoped interrupt: no turn running,
or no runtime session bound, is nothing to stop, and a turn source that
cannot be read, or a turn-scoped interrupt the runtime refuses, sends
nothing (DES-T3I §14 item 3). Keep it that way: a late protocol interrupt
kills the next turn. A turn the stop read but the request did not name that
ends before the interrupt lands is `skipped{not_needed}`, 200; only a named
turn is refused `turn_not_current`. A turn's target key
(`stop:<gen>:<turn>`) replays only a stop that succeeded or found nothing to
do; one whose op failed hands the request to its own key (minted when it has
none), so a failed stop never uses up its turn. A named stop that the target
key's op answers leaves a row under its own Idempotency-Key, closed
`skipped{coalesced_into}` that op (`recordStopUnderKey`), so the key still
names the stop once the in-memory cache is gone. A stop that runs the op
writes the row before it sends anything, so the key names the stop however
it ends: failed, refused `turn_not_current`, or cut short by its API dying.
The row is admitted as the request would be under its key, and its refusal
is the stop's answer: in `require` a row the journal cannot record is 503
`op_journal_unavailable`, nothing sent and the target-key op left open, and
a stop answered from that op answers the 503 instead of its success, so the
client's retry binds the key. The exception is `auto`: a row its op store
does not record is logged and the stop goes on, its key naming nothing, as
an unrecorded request's (I8), so a later retry under the key with no turn
stops the turn running then. A retry under the key, with no turn or naming
the same one, follows the row back to the target key as a stop of the turn
the first request named: it adopts the op while it is open and replays it
once closed, and never stops the turn running then. Its reads of the
session's turns refuse only an adopted op, which runs, so a closed stop
replays after its session closed or its runtime stopped answering too. One
naming another turn is 422 `idempotency_mismatch`.

## Session commands under lane authority

When the running lane host executes a kind (`ConversationLaneKinds`, the
kinds `[ops] authoritative` listed at the controller's start), the session
command that produces it writes a lane op instead of running its legacy path
inline (`session_lane_ops.go`), and the `ops.*` gate does not govern it. A
request whose op cannot be written is 503 `op_journal_unavailable`, nothing
done. The API sends nothing to the runtime itself: the lane host runs the
op, and the API awaits it (`awaitLaneOp`, through the host's
`AwaitConversationOp`, which wakes at the end of each pass over the
conversation) for at most `laneControlAnswerBound` (45 s, under the Go
client's 60 s), then answers from the outcome as OJ-4 answers the same
outcome, or 409 `in_flight` naming the op and its key, under which a retry
adopts it.

- `/stop` (interrupt listed): a key that names a stop is its retry (an open
  lane op awaited, a closed op replayed, a coalesced row followed). A named
  turn keeps OJ-4's front-door checks, is keyed `stop:<gen>:<turn>` and
  scoped to the turn, and leaves an API-executed row under the request's
  own key closed `skipped{coalesced_into}` (`recordLaneStopUnderKey`); a
  target-key op that closed without success is retried under the request's
  key. Without a named turn the op is `session(gen)` under the request's key
  or a minted one, and the executor reads the turn. `skipped{stale}` is 200
  for an unnamed stop and 409 `turn_not_current` for a named one.
- Run cancel (interrupt listed): one op per live holder session, keyed
  `run-cancel:<run id>` with the run as causation, awaited together;
  `succeeded` is interrupted, a failure or an op still open is a failure,
  and `skipped` neither.

## The producer API and journal-row events

`POST`/`GET .../session/{id}/ops`, `GET .../ops/{op}` and
`POST .../ops/{op}/cancel` (`conversation_ops.go`; DES-O1 §11) write and read
conversation-lane ops directly. They do not go through `legacyop` or the
`ops.*` gates: a proposal is a lane op (`executor = lane`) or nothing. The
kind gate is the running lane host's, read through the optional
`ConversationLaneHost` State extension: the kinds `[ops] authoritative`
listed when the controller started, while its host runs, never the live
config, which a reload replaces without changing what the host executes.
Any other kind, or any kind while no host runs, is refused 409
`kind-not-enabled`; every `when` a message names is one the lane lowers
(`now` and the legacy `interrupt` as a deliver-now unit). Every refusal, the request's own validation included
(`opjournal.Store.Preview`), and the read of the event cursor the answer
carries come before the session is resolved, so a refused request, or one
whose cursor cannot be read, writes nothing and materializes no named
session, and no proposal is ever silently shadowed or parked. A replay
answers 200 with the same body as the first answer's 201; both are
declared. The op id, not
the `Idempotency-Key` LRU, is the idempotency: it derives from the session,
the asserted principal (`operator` or `t3:<environment id>`) and the client
key, so a retry across restarts returns the stored op and a different
request under the key is 409 `key-reuse`. `conversation:<id>` and
`gc:<producer>` principals stay reserved to gc's own producers.

Journal rows (bead type `op`) record no `bead.*` event. The controller's
cache-notification bridge (`cmd/gc/cache_notification.go`) derives
`op.changed` from an op row's post-image and `fact.recorded` from a fact
row's, and records nothing for runtime rows or DES-BX's `gc:event-fact` and
`gc:event-projector` rows. A one-shot CLI's write to a journal row (the
in-band hook pass, the CLI's shadow ops) journals nothing either: its
emitter (`cmd/gc/class_store_emit_oprows.go`) rings the controller's lane
doorbell with the row ids, `lane:<conversation>:<id>[,<id>...]`, whose
handler refreshes each row through the controller's sessions cache, so the
same bridge derives the event, for a close of a row the cache did not hold
too. A refresh a local write of the cache holds off (a graph apply holds its
fence for seconds), or that fails, the lane host retries off the socket
handler until the row is installed, for up to a minute; one a quiescent
city skips waits until a retry reads it, the quiescence not counted against
the minute. lane-status counts the rows waiting and those it gave up, and
the doorbell's answer counts the rows waiting. The in-band hook pass rings
once, at the end of the drain, after its output and bookkeeping, and adds
the plain doorbell after a Done only when no row ring's answer says it
poked the conversation; a ring the hook's deadline leaves no time for is
warned of. A row a CLI's write could not read back is known by its pinned id
(`opjournal.IsPinnedID`) and journals nothing either. A write no controller
answers is announced only partly: a running controller's next reconcile
announces a row it finds open and changed, but not a close of a row it did
not hold, and a controller started later primes silently; the row stays
authoritative, so a consumer that reconnects reads it. A row deleted out of
process that the cache held open is announced closed
(`failed{closed_externally}` for an op), as the next reconcile would announce it. No writer may author either event: `POST /v0/events`
and `gc event emit` refuse them (`events.IsJournalDerived`). Both payloads are pure functions of the row
(`OpChangedPayloadFor`, `FactRecordedPayloadFor`) and never carry the op's
payload text. The fields `conversationOpRecord` adds to `ConversationOp` are
set only on the op routes and `op.changed`; a session command's answer keeps
`conversationOpWire`.
