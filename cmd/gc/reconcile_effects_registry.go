package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The effects registry: the effect each admitted intent kind runs on the
// executor. Each later effect PR adds its line. An intent whose kind has none
// settles refused with cause no-effect (submitIntent), so an arm may land
// before its effect.

// effectPass is what one pass hands every effect it submits: a fenced
// writer per census leg (session rows live in work stores too, #5187; v5 R3),
// the composite provider, and the inputs the pass decided from, which a row
// write re-decides against (R2), stripped of their raw store and provider
// handles. The pass never mutates them after submit.
type effectPass struct {
	Writers map[string]fencedWriter // by census leg
	// Runtime is the composite provider. The transaction's runtime read
	// (readRuntime) reads presence and identity on its routed leaf and
	// confirms absence through its fall-through hops. An effect calls no
	// provider verb on it: destructive verbs go through the fence and
	// SessionObjectKiller, and only a Call starts a runtime (the effect lint).
	Runtime runtime.Provider
	World   *World
	Alloc   *allocDecision
	Clock   plannerClock // stamps each section's since: the planner's
	// held are the capabilities the pass holds beyond its writers; an
	// effect reaches each only through txCaps, as its spec grants (capsFor).
	held heldCaps
	// reads are what the live work read (L5) reads.
	reads effectReads
	seam  txSeamFunc // the planner's: runs at the effect's seams (tests, staging)
}

// effectReads are the city work legs, every leg read-only.
type effectReads struct {
	legs WorkLegs
	// city and rigs are the sessions store and the rig stores, read-only,
	// which the start effect's prepare reads (template overrides, the task
	// work dir, the claimed-work probe); the live work read uses legs, and
	// the orphaned signal's live-claim veto reads the row from city.
	city beads.Store
	rigs map[string]beads.Store
	// cityPath keys the city's identifier locks (A7's alias lock).
	cityPath string
}

// heldCaps are a pass's capabilities. create is what the pass hands its
// creates, raw stores included: a create's guarded row write is v5 R1's
// exception 1 (§13). creates runs them. release is the closes' raw work
// legs, for their releases and cascades. The planner sets them, create on
// its first admitted create and release on its first close (planner.submit).
type heldCaps struct {
	create  *createPass
	creates *createEffects
	start   startCaps           // the breaker and its recorder; capsFor adds the provider and clock
	episode startupHealthRecord // over the sessions store
	retire  retiredCleanup      // over the sessions store
	release *closeReleases
}

// newEffectPass is w's and a's effectPass.
func newEffectPass(w *World, a *allocDecision) *effectPass {
	p := &effectPass{Writers: make(map[string]fencedWriter, len(w.LegStores)), Alloc: a, Clock: realPlannerClock{}}
	if w.Env != nil {
		p.Runtime = w.Env.SP
	}
	for leg, store := range w.LegStores {
		p.Writers[leg] = fencedWriter{store: store}
	}
	if w.SessionsStore != nil {
		p.reads.city = readOnlyStore{blindWriteRefusingStore{inner: w.SessionsStore}}
		p.held.episode = startupHealthRecord{store: w.SessionsStore}
		p.held.retire = retiredCleanup{store: w.SessionsStore}
	}
	p.reads.rigs = make(map[string]beads.Store, len(w.RigStores))
	for rig, store := range w.RigStores {
		p.reads.rigs[rig] = readOnlyStore{blindWriteRefusingStore{inner: store}}
	}
	p.reads.legs, p.reads.cityPath = w.WorkLegs.readOnly(), w.CityPath
	stripped := *w
	stripped.LegStores, stripped.Demand.AssignedStores = nil, nil
	stripped.SessionsStore, stripped.RigStores, stripped.WorkLegs = nil, nil, WorkLegs{}
	if w.Env != nil {
		env := *w.Env
		env.Relaunch, env.SP = env.relaunches(), nil
		stripped.Env = &env
	}
	p.World = &stripped
	return p
}

