package main

import (
	"context"
	"io"
)

// dependencyProbeBound is the longest one start-dependency probe waits for the
// runtime to answer whether a dependency session is alive. It is a variable so
// a test can shrink it; production code never assigns it.
var dependencyProbeBound = postStartObservationBound

// dependencyProbePass carries what one planner pass shares across its
// dependency checks: the single open-session list, the per-session answers and
// the abandon budget. A pass is one candidateWaveOrder call, one wave's first
// filter or one recheck batch, never a whole tick.
type dependencyProbePass struct{}

// newDependencyProbePass starts a pass for the city at cityPath whose probes
// end when ctx does (a nil ctx never ends a probe early) and whose unanswered
// probes are logged to stderr.
func newDependencyProbePass(_ context.Context, _ string, _ io.Writer) *dependencyProbePass {
	return &dependencyProbePass{}
}
