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
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// Frozen copies of every function T7d.2 converts, taken verbatim from
// origin/main 8d2fb3120c (huma_handlers_orders.go: buildOrderCheckBody,
// checkOrderTriggerForAPI, humaHandleOrderHistory, humaHandleOrderHistoryDetail,
// orderFrontDoorsFromWorkflowInfos and the four order history reads;
// orders_feed.go: buildOrderRunFeedItems, orderTrackingUpdatedAt;
// handler_convoy_dispatch.go: workflowStoreByRef; huma_handlers_runs.go:
// cancelRun, planRunCancel), for order_history_handlers_equivalence_test.go.
// The leg builders they call are T7b.2's frozen copies
// (order_history_legs_reference_test.go). Substitutions, and nothing else:
//
//   - every name takes the t7d2Frozen prefix, and a method becomes a function
//     of its *Server (after its context, if it takes one);
//   - workflowStoreInfo becomes t7dFrozenInfo (T7d.2 drops the store field);
//     workflowStoreByRef converts its live leg to it (t7dLive) before setting
//     the store;
//   - the leg builders become their t7b2Frozen copies.
//
// Everything else they call is live code T7d.2 leaves byte-identical: the
// pure bead and feed helpers (newestRunRecord, orders.CheckTrigger and the
// decoders, the feed formatters, inProgressAssignees), workflowLegs,
// workflowLegNamed, findWorkflowRoots, cancelWorkflowRoot, and the orders.Store
// methods, whose bodies T7b.2's table pins to main's.
//
// Read markers are kept: markers in test files are neither linted nor
// ledgered.

