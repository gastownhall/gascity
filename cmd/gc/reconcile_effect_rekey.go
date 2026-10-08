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
// Under the runtime name lock it re-reads the runtime fresh: presence, then
// identity (readRuntimeIdentity), then presence again on the same session
// object. It proceeds only while the runtime is still StaleSelf to the
// pass's row under S4's guards, with the token the pass saw
// (rekeyStillHolds). Then it is the row-write effect: one CAS of
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
	before, cause := presentObject(ctx, leaf, name, since)
	if cause != "" {
		return settlement{Outcome: settledRefused, Cause: cause}
	}
	rt := readRuntimeIdentity(ctx, leaf, name)
	after, cause := presentObject(ctx, leaf, name, time.Now())
	switch {
	case cause != "":
		return settlement{Outcome: settledRefused, Cause: cause}
	case after.ObjectID != before.ObjectID:
		return settlement{Outcome: settledRefused, Cause: causeNotPresent}
	case !rekeyStillHolds(row, rt, e.it.Patch["instance_token"]):
		return settlement{Outcome: settledRefused, Cause: causeIdentityChanged}
	}
	return e.rowWrite.run(ctx)
}

// presentObject is one fresh presence read of name on leaf, from a refresh
// that started after since: the reading, or the refusal cause. The rekey
// brackets its identity read between two, which must see the same session
// object, so the identity read belongs to the runtime both found present.
func presentObject(ctx context.Context, leaf runtime.Provider, name string, since time.Time) (runtime.Liveness, string) {
	live, status, err := runtime.ObserveLivenessBoundedSince(ctx, leaf, name, nil, since, fenceProbeTimeout)
	switch {
	case status != runtime.ObservationComplete || err != nil:
		return live, causeLivenessUnknown
	case !live.Present():
		return live, causeNotPresent
	}
	return live, ""
}
