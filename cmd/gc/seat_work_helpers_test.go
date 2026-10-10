package main

import (
	"context"
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

// testWorkLegs is the WorkLegs a test city's controller would mint: over the
// city work store it registered (registerResidencyRoutes), else over store,
// which is the work store of a single-store test city.
func testWorkLegs(cityPath string, cfg *config.City, store beads.Store, rigs map[string]beads.Store) WorkLegs {
	work := store
	if registered := registeredCityWorkStore(cityPath); registered != nil {
		work = registered
	}
	return workLegsFromCensus(cityPath, cfg, cityWorkStore{store: work}, rigs)
}

// workLegsPlan is the plan and error of the WorkLegs minted over work.
func workLegsPlan(cityPath string, cfg *config.City, work beads.Store, rigs map[string]beads.Store) (storeref.ResolvedPlan, error) {
	l := workLegsFromCensus(cityPath, cfg, cityWorkStore{store: work}, rigs)
	return l.plan, l.unusable()
}

// testSeatWork is a tick's SeatWork over testWorkLegs.
func testSeatWork(cityPath string, cfg *config.City, store beads.Store, rigs map[string]beads.Store) *SeatWork {
	return newSeatWork(testWorkLegs(cityPath, cfg, store, rigs))
}

// The reconciler and sync entry points with the signatures the tests were
// written against: each mints the pass's SeatWork from its store.

// reconcileSessionBeadsAtPath runs the reconciler for a specific city
// path. rigStores supplies the attached rig bead stores so live
// cross-store ownership checks (sessionHasOpenAssignedWork) can see
// work that lives outside the primary store. Pass nil when no rig
// stores are attached; the reconciler will fall back to primary-store-
// only queries.
//
//nolint:unparam // compatibility wrapper keeps the established test/helper signature.
func reconcileSessionBeadsAtPath(
	ctx context.Context,
	cityPath string,
	sessions []beads.Bead,
	desiredState map[string]TemplateParams,
	configuredNames map[string]bool,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	dops drainOps,
	assignedWorkBeads []beads.Bead,
	rigStores map[string]beads.Store,
	readyWaitSet map[string]bool,
	dt *drainTracker,
	poolDesired map[string]int,
	storeQueryPartial bool,
	workSet map[string]bool,
	cityName string,
	it idleTracker,
	clk clock.Clock,
	rec events.Recorder,
	startupTimeout time.Duration,
	driftDrainTimeout time.Duration,
	stdout, stderr io.Writer,
	startOptions ...startExecutionOption,
) int {
	// Compat wrapper (tests): build the row feed + carrier snapshot from raw beads.
	snap := newSessionBeadSnapshotFromReconcileRows(sessionpkg.ReconcileRowsFromBeads(sessions))
	return reconcileSessionBeadsAtPathWithNamedDemand(
		ctx, cityPath, snap.OpenForReconcile(), snap, desiredState, configuredNames, cfg, sp, store, testSeatWork(cityPath, cfg, store, rigStores), dops, assignedWorkBeads, rigStores, readyWaitSet, dt, nil,
		poolDesired, nil, nil, storeQueryPartial, workSet, cityName, it, clk, rec, startupTimeout, driftDrainTimeout, stdout, stderr,
		startOptions...,
	)
}

//nolint:unparam // compatibility wrapper keeps the established traced test/helper signature.
func reconcileSessionBeadsTraced(
	ctx context.Context,
	cityPath string,
	sessions []beads.Bead,
	desiredState map[string]TemplateParams,
	configuredNames map[string]bool,
	cfg *config.City,
	sp runtime.Provider,
	store beads.Store,
	dops drainOps,
	assignedWorkBeads []beads.Bead,
	rigStores map[string]beads.Store,
	readyWaitSet map[string]bool,
	dt *drainTracker,
	poolDesired map[string]int,
	storeQueryPartial bool,
	workSet map[string]bool,
	cityName string,
	it idleTracker,
	clk clock.Clock,
	rec events.Recorder,
	startupTimeout time.Duration,
	driftDrainTimeout time.Duration,
	stdout, stderr io.Writer,
	trace *sessionReconcilerTraceCycle,
	startOptions ...startExecutionOption,
) int {
	// Compat wrapper: build the tick's row feed + carrier snapshot from the raw
	// session beads the test/helper caller supplies (production callers pass
	// sessionBeads.OpenForReconcile() directly). The snapshot constructor drops closed
	// beads, exactly as the production store-load feed does.
	snap := newSessionBeadSnapshotFromReconcileRows(sessionpkg.ReconcileRowsFromBeads(sessions))
	return reconcileSessionBeadsTracedWithNamedDemand(
		ctx, cityPath, snap.OpenForReconcile(), snap, desiredState, configuredNames, cfg, sp, beads.SessionStore{Store: store}, testSeatWork(cityPath, cfg, store, rigStores), dops, assignedWorkBeads, rigStores, readyWaitSet, dt, nil,
		poolDesired, nil, nil, storeQueryPartial, workSet, cityName, it, clk, rec, startupTimeout, driftDrainTimeout, stdout, stderr, trace,
		startOptions...,
	)
}

//nolint:unparam // cityPath and skipClose are passed through to syncSessionBeadsWithSnapshot
func syncSessionBeads(
	cityPath string,
	store beads.Store,
	desiredState map[string]TemplateParams,
	sp runtime.Provider,
	configuredNames map[string]bool,
	cfg *config.City,
	clk clock.Clock,
	stderr io.Writer,
	skipClose bool,
) map[string]string {
	openIndex, _ := syncSessionBeadsWithSnapshotAndRigStores(
		cityPath, beads.SessionStore{Store: store}, testSeatWork(cityPath, cfg, store, nil), desiredState, sp, configuredNames, cfg, clk, stderr, skipClose, nil, nil,
	)
	return openIndex
}

func syncSessionBeadsWithSnapshot(
	store beads.Store,
	desiredState map[string]TemplateParams,
	sp runtime.Provider,
	configuredNames map[string]bool,
	cfg *config.City,
	clk clock.Clock,
	stderr io.Writer,
	sessionBeads *sessionBeadSnapshot,
) (map[string]string, *sessionBeadSnapshot) {
	return syncSessionBeadsWithSnapshotAndRigStores(
		"", beads.SessionStore{Store: store}, testSeatWork("", cfg, store, nil), desiredState, sp, configuredNames, cfg, clk, stderr, false, sessionBeads, nil,
	)
}
