package api

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
)

// humaHandleBeadList is the Huma-typed handler for GET /v0/beads.
func (s *Server) humaHandleBeadList(ctx context.Context, input *BeadListInput) (*ListOutput[beads.Bead], error) {
	bp := input.toBlockingParams()
	if bp.isBlocking() {
		waitForChange(ctx, s.state.EventProvider(), bp)
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
		pp.IsPaging = true
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
	seen := map[string]bool{}
	var pa partialAggregator
	for _, rigName := range rigNames {
		store := stores[rigName]
		for _, assignee := range assigneeTerms {
			query := beads.ListQuery{
				Status:   input.Status,
				Type:     input.Type,
				Label:    input.Label,
				Assignee: assignee,
				Live:     input.Status == "in_progress",
			}
			if !query.HasFilter() {
				query.AllowScan = true
			}
			pa.attempt()
			list, err := store.List(query)
			if err != nil {
				if beads.IsPartialResult(err) && len(list) > 0 {
					pa.record("rig "+rigName, err)
					pa.success()
				} else {
					pa.record("rig "+rigName, err)
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

	index := s.latestIndex()
	if !pp.IsPaging {
		total := len(all)
		if pp.Limit < len(all) {
			all = all[:pp.Limit]
		}
		return &ListOutput[beads.Bead]{
			Index: index,
			Body: ListBody[beads.Bead]{
				Items:         all,
				Total:         total,
				Partial:       pa.partial(),
				PartialErrors: pa.messages(),
			},
		}, nil
	}

	page, total, nextCursor := paginate(all, pp)
	if page == nil {
		page = []beads.Bead{}
	}
	return &ListOutput[beads.Bead]{
		Index: index,
		Body: ListBody[beads.Bead]{
			Items:         page,
			Total:         total,
			NextCursor:    nextCursor,
			Partial:       pa.partial(),
			PartialErrors: pa.messages(),
		},
	}, nil
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
	for _, rigName := range rigNames {
		pa.attempt()
		ready, err := beads.ReadyLive(stores[rigName])
		if err != nil {
			if beads.IsPartialResult(err) && len(ready) > 0 {
				pa.record("rig "+rigName, err)
				pa.success()
			} else {
				pa.record("rig "+rigName, err)
				continue
			}
		} else {
			pa.success()
		}
		all = append(all, ready...)
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
	for _, store := range s.beadStoresForID(id) {
		b, err := store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &IndexOutput[beads.Bead]{
			Index: s.latestIndex(),
			Body:  b,
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
		if err := store.Close(id); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
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
	status := "open"

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
		if err := store.Update(id, beads.UpdateOpts{Status: &status}); err != nil {
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
		if _, err := store.Get(id); err != nil {
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
		if err := store.Close(id); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		resp := &OKResponse{}
		resp.Body.Status = "closed"
		return resp, nil
	}
	return nil, huma.Error404NotFound("bead " + id + " not found")
}
