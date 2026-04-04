package api

import (
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

const maxOrdersFeedLimit = 500

type monitorFeedItemResponse struct {
	ID                 string `json:"id"`
	Type               string `json:"type"`
	Status             string `json:"status"`
	Title              string `json:"title"`
	ScopeKind          string `json:"scope_kind"`
	ScopeRef           string `json:"scope_ref"`
	Target             string `json:"target"`
	StartedAt          string `json:"started_at"`
	UpdatedAt          string `json:"updated_at"`
	BeadID             string `json:"bead_id,omitempty"`
	DetailAvailable    bool   `json:"detail_available,omitempty"`
	WorkflowID         string `json:"workflow_id,omitempty"`
	RootBeadID         string `json:"root_bead_id,omitempty"`
	RootStoreRef       string `json:"root_store_ref,omitempty"`
	AttachedBeadID     string `json:"attached_bead_id,omitempty"`
	LogicalBeadID      string `json:"logical_bead_id,omitempty"`
	RunDetailAvailable bool   `json:"run_detail_available,omitempty"`
}

type workflowRunProjection struct {
	WorkflowID     string
	FormulaName    string
	Title          string
	Status         string
	Target         string
	StartedAt      time.Time
	UpdatedAt      time.Time
	ScopeKind      string
	ScopeRef       string
	RootBeadID     string
	RootStoreRef   string
	AttachedBeadID string
}

type workflowRunProjectionResult struct {
	Items         []workflowRunProjection
	Partial       bool
	PartialErrors []string
}

func (s *Server) handleOrdersFeed(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	scopeKind, scopeRef, scopeErr := parseWorkflowRequestScope(q.Get("scope_kind"), q.Get("scope_ref"))
	if scopeErr != "" {
		writeError(w, http.StatusBadRequest, "invalid", scopeErr)
		return
	}

	limit := parseOrdersFeedLimit(q.Get("limit"))
	index := s.latestIndex()
	cacheKey := responseCacheKey("orders-feed", r)
	if body, ok := s.cachedResponse(cacheKey, index); ok {
		writeCachedJSON(w, r, index, body)
		return
	}

	workflowRuns, err := buildWorkflowRunProjections(s.state, scopeKind, scopeRef)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "workflow feed failed")
		return
	}
	orderRuns, err := buildOrderRunFeedItems(s.state, scopeKind, scopeRef)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "order feed failed")
		return
	}

	items := make([]monitorFeedItemResponse, 0, len(workflowRuns.Items)+len(orderRuns))
	for _, run := range workflowRuns.Items {
		item := monitorFeedItemResponse{
			ID:                 run.WorkflowID,
			Type:               "formula",
			Status:             run.Status,
			Title:              run.Title,
			ScopeKind:          run.ScopeKind,
			ScopeRef:           run.ScopeRef,
			Target:             run.Target,
			StartedAt:          run.StartedAt.Format(time.RFC3339Nano),
			UpdatedAt:          run.UpdatedAt.Format(time.RFC3339Nano),
			WorkflowID:         run.WorkflowID,
			RootBeadID:         run.RootBeadID,
			RootStoreRef:       run.RootStoreRef,
			AttachedBeadID:     run.AttachedBeadID,
			LogicalBeadID:      run.RootBeadID,
			RunDetailAvailable: true,
		}
		items = append(items, item)
	}
	items = append(items, orderRuns...)

	sort.SliceStable(items, func(i, j int) bool {
		iRank := monitorStatusRank(items[i].Status)
		jRank := monitorStatusRank(items[j].Status)
		if iRank != jRank {
			return iRank < jRank
		}
		iTypeRank := monitorItemRank(items[i])
		jTypeRank := monitorItemRank(items[j])
		if iTypeRank != jTypeRank {
			return iTypeRank < jTypeRank
		}
		iUpdated := parseMonitorTimestamp(items[i].UpdatedAt)
		jUpdated := parseMonitorTimestamp(items[j].UpdatedAt)
		if !iUpdated.Equal(jUpdated) {
			return iUpdated.After(jUpdated)
		}
		return items[i].Title < items[j].Title
	})

	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}

	resp := map[string]any{
		"items":   items,
		"partial": workflowRuns.Partial,
	}
	if len(workflowRuns.PartialErrors) > 0 {
		resp["partial_errors"] = workflowRuns.PartialErrors
	}

	body, err := s.storeResponse(cacheKey, index, resp)
	if err != nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	writeCachedJSON(w, r, index, body)
}

