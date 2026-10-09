package main

import (
	"context"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The fresh heal: A6's heals whose patch rests on the runtime (CONTRACT v5
// A6; R2, reads propose and effects decide). The asleep heals rest on the
// inventory reading the name gone, which can lag a runtime that came up after
// its pass began: a start that settled deferred (S3) leaves its row creating
// at its own token with its runtime perhaps up, so the token fence alone
// would let the heal orphan it. The awake heal rests on the inventory reading
// the row's own runtime alive.
//
// So the effect takes the runtime name lock (lockRuntimeName), which a v2
// start holds across its provider Start (an abandoned start until that call
// returns, P3), then the row's session mutation lock, which in-process wakes
// and row writers take. Under both it stamps the time, reads the name fresh
// for the effect (LL2's ObserveLivenessBoundedSince), and writes
// through the row write only when that read still proves the heal: nothing
// present for an asleep heal (no live pane and no corpse, until A12's
// classification lands), the row's own Current runtime alive for the awake
// heal. The write also refuses, superseded, unless the fresh row's lifecycle
// facts equal the pass's row's (legacy's ApplyPatchIfLifecycleUnchanged), so
// a wake request that lands between the read and the CAS stops it.
//
// Residual, as in legacy: a process outside the controller (`gc session
// attach`, which starts a runtime at the row's token without the name lock)
// can bring a runtime up after the fresh read and before the CAS. The row is
// then asleep with its own live runtime, which the awake heal restores.

// Fresh-heal refusal causes, beside the shared ones (causeNameBusy,
// causeRouteUnknown, causeLivenessUnknown). A refusal backs the row off (P4);
// repeated ones alert (alertHealRefused).
const (
	causeLivenessUnsupported = "liveness-unsupported"
	causeRuntimePresent      = "runtime-present" // an asleep heal over a present runtime
	causeRuntimeNotOwn       = "runtime-not-own" // the awake heal over a runtime not alive and Current
)

func rowHealFreshEffect(p *effectPass, it intent) func(context.Context) settlement {
	return freshHeal{pass: p, it: it, now: time.Now}.run
}

// freshHeal is one fresh heal. now is the runtime's clock: the fresh read
// accepts only a refresh begun at or after the time it gives.
type freshHeal struct {
	pass *effectPass
	it   intent
	now  func() time.Time
}

func (e freshHeal) run(ctx context.Context) settlement {
	row, ok := e.pass.World.Census.Rows[e.it.Key]
	if !ok {
		return settlement{Outcome: settledRefused, Cause: causeRedecided}
	}
	if !threeOutcome(e.pass.Runtime) {
		return settlement{Outcome: settledRefused, Cause: causeLivenessUnsupported}
	}
	if _, _, known := runtime.ResolveBackend(e.pass.Runtime, strings.TrimSpace(row.Info.SessionName)); !known {
		return settlement{Outcome: settledRefused, Cause: causeRouteUnknown}
	}
	name, unlock, ok := lockRuntimeName(e.pass.World, row.Info)
	switch {
	case !ok && name == "":
		return settlement{Outcome: settledRefused, Cause: causeRouteUnknown}
	case !ok:
		return settlement{Outcome: settledRefused, Cause: causeNameBusy}
	}
	defer unlock()
	var s settlement
	_ = session.WithSessionMutationLock(e.it.Key.ID, func() error {
		if s = e.prove(ctx, name, e.now(), row.Info); s.Outcome == 0 {
			s = rowWrite{pass: e.pass, it: e.it, decide: decideRow, sameLifecycle: true}.runLocked(ctx)
		}
		return nil
	})
	return s
}

// prove reads name fresh and returns the refusal or failure that stops the
// heal, or a zero settlement when the read proves it. An asleep heal reads
// absence through the composite, so a stale route falls through and cannot
// fake it; the awake heal reads presence and identity through the one routed
// leaf, never mixing backends, so a stale route only refuses.
func (e freshHeal) prove(ctx context.Context, name string, since time.Time, row session.Info) settlement {
	awake := e.it.Reason == decideAwakeHeal
	sp := e.pass.Runtime
	if awake {
		sp, _, _ = runtime.ResolveBackend(sp, name)
	}
	live, status, err := runtime.ObserveLivenessBoundedSince(ctx, sp, name, nil, since, fenceProbeTimeout)
	switch {
	case ctx.Err() != nil:
		return settlement{Outcome: settledFailed, Cause: causeDeadline, Err: ctx.Err()}
	case status != runtime.ObservationComplete || err != nil:
		return settlement{Outcome: settledRefused, Cause: causeLivenessUnknown, Err: err}
	case !awake:
		if live.Present() {
			return settlement{Outcome: settledRefused, Cause: causeRuntimePresent}
		}
		return settlement{}
	case !live.Alive || compareIdentity(row, readRuntimeIdentity(ctx, sp, name)) != identityCurrent:
		return settlement{Outcome: settledRefused, Cause: causeRuntimeNotOwn}
	}
	// AdoptLive's bracket: an acp or subprocess sidecar can outlive its
	// runtime, so the identity read proves the row's own runtime only if the
	// runtime is still alive after it.
	again, status, err := runtime.ObserveLivenessBoundedSince(ctx, sp, name, nil, e.now(), fenceProbeTimeout)
	switch {
	case ctx.Err() != nil:
		return settlement{Outcome: settledFailed, Cause: causeDeadline, Err: ctx.Err()}
	case status != runtime.ObservationComplete || err != nil:
		return settlement{Outcome: settledRefused, Cause: causeLivenessUnknown, Err: err}
	case !again.Alive:
		return settlement{Outcome: settledRefused, Cause: causeRuntimeNotOwn}
	}
	return settlement{}
}
