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
runs the reset unrecorded. A recorded `/respond`'s key in `respond:`
(`opjournal.IsRespondKey`) is invalid the same way: a lane respond op is
keyed there by the raising it answers, and an op's id derives from its key,
so a legacy respond under it would take the id of the operator's lane op
for that raising and refuse every answer to it `key_reuse` once respond is
listed. With the gates off nothing is read or
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
  lane op awaited, a closed op replayed, a coalesced row followed); one
  naming a turn the stop did not name, a session-scoped stop's included, is
  422 `idempotency_mismatch`, as under OJ-4. A session with no turn to stop
  as the lane's probe reads it (closed, suspended, or its runtime not both
  running and alive, read through the session's worker handle with its agent
  process names: `sessionHasNoTurn`) gets no lane op, which the lane would
  hold `not_running` until its `start_by`: a named turn is 409
  `turn_not_current`, nothing written, and a stop naming none is 200,
  recorded under its key as an API-executed row closed
  `skipped{not_needed}`, its causation `stop-found-no-turn`
  (`noTurnStopCausation`), which the request digest does not cover. The
  stop decides before it writes that row, so a request that finds the row
  still open (its close failed, its API died between the writes, or a
  concurrent request is between them) closes it `skipped{not_needed}` and
  answers 200, and one that finds it closed any other way (the sweep's
  `failed{deadline_in_flight}`) answers 200 too (`answerLegacyStop`); it
  reads no turn again. A runtime that cannot be observed
  (`runtime.ErrRuntimeUnavailable`, as a t3bridge snapshot outage reads) is
  not known to run no turn: its op goes to the lane, whose probe holds it
  until the runtime answers. Never read "no turn" there from the provider's
  `IsRunning`, which answers false when the runtime cannot be read. A named
  turn keeps OJ-4's front-door checks, is keyed `stop:<gen>:<turn>` and
  scoped to the turn, and leaves an API-executed row under the request's own
  key closed `skipped{coalesced_into}` (`recordLaneStopUnderKey`). A
  same-key retry that finds that row still open (its close failed, or its
  API died between the writes) closes it into the turn's op and answers from
  that op (`finishTurnStopRecord`), as does one that finds it closed
  `failed{deadline_in_flight}` by the sweep, when the turn's op is a lane
  op. Any other API-executed op still open, an OJ-4 stop left at the flip
  included, is a stop nothing runs while the lane runs stops: under the
  request's key it is 409 `op_already_failed`, asking for a new key, and
  under a turn's target key it is passed over, as a target-key op that
  closed without success is: the request stops the turn under its own key.
  Without a named turn the op is `session(gen)` under the request's key or a
  minted one, and the executor reads the turn. A skip is 200 for an unnamed
  stop and 409 `turn_not_current` for a named one (any skip but
  `not_needed`: `skipped{stale}`, or `skipped{conversation_closed}` once the
  session closed). The lane also closes an op of an older generation
  `skipped{stale}`, unrun, recording the generation that fenced it off
  (`opjournal.FencedOff`), which the executor's close of an op that found no
  turn does not. An unnamed stop whose op was fenced off carries on to the
  session's current incarnation within the same bound, keyed
  `stop-restarted:<stale op id>` (`carryOnStop`), whether it awaited the op
  or a retry found it closed (the first request answered `in_flight`): a
  lane op when that incarnation may have a turn, else an API-executed row
  closed `skipped{not_needed}`. The carry-on is scoped no earlier than the
  generation the lane recorded fencing the op off (`followUpScope`): the
  lane fences against its runtime row's incarnation when that leads the
  session row, so an op at the lagging session row's generation would be
  fenced off again, where one at the recorded generation waits for the
  lane's view to catch up. It is due by the fenced op's `finish_by`, so
  every carry-on of a chain is due by the stop's own, and the stop carries
  on only until then: a retry after that which finds no carry-on is 409
  `op_already_failed`, nothing proposed, as is a carry-on the journal
  refuses because that `finish_by` passed meanwhile. A retry follows the
  carry-on, however late, and an op the lane ran is answered as it closed,
  so a retry never stops a turn that began after the stop answered.
