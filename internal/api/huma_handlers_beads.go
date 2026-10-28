package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/session"
)

const beadParentProjectionWaitTimeout = 10 * time.Second

// beadUpdateDescription documents the reparent read-after-write contract
// shared by POST /bead/{id}/update and PATCH /bead/{id}.
var beadUpdateDescription = fmt.Sprintf("Applies the update. When it changes the parent of a bead that is not being closed (moving it to a new parent or clearing its parent), the request waits up to %d seconds until the parent-child listings reflect the change, so an immediate read of the old and new parents' children shows it (subject to those listings' usual status filters). "+
	"A 409 (the bead was reparented or deleted concurrently), 503 (the request was canceled while waiting), or 504 (the move was not confirmed in time) can arrive after the update was applied: read the bead's current state before retrying.",
	int(beadParentProjectionWaitTimeout/time.Second))

// humaHandleBeadList is the Huma-typed handler for GET /v0/beads.
//
// Bounded reads are a deterministic prefix of one total order: every
// per-store query and the cross-rig merge sort by (created_at DESC, id DESC),
// so the same request returns the same page on every call and a truncated
// response always carries next_cursor for the remainder (#3208).
func (s *Server) humaHandleBeadList(ctx context.Context, input *BeadListInput) (*ListOutput[beads.Bead], error) {
	bp := input.toBlockingParams()
	blocking := bp.isBlocking()
	if blocking {
		waitForChange(ctx, s.state.EventProvider(), bp)
	}
	index := s.latestIndex()

	cityStore := s.state.CityBeadStore()
	if err := cacheLiveOr503(cityStore); err != nil {
		return nil, err
	}

	limit := defaultPaginationLimit
	if input.Limit > 0 {
		limit = input.Limit
		if limit > maxPaginationLimit {
			limit = maxPaginationLimit
		}
	}
	// The cursor is a versioned keyset token carrying the (created_at, id)
	// boundary of the last row served — stable under the concurrent writes an
	// active work ledger guarantees, where the old integer offsets skipped or
	// duplicated rows.
	seek, err := beadListSeek(input.Cursor)
	if err != nil {
		return nil, err
	}

	// all=true reads bypass the CachingStore (closed history lives only in
	// the backing store) and are O(full history) per rig — seconds on a
	// large city. Key their response cache on a time bucket so polls within
	// a TTL window reuse the first completed rebuild instead of each
	// rebuilding (#3208, same lever as /status in gascity#3186; there is no
	// single-flight, so simultaneous misses on a cold bucket still rebuild
	// independently). Open-only reads are served from the in-memory cache
	// and stay uncached here; blocking callers bypass so the body reflects
	// the event they waited for.
	cacheKey := ""
	var bucket uint64
	if input.All && !blocking {
		cacheKey = cacheKeyFor("beads", input)
		bucket = responseCacheTimeBucket(time.Now())
		if hit, ok := cachedResponseAs[indexedResponse[ListBody[beads.Bead]]](s, cacheKey, bucket); ok {
			return &ListOutput[beads.Bead]{
				Index:     hit.Index,
				CacheAgeS: cacheAgeSeconds(cityStore),
				Body:      hit.Body,
			}, nil
		}
	}

	stores := s.state.BeadStores()
	assigneeTerms := s.beadListAssigneeTerms(ctx, input.Assignee)
	var rigNames []string
	if input.Rig != "" {
		if _, ok := stores[input.Rig]; ok {
			rigNames = []string{input.Rig}
		}
	} else {
		rigNames = sortedRigNames(stores)
	}
	legs := beadListFanOut(s.state, stores, rigNames, input.Rig != "")

	var all []beads.Bead

	// all=true reads materialize closed history per rig, so the build is
	// O(history) even though the caller only wants a recency-bounded page
	// (gascity#3253). When a store can Count the query exactly, push the page
	// bound down so it returns only the rows this page needs and source its
	// share of Total from a hydration-free Count instead of len(full history).
	// This collapses the FIRST page (no seek boundary) to O(limit) at the store
	// boundary via each backend's native LIMIT; a seeked cursor page disables
	// that native limit and hydrates matching history before the Go-side seek
	// filter (see query.SeekAfter), trading O(limit) fetches for exactness on a
	// deep walk. The response shape (a created_at-desc prefix plus an accurate
	// Total and next_cursor) is unchanged. Scoped to the single-assignee all=true
	// hot path; a WORK store that cannot Count the query keeps the whole request
	// on the full-scan path so Total and ordering stay correct (the Count
	// fallback contract from #3211), while a Counter-less GRAPH leg hydrates
	// alone — see beadListBounding.
	boundedMode := false
	boundedFetch := 0
	var boundedCounts map[int]int
	var hydrateLegs map[int]bool
	if input.All && len(assigneeTerms) == 1 {
		// limit+1: the seek boundary rides on each store query and is enforced
		// Go-side, so a store returns only rows after the boundary — one extra
		// row is the has-more signal (Counts are un-seeked totals and cannot tell).
		boundedFetch = limit + 1
		boundedMode, boundedCounts, hydrateLegs = beadListBounding(ctx, legs, assigneeTerms[0], input)
	}

	// Bead ids are globally unique, so an id is one bead no matter how many legs
	// hold a row for it. A migrated split city really does hold both: `gc storage
	// migrate` copies infrastructure rows into the binding with their ids
	// preserved (CreateWithForeignID) and never deletes them back, so the work
	// store keeps its copy — convergence is DEFINED as that full overlap. First
	// leg wins, and the graph leg is federated last, so the work store's row is
	// the one served — byte-identical to the rule humaHandleBeadReady applies.
	seen := map[string]bool{}
	var pa partialAggregator
	for i, leg := range legs {
		for _, assignee := range assigneeTerms {
			query := beads.ListQuery{
				Status:        input.Status,
				Type:          input.Type,
				Label:         input.Label,
				Assignee:      assignee,
				IncludeClosed: input.All,
				Live:          input.Status == "in_progress",
				// Explicit sort: with SortDefault the CachingStore returns
				// map-iteration order, so a bounded read truncated an
				// arbitrary, per-call-different subset (#3208).
				Sort: beads.SortCreatedDesc,
				// Explicit tier, for the same reason as the sort and with the
				// same failure shape: the zero value is not neutral across these
				// legs. The work legs' bead-policy layer rewrites it to TierBoth
				// and the unwrapped graph leg does not, so the relocated store's
				// ephemeral rows dropped out of an authoritative-looking 200
				// (ga-8lyxc). See beads.FederatedReadTier.
				TierMode: beads.FederatedReadTier,
			}
			if !query.HasFilter() {
				query.AllowScan = true
			}
			legBounded := boundedMode && !hydrateLegs[i]
			if legBounded {
				// Each store need only return enough rows to cover this page;
				// the cross-rig merge below cuts the exact global prefix. On the
				// first page (seek == nil) the native LIMIT makes the per-store
				// fetch O(limit). On a seeked cursor page the boundary is enforced
				// Go-side (backends disable their native limit — see SeekAfter in
				// query.go), so the store hydrates matching history and the fetch
				// is O(matching history), not O(limit); the Go-side filter+sort+
				// limit then cut the exact page. That is the deliberate price of a
				// tie-break identical to the in-memory sort.
				query.Limit = boundedFetch
				query.SeekAfter = seek
			}
			pa.attempt()
			list, err := leg.store.List(query)
			if err != nil {
				if leg.graph {
					// The graph leg is the execution DAG, not a rig: any degraded
					// read there — hard failure or partial — leaves an unnamed hole
					// in a dependency graph, so it fails LOUD instead of degrading
					// to a work-only 200 (the authoritative-failure contract, same
					// as the ready arm). Work-leg failures recorded so far ride
					// along: without them "the graph plane is down" and "the whole
					// city is down" are the same response.
					return nil, graphPlaneUnavailable("list", err, pa.messages()...)
				}
				if beads.IsPartialResult(err) && len(list) > 0 {
					// Partial result: the rig returned rows (appended to `all`
					// below) but flagged a degraded read. Keep its bounded count
					// — these rows ARE reachable, and dropping or shrinking the
					// count risks under-advertising readable rows (silent data
					// loss), strictly worse than the count's slight possible
					// over-advertisement. Only a hard List failure (zero
					// reachable rows, below) drops its count (gascity#3253).
					pa.record(leg.label, err)
					pa.success()
				} else {
					pa.record(leg.label, err)
					if boundedMode {
						// This rig's exact Count was baked into boundedCounts
						// upfront, but its List failed so its rows never reach
						// `all`. Drop its count so Total counts only reachable
						// rows — matching the full-scan accounting under the same
						// partial failure (where total == rows returned) and
						// keeping next_cursor from overshooting (gascity#3253).
						delete(boundedCounts, i)
					}
					continue
				}
			} else {
				pa.success()
			}
			if boundedMode && !legBounded {
				// This leg hydrated because it cannot Count, so the rows it just
				// returned ARE its exact un-seeked total — which is what Total
				// needs, and what keeps Total constant across a cursor walk.
				boundedCounts[i] = len(list)
			}
			for _, b := range list {
				if boundedMode && !legBounded && seek != nil && !seek.After(b, beads.SortCreatedDesc) {
					// A hydrated leg carries no store-side seek boundary (that is
					// what made its row count the un-seeked total), so the page
					// boundary is applied here instead. resolveBeadListPage's
					// bounded branch assumes every row it receives is past it.
					continue
				}
				if seen[b.ID] {
					continue
				}
				seen[b.ID] = true
				all = append(all, b)
			}
		}
	}
	if pa.totalOutage() {
		return nil, pa.outageError()
	}

	if all == nil {
		all = []beads.Bead{}
	}
	// Per-store results are each (created_at, id)-ordered, but the
	// concatenation across rigs and assignee terms is not: re-sort so the
	// merged set has one global total order and a bounded read is a
	// deterministic prefix of it (#3208). A single (leg, assignee) source is
	// already in canonical order — skip the redundant hot-path sort.
	if len(legs)*len(assigneeTerms) > 1 {
		beads.SortBeads(all, beads.SortCreatedDesc)
	}

	cacheAge := cacheAgeSeconds(cityStore)
	// A non-cursor request is first-page paging: a truncated first page
	// carries the continuation cursor too, otherwise the remainder of a
	// limit-bounded read is unfetchable by design (#3208). next_cursor is the
	// keyset boundary of the last row served.
	page, total, hasMore := resolveBeadListPage(all, seek, limit, boundedMode, boundedCounts, pa.partial())
	nextCursor := mintNextCursor(page, hasMore)
	if page == nil {
		page = []beads.Bead{}
	}
	body := ListBody[beads.Bead]{
		Items:         page,
		Total:         total,
		NextCursor:    nextCursor,
		Partial:       pa.partial(),
		PartialErrors: pa.messages(),
	}
	if cacheKey != "" {
		s.storeResponse(cacheKey, bucket, indexedResponse[ListBody[beads.Bead]]{Index: index, Body: body})
	}
	return &ListOutput[beads.Bead]{
		Index:     index,
		CacheAgeS: cacheAge,
		Body:      body,
	}, nil
}

