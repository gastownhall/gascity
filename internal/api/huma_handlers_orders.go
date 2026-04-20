package api

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

// OrderListBody is the response body for GET /v0/orders.
type OrderListBody struct {
	Orders []orderResponse `json:"orders" doc:"Registered orders."`
}

// OrderListOutput is the response envelope for GET /v0/orders.
type OrderListOutput struct {
	Body OrderListBody
}

// humaHandleOrderList is the Huma-typed handler for GET /v0/orders.
func (s *Server) humaHandleOrderList(_ context.Context, _ *OrderListInput) (*OrderListOutput, error) {
	aa := s.state.Orders()
	resp := make([]orderResponse, len(aa))
	for i, a := range aa {
		resp[i] = toOrderResponse(a)
	}
	out := &OrderListOutput{}
	out.Body.Orders = resp
	return out, nil
}

// humaHandleOrderGet is the Huma-typed handler for GET /v0/order/{name}.
func (s *Server) humaHandleOrderGet(_ context.Context, input *OrderGetInput) (*struct {
	Body orderResponse
}, error,
) {
	a, err := resolveOrder(s.state.Orders(), input.Name)
	if err != nil {
		if errors.Is(err, errOrderAmbiguous) {
			return nil, huma.Error409Conflict(err.Error())
		}
		return nil, huma.Error404NotFound(err.Error())
	}
	return &struct {
		Body orderResponse
	}{Body: toOrderResponse(*a)}, nil
}

// OrderCheckListBody is the response body for GET /v0/orders/check.
type OrderCheckListBody struct {
	Checks []orderCheckResponse `json:"checks" doc:"Order trigger evaluations."`
}

// OrderCheckListOutput is the response envelope for GET /v0/orders/check.
type OrderCheckListOutput struct {
	Body OrderCheckListBody
}

// humaHandleOrderCheck is the Huma-typed handler for GET /v0/orders/check.
func (s *Server) humaHandleOrderCheck(_ context.Context, _ *OrderCheckInput) (*OrderCheckListOutput, error) {
	aa := s.state.Orders()

	store := s.state.CityBeadStore()
	lastRunFn := beadLastRunFunc(store)
	ep := s.state.EventProvider()

	var cursorFn orders.CursorFunc
	if store != nil {
		cursorFn = func(name string) uint64 {
			label := "order-run:" + name
			results, err := store.List(beads.ListQuery{
				Label:         label,
				Limit:         10,
				IncludeClosed: true,
				Sort:          beads.SortCreatedDesc,
			})
			if err != nil || len(results) == 0 {
				return 0
			}
			var labelSets [][]string
			for _, b := range results {
				labelSets = append(labelSets, b.Labels)
			}
			return orders.MaxSeqFromLabels(labelSets)
		}
	}

	now := time.Now()
	checks := make([]orderCheckResponse, 0, len(aa))
	for _, a := range aa {
		result := orders.CheckTrigger(a, now, lastRunFn, ep, cursorFn)
		cr := orderCheckResponse{
			Name:       a.Name,
			ScopedName: a.ScopedName(),
			Rig:        a.Rig,
			Due:        result.Due,
			Reason:     result.Reason,
		}
		if !result.LastRun.IsZero() {
			ts := result.LastRun.Format(time.RFC3339)
			cr.LastRun = &ts
		}
		if store != nil {
			label := "order-run:" + a.ScopedName()
			if results, err := store.List(beads.ListQuery{
				Label:         label,
				Limit:         1,
				IncludeClosed: true,
				Sort:          beads.SortCreatedDesc,
			}); err == nil && len(results) > 0 {
				outcome := lastRunOutcomeFromLabels(results[0].Labels)
				if outcome != "" {
					cr.LastRunOutcome = &outcome
				}
			}
		}
		checks = append(checks, cr)
	}

	if checks == nil {
		checks = []orderCheckResponse{}
	}

	out := &OrderCheckListOutput{}
	out.Body.Checks = checks
	return out, nil
}

// orderCheckResponse is the response item for GET /v0/orders/check.
type orderCheckResponse struct {
	Name           string  `json:"name"`
	ScopedName     string  `json:"scoped_name"`
	Rig            string  `json:"rig,omitempty"`
	Due            bool    `json:"due"`
	Reason         string  `json:"reason"`
	LastRun        *string `json:"last_run,omitempty"`
	LastRunOutcome *string `json:"last_run_outcome,omitempty"`
}

