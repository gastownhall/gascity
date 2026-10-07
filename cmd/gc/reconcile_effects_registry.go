package main

import "context"

// The effects registry: the effect each admitted intent kind runs on the
// executor. Each later effect PR adds its line. An intent whose kind has none
// settles refused with cause no-effect (submitIntent), so an arm may land
// before its effect.

// effectPass is what one pass hands every effect it submits: a fenced
// writer per census leg (session rows live in work stores too, #5187; v5 R3)
// and the inputs the pass decided from, which a row write re-decides against
// (R2), stripped of their raw store and provider handles. The pass never
// mutates them after submit.
type effectPass struct {
	Writers map[string]fencedWriter // by census leg
	World   *World
	Alloc   *allocDecision
}

// newEffectPass is w's and a's effectPass.
func newEffectPass(w *World, a *allocDecision) *effectPass {
	p := &effectPass{Writers: make(map[string]fencedWriter, len(w.LegStores)), Alloc: a}
	for leg, store := range w.LegStores {
		p.Writers[leg] = fencedWriter{store: store}
	}
	stripped := *w
	stripped.LegStores, stripped.Demand.AssignedStores = nil, nil
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
	intentRowHeal: rowWriteEffect, // A6
}
