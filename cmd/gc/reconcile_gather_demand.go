package main

import (
	"errors"
	"fmt"
	"io"

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

	v.AssignedWork, v.AssignedStores, v.AssignedStoreRefs, v.ReadyAssigned, v.StorePartial = collectAssignedWorkBeadsWithStores(
		env.CityPath, env.Cfg, env.CityStore, env.RigStores, env.SuspendedRigPaths, env.Sessions, cache)
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