// OrderHistoryListBody is the response body for GET /v0/orders/history.
type OrderHistoryListBody struct {
	Entries []orderHistoryEntry `json:"entries" doc:"Order history entries."`
}

// OrderHistoryListOutput is the response envelope for GET /v0/orders/history.
type OrderHistoryListOutput struct {
	Body OrderHistoryListBody
}

// humaHandleOrderHistory is the Huma-typed handler for GET /v0/orders/history.
func (s *Server) humaHandleOrderHistory(_ context.Context, input *OrderHistoryInput) (*OrderHistoryListOutput, error) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}

	scopedName := input.ScopedName
	if scopedName == "" {
		return nil, huma.Error400BadRequest("scoped_name is required")
	}

	limit := 20
	if input.Limit > 0 {
		limit = input.Limit
	}

	var beforeTime time.Time
	if input.Before != "" {
		t, err := time.Parse(time.RFC3339, input.Before)
		if err != nil {
			return nil, huma.Error400BadRequest("invalid before timestamp: must be RFC3339, got " + strconv.Quote(input.Before))
		}
		beforeTime = t
	}

	aa := s.state.Orders()
	var auto *orders.Order
	for i, a := range aa {
		if a.ScopedName() == scopedName {
			auto = &aa[i]
			break
		}
	}

	label := "order-run:" + scopedName
	fetchLimit := limit
	if !beforeTime.IsZero() {
		fetchLimit = limit * 3
	}
	results, err := store.List(beads.ListQuery{
		Label:         label,
		Limit:         fetchLimit,
		IncludeClosed: true,
		Sort:          beads.SortCreatedDesc,
	})
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}

	entries := make([]orderHistoryEntry, 0, len(results))
	for _, b := range results {
		if !beforeTime.IsZero() && !b.CreatedAt.Before(beforeTime) {
			continue
		}

		name := scopedName
		rig := ""
		if auto != nil {
			name = auto.Name
			rig = auto.Rig
		} else if idx := strings.Index(scopedName, ":rig:"); idx >= 0 {
			name = scopedName[:idx]
			rig = scopedName[idx+5:]
		}

		entry := orderHistoryEntry{
			BeadID:        b.ID,
			Name:          name,
			ScopedName:    scopedName,
			Rig:           rig,
			CreatedAt:     b.CreatedAt.Format(time.RFC3339),
			Labels:        b.Labels,
			CaptureOutput: auto != nil && auto.IsExec(),
		}

		if b.Metadata != nil {
			if v, ok := b.Metadata["convergence.gate_duration_ms"]; ok && v != "" {
				entry.DurationMs = &v
			}
			if v, ok := b.Metadata["convergence.gate_exit_code"]; ok && v != "" {
				entry.ExitCode = &v
			}
		}

		entry.HasOutput = entry.CaptureOutput

		entries = append(entries, entry)
		if len(entries) >= limit {
			break
		}
	}

	out := &OrderHistoryListOutput{}
	out.Body.Entries = entries
	return out, nil
}

// orderHistoryEntry is a single entry in the order history response.
type orderHistoryEntry struct {
	BeadID        string   `json:"bead_id"`
	Name          string   `json:"name"`
	ScopedName    string   `json:"scoped_name"`
	Rig           string   `json:"rig,omitempty"`
	CreatedAt     string   `json:"created_at"`
	Labels        []string `json:"labels"`
	DurationMs    *string  `json:"duration_ms,omitempty"`
	ExitCode      *string  `json:"exit_code,omitempty"`
	Signal        *string  `json:"signal,omitempty"`
	Error         *string  `json:"error,omitempty"`
	WispRootID    *string  `json:"wisp_root_id,omitempty"`
	CaptureOutput bool     `json:"capture_output"`
	HasOutput     bool     `json:"has_output"`
}

