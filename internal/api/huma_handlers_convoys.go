package api

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
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
// Partial/PartialErrors fire when the teardown swept beads but one or
// more operations (list, close, dep-remove, delete) failed mid-way;
// clients see exact counts for what succeeded plus the failed steps.
type workflowDeleteResponse struct {
	WorkflowID    string   `json:"workflow_id" doc:"Workflow ID."`
	Closed        int      `json:"closed" doc:"Number of beads closed."`
	Deleted       int      `json:"deleted" doc:"Number of beads deleted."`
	Partial       bool     `json:"partial,omitempty" doc:"True when one or more teardown steps failed; Closed/Deleted still reflect what succeeded."`
	PartialErrors []string `json:"partial_errors,omitempty" doc:"Human-readable errors from failed teardown steps."`
}

// humaHandleConvoyList is the Huma-typed handler for GET /v0/convoys.
func (s *Server) humaHandleConvoyList(ctx context.Context, input *ConvoyListInput) (*ListOutput[beads.Bead], error) {
	bp := input.toBlockingParams()
	if bp.isBlocking() {
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
		pp.IsPaging = true
	}

	stores := s.state.BeadStores()
	rigNames := sortedRigNames(stores)
	var convoys []beads.Bead
	var pa partialAggregator
	for _, rigName := range rigNames {
		store := stores[rigName]
		pa.attempt()
		list, err := store.List(beads.ListQuery{Type: "convoy"})
		if err != nil {
			pa.record("rig "+rigName, err)
			continue
		}
		pa.success()
		convoys = append(convoys, list...)
	}
	if pa.totalOutage() {
		return nil, pa.outageError()
	}

	if convoys == nil {
		convoys = []beads.Bead{}
	}

	index := s.latestIndex()
	cacheAge := cacheAgeSeconds(cityStore)
	if !pp.IsPaging {
		total := len(convoys)
		if pp.Limit < len(convoys) {
			convoys = convoys[:pp.Limit]
		}
		return &ListOutput[beads.Bead]{
			Index:     index,
			CacheAgeS: cacheAge,
			Body: ListBody[beads.Bead]{
				Items:         convoys,
				Total:         total,
				Partial:       pa.partial(),
				PartialErrors: pa.messages(),
			},
		}, nil
	}

	page, total, nextCursor := paginate(convoys, pp)
	if page == nil {
		page = []beads.Bead{}
	}
	return &ListOutput[beads.Bead]{
		Index:     index,
		CacheAgeS: cacheAge,
		Body: ListBody[beads.Bead]{
			Items:         page,
			Total:         total,
			NextCursor:    nextCursor,
			Partial:       pa.partial(),
			PartialErrors: pa.messages(),
		},
	}, nil
}

// humaHandleConvoyGet is the Huma-typed handler for GET /v0/convoy/{id}.
func (s *Server) humaHandleConvoyGet(_ context.Context, input *ConvoyGetInput) (*IndexOutput[convoyGetResponse], error) {
	id := input.ID

	cityStore := s.state.CityBeadStore()
	if err := cacheLiveOr503(cityStore); err != nil {
		return nil, err
	}

	// Formula-compiled convoy (graph workflow): build the full DAG snapshot.
	if isGraphConvoyID(s, id) {
		index := s.latestIndex()
		snapshot, err := s.buildWorkflowSnapshot(id, "", "", index)
		if err != nil {
			if errors.Is(err, errWorkflowNotFound) {
				return nil, apierr.WorkflowNotFound.Msg("workflow " + id + " not found")
			}
			return nil, apierr.Internal.Msg("workflow snapshot failed")
		}
		return &IndexOutput[convoyGetResponse]{
			Index:     index,
			CacheAgeS: cacheAgeSeconds(cityStore),
			Body:      convoyGetResponse{workflowSnapshotResponse: snapshot},
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
			return nil, apierr.Internal.Msg(err.Error())
		}
		if b.Type != "convoy" {
			return nil, apierr.ConvoyNotFound.Msg("bead " + id + " is not a convoy")
		}

		children, err := convoycore.Members(store, id, true)
		if err != nil {
			return nil, apierr.Internal.Msg(err.Error())
		}
		if children == nil {
			children = []beads.Bead{}
		}

		total := len(children)
		closed := 0
		for _, c := range children {
			if convoycore.IsTerminalStatus(c.Status) {
				closed++
			}
		}

		return &IndexOutput[convoyGetResponse]{
			Index:     s.latestIndex(),
			CacheAgeS: cacheAgeSeconds(cityStore),
			Body: convoyGetResponse{
				Convoy:   &b,
				Children: children,
				Progress: &convoyProgress{Total: total, Closed: closed},
			},
		}, nil
	}
	return nil, apierr.ConvoyNotFound.Msg("convoy " + id + " not found")
}