func buildWorkflowRunProjections(state State, requestedScopeKind, requestedScopeRef string) (workflowRunProjectionResult, error) {
	stores := workflowStores(state)
	projections := make([]workflowRunProjection, 0)
	partialErrors := make([]string, 0)
	cityScopeRef := workflowCityScopeRef(state.CityName())
	includeAllForCity := requestedScopeKind == "city" && requestedScopeRef == cityScopeRef
	var requestedScopeErr error

	for _, info := range stores {
		if info.store == nil {
			continue
		}
		all, err := info.store.ListOpen()
		if err != nil {
			if requestedScopeErr == nil && info.scopeKind == requestedScopeKind && info.scopeRef == requestedScopeRef {
				requestedScopeErr = err
			}
			if includeAllForCity {
				msg := info.ref + " store unavailable"
				log.Printf("api: workflow run projection list failed for %s: %v", info.ref, err)
				partialErrors = append(partialErrors, msg)
			}
			continue
		}

		childrenByRoot := make(map[string][]beads.Bead)
		for _, bead := range all {
			rootID := strings.TrimSpace(bead.Metadata["gc.root_bead_id"])
			if rootID == "" {
				continue
			}
			childrenByRoot[rootID] = append(childrenByRoot[rootID], bead)
		}

		for _, bead := range all {
			if !isWorkflowRoot(bead) || strings.TrimSpace(bead.Metadata["gc.formula_contract"]) != "graph.v2" {
				continue
			}

			scopeKind, scopeRef := workflowProjectionScope(info, bead, cityScopeRef, requestedScopeKind, requestedScopeRef)
			if !includeAllForCity && (scopeKind != requestedScopeKind || scopeRef != requestedScopeRef) {
				continue
			}

			runBeads := append([]beads.Bead{bead}, childrenByRoot[bead.ID]...)
			projection := workflowRunProjection{
				WorkflowID:     resolvedWorkflowID(bead),
				FormulaName:    workflowFormulaName(bead),
				Title:          workflowProjectionTitle(bead),
				Status:         normalizeMonitorStatus(aggregateWorkflowRunStatus(bead, runBeads)),
				Target:         workflowProjectionTarget(bead),
				StartedAt:      bead.CreatedAt,
				UpdatedAt:      workflowProjectionUpdatedAt(runBeads),
				ScopeKind:      scopeKind,
				ScopeRef:       scopeRef,
				RootBeadID:     bead.ID,
				RootStoreRef:   info.ref,
				AttachedBeadID: strings.TrimSpace(bead.Metadata["gc.source_bead_id"]),
			}
			projections = append(projections, projection)
		}
	}

	if len(projections) == 0 && requestedScopeErr != nil && !includeAllForCity {
		return workflowRunProjectionResult{}, requestedScopeErr
	}

	sort.SliceStable(projections, func(i, j int) bool {
		iRank := monitorStatusRank(projections[i].Status)
		jRank := monitorStatusRank(projections[j].Status)
		if iRank != jRank {
			return iRank < jRank
		}
		if !projections[i].UpdatedAt.Equal(projections[j].UpdatedAt) {
			return projections[i].UpdatedAt.After(projections[j].UpdatedAt)
		}
		return projections[i].WorkflowID < projections[j].WorkflowID
	})

	return workflowRunProjectionResult{
		Items:         projections,
		Partial:       len(partialErrors) > 0,
		PartialErrors: partialErrors,
	}, nil
}

