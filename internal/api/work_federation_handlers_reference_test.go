package api

// Frozen reference copies (typed-stores T7e.2, plan principle 3) of the API
// readers T7e.2 moves onto storeref's WorkFederation legs, whole, as they
// stand on main at 6bd609f5bf: GET /v0/beads (humaHandleBeadList with
// beadListBounding and beadListLegCount), the status work counts
// (statusWorkCounts and its per-store helpers), the status session snapshot,
// store health's row count (computeStoreHealth, countBeadStoreRows,
// measuredStoreRows) and the retired-session hand-off
// (handOffRetiredNamedSessionState). Their leg builders are the T7b.3 and T7d
// copies (work_federation_legs_reference_test.go, workflow_legs_reference_test.go)
// and the reassign functions the T7b.3 copies. The equivalence table
// (work_federation_handlers_equivalence_test.go) runs each beside the live
// reader.
//
// The copies were cut from main by script and differ from it only where main
// called a function T7e.2 retypes:
//   - each call of a copied function names its copy;
//   - State.ScopedStoreLike(ctx, store), whose raw signature T7e.2 retires,
//     is frozenScopedStoreLike(ctx, state, store): the fake's raw hook, the
//     store-in, store-out answer main's method gave;
//   - beadListAssigneeTerms, which takes the request's epoch since T7e.2, is
//     handed s.state.Stores(), the read main made inside it.
//
// Everything else the copies call is live code T7e.2 leaves byte-identical.
// The copies keep main's code, read markers included: markers in test files
// are neither linted (storeclass) nor ledgered (reviewedMarkers).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storehealth"
)

// frozenScopedStoreLike is main's State.ScopedStoreLike for the fixtures'
// fakeState: its raw scopedStoreFn hook, and (nil, nil), "not bd-backed",
// without one.
func frozenScopedStoreLike(ctx context.Context, state State, existing beads.Store) (beads.Store, error) {
	var fs *fakeState
	switch st := state.(type) {
	case *fakeState:
		fs = st
	case relicReportingState:
		fs = st.fakeState
	case *t7dEpochSwapState:
		fs = st.fakeState
	}
	if fs == nil || fs.scopedStoreFn == nil {
		return nil, nil
	}
	return fs.scopedStoreFn(ctx, existing)
}

// frozenHumaHandleBeadList is main's humaHandleBeadList.
func frozenHumaHandleBeadList(ctx context.Context, s *Server, input *BeadListInput) (*ListOutput[beads.Bead], error) {
	bp := input.toBlockingParams()
	blocking := bp.isBlocking()
	if blocking {
		waitForChange(ctx, s.state.EventProvider(), bp)
	}
	index := s.latestIndex()

	work := s.state.Stores().Work()
	if err := cacheLiveOr503(work); err != nil {
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
			freshness := cacheAgeSeconds(work)
			return &ListOutput[beads.Bead]{
				Index:        hit.Index,
				CacheAgeS:    freshness.ageS,
				CacheRefresh: freshness.refresh,
				Body:         hit.Body,
			}, nil
		}
	}

	stores := s.state.BeadStores()
	assigneeTerms := s.beadListAssigneeTerms(ctx, s.state.Stores(), input.Assignee)
	var rigNames []string
	if input.Rig != "" {
		if _, ok := stores[input.Rig]; ok {
			rigNames = []string{input.Rig}
		}
	} else {
		rigNames = frozenSortedRigNames(stores)
	}
	legs := frozenBeadListFanOut(s.state, stores, rigNames, input.Rig != "")

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
		boundedMode, boundedCounts, hydrateLegs = frozenBeadListBounding(ctx, legs, assigneeTerms[0], input)
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
			// One leg of the /beads fan-out (work legs, then the class
			// binding): each lists only its own rows, so a type or label the
			// other legs own misses here by design.
			var list []beads.Bead
			err := beads.FederatedRead(func() error {
				var listErr error
				list, listErr = leg.store.List(query)
				return listErr
			})
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

	freshness := cacheAgeSeconds(work)
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
		Index:        index,
		CacheAgeS:    freshness.ageS,
		CacheRefresh: freshness.refresh,
		Body:         body,
	}, nil
}