// beadListSeek decodes the GET /v0/beads pagination cursor into a keyset seek
// boundary. An empty cursor is first-page paging (nil boundary, no error). Any
// other non-empty value — garbage, a legacy offset cursor, or a wrong-kind
// token — is a typed 400 rather than a silent restart at page 1, which
// duplicated rows under the old integer-offset scheme.
func beadListSeek(cursor string) (*beads.SeekBoundary, error) {
	if cursor == "" {
		return nil, nil
	}
	c, err := decodeKeysetCursor(cursor)
	if err != nil || c.Kind != cursorKindCreatedID {
		return nil, apierr.InvalidCursor.Msg("cursor is not a valid pagination token; re-fetch the first page")
	}
	return &beads.SeekBoundary{CreatedAt: c.CreatedAt, ID: c.ID}, nil
}

// resolveBeadListPage cuts the response page, Total, and has-more flag from the
// merged result set, which is already in the global (created_at DESC, id DESC)
// order the store fan-out produced. It performs no I/O.
//
// In boundedMode `all` is the limit+1 overfetch prefix and boundedCounts holds
// the exact per-leg un-seeked totals, so Total is their sum (constant across a
// walk) and the extra overfetched row is the has-more signal. A degraded
// (partial) rig can fall short of that signal, so a non-empty partial page
// force-mints a resume cursor to keep the walk going past the degradation
// (gascity#3253). Otherwise `all` is the complete un-seeked set: Total is its
// length and the page is the contiguous suffix strictly after the Go-side seek
// boundary.
//
// One honest limit on the bounded Total: per-leg counts are summed, and a leg
// counted (rather than hydrated) reports its own rows without knowing which of
// them another leg also holds. On a migrated split city whose graph binding CAN
// Count, an id co-resident in the work store and the binding is therefore
// counted twice even though it is served once. Every page and the has-more
// signal are still exact — they come from the deduped row set — so a cursor walk
// terminates on the real end of the set; only the advertised Total runs high,
// the same direction the partial-failure rule already accepts.
func resolveBeadListPage(all []beads.Bead, seek *beads.SeekBoundary, limit int, boundedMode bool, boundedCounts map[int]int, partial bool) (page []beads.Bead, total int, hasMore bool) {
	if boundedMode {
		for _, n := range boundedCounts {
			total += n
		}
		if len(all) > limit {
			hasMore = true
			all = all[:limit]
		}
		page = all
		if partial && len(page) > 0 {
			hasMore = true
		}
		return page, total, hasMore
	}
	// Full-scan path: `all` is the COMPLETE un-seeked set read in one shot, so
	// `end < len(all)` is the honest has-more. The bounded branch's
	// partial→force-resume is intentionally NOT mirrored here: a full-scan
	// request re-reads every rig un-seeked, so a degraded rig reproduces the
	// same withheld rows on the next request and a resume cursor cannot recover
	// them — unlike bounded mode, where each page is an independent per-rig
	// bounded read that can recover on a later page (gascity#3253).
	total = len(all)
	start := 0
	if seek != nil {
		for start < len(all) && !seek.After(all[start], beads.SortCreatedDesc) {
			start++
		}
	}
	end := start + limit
	if end > len(all) {
		end = len(all)
	}
	return all[start:end], total, end < len(all)
}

