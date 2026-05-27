package beads

import (
	"context"
	"errors"
)

// ErrIndexedListUnsupported reports that a ListQuery cannot be served by the
// bounded active indexed reader and should use the authoritative Beads path.
var ErrIndexedListUnsupported = errors.New("indexed list unsupported")

// IndexedListResult carries indexed-reader rows plus coverage metadata for
// enrichment that Beads CLI normally hydrates.
type IndexedListResult struct {
	Beads              []Bead
	DepsByID           map[string][]Dep
	DependencyCoverage bool
	LabelsCoverage     bool
}

// IndexedLister is the read-only active Beads list acceleration contract.
type IndexedLister interface {
	ListIndexed(ctx context.Context, query ListQuery) (IndexedListResult, error)
}

// IndexedCounter is an optional companion to IndexedLister for cheap aggregate
// counts that do not need the hydrated label/dependency payload from ListIndexed.
type IndexedCounter interface {
	CountIndexed(ctx context.Context, query ListQuery) (int, error)
}
