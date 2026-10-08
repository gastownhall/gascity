package main

import (
	"context"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The fresh heal: A6's heals that write asleep because the inventory read
// the row's runtime gone or dead (CONTRACT v5 A6; R2, reads propose and
// effects decide). The inventory can lag a runtime that came up after its
// pass began, and a start that settled deferred (S3) leaves its row creating
// at its own token with its runtime perhaps up, so the token fence alone
// would let the heal orphan that runtime. So the effect first takes the
// runtime name lock, which a v2 start holds across its provider Start (an
// abandoned start until that call returns, P3), then reads the name fresh for
// the effect (LL2's ObserveLivenessBoundedSince) through a leaf that reports
// observation errors. Only a complete read with no live agent lets it run
// the row write, still under the lock: a live runtime refuses, its row stays
// creating, and S1 adopts it once the inventory lists it.

// Fresh-heal refusal causes. A refusal backs the row off (P4).
const (
	causeLivenessUnsupported = "liveness-unsupported"
	causeLivenessIncomplete  = "liveness-incomplete"
	causeRuntimeRunning      = "runtime-running"
)

func rowHealFreshEffect(p *effectPass, it intent) func(context.Context) settlement {
	return func(ctx context.Context) settlement {
		since := time.Now()
		row := p.World.Census.Rows[it.Key].Info
		leaf, _, known := runtime.ResolveBackend(p.Runtime, strings.TrimSpace(row.SessionName))
		if _, ok := leaf.(runtime.LivenessObserverWithError); !ok || !known {
			return settlement{Outcome: settledRefused, Cause: causeLivenessUnsupported}
		}
		name, unlock, ok := lockRuntimeName(p.World, row)
		if !ok {
			return settlement{Outcome: settledRefused, Cause: causeNameBusy}
		}
		defer unlock()
		var processNames []string
		if p.World.Obs != nil {
			processNames = p.World.Obs.ByName[name].Identity.ProcessNames
		}
		l, status, err := runtime.ObserveLivenessBoundedSince(ctx, p.Runtime, name, processNames, since, fenceProbeTimeout)
		switch {
		case status != runtime.ObservationComplete || err != nil:
			return settlement{Outcome: settledRefused, Cause: causeLivenessIncomplete, Err: err}
		case l.Alive:
			return settlement{Outcome: settledRefused, Cause: causeRuntimeRunning}
		}
		return rowWriteEffect(p, it)(ctx)
	}
}
