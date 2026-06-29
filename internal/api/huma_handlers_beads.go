package api

import (
	"context"
	"errors"
	"time"

	"github.com/danielgtaylor/huma/v2"
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

	pp := pageParams{Limit: 50}
	if input.Limit > 0 {
		pp.Limit = input.Limit
		if pp.Limit > maxPaginationLimit {
			pp.Limit = maxPaginationLimit
		}
	}
	if input.Cursor != "" {
		pp.Offset = decodeCursor(input.Cursor)
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
	// source Total from a hydration-free Count instead of len(full history) —
	// the build collapses to O(limit) at the store boundary while the response
	// shape (a created_at-desc prefix plus an accurate Total and next_cursor)
	// is unchanged. Scoped to the single-assignee all=true hot path; if any
	// store cannot Count the query, keep the full-scan path so Total and
	// ordering stay correct (the Count fallback contract from #3211).
	boundedMode := false
	boundedFetch := 0
	var boundedCounts map[string]int
	if input.All && !dedupe && len(assigneeTerms) == 1 {
		boundedFetch = pp.Offset + pp.Limit
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
				// the cross-rig merge below cuts the exact global prefix.
				query.Limit = boundedFetch
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
	// A non-cursor request is offset-0 paging: a truncated first page carries
	// the continuation cursor too, otherwise the remainder of a limit-bounded
	// read is unfetchable by design (#3208).
	var page []beads.Bead
	var total int
	var nextCursor string
	if boundedMode {
		// Total is the exact Count summed over the rigs whose List actually
		// returned rows, not len(all) (which holds only the bounded prefix) and
		// not the upfront count of every rig: a rig counted then dropped at List
		// time is removed from boundedCounts above, so Total tracks reachable
		// rows and next_cursor still points at the real remainder (gascity#3253).
		for _, n := range boundedCounts {
			total += n
		}
		if pp.Offset < len(all) {
			end := pp.Offset + pp.Limit
			if end > len(all) {
				end = len(all)
			}
			page = all[pp.Offset:end]
		}
		if pp.Offset+pp.Limit < total {
			nextCursor = encodeCursor(pp.Offset + pp.Limit)
		}
	} else {
		page, total, nextCursor = paginate(all, pp)
	}
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
		return nil, huma.Error400BadRequest("rootID is required")
	}

	var root beads.Bead
	var foundStore beads.Store
	for _, store := range s.beadStoresForID(rootID) {
		b, err := store.Get(rootID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		root = b
		foundStore = store
		break
	}
	if foundStore == nil {
		return nil, huma.Error404NotFound("bead " + rootID + " not found")
	}

	graphBeads, parentEdges, err := collectBeadGraph(foundStore, root)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	beadIndex := make(map[string]beads.Bead, len(graphBeads))
	for _, b := range graphBeads {
		beadIndex[b.ID] = b
	}

	deps, depPartial := collectWorkflowDeps(foundStore, beadIndex)
	if depPartial {
		return nil, huma.Error500InternalServerError("listing bead graph dependencies failed")
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
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &IndexOutput[beads.Bead]{
			Index:     s.latestIndex(),
			CacheAgeS: cacheAgeSeconds(cityStore),
			Body:      b,
		}, nil
	}
	return nil, huma.Error404NotFound("bead " + id + " not found")
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
			return nil, huma.Error500InternalServerError(err.Error())
		}
		children, err := store.List(beads.ListQuery{
			ParentID: id,
			Sort:     beads.SortCreatedAsc,
		})
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
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
	return nil, huma.Error404NotFound("bead " + id + " not found")
}

// BeadDepsResponse is the response shape for GET /v0/bead/{id}/deps.
type BeadDepsResponse struct {
	Children []beads.Bead `json:"children"`
}

// humaHandleBeadCreate is the Huma-typed handler for POST /v0/beads.
// Title required via struct tag on BeadCreateInput.
func (s *Server) humaHandleBeadCreate(ctx context.Context, input *BeadCreateInput) (*IndexOutput[beads.Bead], error) {
	// Idempotency check — scope by method+path to prevent cross-endpoint collisions.
	idemKey := ""
	var bodyHash string
	if input.IdempotencyKey != "" {
		idemKey = "POST:/v0/beads:" + input.IdempotencyKey
		bodyHash = hashBody(input.Body)
		existing, found := s.idem.reserve(idemKey, bodyHash)
		if found {
			if existing.bodyHash != bodyHash {
				return nil, huma.Error422UnprocessableEntity("idempotency_mismatch: Idempotency-Key reused with different request body")
			}
			if existing.pending {
				return nil, huma.Error409Conflict("in_flight: request with this Idempotency-Key is already in progress")
			}
			// Replay cached typed response (Fix 3l).
			if b, ok := replayAs[beads.Bead](existing); ok {
				return &IndexOutput[beads.Bead]{
					Index: s.latestIndex(),
					Body:  b,
				}, nil
			}
		}
	}

	store := s.findStore(input.Body.Rig)
	if store == nil {
		s.idem.unreserve(idemKey)
		return nil, huma.Error400BadRequest("rig is required when multiple rigs are configured")
	}
	assignee, err := s.normalizeRawBeadAssignee(ctx, input.Body.Assignee)
	if err != nil {
		s.idem.unreserve(idemKey)
		return nil, huma.Error400BadRequest(err.Error())
	}

	b, err := store.Create(beads.Bead{
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
		s.idem.unreserve(idemKey)
		return nil, huma.Error500InternalServerError(err.Error())
	}

	// Some stores return a minimal create envelope and require a follow-up
	// read for the canonical persisted bead state.
	if persisted, getErr := store.Get(b.ID); getErr == nil {
		b = persisted
	}
	s.idem.storeResponse(idemKey, bodyHash, b)

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
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if err := store.Close(id); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return nil, huma.Error409Conflict("conflict: bead " + id + " was deleted concurrently")
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		resp := &OKResponse{}
		resp.Body.Status = "closed"
		return resp, nil
	}
	return nil, huma.Error404NotFound("bead " + id + " not found")
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
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if b.Status != "closed" {
			return nil, huma.Error409Conflict("conflict: bead " + id + " is not closed (status: " + b.Status + ")")
		}
		if err := store.Reopen(id); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		resp := &OKResponse{}
		resp.Body.Status = "reopened"
		return resp, nil
	}
	return nil, huma.Error404NotFound("bead " + id + " not found")
}

