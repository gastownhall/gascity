package main

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/orderdispatch"
	"github.com/gastownhall/gascity/internal/orders"
)

// memoryOrderDispatcher is the controller's live order dispatcher and the
// concrete implementation of the reusable orderdispatch.Dispatcher seam. The
// webhook receiver (E3/E6) fires orders through this same instance, so a webhook
// dispatch and a tick dispatch run the identical dispatchOne core.
var _ orderdispatch.Dispatcher = (*memoryOrderDispatcher)(nil)

// Dispatch fires a pre-resolved order through the shared launchResolvedDispatch
// → dispatchOne core — the same path the controller tick loop uses.
//
// The caller (the webhook order sink) has already enforced its policy:
// target=="order", the order opts in with trigger=="webhook", the {order,rig}
// is within the webhook's provenance scope, and untrusted payload args have been
// namespaced into req.ExecEnv (R4). Dispatch therefore only re-checks required
// params (defense in depth), resolves the order's store, and launches the async
// dispatch. It returns as soon as the tracking bead is written and the goroutine
// is launched; the wisp/exec runs asynchronously and its outcome lands on the
// events feed (OrderFired/Completed/Failed), matching the design's fast-ACK
// response contract (verify+match+enqueue synchronously, work runs async).
func (m *memoryOrderDispatcher) Dispatch(ctx context.Context, req orderdispatch.DispatchRequest) (orderdispatch.DispatchResult, error) {
	a := req.Order
	scoped := a.ScopedName()

	// Defense in depth: the sink validated already, but the seam is the last
	// gate before a bead is written, so re-check against the raw param vars.
	if err := orders.ValidateRequiredParams(a, req.Vars); err != nil {
		return orderdispatch.DispatchResult{ScopedName: scoped, Rejected: true, Reason: err.Error()}, nil
	}

	target, err := resolveOrderStoreTarget(m.cityPath, m.cfg, a)
	if err != nil {
		return orderdispatch.DispatchResult{ScopedName: scoped}, fmt.Errorf("resolving store target for %s: %w", scoped, err)
	}
	store, err := m.storeFn(target)
	if err != nil {
		return orderdispatch.DispatchResult{ScopedName: scoped}, fmt.Errorf("opening store for %s: %w", scoped, err)
	}

	// Close this dispatch's own store handle once the async dispatchOne goroutine
	// has finished with it. Mirrors the tick loop's detached per-tick closer: the
	// handle must stay open until the goroutine's final store call (the
	// tracking-bead close), so the close is deferred to onDone — launchDispatchOne
	// invokes it only after dispatchOne returns (gascity#3157). The two exit paths
	// are mutually exclusive (a create failure never launches), so the closer runs
	// exactly once.
	closeStore := func() {
		if cerr := closeBeadStoreHandle(store); cerr != nil {
			logDispatchError(m.stderr, "gc: webhook dispatch: closing store for %s: %v", scoped, cerr)
		}
	}

	trackingRun, err := m.launchResolvedDispatch(ctx, store, target, a, m.cityPath, req.Vars, req.ExecEnv, closeStore)
	if err != nil {
		closeStore() // nothing launched; release the handle we opened
		return orderdispatch.DispatchResult{ScopedName: scoped}, fmt.Errorf("creating tracking bead for %s: %w", scoped, err)
	}
	return orderdispatch.DispatchResult{ScopedName: scoped, TrackingID: trackingRun.ID, Fired: true}, nil
}
