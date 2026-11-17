package beadstest

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// ModelListPageRows is the row count one backend listing page carries in the
// wire model. A keyed listing is assumed to fit one page; a listing with no
// narrowing predicate pages through the whole ledger.
const ModelListPageRows = 200

// nativeGetRequests is what NativeDoltStore.Get sends for a bead it finds: the
// reader role's detail view, then readAnchorEdges for the stored edge rows.
const nativeGetRequests = 2

// nativeGetMissRequests is what a Get of a missing bead sends: the detail view
// answers not-found and the edge read is never made.
const nativeGetMissRequests = 1

// WireModel prices recorded store operations as backend requests against a
// remote ledger, so a test can bound what a command costs without a server.
//
// Each store open costs Handshake requests. Get costs what the native store's
// Get sends: the detail view and the bead's edge rows, 2 — or 1 for a miss,
// which the detail view answers alone. List costs one
// request per entry of beads.NativeListPlan, except that a request
// beads.NativeListRequestKeyed reports as unkeyed walks the ledger and costs
// ceil(LedgerRows / ModelListPageRows). Close costs 2 (a re-read plus the
// close). Every other operation that reaches the backend costs 1; the
// clone-local string reads and writes cost nothing.
//
// That 1 is a floor, not the native shape, for an operation that fans out
// behind one call. Ready is the one that matters: the native store sends the
// ready listing and then reads its candidates' blocking edges, in chunks, so
// a budget that records a Ready is understated until Ready is priced here.
type WireModel struct {
	// Handshake is the number of requests one store open costs.
	Handshake int
	// LedgerRows is the number of rows a whole-ledger walk pages through.
	LedgerRows int
}

// WireCost is the priced shape of one command's recorded operations.
type WireCost struct {
	// Opens is the number of stores the command opened.
	Opens int
	// Gets is the number of Get operations, misses included.
	Gets int
	// GetMisses is the number of Gets that answered not-found.
	GetMisses int
	// Lists is the number of List-kind operations.
	Lists int
	// ListRequests is the number of backend listing requests those Lists plan.
	ListRequests int
	// Unkeyed is the number of planned listing requests that walk the whole
	// ledger.
	Unkeyed int
	// Writes is the number of operations that write.
	Writes int
	// Other is the number of operations that are neither reads nor writes
	// above (Ready, DepList, DepMetadata, Ping and similar), each priced at 1:
	// a floor for Ready (see WireModel).
	Other int
	// Requests is the modeled total of backend requests.
	Requests int
	// UnkeyedQueries describes each List whose plan walks the whole ledger.
	UnkeyedQueries []string
}

// String renders the cost on one line for test failure messages.
func (c WireCost) String() string {
	s := fmt.Sprintf("requests=%d opens=%d gets=%d (misses=%d) lists=%d listRequests=%d unkeyed=%d writes=%d other=%d",
		c.Requests, c.Opens, c.Gets, c.GetMisses, c.Lists, c.ListRequests, c.Unkeyed, c.Writes, c.Other)
	if len(c.UnkeyedQueries) > 0 {
		s += "\n  unkeyed: " + strings.Join(c.UnkeyedQueries, "\n  unkeyed: ")
	}
	return s
}

// walkPages is the request cost of one whole-ledger listing.
func (m WireModel) walkPages() int {
	if m.LedgerRows <= 0 {
		return 1
	}
	return (m.LedgerRows + ModelListPageRows - 1) / ModelListPageRows
}

// Price models the backend requests of opens store opens followed by ops.
func (m WireModel) Price(opens int, ops []RecordedOp) WireCost {
	cost := WireCost{Opens: opens, Requests: opens * m.Handshake}
	for _, op := range ops {
		switch op.Kind {
		case OpGet:
			cost.Gets++
			if op.NotFound() {
				cost.GetMisses++
				cost.Requests += nativeGetMissRequests
				continue
			}
			cost.Requests += nativeGetRequests
		case OpList:
			cost.Lists++
			walked := false
			for _, req := range beads.NativeListPlan(op.Query) {
				cost.ListRequests++
				if beads.NativeListRequestKeyed(req) {
					cost.Requests++
					continue
				}
				cost.Unkeyed++
				cost.Requests += m.walkPages()
				walked = true
			}
			if walked {
				cost.UnkeyedQueries = append(cost.UnkeyedQueries, fmt.Sprintf("%s(%s)", op.Method, DescribeListQuery(op.Query)))
			}
		case OpClose:
			cost.Writes++
			cost.Requests += 2
		case OpCreate, OpUpdate, OpReopen, OpCloseAll, OpSetMetadata, OpSetMetadataBatch,
			OpTx, OpDelete, OpDepAdd, OpDepRemove, OpUpdateIfMatch, OpCloseIfMatch,
			OpCloseWithMetadataIfMatch, OpDeleteIfMatch, OpCompareAndSetMetadataKey, OpReleaseIfCurrent:
			cost.Writes++
			cost.Requests++
		case OpGetLocalString, OpSetLocalString:
		default:
			cost.Other++
			cost.Requests++
		}
	}
	return cost
}

