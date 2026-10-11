package main

import "strings"

// The drain effects (CONTRACT v5 D1, D2, F1, F4). A begin is its arm decided
// again inside the CAS (redecideRow) on the fresh row, then its reason's
// legs, read fresh by the transaction; a holding leg refuses with its cause,
// backing the row off (P4):
//   - idle and no-wake-reason (the fresh kind): attach (L3) and a pending
//     interaction (L4) on the routed leaf (C8.9), and for an interactive
//     idle session the agent proved idle (SESS-621), whose failure is a
//     no-op, as legacy's probe is, not a backoff;
//   - orphaned and suspended: L5, the row's open or in-progress assigned
//     work read live, bounded (SESS-074); work, a failed read or an expired
//     one refuses.
//
// A cancel or a void is the row write of its patch, re-decided inside the
// CAS. Every drain write's landing records legacy's drain transition.

// Drain-begin refusal causes, beside the fence's reasons.
const (
	causeHasWork = "has-work"
	causeNotIdle = "not-idle"
)

// drainBeginSections are a begin's one section, behind the drain basis
// (premiseDrainBasis).
var drainBeginSections = []section{{Premise: premiseDrainBasis, Decide: redecideDrainBegin}}

// drainClearSections are a cancel's or a void's one section.
var drainClearSections = []section{{Decide: redecideDrainClear}}

// drainBeginNeeds are a begin's fresh reads: the runtime with L3 and L4 for
// the fresh kind, with the idle proof where SESS-621 asks for it; L5 for an
// undesired row.
func drainBeginNeeds(w *World, it intent) needs {
	reason := strings.TrimPrefix(it.Reason, decideDrainBegin)
	switch {
	case it.Kind == intentDrainBeginFresh:
		n := needs{Runtime: true, Legs: legAttach | legPending}
		if w.Census == nil {
			return n
		}
		if row, ok := w.Census.Rows[it.Key]; ok {
			_, n.Idle = idleProof(w, row.Info, reason)
		}
		return n
	case reason == drainOrphaned || reason == drainSuspended:
		return needs{Legs: legWork}
	}
	return needs{}
}

// redecideDrainBegin is redecideRow, then the begin's legs for its reason;
// its write carries legacy's "begin" drain transition.
func redecideDrainBegin(v txView) txStep {
	step := redecideRow(v)
	if len(step.Write) == 0 {
		return step
	}
	reason := strings.TrimPrefix(v.It.Reason, decideDrainBegin)
	if v.It.Kind == intentDrainBeginFresh {
		if fv := fenceDestructive(v.RT, v.Fence, fenceRequest{Row: v.Row, Legs: legAttach | legPending}); !fv.Proceed {
			return txStep{Refuse: fv.Reason}
		}
		// An agent not proved idle is no refusal: legacy's probe is a no-op
		// every tick, so a busy, detached session is not backed off, which
		// would hold its restarts, heals and cancels for minutes (C6a2 review).
		if _, probe := idleProof(v.World, v.Row, reason); probe && !v.Fence.Idle {
			return txStep{Cause: causeNotIdle}
		}
	}
	if w := v.Fence.Work; (reason == drainOrphaned || reason == drainSuspended) && (w == nil || w.Err != nil || !w.Free) {
		return txStep{Refuse: causeHasWork}
	}
	step.Facts.Transition = &drainTransition{Name: v.Row.SessionNameMetadata, Reason: reason, Transition: "begin"}
	return step
}

// redecideDrainClear is redecideRow; its write carries legacy's "cancel"
// drain transition, a fact only once the write lands.
func redecideDrainClear(v txView) txStep {
	step := redecideRow(v)
	if len(step.Write) > 0 {
		_, reason, _ := strings.Cut(v.It.Reason, ":")
		step.Facts.Transition = &drainTransition{Name: v.Row.SessionNameMetadata, Reason: reason, Transition: "cancel"}
	}
	return step
}
