package session

import "github.com/gastownhall/gascity/internal/beads"

// The TRANSACTIONAL half of the session-class front door.
//
// Sessions has multi-write invariants that a sequence of independent front-door
// calls cannot express: the pending-create rollback clears the in-flight lease,
// stamps the failed-create terminal metadata, closes the bead and clears the
// runtime name, and a bead that reports closed must always carry its terminal
// state. Until this file existed the only way to say that was to reach past the
// front door for the raw bead store (Store().Tx), which is why every consumer of
// that invariant still held the CONCRETE sessions door.
//
// One rule shapes the surface: a transactional method has the SAME NAME, the
// same arguments and the same semantics as its non-transactional Store peer.
// The sessions class already spells "bare status close" and "close with terminal
// metadata" as two different methods (CloseWithoutReason and Close), so a Tx
// that offered a single Close would have made one name mean two things depending
// on which side of the callback boundary it was written on.
//
// Availability is not a capability question here, and deliberately so. Every
// beads.Store has Tx, so this method never answers a caller with "transactions
// unavailable" — a capability that degrades at call time is a silent
// degradation (see internal/storebinding/sqlite/graph_frontdoor.go). What varies
// between backends is ATOMICITY, and that is declared up front as
// storebinding.ClassCapability.Transactions and asserted by the classdbtest
// Sessions suite, exactly as it already is for the graph class.

// Tx is the write surface available inside a [Store.Tx] callback. Each method is
// the transactional peer of the Store method of the same name.
//
// It is deliberately narrower than beads.Tx: sessions are created through
// CreateSessionInfo (which mints IDs and the session label) and healed through
// the typed lifecycle methods, so a transactional Create or Update would hand
// callers a second, codec-free way to write a session bead.
type Tx interface {
	// ApplyPatch applies a MetadataPatch inside the transaction. Like
	// [Store.ApplyPatch] an empty patch is a no-op and empty-string values are
	// written verbatim as clears.
	ApplyPatch(id string, patch MetadataPatch) error
	// CloseWithoutReason sets the bead status to closed inside the transaction,
	// stamping no terminal metadata. Like [Store.CloseWithoutReason] the caller
	// owns the terminal metadata — inside a transaction that means an
	// ApplyPatch(ClosePatch(...)) ordered against this call as the caller's
	// invariant requires.
	CloseWithoutReason(id string) error
}

// Tx runs fn inside one underlying store transaction, exposing only the [Tx]
// surface — the raw beads.Tx never reaches the caller.
//
// The error is returned verbatim: callers branch on the callback's own failure
// (rollbackPendingCreateClears distinguishes a failed transaction from an
// already-closed bead that way), so wrapping here would reclassify their
// decisions. Whether a failed transaction rolls its writes back is a property of
// the backing store, not of this method: see beads.StoreSupportsAtomicTx and the
// Transactions class capability.
func (s *Store) Tx(message string, fn func(Tx) error) error {
	if fn == nil {
		return s.store.Tx(message, nil)
	}
	return s.store.Tx(message, func(tx beads.Tx) error { return fn(storeTx{tx: tx}) })
}

// storeTx narrows one beads transaction to the sessions Tx surface, by
// delegation rather than embedding so the generic bead write surface stays
// unreachable from inside a sessions callback.
type storeTx struct{ tx beads.Tx }

// ApplyPatch applies a MetadataPatch inside the transaction.
func (t storeTx) ApplyPatch(id string, patch MetadataPatch) error {
	if len(patch) == 0 {
		return nil
	}
	return t.tx.SetMetadataBatch(id, map[string]string(patch))
}

// CloseWithoutReason sets the bead status to closed inside the transaction.
func (t storeTx) CloseWithoutReason(id string) error { return t.tx.Close(id) }

var _ Tx = storeTx{}
