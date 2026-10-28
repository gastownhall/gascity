package beads

// ReadyOrder is an order a bounded ready read can be cut in.
type ReadyOrder int

const (
	// ReadyOrderCanonical is the canonical ready order: priority (a missing
	// priority counts as 2), then created_at, then id, all ascending. It is the
	// order SortBeadsReadyOrder imposes.
	ReadyOrderCanonical ReadyOrder = iota + 1
	// ReadyOrderOldest is created_at, then id, ascending: SortBeads with
	// SortCreatedAsc.
	ReadyOrderOldest
)

// OrderedReadyReader is implemented by a store that can answer a BOUNDED ready
// read cut in a requested order.
//
// ReadyOrdered(query, order) returns the first query.Limit rows, in order, of
// exactly the set Ready(query) would return with no limit: every readiness
// filter and veto the store applies is applied BEFORE the cut, so a veto cannot
// leave the answer short while ready rows remain behind it. ordered reports
// whether the store did that; when it is false the rows are the store's whole
// ready set for query, in the store's own order, exactly as Ready with no limit
// returns them. A wrapper whose inner store cannot cut in order says so through
// ordered=false rather than by applying the limit in some other order.
//
// It is a separate capability rather than a field on ReadyQuery because a store
// that ignored a field would still apply Limit in its own order, and a bound cut
// in the wrong order is a wrong answer, not a superset.
type OrderedReadyReader interface {
	ReadyOrdered(query ReadyQuery, order ReadyOrder) (rows []Bead, ordered bool, err error)
}

// ReadyOrdered answers a bounded ready read cut in order when store can, and
// reads store's whole ready set otherwise; ordered reports which. Both read
// LIVE: a caching store answers from its backing store, never its read cache.
//
// A query with no positive Limit is never bounded.
func ReadyOrdered(store Store, query ReadyQuery, order ReadyOrder) ([]Bead, bool, error) {
	if reader, ok := store.(OrderedReadyReader); ok && query.Limit > 0 {
		return reader.ReadyOrdered(query, order)
	}
	query.Limit = 0
	rows, err := HandlesFor(store).Live.Ready(query)
	return rows, false, err
}

// ReadyOrdered reads the backing store, live: a bounded read is an authoritative
// read, and this cache's ready projection is not one.
func (c *CachingStore) ReadyOrdered(query ReadyQuery, order ReadyOrder) ([]Bead, bool, error) {
	return ReadyOrdered(c.backing, query, order)
}
