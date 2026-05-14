package beads

import (
	"errors"
	"sort"
	"time"
)

// ErrQueryRequiresScan reports that a query would require an explicit scan.
// Callers must opt into that behavior with ListQuery.AllowScan.
var ErrQueryRequiresScan = errors.New("bead query requires scan")

// SortOrder controls optional result ordering for List queries.
type SortOrder string

// List query sort orders.
const (
	// SortDefault leaves store-defined ordering unchanged.
	SortDefault     SortOrder = ""
	SortCreatedAsc  SortOrder = "created_asc"
	SortCreatedDesc SortOrder = "created_desc"
)

// TierMode selects which storage tier(s) a List query reads from.
// The zero value is TierIssues.
//
// For BdStore, where issues and wisps live in physically separate Dolt
// tables, TierIssues is naturally tier-restricted by the underlying
// `bd list` call. For in-memory stores (MemStore, ApplyListQuery) where
// both tiers may share a single backing slice, Matches now filters out
// any bead with Ephemeral=true under TierIssues. Pre-PR, such a query
// would have returned ephemeral rows mixed in; callers that relied on
// that behavior must opt into TierBoth explicitly.
type TierMode int

const (
	// TierIssues reads only the permanent (issues) tier. Default.
	TierIssues TierMode = iota
	// TierWisps reads only the ephemeral (wisps) tier.
	TierWisps
	// TierBoth unions the issues and wisps tiers, deduping by ID and
	// preserving the query's sort.
	TierBoth
)

// TierModeFromOpts returns the tier mode implied by a slice of QueryOpts.
// WithBothTiers takes precedence over WithEphemeral.
func TierModeFromOpts(opts []QueryOpt) TierMode {
	switch {
	case HasOpt(opts, WithBothTiers):
		return TierBoth
	case HasOpt(opts, WithEphemeral):
		return TierWisps
	default:
		return TierIssues
	}
}

// ListQuery describes a filtered bead lookup.
//
// Queries are conjunctive: every populated field must match. A zero-value query
// is rejected unless AllowScan is true.
type ListQuery struct {
	Status        string
	Type          string
	Label         string
	Assignee      string
	ParentID      string
	Metadata      map[string]string
	CreatedBefore time.Time
	Limit         int
	IncludeClosed bool
	AllowScan     bool
	// Live bypasses CachingStore and reads from the backing store. Other Store
	// implementations ignore it. Use it only for lifecycle gates that must
	// observe external mutations immediately.
	Live bool
	Sort SortOrder
	// TierMode selects the storage tier(s) to read from. Zero value
	// (TierIssues) preserves the legacy single-tier behavior.
	TierMode TierMode
}

// ReadyQuery describes optional filters for ready-work lookup. A zero-value
// query preserves Ready's historical behavior: all open, unblocked actionable
// work.
type ReadyQuery struct {
	Assignee string
	Limit    int
}

func readyQueryFromArgs(queries []ReadyQuery) ReadyQuery {
	if len(queries) == 0 {
		return ReadyQuery{}
	}
	return queries[0]
}

// HasFilter reports whether the query includes at least one indexed selector.
func (q ListQuery) HasFilter() bool {
	return q.Status != "" ||
		q.Type != "" ||
		q.Label != "" ||
		q.Assignee != "" ||
		q.ParentID != "" ||
		len(q.Metadata) > 0 ||
		!q.CreatedBefore.IsZero()
}

// IncludesClosed reports whether the query may return closed beads.
func (q ListQuery) IncludesClosed() bool {
	return q.IncludeClosed || q.Status == "closed"
}

// Matches reports whether the bead satisfies the query.
func (q ListQuery) Matches(b Bead) bool {
	switch q.TierMode {
	case TierWisps:
		if !b.Ephemeral {
			return false
		}
	case TierBoth:
		// no tier filter
	default: // TierIssues
		if b.Ephemeral {
			return false
		}
	}
	if q.Status != "" {
		if b.Status != q.Status {
			return false
		}
	} else if !q.IncludeClosed && b.Status == "closed" {
		return false
	}
	if q.Type != "" && b.Type != q.Type {
		return false
	}
	if q.Label != "" && !beadHasLabel(b, q.Label) {
		return false
	}
	if q.Assignee != "" && b.Assignee != q.Assignee {
		return false
	}
	if q.ParentID != "" && b.ParentID != q.ParentID {
		return false
	}
	if len(q.Metadata) > 0 && !matchesMetadata(b, q.Metadata) {
		return false
	}
	if !q.CreatedBefore.IsZero() && !b.CreatedAt.Before(q.CreatedBefore) {
		return false
	}
	return true
}

func beadHasLabel(b Bead, want string) bool {
	for _, label := range b.Labels {
		if label == want {
			return true
		}
	}
	return false
}

// ApplyListQuery filters, sorts, and limits an in-memory bead slice.
func ApplyListQuery(items []Bead, q ListQuery) []Bead {
	filtered := make([]Bead, 0, len(items))
	for _, b := range items {
		if q.Matches(b) {
			filtered = append(filtered, b)
		}
	}
	sortBeadsForQuery(filtered, q.Sort)
	if q.Limit > 0 && len(filtered) > q.Limit {
		filtered = filtered[:q.Limit]
	}
	return filtered
}

func applyListQuery(items []Bead, q ListQuery) []Bead {
	return ApplyListQuery(items, q)
}

func sortBeadsForQuery(items []Bead, order SortOrder) {
	switch order {
	case SortCreatedAsc:
		sort.Slice(items, func(i, j int) bool {
			if items[i].CreatedAt.Equal(items[j].CreatedAt) {
				return items[i].ID < items[j].ID
			}
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		})
	case SortCreatedDesc:
		sort.Slice(items, func(i, j int) bool {
			if items[i].CreatedAt.Equal(items[j].CreatedAt) {
				return items[i].ID > items[j].ID
			}
			return items[i].CreatedAt.After(items[j].CreatedAt)
		})
	}
}
