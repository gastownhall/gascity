package main

import (
	"context"
	"io"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

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
	Runtime effectRuntime
}

// effectRuntime is the runtime half of an effect's capabilities (v5 R3):
// the provider its fresh reads and provider calls route through, the
// endpoint breaker, and the clock. Rec receives the breaker's transitions
// only; a row's events ride its settlement.
type effectRuntime struct {
	SP       runtime.Provider
	Capacity *endpointCapacityGuard
	Clock    plannerClock
	Rec      events.Recorder
	Stderr   io.Writer
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
	intentAdopt:   adoptEffect,    // S1: commits a live runtime, never launches
}
