package main

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
// it refuses busy (#7635, #7638 reviews).
var signalSections = []section{{Premise: premiseDrainBasis, Decide: redecideSignal}}

// signalFreshNeeds are the fresh signal's reads: the runtime, with L3 and
// L4 (C8.9).
var signalFreshNeeds = needs{Runtime: true, Legs: legAttach | legPending}

// redecideSignal is redecideRow, then, for the fresh kind, L3 and L4. A
// floor member's canceled ack (DRAIN-010) carries legacy's "min-floor
// cancel" drain transition, a fact only once its write lands.
func redecideSignal(v txView) txStep {
	step, fresh := redecideRowIntent(v)
	if len(step.Write) > 0 && fresh.Reason == decideFloorAckCancel {
		step.Facts.Transition = &drainTransition{Name: v.Row.SessionNameMetadata, Reason: "min-floor", Transition: "cancel"}
	}
	if len(step.Write) == 0 || v.It.Kind != intentSignalFresh {
		return step
	}
	if fv := fenceDestructive(v.RT, v.Fence, fenceRequest{Row: v.Row, Legs: legAttach | legPending}); !fv.Proceed {
		return txStep{Refuse: fv.Reason}
	}
	return step
}
