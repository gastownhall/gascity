package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
)

// humaHandleConvoyList is the Huma-typed handler for GET /v0/convoys.
func (s *Server) humaHandleConvoyList(ctx context.Context, input *ConvoyListInput) (*ListOutput[beads.Bead], error) {
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
	rigNames := sortedRigNames(stores)
	var convoys []beads.Bead
	for _, rigName := range rigNames {
		store := stores[rigName]
		list, err := store.List(beads.ListQuery{Type: "convoy"})
		if err != nil {
			continue
		}
		convoys = append(convoys, list...)
	}

	if convoys == nil {
		convoys = []beads.Bead{}
	}

	index := s.latestIndex()
	if !pp.IsPaging {
		total := len(convoys)
		if pp.Limit < len(convoys) {
			convoys = convoys[:pp.Limit]
		}
		return &ListOutput[beads.Bead]{
			Index: index,
			Body:  ListBody[beads.Bead]{Items: convoys, Total: total},
		}, nil
	}

	page, total, nextCursor := paginate(convoys, pp)
	if page == nil {
		page = []beads.Bead{}
	}
	return &ListOutput[beads.Bead]{
		Index: index,
		Body:  ListBody[beads.Bead]{Items: page, Total: total, NextCursor: nextCursor},
	}, nil
}

// humaHandleConvoyGet is the Huma-typed handler for GET /v0/convoy/{id}.
func (s *Server) humaHandleConvoyGet(_ context.Context, input *ConvoyGetInput) (*IndexOutput[map[string]any], error) {
	id := input.ID

	// Formula-compiled convoy (graph workflow): build the full DAG snapshot.
	if isGraphConvoyID(s, id) {
		index := s.latestIndex()
		snapshot, err := s.buildWorkflowSnapshot(id, "", "", index)
		if err != nil {
			if errors.Is(err, errWorkflowNotFound) {
				return nil, huma.Error404NotFound("workflow " + id + " not found")
			}
			return nil, huma.Error500InternalServerError("workflow snapshot failed")
		}
		return &IndexOutput[map[string]any]{
			Index: index,
			Body:  structToMap(snapshot),
		}, nil
	}

	stores := s.state.BeadStores()
	for _, rigName := range sortedRigNames(stores) {
		store := stores[rigName]
		b, err := store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if b.Type != "convoy" {
			return nil, huma.Error404NotFound("bead " + id + " is not a convoy")
		}

		children, err := store.List(beads.ListQuery{
			ParentID:      id,
			IncludeClosed: true,
			Sort:          beads.SortCreatedAsc,
		})
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if children == nil {
			children = []beads.Bead{}
		}

		total := len(children)
		closed := 0
		for _, c := range children {
			if c.Status == "closed" {
				closed++
			}
		}

		return &IndexOutput[map[string]any]{
			Index: s.latestIndex(),
			Body: map[string]any{
				"convoy":   b,
				"children": children,
				"progress": map[string]int{"total": total, "closed": closed},
			},
		}, nil
	}
	return nil, huma.Error404NotFound("convoy " + id + " not found")
}

// humaHandleConvoyCreate is the Huma-typed handler for POST /v0/convoys.
func (s *Server) humaHandleConvoyCreate(_ context.Context, input *ConvoyCreateInput) (*IndexOutput[beads.Bead], error) {
	if input.Body.Title == "" {
		return nil, huma.Error400BadRequest("title is required")
	}

	store := s.findStore(input.Body.Rig)
	if store == nil {
		return nil, huma.Error400BadRequest("rig is required when multiple rigs are configured")
	}

	// Pre-validate all items exist before creating the convoy to avoid orphans.
	for _, itemID := range input.Body.Items {
		if _, err := store.Get(itemID); err != nil {
			return nil, storeError(err)
		}
	}

	convoy, err := store.Create(beads.Bead{
		Title: input.Body.Title,
		Type:  "convoy",
	})
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}

	// Link child items to convoy.
	for _, itemID := range input.Body.Items {
		pid := convoy.ID
		if err := store.Update(itemID, beads.UpdateOpts{ParentID: &pid}); err != nil {
			return nil, huma.Error500InternalServerError("failed to link item " + itemID + ": " + err.Error())
		}
	}

	return &IndexOutput[beads.Bead]{
		Index: s.latestIndex(),
		Body:  convoy,
	}, nil
}