// humaHandleConvoyCreate is the Huma-typed handler for POST /v0/convoys.
// Title required via struct tag on ConvoyCreateInput.
func (s *Server) humaHandleConvoyCreate(_ context.Context, input *ConvoyCreateInput) (*IndexOutput[beads.Bead], error) {
	// Idempotency: create at most once per Idempotency-Key. Item validation,
	// the convoy bead create, and the link loop (with its rollback) all live in
	// the closure so a failed create releases the reservation for retry.
	convoy, err := withIdempotency(s, "/v0/convoys", input.IdempotencyKey, input.Body,
		func() (beads.Bead, error) {
			store := s.findStore(input.Body.Rig)
			if store == nil {
				return beads.Bead{}, apierr.InvalidRequest.Msg("rig is required when multiple rigs are configured")
			}

			// Pre-validate all items exist before creating the convoy.
			for _, itemID := range input.Body.Items {
				if _, err := store.Get(itemID); err != nil {
					return beads.Bead{}, storeError(err)
				}
			}

			created, err := store.Create(beads.Bead{
				Title: input.Body.Title,
				Type:  "convoy",
			})
			if err != nil {
				return beads.Bead{}, apierr.Internal.Msg(err.Error())
			}

			// Link child items to convoy one at a time. On first failure, roll
			// back previously-created tracks deps and THEN delete the new convoy.
			applied := make([]string, 0, len(input.Body.Items))
			for _, itemID := range input.Body.Items {
				if err := convoycore.TrackItem(store, created.ID, itemID); err != nil {
					rollbackConvoyTracks(store, created.ID, applied, "convoy.create")
					if delErr := store.Delete(created.ID); delErr != nil {
						log.Printf("gc api: convoy create rollback: delete %s after link failure: %v", created.ID, delErr)
					}
					return beads.Bead{}, apierr.Internal.Msg("failed to link item " + itemID + ": " + err.Error())
				}
				applied = append(applied, itemID)
			}
			return created, nil
		})
	if err != nil {
		return nil, err
	}

	return &IndexOutput[beads.Bead]{
		Index: s.latestIndex(),
		Body:  convoy,
	}, nil
}

