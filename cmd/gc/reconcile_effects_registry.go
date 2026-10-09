package main

import "github.com/gastownhall/gascity/internal/runtime"

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
	seam txSeamFunc // the planner's: runs at the effect's seams (tests, staging)
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
	stripped := *w
	stripped.LegStores, stripped.Demand.AssignedStores = nil, nil
	stripped.SessionsStore, stripped.RigStores = nil, nil
	if w.Env != nil {
		env := *w.Env
		env.SP = nil
		stripped.Env = &env
	}
	p.World = &stripped
	return p
}

var effectSpecs = map[string]effectSpec{
	intentStart:           {class: capStarts, tokens: 1},
	intentAdopt:           {class: capProbing},
	intentCreate:          {class: capCreates, caps: capCreate, body: createBody},                    // C1, C2
	intentRekey:           {class: capProbing, needs: needs{Runtime: true}, sections: rekeySections}, // A3
	intentZombie:          {class: capProbing, bootGated: true},
	intentDrainBegin:      {class: capRowWrites, bootGated: true},
	intentDrainBeginFresh: {class: capProbing, bootGated: true},
	intentSignal:          {class: capRowWrites, bootGated: true},
	intentSignalFresh:     {class: capProbing, bootGated: true},
	intentDrainCancel:     {class: capRowWrites, sections: drainClearSections}, // A19 (C6a)
	intentDrainVoid:       {class: capRowWrites, sections: drainClearSections}, // A19 (C6a)
	intentStop:            {class: capProbing, bootGated: true},
	intentClose:           {class: capProbing, bootGated: true},
	intentRollback:        {class: capProbing, bootGated: true},
	intentRowMetadata:     {class: capRowWrites},
	intentBaseline:        {class: capRowWrites},
	intentRowHeal:         {class: capRowWrites, sections: rowWriteSections},                        // A6
	intentRowHealFresh:    {class: capProbing, needs: needs{Runtime: true}, sections: healSections}, // A6
}