// mintNextCursor returns the keyset continuation cursor for a truncated page:
// the (created_at, id) boundary of the last row served. An exhausted or empty
// page mints nothing, which the client reads as walk-complete.
//
// The resume key is (created_at, id), which assumes (created_at, id) is
// globally unique across the merged legs. The fan-out's identity key is the
// bead id alone, so the two ways a leg pair can hold the same id — legacy
// file-mode aliasing of the city and rig stores, and the work/binding
// co-residence a migrated split city keeps — are collapsed before the page is
// cut, and no twin can reach a page boundary. A future globally-non-unique ID
// scheme would need a wider resume key here.
func mintNextCursor(page []beads.Bead, hasMore bool) string {
	if !hasMore || len(page) == 0 {
		return ""
	}
	last := page[len(page)-1]
	return encodeKeysetCursor(keysetCursor{
		Kind:      cursorKindCreatedID,
		CreatedAt: last.CreatedAt,
		ID:        last.ID,
	})
}

// beadListLeg is one source in the bead-list fan-out.
//
// Legs are a SLICE, not a map keyed by rig name: the relocated graph store has
// no rig name, and giving it a synthetic one put it in the same namespace as
// real rigs — a rig that happened to carry that name was silently overwritten
// and vanished from the response — while sorting it into the middle of the rig
// order made the merged sequence depend on the city's name.
type beadListLeg struct {
	// label is what a degraded read is reported under in partial_errors.
	label string
	store beads.Store
	// graph marks the relocated graph plane, which does not degrade: it either
	// answers or the request fails loud (see graphPlaneUnavailable).
	graph bool
}

// beadListFanOut builds the ordered leg list for a bead-list request: the work
// stores in rigNames order, then the relocated graph store last.
//
// Graph-class beads (gcg- molecule roots, steps, control beads) live in the
// relocated graph store on a split city, and BeadStores() does not include it —
// without this leg the whole execution DAG is invisible behind an authoritative
// 200. It goes LAST so first-leg-wins id dedupe resolves a bead co-resident in
// both planes to the work store's row, exactly as humaHandleBeadReady does, and
// so the merged order stays a leg concatenation whose prefix is what a legacy
// city already serves.
//
// A rig-scoped request gets no graph leg: it is asking for one rig, and the
// graph plane is not a rig.
func beadListFanOut(state State, stores map[string]beads.Store, rigNames []string, rigScoped bool) []beadListLeg {
	legs := make([]beadListLeg, 0, len(rigNames)+1)
	for _, rigName := range rigNames {
		legs = append(legs, beadListLeg{label: "rig " + rigName, store: stores[rigName]})
	}
	if rigScoped {
		return legs
	}
	if graph := relocatedGraphStore(state); graph != nil {
		legs = append(legs, beadListLeg{label: "graph", store: graph, graph: true})
	}
	return legs
}

// beadListBounding plans, per leg, how the all=true page is bounded.
//
// It returns whether bounding is on at all, the exact un-seeked count of every
// leg it could count (keyed by leg index, not pre-summed, so the caller can drop
// the count of a leg whose subsequent List fails — Total must reflect only rows
// actually reachable, matching the full-scan path under partial failure,
// gascity#3253), and the legs that must hydrate instead.
//
// The WORK legs are all-or-nothing, which is the #3211 Count-fallback contract:
// one work store that cannot Count the query exactly means Total and the
// recency-ordered prefix have to come from the full scan.
//
// The GRAPH leg is deliberately outside that gate. It is one more source, not a
// capability veto: the canonical compiled-in binding hands the API a raw
// *beads.SQLiteStore (internal/storebinding/sqlite/beads_engine.go OpenEngine),
// which implements no Count at all, so letting it into the all-or-nothing gate
// would switch bounded paging off for every leg on precisely the split city this
// federation exists for. When it can Count it is bounded like the rest; when it
// cannot, only that leg hydrates and the caller takes its exact total from the
// rows it returned.
func beadListBounding(ctx context.Context, legs []beadListLeg, assignee string, input *BeadListInput) (on bool, counts map[int]int, hydrate map[int]bool) {
	counts = make(map[int]int, len(legs))
	for i, leg := range legs {
		n, ok := beadListLegCount(ctx, leg.store, assignee, input)
		if ok {
			counts[i] = n
			continue
		}
		if !leg.graph {
			return false, nil, nil
		}
		if hydrate == nil {
			hydrate = make(map[int]bool, 1)
		}
		hydrate[i] = true
	}
	return true, counts, hydrate
}

// beadListLegCount returns one leg's exact un-seeked count, reporting false when
// the store implements no Counter or cannot count this query shape exactly
// (ErrCountUnsupported).
func beadListLegCount(ctx context.Context, store beads.Store, assignee string, input *BeadListInput) (int, bool) {
	counter, ok := store.(beads.Counter)
	if !ok {
		return 0, false
	}
	n, err := counter.Count(ctx, beadListCountQuery(assignee, input))
	if err != nil {
		return 0, false
	}
	return n, true
}

// beadListCountQuery builds the count query for the all=true list path. It
// carries the same filters as the list query so the count matches exactly;
// Sort and Limit are omitted because they do not affect a count. The tier is
// NOT omitted for the same reason: a count taken over a narrower tier than the
// list it bounds advertises a Total the walk can never reach.
func beadListCountQuery(assignee string, input *BeadListInput) beads.ListQuery {
	q := beads.ListQuery{
		Status:        input.Status,
		Type:          input.Type,
		Label:         input.Label,
		Assignee:      assignee,
		IncludeClosed: input.All,
		Live:          input.Status == "in_progress",
		TierMode:      beads.FederatedReadTier,
	}
	if !q.HasFilter() {
		q.AllowScan = true
	}
	return q
}

// readyFederationQuery is the ready query every leg of the ready federation is
// read with. It exists so the work legs and the graph leg cannot be given
// different ones: they are read from two places in the handler below, and the
// defect this closes was exactly the two places disagreeing about the tier
// without either of them naming it. See beads.FederatedReadTier.
func readyFederationQuery() beads.ReadyQuery {
	return beads.ReadyQuery{TierMode: beads.FederatedReadTier}
}

