// Package eventfact is the event-fact row: how an event recorded on a worker
// site (a box) reaches the city's event log. A box process never appends to a
// local events.jsonl; each event it may author (events.AuthorshipOf is
// AuthorWorker) becomes one event-fact row in the sessions-class store, and
// the controller's projector, tailing that store's journal, records it through
// the controller's own recorder, which assigns its Seq. This package owns the
// row: its schema, its pinned id, its codec, its decode from a journal row,
// and the projector's checkpoint row. It writes nothing; the recorder and the
// projector (cmd/gc) do the I/O.
//
// The design is engdocs/design/des-box-daemon-v0.md D13, D14, §3.4 and §3.5
// (BX-4a), under the user decision D-TRUST: until bd principals (BD-8) a
// fact's actor, subject and session are what the box asserts.
//
// # The fact row
//
// A closed, no-history row of type "op", the journal row type, so it is
// ready-excluded, routed to the sessions class and controller-owned like an op
// row, and never ephemeral. Labels gc:event-fact and
// gc:event-fact-conv:<session>. The opjournal codecs decode only gc:op, gc:fact
// and gc:rt rows, so they refuse it, and Decode refuses theirs. Fields live in
// gc.ev.* metadata: the writer's identity (session, gen, origin, n), then the
// event's own fields, each named after its events.Event JSON tag (type, ts,
// actor, subject, message, payload, run_id, session_id, step_id,
// depends_on_step_ids). gc.ev.session is the session the writing box serves,
// the row's key; gc.ev.session_id is the event's SessionID, whatever it names.
// Empty fields are absent. Seq is never stored.
//
// # Identity
//
// id = <prefix>-ev-<hash16("gascity/event-fact/v1", session, gen, origin, n)>
// (opjournal.PinnedID), prefix the sessions store's mint prefix. origin is
// proc:<nonce> or daemon:<nonce>, a nonce the writer draws at start, and n
// counts that writer's facts from 1, so a sealed batch retried after an
// ambiguous failure re-creates the same ids and a duplicate create is refused
// rather than recorded twice.
//
// # Limits
//
// The payload is stored compacted; above MaxPayload bytes it is omitted and
// gc.ev.payload_dropped records its size, so the event still reaches the log.
// Every other field has a hard limit, and Encode refuses a fact over one
// (ErrInvalid): the writer drops that event with a reason rather than store a
// row the projector could not record.
//
// # The checkpoint row
//
// One per city: <prefix>-evproj-<hash16("gascity/event-projector/v1", city)>,
// closed, no-history, type "op", label gc:event-projector, holding the
// projector's whole ledgerfeed.Checkpoint (its seq, the fingerprint of the row
// at that seq, and the lapse mark) in gc.evproj.* metadata. The projector is
// its only writer and moves it by a revision-guarded update after each page.
package eventfact
