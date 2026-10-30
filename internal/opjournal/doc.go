// Package opjournal is the conversation operation journal's O0 schema and
// shadow store: op and fact rows, the closed kind registry, pinned op ids, the
// bead codec, and a store over the sessions-class beads store that holds the
// conversation rows. In O0 it decides and executes nothing: API and CLI write an
// op before running their legacy path; lanes arrive in O1 and O2.
//
// The design is engdocs/design/conversation-operation-journal.md §3.1–3.6,
// §5.2 and §5.5 O0, with its recorded decisions and critic corrections. That
// document is not on main yet; until it lands, the rules below and the tests
// that pin them are the in-tree contract.
//
// # Rows
//
// A conversation is the durable entity, a session (generation) one execution of
// it, a context epoch its transcript lineage. L1 rows are beads of type "op"
// stored with the conversation row (coordclass routes them to the sessions
// class): op rows, one per request (labels gc:op, gc:op-conv:<conversation>);
// fact rows, one per occurrence that is never coalesced (gc:fact,
// gc:fact-conv:<conversation>), written closed or, on a store that creates
// rows open, closed right after the create (RecordFact); and one runtime row
// per conversation (gc:rt, gc:rt-conv:<conversation>) at the pinned id
// RuntimeRowID, which holds the lane cells (gen, epoch, seq, lease, and the
// interaction occurrence the lane last saw pending) and the runtime and turn
// observations in gc.rt.* metadata; lanes and observers
// update it in place under revision guards (RuntimeRowPatch builds the
// update). Rows are no-history and never ephemeral. Type "op" is
// ready-excluded, rows carry no gc.routed_to, and an op's assignee is only ever
// a lane lease <lane>@<conversation>#<gen>#<nonce>@<deadline>, so ops never
// reach Ready or work queries. A lease is at most 255 runes, bd's assignee
// limit, and a conversation id at most 161 bytes, so that the longest lane
// lease fits. Fields live in gc.op.* metadata. Type "op" is
// coordclass.ControllerOwned (decided in OJ-2a, before any producer writes a
// row): the generic bead API never writes an op or fact row; only this
// package's store, through the producers' and lanes' verbs, does.
//
// # Status and outcome
//
// gc.op.status moves only by revision CAS, never by bd custom statuses:
// submitted → accepted|executing|closed; accepted → executing|settling|closed;
// executing → settling|closed; settling → closed. Lifecycle kinds never enter
// executing: they are lowered (accepted), decided (settling), settled. The
// conversation lane (OJ-10) adds two edges that only lane code writes:
// executing → accepted (a release; the op keeps its order position) and
// executing → executing (a re-take of an op whose lease expired, with
// attempt+1). An accepted conversation-lane op is therefore promoted but not
// in flight: a lowered deliver-now message or a released op. The bead
// is open until the op closes; metadata is authoritative. A bead closed outside
// the journal reads closed failed{closed_externally}, and the next Close,
// Cancel or Transition stores that outcome. A closed op has one outcome: arm
// succeeded|failed|skipped|canceled plus an open reason; an unknown failed or
// skipped reason reads failed{other}. The conversation lane takes and
// settles ops only through guarded batches (beads.GuardedBatcher); see
// "Conversation lane" below.
//
// # Conversation lane
//
// ConversationLane.Pass is the conversation lane's core (DES-O1 §4, OJ-10).
// A pass reads the conversation's open ops (ListOpenTolerant, quarantining
// what it cannot decode), its runtime row and each after target; closes what
// the rules decide (planRules: cancel, deadlines, poison at MaxFailures,
// stale scope, failed dependencies, a closed conversation); and, when a Take
// is possible, probes the executor and takes one batch (planTake):
// control before content (I10), urgent content before normal, ordered by
// OrderKey with a per-principal client_seq fix-up, at most one prompt,
// MaxBatchOps ops and MaxBatchPayload bytes. Take is one guarded batch: the
// runtime row's lease cell, seq and incarnation plus every op's lease,
// attempt, lane_seq and delivery key, so one Take is in flight per
// conversation (I3) and a stale read loses its batch (I9); an attempt that
// loses a race re-reads the conversation and probes again. No effect runs
// before the Take is known durable (I5); an unknown batch error is resolved
// by re-reading the lease and then each op, retrying a failed read on
// Done's backoff while the lease lasts, and the Take goes on with the
// ops as re-read (an op that closed in that window is dropped and the rest
// are released without the effect). Done is one guarded batch that accepts
// (the delivery record and the turn link CorrelateTurn decides), releases
// (with retry_at, failures and maybe_delivered) or rejects; after a refusal
// or an unknown error it re-reads every row it still has to write. Only a
// missing or unknown ack makes a Delivered result Unknown: once the runtime
// acked, a defect in another recording field is clamped or dropped, the
// outcome records DegradationExecutorContract and Pass returns the breach,
// so the effect never runs again for it.
//
// A content op whose lease expired is re-taken with its delivery key, so a
// deduplicating runtime replays it; on any other runtime the redelivery
// ends with RedeliveryNote (D-NOTE). A control op carries no key; Op.ID is
// its idempotency key. A released op is re-keyed only when the runtime
// refused its key on its last attempt (L3), and a runtime refuses only a
// key that never landed (Rejected), so the send under the new key is no
// duplicate. Delivery keys are
// <head op id>:<the head's lane_seq at the minting Take>. A deduplicating
// runtime gets a maybe-delivered group whole or not at all (a member that
// closed leaves it; a member that cannot go yet, including one the rules
// keep out of its class, such as one whose after target cannot be read on
// that pass, holds the head as a replay_group waiter), so every send under a
// key is a subset of every earlier send under it (L2); any other runtime may
// get a group in parts. A replay group whose open members are all
// after_turn riders goes without a starter. A fresh op the probe rules out
// closes (if_idle while busy: skipped{busy}; steer with fallback_fail, or a
// respond the runtime cannot perform: failed{unsupported}); an op that may
// have been delivered (a re-take, or one released after an Unknown result)
// waits instead, busy, to be replayed, and start_by bounds
// the wait (failed{deadline_in_flight}). An after_unreadable wait names
// the cause in its Detail; it retries after TransientBackoffBase when the
// store failed to read the target, and waits with no timer, as after_pending
// does, when the target was read but does not decode. An incarnation wait
// (the view is behind the runtime row, or the probe's fence disagrees with
// it) retries after TransientBackoffBase too: nothing rings when they agree.
// A pass's Next is never the instant it planned at. Delivered text names
// no identifier (I14). A lane store must apply guarded batches
// (NewLaneStore); there is no production caller until OJ-12.
//
// # Deliver-now
//
// A deliver-now message M (when: now, or the legacy interrupt) is lowered
// by the rule phase into its derived interrupt I in one guarded batch: I
// created at NowInterruptID (client key <M>:interrupt, M's producer,
// causation M, at most once, on_failure fail, scoped to M's now_gen, or the
// effective generation, and now_turn, no after), M moved to accepted. A pass
// that lowers reads again before any Take. While both are open they are one
// unit: a control entry in M's arrival order, taken [I, M] under one lease
// (TakeNow), and while a unit is open no content is taken (U-ORDER),
// whatever either op waits on, but content the probe admits past a content
// block every open unit waits on; an interrupt this build cannot decode
// holds nothing. A lowered M's after is not read again. The
// unit's effect is direct when the probe shows no turn to replace (I closes
// skipped{stale} with no call and M goes at once); otherwise the
// interrupt's replacement step (InterruptCall.ForReplacement), then M as a
// replacement delivery only when the step settled at a boundary
// (Settled.After) with time left, linked from the step's read so the
// replaced turn is never its candidate; or one FusedNowExecutor command. A
// targeted unit whose turn is not running, and a runtime that is not
// running, close I skipped{stale} at plan time; a unit never reports Busy.
// A message the effect did not send is released with no gate and no failure
// counted (last_error not_attempted). Once I is closed, M is urgent content
// that goes alone at a boundary, marked interrupt_unsupported after
// failed{unsupported} and interrupt_failed after any other failure, unless
// its delivery has its own mark. An open I whose M is closed or gone is an
// orphan: it closes canceled (cancel_by lane:cascade) when M was canceled,
// which replaces the rule close of an I that never ran, else
// skipped{not_needed}. A partner row that cannot be read waits
// now_unreadable. An open derived interrupt pairs by identity, not by
// namespace: a message that moved here with its id from another namespace
// forms its unit with it wherever a lane pinned it. A closed one is read
// only where this lane or the message's own namespace pins it, so one a
// lane of a third namespace pinned (a lowering on a stale mint prefix)
// reads as missing here, and its message goes marked interrupt_failed
// whatever its outcome. A message not yet lowered whose derived interrupt
// already exists, open anywhere or found at either of those ids, is refused
// (now_interrupt_taken) and never lowered beside it: only a lowering mints
// that key. An interrupt keyed as a derived one whose causation is another
// namespace's op id with no row here is plain.
//
// RelowerNowUnits re-pins under this store's prefix the derived interrupt
// of every open deliver-now unit whose interrupt carries another
// namespace's prefix, as a namespace move (gc storage migrate) leaves them.
// One guarded batch per unit creates the interrupt at NowInterruptID as a
// copy of the row it reads and closes that row
// skipped{coalesced_into: <copy>} under the revision it copied; the
// message is not written. Only a unit whose two ops are open and not in
// flight, with one open derived interrupt and a free id here, moves: an
// open derived interrupt never reached its runtime, so its copy runs
// nothing twice. A forward pointer is never a unit's outcome. It is
// idempotent and every batch is all or nothing; run it with the store's
// writers stopped.
//
// # Executor and open rows
//
// gc.op.executor says who executes an op: lane, legacy, legacy:api or
// legacy:cli; a row without it reads legacy and any other value is
// ErrUnsupportedValue. Propose writes it and nothing changes it; it is outside
// the request digest, so a retry after a mode flip returns the stored op.
// Lanes take, lease and settle only lane ops. A legacy op carries no lease
// and never leaves submitted until it closes (the codec refuses either, and
// Transition and Cancel refuse an open legacy op); it is closed only by its
// legacy path, an adopter (Adopt, legacy:api ops only) or the sweeper, at
// finish_by (CloseExpired).
// gc.op.open holds the conversation id exactly while the op is not closed (the
// close patch blanks it), so ListOpen is one keyed read that returns open rows
// only; the gc:op-conv label stays for history reads. ListOpenTolerant serves
// lanes, quarantining a row it cannot decode instead of failing the read.
//
// # Identity
//
// id = <prefix>-op-<base32(sha256(lp("gascity/op-id/v1") lp(conversation)
// lp(principal) lp(client_key)))[:16]>, lp a uvarint length prefix, prefix the
// mint prefix of the conversation row's store (never parsed from the id).
// Target keys: stop:<gen>:<turn> (InterruptKey), and respond:<raised fact
// id> (RespondKey) for a lane respond, keyed by the interaction occurrence it
// answers, never the runtime's request id, which can be a content hash: an
// occurrence is <request id>#<n>, the nth raising of that request id in its
// generation (RecordRaising). A lane respond is at most once. The first
// accepted answer wins: a close that may have answered ends the occurrence,
// a close with nothing sent ends it once no other open answer names it
// (occurrenceEnd, re-read once the close lands: endClosed), each end in the
// batch that writes its close (endItem), and a look that shows its prompt
// gone ends it as well, even before any answer names it (clearGone, which
// reads the runtime row's pending cell), so a prompt still showing is the
// next occurrence. A legacy respond
// keeps its client key. The digest covers the defaulted kind, policy (a
// deliver-now's now_gen and now_turn included), scope, after and payload
// (≤16 KiB): same id and digest returns the existing op; another digest is
// ErrKeyReuse, refused at the front door and never written. The principal is a stable logical id, never a
// bearer, credential or box epoch; the actor is an open class string.
//
// # Kinds (class · lane · default scope — Primitive Test: atomic, gains with models, transport)
//
//	message   content   conversation  conversation  create-if-absent delivery; producers decide what to say
//	interrupt control   conversation  session+turn  turn-fenced, keyed by target; the requester decides to stop
//	respond   control   conversation  session+occ   at most once; first accepted answer wins; a principal answers
//	create    lifecycle lifecycle     conversation  materializes a captured launch spec; a generator decides
//	wake      lifecycle lifecycle     conversation  one watermark; a generator (the planner too) decides
//	suspend   lifecycle lifecycle     conversation  sets an operator hold; the requester decides
//	resume    lifecycle lifecycle     conversation  clears the hold; the requester decides
//	restart   lifecycle lifecycle     conversation  mints a session (fresh: new epoch); agent or operator decides
//	kill      lifecycle lifecycle     session       fenced stop with grace; the requester decides
//	close     lifecycle lifecycle     conversation  ends the conversation; the requester decides
//
// message.when: boundary (default), after_turn, interrupt (legacy), if_idle,
// now (lowered into a derived interrupt that the lane executes with the
// message as one unit; no after link), steer (runtime-declared, else
// on_unsupported). A now message may name its target, now_gen and now_turn,
// valid only with when: now. Stop is interrupt plus the content_hold cell.
//
// # Rules
//
// A producer-declared after (handoff's message{after: restart}, for one) is
// satisfied only by succeeded; otherwise the dependent closes
// skipped{dependency_failed}. Deliver-now uses no after. Order:
// (max(not_before, created_at), created_at, id) via CompareOrder, the interim
// key until gc reads bd's arrival_seq (BD-5, OJ-10a). arrival_seq is
// platform-owned: gc reads it from the row column, never from gc.op.*
// metadata, so Op.ArrivalSeq stays zero until then. Cancel: before Take (or,
// for lifecycle, before lowering) closes canceled; executing or lowered
// records the request and the outcome says who won; settling is
// already_decided, closed already_terminal, and neither records the cancel.
// On conversation close, open ops close
// skipped{conversation_closed} except settling ops and the close op. Facts:
// session.started/ended, turn.started/ended, interaction.raised/resolved,
// create-if-absent by occurrence; planner level changes stay cells and reach L1
// as session.ended{reason}. This package never deletes an op, fact or runtime
// row; its two deletes, in Propose and RecordFact, remove a row a store minted
// under its own id instead of the pinned one (ErrPinnedIDNotHonored).
// Retention, archive and permanent tombstones are later milestones (the
// design's OJ-29, OJ-30 and BD-6).
package opjournal