// frozenBeadListBounding is main's beadListBounding.
func frozenBeadListBounding(ctx context.Context, legs []frozenBeadListLeg, assignee string, input *BeadListInput) (on bool, counts map[int]int, hydrate map[int]bool) {
	counts = make(map[int]int, len(legs))
	for i, leg := range legs {
		n, ok := frozenBeadListLegCount(ctx, leg.store, assignee, input)
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

// frozenBeadListLegCount is main's beadListLegCount.
func frozenBeadListLegCount(ctx context.Context, store beads.Store, assignee string, input *BeadListInput) (int, bool) {
	counter, ok := beads.As[beads.Counter](store)
	if !ok {
		return 0, false
	}
	n, err := counter.Count(ctx, beadListCountQuery(assignee, input))
	if err != nil {
		return 0, false
	}
	return n, true
}

// frozenStatusSessionSnapshot is main's statusSessionSnapshot.
func frozenStatusSessionSnapshot(ctx context.Context, s *Server) statusSessionSnapshot {
	snapshot := statusSessionSnapshot{
		bySessionName: make(map[string]statusSessionInfo),
		byTemplate:    make(map[string][]statusSessionInfo),
	}
	sessions := s.state.Stores().Sessions()
	store := sessions.Store
	if store == nil {
		// A nil session-class store is benign only when the city has no bead
		// store at all. When the work store IS present, the sessions binding
		// failed to resolve and this projection cannot see the class: say so
		// rather than reporting an empty fleet, and do NOT fall back to the
		// work store. Reading session beads off the work ledger is precisely
		// what kept this mis-routing invisible.
		if s.state.CityBeadStore() != nil {
			snapshot.partialErrors = []string{"sessions: session-class bead store unavailable"}
		}
		return snapshot
	}

	// reqCtx bounds the scoped-store read below; defer cancel() fires on
	// every return path (including the time.After timeout), killing an
	// in-flight bd child instead of leaking it past this function's budget
	// (gascity ga-cdmx6x).
	reqCtx, cancel := context.WithTimeout(ctx, statusStoreReadTimeout)
	defer cancel()

	type snapshotResult struct {
		infos         []session.Info
		partialErrors []string
		err           error
	}
	done := make(chan snapshotResult, 1)
	go func() {
		// Resolve the ctx-bound scoped store INSIDE the timed goroutine.
		// ScopedStoreLike hands back a bd-CLI-backed clone reqCtx can cancel,
		// or (nil, nil) for non-bd backends (native/file/mem) — those read
		// through store unchanged. Its resolution (bd env / managed-dolt
		// connection state) is synchronous and can block on a mutex the
		// reconcile loop holds without honoring reqCtx; kept before the select
		// it hung the whole handler past its read budget, dragging the
		// supervisor loop (gc-08qgn). Under the goroutine the same time.After
		// as the read bounds it.
		readSessions := sessions
		if scoped, err := frozenScopedStoreLike(reqCtx, s.state, store); err != nil {
			done <- snapshotResult{err: fmt.Errorf("resolving scoped store: %w", err)}
			return
		} else if scoped != nil {
			readSessions = beads.SessionStore{Store: scoped}
		}
		infos, partialErrors, err := sessionReadModelInfos(session.NewStore(readSessions))
		done <- snapshotResult{infos: infos, partialErrors: partialErrors, err: err}
	}()

	var infos []session.Info
	var partialErrors []string
	var err error
	select {
	case result := <-done:
		infos = result.infos
		partialErrors = result.partialErrors
		err = result.err
	case <-time.After(statusStoreReadTimeout):
		snapshot.partialErrors = []string{fmt.Sprintf("sessions: loading session snapshot timed out after %s", statusStoreReadTimeout)}
		return snapshot
	}

	if err != nil {
		snapshot.partialErrors = []string{fmt.Sprintf("sessions: %v", err)}
		return snapshot
	}
	for _, partialErr := range partialErrors {
		snapshot.partialErrors = append(snapshot.partialErrors, fmt.Sprintf("sessions: %s", partialErr))
	}

	seenSessionName := make(map[string]bool, len(infos))
	for _, sessInfo := range infos {
		if sessInfo.Closed {
			continue
		}
		info := statusSessionInfo{
			id:          strings.TrimSpace(sessInfo.ID),
			sessionName: strings.TrimSpace(sessInfo.SessionNameMetadata),
			agentName:   strings.TrimSpace(sessInfo.AgentName),
			template:    strings.TrimSpace(sessInfo.Template),
			state:       statusSessionStateInfo(sessInfo),
		}
		if info.sessionName == "" {
			continue
		}
		if info.state == session.StateArchived {
			continue
		}
		if seenSessionName[info.sessionName] {
			continue
		}
		seenSessionName[info.sessionName] = true
		snapshot.bySessionName[info.sessionName] = info
		if info.template != "" {
			snapshot.byTemplate[info.template] = append(snapshot.byTemplate[info.template], info)
		}
	}
	return snapshot
}

// frozenStatusWorkCounts is main's statusWorkCounts.
func frozenStatusWorkCounts(ctx context.Context, s *Server, cacheColdRigs map[string]bool) (workCounts, []string) {
	stores := s.state.BeadStores()
	// sortedRigNames deduplicates rigs sharing one store instance, so each
	// store's persisted statuses are counted exactly once.
	rigNames := frozenSortedRigNames(stores)
	type workQuery struct {
		label         string
		store         beads.Store
		includeStored bool
		includeReady  bool
	}
	queries := make([]workQuery, 0, len(rigNames)+1)
	if cityStore := s.state.CityBeadStore(); cityStore != nil {
		queries = append(queries, workQuery{
			label:        "city",
			store:        cityStore,
			includeReady: true,
		})
	}
	cityName := s.state.CityName()
	excluded := 0
	for _, rigName := range rigNames {
		if cacheColdRigs[rigName] {
			excluded++
			continue
		}
		queries = append(queries, workQuery{
			label:         "rig " + rigName,
			store:         stores[rigName],
			includeStored: true,
			includeReady:  rigName != cityName,
		})
	}

	if graph := s.state.Stores().Graph().Store; graph != nil && !slices.ContainsFunc(queries, func(q workQuery) bool { return q.store == graph }) {
		queries = append(queries, workQuery{label: "graph", store: graph, includeReady: true})
	}

	results := make([]statusWorkResult, len(queries))
	var wg sync.WaitGroup
	for i, query := range queries {
		wg.Add(1)
		go func(i int, query workQuery) {
			defer wg.Done()
			results[i] = frozenStatusStoreWorkCountsFor(
				ctx,
				s.state,
				query.label,
				query.store,
				query.includeStored,
				query.includeReady,
			)
		}(i, query)
	}
	wg.Wait()

	var wc workCounts
	var errs []string
	seenReady := make(map[string]bool)
	for _, r := range results {
		wc.Open += r.wc.Open
		wc.InProgress += r.wc.InProgress
		for _, id := range r.readyIDs {
			if seenReady[id] {
				continue
			}
			seenReady[id] = true
			wc.Ready++
		}
		errs = append(errs, r.errs...)
	}
	wc.SuspendedRigsExcluded = excluded
	return wc, errs
}

// frozenStatusStoreWorkCountsFor is main's statusStoreWorkCountsFor.
func frozenStatusStoreWorkCountsFor(
	ctx context.Context,
	state State,
	label string,
	store beads.Store,
	includeStored bool,
	includeReady bool,
) statusWorkResult {
	ctx, cancel := context.WithTimeout(ctx, statusStoreReadTimeout)
	defer cancel()

	type storedResult struct {
		wc  workCounts
		err error
	}
	type readyResult struct {
		rows []beads.Bead
		err  error
	}
	storedDone := make(chan storedResult, 1)
	stored := &storedResult{}
	if includeStored {
		stored = nil
		go func() {
			wc, err := frozenStatusStoredWorkCounts(ctx, state, store)
			storedDone <- storedResult{wc: wc, err: err}
		}()
	}
	ready := &readyResult{}
	if includeReady {
		// ContextReadyReader and ScopedStoreLike both guarantee cleanup before
		// returning after cancellation. Invoke this branch synchronously so the
		// coordinator cannot return while scoped resolution is still cleaning up.
		rows, err := frozenStatusReadyStoreWithTimeout(ctx, state, store)
		ready = &readyResult{rows: rows, err: err}
	}

	for stored == nil {
		select {
		case value := <-storedDone:
			stored = &value
			storedDone = nil
		case <-ctx.Done():
			// Prefer a result that completed at the deadline boundary. The channel
			// is buffered, so a context-blind legacy operation can finish later
			// without blocking on its abandoned result send.
			if stored == nil {
				select {
				case value := <-storedDone:
					stored = &value
					storedDone = nil
				default:
				}
			}
			if stored == nil {
				err := ctx.Err()
				if errors.Is(err, context.DeadlineExceeded) {
					err = fmt.Errorf("stored counts timed out: %w", err)
				}
				stored = &storedResult{err: err}
			}
		}
	}

	result := statusWorkResult{wc: stored.wc}
	if stored.err != nil {
		result.errs = append(result.errs, fmt.Sprintf("%s work: %v", label, stored.err))
	}
	if ready.err != nil {
		result.errs = append(result.errs, fmt.Sprintf("%s work ready: %v", label, ready.err))
	}
	if ready.err == nil || (beads.IsPartialResult(ready.err) && len(ready.rows) > 0) {
		result.wc.Ready = len(ready.rows)
		result.readyIDs = make([]string, 0, len(ready.rows))
		for _, row := range ready.rows {
			result.readyIDs = append(result.readyIDs, row.ID)
		}
	}
	return result
}

// frozenStatusStoredWorkCounts is main's statusStoredWorkCounts.
func frozenStatusStoredWorkCounts(ctx context.Context, state State, store beads.Store) (workCounts, error) {
	if counter, ok := beads.As[beads.Counter](store); ok {
		wc, err := statusCountWork(ctx, counter)
		if err == nil || !errors.Is(err, beads.ErrCountUnsupported) {
			return wc, err
		}
	}

	list, err := frozenStatusListStoreWithTimeout(ctx, state, store, beads.ListQuery{AllowScan: true})
	var wc workCounts
	if err != nil && (!beads.IsPartialResult(err) || len(list) == 0) {
		return wc, err
	}
	for _, b := range list {
		if slices.Contains(statusWorkExcludedTypes, b.Type) {
			continue
		}
		switch b.Status {
		case "in_progress":
			wc.InProgress++
		case "open":
			wc.Open++
		}
	}
	return wc, err
}

// frozenStatusListStoreWithTimeout is main's statusListStoreWithTimeout.
func frozenStatusListStoreWithTimeout(ctx context.Context, state State, store beads.Store, query beads.ListQuery) ([]beads.Bead, error) {
	if store == nil {
		return nil, nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, statusStoreReadTimeout)
	defer cancel()
	if err := reqCtx.Err(); err != nil {
		return nil, err
	}
	type listResult struct {
		rows []beads.Bead
		err  error
	}
	done := make(chan listResult, 1)
	go func() {
		// Resolve the ctx-bound scoped store INSIDE the timed goroutine so a
		// slow, ctx-blind resolution (a store mutex held by the reconcile
		// loop) is bounded by the same request deadline as the list instead of
		// hanging the handler synchronously (gc-08qgn).
		readStore := store
		if scoped, err := frozenScopedStoreLike(reqCtx, state, store); err != nil {
			done <- listResult{err: fmt.Errorf("resolving scoped store: %w", err)}
			return
		} else if scoped != nil {
			readStore = scoped
		}
		rows, err := readStore.List(query)
		done <- listResult{rows: rows, err: err}
	}()
	select {
	case result := <-done:
		return result.rows, result.err
	case <-reqCtx.Done():
		if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("list timed out: %w", reqCtx.Err())
		}
		return nil, reqCtx.Err()
	}
}

// frozenStatusReadyStoreWithTimeout is main's statusReadyStoreWithTimeout.
func frozenStatusReadyStoreWithTimeout(ctx context.Context, state State, store beads.Store) ([]beads.Bead, error) {
	if store == nil {
		return nil, nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, statusStoreReadTimeout)
	defer cancel()
	if err := reqCtx.Err(); err != nil {
		return nil, err
	}
	var capabilityErr error
	if reader, ok := beads.As[beads.ContextReadyReader](store); ok {
		rows, err := reader.ReadyContext(reqCtx)
		if !errors.Is(err, beads.ErrReadyContextUnsupported) {
			return rows, statusReadyError(err)
		}
		capabilityErr = err
	}

	// ScopedStoreLike is part of the context-aware read contract: resolution
	// must finish its own cleanup before returning after reqCtx cancellation.
	// Keep it synchronous so the status deadline cannot abandon a resolver
	// goroutine after the response has returned.
	scoped, err := frozenScopedStoreLike(reqCtx, state, store)
	if err != nil {
		return nil, statusReadyError(fmt.Errorf("resolving scoped store: %w", err))
	}
	if scoped == nil {
		if capabilityErr == nil {
			capabilityErr = fmt.Errorf("reading canonical ready projection: %w", beads.ErrReadyContextUnsupported)
		}
		return nil, capabilityErr
	}

	// ScopedStoreLike guarantees the clone and its legacy Ready operation are
	// bound to reqCtx, including child-process cleanup, so no outer goroutine is
	// needed to enforce the deadline.
	rows, err := beads.HandlesFor(scoped).Live.Ready()
	return rows, statusReadyError(err)
}

// frozenComputeStoreHealth is main's computeStoreHealth.
func frozenComputeStoreHealth(ctx context.Context, s *Server) (*StatusStoreHealth, error) {
	cityPath := s.state.CityPath()
	if cityPath == "" {
		return nil, nil
	}
	// WalkSize is a synchronous, uncancellable disk walk; the
	// storeHealthCacheTTL cache bounds how often it runs. Plumbing
	// context/timeout through WalkSize is deferred until it shows up
	// in profiles.
	size := storehealth.WalkSize(storehealth.StorePath(cityPath))
	rows, err := frozenCountBeadStoreRows(ctx, s.state, s.state.CityBeadStore())
	if err != nil {
		return nil, err
	}
	// countBeadStoreRows returns an error (handled above) rather than a
	// fabricated count on every failure path, and refuses a zero the store's
	// own row witness contradicts, so rows here is a count this process holds
	// no evidence against. That is what the true below asserts.
	h := storehealth.Compute(cityPath, size, rows, true)
	return statusStoreHealthFromDomain(h), nil
}

// frozenCountBeadStoreRows is main's countBeadStoreRows.
func frozenCountBeadStoreRows(ctx context.Context, state State, store beads.Store) (int, error) {
	if store == nil {
		return 0, errors.New("counting retained bead rows: store unavailable")
	}
	query := beads.ListQuery{AllowScan: true, IncludeClosed: true}
	if counter, ok := beads.As[beads.Counter](store); ok {
		// cachedStoreHealth strips the request deadline, so bound the count
		// here the same way the status handler bounds its store reads.
		reqCtx, cancel := context.WithTimeout(ctx, statusStoreReadTimeout)
		n, err := counter.Count(reqCtx, query)
		cancel()
		if err == nil {
			return frozenMeasuredStoreRows(store, n, countBranchCounter)
		}
		if !errors.Is(err, beads.ErrCountUnsupported) {
			return 0, fmt.Errorf("counting retained bead rows: %s: %w", countBranchCounter, err)
		}
	}
	list, err := frozenStatusListStoreWithTimeout(ctx, state, store, query)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = retainedListOverrun{err: err}
		}
		return 0, fmt.Errorf("counting retained bead rows: %s: %w", countBranchList, err)
	}
	return frozenMeasuredStoreRows(store, len(list), countBranchList)
}

