package beads

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/steveyegge/beads/issueops"
)

// NativeListPlan returns the reader-role requests NativeDoltStore.List issues
// for query, in the order it issues them. The store unions their rows by id and
// ApplyListQuery then applies the whole query exactly, so a plan only decides
// how many rows cross from the backend, never which rows the caller receives:
// every predicate it pushes is exact or a superset of the query's.
//
// A plural Assignees set fans out into one request per distinct assignee, each
// with that assignee pushed, up to nativeListAssigneeFanOutMax. The status and
// type pushdowns are nativeListRequestFromListQuery's.
//
// It is a pure function of the query so a test can price a recorded List
// without a server: each request costs one backend listing, and a request that
// NativeListRequestKeyed reports as unkeyed reads the whole ledger.
func NativeListPlan(query ListQuery) []issueops.ListRequest {
	return nativeListPlan(query, true)
}

// nativeListAssigneeFanOutMax bounds the plural-assignee fan-out. No hook asks
// for more than an identity's routes (id, alias, session name and alias
// history); a larger set stays one request with the assignees applied Go-side.
const nativeListAssigneeFanOutMax = 8

// nativeListPlan is NativeListPlan with the type pushdown switchable, for a
// store whose backend refused a pushed type.
func nativeListPlan(query ListQuery, pushType bool) []issueops.ListRequest {
	base := nativeListRequestFromListQuery(query, pushType)
	assignees := nativeListFanOutAssignees(query)
	if len(assignees) == 0 {
		return []issueops.ListRequest{base}
	}
	plan := make([]issueops.ListRequest, 0, len(assignees))
	for _, assignee := range assignees {
		req := base
		req.Assignee = assignee
		plan = append(plan, req)
	}
	return plan
}

// nativeListFanOutAssignees returns the distinct assignees a plan fans out
// over, or nil when the plural set cannot be pushed exactly. An empty entry
// means "unassigned", which an Assignee predicate cannot say.
func nativeListFanOutAssignees(query ListQuery) []string {
	if query.Assignee != "" || len(query.Assignees) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(query.Assignees))
	out := make([]string, 0, len(query.Assignees))
	for _, assignee := range query.Assignees {
		if assignee == "" {
			return nil
		}
		if seen[assignee] {
			continue
		}
		seen[assignee] = true
		out = append(out, assignee)
	}
	if len(out) > nativeListAssigneeFanOutMax {
		return nil
	}
	return out
}

// nativeListPushableStatus reports whether a gc status maps one-to-one onto a
// bd status. mapBdStatus folds every other bd status into gc's "open".
func nativeListPushableStatus(status string) bool {
	return status == "in_progress" || status == "closed"
}

// nativeListInfraTypes are bd's default infra types. Naming one on a listing
// routes it to the ephemeral plane alone (workapi.BuildListFilter), which
// would drop durable rows of that type, so they are never pushed.
var nativeListInfraTypes = map[string]bool{"agent": true, "role": true, "message": true}

// nativeListPushableTypes is the type vocabulary a listing may push: the types
// gc registers in every scope it provisions (RequiredCustomTypes), less bd's
// infra types. A server that validates types knows every one of them; a scope
// provisioned without them refuses the request, and NativeDoltStore.List
// retries it without the type.
var nativeListPushableTypes = func() map[string]bool {
	out := make(map[string]bool, len(RequiredCustomTypes))
	for _, t := range RequiredCustomTypes {
		if !nativeListInfraTypes[t] {
			out[t] = true
		}
	}
	return out
}()

// nativeListPushableType reports whether a gc type may be pushed to the role.
func nativeListPushableType(issueType string) bool {
	return nativeListPushableTypes[issueType]
}

// nativeListTypeRefused reports whether err is the role refusing a pushed
// type: the wire's validation problem, or the local filter builder's
// unknown-type error, which carries no sentinel.
func nativeListTypeRefused(err error) bool {
	return errors.Is(err, issueops.ErrValidation) || strings.Contains(err.Error(), "invalid issue type")
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

// listLogger is the logger the list pushdown reports through.
func (s *NativeDoltStore) listLogger() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// disableTypePushdown latches type pushdown off for this store after the
// backend refused issueType, and warns once: the answers stay exact, but the
// scope is missing gc's type vocabulary and every typed listing now reads
// more rows than it needs.
func (s *NativeDoltStore) disableTypePushdown(issueType string, err error) {
	if !s.typePushdownOff.CompareAndSwap(false, true) {
		return
	}
	s.listLogger().Warn("native bead store: backend refused a pushed issue type; listing without type pushdown for this store (register gc's custom types in this scope: gc doctor --fix)",
		"prefix", s.idPrefix, "issue_type", issueType, "err", err)
}

// noteUnkeyedPlan is the whole-ledger tripwire: a plan request that carries no
// narrowing predicate pages through every row, and is logged at debug with
// the query that asked for it. ListRequestStats counts it.
func (s *NativeDoltStore) noteUnkeyedPlan(query ListQuery, plan []issueops.ListRequest) {
	for _, req := range plan {
		if NativeListRequestKeyed(req) {
			continue
		}
		s.listLogger().Debug("native bead store: list walks the whole ledger",
			"prefix", s.idPrefix, "query", fmt.Sprintf("%+v", query))
		return
	}
}
