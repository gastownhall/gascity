package main

import (
	"context"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The rekey effect (CONTRACT v5 S4; owner ruling 3): arm A3's intent to
// align a row's instance_token with the runtime of the same row that carries
// an older token, so that runtime reads Current and its stop's L2 passes.
// Under the runtime name lock it re-reads the runtime fresh, presence first
// and identity second (readRuntimeIdentity), and proceeds only while the
// runtime is still StaleSelf to the pass's row under S4's guards, with the
// token the pass saw. Then it is the row-write effect: one CAS of
// instance_token that re-decides on the fresh row, so the row is still open,
// at the generation and token the pass saw (the basis), and still holds no
// pending_create_claim (rekeyable). It never writes generation. A probing
// effect (60s); not boot-gated, and it costs no token.

// causeIdentityChanged refuses a rekey whose fresh read no longer finds the
// runtime the pass saw. A refusal backs the row off (P4).
const causeIdentityChanged = "identity-changed"

func rekeyEffect(p *effectPass, it intent) func(context.Context) settlement {
	return rekey{rowWrite{pass: p, it: it, decide: decideRow}}.run
}

// rekey is one admitted rekey: the row write, behind the fresh re-read.
type rekey struct{ rowWrite }

func (e rekey) run(ctx context.Context) settlement {
	since := time.Now()
	row := e.pass.World.Census.Rows[e.it.Key].Info
	name := strings.TrimSpace(row.SessionName)
	leaf, _, known := runtime.ResolveBackend(e.pass.Runtime, name)
	if !known || leaf == nil || name == "" {
		return settlement{Outcome: settledRefused, Cause: causeRouteUnknown}
	}
	unlock := runtimeNames.tryLock(e.pass.World.CityPath, name)
	if unlock == nil {
		return settlement{Outcome: settledRefused, Cause: causeNameBusy}
	}
	defer unlock()
	live, status, err := runtime.ObserveLivenessBoundedSince(ctx, leaf, name, nil, since, fenceProbeTimeout)
	if status != runtime.ObservationComplete || err != nil || !live.Present() {
		return settlement{Outcome: settledRefused, Cause: causeNotPresent}
	}
	rt := readRuntimeIdentity(ctx, leaf, name)
	if !rekeyable(row, rt) || strings.TrimSpace(rt.Token) != e.it.Patch["instance_token"] {
		return settlement{Outcome: settledRefused, Cause: causeIdentityChanged}
	}
	return e.rowWrite.run(ctx)
}