func buildOrderRunFeedItems(state State, requestedScopeKind, requestedScopeRef string) ([]monitorFeedItemResponse, error) {
	store := state.CityBeadStore()
	if store == nil {
		return []monitorFeedItemResponse{}, nil
	}

	results, err := store.ListByLabel("order-tracking", 0)
	if err != nil {
		return nil, err
	}

	orderByScopedName := make(map[string]orders.Order, len(state.Orders()))
	for _, order := range state.Orders() {
		orderByScopedName[order.ScopedName()] = order
	}

	cityScopeRef := workflowCityScopeRef(state.CityName())
	includeAllForCity := requestedScopeKind == "city" && requestedScopeRef == cityScopeRef
	items := make([]monitorFeedItemResponse, 0, len(results))
	updatedAtByScopedName := make(map[string]time.Time, len(results))
	for _, bead := range results {
		scopedName := orderTrackingScopedName(bead)
		if scopedName == "" {
			continue
		}
		scopeKind, scopeRef := orderTrackingScope(scopedName, cityScopeRef)
		if !includeAllForCity && (scopeKind != requestedScopeKind || scopeRef != requestedScopeRef) {
			continue
		}

		updatedAt := updatedAtByScopedName[scopedName]
		if updatedAt.IsZero() {
			updatedAt = orderTrackingUpdatedAt(store, bead, scopedName)
			updatedAtByScopedName[scopedName] = updatedAt
		}

		orderDef, ok := orderByScopedName[scopedName]
		title := orderTrackingTitle(scopedName, orderDef, ok)
		target := orderTrackingTarget(orderDef, ok, bead)
		itemType := orderTrackingType(orderDef, ok, bead)
		item := monitorFeedItemResponse{
			ID:                 "order:" + bead.ID,
			Type:               itemType,
			Status:             normalizeMonitorStatus(orderTrackingStatus(bead)),
			Title:              title,
			ScopeKind:          scopeKind,
			ScopeRef:           scopeRef,
			Target:             target,
			StartedAt:          bead.CreatedAt.Format(time.RFC3339Nano),
			UpdatedAt:          updatedAt.Format(time.RFC3339Nano),
			BeadID:             bead.ID,
			DetailAvailable:    ok && orderDef.IsExec(),
			RunDetailAvailable: ok && orderDef.IsExec(),
		}
		items = append(items, item)
	}

	return items, nil
}

func orderTrackingUpdatedAt(store beads.Store, tracking beads.Bead, scopedName string) time.Time {
	updatedAt := tracking.CreatedAt
	if store == nil || strings.TrimSpace(scopedName) == "" {
		return updatedAt
	}

	runs, err := store.ListByLabel("order-run:"+scopedName, 1)
	if err != nil {
		return updatedAt
	}
	if len(runs) > 0 && runs[0].CreatedAt.After(updatedAt) {
		updatedAt = runs[0].CreatedAt
	}
	return updatedAt
}

func workflowProjectionScope(info workflowStoreInfo, root beads.Bead, cityScopeRef, requestedScopeKind, requestedScopeRef string) (string, string) {
	if scopeKind, scopeRef := workflowRootScope(root); scopeKind != "" && scopeRef != "" {
		return scopeKind, scopeRef
	}
	if preserveRequestedWorkflowScope(info, root, requestedScopeKind, requestedScopeRef, cityScopeRef) {
		return requestedScopeKind, requestedScopeRef
	}
	return info.scopeKind, info.scopeRef
}

func workflowFormulaName(root beads.Bead) string {
	if name := strings.TrimSpace(root.Ref); name != "" {
		return name
	}
	if name := strings.TrimSpace(root.Metadata["gc.formula_name"]); name != "" {
		return name
	}
	return root.ID
}

func workflowProjectionTitle(root beads.Bead) string {
	if title := strings.TrimSpace(root.Title); title != "" {
		return title
	}
	return workflowFormulaName(root)
}

func workflowProjectionTarget(root beads.Bead) string {
	for _, key := range []string{"gc.run_target", "gc.execution_routed_to", "gc.routed_to"} {
		if value := strings.TrimSpace(root.Metadata[key]); value != "" {
			return value
		}
	}
	return "workflow"
}

