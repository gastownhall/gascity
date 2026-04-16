package api

import (
	"context"
	"errors"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
)

// convoyProgress is the shared {total, closed} progress shape used by
// simple convoy detail and check responses.
type convoyProgress struct {
	Total  int `json:"total" doc:"Total child bead count."`
	Closed int `json:"closed" doc:"Closed child bead count."`
}

// convoyGetResponse is the response for GET /v0/convoy/{id}. It is a union
// of two cases:
//   - Graph/workflow convoys: fields are populated from the embedded
//     workflowSnapshotResponse and the simple-convoy fields are absent.
//   - Simple convoys (type=convoy bead with children): Convoy, Children,
//     and Progress are populated; the workflow fields are absent.
//
// The embedded pointer to workflowSnapshotResponse is nil for simple
// convoys, so its fields are omitted from the JSON output.
type convoyGetResponse struct {
	*workflowSnapshotResponse
	Convoy   *beads.Bead     `json:"convoy,omitempty" doc:"Simple convoy bead (non-workflow case)."`
	Children []beads.Bead    `json:"children,omitempty" doc:"Direct child beads (non-workflow case)."`
	Progress *convoyProgress `json:"progress,omitempty" doc:"Child bead progress (non-workflow case)."`
}

// convoyCheckResponse is the response for GET /v0/convoy/{id}/check.
type convoyCheckResponse struct {
	ConvoyID string `json:"convoy_id" doc:"Convoy ID."`
	Total    int    `json:"total" doc:"Total child bead count."`
	Closed   int    `json:"closed" doc:"Closed child bead count."`
	Complete bool   `json:"complete" doc:"True when all child beads are closed and total > 0."`
}

// workflowDeleteResponse is the response for DELETE /v0/workflow/{workflow_id}.
type workflowDeleteResponse struct {
	WorkflowID string `json:"workflow_id" doc:"Workflow ID."`
	Closed     int    `json:"closed" doc:"Number of beads closed."`
	Deleted    int    `json:"deleted" doc:"Number of beads deleted."`
}

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
func (s *Server) humaHandleConvoyGet(_ context.Context, input *ConvoyGetInput) (*IndexOutput[convoyGetResponse], error) {
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
		return &IndexOutput[convoyGetResponse]{
			Index: index,
			Body:  convoyGetResponse{workflowSnapshotResponse: snapshot},
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

		return &IndexOutput[convoyGetResponse]{
			Index: s.latestIndex(),
			Body: convoyGetResponse{
				Convoy:   &b,
				Children: children,
				Progress: &convoyProgress{Total: total, Closed: closed},
			},
		}, nil
	}
	return nil, huma.Error404NotFound("convoy " + id + " not found")
}

// humaHandleConvoyCreate is the Huma-typed handler for POST /v0/convoys.
// Title required via struct tag on ConvoyCreateInput.
func (s *Server) humaHandleConvoyCreate(_ context.Context, input *ConvoyCreateInput) (*IndexOutput[beads.Bead], error) {
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
func (s *Server) humaHandleConvoyCheck(_ context.Context, input *ConvoyCheckInput) (*IndexOutput[convoyCheckResponse], error) {
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
		return &IndexOutput[convoyCheckResponse]{
			Index: s.latestIndex(),
			Body: convoyCheckResponse{
				ConvoyID: id,
				Total:    total,
				Closed:   closed,
				Complete: complete,
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

// humaHandleWorkflowGet is the Huma-typed handler for GET /v0/workflow/{workflow_id}.
// Backward-compatible alias for the convoy/workflow snapshot endpoint.
func (s *Server) humaHandleWorkflowGet(_ context.Context, input *WorkflowGetInput) (*IndexOutput[workflowSnapshotResponse], error) {
	workflowID := strings.TrimSpace(input.WorkflowID)
	if workflowID == "" {
		return nil, huma.Error400BadRequest("convoy id is required")
	}

	scopeKind, scopeRef, scopeErr := parseOptionalWorkflowRequestScope(input.ScopeKind, input.ScopeRef)
	if scopeErr != "" {
		return nil, huma.Error400BadRequest(scopeErr)
	}
	index := s.latestIndex()

	snapshot, err := s.buildWorkflowSnapshot(workflowID, scopeKind, scopeRef, index)
	if err != nil {
		if errors.Is(err, errWorkflowNotFound) {
			return nil, huma.Error404NotFound("workflow " + workflowID + " not found")
		}
		return nil, huma.Error500InternalServerError("workflow snapshot failed")
	}

	return &IndexOutput[workflowSnapshotResponse]{
		Index: index,
		Body:  *snapshot,
	}, nil
}

// humaHandleWorkflowDelete is the Huma-typed handler for DELETE /v0/workflow/{workflow_id}.
// Backward-compatible alias for the convoy/workflow delete endpoint.
func (s *Server) humaHandleWorkflowDelete(_ context.Context, input *WorkflowDeleteInput) (*struct {
	Body workflowDeleteResponse
}, error) {
	workflowID := strings.TrimSpace(input.WorkflowID)
	if workflowID == "" {
		return nil, huma.Error400BadRequest("convoy id is required")
	}

	scopeKind := strings.TrimSpace(input.ScopeKind)
	scopeRef := strings.TrimSpace(input.ScopeRef)
	deleteFromStore := input.Delete == "true"

	stores := s.workflowStores()

	closed := 0
	deleted := 0
	found := false

	for _, info := range stores {
		if info.store == nil {
			continue
		}
		if scopeKind != "" && info.scopeKind != scopeKind {
			continue
		}
		if scopeRef != "" && info.scopeRef != scopeRef {
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

		// Phase 1: Batch close all open beads.
		n, _ := info.store.CloseAll(ids, map[string]string{"gc.outcome": "skipped"})
		closed += n

		// Phase 2: Delete if requested.
		if deleteFromStore {
			for _, id := range ids {
				if deps, err := info.store.DepList(id, "down"); err == nil {
					for _, dep := range deps {
						_ = info.store.DepRemove(id, dep.DependsOnID)
					}
				}
				if deps, err := info.store.DepList(id, "up"); err == nil {
					for _, dep := range deps {
						_ = info.store.DepRemove(dep.IssueID, id)
					}
				}
				if err := info.store.Delete(id); err == nil {
					deleted++
				}
			}
		}
	}

	if !found {
		return nil, huma.Error404NotFound("workflow " + workflowID + " not found")
	}

	return &struct {
		Body workflowDeleteResponse
	}{Body: workflowDeleteResponse{
		WorkflowID: workflowID,
		Closed:     closed,
		Deleted:    deleted,
	}}, nil
}