// humaHandleBeadReady is the Huma-typed handler for GET /v0/beads/ready.
//
// FEDERATION CONTRACT — the follow-on CLI federation (`gc ready`, ga-oxsyu) is
// specified against this handler, so its conformance test can assert CLI == API
// instead of inventing an oracle. The rules it has to match:
//
//   - Legs, in order: the city store, then the rigs by name ascending, then the
//     relocated graph store. The CLI composite's legs are work-then-infra, the
//     same relative order, so on a rig-less city the two sequences agree.
//     GET /v0/beads federates the same legs in the same order.
//   - Frame: every rig BeadStores holds, suspended ones included. `gc ready`
//     deliberately does not match this: it federates the serving rigs only,
//     the frame the controller's demand census reads, so a suspended rig's own
//     rows are in this answer and absent from the CLI's (ga-h07n0z; see
//     readyServingRigs in cmd/gc/ready_federation.go). CLI == API holds over a
//     city with no suspended rig.
//   - Within a leg: whatever order that leg's own Ready reader emits. For a
//     work store (CachingStore.Ready) and for the canonical relocated binding
//     (beads.SQLiteStore.Ready) alike that is the canonical (priority ASC,
//     created_at ASC, id ASC) order, so each leg is sorted on its own.
//   - Dedupe: first leg to return an id wins. The graph leg runs last, so a bead
//     co-resident in the work store and the binding — the documented steady
//     state of a migrated city, where the migration preserves ids and never
//     deletes back — resolves to the work store's row on both endpoints.
//   - Merged order: leg concatenation, deliberately NOT re-sorted. A global
//     re-sort would change the bytes a multi-rig single-store city already
//     serves, and the graph leg must be free for such a city. Both sides are
//     therefore compared after normalizing with beads.SortBeadsReadyOrder. That
//     normalization is load-bearing rather than cosmetic, because a
//     concatenation of sorted legs is not itself sorted; it is well-defined
//     because SortBeadsReadyOrder is a total order over the merged set.
//   - Failure: a rig degrades (Partial 200 + partial_errors); the graph leg
//     does not (503, carrying any work-leg errors recorded before it). See
//     graphPlaneUnavailable.
//   - Source: each leg is answered by its ready cache when the cache is warm
//     and can vouch for the answer (beads.ReadyPreferCache: the freshness and
//     invalidation rules of the zero-query CachingStore.Ready), and read live
//     only when it is cold or invalidated. Two limits follow from that. A cache
//     over a backing with no ready projection (MemStore, FileStore, and
//     DoltliteReadStore, whose projection is a no-op) treats a blocker it does
//     not hold as satisfied, where that store's own Ready can hold the
//     dependent back (ga-gmf8r). And a blocking request (?index=N) wakes on
//     its own watch, which can run ahead of the cache absorbing the event that
//     woke it: the body can miss that change while the index moves past it,
//     and the client sees the change on the next event or when its next wait
//     runs out (30s by default, 5m at most). DoltliteReadStore's cache also
//     ignores a closed blocker's gc.work_outcome, but so does its live Ready,
//     so the two agree there (ga-e3zi0s).
//   - Tier: every leg is read at beads.FederatedReadTier, stated explicitly.
//     A no-argument Ready() left TierMode at its zero value, which the work
//     legs' bead-policy layer rewrote to TierBoth while the unwrapped graph leg
//     took literally — so the relocated store's whole ephemeral tier fell out of
//     a 200 that named no failure (ga-8lyxc).
func (s *Server) humaHandleBeadReady(ctx context.Context, input *BeadReadyInput) (*ListOutput[beads.Bead], error) {
	bp := input.toBlockingParams()
	if bp.isBlocking() {
		waitForChange(ctx, s.state.EventProvider(), bp)
	}
	index := s.latestIndex()

	stores := s.state.BeadStores()
	rigNames := sortedRigNames(stores)
	var all []beads.Bead
	var pa partialAggregator
	seen := make(map[string]bool)
	federate := func(label string, store beads.Store) {
		if store == nil {
			return
		}
		pa.attempt()
		ready, err := beads.ReadyPreferCache(store, readyFederationQuery())
		if err != nil {
			if beads.IsPartialResult(err) && len(ready) > 0 {
				pa.record(label, err)
				pa.success()
			} else {
				pa.record(label, err)
				return
			}
		} else {
			pa.success()
		}
		for _, b := range ready {
			if seen[b.ID] {
				// An id is one bead: legacy file mode can alias the city and rig
				// stores, and a migrated split city holds the same infrastructure
				// row in both the work store and the binding.
				continue
			}
			seen[b.ID] = true
			all = append(all, b)
		}
	}
	// City-scope ready work (graph.v2 molecules in a single-HQ city, control
	// beads) lives in the city store, so federate it explicitly first or HTTP
	// `bd ready` would never surface it. In production BeadStores() also returns
	// the city store keyed by CityName() (cmd/gc/api_state.go), so skip that
	// duplicate key in the rig loop below to avoid querying it twice.
	federate("city", s.state.CityBeadStore())
	cityName := s.state.CityName()
	for _, rigName := range rigNames {
		if rigName == cityName {
			continue // city store already federated explicitly above; production
			// BeadStores() also returns it under cityName (cmd/gc/api_state.go)
		}
		federate("rig "+rigName, stores[rigName])
	}
	// Split city: graph-class ready work (gcg- steps, control beads, molecule
	// roots) lives in the relocated graph store, which BeadStores() does not
	// include — federate it or the whole execution DAG is invisible behind an
	// authoritative-looking 200. It runs LAST so the merged order stays a leg
	// concatenation whose prefix is exactly what a legacy city serves, and it
	// does NOT go through federate(): the graph leg has no partial tier.
	if graph := relocatedGraphStore(s.state); graph != nil {
		pa.attempt()
		ready, err := beads.ReadyPreferCache(graph, readyFederationQuery())
		if err != nil {
			return nil, graphPlaneUnavailable("ready", err, pa.messages()...)
		}
		pa.success()
		for _, b := range ready {
			if seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			all = append(all, b)
		}
	}
	if pa.totalOutage() {
		return nil, pa.outageError()
	}

	if all == nil {
		all = []beads.Bead{}
	}

	return &ListOutput[beads.Bead]{
		Index: index,
		Body: ListBody[beads.Bead]{
			Items:         all,
			Total:         len(all),
			Partial:       pa.partial(),
			PartialErrors: pa.messages(),
		},
	}, nil
}

// humaHandleBeadGraph is the Huma-typed handler for GET /v0/beads/graph/{rootID}.
func (s *Server) humaHandleBeadGraph(_ context.Context, input *BeadGraphInput) (*IndexOutput[BeadGraphResponse], error) {
	index := s.latestIndex()
	rootID := input.RootID
	if rootID == "" {
		// Defensive: the {rootID} path segment is required, so the router never
		// dispatches here with an empty id. Unreachable in practice, hence the op
		// does not declare a 400 in its error contract.
		return nil, apierr.InvalidRequest.Msg("rootID is required")
	}

	foundStore, root, err := s.resolveBeadOwner(rootID)
	if err != nil {
		return nil, err
	}

	graphBeads, parentEdges, membership, err := collectBeadGraph(foundStore, root)
	if err != nil {
		return nil, apierr.Internal.Msg(err.Error())
	}
	beadIndex := make(map[string]beads.Bead, len(graphBeads))
	for _, b := range graphBeads {
		beadIndex[b.ID] = b
	}

	deps, depPartial := collectWorkflowDeps(foundStore, beadIndex)
	if depPartial {
		return nil, apierr.Internal.Msg("listing bead graph dependencies failed")
	}
	deps = mergeWorkflowDeps(deps, parentEdges)

	return &IndexOutput[BeadGraphResponse]{
		Index: index,
		Body: BeadGraphResponse{
			Root:       root,
			Beads:      graphBeads,
			Deps:       deps,
			Membership: membership,
		},
	}, nil
}