func t7d2FrozenBuildOrderCheckBody(s *Server, fresh bool) OrderCheckListBody {
	aa := s.state.Orders()
	ep := s.state.EventProvider()

	now := time.Now()
	checks := make([]orderCheckResponse, 0, len(aa))
	for _, a := range aa {
		storeInfos, err := t7b2FrozenOrderStoreInfosForState(s.state, a)
		if err != nil {
			storeInfos = nil
		}
		// The newest run record among a bounded window: a plain bead carrying
		// the order's labels is no run (orders.IsRunRecord, the #7766 review).
		history, _ := t7d2FrozenOrderHistoryBeadsAcrossStoreInfosForCheck(storeInfos, a.ScopedName(), orderCheckRunScan, time.Time{}, fresh)
		history = newestRunRecord(history)
		result := t7d2FrozenCheckOrderTriggerForAPI(a, now, history, storeInfos, ep, fresh)
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

	return OrderCheckListBody{Checks: checks}
}

func t7d2FrozenCheckOrderTriggerForAPI(a orders.Order, now time.Time, history []orderHistoryStoreBead, infos []t7dFrozenInfo, ep events.Provider, fresh bool) orders.TriggerResult {
	// history is the order's newest run record (newestRunRecord), or
	// nothing: as in the dispatcher's cooldown and cursor, a plain bead
	// carrying its labels moves neither (the #7766 review).
	lastRunFn := func(string) (time.Time, error) {
		if len(history) == 0 {
			return time.Time{}, nil
		}
		return history[0].bead.CreatedAt, nil
	}
	var cursorFn orders.CursorFunc
	if a.Trigger == "event" {
		var cursor uint64
		if fresh {
			cursor = orders.CursorAcross(t7d2FrozenOrderFrontDoorsFromWorkflowInfos(infos))(a.ScopedName())
		} else {
			labelSets := make([][]string, 0, len(history))
			for _, row := range history {
				labelSets = append(labelSets, row.bead.Labels)
			}
			cursor = orders.MaxSeqFromLabels(labelSets)
		}
		start, err := orders.CheckEventStart(ep, cursor)
		if err != nil {
			return orders.TriggerResult{Due: false, ReadFailed: true, Reason: "event: " + err.Error()}
		}
		cursorFn = func(string) uint64 { return start }
	}
	return orders.CheckTrigger(a, now, lastRunFn, ep, cursorFn)
}

func t7d2FrozenHumaHandleOrderHistory(_ context.Context, s *Server, input *OrderHistoryInput) (*OrderHistoryListOutput, error) {
	// The gate and the cache age are the city work store's, while the rows
	// come from the order's store infos (E1: preserved, a follow-up reports
	// the serving store's cache).
	work := s.state.Stores().Work()
	if err := cacheLiveOr503(work); err != nil {
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
	storeInfos, err := t7b2FrozenOrderStoreInfosForState(s.state, orderDef)
	if err != nil {
		if errors.Is(err, errNoOrderStores) {
			return nil, apierr.ServiceUnavailable.Msg(err.Error())
		}
		return nil, apierr.Internal.Msg(err.Error())
	}

	results, err := t7d2FrozenOrderHistoryBeadsAcrossStoreInfos(storeInfos, scopedName, limit, beforeTime)
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
			CreatedAt:     b.CreatedAt.Format(time.RFC3339Nano),
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
		// has_output reports whether this run recorded output for the history
		// detail to serve; capture_output describes the order, not the run.
		entry.HasOutput = gate.HasOutput()

		entries = append(entries, entry)
		if len(entries) >= limit {
			break
		}
	}

	freshness := cacheAgeSeconds(work)
	out := &OrderHistoryListOutput{
		CacheAgeS:    freshness.ageS,
		CacheRefresh: freshness.refresh,
	}
	out.Body.Entries = entries
	return out, nil
}

func t7d2FrozenHumaHandleOrderHistoryDetail(_ context.Context, s *Server, input *OrderHistoryDetailInput) (*struct {
	Body orderHistoryDetailResponse
}, error,
) {
	// The bead this reads is an order-tracking bead, which on a split city lives in
	// the orders binding. workflowStores leads with the GRAPH binding, so without
	// the orders leg this only finds one because the shape this build serves
	// happens to serve both classes from one store — the co-residency the feed
	// already refuses to rely on.
	storeInfos := t7b2FrozenAppendOrdersClassStoreInfo(t7b2FrozenWorkflowWritableStores(s.state), s.state, workflowCityScopeRef(s.state.CityName()))
	if input.StoreRef != "" {
		info, ok := t7d2FrozenWorkflowStoreByRef(s.state, input.StoreRef)
		if !ok {
			return nil, apierr.ScopeNotFound.Msg("store_ref does not resolve to a known scope")
		}
		storeInfos = []t7dFrozenInfo{info}
	}
	result, err := t7d2FrozenOrderHistoryBeadAcrossStoreInfos(storeInfos, input.BeadID)
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
		CreatedAt: b.CreatedAt.Format(time.RFC3339Nano),
		Labels:    b.Labels,
		Output:    output,
	}}, nil
}

