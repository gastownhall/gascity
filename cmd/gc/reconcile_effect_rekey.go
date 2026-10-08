package main

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The rekey effect (CONTRACT v5 S4; owner ruling 3): arm A3's intent to
// align a row's instance_token with the runtime of the same row that carries
// an older token, so that runtime reads Current and its stop's L2 passes.
// Under the runtime name lock (lockRuntimeName), and then the row's session
// mutation lock held through the CAS (rereadAndWrite), it re-reads the
// runtime fresh: presence, then identity (readRuntimeIdentity), then
// presence again on the same session object. It proceeds only while the runtime is still StaleSelf to the
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
	row := e.pass.World.Census.Rows[e.it.Key].Info
	name, unlock, ok := lockRuntimeName(e.pass.World, row)
	switch {
	case name == "":
		return settlement{Outcome: settledRefused, Cause: causeRouteUnknown}
	case !ok:
		return settlement{Outcome: settledRefused, Cause: causeNameBusy}
	}
	defer unlock()
	leaf, _, known := runtime.ResolveBackend(e.pass.Runtime, name)
	if !known || leaf == nil {
		return settlement{Outcome: settledRefused, Cause: causeRouteUnknown}
	}
	var s settlement
	_ = session.WithSessionMutationLock(e.it.Key.ID, func() error {
		s = e.rereadAndWrite(ctx, leaf, name, row)
		return nil
	})
	return s
}

// rereadAndWrite is the fresh re-read and the CAS, in one section under the
// row's session mutation lock, so an in-process start or restart (which
// takes that lock) cannot land between the identity read and the CAS: a
// runtime restarted with the row's token after the read would otherwise be
// re-keyed back to the stale one. Residual: an out-of-process writer of the
// runtime's identity (`gc attach` relaunching it) takes neither lock. The
// bracketing presence reads catch a replaced session object; a token
// rewritten in place on the same object after the read can still be
// overwritten by this CAS, and the next pass's A3 re-keys the row to it.
func (e rekey) rereadAndWrite(ctx context.Context, leaf runtime.Provider, name string, row session.Info) settlement {
	before, cause := presentObject(ctx, leaf, name, time.Now())
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
	return e.runLocked(ctx)
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