func workflowProjectionUpdatedAt(beadsForRun []beads.Bead) time.Time {
	var updatedAt time.Time
	for _, bead := range beadsForRun {
		if bead.CreatedAt.After(updatedAt) {
			updatedAt = bead.CreatedAt
		}
	}
	return updatedAt
}

func aggregateWorkflowRunStatus(root beads.Bead, beadsForRun []beads.Bead) string {
	if status := workflowStatus(root); isTerminalWorkflowStatus(status) {
		return status
	}

	best := workflowStatus(root)
	bestRank := statusRank(best)
	for _, bead := range beadsForRun {
		status := workflowStatus(bead)
		if status == "" {
			continue
		}
		rank := statusRank(status)
		if rank > bestRank {
			best = status
			bestRank = rank
		}
	}
	return best
}

func orderTrackingScopedName(bead beads.Bead) string {
	for _, label := range bead.Labels {
		if scopedName, ok := strings.CutPrefix(label, "order-run:"); ok && strings.TrimSpace(scopedName) != "" {
			return strings.TrimSpace(scopedName)
		}
	}
	return ""
}

func orderTrackingScope(scopedName, cityScopeRef string) (string, string) {
	if idx := strings.LastIndex(scopedName, ":rig:"); idx >= 0 {
		return "rig", scopedName[idx+5:]
	}
	return "city", cityScopeRef
}

func orderTrackingTitle(scopedName string, orderDef orders.Order, found bool) string {
	if found && strings.TrimSpace(orderDef.Name) != "" {
		return orderDef.Name
	}
	if idx := strings.LastIndex(scopedName, ":rig:"); idx >= 0 {
		return scopedName[:idx]
	}
	return scopedName
}

func orderTrackingTarget(orderDef orders.Order, found bool, bead beads.Bead) string {
	if found {
		if orderDef.IsExec() {
			return "exec"
		}
		if orderDef.Pool != "" {
			return qualifyOrderFeedTarget(orderDef.Pool, orderDef.Rig)
		}
		if orderDef.Formula != "" {
			return orderDef.Formula
		}
	}
	if containsString(bead.Labels, "exec") || containsString(bead.Labels, "exec-failed") {
		return "exec"
	}
	return "formula"
}

func qualifyOrderFeedTarget(pool, rig string) string {
	if rig == "" || strings.Contains(pool, "/") {
		return pool
	}
	return rig + "/" + pool
}

func orderTrackingType(orderDef orders.Order, found bool, bead beads.Bead) string {
	if found {
		if orderDef.IsExec() {
			return "exec"
		}
		return "formula"
	}
	if containsString(bead.Labels, "exec") || containsString(bead.Labels, "exec-failed") {
		return "exec"
	}
	return "formula"
}

func orderTrackingStatus(bead beads.Bead) string {
	if strings.TrimSpace(bead.Status) != "closed" {
		return "active"
	}
	if containsString(bead.Labels, "exec-failed") ||
		containsString(bead.Labels, "wisp-canceled") ||
		containsString(bead.Labels, "wisp-failed") {
		return "failed"
	}
	return "completed"
}

func parseOrdersFeedLimit(raw string) int {
	limit := 50
	if parsed, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && parsed > 0 {
		limit = parsed
	}
	if limit > maxOrdersFeedLimit {
		return maxOrdersFeedLimit
	}
	return limit
}

func normalizeMonitorStatus(status string) string {
	switch status {
	case "completed":
		return "done"
	default:
		return status
	}
}

func monitorStatusRank(status string) int {
	switch status {
	case "active":
		return 0
	case "pending":
		return 1
	case "failed":
		return 2
	case "done":
		return 3
	case "skipped":
		return 4
	default:
		return 5
	}
}

func monitorItemRank(item monitorFeedItemResponse) int {
	if strings.TrimSpace(item.WorkflowID) != "" {
		return 0
	}
	if item.Type == "formula" {
		return 1
	}
	return 2
}

func parseMonitorTimestamp(raw string) time.Time {
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return ts
}
