package beads

import (
	"context"
	"errors"
)

// ErrCountUnsupported reports that a Counter implementation cannot answer
// the requested query shape exactly without hydrating rows. Callers should
// fall back to List for that query.
var ErrCountUnsupported = errors.New("bead count unsupported for query")

// Counter is an optional Store capability: counting beads without
// hydrating full rows. Implementations must return exactly the number of
// beads List would return for the same query, minus beads whose Type is
// listed in excludeTypes. Query shapes an implementation cannot answer
// with that guarantee return ErrCountUnsupported (possibly wrapped).
//
// Unlike Store.List, Count accepts a context so callers with deadlines
// (e.g. the status endpoint's per-store timeout) can cancel the backing
// query and release its connection instead of leaking a goroutine.
type Counter interface {
	Count(ctx context.Context, query ListQuery, excludeTypes ...string) (int, error)
}
