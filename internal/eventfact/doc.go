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
// # The recorder contract
//
// The worker-site recorder (D13's factRecorder) writes a fact only for a type
// events.AuthorshipOf calls AuthorWorker; Ledger and Controller types are
// dropped at the source. Its RecordAck returns a non-nil error for every
// event it does not write, a dropped type included, as events.AckRecorder
// requires: a caller that marks durable state on a nil ack
// (executionevent.EmitCurrent's step_defined marker) would otherwise
// suppress the fact for good. Encode refuses those types as well, and Decode
// refuses a row that carries one, since a box need not use the recorder.
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
// row the projector could not record. A string must be valid UTF-8, the
// payload's strings included, since the store's JSON would rewrite anything
// else.
//
// Decode is the projector's trust boundary: a box writes rows with its own
// bearer and need not go through Encode. So Decode accepts only a row Encode
// writes: a type a worker site may author (events.AuthorshipOf is
// AuthorWorker), every limit above, the identity rules, compacted payload
// JSON, the depends-on list exactly as Encode marshals it, canonical
// integers, and the time in canonical UTC RFC 3339 with nanoseconds.
// Anything else is ErrCorrupt. The type check reads the authorship registry,
// so it knows a type registered outside internal/events only in a binary that
// links the package registering it.
//
// # Schema evolution
//
// gc.ev.schema and gc.evproj.schema carry separate revisions (FactSchemaRev,
// CheckpointSchemaRev), so a change to one row kind leaves the other
// readable. A reader reads every revision from 1 up to its own and refuses a
// later one with ErrUnsupportedSchema, never skipping the row silently; the
// controller is upgraded before its boxes, so a projector meets a later
// revision only on a downgrade. Within a revision a field may only be added,
// and only as an optional gc.ev.* key whose absence keeps the meaning it had:
// a reader ignores a key it does not know and records the event without it.
// A change that a reader ignoring the key would get wrong (a new required
// field, a changed limit or form, a changed meaning) bumps the revision, and
// the reader that introduces it keeps decoding the earlier ones, since facts
// stay in the store for a day and the checkpoint row for good. Every
// events.Event field but Seq has a gc.ev.<json tag> key
// (TestCodecCoversEveryEventField fails until a new one does).
//
// # The checkpoint row
//
// One per city: <prefix>-evproj-<hash16("gascity/event-projector/v1", city)>,
// closed, no-history, type "op", label gc:event-projector, holding the
// projector's whole ledgerfeed.Checkpoint (its seq, the fingerprint of the row
// at that seq, and the lapse mark) in gc.evproj.* metadata. The projector
// creates it and moves it by a revision-guarded update (CheckpointMetadata),
// which leaves the city the create wrote.
//
// DecodeCheckpoint checks the row's shape, not its writer. The id derives
// from the mint prefix and the city, which a box writing facts in the same
// store knows too, so a row a box created or moved decodes like the
// projector's own, and its seq would move the city's resume point, dropping
// facts or replaying them. Before resuming from a checkpoint row, the
// projector (BX-4b) must verify the row is its own by a check that holds
// under D-TRUST, or record the gap as accepted under D-TRUST. The journal
// actor of the row's writes is no such check: until BD-8 it is what the
// writer asserts (BUILD-PLAN X5). Nor is refusing a row that exists before
// the projector's create, which stops a box before the create but not an
// update after it. A MAC over the gc.evproj.* fields, keyed by a secret
// boxes never hold, holds only if it also stops a box writing back an
// earlier checkpoint the projector wrote, which the journal records. An
// authenticator kept in the row is a required field, so it belongs in
// revision 1, before anything writes the row, or it costs a
// CheckpointSchemaRev bump.
//
// The row lives in the store whose journal the projector tails, and bd
// journals every update, no-history rows included, so each checkpoint write
// appends a journal row. A write after every page would make the next page,
// whose only row is that write, and so on forever. So after an applied page
// the projector writes the checkpoint only when the page advanced past a row
// that is not its own checkpoint write (DES-BX D14 step 6). A page holding
// only its own checkpoint rows moves the cursor in memory and writes nothing,
// so a quiet journal reaches a steady state with no writes. The cache-
// notification bridge (OJ-20), when it lands, must skip both gc:event-fact
// and gc:event-projector rows, for bead.* as well as op.changed.
//
// # Rows that do not decode
//
// Any worker site can write a row that Decode refuses with ErrCorrupt or
// ErrUnsupportedSchema. Such a row is refused, never skipped silently; the
// projector must not stall on it either, since one box could then halt fact
// projection for the whole city. It skips and counts the row loudly (a
// warning, a counter, gc doctor) and moves on.
package eventfact
