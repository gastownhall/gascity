package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/readywork"
)

// The cross-store gate on pool demand (#8360): on a split city every ready
// snapshot demand counts is passed through the rule `gc ready` applies
// (readywork.LegGate), so the controller and a seat agree row for row. Judging
// a row reads the stores that may hold its blockers, so it is remote I/O.
// Legacy judges inside its demand pass under crossStoreGateDeadline
// (readyDemandCache.gateSnapshot). The v2 pass does no remote I/O (CONTRACT v5
// R4): the external-reads lane judges each leg's snapshot off the pass and
// records it gated (allocator_backstop_lane.go), and the pass serves only a
// gated recording for a gate leg (v2DemandReads.ReadyAll).

// crossStoreGateDeadline bounds how long one judgment of a store's ready
// snapshot may take. The gate's reads take no context, so a read still in
// flight when it passes runs on and its answer is dropped; the snapshot is
// then kept ungated and reported partial, which retains and blocks creates
// rather than under-counting demand (see readyDemandCache).
const crossStoreGateDeadline = 10 * time.Second

// crossStoreGateLegs returns the ready legs a split city's demand is gated
// over, or nil for a city that relocates nothing (or whose topology cannot be
// planned, which its readers report themselves).
//
// The legs are planned over the city's registered WORK store
// (censusWorkLeg), not the store handed in: every caller hands the sessions
// store, which on a split city is the binding itself, and a plan whose work leg
// is the binding has no binding leg left, so the gate never armed (#8360 review,
// Finding 2). The census plans the legs demand reads over the same store, so
// the snapshots demand gates are the legs' own stores.
func crossStoreGateLegs(cityPath string, cfg *config.City, cityStore beads.Store, rigStores map[string]beads.Store, suspendedRigPaths map[string]bool) []readywork.Leg {
	if cityStore == nil {
		return nil
	}
	legs, err := readyLegsForTopology(residencyTopologyForCity(cityPath, cfg, censusWorkLeg(cityPath, cityStore), servingRigStores(cfg, rigStores, suspendedRigPaths))) // residency:allow — the same planned legs `gc ready` federates, so demand gates exactly as serve does
	if err != nil || !readywork.IsSplit(legs) {
		return nil
	}
	return legs
}

// crossStoreGateLeg returns the index of the gate leg that is store, compared
// behind the bead-policy front door (demandLabelKey), or -1.
func crossStoreGateLeg(legs []readywork.Leg, store beads.Store) int {
	if store == nil {
		return -1
	}
	key := demandLabelKey(store)
	for i, leg := range legs {
		if leg.Store != nil && demandLabelKey(leg.Store) == key {
			return i
		}
	}
	return -1
}

// errCrossStoreGateDeadline is a judgment that did not finish within
// crossStoreGateDeadline.
var errCrossStoreGateDeadline = errors.New("cross-store gate: judging the snapshot exceeded its deadline")

// gateReadyRows applies the cross-store rule to rows, the ready snapshot
// legs[own] served (readErr is that read's error), waiting at most deadline.
// It returns the rows to count and the gate's own failure, which the caller
// joins with readErr. A snapshot whose read failed outright is returned as
// read. A judgment that fails, panics or runs out of time keeps the ungated
// rows and fails as a partial result: demand must never be under-counted from
// a read hiccup (see readyDemandCache).
func gateReadyRows(gate *readywork.LegGate, own int, rows []beads.Bead, readErr error, deadline time.Duration) ([]beads.Bead, error) {
	if gate == nil || own < 0 || (readErr != nil && !beads.IsPartialResult(readErr)) {
		return rows, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	type judged struct {
		rows []beads.Bead
		err  error
	}
	done := make(chan judged, 1)
	go func() {
		gated, err := guardedRead(func() ([]beads.Bead, error) { return gate.Gate(ctx, own, rows) })
		done <- judged{gated, err}
	}()
	var gateErr error
	select {
	case j := <-done:
		if j.err == nil {
			return j.rows, nil
		}
		gateErr = j.err
	case <-ctx.Done():
		gateErr = fmt.Errorf("%w (%s)", errCrossStoreGateDeadline, deadline)
	}
	return rows, &beads.PartialResultError{Op: "controller ready demand cross-store gate", Err: gateErr}
}

// joinGateErr adds a gate failure to a snapshot read's own error, keeping the
// read's error as it is when the gate did not fail.
func joinGateErr(readErr, gateErr error) error {
	if gateErr == nil {
		return readErr
	}
	return errors.Join(readErr, gateErr)
}
