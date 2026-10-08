package main

import (
	"context"

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
	// Runtime is the composite provider. Fresh reads and C8.8 go through it,
	// never a routed leaf alone (mc-zndi7.24). An effect calls no provider
	// verb on it directly: destructive verbs go through the fence
	// (fenceDestructive, stopFenced) and SessionObjectKiller, and only the
	// start effect calls Start (the effect lint).
	Runtime runtime.Provider
	World   *World
	Alloc   *allocDecision
	// create is what the pass hands its creates, raw stores included: a
	// create's guarded row write is v5 R1's exception 1 (§13), and only
	// createEffect reads it. creates runs them. The planner sets both, the
	// first on its first admitted create (planner.submit).
	create  *createPass
	creates *createEffects
	// Start is what a start effect holds beyond the pass (startEnv).
	Start startEnv
}

// newEffectPass is w's and a's effectPass.
func newEffectPass(w *World, a *allocDecision) *effectPass {
	p := &effectPass{Writers: make(map[string]fencedWriter, len(w.LegStores)), Alloc: a}
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

// effectBuilder builds an admitted intent's Run for one pass.
type effectBuilder func(p *effectPass, it intent) func(context.Context) settlement

var effectRegistry = map[string]effectBuilder{
	intentRowHeal:     rowWriteEffect,   // A6
	intentCreate:      createEffect,     // C1, C2
	intentDrainCancel: drainClearEffect, // A19 (C6a)
	intentDrainVoid:   drainClearEffect, // A19 (C6a)
	intentRekey:       rekeyEffect,      // A3
	intentAdopt:       adoptEffect,      // S1: commits a live runtime, never launches
}