// humaHandleBeadGet is the Huma-typed handler for GET /v0/bead/{id}.
func (s *Server) humaHandleBeadGet(_ context.Context, input *BeadGetInput) (*IndexOutput[beads.Bead], error) {
	index := s.latestIndex()
	id := input.ID

	cityStore := s.state.CityBeadStore()
	if err := cacheLiveOr503(cityStore); err != nil {
		return nil, err
	}

	_, b, err := s.resolveBeadOwner(id)
	if err != nil {
		return nil, err
	}
	return &IndexOutput[beads.Bead]{
		Index:     index,
		CacheAgeS: cacheAgeSeconds(cityStore),
		Body:      b,
	}, nil
}

// humaHandleBeadDeps is the Huma-typed handler for GET /v0/bead/{id}/deps.
func (s *Server) humaHandleBeadDeps(_ context.Context, input *BeadDepsInput) (*IndexOutput[BeadDepsResponse], error) {
	index := s.latestIndex()
	id := input.ID
	store, parent, err := s.resolveBeadOwner(id)
	if err != nil {
		return nil, err
	}
	children, err := store.List(beads.ListQuery{
		ParentID: id,
		Sort:     beads.SortCreatedAsc,
	})
	if err != nil {
		return nil, apierr.Internal.Msg(err.Error())
	}
	children = appendMetadataAttachedChildren(store, parent, children)
	if children == nil {
		children = []beads.Bead{}
	}
	return &IndexOutput[BeadDepsResponse]{
		Index: index,
		Body:  BeadDepsResponse{Children: children},
	}, nil
}

// BeadDepsResponse is the response shape for GET /v0/bead/{id}/deps.
type BeadDepsResponse struct {
	Children []beads.Bead `json:"children"`
}

// humaHandleBeadCreate is the Huma-typed handler for POST /v0/beads.
// Title required via struct tag on BeadCreateInput.
func (s *Server) humaHandleBeadCreate(ctx context.Context, input *BeadCreateInput) (*IndexOutput[beads.Bead], error) {
	index := s.latestIndex()
	// Idempotency: run the create at most once per Idempotency-Key. The helper
	// owns reserve/replay/mismatch/in-flight and guarantees the reservation is
	// released on any error, so every fallible step lives in the closure. A
	// replay returns the index cached with the body, not the current head.
	res, err := withIndexedIdempotency(s.idem, "/v0/beads", input.IdempotencyKey, input.Body, index,
		func() (beads.Bead, error) {
			candidate := beads.Bead{
				Title:       input.Body.Title,
				Type:        input.Body.Type,
				Priority:    input.Body.Priority,
				Description: input.Body.Description,
				Labels:      input.Body.Labels,
				ParentID:    input.Body.Parent,
				Metadata:    input.Body.Metadata,
				DeferUntil:  input.Body.DeferUntil,
			}
			// Before anything is written, the assignee's materialized session
			// included, and before the store is chosen: an owned row is a 422
			// whatever rig it names, never the placement's 400 (the #7766
			// review).
			if err := refuseGenericWrite("(new)", nil, candidate, genericWrite{meta: candidate.Metadata, labels: candidate.Labels}); err != nil {
				return beads.Bead{}, err
			}
			// The store follows from the bead, not from the request: an
			// infrastructure-class body belongs in this city's class binding
			// wherever the caller posted it from.
			store, err := s.createStoreForBead(candidate, input.Body.Rig)
			if err != nil {
				return beads.Bead{}, err
			}
			candidate.Assignee, err = s.normalizeRawBeadAssignee(ctx, input.Body.Assignee)
			if err != nil {
				return beads.Bead{}, apierr.InvalidRequest.Msg(err.Error())
			}
			created, err := store.Create(candidate)
			if err != nil {
				return beads.Bead{}, apierr.Internal.Msg(err.Error())
			}
			// Some stores return a minimal create envelope and require a
			// follow-up read for the canonical persisted bead state.
			if persisted, getErr := store.Get(created.ID); getErr == nil {
				created = persisted
			}
			return created, nil
		})
	if err != nil {
		return nil, err
	}
	s.wakeAfterBeadWrite()

	return &IndexOutput[beads.Bead]{
		Index: res.Index,
		Body:  res.Body,
	}, nil
}

// humaHandleBeadClose is the Huma-typed handler for POST /v0/bead/{id}/close.
//
// An optional {"reason": "..."} body records why the bead was closed. The
// reason is stamped as metadata.close_reason before the close, the convention
// every store's close honors: BdStore and NativeDoltStore forward it as the
// close reason (bd close --reason), and the whole-row stores record it, so it
// comes back as the bead's close_reason on GET and on bead.closed.
func (s *Server) humaHandleBeadClose(ctx context.Context, input *BeadCloseInput) (*OKResponse, error) {
	id := input.ID
	store, current, err := s.resolveBeadOwner(id)
	if err != nil {
		return nil, err
	}
	if err := refuseOwnedClose(id, "closes", current); err != nil {
		return nil, err
	}
	var submitted map[string]string
	reason := ""
	if input.Body != nil {
		reason = strings.TrimSpace(input.Body.Reason)
	}
	if reason != "" {
		submitted = map[string]string{"close_reason": reason}
	}
	if err := s.gateWorkRecordClose(ctx, id, store, current, submitted); err != nil {
		return nil, err
	}
	// Only a close that happens stamps a reason. On a bead that is not closed,
	// a recorded close_reason is left over from an earlier close (bd's reopen
	// clears its close_reason column but not the metadata), so this close
	// replaces it with its own reason, or with none. On a bead already closed,
	// Close is a no-op that keeps the reason its close recorded, so no other
	// reason is stamped beside it.
	priorReason := current.Metadata["close_reason"]
	stampReason := current.Status != "closed" && (reason != "" || strings.TrimSpace(priorReason) != "")
	if stampReason {
		if err := store.SetMetadata(id, "close_reason", reason); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return nil, apierr.ConflictConcurrentDelete.Msg("conflict: bead " + id + " was deleted concurrently")
			}
			return nil, apierr.Internal.Msg(err.Error())
		}
	}
	if err := store.Close(id); err != nil {
		if stampReason && !errors.Is(err, beads.ErrNotFound) {
			// The bead stays open, so it must not keep the reason stamped
			// for a close that did not happen.
			if restoreErr := store.SetMetadata(id, "close_reason", priorReason); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restoring close_reason after failed close: %w", restoreErr))
			}
		}
		if errors.Is(err, beads.ErrNotFound) {
			return nil, apierr.ConflictConcurrentDelete.Msg("conflict: bead " + id + " was deleted concurrently")
		}
		return nil, apierr.Internal.Msg(err.Error())
	}
	s.wakeAfterBeadWrite()
	resp := &OKResponse{}
	resp.Body.Status = "closed"
	return resp, nil
}

