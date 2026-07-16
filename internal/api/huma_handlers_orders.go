package api

import (
	"context"
	"errors"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/events"
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
	a, err := resolveOrder(s.state.OrdersAll(), input.Name)
	if err != nil {
		if errors.Is(err, errOrderAmbiguous) {
			return nil, apierr.AmbiguousReference.Msg(err.Error())
		}
		return nil, apierr.OrderNotFound.Msg(err.Error())
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
func (s *Server) humaHandleOrderCheck(_ context.Context, input *OrderCheckInput) (*OrderCheckListOutput, error) {
	aa := s.state.Orders()

	ep := s.state.EventProvider()

	index := s.latestIndex()
	cacheKey := cacheKeyFor("orders-check", input)
	useResponseCache := !input.Fresh && !hasConditionOrder(aa)
	if useResponseCache {
		if body, ok := cachedResponseAs[OrderCheckListBody](s, cacheKey, index); ok {
			return &OrderCheckListOutput{Body: body}, nil
		}
	}

	now := time.Now()
	checks := make([]orderCheckResponse, 0, len(aa))
	for _, a := range aa {
		storeInfos, err := orderStoreInfosForState(s.state, a)
		if err != nil {
			storeInfos = nil
		}
		history, _ := orderHistoryBeadsAcrossStoreInfosForCheck(storeInfos, a.ScopedName(), 1, time.Time{}, input.Fresh)
		result := checkOrderTriggerForAPI(a, now, history, storeInfos, ep, input.Fresh)
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
		if len(history) > 0 {
			if run, ok := orders.RunFromTrackingBead(history[0].bead); ok {
				if outcome := run.Outcome.Display(); outcome != "" {
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
	if useResponseCache {
		s.storeResponse(cacheKey, index, out.Body)
	}
	return out, nil
}

func hasConditionOrder(aa []orders.Order) bool {
	for _, a := range aa {
		if a.Trigger == "condition" {
			return true
		}
	}
	return false
}

func checkOrderTriggerForAPI(a orders.Order, now time.Time, history []orderHistoryStoreBead, infos []workflowStoreInfo, ep events.Provider, fresh bool) orders.TriggerResult {
	lastRunFn := func(string) (time.Time, error) {
		if len(history) == 0 {
			return time.Time{}, nil
		}
		return history[0].bead.CreatedAt, nil
	}
	var cursorFn orders.CursorFunc
	if a.Trigger == "event" {
		if fresh {
			cursorFn = orders.CursorAcross(orderFrontDoorsFromWorkflowInfos(infos))
		} else {
			labelSets := make([][]string, 0, len(history))
			for _, row := range history {
				labelSets = append(labelSets, row.bead.Labels)
			}
			cursor := orders.MaxSeqFromLabels(labelSets)
			cursorFn = func(string) uint64 { return cursor }
		}
	}
	return orders.CheckTrigger(a, now, lastRunFn, ep, cursorFn)
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
	CacheAgeS float64 `header:"X-GC-Cache-Age-S" doc:"Age in seconds of the CachingStore snapshot that served this response (0 if not applicable)."`
	Body      OrderHistoryListBody
}

// humaHandleOrderHistory is the Huma-typed handler for GET /v0/orders/history.
func (s *Server) humaHandleOrderHistory(_ context.Context, input *OrderHistoryInput) (*OrderHistoryListOutput, error) {
	store := s.state.CityBeadStore()
	if err := cacheLiveOr503(store); err != nil {
		return nil, err
	}
	scopedName := input.ScopedName
	if scopedName == "" {
		return nil, apierr.InvalidRequest.Msg("scoped_name is required")
	}

	limit := 20
	if input.Limit > 0 {
		limit = input.Limit
	}

	var beforeTime time.Time
	if input.Before != "" {
		t, err := time.Parse(time.RFC3339, input.Before)
		if err != nil {
			return nil, apierr.InvalidRequest.Msg("invalid before timestamp: must be RFC3339, got " + strconv.Quote(input.Before))
		}
		beforeTime = t
	}

	aa := s.state.OrdersAll()
	var auto *orders.Order
	var orderDef orders.Order
	for i, a := range aa {
		if a.ScopedName() == scopedName {
			auto = &aa[i]
			orderDef = aa[i]
			break
		}
	}
	if auto == nil {
		orderDef = orders.Order{Name: scopedName}
		if idx := strings.Index(scopedName, ":rig:"); idx >= 0 {
			orderDef.Name = scopedName[:idx]
			orderDef.Rig = scopedName[idx+5:]
		}
	}
	storeInfos, err := orderStoreInfosForState(s.state, orderDef)
	if err != nil {
		if errors.Is(err, errNoOrderStores) {
			return nil, apierr.ServiceUnavailable.Msg(err.Error())
		}
		return nil, apierr.Internal.Msg(err.Error())
	}

	results, err := orderHistoryBeadsAcrossStoreInfos(storeInfos, scopedName, limit, beforeTime)
	if err != nil {
		return nil, apierr.Internal.Msg(err.Error())
	}

	entries := make([]orderHistoryEntry, 0, len(results))
	for _, result := range results {
		b := result.bead
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
			StoreRef:      result.storeRef,
			Name:          name,
			ScopedName:    scopedName,
			Rig:           rig,
			CreatedAt:     b.CreatedAt.Format(time.RFC3339),
			Labels:        b.Labels,
			CaptureOutput: auto != nil && auto.IsExec(),
		}

		gate := convergence.GateOutputFromMetadata(b.Metadata)
		if gate.DurationMs != "" {
			entry.DurationMs = &gate.DurationMs
		}
		if gate.ExitCode != "" {
			entry.ExitCode = &gate.ExitCode
		}
		entry.HasOutput = entry.CaptureOutput || gate.HasOutput()

		entries = append(entries, entry)
		if len(entries) >= limit {
			break
		}
	}

	out := &OrderHistoryListOutput{
		CacheAgeS: cacheAgeSeconds(store),
	}
	out.Body.Entries = entries
	return out, nil
}

// orderHistoryEntry is a single entry in the order history response.
type orderHistoryEntry struct {
	BeadID        string   `json:"bead_id"`
	StoreRef      string   `json:"store_ref"`
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
	storeInfos := workflowStores(s.state)
	if input.StoreRef != "" {
		info, ok := workflowStoreByRef(s.state, input.StoreRef)
		if !ok {
			return nil, apierr.ScopeNotFound.Msg("store_ref does not resolve to a known scope")
		}
		storeInfos = []workflowStoreInfo{info}
	}
	result, err := orderHistoryBeadAcrossStoreInfos(storeInfos, input.BeadID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, apierr.BeadNotFound.Msg("bead not found")
		}
		if errors.Is(err, errNoOrderStores) {
			return nil, apierr.ServiceUnavailable.Msg(err.Error())
		}
		return nil, apierr.Internal.Msg(err.Error())
	}
	b := result.bead

	output := convergence.GateOutputFromMetadata(b.Metadata).CombinedOutput()

	return &struct {
		Body orderHistoryDetailResponse
	}{Body: orderHistoryDetailResponse{
		BeadID:    b.ID,
		StoreRef:  result.storeRef,
		CreatedAt: b.CreatedAt.Format(time.RFC3339),
		Labels:    b.Labels,
		Output:    output,
	}}, nil
}

// orderHistoryDetailResponse is the response for GET /v0/order/history/{bead_id}.
type orderHistoryDetailResponse struct {
	BeadID    string   `json:"bead_id"`
	StoreRef  string   `json:"store_ref"`
	CreatedAt string   `json:"created_at"`
	Labels    []string `json:"labels"`
	Output    string   `json:"output"`
}

type orderHistoryStoreBead struct {
	storeRef string
	bead     beads.Bead
}

func orderStoreInfosForState(state State, a orders.Order) ([]workflowStoreInfo, error) {
	cityName := workflowCityScopeRef(state.CityName())
	infos := make([]workflowStoreInfo, 0, 2)
	if strings.TrimSpace(a.Rig) != "" {
		if rigStore := state.BeadStore(a.Rig); rigStore != nil {
			infos = append(infos, workflowStoreInfo{
				ref:       "rig:" + a.Rig,
				scopeKind: beadmeta.ScopeKindRig,
				scopeRef:  a.Rig,
				store:     rigStore,
			})
		}
	}

	if cityStore := state.CityBeadStore(); cityStore != nil {
		infos = append(infos, workflowStoreInfo{
			ref:       "city:" + cityName,
			scopeKind: beadmeta.ScopeKindCity,
			scopeRef:  cityName,
			store:     cityStore,
		})
	}

	if len(infos) == 0 {
		return nil, errNoOrderStores
	}
	return infos, nil
}

// orderFrontDoorsFromWorkflowInfos wraps the order read path's store infos as
// order front doors for the mixed orders+graph Cursor read. Each store is used
// as both the orders leg and the graph leg (single-store colocation dedups to one
// read); a graph-store split would supply a distinct graph leg here.
func orderFrontDoorsFromWorkflowInfos(infos []workflowStoreInfo) []*orders.Store {
	out := make([]*orders.Store, 0, len(infos))
	for _, info := range infos {
		if info.store != nil {
			out = append(out, orders.NewStoreWithGraph(
				beads.OrdersStore{Store: info.store},
				beads.GraphStore{Store: info.store},
			))
		}
	}
	return out
}

func orderHistoryBeadsAcrossStoreInfosForCheck(infos []workflowStoreInfo, scopedName string, limit int, beforeTime time.Time, fresh bool) ([]orderHistoryStoreBead, error) {
	if fresh {
		return orderHistoryBeadsAcrossStoreInfos(infos, scopedName, limit, beforeTime)
	}
	return orderHistoryBeadsAcrossStoreInfosCachedFirst(infos, scopedName, limit, beforeTime)
}

func orderHistoryBeadsAcrossStoreInfosCachedFirst(infos []workflowStoreInfo, scopedName string, limit int, beforeTime time.Time) ([]orderHistoryStoreBead, error) {
	if len(infos) == 0 {
		return nil, errNoOrderStores
	}

	label := "order-run:" + scopedName
	seen := make(map[string]bool)
	results := make([]orderHistoryStoreBead, 0)
	for i, info := range infos {
		if info.store == nil {
			continue
		}
		query := beads.ListQuery{
			Label:         label,
			CreatedBefore: beforeTime,
			Limit:         limit,
			IncludeClosed: true,
			Sort:          beads.SortCreatedDesc,
			TierMode:      beads.TierBoth,
		}
		var (
			rows []beads.Bead
			err  error
		)
		if cached, ok := info.store.(cachedListStore); ok {
			var cacheOK bool
			rows, cacheOK = cached.CachedList(query)
			if !cacheOK {
				rows, err = info.store.List(query)
			}
		} else {
			rows, err = info.store.List(query)
		}
		if err != nil {
			if i == 0 && len(rows) == 0 {
				return nil, err
			}
			log.Printf("api: order history list failed for %s: %v", info.ref, err)
			if len(rows) == 0 {
				continue
			}
		}
		for _, row := range rows {
			if !beforeTime.IsZero() && !row.CreatedAt.Before(beforeTime) {
				continue
			}
			key := info.ref + "\x00" + row.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			results = append(results, orderHistoryStoreBead{storeRef: info.ref, bead: row})
		}
	}

	sort.SliceStable(results, func(i, j int) bool {
		return results[i].bead.CreatedAt.After(results[j].bead.CreatedAt)
	})
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func orderHistoryBeadsAcrossStoreInfos(infos []workflowStoreInfo, scopedName string, limit int, beforeTime time.Time) ([]orderHistoryStoreBead, error) {
	if len(infos) == 0 {
		return nil, errNoOrderStores
	}

	label := "order-run:" + scopedName
	seen := make(map[string]bool)
	results := make([]orderHistoryStoreBead, 0)
	for i, info := range infos {
		if info.store == nil {
			continue
		}
		rows, err := info.store.List(beads.ListQuery{
			Label:         label,
			CreatedBefore: beforeTime,
			Limit:         limit,
			IncludeClosed: true,
			Sort:          beads.SortCreatedDesc,
			TierMode:      beads.TierBoth,
		})
		if err != nil {
			if i == 0 && len(rows) == 0 {
				return nil, err
			}
			log.Printf("api: order history list failed for %s: %v", info.ref, err)
			if len(rows) == 0 {
				continue
			}
		}
		for _, row := range rows {
			if !beforeTime.IsZero() && !row.CreatedAt.Before(beforeTime) {
				continue
			}
			key := info.ref + "\x00" + row.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			results = append(results, orderHistoryStoreBead{storeRef: info.ref, bead: row})
		}
	}

	sort.SliceStable(results, func(i, j int) bool {
		return results[i].bead.CreatedAt.After(results[j].bead.CreatedAt)
	})
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func orderHistoryBeadAcrossStoreInfos(infos []workflowStoreInfo, beadID string) (orderHistoryStoreBead, error) {
	if len(infos) == 0 {
		return orderHistoryStoreBead{}, errNoOrderStores
	}

	var lastErr error
	for _, info := range infos {
		if info.store == nil {
			continue
		}
		bead, err := info.store.Get(beadID)
		if err == nil {
			return orderHistoryStoreBead{storeRef: info.ref, bead: bead}, nil
		}
		if errors.Is(err, beads.ErrNotFound) {
			continue
		}
		lastErr = err
	}
	if lastErr != nil {
		return orderHistoryStoreBead{}, lastErr
	}
	return orderHistoryStoreBead{}, beads.ErrNotFound
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
		return nil, apierr.InvalidRequest.Msg(scopeErr)
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
		return nil, apierr.Internal.Msg("workflow feed failed")
	}
	orderRuns, err := buildOrderRunFeedItems(s.state, scopeKind, scopeRef)
	if err != nil {
		return nil, apierr.Internal.Msg("order feed failed")
	}

	items := make([]monitorFeedItemResponse, 0, len(workflowRuns.Items)+len(orderRuns.Items))
	for _, run := range workflowRuns.Items {
		items = append(items, workflowRunProjectionFeedItem(run))
	}
	items = append(items, orderRuns.Items...)

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
		Partial: workflowRuns.Partial || orderRuns.Partial,
	}
	body.PartialErrors = appendUniqueStrings(body.PartialErrors, workflowRuns.PartialErrors...)
	body.PartialErrors = appendUniqueStrings(body.PartialErrors, orderRuns.PartialErrors...)

	s.storeResponse(cacheKey, index, body)

	return &struct {
		Body ordersFeedBody
	}{Body: body}, nil
}

func appendUniqueStrings(dst []string, values ...string) []string {
	seen := make(map[string]bool, len(dst))
	for _, value := range dst {
		seen[value] = true
	}
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		dst = append(dst, value)
	}
	return dst
}

func (s *Server) setOrderEnabledHuma(name string, enabled bool) (*OKResponse, error) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}

	a, err := resolveOrder(s.state.OrdersAll(), name)
	if err != nil {
		if errors.Is(err, errOrderAmbiguous) {
			return nil, apierr.AmbiguousReference.Msg(err.Error())
		}
		return nil, apierr.OrderNotFound.Msg(err.Error())
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
