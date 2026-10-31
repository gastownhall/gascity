package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The v2 pass's demand gather (P3 spec §4.3): legacy's demand collectors,
// called with legacy's arguments, read through one pass's demandReads and
// assembled into a demandView. It reads and never writes. Legacy's demand
// pass interleaves its repairs with these reads (the work-dir repair, the
// stamp, the two canonicalizations, the slot collapse and the
// control-dispatcher route repair); under v2 they are the external-reads
// lane's demand-repairs step, and the decide projects the routes they leave
// (projectControlDispatcherRoutes). With no write between the reads, one
// readyDemandCache serves the whole gather, where legacy needs a second one
// after its writes.
//
// Two of the demand pass's inputs are not gathered here. The custom
// scale_check counts are the lane's (I5): gather names the pools that have
// one, and the decide reads their counts from allocInputs.ScaleCheck. The
// continuation-claim candidates (selectReadyContinuationClaimCandidates) Get
// each candidate's root from its work store, so they belong to the lane step
// that nudges continuations, not to the pass.

var errDemandGatherNoStore = errors.New("demand gather: no city config or store")

// demandGatherEnv is what one demand gather reads, besides its demandReads.
type demandGatherEnv struct {
	CityName          string
	CityPath          string
	Cfg               *config.City
	CityStore         beads.Store
	RigStores         map[string]beads.Store
	SuspendedRigPaths map[string]bool
	// OpenSessions is the cross-leg open session census
	// (collectAllOpenSessionInfos): it decides which pools are cold.
	OpenSessions []session.Info
	// Sessions is the open session snapshot the assigned-work collector reads
	// session identities from; nil reads none, as in legacy.
	Sessions *sessionBeadSnapshot
	// Stderr takes the collectors' diagnostics; nil discards them.
	Stderr io.Writer
}

// gatherDemand runs legacy's demand collectors over env through reads (nil
// is legacyDemandReads). A failed or refused read never fails the gather:
// the collectors mark what it covered partial, and the view carries the
// partial flags so the decide retains instead of shrinking (P-3). A panic in
// a collector's fan-out leg is that leg's partial (recoverLeg); one on the
// gather's own goroutine (the default and named probes read inline) fails
// the gather. The error is for that, and for an env with no config or city
// store, which has no demand to read; with an error the view is empty.
func gatherDemand(env demandGatherEnv, reads demandReads) (v demandView, err error) {
	if env.Cfg == nil || env.CityStore == nil {
		return demandView{}, errDemandGatherNoStore
	}
	stderr := env.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	defer func() {
		if err != nil {
			v = demandView{}
		}
	}()
	defer recoverLeg(&err, "demand gather", stderr)
	targets := buildDemandTargets(env.CityName, env.CityPath, env.Cfg, env.CityStore, env.RigStores, env.SuspendedRigPaths,
		env.OpenSessions, noProbeEnv, stderr)
	cache := newReadyDemandCacheWithReads(reads)
	gateLegs := crossStoreGateLegs(env.CityPath, env.Cfg, env.CityStore, env.RigStores, env.SuspendedRigPaths)
	if v2, ok := reads.(*v2DemandReads); ok {
		// The v2 pass does no remote I/O (CONTRACT v5 R4), and judging a
		// row's cross-store blockers reads other stores: the external-reads
		// lane gates a split city's snapshots, and the pass serves them.
		v2.gateLegs = gateLegs
	} else {
		cache = cache.withGateLegs(gateLegs)
	}

	v.AssignedWork, v.AssignedStores, v.AssignedStoreRefs, v.ReadyAssigned, v.StorePartial = collectAssignedWorkBeadsWithStores(
		env.CityPath, env.Cfg, env.CityStore, env.RigStores, env.SuspendedRigPaths, env.Sessions, cache)
	v.HeldWork, v.HeldWorkPartial = collectHeldWork(env, cache.demandReads())
	c := &v.Collected
	c.UnassignedRouted, _, c.UnassignedRoutedRefs, c.UnassignedRoutedPartial = collectOpenUnassignedRoutedWork(
		env.CityPath, env.Cfg, env.CityStore, env.RigStores, env.SuspendedRigPaths, stderr, nil, reads)
	c.ColdWakeTemplates, c.NamedOnDemandTemplates = targets.coldWakeTemplates, targets.namedOnDemandTemplates
	for _, pool := range targets.pendingPools {
		v.CustomCheckTemplates = append(v.CustomCheckTemplates, env.Cfg.Agents[pool.agentIdx].QualifiedName())
	}
	if len(targets.defaultScaleTargets) > 0 {
		var errs []error
		c.DefaultProbed = true
		c.DefaultCounts, c.DefaultDemand, c.DefaultPartials, errs = defaultScaleCheckCountsAndDemand(env.Cfg, targets.defaultScaleTargets, cache)
		// A bead whose claims keep failing the same way is not spawn demand
		// (claim_loop_guard.go).
		c.DefaultCounts, c.DefaultDemand = claimLoopGuardFor(env.CityPath).withhold(c.DefaultCounts, c.DefaultDemand)
		for _, probeErr := range errs {
			fmt.Fprintf(stderr, "gatherDemand: %v (counts above may be a partial of one demand source)\n", probeErr) //nolint:errcheck
		}
	}
	if len(targets.defaultNamedScaleTargets) > 0 {
		var errs []error
		v.NamedDefault, c.NamedPartials, errs = defaultNamedSessionDemand(targets.defaultNamedScaleTargets, env.Cfg, env.CityName, cache)
		for _, probeErr := range errs {
			fmt.Fprintf(stderr, "gatherDemand: %v (using named demand=false)\n", probeErr) //nolint:errcheck
		}
	}
	v.ProbeStores = demandProbeStores(targets)
	v.RelocatedClaimRefs = assignedWorkRelocatedClaimRefs(env.CityPath, env.Cfg, env.CityStore)
	v.WakeClaimRefs = assignedWorkClaimRefs(env.CityPath, env.Cfg, env.CityStore)
	return v, nil
}