// humaHandleBeadReopen is the Huma-typed handler for POST /v0/bead/{id}/reopen.
func (s *Server) humaHandleBeadReopen(_ context.Context, input *BeadReopenInput) (*OKResponse, error) {
	id := input.ID

	store, b, err := s.resolveBeadOwner(id)
	if err != nil {
		return nil, err
	}
	if err := refuseOwned(id, "reopens", "path.id", b); err != nil {
		return nil, err
	}
	if b.Status != "closed" {
		return nil, apierr.ConflictWrongState.Msg("conflict: bead " + id + " is not closed (status: " + b.Status + ")")
	}
	if err := store.Reopen(id); err != nil {
		return nil, apierr.Internal.Msg(err.Error())
	}
	s.wakeAfterBeadWrite()
	resp := &OKResponse{}
	resp.Body.Status = "reopened"
	return resp, nil
}

// humaHandleBeadAssign is the Huma-typed handler for POST /v0/bead/{id}/assign.
func (s *Server) humaHandleBeadAssign(ctx context.Context, input *BeadAssignInput) (*IndexOutput[map[string]string], error) {
	index := s.latestIndex()
	id := input.ID
	store, current, err := s.resolveBeadOwner(id)
	if err != nil {
		return nil, err
	}
	// Before the assignee's normalization, which may materialize a session.
	if err := refuseOwned(id, "assigns", "path.id", current); err != nil {
		return nil, err
	}
	assignee, err := s.normalizeRawBeadAssignee(ctx, input.Body.Assignee)
	if err != nil {
		return nil, apierr.InvalidRequest.Msg(err.Error())
	}
	// Once Get succeeded in the resolved store, treat Update-ErrNotFound as a
	// concurrent-delete race rather than resolving again — the bead was just
	// there, and a second resolution could land on a different store that
	// happens to share the ID prefix.
	if err := store.Update(id, beads.UpdateOpts{Assignee: &assignee}); err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, apierr.ConflictConcurrentDelete.Msg("conflict: bead " + id + " was deleted concurrently")
		}
		return nil, apierr.Internal.Msg(err.Error())
	}
	s.wakeAfterBeadWrite()
	return &IndexOutput[map[string]string]{
		Index: index,
		Body:  map[string]string{"status": "assigned", "assignee": assignee},
	}, nil
}

// humaHandleBeadUpdate is the Huma-typed handler for POST /v0/bead/{id}/update
// and PATCH /v0/bead/{id}. Body fields are pointer-typed so absent fields
// remain unchanged in the underlying store.
//
// Note on null vs absent: standard Go JSON decoding folds `field: null` and
// "field absent" together — both produce a nil pointer, treated as "no
// change." To keep "clear priority" from silently becoming "no change,"
// beadUpdateBody has a custom UnmarshalJSON that inspects the raw tokens
// and rejects `priority: null` with a 4xx + migration hint. See
// huma_types_beads.go. Clients that want to clear priority must use a
// dedicated endpoint (not yet exposed); sending null is a hard error.
func (s *Server) humaHandleBeadUpdate(ctx context.Context, input *BeadUpdateInput) (*OKResponse, error) {
	id := input.ID
	body := input.Body

	opts := beads.UpdateOpts{
		Title:        body.Title,
		Status:       body.Status,
		Type:         body.Type,
		Priority:     body.Priority,
		Description:  body.Description,
		Labels:       body.Labels,
		RemoveLabels: body.RemoveLabels,
		Metadata:     body.Metadata,
	}
	if body.parentSet {
		parent := ""
		if body.Parent != nil {
			parent = *body.Parent
		}
		opts.ParentID = &parent
	}

	store, current, err := s.resolveBeadOwner(id)
	if err != nil {
		return nil, err
	}
	// Before the assignee's normalization, which may materialize a named
	// session: a refused write creates nothing.
	after := afterUpdate(current, body)
	if err := refuseGenericWrite(id, &current, after, genericWrite{meta: body.Metadata, labels: body.Labels, removeLabels: body.RemoveLabels}); err != nil {
		return nil, err
	}
	if beadUpdateMutation(body).TouchesClass() {
		if err := s.refuseClassFlip(id, store, current, after); err != nil {
			return nil, err
		}
	}
	if body.Assignee != nil {
		assignee, err := s.normalizeRawBeadAssignee(ctx, *body.Assignee)
		if err != nil {
			return nil, apierr.InvalidRequest.Msg(err.Error())
		}
		opts.Assignee = &assignee
	}
	if closesStatus(opts.Status) {
		// This spelling closes the bead, so it answers to the same contract as
		// the close route. body.Metadata rides along because the documented
		// atomic close stamps the work record in this very request.
		if err := s.gateWorkRecordClose(ctx, id, store, current, body.Metadata); err != nil {
			return nil, err
		}
	}
	// Once Get succeeded in the resolved store, treat Update-ErrNotFound as a
	// concurrent-delete race (409) rather than resolving again — otherwise a
	// delete racing with update silently applies the mutation to a different
	// store that happens to share the ID.
	if err := store.Update(id, opts); err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, apierr.ConflictConcurrentDelete.Msg("conflict: bead " + id + " was deleted concurrently")
		}
		return nil, apierr.Internal.Msg(err.Error())
	}
	// The write landed, so it wakes even if the parent projection wait fails;
	// deferred so a woken pass's parent-keyed listings, which bypass the
	// CachingStore, read bd only after the wait.
	defer s.wakeAfterBeadWrite()
	if opts.ParentID != nil && current.ParentID != *opts.ParentID && !closesStatus(opts.Status) {
		if waiter, ok := store.(beads.ParentProjectionWaiter); ok {
			if err := awaitBeadParentProjection(ctx, waiter, id, current.ParentID, *opts.ParentID); err != nil {
				return nil, err
			}
		}
	}
	resp := &OKResponse{}
	resp.Body.Status = "updated"
	return resp, nil
}

// genericRefused reports a row the generic bead API never writes: a
// controller-owned row (coordclass.ControllerOwned; architecture B) or a
// coordination-class row (coordclass.CoordinationOwned; architecture B v2,
// mc-v8g4p). Teardowns skip only the controller-owned ones: an order's wisp
// root carries its order-run label and stays a workflow to tear down.
func genericRefused(b beads.Bead) bool {
	return coordclass.ControllerOwned(b) || coordclass.CoordinationOwned(b)
}

// refuseOwned refuses a generic write to b, a row genericRefused names: the
// generic bead API never writes one, whatever the write (create, update of
// any field, close, delete, reopen, assign, sling, convoy membership). The
// owner's routes do, and bd is the escape hatch. verb names the write, where
// is the refused field's errors[] location.
func refuseOwned(id, verb, where string, b beads.Bead) error {
	if !genericRefused(b) {
		return nil
	}
	kind, owner := ownerOf(b)
	return apierr.ValidationFailed.With(fmt.Sprintf(
		"bead %s is %s, which only %s writes: the generic bead API never %s one; nothing was written", id, withArticle(kind), owner, verb),
		&huma.ErrorDetail{Location: where, Message: "an owned " + kind, Value: id})
}

