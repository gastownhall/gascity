package dashboardbff

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/runproj"
)

func (t *cityRunTailer) runCensus(ctx context.Context) runproj.CanonicalRunCensus {
	timer := time.NewTimer(runColdLoadWait)
	defer timer.Stop()
	select {
	case <-t.readyCh:
	case <-ctx.Done():
	case <-timer.C:
	}

	t.mu.RLock()
	counts := t.census
	ready := t.ready
	incomplete := t.summary.LanesPartial
	t.mu.RUnlock()

	response := runproj.CanonicalRunCensus{
		Ready:        ready,
		StatusCounts: counts,
		Partial:      incomplete,
	}
	if !ready {
		response.Partial = true
		response.PartialReasons = []string{"run projection is warming"}
	} else if incomplete {
		response.PartialReasons = []string{"run projection is incomplete"}
	}
	return response
}

// RunCensus returns a bounded canonical status census from the plane's warm
// incremental projector. The bool is false only when cityName is unknown.
func (p *Plane) RunCensus(ctx context.Context, cityName string) (runproj.CanonicalRunCensus, bool) {
	tailer, ok := p.cityRunTailer(cityName)
	if !ok {
		return runproj.CanonicalRunCensus{}, false
	}
	return tailer.runCensus(ctx), true
}