// DescribeListQuery renders the populated selectors of q compactly, for
// test logs and failure messages.
func DescribeListQuery(q beads.ListQuery) string {
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	add("type", q.Type)
	add("status", q.Status)
	add("label", q.Label)
	add("assignee", q.Assignee)
	if len(q.Assignees) > 0 {
		add("assignees", strings.Join(q.Assignees, ","))
	}
	add("parent", q.ParentID)
	if len(q.IDs) > 0 {
		add("ids", strings.Join(q.IDs, ","))
	}
	keys := make([]string, 0, len(q.Metadata))
	for k := range q.Metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		add("meta."+k, q.Metadata[k])
	}
	if q.IncludeClosed {
		parts = append(parts, "include_closed")
	}
	if q.Limit > 0 {
		parts = append(parts, fmt.Sprintf("limit=%d", q.Limit))
	}
	if q.TierMode != beads.TierIssues {
		parts = append(parts, fmt.Sprintf("tier=%d", q.TierMode))
	}
	return strings.Join(parts, " ")
}

// IndexedRows prices one listing on a ledger holding rows: how many of them a
// backend must examine to answer it, summed over the requests the native store
// sends for query (beads.NativeListPlan).
//
// A backend seeks what it indexes — the id, the status, the type, the
// assignee, the labels, the parent — and compares everything else, metadata
// included, on each row those leave. So a request that pushes none of them
// examines every row of the ledger, closed history included, however few it
// returns; WireModel's keyed/unkeyed split does not see that, because a
// metadata filter narrows the answer without narrowing the read.
//
// The parent predicate is modeled as the direct children, which is all a
// fixture needs. Tests compare this figure across two ledger sizes: a read on
// a per-message path must cost the same whatever history the ledger holds.
func IndexedRows(rows []beads.Bead, query beads.ListQuery) int {
	total := 0
	for _, req := range beads.NativeListPlan(query) {
		ids := map[string]bool{}
		for _, id := range strings.Split(req.IDFilter, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids[id] = true
			}
		}
		statuses := map[string]bool{}
		for _, status := range strings.Split(req.Status, ",") {
			if status = strings.TrimSpace(status); status != "" {
				statuses[status] = true
			}
		}
		excluded := map[string]bool{}
		for _, entry := range req.ExcludeStatus {
			for _, status := range strings.Split(entry, ",") {
				if status = strings.TrimSpace(status); status != "" {
					excluded[status] = true
				}
			}
		}
		for _, row := range rows {
			if len(ids) > 0 && !ids[row.ID] {
				continue
			}
			if len(statuses) > 0 && !statuses[row.Status] {
				continue
			}
			if excluded[row.Status] {
				continue
			}
			if req.IssueType != "" && row.Type != req.IssueType {
				continue
			}
			if req.Assignee != "" && row.Assignee != req.Assignee {
				continue
			}
			if req.ParentID != "" && row.ParentID != req.ParentID {
				continue
			}
			labeled := true
			for _, label := range req.Labels {
				found := false
				for _, have := range row.Labels {
					found = found || have == label
				}
				labeled = labeled && found
			}
			if !labeled {
				continue
			}
			total++
		}
	}
	return total
}

// ClosedHistory seeds store with n closed order-run records, the rows a city
// accumulates by running: one per firing of each periodic order, kept until
// retention deletes them.
func ClosedHistory(store beads.Store, n int) error {
	for i := range n {
		b, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("order:history-%d", i),
			Type:   "task",
			Labels: []string{"order-tracking", "order-run:history"},
		})
		if err != nil {
			return fmt.Errorf("seeding closed history row %d: %w", i, err)
		}
		if err := store.Close(b.ID); err != nil {
			return fmt.Errorf("closing history row %d: %w", i, err)
		}
	}
	return nil
}

// LedgerRows returns every row store holds, both tiers, closed included.
func LedgerRows(store beads.Store) ([]beads.Bead, error) {
	return store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
}