// effectSpecs is the kind table. Its functions are effects, run by the
// executor, never by the decide that reads the table for a kind's class
// (v2purity reaches a function stored in a field only through a read of
// that field):
var effectSpecs = map[string]effectSpec{
	intentStart:           {class: capStarts, tokens: 1, needs: needs{Runtime: true, Lease: true}, caps: capReadStores | capProviderStart | capEpisode | capRetire, sections: startSections, around: retireAround},
	intentAdopt:           {class: capProbing, needs: needs{Runtime: true, Lease: true}, caps: capReadStores, sections: adoptSections},
	intentCreate:          {class: capCreates, caps: capCreate, body: createBody},                                                                                                                          // C1, C2
	intentDriftRepair:     {class: capProbing, needs: needs{Runtime: true}, sections: healSections},                                                                                                        // A14 (C7b2): the fresh heal's, under its own kind
	intentDriftRestart:    {class: capProbing, bootGated: true, needs: driftRestartNeeds, caps: capReadStores | capProviderStop, sections: driftRestartSections},                                           // A14 (C7b2)
	intentDriftRelaunch:   {class: capProbing, bootGated: true, needs: driftRestartNeeds, needsFor: driftNeeds, caps: capReadStores | capProviderStart | capProviderStop, sections: driftRelaunchSections}, // A14 (C7b2)
	intentLiveApply:       {class: capProbing, needs: needs{Runtime: true, Lease: true}, sections: liveApplySections},                                                                                      // A14 (C7b2)
	intentRekey:           {class: capProbing, needs: needs{Runtime: true, Lease: true}, sections: rekeySections},                                                                                          // A3: writes the incarnation under the row's lease record
	intentZombie:          {class: capProbing, bootGated: true, needs: needs{Runtime: true, Lease: true}, sections: zombieSections},                                                                        // A12 (C5c2)
	intentDrainBegin:      {class: capRowWrites, bootGated: true, needsFor: drainBeginNeeds, sections: drainBeginSections},                                                                                 // A20 (C6a2)
	intentDrainBeginFresh: {class: capProbing, bootGated: true, needsFor: drainBeginNeeds, sections: drainBeginSections},                                                                                   // A20 (C6a2)
	intentSignal:          {class: capRowWrites, bootGated: true, needs: needs{NameLock: true}, needsFor: signalNeeds, sections: signalSections},                                                           // A20's signal (C6b1); L5 for orphaned (v5.10a B)
	intentSignalFresh:     {class: capProbing, bootGated: true, needs: signalFreshNeeds, needsFor: signalNeeds, caps: capReadStores, sections: signalSections},                                             // A20's signal (C6b1); L5 for no-wake-reason (v5.10a B)
	intentSignalAck:       {class: capProbing, bootGated: true, needs: needs{Runtime: true}, sections: signalSections},                                                                                     // A11 (C6c2)
	intentDrainCancel:     {class: capRowWrites, sections: drainClearSections},                                                                                                                             // A19 (C6a)
	intentDrainVoid:       {class: capRowWrites, sections: drainClearSections},                                                                                                                             // A19 (C6a)
	intentStop:            {class: capProbing, bootGated: true, needs: needs{Runtime: true, Lease: true}, needsFor: stopNeeds, caps: capReadStores | capProviderStop | capRelease, sections: stopSections}, // A4 (C6b2)
	intentClose:           {class: capProbing, bootGated: true, needs: needs{Runtime: true, Legs: legWork, Lease: true}, caps: capReadStores | capRelease, sections: closeSections, around: closeAround},   // A21 (C5c1)
	intentRollback:        {class: capProbing, bootGated: true, needs: needs{Runtime: true, Lease: true}, caps: capProviderStart | capRetire, sections: rollbackSections, around: retireAround},            // A10 (C3)
	intentRowMetadata:     {class: capRowWrites, caps: capReadStores, around: aliasLockAround, sections: rowMetadataSections},                                                                              // A7 (C7d): a row write, lease-free
	intentRuntimeAlias:    {class: capProbing, needs: needs{Runtime: true, Lease: true}, caps: capProviderStart, sections: runtimeAliasSections},                                                           // A7 (C7d)
	intentMarkerClear:     {class: capProbing, needs: needs{Runtime: true, Lease: true}, caps: capProviderStart, sections: markerClearSections},                                                            // A13's pinned skip (mc-nm6yt)
	intentBaseline:        {class: capRowWrites, sections: baselineSections},                                                                                                                               // A8 (C7c)
	intentRowHeal:         {class: capRowWrites, sections: rowWriteSections},                                                                                                                               // A6
	intentRowHealFresh:    {class: capProbing, needs: needs{Runtime: true}, sections: healSections},                                                                                                        // A6
}
