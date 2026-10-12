package main

import (
	"cmp"
	"context"
	"maps"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The stop verb (CONTRACT v5 D3, D4, F2, F3; C6b2), A4's effect, under the
// runtime lease. Each attempt reads the runtime fresh and decides A4 again
// on it and the fresh row, so a row that left its stop is never stopped.
//
//  1. The fence, with the legs the stop's origin keeps (stopLegs): a
//     signaled request's stop-pending kill keeps L2 and L3 (L1 waived, as L2
//     reads Current; no L5, DRAIN-048/011); an operator-dormant row's
//     direct stop the full fence but L5, so an operator's suspend stops a
//     seat that holds work (J42). A fresh Foreign verdict ends the stop
//     without one: the name holds another row's runtime, the proof this
//     row's is gone (DRAIN-036/038, R52). A bead-scoped pool name is never
//     Foreign (C8.2(a), ownsName), and L2 still passes only on Current.
//  2. The Call: the fenced provider Stop of a live runtime (stopFenced), or,
//     with nothing live, C8.8 alone (confirmStopped). Stop's return decides
//     nothing; the fresh read after it does. A corpse counts as stopped, and
//     the Call removes it under the corpse exception, by exact session
//     object, when L3 passes; an attached corpse stays.
//  3. The finalize (D4): the intent's builder (Finish), else finalizePatch,
//     on the close-gate work read live. A close lands as the premise close
//     (closeSection); any other finalize is one CAS that writes its patch
//     and clears the stop request (D3 step 5). The events are legacy's:
//     session.stopped with the stop metric when a stop was performed, and
//     session.drain_acked_with_assigned_work when a drained row keeps work.

// causeDispositionRefused: the Sleep cell refused the finalize's move.
const causeDispositionRefused = "disposition-refused"

// stopSections are the fence and the Call, the finalize write, and the
// premise close, which only a finalize that closes reaches: a store that
// cannot close atomically still stops and finalizes every other row.
var stopSections = []section{
	called(callStop, section{Premise: premiseDrainBasis, Decide: stopDecide}, stopCall),
	called(0, probed(finalizeWorkProbe, stopFinalizeStep), passStopped),
	asStopClose(probed(finalizeWorkProbe, stopCloseStep)),
}

// stopNeeds are the stop verb's fresh reads: the runtime under its lease,
// and L3 and L4 for an operator-dormant stop's full fence.
func stopNeeds(_ *World, it intent) needs {
	return needs{Runtime: true, Lease: true, Legs: stopLegs(it) &^ (legLiveness | legToken)}
}

// stopLegs are the fence legs the stop's origin keeps (v5 F1, F2).
func stopLegs(it intent) fenceLegs {
	if it.Reason == decideDormantStop || it.Kind == intentDriftRestart || it.Kind == intentDriftRelaunch { // A14's restart: the Full fence (D3)
		return legsFull
	}
	return legToken | legAttach
}

// asStopClose is sec as the stop's premise-close section.
func asStopClose(sec section) section {
	sec.Premise = premiseClose
	return sec
}

// stopPlan is what the fence hands the Call.
type stopPlan struct {
	name    string
	verdict fenceVerdict // a live runtime's fence; zero for nothing live
	names   []string
	foreign bool // a fresh Foreign verdict: finalize without a stop
	corpse  *corpseObject
	now     time.Time
}

// corpseObject is the corpse a confirmed stop removes: its leaf and the
// session object the read saw.
type corpseObject struct {
	leaf runtime.Provider
	name string
	live runtime.Liveness
}

// stopped is the Call's outcome, which the finalize reads.
type stopped struct {
	Confirmed bool
	Performed bool   // a provider Stop was issued
	Reason    string // why not confirmed
}

// stopDecide is A4 decided again on the fresh row and runtime, then the
// fence. It refuses a stop A4 no longer decides, or one the fence holds.
func stopDecide(v txView) txStep {
	w := v.World.withRow(v.It.Key, v.Row, v.Meta).withRuntime(v.It.Key, v.RT)
	if fresh, _ := decideRow(&w, v.Alloc, v.It.Key); fresh.Kind != v.It.Kind { // the stop's, or A14's restart that runs it
		return txStep{Refuse: causeRedecided}
	}
	name := strings.TrimSpace(v.Row.SessionName)
	plan := stopPlan{name: name, names: processNamesFor(v.World, v.Row), now: v.Now}
	switch rt := v.RT; rt.Class {
	case rtAbsent:
		return txStep{Pass: plan}
	case rtCorpse:
		if rt.Same {
			plan.corpse = &corpseObject{leaf: rt.leaf, name: name, live: rt.Again}
		}
		return txStep{Pass: plan}
	case rtAlive, rtZombie:
	default:
		return txStep{Refuse: fenceLivenessUnknown}
	}
	// A bead-scoped pool name embeds this row's ID, so it is never Foreign
	// (C8.2(a), ownsName); L2 still passes only on Current (I3).
	if !ownsName(v.Row, name) && v.RT.Same && v.RT.Identity.Known && compareIdentity(v.Row, v.RT.Identity) == identityForeign {
		plan.foreign = true
		return txStep{Pass: plan}
	}
	fv := fenceDestructive(v.RT, v.Fence, fenceRequest{Row: v.Row, Legs: stopLegs(v.It)})
	if !fv.Proceed {
		return txStep{Refuse: fv.Reason}
	}
	plan.verdict = fv
	return txStep{Pass: plan}
}

// stopCall stops a live runtime through its fence, or, with nothing live,
// confirms C8.8 alone; a confirmed corpse is then removed when L3 passes.
func stopCall(ctx context.Context, c txCaps, p stopPlan) (stopped, error) {
	sp, now := c.stop.sp, c.stop.clock.Now
	switch {
	case p.foreign:
		return stopped{Confirmed: true}, nil
	case p.verdict.Proceed:
		v, ok := stopFenced(ctx, sp, p.verdict, p.names, now)
		return stopped{Confirmed: ok, Performed: v.Proceed, Reason: v.Reason}, nil
	}
	if p.name == "" || !confirmStopped(ctx, sp, p.names, p.name, p.now) {
		return stopped{Reason: fenceStopUnconfirmed}, nil
	}
	if k := p.corpse; k != nil {
		removeCorpse(ctx, *k, now())
	}
	return stopped{Confirmed: true}, nil
}

// removeCorpse removes a confirmed corpse under the corpse exception (v5
// F2): L3 kept, by exact session object, which the kill re-checks
// server-side. An attached corpse, or a refused kill, stays.
func removeCorpse(ctx context.Context, k corpseObject, now time.Time) {
	killer, ok := k.leaf.(runtime.SessionObjectKiller)
	if !ok {
		return
	}
	if reason, _ := attachLeg(ctx, k.leaf, k.name, false, now); reason != "" {
		return
	}
	_, _ = killer.KillCorpseObject(k.name, k.live.ObjectID, k.live.ObjectCreated)
}

// finalizeWork is the finalize's live work read: the close-gate form (v5.7
// A9), which excludes the seat's own drain step; an error counts as work.
// anomaly is the work bead legacy's drain_acked_with_assigned_work names
// (drainAckClaimableAnomalyBead), when the row keeps work.
type finalizeWork struct {
	has     bool
	err     error
	anomaly *beads.Bead
}

// finalizeWorkProbe reads the row's work in the close-gate form.
func finalizeWorkProbe(_ context.Context, r effectReads, v txView) (finalizeWork, error) {
	if r.legs.unusable() != nil || v.World.Env == nil {
		return finalizeWork{err: errNoReadStore}, nil
	}
	// One seat's read, live, over the pass's city work legs.
	sw := seatWorkFor(r.legs, releaseScope(v.Row, v.World.Env.Cfg))
	w := finalizeWork{}
	w.has, w.err = sessionHasOpenAssignedWorkForReachableStoreForCloseGate(sw, v.Row)
	if w.has {
		if b, found, _ := drainAckClaimableAnomalyBead(sw, v.Row, v.Now); found {
			w.anomaly = &b
		}
	}
	return w, nil
}

// stopChoice is the finalize's choice on the fresh row: the intent's
// builder, else finalizePatch.
func stopChoice(v txView, work finalizeWork) finalizeChoice {
	f := finalizeFacts{
		Now: v.Now, CloseIfUnassigned: closeIfUnassigned(v), HasWork: work.has || work.err != nil,
		OperatorHeld: session.DispositionOfInfo(v.Row).KeepsSlot(),
		HonoredWake:  honoredWake(v),
		FreshWake:    v.Row.WakeMode == "fresh", RestartRequested: v.Row.RestartRequested == "true",
	}
	switch {
	case v.It.Finish != nil:
		return v.It.Finish(f)
	case v.It.Reason == decideDormantStop:
		return dormantChoice(v.Row, f)
	case stopReason(v.It) == drainIdle:
		return idleFinish(f)
	}
	return finalizePatch(f)
}

// closeIfUnassigned is legacy's finalize close condition
// (finalizeDrainAckStopPendingSessions): a seat the allocation no longer
// wants, or any pool seat.
func closeIfUnassigned(v txView) bool {
	var desired bool
	if v.Alloc != nil && v.Alloc.Snapshot != nil {
		e := v.Alloc.Snapshot.Entries[v.It.Key]
		desired = e != nil && e.InDesired
	}
	return !desired || isPoolManagedSessionInfo(v.Row)
}

// dormantChoice is an operator-dormant stop's finalize: finalizePatch, as
// legacy completes the drain of a managed suspend, written through the Sleep
// cell, which keeps the operator's hold; never a close, as the operator's
// state is the row's. A killed row keeps sleep_reason=killed.
func dormantChoice(row session.Info, f finalizeFacts) finalizeChoice {
	f.CloseIfUnassigned = false
	c := finalizePatch(f)
	c.Patch["drain_at"] = f.Now.UTC().Format(time.RFC3339) // legacy's drain stamps it, and its completion keeps it
	if session.SleepReason(strings.TrimSpace(row.SleepReason)) == session.SleepReasonKilled {
		c.Patch["sleep_reason"] = string(session.SleepReasonKilled)
	}
	return c
}

// honoredWake is D7 rule 4's honored wake: the row holds a pending
// explicit wake, and AL1 honored it (the pass wants the row awake).
func honoredWake(v txView) bool {
	if strings.TrimSpace(v.Row.WakeRequest) != string(session.WakeCauseExplicit) || v.Alloc == nil || v.Alloc.Snapshot == nil {
		return false
	}
	e := v.Alloc.Snapshot.Entries[v.It.Key]
	return e != nil && e.Desired == desireWake
}

// stopReason is a stop intent's drain reason.
func stopReason(it intent) string {
	if r, ok := strings.CutPrefix(it.Reason, decideStopFinalize); ok {
		return r
	}
	return strings.TrimPrefix(it.Reason, decideStopVerb)
}

// idleFinish is an idle drain's finalize (D4's idle builder): legacy's idle
// sleep, SleepPatch(idle), which keeps the sleep_policy_fingerprint the
// begin wrote (the idle latch, D2); never a close.
func idleFinish(f finalizeFacts) finalizeChoice {
	return finalizeChoice{Patch: session.SleepPatch(f.Now, drainIdle), AsSleep: true}
}

// stopCloseStep is the premise close of a confirmed stop whose finalize
// closes; one that no longer closes (work assigned since) refuses, and the
// next pass finalizes it again.
func stopCloseStep(v txView, work finalizeWork, _ error) txStep {
	out, _ := callResult[stopped](v)
	if !stopChoice(v, work).CloseSeat {
		return txStep{Refuse: causeRedecided}
	}
	terminal := session.ClosePatch(v.Now, "drained")
	maps.Copy(terminal, stopVoidResiduePatch()) // a closed row keeps no stop request
	return txStep{Terminal: terminal, Facts: stopFacts(v, out, nil), Done: true}
}

// passStopped hands the stop's outcome to the premise close.
func passStopped(_ context.Context, _ txCaps, out stopped) (stopped, error) { return out, nil }

// stopFinalizeStep, on a confirmed stop, writes the finalize's patch and
// clears the stop request in one CAS, or, for a finalize that closes, hands
// the stop's outcome to the premise close.
func stopFinalizeStep(v txView, work finalizeWork, _ error) txStep {
	out, err := callResult[stopped](v)
	switch {
	case err != nil:
		return txStep{Fail: causeCallError, Err: err}
	case !out.Confirmed:
		return txStep{Refuse: out.Reason}
	}
	choice := stopChoice(v, work)
	if choice.CloseSeat {
		return txStep{Pass: out}
	}
	// The choice's lifecycle patch and, for a sleep, the Sleep cell's move on
	// the fresh row in the same CAS, as legacy's ApplySleep splits it: the
	// cell keeps an operator's hold, a wait's intent and an intent it cannot
	// read, and clears the idle stop's transient.
	patch := maps.Clone(choice.Patch) // a builder may hand back a shared map
	if _, ok := activeStop(newCensusRow(v.It.Key, v.Row)); ok {
		maps.Copy(patch, stopVoidResiduePatch())
	}
	var kept *beads.Bead
	var class session.DispositionPatch
	if choice.AsSleep {
		to, err := session.DecodeDisposition(v.Meta).Apply(session.ActorController, session.DispEvent{Kind: session.EvSleep}, session.State(v.Row.MetadataState), v.Now)
		if err != nil {
			return txStep{Refuse: causeDispositionRefused, Err: err}
		}
		class, kept = to.Changed(), work.anomaly // the tx mints the cell's move into this write
	}
	return txStep{Write: patch, Class: class, Facts: stopFacts(v, out, kept), Done: true}
}

// stopFacts are legacy's events for a finalized stop (SESS-033/034,
// DRAIN-051/054).
func stopFacts(v txView, out stopped, kept *beads.Bead) effectFacts {
	template := cmp.Or(normalizedSessionTemplateInfo(v.Row, v.World.Env.Cfg), v.Row.Template)
	f := effectFacts{}
	if out.Performed {
		f.Stopped = &agentStop{Name: strings.TrimSpace(v.Row.SessionName), Identity: sessionAgentMetricIdentityInfo(v.Row, v.World.Env.Cfg)}
	}
	f.Events = append(f.Events, events.Event{
		Type: events.SessionStopped, Actor: "gc", Subject: template, Message: "drain acknowledged by agent",
		SessionID: v.Row.ID, Payload: api.SessionLifecyclePayloadJSON(v.Row.ID, template, "drain acknowledged"),
	})
	if kept != nil {
		f.Events = append(f.Events, events.Event{
			Type: events.SessionDrainAckedWithAssignedWork, Actor: "gc", Subject: template,
			Message: "session drain-acked while still assigned to work bead", SessionID: v.Row.ID,
			Payload: api.SessionDrainAckedWithAssignedWorkPayloadJSON(v.Row.ID, kept.ID, template, kept.Status, "drain_acked_with_assigned_work"),
		})
	}
	return f
}

// agentStop is a performed stop, for legacy's stop metric
// (telemetry.RecordAgentStop).
type agentStop struct{ Name, Identity string }