// refuseOwnedClose is refuseOwned for a generic close or delete (a soft
// close), which one owned row takes: an order's run root, a wisp or
// molecule root its run poured carrying the order-run label and nothing
// else owned (v5.10f, the #7766 review). Closing it ends that run's work, as
// an operator may; reopening, assigning, updating or relabeling it stays
// the owner's.
func refuseOwnedClose(id, verb string, b beads.Bead) error {
	if orderRunRoot(b) {
		return nil
	}
	return refuseOwned(id, verb, "path.id", b)
}

// orderRunRoot reports b, an owned row, as an order's run root: a run record
// (orders.IsRunRecord) whose only owned mark is its order-run label, so no
// tracking bead (its order-tracking label) and nothing else owned.
func orderRunRoot(b beads.Bead) bool {
	if !orders.IsRunRecord(b) || coordclass.ControllerOwned(b) {
		return false
	}
	bare := b
	bare.Labels = slices.DeleteFunc(slices.Clone(b.Labels), func(l string) bool {
		return strings.HasPrefix(strings.ToLower(strings.TrimSpace(l)), "order-run:")
	})
	return !coordclass.CoordinationOwned(bare)
}

// withArticle is kind with its indefinite article.
func withArticle(kind string) string {
	if strings.ContainsRune("aeiou", rune(kind[0])) {
		return "an " + kind
	}
	return "a " + kind
}

// ownerOf is an owned row's kind and who writes it. A typed work bead
// carrying the session label is no session any verb owns, so bd is its only
// writer (the #7742 review).
func ownerOf(b beads.Bead) (kind, owner string) {
	if coordclass.ControllerOwned(b) {
		switch t := strings.ToLower(strings.TrimSpace(b.Type)); t {
		case session.StartupHealthEpisodeType:
			return "startup-health episode", "the controller's startup health"
		case "convergence":
			return "convergence root", "the convergence handler (gc converge, the convergence routes)"
		case opjournal.BeadType:
			return "operation-journal row", "the operation journal (the session verbs that record ops, the controller's op sweeper and lanes)"
		case session.BeadType, "":
			return "session row", "the session verbs (POST /v0/city/{cityName}/sessions and /session/{id}/..., /pin, /unpin, gc session)"
		}
		return "bead carrying the " + session.LabelSession + " label", "bd (bd update, bd close)"
	}
	if coordclass.CoordinationOwnedType(b.Type) {
		return "mail message", "the mail routes (POST /v0/city/{cityName}/mail, /mail/{id}/..., gc mail)"
	}
	label := ""
	if i := coordclass.CoordinationOwnedLabel(b.Type, b.Labels, b.Metadata); i >= 0 {
		label = strings.ToLower(strings.TrimSpace(b.Labels[i]))
	}
	switch {
	case strings.HasPrefix(label, "gc:nudge"):
		return "queued nudge", "the nudge queue (gc nudge, the session message routes)"
	case label == "order-tracking" || strings.HasPrefix(label, "order-run:"):
		return "order run record", "order dispatch, and bd (bd update, bd close) or the workflow's teardown (DELETE /v0/city/{cityName}/workflow/{id}, POST /v0/city/{cityName}/runs/{id}/cancel) for an operator; a run's root alone also takes a generic close"
	case strings.HasPrefix(label, "gc:extmsg-"):
		return "external-messaging row", "the extmsg routes (/v0/city/{cityName}/extmsg/...)"
	}
	return "session wait", "gc session wait and the controller's waits step"
}

// refuseGenericWrite refuses, before anything is written (an assignee's
// materialized session included), a generic bead create or update
// (architecture B; ARCH-RESTRUCTURE-2 R7):
//   - on any bead, a metadata key that fails beadmeta.ValidKey: a bd-backed
//     store would split "wait_hold=true" into another key; and a label that
//     fails beadmeta.ValidLabel, which bd reads as a CSV record
//     ("x,gc:session" or "gc:session\n" would add the session label);
//   - any write to a row genericRefused names (refuseOwned), and any that
//     would make one, at the type, label or session_id that does.
//
// before is the row as read, nil for a create; after is the row with the
// request's type and labels applied. Each refusal is 422 with an errors[]
// entry per offending field.
func refuseGenericWrite(id string, before *beads.Bead, after beads.Bead, w genericWrite) error {
	var invalid []*huma.ErrorDetail
	for _, k := range slices.Sorted(maps.Keys(w.meta)) {
		if !beadmeta.ValidKey(k) {
			invalid = append(invalid, &huma.ErrorDetail{Location: "body.metadata." + k, Message: "invalid metadata key: must match " + beadmeta.ValidKeyPattern, Value: k})
		}
	}
	for field, labels := range map[string][]string{"labels": w.labels, "remove_labels": w.removeLabels} {
		for i, l := range labels {
			if !beadmeta.ValidLabel(l) {
				invalid = append(invalid, &huma.ErrorDetail{Location: fmt.Sprintf("body.%s[%d]", field, i), Message: "a label must read back as itself from one CSV record: no comma, quote, newline or carriage return (bd reads labels as a CSV list)", Value: l})
			}
		}
	}
	if len(invalid) > 0 {
		slices.SortFunc(invalid, func(a, b *huma.ErrorDetail) int { return strings.Compare(a.Location, b.Location) })
		return apierr.ValidationFailed.With("bead "+id+": invalid metadata key or label; nothing was written", invalid...)
	}
	if before != nil {
		if err := refuseOwned(id, "writes", "path.id", *before); err != nil {
			return err
		}
	}
	if !genericRefused(after) {
		return nil
	}
	kind, owner := ownerOf(after)
	msg := "makes the row an owned " + kind
	detail := &huma.ErrorDetail{Location: "body.type", Message: msg, Value: after.Type}
	switch i, j := coordclass.ControllerOwnedLabel(w.labels), coordclass.CoordinationOwnedLabel(after.Type, w.labels, after.Metadata); {
	case i >= 0:
		detail = &huma.ErrorDetail{Location: fmt.Sprintf("body.labels[%d]", i), Message: msg, Value: w.labels[i]}
	case j >= 0:
		detail = &huma.ErrorDetail{Location: fmt.Sprintf("body.labels[%d]", j), Message: msg, Value: w.labels[j]}
	case !coordclass.ControllerOwnedType(after.Type) && !coordclass.CoordinationOwnedType(after.Type):
		// A wait made by its session_id: a gate carrying session:<id>.
		if _, ok := w.meta["session_id"]; ok {
			detail = &huma.ErrorDetail{Location: "body.metadata.session_id", Message: msg, Value: after.Metadata["session_id"]}
		}
	}
	return apierr.ValidationFailed.With(fmt.Sprintf(
		"bead %s: the generic bead API never makes %s, which only %s writes; nothing was written", id, withArticle(kind), owner), detail)
}

// genericWrite is what a generic create or update asks to write: its
// metadata, and the labels it adds and removes.
type genericWrite struct {
	meta                 map[string]string
	labels, removeLabels []string
}

