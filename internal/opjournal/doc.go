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
// RuntimeRowID, which holds the lane cells (gen, epoch, seq, lease) and the
// runtime and turn observations in gc.rt.* metadata; lanes and observers
// update it in place under revision guards (RuntimeRowPatch builds the
// update). Rows are no-history and never ephemeral. Type "op" is
// ready-excluded, rows carry no gc.routed_to, and an op's assignee is only ever
// a lane lease <lane>@<conversation>#<gen>#<nonce>@<deadline>, so ops never
// reach Ready or work queries. A lease is at most 255 runes, bd's assignee
// limit. Fields live in gc.op.* metadata. Rows are not yet
// coordclass.ControllerOwned, so the generic bead API can still write them; the
// design's DES-O0API decides whether they join it before OJ-2 writes real rows.
//
// # Status and outcome
//
// gc.op.status moves only by revision CAS, never by bd custom statuses:
// submitted → accepted|executing|closed; accepted → executing|settling|closed;
// executing → settling|closed; settling → closed. Lifecycle kinds never enter
// executing: they are lowered (accepted), decided (settling), settled. The bead
// is open until the op closes; metadata is authoritative. A bead closed outside
// the journal reads closed failed{closed_externally}, and the next Close,
// Cancel or Transition stores that outcome. A closed op has one outcome: arm
// succeeded|failed|skipped|canceled plus an open reason; an unknown failed or
// skipped reason reads failed{other}. O1 lanes Take with a guarded update
// expecting assignee "" (or the expired lease) and finish with one update that
// closes op and bead expecting their own lease; until bd batch guards exist the
// revision CAS subsumes both.
//
// # Identity
//
// id = <prefix>-op-<base32(sha256(lp("gascity/op-id/v1") lp(conversation)
// lp(principal) lp(client_key)))[:16]>, lp a uvarint length prefix, prefix the
// mint prefix of the conversation row's store (never parsed from the id).
// Target keys: stop:<gen>:<turn>, respond:<gen>:<request_id>. The digest covers
// the defaulted kind, policy, scope, after and payload (≤16 KiB): same id and
// digest returns the existing op; another digest is ErrKeyReuse, refused at the
// front door and never written. The principal is a stable logical id, never a
// bearer, credential or box epoch; the actor is an open class string.
//
// # Kinds (class · lane · default scope — Primitive Test: atomic, gains with models, transport)
//
//	message   content   conversation  conversation  create-if-absent delivery; producers decide what to say
//	interrupt control   conversation  session+turn  turn-fenced, keyed by target; the requester decides to stop
//	respond   control   conversation  session+req   first answer wins; a human or granted principal answers
//	create    lifecycle lifecycle     conversation  materializes a captured launch spec; a generator decides
//	wake      lifecycle lifecycle     conversation  one watermark; a generator (the planner too) decides
//	suspend   lifecycle lifecycle     conversation  sets an operator hold; the requester decides
//	resume    lifecycle lifecycle     conversation  clears the hold; the requester decides
//	restart   lifecycle lifecycle     conversation  mints a session (fresh: new epoch); agent or operator decides
//	kill      lifecycle lifecycle     session       fenced stop with grace; the requester decides
//	close     lifecycle lifecycle     conversation  ends the conversation; the requester decides
//
// message.when: boundary (default), after_turn, interrupt (legacy), if_idle,
// now (lowered into interrupt + message{after}), steer (runtime-declared, else
// on_unsupported). Stop is interrupt plus the content_hold cell.
//
// # Rules
//
// after is satisfied only by succeeded; otherwise the dependent closes
// skipped{dependency_failed}. Order: (max(not_before, created_at), created_at,
// id) via CompareOrder. Cancel: before Take (or, for lifecycle, before
// lowering) closes canceled; executing or lowered records the request and the
// outcome says who won; settling is already_decided, closed already_terminal,
// and neither records the cancel. On conversation close, open ops close
// skipped{conversation_closed} except settling ops and the close op. Facts:
// session.started/ended, turn.started/ended, interaction.raised/resolved,
// create-if-absent by occurrence; planner level changes stay cells and reach L1
// as session.ended{reason}. This package never deletes an op, fact or runtime
// row; its two deletes, in Propose and RecordFact, remove a row a store minted
// under its own id instead of the pinned one (ErrPinnedIDNotHonored).
// Retention, archive and permanent tombstones are later milestones (the
// design's OJ-29, OJ-30 and BD-6).
package opjournal