// frozenMeasuredStoreRows is main's measuredStoreRows.
func frozenMeasuredStoreRows(store beads.Store, rows int, branch string) (int, error) {
	if rows > 0 {
		return rows, nil
	}
	witness, ok := beads.As[beads.RowWitness](store)
	if !ok || !witness.SawRows() {
		return rows, nil
	}
	return 0, fmt.Errorf("counting retained bead rows: %s: %w", branch, errRowCountContradicted)
}

// frozenHandOffRetiredNamedSessionState is main's handOffRetiredNamedSessionState.
func frozenHandOffRetiredNamedSessionState(ctx context.Context, s *Server, store beads.SessionStore, retired []session.Info, replacementID string, moveWork func(store beads.Store, oldID, newID string) error) error {
	if store == (beads.SessionStore{}) || strings.TrimSpace(replacementID) == "" || len(retired) == 0 {
		return nil
	}
	now := time.Now().UTC()
	workStores := frozenRetiredSessionWorkStores(s, store)
	// The bindings and participants are messaging-class: they move in the
	// city's messaging store, which is the sessions store on every topology
	// the routes serve (one store unsplit, one binding split; typed-stores T6
	// plan, R13). Reading Stores() again here, mid-request, is deliberate and
	// benign: an epoch swapped since store was read serves the same database,
	// so the bindings move in the store they live in either way. T7 threads
	// the request's Stores through instead.
	messaging := s.state.Stores().Mail()
	var errs []error
	for _, info := range retired {
		for _, workStore := range workStores {
			if err := moveWork(workStore, info.ID, replacementID); err != nil {
				errs = append(errs, err)
			}
		}
		if err := session.NewStore(store).ReassignWaits(info.ID, replacementID); err != nil {
			errs = append(errs, fmt.Errorf("reassign waits from retired session %s to %s: %w", info.ID, replacementID, err))
		}
		if err := extmsg.ReassignSessionBindings(ctx, messaging, info.ID, replacementID, now); err != nil {
			errs = append(errs, fmt.Errorf("reassign external message bindings from retired session %s to %s: %w", info.ID, replacementID, err))
		}
		if err := extmsg.ReassignSessionParticipants(ctx, messaging, info.ID, replacementID); err != nil {
			errs = append(errs, fmt.Errorf("reassign external message participants from retired session %s to %s: %w", info.ID, replacementID, err))
		}
	}
	return errors.Join(errs...)
}