- `/respond` (respond listed): a key that names a respond is its retry when
  it asks the same answer of the same request id, answered through the
  occurrence the first request pinned the key to (an open op awaited, a
  closed one replayed), so a retry never answers an identical prompt raised
  later. A different answer or `request_id` under the key is 422
  `idempotency_mismatch`, nothing written, compared as OJ-4 compares it
  (`sameRespondRequest`: a legacy-shaped row's request digest under its own
  scope, a lane op's canonical answer). The front door reads the pending
  interaction and writes nothing on a refusal. The op answers the occurrence
  (`Store.RecordRaising`: the latest unresolved raising of the pending
  request id, or the next), keyed `opjournal.RespondKey`, `at_most_once`; a
  different answer to the same occurrence is 409 `key_reuse`. A request
  with a key pins the key to the occurrence before it proposes the op and
  pokes the lane (`pinLaneRespondKey`): a row of the legacy op's frozen
  shape (request-id scope, `respondOpPayload`, `at_least_once`) with the
  occurrence as its causation, which the request digest does not cover,
  closed `skipped{coalesced_into}` the lane op once the op exists, or
  `failed{refused:key_reuse}` when the occurrence's op records another
  answer; the legacy op is never re-keyed. The row's executor is `legacy`,
  which nothing adopts: OJ-4 adopting it would run the respond again, by
  request id, which can answer a later, identical prompt. A key that is the
  occurrence's own (`respond:<raised fact id>`, as a producer that saw the
  raising keys its answer) pins nothing: the lane op is the row under it,
  and a pin would take the op's id and refuse every answer to the prompt
  `key_reuse`. Any other key in that namespace (`opjournal.IsRespondKey`)
  that names no op names no raising pending now (one that ended, one of
  another request, one not raised yet, or none) and is 409 `no_pending`,
  nothing proposed or pinned: a pin would send its answer to the prompt
  pending now, which can be a later, identical one, and would take the id
  of the operator's op answering the raising the key names. A row that
  cannot be written is 503, no op proposed. A
  retry under the key never runs the front door or `RecordRaising` again:
  it proposes the pinned occurrence's op create-if-absent and answers from
  it, whether the row is closed, still open (its request died, or its close
  failed, which answers from the op all the same) or closed
  `failed{deadline_in_flight}` by the op sweeper.
  The answer carries the request's own key, or the op's for a request with
  none. The await is 20 s, cut short when the request ends; an op still open
  answers 202 with it, never `replayed`, a provisional answer the
  `Idempotency-Key` LRU does not keep (`withIdempotencyKeeping`), so a retry
  reads the op again. A failure offers a resend under a new key only when
  nothing was sent: an answer that may have reached its prompt
  (`maybe_delivered`, `ambiguous_delivery`) is 409 `op_already_failed` with
  no resend offered, since a resend would answer the next occurrence.
  A retry under the key of OJ-4's own respond op (`legacy:api`) that is
  still open, its API having died before respond was listed, is finished
  through the lane (`adoptLegacyRespond`): OJ-4's admission takes the op as
  it would (`Recorder.Admit`, through `serverOps.laneAdopt`, whatever
  `ops.control` says), so an op in flight in this process is 409
  `in_flight`, one past its `finish_by` closes `failed{deadline_in_flight}`
  and replays, and one inside it is adopted. The lane then answers only the
  request id the op names, in the op's generation, through the lane op of
  its occurrence, into which the adopted op closes `skipped{coalesced_into}`;
  the API never answers the runtime itself while respond is listed. Before
  that lane op is proposed, the adopted op names the occurrence as its
  causation (`opjournal.Store.SetCausation`), as a pin does, so from then on
  it is read as a pin: a retry under the key, and a rollback, follow it to
  the occurrence's lane op whether its close landed, never landed (its API
  died once the lane answered) or was the op sweeper's
  `failed{deadline_in_flight}`, and nothing adopts it again. A name that
  cannot be written leaves the op open, nothing proposed, 503. A
  front-door refusal closes the op as OJ-4 closes it (no prompt pending,
  another one, or another generation is `skipped{stale}`), and an occurrence
  whose op records another answer closes it `failed{refused:key_reuse}`,
  which replays 409 `key_reuse`. A closed OJ-4 op replays as OJ-4 replays
  it.
  While OJ-4 answers `/respond` again (respond unlisted: a rollback, or no
  lane host runs), a key the lane path wrote or answered a respond under (a
  lane op, a pin, OJ-4's op the lane adopted, or OJ-4's op the lane
  closed) answers from that lane op
  (`replayLaneRespond`), never by running the respond, whatever
  `ops.control` says: the key's row is read from the lane path's own store,
  which no gate governs, since `off`, and `auto` by its builtin default over
  a store that is not SQLite-backed (a native Dolt one, which a lane host
  runs over), read nothing through OJ-4. The read happens only over a
  sessions store that can apply guarded batches, which every lane host
  needs, and that names an id namespace, without which no op row exists.
  An op store that otherwise does not open, and a read that fails, is 503
  `op_journal_unavailable`, nothing done: `opjournal.NewStore` reports
  storage it cannot reach as it reports a store that cannot fence writes,
  so its error does not say the lane never wrote there. A closed op
  replays under the request's key, one still open is 409 `in_flight`, and
  a pinned occurrence whose op records another answer, like OJ-4's op the
  lane closed `failed{refused:key_reuse}`, is 409 `key_reuse`. Nothing
  proposes a lane op then. A pin, or an adopted OJ-4 op, whose request ended
  before it proposed the op had the lane send nothing, and answers from
  itself: `in_flight` until the op sweeper closes it at its `finish_by`,
  then that close.
