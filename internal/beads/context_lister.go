package beads

import "context"

// ContextLister is an optional Store capability: listing beads with a
// context. Implementations must return the same rows List would return for
// the same query.
//
// Unlike Store.List, ListContext accepts a context so callers with deadlines
// (e.g. the status endpoint's per-store timeout) can cancel the backing
// query and release its connection instead of leaking a goroutine. Mirrors
// Counter's rationale and shape exactly.
type ContextLister interface {
	ListContext(ctx context.Context, query ListQuery) ([]Bead, error)
}

// ContextListOrFallback calls store.ListContext when store implements
// ContextLister, else falls back to the plain, non-cancellable store.List.
// Callers with a deadline should still bound the fallback path themselves
// (e.g. a goroutine+select), since a plain List cannot be canceled.
func ContextListOrFallback(ctx context.Context, store Store, query ListQuery) ([]Bead, error) {
	if lister, ok := store.(ContextLister); ok {
		return lister.ListContext(ctx, query)
	}
	return store.List(query)
}
