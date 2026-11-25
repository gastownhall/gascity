package main

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// tickProbeBound is the longest the reconcile tick waits for one session's
// runtime to answer the loop head's probe, and for one tick kill. It is a
// package var so tests can shrink it.
var tickProbeBound = postStartObservationBound

// tickProbeGate is the tick-scoped record of which sessions' runtimes did not
// answer. RED scaffolding: the gate admits every session and records nothing.
type tickProbeGate struct{}

// newTickProbeGate builds the gate for one reconcile tick.
func newTickProbeGate(_ context.Context, _ string, _ runtime.Provider) *tickProbeGate {
	return &tickProbeGate{}
}

// mayReconcile reports whether the session named name may be reconciled this
// tick. RED scaffolding: always true.
func (g *tickProbeGate) mayReconcile(_ string) bool { return true }