- `/submit` and `/messages` (message listed): accepted by
  `lanesubmit.Accept` (`internal/session/lanesubmit`), the one acceptance
  `gc session submit`'s local fallback shares; change an acceptance rule
  there, never in a projection. The API keeps its answers
  (`answerLaneMessage`), its events and its poke, and maps `Accept`'s typed
  refusals onto the wire (`laneAcceptError`). A lane message op under the
  operator, keyed by the Idempotency-Key or a minted one, with
  `lanesubmit.Policy` (default and follow_up at the boundary,
  interrupt_now as a deliver-now unit, which never hard-restarts), proposed
  after a configured named session's row is materialized. `resume: true`
  consumes the session's operator hold once the op is proposed, and
  nothing else, recording the session's wake in the same write
  (`session.Store.ResumeHeld`): no wait is canceled and no will-not-start
  verdict refuses it (the result reports one). It consumes only a hold
  stamped at or before the op's `created_at`, so a hold the operator
  places while the message waits stands through every retry and the CLI
  fallback's resume. A proposal
  that fails leaves the hold in place. A closed, closing or archived
  session is refused 409 and one draining under the operator's suspend
  409 `session_stopping` before anything is written
  (`session.Store.ResumeHeldRefusal`), so the key stays free for a retry.
  A resume refused once the op is written (the row changed since that
  read, a revision fence lost on every attempt, a store error) cancels the
  op the request wrote (it created it, or its create committed and
  answered an error under a key the request minted) before the lane is
  rung, and the error says the key is spent: a retry under it reports that
  op instead of sending again. It offers a resend under a new key only when
  the cancel closed the op: an op the lane host's scan took first (the
  cancel only requested), or whose cancel failed, may still deliver the
  message, and the error says so instead (`laneKeySpent`). Never reuse
  `wakeSession` there: it cancels
  the session's waits. The 202 follows the commit, `request_id` the op
  id, and the acceptance is recorded before the lane is rung (a refused
  acceptance cancels the op; a cancel only requested, the lane host's scan
  having taken it, is logged). The terminal event
  follows `laneSubmitSettle` (5 s) later through `cityOps.emit`: the
  result, queued false, for an op closed succeeded; `request.failed`
  (`messageReplayErrorCode`) for one closed otherwise; the result, queued
  true, for one still open, its `will_start` read alone (`laneWillStart`:
  `session.WakeRequestVerdict` on the row as the lane host reads it, a
  dormant row as not running and any other with its runtime's reading
  (`laneWakeVerdict`), and the `WakeStartRefuser` reading, no wake written
  and no poke, since the lane host's wake request does both). A failed read
  of the op gets the result from the op as accepted, queued true: a
  `request.failed` would invite a resend of a message the lane may still
  deliver, and the op's fate is `op.changed`'s. A held
  session's reason offers the same-key `resume: true` resend, under the
  `idempotency_key` of the request's 202 (the client's or a minted one),
  when that resend lifts every hold (`session.ResumeHeldPreview`: a
  suspend stamped by the op's `created_at`); when it lifts that suspend
  and other holds stand, the reason names the suspend and those holds and
  offers the resend for the suspend; when a drain is stopping the session,
  whose stop refuses the resume 409 `session_stopping` until it is done,
  the reason names the holds and the drain and offers the resend once the
  drain is done (`laneDrainingReason`); and otherwise it names the holds
  the resend leaves standing (`session.Disposition.ConsumeHolds`: a wait,
  a quarantine, a heartbeat, a later suspend); a refusal carries no
  `resume: true` hint, as resume starts nothing under the lane. A
  `/submit` result says the intent the op's policy runs
  (`submitIntentForWhen`), as its replay does: follow_up is the default
  request on the lane, so under one key either answers from the other's
  op. A same-key retry of a closed op replays it
  (`storedReplayAnswer`); an open lane op is accepted again
  (`answerLaneMessage`: its acceptance recorded, the lane rung and its
  terminal event emitted after the retry's cursor, so its `request_id`
  resolves, 409 `in_flight` while a terminal event of the op's stays
  kept), its `resume: true` run again first, since the request that wrote
  the op may have died before its own resume (a hold stamped after the
  op's `created_at` stands); a resume refused there leaves the op open, as
  an earlier answer may have accepted it. An open legacy op is 409
  `in_flight`. A legacy op the key names from before the flip answers only
  the request it recorded (its digest in the legacy shape,
  `lanesubmit.LegacyPolicy`). The `Idempotency-Key`
  LRU keeps no answer of a request the lane accepts
  (`withMessageIdempotency`, which reads `laneExecutes` once per request
  and hands that reading to `acceptMessageOp`), so a same-key retry
  reaches the op on the API process that answered the first request too:
  a resend that adds `resume: true`, or sends follow_up for default, is
  answered from the op, where the LRU, which hashes the whole body, would
  refuse it 422 or replay the first 202 without running its resume. The
  op answers a same-key retry whatever the session waits on: only a
  request no op under its key answers is refused 409 `pending_interaction`,
  writing nothing (`Accept`'s front-door step, the API's
  `rejectPendingInteraction` over `lanesubmit.CheckPending`, whose look
  is bounded at 5 s and refuses a look that fails, times out or panics
  503 `pending_check_unavailable`), so a retry of a message that
  raised an approval is not told to answer it before sending text its 202
  already accepted (request_id rule 8). The
  LRU still holds the key while the request runs (a concurrent same-key
  request is 409 `in_flight`), and still replays an answer it kept before
  the lane host ran, which the journal may not have recorded. A
  message over `opjournal.MaxInlinePayload` (16 KiB) is 422
  `payload_too_large`. extmsg member notices are lane notices from
  `gc:extmsg`, their inbound text cut to fit that limit (`extmsgNotice`). A
  provider session create's initial message (`POST /sessions`, `kind:
  provider`, with `message`) is an operator lane message op at the boundary
  under a minted key (`deliverInitialMessage`), refused 422 before anything
  is created when it is over the limit (`checkInitialMessage`); only a
  failed proposal fails the create and rolls it back. An agent create's
  message rides its first start's prompt
  (`template_overrides.initial_message`) and is no lane op.
- Wait withdrawal (`withdrawQueuedWaitNudges`): a wait whose nudge is a lane
  notice holds its op id as `nudge_id`; an op id (`opjournal.IsOpID`) is
  canceled in the op journal as `gc:wait`, any other id goes to the nudge
  queue as before.
- Run cancel (interrupt listed): one op per holder session with a turn to
  stop, or that may have one, as for `/stop` (`sessionHasNoTurn`), keyed
  `run-cancel:<run id>:<gen>` (one per holder incarnation, so a holder that
  restarted gets its own) with the run as causation, awaited together;
  `succeeded` is interrupted, a failure or an op still open is a failure,
  and `skipped` neither, but an op the lane fenced off after its holder
  restarted, as for `/stop`, is followed by the new incarnation's op when
  that may have a turn, scoped no earlier than the generation that fenced
  the op off, as a `/stop` carry-on is, awaited within the same bound.
  Every cancel interrupts each live holder again, as legacy's `StopTurn` of
  every live holder does: an op that closed before a cancel reached it is
  followed by the next, keyed
  `run-cancel:<run id>:<gen>:retry:<that op's id>`, so a holder's ops form a
  chain (`proposeRunCancelInterrupt`), which a cancel walks to its first op
  still open, awaiting it, or else extends. Overlapping cancels share one
  open op per holder.

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
