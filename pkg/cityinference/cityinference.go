// Package cityinference is Gas City's producer adapter for the neutral
// inference resource of the Beads Team Server v1 API.
//
// City is ONE OPTIONAL producer of inference records. A custom orchestrator
// with no City in the picture uploads the same records on its own credential,
// so nothing here may become a schema, an identity or an authority that a
// non-City producer would have to satisfy.
//
// Three facts shape every line below, and all three come from the frozen
// inference identity design (API-6.1):
//
//   - Identity is the triplet (tenant, session_id, upstream_req_id) and the
//     record ID is a one-way digest over it. This adapter derives the same ID
//     the server does, and offers it as a checksum, never as a choice.
//   - Only an exact-tuple match folds. Timestamp, model, vendor, token count,
//     cost and textual similarity are not comparators here; [Mapper.Fold]
//     refuses to evaluate them at all rather than returning "no".
//   - Step attribution is UNAVAILABLE by construction. There is no admitted
//     wire field for it and there is no field on [CityInvocation] that could
//     carry one, so the adapter cannot synthesize a step even by accident.
//     "Unavailable" is a claim this adapter makes out loud; it is not silence.
//
// What the adapter does NOT do is as load-bearing as what it does. It never
// produces a run, session, transcript record or artifact — those are
// [github.com/gastownhall/gascity/pkg/cityneutral]'s job, and this package only
// links to the server-minted Team IDs that producer read back. It never sends
// prompt text, completion text, transcript content or a raw provider payload:
// [Preflight] rejects the whole request before a byte leaves the process. It
// never emits an inference event or SSE stream, and it never touches Team
// Server storage directly.
//
// Rollback is per producer. Set [Producer.Disabled] and this adapter stops
// issuing requests while every acknowledged record stays exactly as the server
// accepted it, other City producers keep running, and a custom (non-City)
// inference producer against the same API is unaffected.
package cityinference

import "errors"

// Frozen operation IDs from operation_matrix_v6. They are spelled here so a
// reader can check this adapter's surface against the frozen matrix without
// leaving the package, and so the client seam cannot quietly grow a route that
// is not in the matrix.
const (
	OpCreateInference = "createInference"
	OpListInferences  = "listInferences"
	OpGetInference    = "getInference"
)

// KindInference is the resource kind in the server's idempotency namespace. A
// key minted for an inference may not be replayed into a run, a session or a
// transcript record, so the kind is part of the key preimage.
const KindInference = "inference"

// Bounds mirroring the server's validators, so an oversized field is refused
// here with a source field name attached rather than as a correlated 4xx.
const maxNativeIDLen = 200

// Refusals. Every one of them stops this producer with its checkpoint intact:
// nothing has advanced past the last acknowledged record, so an operator who
// fixes the source can restart and resume rather than reconcile.
var (
	// ErrChangedReplay is an invocation whose stable source identity was
	// already acknowledged but whose payload no longer matches. Sending it
	// would ask the server to rewrite accepted input, so the adapter stops.
	ErrChangedReplay = errors.New("cityinference: source identity replayed with changed payload")

	// ErrHeuristicFold is an offered correlation that rests on anything but an
	// exact identity tuple. It is a refusal rather than a false answer: a
	// producer that asked "do these two match on timestamp?" has already made
	// the category error, and answering the question at all would let the next
	// caller treat a near-miss as evidence.
	ErrHeuristicFold = errors.New("cityinference: fold may rest only on an exact identity tuple")

	// ErrSyntheticLinkage is an offered ordinal or step reference. This domain
	// asserts no order and has no step concept; the honest expression is
	// coverage class unavailable, never a fabricated link.
	ErrSyntheticLinkage = errors.New("cityinference: synthesized ordering or step linkage is inadmissible")

	// ErrAttemptRow is an upstream request ID in the err_ class. Those rows are
	// failed attempts that never produced a metered invocation; admitting one
	// would let an attempt stand as a call in every sum built over this domain.
	ErrAttemptRow = errors.New("cityinference: attempt row is not an invocation")

	// ErrContentLeak is outbound bytes carrying prompt, completion, transcript
	// text or a raw provider payload. Content lives behind transcript and
	// artifact content authorization and never rides an inference record.
	ErrContentLeak = errors.New("cityinference: outbound payload carries model content")

	// ErrCredentialLeak is outbound bytes carrying credential evidence. Key
	// identifiers are attribution tags at most, and never leave this process.
	ErrCredentialLeak = errors.New("cityinference: outbound payload carries credential evidence")

	// ErrNotFound is any canonical link this tenant cannot resolve. Foreign,
	// absent, wrong-kind and container-mismatched links all land here with the
	// same text on purpose: a caller that could tell them apart would have an
	// existence oracle over another tenant's records.
	ErrNotFound = errors.New("cityinference: linked record not found")

	// ErrIdentityDrift is a response binding a derived inference identity to a
	// different neutral Team ID than the checkpoint holds. That is either a
	// wrong-tenant credential or an undeclared reset; both are louder than a
	// retry.
	ErrIdentityDrift = errors.New("cityinference: derived identity resolved to a different neutral id")

	// ErrCoverageRaised is an accepted record whose coverage claims more than
	// this adapter offered. Coverage is server-derived and immutable, but a
	// producer that let a raised claim through would be lending its own name to
	// a completeness assertion it cannot support.
	ErrCoverageRaised = errors.New("cityinference: accepted coverage claims more than was offered")

	// ErrInvalidInvocation is a City invocation this adapter refuses to map.
	ErrInvalidInvocation = errors.New("cityinference: invalid City invocation")

	// ErrDisabled is the rollback state: the producer is off and issues no
	// requests. Acknowledged records and every other producer are unaffected.
	ErrDisabled = errors.New("cityinference: producer disabled")
)