func t7d2FrozenOrderFrontDoorsFromWorkflowInfos(infos []t7dFrozenInfo) []*orders.Store {
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

func t7d2FrozenOrderHistoryBeadsAcrossStoreInfosForCheck(infos []t7dFrozenInfo, scopedName string, limit int, beforeTime time.Time, fresh bool) ([]orderHistoryStoreBead, error) {
	if fresh {
		return t7d2FrozenOrderHistoryBeadsAcrossStoreInfos(infos, scopedName, limit, beforeTime)
	}
	return t7d2FrozenOrderHistoryBeadsAcrossStoreInfosCachedFirst(infos, scopedName, limit, beforeTime)
}

func t7d2FrozenOrderHistoryBeadsAcrossStoreInfosCachedFirst(infos []t7dFrozenInfo, scopedName string, limit int, beforeTime time.Time) ([]orderHistoryStoreBead, error) {
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
		// Relic leg (S4 debt) on a scope store: a city or rig ledger holds these
		// rows only as pre-split copies, so beads.RelicRead counts the read as
		// debt; on the class binding the same read is legitimate.
		// One leg of the order-history federation (rig and city scopes for
		// pre-cutover history, the orders binding): each leg holds only its
		// own rows.
		err = beads.RelicRead(func() error {
			var listErr error
			if cached, ok := beads.As[beads.CachedLister](info.store); ok {
				var cacheOK bool
				rows, cacheOK = cached.CachedList(query)
				if !cacheOK {
					rows, listErr = info.store.List(query)
				}
			} else {
				rows, listErr = info.store.List(query)
			}
			return listErr
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

func t7d2FrozenOrderHistoryBeadsAcrossStoreInfos(infos []t7dFrozenInfo, scopedName string, limit int, beforeTime time.Time) ([]orderHistoryStoreBead, error) {
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
		// Relic leg (S4 debt) on a scope store: a city or rig ledger holds these
		// rows only as pre-split copies, so beads.RelicRead counts the read as
		// debt; on the class binding the same read is legitimate.
		// One leg of the order-history federation; each leg holds only its
		// own rows.
		var rows []beads.Bead
		err := beads.RelicRead(func() error {
			var listErr error
			rows, listErr = info.store.List(beads.ListQuery{
				Label:         label,
				CreatedBefore: beforeTime,
				Limit:         limit,
				IncludeClosed: true,
				Sort:          beads.SortCreatedDesc,
				TierMode:      beads.TierBoth,
			})
			return listErr
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

func t7d2FrozenOrderHistoryBeadAcrossStoreInfos(infos []t7dFrozenInfo, beadID string) (orderHistoryStoreBead, error) {
	if len(infos) == 0 {
		return orderHistoryStoreBead{}, errNoOrderStores
	}

	var lastErr error
	for _, info := range infos {
		if info.store == nil {
			continue
		}
		// Relic leg (S4 debt) on a scope store: a city or rig ledger holds these
		// rows only as pre-split copies, so beads.RelicRead counts the read as
		// debt; on the class binding the same read is legitimate.
		// A residence probe over the order-history legs.
		bead, err := beads.RelicGet(info.store, beadID)
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

func t7d2FrozenBuildOrderRunFeedItems(state State, requestedScopeKind, requestedScopeRef string, limit int) (orderRunFeedResult, error) {
	// The feed lists order-tracking beads, which are orders class and live in
	// the orders binding on a split city. workflowStores leads with the GRAPH
	// binding (its own callers scan workflow roots), so on a city that relocates
	// only graph the tracking beads would still be missed; the orders leg is
	// appended here and deduplicated, so a city that serves both classes from
	// one binding — the shape this build supports — reads it once.
	stores := t7b2FrozenAppendOrdersClassStoreInfo(t7b2FrozenWorkflowWritableStores(state), state, workflowCityScopeRef(state.CityName()))
	allOrders := state.OrdersAll()
	orderByScopedName := make(map[string]orders.Order, len(allOrders))
	for _, order := range allOrders {
		orderByScopedName[order.ScopedName()] = order
	}

	cityScopeRef := workflowCityScopeRef(state.CityName())
	includeAllForCity := requestedScopeKind == beadmeta.ScopeKindCity && requestedScopeRef == cityScopeRef
	items := make([]monitorFeedItemResponse, 0)
	partialErrors := make([]string, 0)
	var requestedScopeErr error
	for _, info := range stores {
		if info.store == nil {
			continue
		}
		front := orders.NewStore(beads.OrdersStore{Store: info.store})
		// Relic leg (S4 debt) on a scope store: a city or rig ledger holds these
		// rows only as pre-split copies, so beads.RelicRead counts the read as
		// debt; on the class binding the same read is legitimate.
		// One leg of the order-feed federation (graph, orders binding, and
		// the city and rig scopes for pre-cutover history).
		var runs []orders.OrderRun
		err := beads.RelicRead(func() error {
			var listErr error
			runs, listErr = front.ListTracking(limit)
			return listErr
		})
		if err != nil {
			if requestedScopeErr == nil && info.scopeKind == requestedScopeKind && info.scopeRef == requestedScopeRef {
				requestedScopeErr = err
			}
			if includeAllForCity {
				msg := info.ref + " store unavailable"
				log.Printf("api: order feed list failed for %s: %v", info.ref, err)
				partialErrors = append(partialErrors, msg)
			}
			continue
		}

		// Scoped to this store's front: LatestOpenRun is a per-store lookup, so
		// a cache keyed only by run.Scoped must not be reused across stores.
		// Runs sharing a scoped name within this store's result (repeat runs of
		// the same order) cost one backing lookup instead of one per run.
		latestOpenCache := make(map[string]latestOpenRunLookup, len(runs))
		for _, run := range runs {
			scopeKind, scopeRef := orderTrackingScope(run.Scoped, cityScopeRef)
			if !includeAllForCity && (scopeKind != requestedScopeKind || scopeRef != requestedScopeRef) {
				continue
			}

			updatedAt := t7d2FrozenOrderTrackingUpdatedAt(front, run, latestOpenCache)
			orderDef, ok := orderByScopedName[run.Scoped]
			title := orderTrackingTitle(run.Scoped, orderDef, ok)
			target := orderTrackingTarget(orderDef, ok, run)
			itemType := orderTrackingType(orderDef, ok, run)
			item := monitorFeedItemResponse{
				ID:                 "order:" + info.ref + ":" + run.ID,
				Type:               itemType,
				Status:             normalizeMonitorStatus(run.State()),
				Title:              title,
				ScopeKind:          scopeKind,
				ScopeRef:           scopeRef,
				Target:             target,
				StartedAt:          run.CreatedAt.Format(time.RFC3339Nano),
				UpdatedAt:          updatedAt.Format(time.RFC3339Nano),
				BeadID:             run.ID,
				StoreRef:           info.ref,
				DetailAvailable:    ok && orderDef.IsExec(),
				RunDetailAvailable: ok && orderDef.IsExec(),
			}
			items = append(items, item)
		}
	}

	if requestedScopeErr != nil && !includeAllForCity {
		return orderRunFeedResult{}, requestedScopeErr
	}

	return orderRunFeedResult{
		Items:         items,
		Partial:       len(partialErrors) > 0,
		PartialErrors: partialErrors,
	}, nil
}

func t7d2FrozenOrderTrackingUpdatedAt(front *orders.Store, run orders.OrderRun, cache map[string]latestOpenRunLookup) time.Time {
	updatedAt := run.CreatedAt
	lookup, cached := cache[run.Scoped]
	if !cached {
		latest, found, err := front.LatestOpenRun(run.Scoped)
		lookup = latestOpenRunLookup{run: latest, found: found, err: err}
		cache[run.Scoped] = lookup
	}
	if lookup.err != nil && !lookup.found {
		orderFeedLogf("api: order feed update lookup failed for %s bead %s: %v", run.Scoped, run.ID, lookup.err)
		return updatedAt
	}
	if lookup.err != nil {
		orderFeedLogf("api: order feed update lookup partially failed for %s bead %s: %v", run.Scoped, run.ID, lookup.err)
	}
	if lookup.found && lookup.run.CreatedAt.After(updatedAt) {
		updatedAt = lookup.run.CreatedAt
	}
	return updatedAt
}

func t7d2FrozenWorkflowStoreByRef(state State, ref string) (t7dFrozenInfo, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return t7dFrozenInfo{}, false
	}

	kind, scopeRef, ok := strings.Cut(ref, ":")
	if !ok {
		return t7dFrozenInfo{}, false
	}
	scopeRef = strings.TrimSpace(scopeRef)
	if scopeRef == "" {
		return t7dFrozenInfo{}, false
	}

	switch kind = strings.TrimSpace(kind); kind {
	case beadmeta.ScopeKindCity, workflowGraphStoreRefPrefix, beadmeta.ScopeKindRig:
		// The graph ref round-trips the binding workflowStores names when
		// graph is relocated; on a default city it names nothing, and callers
		// fall back to the store scan. A rig resolves retired, suspended or
		// named like the city, as BeadStore(name) did.
		// One epoch for the request: the legs and the shared-store fallback
		// read the same rig set.
		st := state.Stores()
		infos, resident := workflowLegs(st, state.CityName(), workflowLegQuery{includeRetiredRigs: true, keepRigNamedLikeCity: true})
		live, ok := workflowLegNamed(st, infos, kind+":"+scopeRef)
		if !ok {
			return t7dFrozenInfo{}, false
		}
		info, _ := t7dLive(live, true)
		info.store = resident(live)
		return info, true
	case ordersClassStoreRefPrefix:
		// Round-trip the orders-class ref minted by appendOrdersClassStoreInfo: the
		// order history list hands it to a client as the store_ref for every
		// binding-resident tracking bead, and the detail endpoint takes it back. On
		// a default city (orders == city) there is no orders entry to name, so this
		// resolves nothing and callers fall back to the store scan.
		ordersStore := state.Stores().Orders().Store
		cityName := workflowCityScopeRef(state.CityName())
		if ordersStore == nil || ordersStore == state.CityBeadStore() || scopeRef != cityName {
			return t7dFrozenInfo{}, false
		}
		return t7dFrozenInfo{
			ref:       ordersClassStoreRefPrefix + ":" + cityName,
			scopeKind: beadmeta.ScopeKindCity,
			scopeRef:  cityName,
			store:     ordersStore,
		}, true
	}
	return t7dFrozenInfo{}, false
}

func t7d2FrozenCancelRun(s *Server, runID string, interrupt func(holders []string)) (cancelRunResult, error) {
	var res cancelRunResult
	var targets []runCancelTarget
	legs, resident := workflowLegs(s.state.Stores(), s.state.CityName(), workflowLegQuery{})
	for _, info := range legs {
		// One leg of the workflowStores federation (graph, city, rigs), read
		// in its plan role (a scope leg of a split city is a relic leg, S4
		// debt): the root probes miss on every leg but the one that owns the
		// run.
		roots, err := findWorkflowRoots(info.leg.Reader, runID)
		if err != nil {
			return res, err
		}
		for _, root := range roots {
			res.found = true
			if isClosedStatus(root.Status) {
				continue // already terminal — nothing to wind down
			}
			targets = append(targets, runCancelTarget{store: resident(info), root: root})
		}
	}
	if len(targets) == 0 {
		res.terminal = res.found
		return res, nil
	}

	for _, target := range targets {
		if strings.TrimSpace(target.root.Metadata[beadmeta.CancelRequestedMetadataKey]) != "" {
			continue // marked by an earlier attempt
		}
		if err := target.store.SetMetadata(target.root.ID, beadmeta.CancelRequestedMetadataKey, "true"); err != nil {
			return res, err
		}
	}

	var holders []string
	for i := range targets {
		held, err := t7d2FrozenPlanRunCancel(&targets[i])
		if err != nil {
			return res, err
		}
		holders = append(holders, held...)
	}
	if len(holders) > 0 && interrupt != nil {
		interrupt(holders)
	}

	for _, target := range targets {
		for _, rootID := range target.workflows {
			// target.store may hold a graph or a v1 work molecule: a relabel until the T7 Molecule intent (typed-store T5a).
			n, err := cancelWorkflowRoot(beads.GraphMolecule(beads.GraphStore{Store: target.store}), rootID)
			res.closed += n
			if err != nil {
				return res, err
			}
		}
		res.status = RunStatusCanceled
	}
	return res, nil
}

func t7d2FrozenPlanRunCancel(target *runCancelTarget) ([]string, error) {
	// target.store may hold a graph or a v1 work molecule: a relabel until the T7 Molecule intent (typed-store T5a).
	tree, err := sourceworkflow.WorkflowTree(beads.GraphMolecule(beads.GraphStore{Store: target.store}), target.root.ID)
	if err != nil {
		return nil, err
	}
	var holders []string
	for _, node := range tree {
		if node.Root.ID == "" || isClosedStatus(node.Root.Status) {
			continue
		}
		exclude := molecule.TeardownTailExclusionFromMembers(node.Members)
		holders = append(holders, inProgressAssignees(node.Members, exclude)...)
		target.workflows = append(target.workflows, node.Root.ID)
	}
	return holders, nil
}
