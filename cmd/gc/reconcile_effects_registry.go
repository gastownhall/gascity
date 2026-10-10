package main

import (
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
}

// heldCaps are a pass's capabilities. create is what the pass hands its
// creates, raw stores included: a create's guarded row write is v5 R1's
// exception 1 (§13). creates runs them. The planner sets both, the first on
// its first admitted create (planner.submit).
type heldCaps struct {
	create  *createPass
	creates *createEffects
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
	p.reads.legs = w.WorkLegs.readOnly()
	stripped := *w
	stripped.LegStores, stripped.Demand.AssignedStores = nil, nil
	stripped.SessionsStore, stripped.RigStores, stripped.WorkLegs = nil, nil, WorkLegs{}
	if w.Env != nil {
		env := *w.Env
		env.SP = nil
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
	intentStart:           {class: capStarts, tokens: 1, needs: needs{Lease: true}},
	intentAdopt:           {class: capProbing, needs: needs{Lease: true}},
	intentCreate:          {class: capCreates, caps: capCreate, body: createBody},                                 // C1, C2
	intentRekey:           {class: capProbing, needs: needs{Runtime: true, Lease: true}, sections: rekeySections}, // A3: writes the incarnation under the row's lease record
	intentZombie:          {class: capProbing, bootGated: true, needs: needs{Lease: true}},
	intentDrainBegin:      {class: capRowWrites, bootGated: true},
	intentDrainBeginFresh: {class: capProbing, bootGated: true},
	intentSignal:          {class: capRowWrites, bootGated: true},
	intentSignalFresh:     {class: capProbing, bootGated: true},
	intentDrainCancel:     {class: capRowWrites, sections: drainClearSections}, // A19 (C6a)
	intentDrainVoid:       {class: capRowWrites, sections: drainClearSections}, // A19 (C6a)
	intentStop:            {class: capProbing, bootGated: true, needs: needs{Lease: true}},
	intentClose:           {class: capProbing, bootGated: true, needs: needs{Lease: true}},
	intentRollback:        {class: capProbing, bootGated: true, needs: needs{Lease: true}},
	intentRowMetadata:     {class: capRowWrites},
	intentBaseline:        {class: capRowWrites},
	intentRowHeal:         {class: capRowWrites, sections: rowWriteSections},                        // A6
	intentRowHealFresh:    {class: capProbing, needs: needs{Runtime: true}, sections: healSections}, // A6
}