// humaHandleOrderHistoryDetail is the Huma-typed handler for GET /v0/order/history/{bead_id}.
func (s *Server) humaHandleOrderHistoryDetail(_ context.Context, input *OrderHistoryDetailInput) (*struct {
	Body orderHistoryDetailResponse
}, error,
) {
	store := s.state.CityBeadStore()
	if store == nil {
		return nil, huma.Error503ServiceUnavailable("no bead store configured")
	}

	b, err := store.Get(input.BeadID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, huma.Error404NotFound("bead not found")
		}
		return nil, huma.Error500InternalServerError(err.Error())
	}

	output := ""
	if b.Metadata != nil {
		if stdout := b.Metadata["convergence.gate_stdout"]; stdout != "" {
			output = stdout
		}
		if stderr := b.Metadata["convergence.gate_stderr"]; stderr != "" {
			if output != "" {
				output += "\n"
			}
			output += stderr
		}
	}

	return &struct {
		Body orderHistoryDetailResponse
	}{Body: orderHistoryDetailResponse{
		BeadID:    b.ID,
		CreatedAt: b.CreatedAt.Format(time.RFC3339),
		Labels:    b.Labels,
		Output:    output,
	}}, nil
}

// orderHistoryDetailResponse is the response for GET /v0/order/history/{bead_id}.
type orderHistoryDetailResponse struct {
	BeadID    string   `json:"bead_id"`
	CreatedAt string   `json:"created_at"`
	Labels    []string `json:"labels"`
	Output    string   `json:"output"`
}

// humaHandleOrderEnable is the Huma-typed handler for POST /v0/order/{name}/enable.
func (s *Server) humaHandleOrderEnable(_ context.Context, input *OrderEnableInput) (*OKResponse, error) {
	return s.setOrderEnabledHuma(input.Name, true)
}

// humaHandleOrderDisable is the Huma-typed handler for POST /v0/order/{name}/disable.
func (s *Server) humaHandleOrderDisable(_ context.Context, input *OrderDisableInput) (*OKResponse, error) {
	return s.setOrderEnabledHuma(input.Name, false)
}

// ordersFeedBody is the response body for GET /v0/orders/feed.
type ordersFeedBody struct {
	Items         []monitorFeedItemResponse `json:"items"`
	Partial       bool                      `json:"partial"`
	PartialErrors []string                  `json:"partial_errors,omitempty"`
}

// humaHandleOrdersFeed is the Huma-typed handler for GET /v0/orders/feed.
func (s *Server) humaHandleOrdersFeed(_ context.Context, input *OrdersFeedInput) (*struct {
	Body ordersFeedBody
}, error,
) {
	scopeKind, scopeRef, scopeErr := parseWorkflowRequestScope(input.ScopeKind, input.ScopeRef)
	if scopeErr != "" {
		return nil, huma.Error400BadRequest(scopeErr)
	}

	limit := normalizeFeedLimit(input.Limit)
	index := s.latestIndex()

	cacheKey := "orders-feed?" + scopeKind + "|" + scopeRef + "|" + strconv.Itoa(input.Limit)
	if body, ok := cachedResponseAs[ordersFeedBody](s, cacheKey, index); ok {
		return &struct {
			Body ordersFeedBody
		}{Body: body}, nil
	}

	workflowRuns, err := buildWorkflowRunProjections(s.state, scopeKind, scopeRef, "")
	if err != nil {
		return nil, huma.Error500InternalServerError("workflow feed failed")
	}
	orderRuns, err := buildOrderRunFeedItems(s.state, scopeKind, scopeRef)
	if err != nil {
		return nil, huma.Error500InternalServerError("order feed failed")
	}

	items := make([]monitorFeedItemResponse, 0, len(workflowRuns.Items)+len(orderRuns))
	for _, run := range workflowRuns.Items {
		items = append(items, workflowRunProjectionFeedItem(run))
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

	body := ordersFeedBody{
		Items:   items,
		Partial: workflowRuns.Partial,
	}
	if len(workflowRuns.PartialErrors) > 0 {
		body.PartialErrors = workflowRuns.PartialErrors
	}

	s.storeResponse(cacheKey, index, body)

	return &struct {
		Body ordersFeedBody
	}{Body: body}, nil
}

func (s *Server) setOrderEnabledHuma(name string, enabled bool) (*OKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	a, err := resolveOrder(s.state.Orders(), name)
	if err != nil {
		if errors.Is(err, errOrderAmbiguous) {
			return nil, huma.Error409Conflict(err.Error())
		}
		return nil, huma.Error404NotFound(err.Error())
	}

	if enabled {
		err = sm.EnableOrder(a.Name, a.Rig)
	} else {
		err = sm.DisableOrder(a.Name, a.Rig)
	}
	if err != nil {
		return nil, mutationError(err)
	}

	resp := &OKResponse{}
	if enabled {
		resp.Body.Status = "enabled"
	} else {
		resp.Body.Status = "disabled"
	}
	return resp, nil
}