// noProbeEnv builds no custom scale_check probe env. Gather reads only which
// pools have a custom check; the lane builds their env and runs them
// (customScaleCheckWork), and marks a pool whose env fails partial. With
// legacy's env builder a failed build would drop the pool from the list, and
// the decide would read no count for it instead of a partial one.
func noProbeEnv(string, *config.City, *config.Agent) (map[string]string, error) {
	return nil, nil
}

// collectHeldWork is legacy's held-work read (sessionHoldsAssignedWork over
// the tick's SeatWork) as a pass fact, so that A20's keep and A19's cancel
// count the work legacy's keep counts. The demand read (AssignedWork) carries
// an open bead only when it is routed, ready, or a molecule root. This read
// carries every open or in-progress bead assigned to an identity on the
// assigned-work collector's legs that heldWorkRow counts.
//
// It reads the collector's two lists, which a v2 pass's reads serve again
// from their memo and recording, with no I/O of their own. Open is the
// backing's raw status (RawOpen, the live read SeatWork's open list is), so
// dependency-blocked work is in and status-parked work out. In progress is the
// collector's cached list (Cached), where SeatWork's is live: a claim the
// leg's cache has not absorbed yet is missing here. The signal's live read of
// the seat's held work (intent.HeldWork) is the backstop: it refuses the
// signal, and A19 cancels the request once a pass reads the work.
//
// A leg that fails or panics makes the read partial, which keeps every
// eligible seat (fail closed), as legacy's keep does: a seat's work may live
// on any leg, so every seat reads every leg (workLegsFromCensus), and a dark
// leg leaves every seat's held work unknown. The collector's read of that leg
// failed too, so the demand read is partial (StorePartial) as well, and the
// decide retains every row on it.
func collectHeldWork(env demandGatherEnv, reads demandReads) (rows []beads.Bead, partial bool) {
	stores, err := censusStoreCandidates(env.CityPath, env.Cfg, env.CityStore, env.RigStores, env.SuspendedRigPaths, censusRefBare)
	if err != nil {
		return nil, true
	}
	keep := func(list []beads.Bead, err error) {
		partial = partial || err != nil
		for _, b := range list {
			if strings.TrimSpace(b.Assignee) != "" && heldWorkRow(b) {
				rows = append(rows, b)
			}
		}
	}
	for _, source := range stores {
		keep(guardedRead(func() ([]beads.Bead, error) { return reads.RawOpen(source.store) }))
		keep(guardedRead(func() ([]beads.Bead, error) {
			return reads.Cached(source.store, beads.ListQuery{Status: "in_progress"})
		}))
	}
	return rows, partial
}
