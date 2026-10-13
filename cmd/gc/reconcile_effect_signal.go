package main

import (
	"cmp"
	"context"
	"strings"
)

// The drain signal's effect (CONTRACT v5 D1's signaled transition, D2, F4;
// C6b1): A20's signal (armSignal) decided again inside the CAS (redecideRow)
// on the fresh row, so a drain A19 cancels or voids since the pass, or one
// whose request an operator's suspend or kill ended (activeStop's rule 2),
// is never signaled. Its one write is legacy's stop-pending projection
// (DrainAckStopPendingPatch), which legacy reads as its own
// (isDrainAckStopPendingInfo); never the blind Store.BeginDrainAckStopPending.
// The fresh kind first reads L3 and L4 on the routed leaf (C8.9): attached
// or pending refuses with its cause, and the row backs off.

// signalSections are a signal's one section, behind the drain basis
// (premiseDrainBasis), so an agent's heartbeat cannot starve it. Every
// signal kind holds the runtime name's flock (v5.9h: a drain takes the
// flock only), the plain one by NameLock. An operator's start
// (Manager.ensureRunning) holds the name's lease from its read of the row
// under that lease to its confirm, which clears the request: a signal
// before the lease is seen by that read, and the start refuses; one inside
// it refuses busy (#7635, #7638 reviews). Its probe revalidates an
// idle-respawn drain (idleRespawnRecheck).
var signalSections = []section{signalSection()}

func signalSection() section {
	s := probed(idleRespawnRecheck, redecideSignal)
	s.Premise = premiseDrainBasis
	return s
}

// causeIdleRespawnVoid prefixes the cause of an idle-respawn signal whose
// revalidation voided the drain instead (the cause's suffix says why).
const causeIdleRespawnVoid = "idle-respawn-void/"

// respawnRecheck is an idle-respawn signal's revalidation (DRAIN-004,
// DRAIN-044; legacy's idleRespawnAckUnsafeToStop and the drain advance's
// check): checked once every read answered, and why the stop is no longer
// safe, "" when it is. Its zero value, a revalidation that never ran or was
// lost (the probe expired or panicked), is not safe.
type respawnRecheck struct {
	checked bool
	unsafe  string
}

// idleRespawnRecheck re-proves, for an idle-respawn drain's own signal
// (the fresh kind), that the seat holds no claim (in-progress assigned work,
// read live, in legacy's narrow release scope, as A15's, v5.9s) and that its
// runtime shows no activity since the drain began (the pass's begin stamp,
// on the fresh read's leaf). Every read that fails fails closed, as
// legacy's. An agent's own ack of the drain signals unrevalidated
// (signal-ack), as legacy's DRAIN-004 covers only the acks it wrote itself.
func idleRespawnRecheck(_ context.Context, r effectReads, v txView) (respawnRecheck, error) {
	if !respawnSignal(v.It) {
		return respawnRecheck{}, nil
	}
	chk := respawnReads(r, v)
	chk.checked = true
	return chk, nil
}

// respawnSignal reports an idle-respawn drain's own signal.
func respawnSignal(it intent) bool {
	return it.Kind == intentSignalFresh && it.Reason == decideDrainSignal+idleRespawnDrainReason
}

// respawnReads are idleRespawnRecheck's reads.
func respawnReads(r effectReads, v txView) respawnRecheck {
	if r.legs.unusable() != nil || v.World.Env == nil {
		return respawnRecheck{unsafe: "claim-observation-error"}
	}
	claimed, err := sessionHasInProgressAssignedWorkForConfig(seatWorkFor(r.legs, releaseScope(v.Row, v.World.Env.Cfg)), v.Row)
	switch {
	case err != nil:
		return respawnRecheck{unsafe: "claim-observation-error"}
	case claimed:
		return respawnRecheck{unsafe: "claimed-work"}
	}
	req, ok := activeStop(v.World.Census.reread(v.It.Key, v.Row, v.Meta)) // the pass's stop keys (the probe runs before the row read), the safe stamp
	if !ok || req.At.IsZero() {
		return respawnRecheck{unsafe: "missing-drain-state"}
	}
	if v.RT == nil || v.RT.leaf == nil {
		return respawnRecheck{unsafe: "activity-observation-error"}
	}
	switch last, err := v.RT.leaf.GetLastActivity(strings.TrimSpace(v.Row.SessionName)); {
	case err != nil:
		return respawnRecheck{unsafe: "activity-observation-error"}
	case last.After(req.At):
		return respawnRecheck{unsafe: "activity-resumed"}
	}
	return respawnRecheck{}
}

// respawnClaimMoved reports a claim recorded on the fresh row since the pass
// (#7688 review): a `gc hook --claim` that lands after the probe's claim
// read and before the CAS, which the drain basis does not compare. The key
// is a sticky most-recent-claim marker, so it is compared with the pass's
// value, not tested for non-empty.
func respawnClaimMoved(v txView) bool {
	return strings.TrimSpace(v.Row.CurrentClaimBeadID) != strings.TrimSpace(v.World.Census.Rows[v.It.Key].Info.CurrentClaimBeadID)
}

// signalFreshNeeds are the fresh signal's reads: the runtime, with L3 and
// L4 (C8.9).
var signalFreshNeeds = needs{Runtime: true, Legs: legAttach | legPending}

// redecideSignal is redecideRow, then, for the fresh kind, an idle-respawn
// drain's revalidation, which voids the drain in the signal's stead (its
// attempt kept, so the bead gets no second one), as does a claim recorded on
// the fresh row since the pass (respawnClaimMoved), then L3 and L4. A floor
// member's canceled ack (DRAIN-010) carries legacy's "min-floor cancel"
// drain transition, a fact only once its write lands.
func redecideSignal(v txView, chk respawnRecheck, _ error) txStep {
	step, fresh := redecideRowIntent(v)
	if len(step.Write) > 0 && fresh.Reason == decideFloorAckCancel {
		step.Facts.Transition = &drainTransition{Name: v.Row.SessionNameMetadata, Reason: "min-floor", Transition: "cancel"}
	}
	if len(step.Write) == 0 || v.It.Kind != intentSignalFresh {
		return step
	}
	if respawnSignal(v.It) && chk.unsafe == "" && respawnClaimMoved(v) {
		chk.unsafe = "claim-moved"
	}
	if respawnSignal(v.It) && (!chk.checked || chk.unsafe != "") {
		void := txStep{Write: stopCancelPatch(), Cause: causeIdleRespawnVoid + cmp.Or(chk.unsafe, "unchecked")}
		void.Facts.Transition = &drainTransition{Name: v.Row.SessionNameMetadata, Reason: idleRespawnDrainReason, Transition: "cancel"}
		return void
	}
	if fv := fenceDestructive(v.RT, v.Fence, fenceRequest{Row: v.Row, Legs: legAttach | legPending}); !fv.Proceed {
		return txStep{Refuse: fv.Reason}
	}
	return step
}
