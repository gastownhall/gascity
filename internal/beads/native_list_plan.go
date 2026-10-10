package beads

import (
	"sync/atomic"

	"github.com/steveyegge/beads/issueops"
)

// NativeListPlan returns the reader-role requests NativeDoltStore.List issues
// for query, in the order it issues them. The store unions their rows by id and
// ApplyListQuery then applies the whole query exactly, so a plan only decides
// how many rows cross from the backend, never which rows the caller receives.
//
// It is a pure function of the query so a test can price a recorded List
// without a server: each request costs one backend listing, and a request that
// NativeListRequestKeyed reports as unkeyed reads the whole ledger.
func NativeListPlan(query ListQuery) []issueops.ListRequest {
	return []issueops.ListRequest{nativeListRequestFromListQuery(query)}
}

// NativeListRequestKeyed reports whether req carries a predicate that narrows
// the backend's candidate set below the whole ledger. A request without one
// returns every row of both tiers, closed rows included, and is the full walk
// the list-request tripwire counts.
func NativeListRequestKeyed(req issueops.ListRequest) bool {
	return req.ParentID != "" ||
		req.Assignee != "" ||
		len(req.Labels) > 0 ||
		len(req.MetadataFields) > 0 ||
		req.CreatedBefore != nil ||
		req.Status != "" ||
		req.IssueType != ""
}

// ListRequestStats counts the backend listing requests a store has issued.
type ListRequestStats struct {
	// Lists is the number of List calls the store served.
	Lists int64
	// Requests is the number of backend listing requests those calls issued.
	Requests int64
	// Unkeyed is the number of requests that carried no narrowing predicate
	// and therefore read the whole ledger.
	Unkeyed int64
	// Rows is the number of rows the backend returned across all requests,
	// before any client-side filtering.
	Rows int64
}

// ListRequestCounter is the optional capability of a store that counts the
// backend listing requests its List calls issue. NativeDoltStore implements
// it; budget tests read it to prove a hook never walks the whole ledger.
type ListRequestCounter interface {
	// ListRequestStats returns the counts accumulated since the store opened.
	ListRequestStats() ListRequestStats
}

// nativeListCounters is the atomic backing of ListRequestStats.
type nativeListCounters struct {
	lists    atomic.Int64
	requests atomic.Int64
	unkeyed  atomic.Int64
	rows     atomic.Int64
}

func (c *nativeListCounters) noteList() { c.lists.Add(1) }

func (c *nativeListCounters) noteRequest(req issueops.ListRequest, rows int) {
	c.requests.Add(1)
	if !NativeListRequestKeyed(req) {
		c.unkeyed.Add(1)
	}
	c.rows.Add(int64(rows))
}

func (c *nativeListCounters) snapshot() ListRequestStats {
	return ListRequestStats{
		Lists:    c.lists.Load(),
		Requests: c.requests.Load(),
		Unkeyed:  c.unkeyed.Load(),
		Rows:     c.rows.Load(),
	}
}

// ListRequestStats implements ListRequestCounter.
func (s *NativeDoltStore) ListRequestStats() ListRequestStats {
	if s == nil {
		return ListRequestStats{}
	}
	return s.listCounters.snapshot()
}

// ListOpenQuery is the ListQuery a List-composed store answers ListOpen with.
func ListOpenQuery(status ...string) ListQuery {
	query := ListQuery{AllowScan: true}
	if len(status) > 0 {
		query.Status = status[0]
		if status[0] == "closed" {
			query.IncludeClosed = true
		}
	}
	return query
}

// ChildrenQuery is the ListQuery a List-composed store answers Children with.
func ChildrenQuery(parentID string, opts ...QueryOpt) ListQuery {
	return ListQuery{
		ParentID:      parentID,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		TierMode:      TierModeFromOpts(opts),
	}
}

// ListByLabelQuery is the ListQuery a List-composed store answers ListByLabel
// with.
func ListByLabelQuery(label string, limit int, opts ...QueryOpt) ListQuery {
	return ListQuery{
		Label:         label,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		TierMode:      TierModeFromOpts(opts),
	}
}

// ListByAssigneeQuery is the ListQuery a List-composed store answers
// ListByAssignee with.
func ListByAssigneeQuery(assignee, status string, limit int) ListQuery {
	return ListQuery{Assignee: assignee, Status: status, Limit: limit, AllowScan: true}
}

// ListByMetadataQuery is the ListQuery a List-composed store answers
// ListByMetadata with.
func ListByMetadataQuery(filters map[string]string, limit int, opts ...QueryOpt) ListQuery {
	return ListQuery{
		Metadata:      filters,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		TierMode:      TierModeFromOpts(opts),
	}
}
