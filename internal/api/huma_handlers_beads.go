package api

import (
	"context"
	"errors"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beads"
)

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
		if body, ok := cachedResponseAs[ListBody[beads.Bead]](s, cacheKey, bucket); ok {
			return &ListOutput[beads.Bead]{
				Index:     s.latestIndex(),
				CacheAgeS: cacheAgeSeconds(cityStore),
				Body:      body,
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

	var all []beads.Bead
	dedupe := len(assigneeTerms) > 1

	// all=true reads materialize closed history per rig, so the build is
	// O(history) even though the caller only wants a recency-bounded page
	// (gascity#3253). When every store can Count the query exactly, push the
	// page bound down so each store returns only the rows this page needs and
	// source Total from a hydration-free Count instead of len(full history).
	// This collapses the FIRST page (no seek boundary) to O(limit) at the store
	// boundary via each backend's native LIMIT; a seeked cursor page disables
	// that native limit and hydrates matching history before the Go-side seek
	// filter (see query.SeekAfter), trading O(limit) fetches for exactness on a
	// deep walk. The response shape (a created_at-desc prefix plus an accurate
	// Total and next_cursor) is unchanged. Scoped to the single-assignee all=true
	// hot path; if any store cannot Count the query, keep the full-scan path so
	// Total and ordering stay correct (the Count fallback contract from #3211).
	boundedMode := false
	boundedFetch := 0
	var boundedCounts map[string]int
	if input.All && !dedupe && len(assigneeTerms) == 1 {
		// limit+1: the seek boundary rides on each store query and is enforced
		// Go-side, so a store returns only rows after the boundary — one extra
		// row is the has-more signal (Counts are un-seeked totals and cannot tell).
		boundedFetch = limit + 1
		boundedMode, boundedCounts = beadListBoundedTotal(ctx, stores, rigNames, assigneeTerms[0], input)
	}

	seen := map[string]bool{}
	var pa partialAggregator
	for _, rigName := range rigNames {
		store := stores[rigName]
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
			}
			if !query.HasFilter() {
				query.AllowScan = true
			}
			if boundedMode {
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
			list, err := store.List(query)
			if err != nil {
				if beads.IsPartialResult(err) && len(list) > 0 {
					// Partial result: the rig returned rows (appended to `all`
					// below) but flagged a degraded read. Keep its bounded count
					// — these rows ARE reachable, and dropping or shrinking the
					// count risks under-advertising readable rows (silent data
					// loss), strictly worse than the count's slight possible
					// over-advertisement. Only a hard List failure (zero
					// reachable rows, below) drops its count (gascity#3253).
					pa.record("rig "+rigName, err)
					pa.success()
				} else {
					pa.record("rig "+rigName, err)
					if boundedMode {
						// This rig's exact Count was baked into boundedCounts
						// upfront, but its List failed so its rows never reach
						// `all`. Drop its count so Total counts only reachable
						// rows — matching the full-scan accounting under the same
						// partial failure (where total == rows returned) and
						// keeping next_cursor from overshooting (gascity#3253).
						delete(boundedCounts, rigName)
					}
					continue
				}
			} else {
				pa.success()
			}
			for _, b := range list {
				dedupeKey := rigName + "\x00" + b.ID
				if dedupe && seen[dedupeKey] {
					continue
				}
				if dedupe {
					seen[dedupeKey] = true
				}
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
	// deterministic prefix of it (#3208). A single (rig, assignee) source is
	// already in canonical order — skip the redundant hot-path sort.
	if len(rigNames)*len(assigneeTerms) > 1 {
		beads.SortBeads(all, beads.SortCreatedDesc)
	}

	index := s.latestIndex()
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
		s.storeResponse(cacheKey, bucket, body)
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
// the exact per-rig un-seeked Counts, so Total is their sum (constant across a
// walk) and the extra overfetched row is the has-more signal. A degraded
// (partial) rig can fall short of that signal, so a non-empty partial page
// force-mints a resume cursor to keep the walk going past the degradation
// (gascity#3253). Otherwise `all` is the complete un-seeked set: Total is its
// length and the page is the contiguous suffix strictly after the Go-side seek
// boundary.
func resolveBeadListPage(all []beads.Bead, seek *beads.SeekBoundary, limit int, boundedMode bool, boundedCounts map[string]int, partial bool) (page []beads.Bead, total int, hasMore bool) {
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
// The resume key is (created_at, id) while the fan-out's identity key is
// (rig, id): this assumes (created_at, id) is globally unique across the merged
// rigs. That holds for distinct-store rigs; the only collision is the
// documented legacy file-mode aliasing of the city and rig stores, where twins
// would make the page boundary position-dependent (benign today — a true
// duplicate). A future globally-non-unique ID scheme would need a wider resume
// key here.
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

// beadListBoundedTotal returns the exact per-rig bead counts for the all=true
// list query across rigNames, sourced from each store's hydration-free Count.
// The first return value reports whether bounding is safe: it is false (and
// the caller keeps the full-scan path) if any store does not implement Counter
// or cannot count the query exactly (ErrCountUnsupported), so Total and the
// recency-ordered prefix stay correct on backends without a cheap count.
//
// Counts are returned keyed by rig (not pre-summed) so the caller can drop the
// count of any rig whose subsequent List fails: Total must reflect only the
// rows actually reachable, matching the full-scan path under partial failure
// (gascity#3253).
func beadListBoundedTotal(ctx context.Context, stores map[string]beads.Store, rigNames []string, assignee string, input *BeadListInput) (bool, map[string]int) {
	counts := make(map[string]int, len(rigNames))
	for _, rigName := range rigNames {
		counter, ok := stores[rigName].(beads.Counter)
		if !ok {
			return false, nil
		}
		n, err := counter.Count(ctx, beadListCountQuery(assignee, input))
		if err != nil {
			return false, nil
		}
		counts[rigName] = n
	}
	return true, counts
}

// beadListCountQuery builds the count query for the all=true list path. It
// carries the same filters as the list query so the count matches exactly;
// Sort and Limit are omitted because they do not affect a count.
func beadListCountQuery(assignee string, input *BeadListInput) beads.ListQuery {
	q := beads.ListQuery{
		Status:        input.Status,
		Type:          input.Type,
		Label:         input.Label,
		Assignee:      assignee,
		IncludeClosed: input.All,
		Live:          input.Status == "in_progress",
	}
	if !q.HasFilter() {
		q.AllowScan = true
	}
	return q
}

// humaHandleBeadReady is the Huma-typed handler for GET /v0/beads/ready.
func (s *Server) humaHandleBeadReady(ctx context.Context, input *BeadReadyInput) (*ListOutput[beads.Bead], error) {
	bp := input.toBlockingParams()
	if bp.isBlocking() {
		waitForChange(ctx, s.state.EventProvider(), bp)
	}

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
		ready, err := beads.HandlesFor(store).Live.Ready()
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
				continue // legacy file mode can alias the city and rig stores
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
	if pa.totalOutage() {
		return nil, pa.outageError()
	}

	if all == nil {
		all = []beads.Bead{}
	}

	index := s.latestIndex()
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
	rootID := input.RootID
	if rootID == "" {
		// Defensive: the {rootID} path segment is required, so the router never
		// dispatches here with an empty id. Unreachable in practice, hence the op
		// does not declare a 400 in its error contract.
		return nil, apierr.InvalidRequest.Msg("rootID is required")
	}

	var root beads.Bead
	var foundStore beads.Store
	for _, store := range s.beadStoresForID(rootID) {
		b, err := store.Get(rootID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, apierr.Internal.Msg(err.Error())
		}
		root = b
		foundStore = store
		break
	}
	if foundStore == nil {
		return nil, apierr.BeadNotFound.Msg("bead " + rootID + " not found")
	}

	graphBeads, parentEdges, err := collectBeadGraph(foundStore, root)
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
		Index: s.latestIndex(),
		Body: BeadGraphResponse{
			Root:  root,
			Beads: graphBeads,
			Deps:  deps,
		},
	}, nil
}

// humaHandleBeadGet is the Huma-typed handler for GET /v0/bead/{id}.
func (s *Server) humaHandleBeadGet(_ context.Context, input *BeadGetInput) (*IndexOutput[beads.Bead], error) {
	id := input.ID

	cityStore := s.state.CityBeadStore()
	if err := cacheLiveOr503(cityStore); err != nil {
		return nil, err
	}

	for _, store := range s.beadStoresForID(id) {
		b, err := store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, apierr.Internal.Msg(err.Error())
		}
		return &IndexOutput[beads.Bead]{
			Index:     s.latestIndex(),
			CacheAgeS: cacheAgeSeconds(cityStore),
			Body:      b,
		}, nil
	}
	return nil, apierr.BeadNotFound.Msg("bead " + id + " not found")
}

// humaHandleBeadDeps is the Huma-typed handler for GET /v0/bead/{id}/deps.
func (s *Server) humaHandleBeadDeps(_ context.Context, input *BeadDepsInput) (*IndexOutput[BeadDepsResponse], error) {
	id := input.ID
	for _, store := range s.beadStoresForID(id) {
		parent, err := store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, apierr.Internal.Msg(err.Error())
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
			Index: s.latestIndex(),
			Body:  BeadDepsResponse{Children: children},
		}, nil
	}
	return nil, apierr.BeadNotFound.Msg("bead " + id + " not found")
}

// BeadDepsResponse is the response shape for GET /v0/bead/{id}/deps.
type BeadDepsResponse struct {
	Children []beads.Bead `json:"children"`
}

// humaHandleBeadCreate is the Huma-typed handler for POST /v0/beads.
// Title required via struct tag on BeadCreateInput.
func (s *Server) humaHandleBeadCreate(ctx context.Context, input *BeadCreateInput) (*IndexOutput[beads.Bead], error) {
	// Idempotency: run the create at most once per Idempotency-Key. The helper
	// owns reserve/replay/mismatch/in-flight and guarantees the reservation is
	// released on any error, so every fallible step lives in the closure.
	b, err := withIdempotency(s.idem, "/v0/beads", input.IdempotencyKey, input.Body,
		func() (beads.Bead, error) {
			store := s.findStore(input.Body.Rig)
			if store == nil {
				return beads.Bead{}, apierr.InvalidRequest.Msg("rig is required when multiple rigs are configured")
			}
			assignee, err := s.normalizeRawBeadAssignee(ctx, input.Body.Assignee)
			if err != nil {
				return beads.Bead{}, apierr.InvalidRequest.Msg(err.Error())
			}
			created, err := store.Create(beads.Bead{
				Title:       input.Body.Title,
				Type:        input.Body.Type,
				Priority:    input.Body.Priority,
				Assignee:    assignee,
				Description: input.Body.Description,
				Labels:      input.Body.Labels,
				ParentID:    input.Body.Parent,
				Metadata:    input.Body.Metadata,
				DeferUntil:  input.Body.DeferUntil,
			})
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

	return &IndexOutput[beads.Bead]{
		Index: s.latestIndex(),
		Body:  b,
	}, nil
}

// humaHandleBeadClose is the Huma-typed handler for POST /v0/bead/{id}/close.
func (s *Server) humaHandleBeadClose(_ context.Context, input *BeadCloseInput) (*OKResponse, error) {
	id := input.ID
	for _, store := range s.beadStoresForID(id) {
		if _, err := store.Get(id); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, apierr.Internal.Msg(err.Error())
		}
		if err := store.Close(id); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return nil, apierr.ConflictConcurrentDelete.Msg("conflict: bead " + id + " was deleted concurrently")
			}
			return nil, apierr.Internal.Msg(err.Error())
		}
		resp := &OKResponse{}
		resp.Body.Status = "closed"
		return resp, nil
	}
	return nil, apierr.BeadNotFound.Msg("bead " + id + " not found")
}

// humaHandleBeadReopen is the Huma-typed handler for POST /v0/bead/{id}/reopen.
func (s *Server) humaHandleBeadReopen(_ context.Context, input *BeadReopenInput) (*OKResponse, error) {
	id := input.ID

	for _, store := range s.beadStoresForID(id) {
		b, err := store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, apierr.Internal.Msg(err.Error())
		}
		if b.Status != "closed" {
			return nil, apierr.ConflictWrongState.Msg("conflict: bead " + id + " is not closed (status: " + b.Status + ")")
		}
		if err := store.Reopen(id); err != nil {
			return nil, apierr.Internal.Msg(err.Error())
		}
		resp := &OKResponse{}
		resp.Body.Status = "reopened"
		return resp, nil
	}
	return nil, apierr.BeadNotFound.Msg("bead " + id + " not found")
}