// afterUpdate is current with the update's type, label and metadata changes
// applied: the row the update would leave, as far as whether it is owned.
//
// It is the update stated as a coordclass.Mutation, the shape the
// class-must-not-flip guard (coordclass.CheckFlip) judges for every door.
func afterUpdate(current beads.Bead, body beadUpdateBody) beads.Bead {
	return beadUpdateMutation(body).Apply(current)
}

// beadUpdateMutation is a generic update's type, label and metadata writes.
func beadUpdateMutation(body beadUpdateBody) coordclass.Mutation {
	return coordclass.Mutation{
		Type:         body.Type,
		AddLabels:    body.Labels,
		RemoveLabels: body.RemoveLabels,
		SetMetadata:  body.Metadata,
	}
}

// refuseClassFlip is the bead API's door onto the one class-must-not-flip
// guard (coordclass.CheckFlip). On a split city a work store holds only work
// beads and a class binding only its classes, so a write that changes a bead's
// class in place — a work bead given gc.root_bead_id, a wisp label or gc:rt; a
// graph step whose only graph marker is blanked — strands it in a store its
// class is never read from. One stranded row makes every later routed gc
// command refuse. It is refused here with 422 and nothing is written.
//
// A single-store city holds every class in its one store, so storeClassHolds
// answers nil there and the guard is silent: no behavior change.
func (s *Server) refuseClassFlip(id string, store beads.Store, before, after beads.Bead) error {
	flip := coordclass.CheckFlip(before, after, s.storeClassHolds(store))
	if flip == nil {
		return nil
	}
	return apierr.ValidationFailed.With(fmt.Sprintf("bead %s: %s. %s", id, flip.Error(), flip.Guidance()),
		&huma.ErrorDetail{Location: "body", Message: fmt.Sprintf("changes the bead's class from %s to %s", flip.Before, flip.After), Value: id})
}

// storeClassHolds returns the classes store holds on this city, or nil on a
// single-store city (where the one store holds every class). A class binding
// holds the classes it serves; a work store — the city's or a rig's — holds
// work plus every class no binding serves.
func (s *Server) storeClassHolds(store beads.Store) []coordclass.Class {
	topo := s.residencyTopology()
	if topo.IsSingleStore() {
		return nil
	}
	for _, binding := range topo.Bindings {
		if binding.Leg.Store == store { // residency:allow — identifies which binding the already-resolved owner store is, to read its classes; resolves no bead
			return binding.Classes
		}
	}
	holds := []coordclass.Class{coordclass.ClassWork}
	for _, class := range coordclass.Classes() {
		if _, served := topo.BindingFor(class); class.IsInfrastructure() && !served {
			holds = append(holds, class)
		}
	}
	return holds
}

// awaitBeadParentProjection waits, for at most
// beadParentProjectionWaitTimeout, until the parent-child listings reflect a
// reparent of bead id that the store has already applied, and maps a failed
// wait to its HTTP error.
func awaitBeadParentProjection(ctx context.Context, waiter beads.ParentProjectionWaiter, id, oldParentID, newParentID string) error {
	waitCtx, cancel := context.WithTimeout(ctx, beadParentProjectionWaitTimeout)
	defer cancel()
	err := waiter.WaitForParentProjection(waitCtx, id, oldParentID, newParentID)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, beads.ErrParentProjectionSuperseded):
		return apierr.ConflictConcurrentModify.Msg("conflict: bead " + id + " was reparented concurrently")
	case errors.Is(err, beads.ErrNotFound):
		return apierr.ConflictConcurrentDelete.Msg("conflict: bead " + id + " was deleted concurrently")
	}
	log.Printf("api: bead %s parent projection confirmation failed after update: %v", id, err)
	// Classify by why this wait stopped: its own context says so directly,
	// whereas err may also carry an earlier backend timeout from inside the
	// wait. Reading it before cancel runs keeps that answer.
	stopped := waitCtx.Err()
	if stopped == nil {
		stopped = err
	}
	switch {
	case errors.Is(stopped, context.DeadlineExceeded):
		return apierr.GatewayTimeout.Msg("bead " + id + " was updated, but its parent projection was not confirmed before the timeout")
	case errors.Is(stopped, context.Canceled):
		return apierr.ServiceUnavailable.Msg("bead " + id + " was updated, but parent projection confirmation was canceled")
	default:
		return apierr.Internal.Msg(err.Error())
	}
}

// humaHandleBeadDelete is the Huma-typed handler for DELETE /v0/bead/{id}.
// It is implemented as a soft-delete (store.Close) — see the `"closed"`
// status field for honest wire-contract semantics. Hard-delete is not
// exposed through the API.
//
// The work-record close gate deliberately does not apply here. Delete says
// "this bead should not exist", not "this work completed", so demanding a work
// record would make an unwanted bead undeletable under enforcement; the CLI
// plane draws the same line, gating `bd close` and `bd update --status closed`
// but not `bd delete`.
//
// The same exclusion covers bulk teardown: the paths that close a whole
// workflow root or scope at once through beads.Store.CloseAll rather than
// through a per-bead route — run cancel and workflow delete/purge here, and the
// convoy-cleanup and scope-abort paths on the CLI and dispatch side. They select
// the subtree by gc.root_bead_id with no gc.kind filter, so they do close beads
// this contract otherwise covers, and they are ungated symmetrically on both
// planes rather than leaking on one. That is deliberate: they stamp the disjoint
// control-plane gc.outcome vocabulary (canceled, skipped) precisely because the
// close does not assert completion — see skipScopeMembers in internal/dispatch —
// and requiring a work record would make a run uncancellable and a workflow
// impossible to tear down under enforcement. None is an escape hatch for a
// refused per-bead close: each tears down an entire subtree rather than one bead.
func (s *Server) humaHandleBeadDelete(_ context.Context, input *BeadDeleteInput) (*OKResponse, error) {
	id := input.ID
	store, current, err := s.resolveBeadOwner(id)
	if err != nil {
		return nil, err
	}
	if err := refuseOwnedClose(id, "deletes", current); err != nil {
		return nil, err
	}
	if err := store.Close(id); err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, apierr.ConflictConcurrentDelete.Msg("conflict: bead " + id + " was deleted concurrently")
		}
		return nil, apierr.Internal.Msg(err.Error())
	}
	s.wakeAfterBeadWrite()
	resp := &OKResponse{}
	resp.Body.Status = "closed"
	return resp, nil
}

// wakeAfterBeadWrite asks the controller to reconcile after a successful
// generic bead write. The write lands through the controller's CachingStore
// as a cache-authored bead event, which never wakes the legacy reconciler,
// so without this an API-written bead waits for the patrol interval. One
// call: one legacy poke plus the control-dispatcher key, the pair sling
// wakes. Under v2 this marks the planner on every write, including writes
// OnBeadEvent's relevance filter skips (critic D7); v2 is trace-only in
// builds, and gating this to the legacy reconciler is a tracked follow-up.
func (s *Server) wakeAfterBeadWrite() {
	s.state.Enqueue(reconcilekey.Allocator(), reconcilekey.ControlDispatch())
}
