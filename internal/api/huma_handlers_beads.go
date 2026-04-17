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
	var rigNames []string
	if input.Rig != "" {
		if _, ok := stores[input.Rig]; ok {
			rigNames = []string{input.Rig}
		}
	} else {
		rigNames = sortedRigNames(stores)
	}

	var all []beads.Bead
	for _, rigName := range rigNames {
		store := stores[rigName]
		query := beads.ListQuery{
			Status:   input.Status,
			Type:     input.Type,
			Label:    input.Label,
			Assignee: input.Assignee,
		}
		if !query.HasFilter() {
			query.AllowScan = true
		}
		list, err := store.List(query)
		if err != nil {
			continue
		}
		all = append(all, list...)
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
			Body:  ListBody[beads.Bead]{Items: all, Total: total},
		}, nil
	}

	page, total, nextCursor := paginate(all, pp)
	if page == nil {
		page = []beads.Bead{}
	}
	return &ListOutput[beads.Bead]{
		Index: index,
		Body:  ListBody[beads.Bead]{Items: page, Total: total, NextCursor: nextCursor},
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
	for _, rigName := range rigNames {
		ready, err := stores[rigName].Ready()
		if err != nil {
			continue
		}
		all = append(all, ready...)
	}

	if all == nil {
		all = []beads.Bead{}
	}

	index := s.latestIndex()
	return &ListOutput[beads.Bead]{
		Index: index,
		Body:  ListBody[beads.Bead]{Items: all, Total: len(all)},
	}, nil
}

// humaHandleBeadGraph is the Huma-typed handler for GET /v0/beads/graph/{rootID}.
func (s *Server) humaHandleBeadGraph(_ context.Context, input *BeadGraphInput) (*IndexOutput[beadGraphResponseJSON], error) {
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

	all, err := foundStore.List(beads.ListQuery{
		Metadata:      map[string]string{"gc.root_bead_id": rootID},
		IncludeClosed: true,
	})
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}

	graphBeads := []beads.Bead{root}
	beadIndex := map[string]beads.Bead{root.ID: root}
	for _, b := range all {
		if b.ID == root.ID {
			continue
		}
		graphBeads = append(graphBeads, b)
		beadIndex[b.ID] = b
	}

	deps, _ := collectWorkflowDeps(foundStore, beadIndex)

	return &IndexOutput[beadGraphResponseJSON]{
		Index: s.latestIndex(),
		Body: beadGraphResponseJSON{
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
func (s *Server) humaHandleBeadDeps(_ context.Context, input *BeadDepsInput) (*IndexOutput[beadDepsResponse], error) {
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
		return &IndexOutput[beadDepsResponse]{
			Index: s.latestIndex(),
			Body:  beadDepsResponse{Children: children},
		}, nil
	}
	return nil, huma.Error404NotFound("bead " + id + " not found")
}

// beadDepsResponse is the response shape for GET /v0/bead/{id}/deps.
type beadDepsResponse struct {
	Children []beads.Bead `json:"children"`
}

// humaHandleBeadCreate is the Huma-typed handler for POST /v0/beads.
// Title required via struct tag on BeadCreateInput.
func (s *Server) humaHandleBeadCreate(_ context.Context, input *BeadCreateInput) (*IndexOutput[beads.Bead], error) {
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

	b, err := store.Create(beads.Bead{
		Title:       input.Body.Title,
		Type:        input.Body.Type,
		Priority:    input.Body.Priority,
		Assignee:    input.Body.Assignee,
		Description: input.Body.Description,
		Labels:      input.Body.Labels,
	})
	if err != nil {
		s.idem.unreserve(idemKey)
		return nil, huma.Error500InternalServerError(err.Error())
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
func (s *Server) humaHandleBeadAssign(_ context.Context, input *BeadAssignInput) (*IndexOutput[map[string]string], error) {
	id := input.ID
	for _, store := range s.beadStoresForID(id) {
		if err := store.Update(id, beads.UpdateOpts{Assignee: &input.Body.Assignee}); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &IndexOutput[map[string]string]{
			Index: s.latestIndex(),
			Body:  map[string]string{"status": "assigned", "assignee": input.Body.Assignee},
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
// change." Earlier revisions used a json.RawMessage body to peek at field
// tokens and reject `priority: null` with a 400 ("clearing priority is not
// supported"). That UX nicety was removed in Phase 3 because (a) the only
// in-repo caller (dashboard issue update) never sends null, and (b) keeping
// it required a json.RawMessage body that broke the spec-driven contract.
func (s *Server) humaHandleBeadUpdate(_ context.Context, input *BeadUpdateInput) (*OKResponse, error) {
	id := input.ID
	body := input.Body

	opts := beads.UpdateOpts{
		Title:        body.Title,
		Status:       body.Status,
		Type:         body.Type,
		Priority:     body.Priority,
		Assignee:     body.Assignee,
		Description:  body.Description,
		Labels:       body.Labels,
		RemoveLabels: body.RemoveLabels,
	}

	for _, store := range s.beadStoresForID(id) {
		if err := store.Update(id, opts); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		// Apply metadata key-value pairs if provided.
		if len(body.Metadata) > 0 {
			if err := store.SetMetadataBatch(id, body.Metadata); err != nil {
				return nil, huma.Error500InternalServerError(err.Error())
			}
		}
		resp := &OKResponse{}
		resp.Body.Status = "updated"
		return resp, nil
	}
	return nil, huma.Error404NotFound("bead " + id + " not found")
}

// humaHandleBeadDelete is the Huma-typed handler for DELETE /v0/bead/{id}.
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
		resp.Body.Status = "deleted"
		return resp, nil
	}
	return nil, huma.Error404NotFound("bead " + id + " not found")
}