// humaHandleBeadAssign is the Huma-typed handler for POST /v0/bead/{id}/assign.
func (s *Server) humaHandleBeadAssign(ctx context.Context, input *BeadAssignInput) (*IndexOutput[map[string]string], error) {
	id := input.ID
	for _, store := range s.beadStoresForID(id) {
		if _, err := store.Get(id); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, apierr.Internal.Msg(err.Error())
		}
		assignee, err := s.normalizeRawBeadAssignee(ctx, input.Body.Assignee)
		if err != nil {
			return nil, apierr.InvalidRequest.Msg(err.Error())
		}
		// Once Get succeeded in this store, treat Update-ErrNotFound as a
		// concurrent-delete race rather than "try the next store" — the bead
		// was just there; iterating would silently apply to a different store
		// that happens to share the ID prefix.
		if err := store.Update(id, beads.UpdateOpts{Assignee: &assignee}); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return nil, apierr.ConflictConcurrentDelete.Msg("conflict: bead " + id + " was deleted concurrently")
			}
			return nil, apierr.Internal.Msg(err.Error())
		}
		return &IndexOutput[map[string]string]{
			Index: s.latestIndex(),
			Body:  map[string]string{"status": "assigned", "assignee": assignee},
		}, nil
	}
	return nil, apierr.BeadNotFound.Msg("bead " + id + " not found")
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

	for _, store := range s.beadStoresForID(id) {
		current, err := store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, apierr.Internal.Msg(err.Error())
		}
		if body.Assignee != nil {
			assignee, err := s.normalizeRawBeadAssignee(ctx, *body.Assignee)
			if err != nil {
				return nil, apierr.InvalidRequest.Msg(err.Error())
			}
			opts.Assignee = &assignee
		}
		waitStatus := current.Status
		if opts.Status != nil {
			waitStatus = *opts.Status
		}
		// Once Get succeeded in this store, treat Update-ErrNotFound as a
		// concurrent-delete race (409) rather than iterating to the next
		// store — otherwise a delete racing with update silently applies
		// the mutation to a different store that happens to share the ID.
		if err := store.Update(id, opts); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return nil, apierr.ConflictConcurrentDelete.Msg("conflict: bead " + id + " was deleted concurrently")
			}
			return nil, apierr.Internal.Msg(err.Error())
		}
		if opts.ParentID != nil && current.ParentID != *opts.ParentID && waitStatus != "closed" {
			if waiter, ok := store.(beads.ParentProjectionWaiter); ok {
				if err := waiter.WaitForParentProjection(ctx, id, current.ParentID, *opts.ParentID); err != nil {
					if errors.Is(err, beads.ErrParentProjectionSuperseded) {
						return nil, apierr.ConflictConcurrentModify.Msg("conflict: bead " + id + " was reparented concurrently")
					}
					return nil, apierr.Internal.Msg(err.Error())
				}
			}
		}
		resp := &OKResponse{}
		resp.Body.Status = "updated"
		return resp, nil
	}
	return nil, apierr.BeadNotFound.Msg("bead " + id + " not found")
}

// humaHandleBeadDelete is the Huma-typed handler for DELETE /v0/bead/{id}.
// It is implemented as a soft-delete (store.Close) — see the `"closed"`
// status field for honest wire-contract semantics. Hard-delete is not
// exposed through the API.
func (s *Server) humaHandleBeadDelete(_ context.Context, input *BeadDeleteInput) (*OKResponse, error) {
	id := input.ID
	for _, store := range s.beadStoresForID(id) {
		if _, err := store.Get(id); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, apierr.Internal.Msg(err.Error())
		}
		if err := store.Close(id); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return nil, apierr.ConflictConcurrentDelete.Msg("conflict: bead " + id + " was deleted concurrently")
			}
			return nil, apierr.Internal.Msg(err.Error())
		}
		resp := &OKResponse{}
		resp.Body.Status = "closed"
		return resp, nil
	}
	return nil, apierr.BeadNotFound.Msg("bead " + id + " not found")
}
