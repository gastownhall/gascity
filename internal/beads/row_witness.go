package beads

// RowWitness reports whether this process has already seen a store answer a
// read with at least one row.
//
// It exists for one caller shape: a consumer holding a count of zero that has
// to decide whether that zero is a measurement. Two states produce it — a
// ledger that holds nothing, and a read that could not see the ledger it was
// pointed at — and they are indistinguishable from the number alone. That is
// the whole subject of unread_store_notice.go, which diagnoses the shape at
// the store layer and deliberately lets the empty read succeed, because
// refusing there would fail a merely idle city closed across `gc ready`, `gc
// rig add` and the federation.
//
// A store that has already handed this process a row settles the ambiguity in
// the one direction a read path can act on: the ledger is not empty, so a
// later zero for a whole-ledger read is a failed measurement rather than a
// small one. That lets a consumer for which zero is never a usable answer —
// the store-health denominator is the first — reject the zero without the
// store layer having to refuse anything.
//
// The capability is one-directional on purpose. SawRows reporting true is
// proof the store holds rows. Reporting false proves nothing, because a
// process that has not read the scope yet and a genuinely empty ledger answer
// identically. Callers may use it to reject a zero and never to certify one.
//
// It is optional in the style of Counter and BatchDeleter: a store that cannot
// witness its own rows simply does not implement it, and callers fall back to
// their prior behavior rather than to a refusal.
type RowWitness interface {
	// SawRows reports whether a read of this store's scope has returned at
	// least one row in this process, counted as the store received it and
	// before any client-side filtering. Filtering can reduce a real answer to
	// nothing, and that reduction says nothing about the ledger.
	SawRows() bool
}

// noteRows records that the backend handed this store rows for some read.
//
// It takes the count the BACKEND returned rather than the count the caller
// received, for the reason BdStore.noteServerRows takes the same care: a
// tier, assignee or limit filter applied Go-side can reduce a real answer to
// nothing, and a store that answered and then had its answer narrowed is
// still demonstrably populated.
//
// A zero never latches, so a read that found nothing can never certify
// itself. That is what lets the store-health count consult the witness on the
// result of the very read it is judging.
func (s *NativeDoltStore) noteRows(rows int) {
	if s == nil || rows <= 0 || s.sawRows.Load() {
		return
	}
	s.sawRows.Store(true)
}

// SawRows implements RowWitness for the native backend.
//
// The latch is per STORE here rather than per scope, which is the weaker of
// the two shapes and is the right one for this backend: a NativeDoltStore
// holds a live connection for its lifetime instead of being rebuilt per read,
// so the store object and the reader are the same thing. The cost is that a
// freshly reopened store starts with no evidence again, which errs toward
// keeping a zero rather than toward rejecting one.
func (s *NativeDoltStore) SawRows() bool {
	return s != nil && s.sawRows.Load()
}
