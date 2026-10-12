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
// (premiseDrainBasis), so an agent's heartbeat cannot starve it.
var signalSections = []section{{Premise: premiseDrainBasis, Decide: redecideSignal}}

// signalFreshNeeds are the fresh signal's reads: the runtime, with L3 and
// L4 (C8.9).
var signalFreshNeeds = needs{Runtime: true, Legs: legAttach | legPending}

// redecideSignal is redecideRow, then, for the fresh kind, L3 and L4.
func redecideSignal(v txView) txStep {
	step := redecideRow(v)
	if len(step.Write) == 0 || v.It.Kind != intentSignalFresh {
		return step
	}
	if fv := fenceDestructive(v.RT, v.Fence, fenceRequest{Row: v.Row, Legs: legAttach | legPending}); !fv.Proceed {
		return txStep{Refuse: fv.Reason}
	}
	return step
}