// humaHandleBeadAssign is the Huma-typed handler for POST /v0/bead/{id}/assign.
func (s *Server) humaHandleBeadAssign(ctx context.Context, input *BeadAssignInput) (*IndexOutput[map[string]string], error) {
	id := input.ID
	for _, store := range s.beadStoresForID(id) {
		if _, err := store.Get(id); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		assignee, err := s.normalizeRawBeadAssignee(ctx, input.Body.Assignee)
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		// Once Get succeeded in this store, treat Update-ErrNotFound as a
		// concurrent-delete race rather than "try the next store" — the bead
		// was just there; iterating would silently apply to a different store
		// that happens to share the ID prefix.
		if err := store.Update(id, beads.UpdateOpts{Assignee: &assignee}); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return nil, huma.Error409Conflict("conflict: bead " + id + " was deleted concurrently")
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &IndexOutput[map[string]string]{
			Index: s.latestIndex(),
			Body:  map[string]string{"status": "assigned", "assignee": assignee},
		}, nil
	}
	return nil, huma.Error404NotFound("bead " + id + " not found")
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
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if body.Assignee != nil {
			assignee, err := s.normalizeRawBeadAssignee(ctx, *body.Assignee)
			if err != nil {
				return nil, huma.Error400BadRequest(err.Error())
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
				return nil, huma.Error409Conflict("conflict: bead " + id + " was deleted concurrently")
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if opts.ParentID != nil && current.ParentID != *opts.ParentID && waitStatus != "closed" {
			if waiter, ok := store.(beads.ParentProjectionWaiter); ok {
				if err := waiter.WaitForParentProjection(ctx, id, current.ParentID, *opts.ParentID); err != nil {
					if errors.Is(err, beads.ErrParentProjectionSuperseded) {
						return nil, huma.Error409Conflict("conflict: bead " + id + " was reparented concurrently")
					}
					return nil, huma.Error500InternalServerError(err.Error())
				}
			}
		}
		resp := &OKResponse{}
		resp.Body.Status = "updated"
		return resp, nil
	}
	return nil, huma.Error404NotFound("bead " + id + " not found")
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
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if err := store.Close(id); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return nil, huma.Error409Conflict("conflict: bead " + id + " was deleted concurrently")
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		resp := &OKResponse{}
		resp.Body.Status = "closed"
		return resp, nil
	}
	return nil, huma.Error404NotFound("bead " + id + " not found")
}
