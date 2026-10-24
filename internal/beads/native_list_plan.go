package beads

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// NativeListPlan returns the reader-role requests NativeDoltStore.List issues
// for query, in the order it issues them, on a scope whose infra-type setting
// is unset. The store unions their rows by id and ApplyListQuery then applies
// the whole query exactly, so a plan only decides how many rows cross from the
// backend, never which rows the caller receives: every predicate it pushes is
// exact or a superset of the query's. The one exception is a Live "open", which
// is pushed as bd's raw status (nativeListPushesRawOpen).
//
// A plural Assignees set fans out into one request per distinct assignee, each
// with that assignee pushed, however many there are: a keyed request reads one
// assignee's rows, while the one request a plural set would otherwise collapse
// into carries no assignee and, for a type and status that cannot be pushed
// (open mail), walks the whole ledger. The status and type pushdowns are
// nativeListRequestFromListQuery's.
//
// A FANNED-OUT PLAN IS SEVERAL SNAPSHOTS. The reader role publishes no read
// transaction, so each request reads the ledger as of its own arrival: a bead
// whose assignee moves between two of the requests can be seen by both (the
// union keeps it once) or by neither. The answer is then one the ledger never
// held at a single instant, but every row in it was true when it was read, and
// the next hook's listing reads the moved row where it now lives. Hooks
// tolerate that by design — they are re-run on every turn and converge — and a
// caller that needs one instant's answer passes a singular Assignee.
//
// It is a pure function of the query so a test can price a recorded List
// without a server: each request costs one backend listing, and a request that
// NativeListRequestKeyed reports as unkeyed reads the whole ledger.
func NativeListPlan(query ListQuery) []issueops.ListRequest {
	return nativeListPlan(query, nativeListDefaultPushableTypes)
}

// nativeListPlan is NativeListPlan over a store's own pushable type set. A nil
// set pushes no type at all.
func nativeListPlan(query ListQuery, pushableTypes map[string]bool) []issueops.ListRequest {
	base := nativeListRequestFromListQuery(query, pushableTypes)
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
	return out
}

// nativeListPushableStatus reports whether a gc status maps one-to-one onto a
// bd status. mapBdStatus folds every other bd status into gc's "open".
func nativeListPushableStatus(status string) bool {
	return status == "in_progress" || status == "closed"
}

// nativeListPushesRawOpen reports whether query's "open" is pushed to the
// backing as bd's raw status rather than answered as gc's folded "open".
//
// A Live read is a lifecycle gate asking for the backing's own answer, and
// every other backing gives the RAW status for Live Status:"open": BdStore
// passes --status=open to bd, while SQLiteStore, DoltliteReadStore and MemStore
// match the stored status exactly. The callers are written against that
// answer: the controller's demand reads; the seat-work gates (config-drift
// deferral, max session age, slot-freeable, the drain-ack close gate); route
// recovery; and the orphan sweep. To them a blocked or deferred bead is
// parked, not work (gc-4zb/#4395, EB-42o8/gc-nz5i). Before this, only native
// cities answered with the folded "open", so every one of those gates counted
// parked beads as live work. A seat holding only a blocked bead never drained,
// never took a config change, and was re-offered the bead on every wake.
//
// The readers that move or flag a session's work read the same answer, so
// parked work is left assigned where it is: the retirement reassignments
// (cmd/gc session_beads.go, internal/api session_resolution.go), the
// stranded-repair unclaim through SeatWork, and the identity-residue doctor;
// a seat the close gate lets go with only parked work keeps it too. That is
// BdStore's behavior, accepted here as parity; ga-7uf6c7 tracks moving parked
// work as well.
//
// A non-Live read keeps the folded answer while CachingStore serves it from
// the cache: PrimeActive lists without Live, so the cache holds blocked rows
// as "open". The cache's fallback re-issues the read Live, though, so a cold
// or degraded cache, more than dirtyOverlayMaxGets dirty rows, or a failed
// overlay read returns the raw answer (ga-4w9wpf). Count declines the open
// shape (nativeDoltCountSupported); callers keep the folded answer through the
// List fallback, or the raw one when that List is Live.
//
// CachingStore's Live stale-row sweep (staleLiveCacheIDs) skips a cached row
// whose SourceStatus is another status, so the parked rows this leaves out
// cost no read.
//
// This is the one plan predicate that is not a superset of the query: it
// leaves out rows that ApplyListQuery's folded "open" would keep. That is
// deliberate, and it applies only under Live.
func nativeListPushesRawOpen(query ListQuery) bool {
	return query.Live && query.Status == "open"
}