// humaHandleConvoyAdd is the Huma-typed handler for POST /v0/convoy/{id}/add.
// Applies each tracks link one at a time; on first failure, rolls back
// previously-applied links so the convoy never ends up half-added.
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
			return nil, apierr.Internal.Msg(err.Error())
		}
		if b.Type != "convoy" {
			return nil, apierr.InvalidRequest.Msg("bead " + id + " is not a convoy")
		}
		// Pre-validate all items exist before linking.
		for _, itemID := range input.Body.Items {
			if _, err := store.Get(itemID); err != nil {
				return nil, storeError(err)
			}
		}
		applied := make([]string, 0, len(input.Body.Items))
		for _, itemID := range input.Body.Items {
			if err := convoycore.TrackItem(store, id, itemID); err != nil {
				rollbackConvoyTracks(store, id, applied, "convoy.add")
				return nil, apierr.Internal.Msg("failed to link item " + itemID + ": " + err.Error())
			}
			applied = append(applied, itemID)
		}
		resp := &OKResponse{}
		resp.Body.Status = "updated"
		return resp, nil
	}
	return nil, apierr.ConvoyNotFound.Msg("convoy " + id + " not found")
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
			return nil, apierr.Internal.Msg(err.Error())
		}
		if b.Type != "convoy" {
			return nil, apierr.InvalidRequest.Msg("bead " + id + " is not a convoy")
		}
		// Pre-validate all items exist and belong to this convoy via either
		// legacy parent-child membership or the current tracks dependency.
		snapshots := make(map[string]convoyMembershipSnapshot, len(input.Body.Items))
		for _, itemID := range input.Body.Items {
			item, gerr := store.Get(itemID)
			if gerr != nil {
				if errors.Is(gerr, beads.ErrNotFound) {
					return nil, apierr.BeadNotFound.Msg("item " + itemID + " not found")
				}
				return nil, apierr.Internal.Msg(gerr.Error())
			}
			hadTrack, terr := convoycore.HasTrack(store, id, itemID)
			if terr != nil {
				return nil, apierr.Internal.Msg(terr.Error())
			}
			if item.ParentID != id && !hadTrack {
				return nil, apierr.InvalidRequest.Msg("item " + itemID + " does not belong to convoy " + id)
			}
			snapshots[itemID] = convoyMembershipSnapshot{
				ParentID: item.ParentID,
				HadTrack: hadTrack,
			}
		}

		applied := make([]string, 0, len(input.Body.Items))
		empty := ""
		for _, itemID := range input.Body.Items {
			snapshot := snapshots[itemID]
			if snapshot.HadTrack {
				if err := convoycore.UntrackItem(store, id, itemID); err != nil {
					rollbackConvoyMembershipRemoval(store, id, applied, snapshots, "convoy.remove")
					return nil, apierr.Internal.Msg("failed to unlink item " + itemID + ": " + err.Error())
				}
			}
			if snapshot.ParentID == id {
				if err := store.Update(itemID, beads.UpdateOpts{ParentID: &empty}); err != nil {
					if snapshot.HadTrack {
						if trackErr := convoycore.TrackItem(store, id, itemID); trackErr != nil {
							log.Printf("gc api: convoy.remove rollback failed for current item %s tracks dep: %v", itemID, trackErr)
						}
					}
					rollbackConvoyMembershipRemoval(store, id, applied, snapshots, "convoy.remove")
					return nil, apierr.Internal.Msg("failed to unlink item " + itemID + ": " + err.Error())
				}
			}
			applied = append(applied, itemID)
		}
		resp := &OKResponse{}
		resp.Body.Status = "updated"
		return resp, nil
	}
	return nil, apierr.ConvoyNotFound.Msg("convoy " + id + " not found")
}

func rollbackConvoyTracks(store beads.Store, convoyID string, applied []string, op string) {
	for i := len(applied) - 1; i >= 0; i-- {
		itemID := applied[i]
		if err := convoycore.UntrackItem(store, convoyID, itemID); err != nil {
			log.Printf("gc api: %s rollback failed for item %s tracks dep: %v", op, itemID, err)
		}
	}
}

type convoyMembershipSnapshot struct {
	ParentID string
	HadTrack bool
}

func rollbackConvoyMembershipRemoval(store beads.Store, convoyID string, applied []string, snapshots map[string]convoyMembershipSnapshot, op string) {
	for i := len(applied) - 1; i >= 0; i-- {
		itemID := applied[i]
		snapshot := snapshots[itemID]
		if snapshot.ParentID == convoyID {
			prev := snapshot.ParentID
			if err := store.Update(itemID, beads.UpdateOpts{ParentID: &prev}); err != nil {
				log.Printf("gc api: %s rollback failed for item %s parent (→ %q): %v", op, itemID, prev, err)
			}
		}
		if snapshot.HadTrack {
			if err := convoycore.TrackItem(store, convoyID, itemID); err != nil {
				log.Printf("gc api: %s rollback failed for item %s tracks dep: %v", op, itemID, err)
			}
		}
	}
}

