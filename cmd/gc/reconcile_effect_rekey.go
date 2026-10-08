package main

import (
	"context"
	"time"

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
	var s settlement
	_ = session.WithSessionMutationLock(e.it.Key.ID, func() error {
		s = e.rereadAndWrite(ctx, name, row)
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
func (e rekey) rereadAndWrite(ctx context.Context, name string, row session.Info) settlement {
	rt, cause := readRuntime(ctx, e.pass.Runtime, processNamesFor(e.pass.World, row), name, time.Now(), time.Now)
	if cause == "" {
		cause = rekeyRefusal(rt, row, e.it.Patch["instance_token"])
	}
	if cause != "" {
		return settlement{Outcome: settledRefused, Cause: cause}
	}
	return e.runLocked(ctx)
}

// rekeyRefusal is the cause that refuses a rekey of the pass's row to token
// on rt, or "": rt must read fresh, present, one session object around its
// identity read, and still rekeyable (S4).
func rekeyRefusal(rt *txRuntime, row session.Info, token string) string {
	switch {
	case rt.Class == rtUnsupported:
		return causeLivenessUnsupported
	case rt.Class == rtUnknown:
		return causeLivenessUnknown
	case rt.Class == rtAbsent || !rt.Same:
		return causeNotPresent
	case !rekeyStillHolds(row, rt.Identity, token):
		return causeIdentityChanged
	}
	return ""
}