// humaHandleConvoyAdd is the Huma-typed handler for POST /v0/convoy/{id}/add.
func (s *Server) humaHandleConvoyAdd(_ context.Context, input *ConvoyAddInput) (*OKResponse, error) {
	id := input.ID
	stores := s.state.BeadStores()
	for _, rigName := range sortedRigNames(stores) {
		store := stores[rigName]
		b, err := store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if b.Type != "convoy" {
			return nil, huma.Error400BadRequest("bead " + id + " is not a convoy")
		}
		// Pre-validate all items exist before linking.
		for _, itemID := range input.Body.Items {
			if _, err := store.Get(itemID); err != nil {
				return nil, storeError(err)
			}
		}
		for _, itemID := range input.Body.Items {
			pid := id
			if err := store.Update(itemID, beads.UpdateOpts{ParentID: &pid}); err != nil {
				return nil, huma.Error500InternalServerError("failed to link item " + itemID + ": " + err.Error())
			}
		}
		resp := &OKResponse{}
		resp.Body.Status = "updated"
		return resp, nil
	}
	return nil, huma.Error404NotFound("convoy " + id + " not found")
}

// humaHandleConvoyRemove is the Huma-typed handler for POST /v0/convoy/{id}/remove.
func (s *Server) humaHandleConvoyRemove(_ context.Context, input *ConvoyRemoveInput) (*OKResponse, error) {
	id := input.ID
	stores := s.state.BeadStores()
	for _, rigName := range sortedRigNames(stores) {
		store := stores[rigName]
		b, err := store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if b.Type != "convoy" {
			return nil, huma.Error400BadRequest("bead " + id + " is not a convoy")
		}
		// Pre-validate all items exist and belong to this convoy.
		for _, itemID := range input.Body.Items {
			item, gerr := store.Get(itemID)
			if gerr != nil {
				if errors.Is(gerr, beads.ErrNotFound) {
					return nil, huma.Error404NotFound("item " + itemID + " not found")
				}
				return nil, huma.Error500InternalServerError(gerr.Error())
			}
			if item.ParentID != id {
				return nil, huma.Error400BadRequest("item " + itemID + " does not belong to convoy " + id)
			}
		}
		// Unlink items by clearing their ParentID.
		empty := ""
		for _, itemID := range input.Body.Items {
			if err := store.Update(itemID, beads.UpdateOpts{ParentID: &empty}); err != nil {
				return nil, huma.Error500InternalServerError("failed to unlink item " + itemID + ": " + err.Error())
			}
		}
		resp := &OKResponse{}
		resp.Body.Status = "updated"
		return resp, nil
	}
	return nil, huma.Error404NotFound("convoy " + id + " not found")
}

// humaHandleConvoyCheck is the Huma-typed handler for GET /v0/convoy/{id}/check.
func (s *Server) humaHandleConvoyCheck(_ context.Context, input *ConvoyCheckInput) (*IndexOutput[map[string]any], error) {
	id := input.ID
	stores := s.state.BeadStores()

	for _, rigName := range sortedRigNames(stores) {
		store := stores[rigName]
		b, err := store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if b.Type != "convoy" {
			return nil, huma.Error400BadRequest("bead " + id + " is not a convoy")
		}

		children, err := store.List(beads.ListQuery{
			ParentID:      id,
			IncludeClosed: true,
			Sort:          beads.SortCreatedAsc,
		})
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}

		total := len(children)
		closed := 0
		for _, c := range children {
			if c.Status == "closed" {
				closed++
			}
		}

		complete := total > 0 && closed == total
		return &IndexOutput[map[string]any]{
			Index: s.latestIndex(),
			Body: map[string]any{
				"convoy_id": id,
				"total":     total,
				"closed":    closed,
				"complete":  complete,
			},
		}, nil
	}
	return nil, huma.Error404NotFound("convoy " + id + " not found")
}

