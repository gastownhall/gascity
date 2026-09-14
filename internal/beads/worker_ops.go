package beads

import "errors"

// ErrClaimUnsupported reports that the resolved store exposes neither claim
// shape, so a caller asking it to claim on a named assignee's behalf has
// nothing to delegate to. It is the claim-side sibling of
// ErrConditionalReleaseUnsupported: an absent optional capability, not a
// failed write.
var ErrClaimUnsupported = errors.New("assignee-scoped claim unsupported")

// ErrCommentUnsupported reports that the resolved store cannot append a
// comment. Comments are an optional store capability like the conditional
// writers above: the in-process stores keep no comment log, so only the
// ledger-backed stores implement it.
var ErrCommentUnsupported = errors.New("bead comments unsupported")

// ErrEmptyComment reports a comment request whose text was empty or all
// whitespace. Refused at the store rather than written, so a caller that lost
// its message body learns that instead of seeing a successful no-op.
var ErrEmptyComment = errors.New("comment text is empty")

// AssigneeClaimer is the claim capability in the shape the in-process stores
// already expose (SQLiteStore.Claim): the assignee rides on the call, so one
// store instance can claim on behalf of many callers. This is the shape a
// server-side claim needs, because the caller's identity arrives per-request
// and is not a property of the store the process opened at startup.
type AssigneeClaimer interface {
	Claim(id, assignee string) (Bead, bool, error)
}

// ActorClaimer is the claim capability in the shape the bd-backed store
// exposes (BdStore.ClaimAs): the same compare-and-swap, reached through bd's
// own claim verb with the actor named explicitly rather than inherited from
// the store's CommandRunner environment.
type ActorClaimer interface {
	ClaimAs(id, assignee string) (Bead, bool, error)
}

// Commenter appends a comment to a bead. Append-only: there is no edit or
// delete, matching the underlying ledger's comment log.
type Commenter interface {
	Comment(id, text string) error
}

// ClaimSupported reports whether store implements either claim shape, i.e.
// whether ClaimFor would delegate to the backend rather than answer
// ErrClaimUnsupported. It exists for the callers that must refuse BEFORE they
// have written anything: a front door that reserves a session pointer or a
// transaction row first cannot learn capability support from the claim result
// without leaving its own write behind.
//
// It is the same predicate ClaimFor applies, stated once and next to it, so the
// pre-write probe and the dispatch cannot drift — the failure mode this seam
// prevents is a caller restating one of the two shapes locally and asserting
// that narrower shape against a wrapper (e.g. a cache that forwards to ClaimFor
// and so exposes only ActorClaimer). Discover the capability here, never with a
// locally declared interface.
func ClaimSupported(store Store) bool {
	if store == nil {
		return false
	}
	if _, ok := store.(AssigneeClaimer); ok {
		return true
	}
	_, ok := store.(ActorClaimer)
	return ok
}

// ClaimFor performs the store's atomic claim compare-and-swap for assignee,
// whichever of the two claim shapes the resolved store implements. It is the
// single discovery point so callers do not repeat the pair of type
// assertions, and so a store that implements neither fails with a named
// capability error instead of a nil-interface panic.
//
// The CAS semantics are the store's, not this function's: ok=false is a
// conflict (a different assignee holds the bead, or it is closed), not an
// error, and a re-claim by the current holder is idempotent. See the Claim
// doc comment on sqlite_store_claim.go for the normative statement.
func ClaimFor(store Store, id, assignee string) (Bead, bool, error) {
	if !ClaimSupported(store) {
		return Bead{}, false, ErrClaimUnsupported
	}
	if claimer, ok := store.(AssigneeClaimer); ok {
		return claimer.Claim(id, assignee)
	}
	if claimer, ok := store.(ActorClaimer); ok {
		return claimer.ClaimAs(id, assignee)
	}
	return Bead{}, false, ErrClaimUnsupported
}

// CommentOn appends text as a comment on the bead, or reports
// ErrCommentUnsupported when the resolved store keeps no comment log.
func CommentOn(store Store, id, text string) error {
	if store == nil {
		return ErrCommentUnsupported
	}
	commenter, ok := store.(Commenter)
	if !ok {
		return ErrCommentUnsupported
	}
	return commenter.Comment(id, text)
}