// nativeTypesInfraConfigKey is the workspace setting naming the types bd treats
// as infrastructure.
const nativeTypesInfraConfigKey = "types.infra"

// nativeListHistoricalInfraTypes are every type a bd release has treated as
// infra by default: agent, role and message today, and rig until bd commit
// 4d3a552424 (Jun 22 2026). Naming an infra type on a listing routes it to the
// ephemeral plane alone (workapi.BuildListFilter), and the library reads the
// durable rows only when no wisp matches, so pushing one would intermittently
// drop durable rows of that type — a loss the Go-side re-filter cannot undo.
// None of them is ever pushed, whatever the server's own setting says, because
// a server may run a release whose built-in default still carries one.
var nativeListHistoricalInfraTypes = map[string]bool{"agent": true, "rig": true, "role": true, "message": true}

// nativeListDefaultPushableTypes is the type vocabulary a listing may push on a
// scope whose types.infra setting is unset: the types gc registers in every
// scope it provisions (RequiredCustomTypes), less the historical infra types.
// A server that validates types knows every one of them; a scope provisioned
// without them refuses the request, and NativeDoltStore.List retries it
// without the type.
var nativeListDefaultPushableTypes = nativeListPushableTypesFor("")

// nativeListPushableTypesFor is nativeListDefaultPushableTypes less every type
// a types.infra setting value names. The value is bd's comma-separated list.
//
// THE ONE RESIDUE is a server whose setting is unset and whose config.yaml
// carries its own types.infra (bd's second fallback). The workspace-config
// role publishes only stored settings, so that list is invisible here; gc
// never writes it, and a type an operator adds there must also be added to the
// stored setting for the pushdown to honor it.
func nativeListPushableTypesFor(infraSetting string) map[string]bool {
	infra := make(map[string]bool)
	for _, t := range strings.Split(infraSetting, ",") {
		if t = strings.TrimSpace(t); t != "" {
			infra[t] = true
		}
	}
	out := make(map[string]bool, len(RequiredCustomTypes))
	for _, t := range RequiredCustomTypes {
		if !nativeListHistoricalInfraTypes[t] && !infra[t] {
			out[t] = true
		}
	}
	return out
}

// nativeReadListPushableTypes reads the scope's types.infra setting through the
// workspace-config role, as nativeReadIssuePrefix reads the prefix, and returns
// the types a listing of this scope may push.
func nativeReadListPushableTypes(ctx context.Context, storage beadslib.Storage) (map[string]bool, error) {
	settings, err := storage.WorkspaceConfig()
	if err != nil {
		return nil, err
	}
	result, err := settings.GetSetting(ctx, issueops.GetSettingRequest{Key: nativeTypesInfraConfigKey})
	if err != nil {
		return nil, err
	}
	return nativeListPushableTypesFor(result.Value), nil
}