// humaHandleConvoyCheck is the Huma-typed handler for GET /v0/convoy/{id}/check.
func (s *Server) humaHandleConvoyCheck(_ context.Context, input *ConvoyCheckInput) (*IndexOutput[convoyCheckResponse], error) {
	id := input.ID

	cityStore := s.state.CityBeadStore()
	if err := cacheLiveOr503(cityStore); err != nil {
		return nil, err
	}

	stores := s.state.BeadStores()

	for _, rigName := range sortedRigNames(stores) {
		store := stores[rigName]
		b, err := store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, apierr.Internal.Msg(err.Error())
		}
		if b.Type != "convoy" {
			return nil, apierr.InvalidRequest.Msg("bead " + id + " is not a convoy")
		}

		children, err := convoycore.Members(store, id, true)
		if err != nil {
			return nil, apierr.Internal.Msg(err.Error())
		}

		total := len(children)
		closed := 0
		for _, c := range children {
			if convoycore.IsTerminalStatus(c.Status) {
				closed++
			}
		}

		complete := total > 0 && closed == total
		return &IndexOutput[convoyCheckResponse]{
			Index:     s.latestIndex(),
			CacheAgeS: cacheAgeSeconds(cityStore),
			Body: convoyCheckResponse{
				ConvoyID: id,
				Total:    total,
				Closed:   closed,
				Complete: complete,
			},
		}, nil
	}
	return nil, apierr.ConvoyNotFound.Msg("convoy " + id + " not found")
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
			return nil, apierr.Internal.Msg(err.Error())
		}
		if b.Type != "convoy" {
			return nil, apierr.InvalidRequest.Msg("bead " + id + " is not a convoy")
		}
		if err := store.Close(id); err != nil {
			return nil, apierr.Internal.Msg(err.Error())
		}
		resp := &OKResponse{}
		resp.Body.Status = "closed"
		return resp, nil
	}
	return nil, apierr.ConvoyNotFound.Msg("convoy " + id + " not found")
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
			return nil, apierr.Internal.Msg(err.Error())
		}
		if b.Type != "convoy" {
			return nil, apierr.InvalidRequest.Msg("bead " + id + " is not a convoy")
		}
		if err := store.Close(id); err != nil {
			return nil, apierr.Internal.Msg(err.Error())
		}
		resp := &OKResponse{}
		resp.Body.Status = "closed"
		return resp, nil
	}
	return nil, apierr.ConvoyNotFound.Msg("convoy " + id + " not found")
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
				beadmeta.KindMetadataKey:       beadmeta.KindWorkflow,
				beadmeta.WorkflowIDMetadataKey: workflowID,
			},
			IncludeClosed: true,
		}); err == nil {
			for _, root := range roots {
				addRoot(root)
			}
		}
		for _, rootID := range rootIDs {
			all, err := info.store.List(beads.ListQuery{
				Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
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
		info.store.CloseAll(ids, map[string]string{ //nolint:errcheck
			beadmeta.OutcomeMetadataKey: beadmeta.OutcomeSkipped,
			"close_reason":              sourceworkflow.WorkflowSkippedCloseReason,
		})
	}

	if !found {
		return nil, apierr.WorkflowNotFound.Msg("workflow " + workflowID + " not found")
	}

	resp := &OKResponse{}
	resp.Body.Status = "deleted"
	return resp, nil
}

// storeError converts a bead store error into an apierr problem. It runs during
// convoy create/add item pre-validation, so a not-found is a missing member bead.
func storeError(err error) error {
	if errors.Is(err, beads.ErrNotFound) {
		return apierr.BeadNotFound.Msg(err.Error())
	}
	return apierr.Internal.Msg(err.Error())
}

// humaHandleWorkflowGet is the Huma-typed handler for GET /v0/workflow/{workflow_id}.
// Backward-compatible alias for the convoy/workflow snapshot endpoint.
func (s *Server) humaHandleWorkflowGet(_ context.Context, input *WorkflowGetInput) (*IndexOutput[workflowSnapshotResponse], error) {
	workflowID := strings.TrimSpace(input.WorkflowID)
	if workflowID == "" {
		return nil, apierr.InvalidRequest.Msg("convoy id is required")
	}

	scopeKind, scopeRef, scopeErr := parseOptionalWorkflowRequestScope(input.ScopeKind, input.ScopeRef)
	if scopeErr != "" {
		return nil, apierr.InvalidRequest.Msg(scopeErr)
	}
	index := s.latestIndex()

	snapshot, err := s.buildWorkflowSnapshot(workflowID, scopeKind, scopeRef, index)
	if err != nil {
		if errors.Is(err, errWorkflowNotFound) {
			return nil, apierr.WorkflowNotFound.Msg("workflow " + workflowID + " not found")
		}
		return nil, apierr.Internal.Msg("workflow snapshot failed")
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
}, error,
) {
	workflowID := strings.TrimSpace(input.WorkflowID)
	if workflowID == "" {
		return nil, apierr.InvalidRequest.Msg("convoy id is required")
	}

	scopeKind := strings.TrimSpace(input.ScopeKind)
	scopeRef := strings.TrimSpace(input.ScopeRef)
	deleteFromStore := input.Delete

	stores := s.workflowStores()

	closed := 0
	deleted := 0
	found := false
	var pa partialAggregator

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
		} else if !errors.Is(err, beads.ErrNotFound) {
			pa.record("store "+info.scopeRef+" get root", err)
		}
		if roots, err := info.store.List(beads.ListQuery{
			Metadata: map[string]string{
				beadmeta.KindMetadataKey:       beadmeta.KindWorkflow,
				beadmeta.WorkflowIDMetadataKey: workflowID,
			},
			IncludeClosed: true,
		}); err == nil {
			for _, root := range roots {
				addRoot(root)
			}
		} else {
			pa.record("store "+info.scopeRef+" list roots", err)
		}
		for _, rootID := range rootIDs {
			all, err := info.store.List(beads.ListQuery{
				Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
				IncludeClosed: true,
			})
			if err != nil {
				pa.record("store "+info.scopeRef+" list descendants", err)
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
		n, closeErr := info.store.CloseAll(ids, map[string]string{
			beadmeta.OutcomeMetadataKey: beadmeta.OutcomeSkipped,
			"close_reason":              sourceworkflow.WorkflowSkippedCloseReason,
		})
		closed += n
		if closeErr != nil {
			pa.record("store "+info.scopeRef+" close", closeErr)
		}

		// Phase 2: Delete if requested.
		if deleteFromStore {
			for _, id := range ids {
				if deps, err := info.store.DepList(id, "down"); err == nil {
					for _, dep := range deps {
						if err := info.store.DepRemove(id, dep.DependsOnID); err != nil {
							pa.record("store "+info.scopeRef+" dep-remove "+id+"→"+dep.DependsOnID, err)
						}
					}
				} else {
					pa.record("store "+info.scopeRef+" dep-list down "+id, err)
				}
				if deps, err := info.store.DepList(id, "up"); err == nil {
					for _, dep := range deps {
						if err := info.store.DepRemove(dep.IssueID, id); err != nil {
							pa.record("store "+info.scopeRef+" dep-remove "+dep.IssueID+"→"+id, err)
						}
					}
				} else {
					pa.record("store "+info.scopeRef+" dep-list up "+id, err)
				}
				if err := info.store.Delete(id); err != nil {
					pa.record("store "+info.scopeRef+" delete "+id, err)
					continue
				}
				deleted++
			}
		}
	}

	if !found {
		return nil, apierr.WorkflowNotFound.Msg("workflow " + workflowID + " not found")
	}

	return &struct {
		Body workflowDeleteResponse
	}{Body: workflowDeleteResponse{
		WorkflowID:    workflowID,
		Closed:        closed,
		Deleted:       deleted,
		Partial:       pa.partial(),
		PartialErrors: pa.messages(),
	}}, nil
}