// humaHandleConvoyClose is the Huma-typed handler for POST /v0/convoy/{id}/close.
func (s *Server) humaHandleConvoyClose(_ context.Context, input *ConvoyCloseInput) (*OKResponse, error) {
	id := input.ID
	stores := s.state.BeadStores()

	for _, rigName := range sortedRigNames(stores) {
		store := stores[rigName]
		b, err := store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if b.Type != "convoy" {
			return nil, huma.Error400BadRequest("bead " + id + " is not a convoy")
		}
		if err := store.Close(id); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		resp := &OKResponse{}
		resp.Body.Status = "closed"
		return resp, nil
	}
	return nil, huma.Error404NotFound("convoy " + id + " not found")
}

// humaHandleConvoyDelete is the Huma-typed handler for DELETE /v0/convoy/{id}.
func (s *Server) humaHandleConvoyDelete(_ context.Context, input *ConvoyDeleteInput) (*OKResponse, error) {
	id := input.ID

	// Formula-compiled convoy (graph workflow): delegate to the workflow
	// delete logic which tears down the full DAG.
	if isGraphConvoyID(s, id) {
		return s.humaDeleteWorkflow(id)
	}

	stores := s.state.BeadStores()
	for _, rigName := range sortedRigNames(stores) {
		store := stores[rigName]
		b, err := store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if b.Type != "convoy" {
			return nil, huma.Error400BadRequest("bead " + id + " is not a convoy")
		}
		if err := store.Close(id); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		resp := &OKResponse{}
		resp.Body.Status = "deleted"
		return resp, nil
	}
	return nil, huma.Error404NotFound("convoy " + id + " not found")
}

// humaDeleteWorkflow handles workflow convoy deletion through the Huma handler.
func (s *Server) humaDeleteWorkflow(workflowID string) (*OKResponse, error) {
	stores := s.workflowStores()
	found := false

	for _, info := range stores {
		if info.store == nil {
			continue
		}

		var ids []string
		seen := make(map[string]struct{}, 4)
		rootIDs := make([]string, 0, 2)
		rootSeen := make(map[string]struct{}, 2)
		addID := func(id string) {
			if id == "" {
				return
			}
			if _, ok := seen[id]; ok {
				return
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		addRoot := func(root beads.Bead) {
			if !isWorkflowRoot(root) || !matchesWorkflowID(root, workflowID) {
				return
			}
			if _, ok := rootSeen[root.ID]; ok {
				return
			}
			rootSeen[root.ID] = struct{}{}
			rootIDs = append(rootIDs, root.ID)
			addID(root.ID)
		}
		if root, err := info.store.Get(workflowID); err == nil {
			addRoot(root)
		}
		if roots, err := info.store.List(beads.ListQuery{
			Metadata: map[string]string{
				"gc.kind":        "workflow",
				"gc.workflow_id": workflowID,
			},
			IncludeClosed: true,
		}); err == nil {
			for _, root := range roots {
				addRoot(root)
			}
		}
		for _, rootID := range rootIDs {
			all, err := info.store.List(beads.ListQuery{
				Metadata:      map[string]string{"gc.root_bead_id": rootID},
				IncludeClosed: true,
			})
			if err != nil {
				continue
			}
			for _, b := range all {
				addID(b.ID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		found = true
		info.store.CloseAll(ids, map[string]string{"gc.outcome": "skipped"}) //nolint:errcheck
	}

	if !found {
		return nil, huma.Error404NotFound("workflow " + workflowID + " not found")
	}

	resp := &OKResponse{}
	resp.Body.Status = "deleted"
	return resp, nil
}

// storeError converts a bead store error into the appropriate Huma error.
func storeError(err error) error {
	if errors.Is(err, beads.ErrNotFound) {
		return huma.Error404NotFound(err.Error())
	}
	return huma.Error500InternalServerError(err.Error())
}

// structToMap converts a struct to map[string]any via JSON round-trip.
func structToMap(v any) map[string]any {
	data, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]any{}
	}
	return m
}