// loadListPushableTypes sets the store's pushable type set from its scope's
// types.infra setting. It runs once, at open, so a type an operator adds to
// the setting later stays pushable on a store already open — losing that
// type's durable rows — until the process holding the store restarts. A
// setting that cannot be read leaves the set nil, and the store then pushes no
// type at all: a type the server classifies as infra would lose durable rows,
// and only the server's own setting says which types those are.
func (s *NativeDoltStore) loadListPushableTypes(ctx context.Context) {
	pushable, err := nativeReadListPushableTypes(ctx, s.storage)
	if err != nil {
		s.listLogger().Warn("native bead store: cannot read the scope's types.infra setting; listing without type pushdown for this store",
			"prefix", s.idPrefix, "err", err)
		return
	}
	s.listPushableTypes = pushable
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

// listRequestCounter is implemented by a store that counts the backend listing
// requests its List calls issue. It is unexported so the diagnostic stays out
// of the engine's method set, which the store wrappers must forward in full.
type listRequestCounter interface {
	listRequestStats() ListRequestStats
}

// ListRequestStatsOf returns the backend listing counts store has accumulated
// since it opened, and false when store does not keep them. NativeDoltStore
// keeps them; budget tests read them to prove a hook never walks the whole
// ledger.
func ListRequestStatsOf(store Store) (ListRequestStats, bool) {
	c, ok := store.(listRequestCounter)
	if !ok {
		return ListRequestStats{}, false
	}
	return c.listRequestStats(), true
}

// nativeListCounters is the atomic backing of ListRequestStats.
type nativeListCounters struct {
	lists    atomic.Int64
	requests atomic.Int64
	unkeyed  atomic.Int64
	rows     atomic.Int64
}

func (c *nativeListCounters) noteList() { c.lists.Add(1) }

// nativeListTally is what one attempt of a List sent: every backend listing
// request, a refused one included, and the rows the answered ones returned. A
// List commits only its last attempt's tally, so a withReadRetry retry does not
// count the abandoned attempt's requests twice.
type nativeListTally struct {
	requests int64
	unkeyed  int64
	rows     int64
}

func (t *nativeListTally) noteRequest(req issueops.ListRequest, rows int) {
	t.requests++
	if !NativeListRequestKeyed(req) {
		t.unkeyed++
	}
	t.rows += int64(rows)
}

func (c *nativeListCounters) commit(t nativeListTally) {
	c.requests.Add(t.requests)
	c.unkeyed.Add(t.unkeyed)
	c.rows.Add(t.rows)
}

func (c *nativeListCounters) snapshot() ListRequestStats {
	return ListRequestStats{
		Lists:    c.lists.Load(),
		Requests: c.requests.Load(),
		Unkeyed:  c.unkeyed.Load(),
		Rows:     c.rows.Load(),
	}
}

// listRequestStats implements listRequestCounter.
func (s *NativeDoltStore) listRequestStats() ListRequestStats {
	if s == nil {
		return ListRequestStats{}
	}
	return s.listCounters.snapshot()
}

// ListOpenQuery is the ListQuery the native store answers ListOpen with, and
// the one OpRecordingStore records for every delegate's ListOpen. MemStore and
// FileStore leave IncludeClosed unset for a closed status; they select the
// same beads, because a set Status alone decides ListQuery.Matches.
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
// backend refused issueType and answered the same request without it, and
// warns once: the answers stay exact, but the scope is missing gc's type
// vocabulary and every typed listing now reads more rows than it needs.
func (s *NativeDoltStore) disableTypePushdown(issueType string, err error) {
	if !s.typePushdownOff.CompareAndSwap(false, true) {
		return
	}
	s.listLogger().Warn("native bead store: backend refused a pushed issue type; listing without type pushdown for this store (register gc's custom types in this scope: gc doctor --fix)",
		"prefix", s.idPrefix, "issue_type", issueType, "err", err)
}

// noteUnkeyedPlan is the whole-ledger tripwire: a plan request that carries no
// narrowing predicate pages through every row, and is logged at debug with
// the query that asked for it. ListRequestStats counts it. The query is
// rendered only when debug logging is on.
func (s *NativeDoltStore) noteUnkeyedPlan(query ListQuery, plan []issueops.ListRequest) {
	for _, req := range plan {
		if NativeListRequestKeyed(req) {
			continue
		}
		logger := s.listLogger()
		if !logger.Enabled(context.Background(), slog.LevelDebug) {
			return
		}
		logger.Debug("native bead store: list walks the whole ledger",
			"prefix", s.idPrefix, "query", fmt.Sprintf("%+v", query))
		return
	}
}
