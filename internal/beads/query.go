package beads

import (
	"errors"
	"sort"
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
	Limit         int
	IncludeClosed bool
	AllowScan     bool
	Sort          SortOrder
}

// HasFilter reports whether the query includes at least one indexed selector.
func (q ListQuery) HasFilter() bool {
	return q.Status != "" ||
		q.Type != "" ||
		q.Label != "" ||
		q.Assignee != "" ||
		q.ParentID != "" ||
		len(q.Metadata) > 0
}

// IncludesClosed reports whether the query may return closed beads.
func (q ListQuery) IncludesClosed() bool {
	return q.IncludeClosed || q.Status == "closed"
}

// Matches reports whether the bead satisfies the query.
func (q ListQuery) Matches(b Bead) bool {
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
