package beads

// RowRefresher is a bead cache that can install one row as its backing has
// it now (CachingStore.RefreshRowReport) and schedule a full re-scan
// (CachingStore.RequestReconcile). A controller uses it to make a write that
// another process made on the backing visible before it decides on the row.
type RowRefresher interface {
	RefreshRowReport(id string, opts RowRefreshOptions) (RowRefresh, error)
	RequestReconcile()
}

var _ RowRefresher = (*CachingStore)(nil)

// rowRefresherMaxDepth bounds RowRefresherFor's walk through wrappers.
const rowRefresherMaxDepth = 8

// RowRefresherFor returns the bead cache s reads through: the store below
// s's class layers, or the store its conditional-write wrappers
// (ConditionalWritesResolveTargeter) resolve to. It reports false when no
// cache stands between s and its backing, so reads already go to the store.
func RowRefresherFor(s Store) (RowRefresher, bool) {
	cur := physicalStore(s)
	for range rowRefresherMaxDepth {
		if cur == nil {
			return nil, false
		}
		if r, ok := cur.(RowRefresher); ok {
			return r, true
		}
		t, ok := cur.(ConditionalWritesResolveTargeter)
		if !ok {
			return nil, false
		}
		cur = physicalStore(t.ConditionalWritesResolveTarget())
	}
	return nil, false
}
